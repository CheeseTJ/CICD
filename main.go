package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

type config struct {
	Port             string
	DataFile         string
	GitHubToken      string
	PGYERKey         string
	GitHubOwner      string        // 可选：非空时列出指定用户/组织的公开仓库
	AutoSyncInterval time.Duration // 自动同步轮询间隔
	CFToken          string        // 可选：Cloudflare API Token（Pages:Read + Workers Scripts:Read），用于展示前端站点部署记录
	CFAccountID      string        // 可选：Cloudflare 账号 ID
	CFProjects       []string      // 可选：只展示这些 Pages 项目，空表示全部
	CFWorkerSites    []string      // 显式标识：哪些 Worker 是前端静态站（如 remind-web），空表示不展示 Workers 站点
	Auth             authConfig    // 鉴权：USER_CENTER_URL 非空时启用用户中心登录
}

func loadConfig() *config {
	return &config{
		Port:             getenv("PORT", "8080"),
		DataFile:         getenv("DATA_FILE", "./data.json"),
		GitHubToken:      os.Getenv("GITHUB_TOKEN"),
		PGYERKey:         os.Getenv("PGYER_API_KEY"),
		GitHubOwner:      os.Getenv("GITHUB_OWNER"),
		AutoSyncInterval: getenvDuration("AUTO_SYNC_INTERVAL", 5*time.Minute),
		CFToken:          os.Getenv("CLOUDFLARE_API_TOKEN"),
		CFAccountID:      os.Getenv("CLOUDFLARE_ACCOUNT_ID"),
		CFProjects:       splitCSV(os.Getenv("CF_PAGES_PROJECTS")),
		CFWorkerSites:    splitCSV(os.Getenv("CF_WORKERS_SITES")),
		Auth: authConfig{
			UserCenterURL: os.Getenv("USER_CENTER_URL"),
			App:           getenv("USER_CENTER_APP", "cicd"),
			AllowedEmails: splitCSV(os.Getenv("ALLOWED_EMAILS")),
		},
	}
}

// splitCSV 解析逗号分隔的配置项，忽略空白项
func splitCSV(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

type server struct {
	cfg   *config
	store *Store
	gh    *githubClient
	pgyer *pgyerClient
	cf    *cloudflareClient
	auth  *authenticator

	syncMu  sync.Mutex
	syncing map[string]bool

	jobsMu sync.Mutex
	jobs   map[string]*syncJob
}

type assetView struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

type repoView struct {
	FullName    string `json:"full_name"`
	Name        string `json:"name"`
	Description string `json:"description"`
	HTMLURL     string `json:"html_url"`
	Language    string `json:"language"`
	Private     bool   `json:"private"`
	Archived    bool   `json:"archived"`
	Fork        bool   `json:"fork"`

	IsApp        bool `json:"is_app"`
	AutoDetected bool `json:"auto_detected"`
	Marked       bool `json:"marked"`
	MarkedSet    bool `json:"marked_set"`

	LatestTag         string      `json:"latest_tag"`
	LatestName        string      `json:"latest_name"`
	LatestPublishedAt string      `json:"latest_published_at"`
	LatestHTMLURL     string      `json:"latest_html_url"`
	ApkAssets         []assetView `json:"apk_assets"`

	PgyerLinked      bool   `json:"pgyer_linked"`
	PgyerVersion     string `json:"pgyer_version"`
	PgyerName        string `json:"pgyer_name"`
	PgyerShortcutUrl string `json:"pgyer_shortcut_url"`

	AutoSync          bool   `json:"auto_sync"`
	LastSyncedVersion string `json:"last_synced_version"`
	LastSyncedTag     string `json:"last_synced_tag"`
	LastSyncedAt      string `json:"last_synced_at"`
	LastSyncedAtUnix  int64  `json:"last_synced_at_unix"`

	Platform string `json:"platform"` // 主体平台：pgyer / cf / oci，空表示判断不出
}

func main() {
	cfg := loadConfig()

	store, err := NewStore(cfg.DataFile)
	if err != nil {
		log.Fatalf("初始化数据文件失败: %v", err)
	}

	s := &server{
		cfg:     cfg,
		store:   store,
		gh:      &githubClient{http: newHTTPClient(), token: cfg.GitHubToken},
		pgyer:   &pgyerClient{http: newHTTPClient(), apiKey: cfg.PGYERKey},
		cf:      &cloudflareClient{http: newHTTPClient(), token: cfg.CFToken, accountID: cfg.CFAccountID, projects: cfg.CFProjects, workerSites: cfg.CFWorkerSites},
		auth:    newAuthenticator(cfg.Auth, newHTTPClient()),
		syncing: map[string]bool{},
		jobs:    map[string]*syncJob{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	// /api/health 保持公开：前端要先知道「是否启用鉴权、用户中心在哪」才能弹出登录框。
	// 未登录时只返回鉴权相关字段，配置明细不外泄（见 handleHealth）。
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/repos", s.auth.wrap(s.handleRepos))
	mux.HandleFunc("/api/deployments", s.auth.wrap(s.handleDeployments))
	mux.HandleFunc("/api/pages", s.auth.wrap(s.handlePages))
	mux.HandleFunc("/api/repos/mark", s.auth.wrap(s.handleMark))
	mux.HandleFunc("/api/repos/autosync", s.auth.wrap(s.handleAutoSync))
	mux.HandleFunc("/api/repos/sync", s.auth.wrap(s.handleSync))
	mux.HandleFunc("/api/repos/sync/progress", s.auth.wrap(s.handleSyncProgress))

	s.startAutoSync()

	addr := ":" + cfg.Port
	if cfg.Auth.enabled() {
		if len(cfg.Auth.AllowedEmails) > 0 {
			log.Printf("已启用用户中心登录（%s，app=%s，仅管理员可访问，邮箱白名单 %d 个）",
				cfg.Auth.UserCenterURL, cfg.Auth.App, len(cfg.Auth.AllowedEmails))
		} else {
			log.Printf("已启用用户中心登录（%s，app=%s，仅管理员可访问，未设邮箱白名单）",
				cfg.Auth.UserCenterURL, cfg.Auth.App)
		}
	} else {
		log.Printf("警告：未配置 USER_CENTER_URL，鉴权已关闭，接口对外裸奔")
	}
	log.Printf("cicd 启动: http://localhost%s (自动同步间隔 %s)", addr, cfg.AutoSyncInterval)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// 鉴权三字段始终返回：前端要先知道「是否需要登录、去哪个用户中心、用哪个 app」才能弹登录框。
	// 用户中心地址与 app 标识都非密钥，公开无妨。
	out := map[string]interface{}{
		"ok":              true,
		"auth_required":   s.cfg.Auth.enabled(),
		"auth_app":        s.cfg.Auth.App,
		"user_center_url": s.cfg.Auth.UserCenterURL,
	}

	// 未登录时不下发配置明细（哪些密钥已配置、数据文件路径等）
	if s.cfg.Auth.enabled() {
		claims := s.auth.authenticate(r)
		if claims == nil {
			writeJSON(w, http.StatusOK, out)
			return
		}
		out["account_email"] = claims.Email
	}

	out["github_token"] = s.cfg.GitHubToken != ""
	out["pgyer_key"] = s.cfg.PGYERKey != ""
	out["github_owner"] = s.cfg.GitHubOwner
	out["auto_sync_interval"] = s.cfg.AutoSyncInterval.String()
	out["data_file"] = s.cfg.DataFile
	out["cloudflare_token"] = s.cfg.CFToken != ""
	out["cloudflare_account_id"] = s.cfg.CFAccountID != ""
	out["cf_pages_projects"] = strings.Join(s.cfg.CFProjects, ",")
	writeJSON(w, http.StatusOK, out)
}

// handlePages 展示 Cloudflare Pages 站点与最近部署记录（只读，数据实时取自 CF API）。
// 未配置 CLOUDFLARE_API_TOKEN / CLOUDFLARE_ACCOUNT_ID 时返回 configured=false，前端据此给出提示。
func (s *server) handlePages(w http.ResponseWriter, r *http.Request) {
	if !s.cf.ready() {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"configured": false,
			"projects":   []pagesProjectView{},
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	projects, err := s.cf.listProjects(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	const perProject = 6
	views := make([]pagesProjectView, len(projects))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, p := range projects {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p cfProject) {
			defer wg.Done()
			defer func() { <-sem }()
			c, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			deps, err := s.cf.listDeployments(c, p.Name, perProject)
			if err != nil {
				// 单个站点的部署记录失败不影响其它站点，退回项目自带的 latest_deployment
				log.Printf("读取 Pages 部署记录失败 %s: %v", p.Name, err)
			}
			views[i] = buildPagesProjectView(p, deps, s.cf.accountID)
		}(i, p)
	}
	wg.Wait()

	wg.Wait()

	// Workers 静态托管（如 remind-web）：不在 Pages API 里，单独拉取后合并进同一列表。
	// 部分失败（权限不足/单个 Worker 读取失败）只降级不阻断，Pages 数据照常返回。
	workerViews, err := s.listWorkerSiteViews(ctx, perProject)
	if err != nil {
		log.Printf("读取 Workers 静态站点失败（Token 需要 Workers Scripts:Read）: %v", err)
	}
	views = append(views, workerViews...)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"configured": true,
		"account_id": s.cf.accountID,
		"projects":   views,
	})
}

// listWorkerSiteViews 拉取指定为前端静态站的 Worker（白名单 CF_WORKERS_SITES），
// 转成与 Pages 相同的视图。
//
// 为什么用显式白名单而不是自动识别：Workers API 不暴露「是否托管静态资产」，
// settings 里既没有 assets 标记、bindings 为空也无法区分纯静态站与无绑定的
// 代理型 Worker（实测 2026-09）。与其靠特征猜测，不如由配置显式声明。
func (s *server) listWorkerSiteViews(ctx context.Context, perProject int) ([]pagesProjectView, error) {
	if len(s.cf.workerSites) == 0 {
		return nil, nil
	}
	keep := make(map[string]bool, len(s.cf.workerSites))
	for _, name := range s.cf.workerSites {
		keep[name] = true
	}

	scripts, err := s.cf.listWorkerScripts(ctx)
	if err != nil {
		return nil, err
	}
	var statics []cfWorkerScript
	for _, sc := range scripts {
		if keep[sc.ID] {
			statics = append(statics, sc)
		}
	}
	if len(statics) == 0 {
		return nil, nil
	}

	// 自定义域一次拉全，按 service（script 名）分组
	domains, err := s.cf.listWorkerDomains(ctx)
	if err != nil {
		log.Printf("读取 Workers 自定义域失败: %v", err)
		domains = nil
	}
	subdomain, err := s.cf.getWorkerSubdomain(ctx)
	if err != nil {
		subdomain = "" // workers.dev 子域缺失不致命（多数站点用自定义域）
	}

	views := make([]pagesProjectView, len(statics))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, sc := range statics {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, sc cfWorkerScript) {
			defer wg.Done()
			defer func() { <-sem }()
			c, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			versions, err := s.cf.listWorkerVersions(c, sc.ID, perProject)
			if err != nil {
				log.Printf("读取 Worker 版本失败 %s: %v", sc.ID, err)
			}
			views[i] = buildWorkerProjectView(sc, domains, subdomain, versions, s.cf.accountID)
		}(i, sc)
	}
	wg.Wait()
	return views, nil
}

func (s *server) handleRepos(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	repos, err := s.gh.listRepos(ctx, s.cfg.GitHubOwner)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	// 蒲公英应用列表（失败不阻断仓库展示）
	apps, err := s.pgyer.listMy(ctx)
	if err != nil {
		log.Printf("listMy 失败: %v", err)
		apps = nil
	}
	byIdentifier := map[string]pgyerApp{}
	for _, a := range apps {
		if a.BuildIdentifier != "" {
			byIdentifier[a.BuildIdentifier] = a
		}
	}

	// 并发拉取每个仓库的最新 release（限流）
	type result struct {
		idx int
		rel *GHRelease
		err error
	}
	sem := make(chan struct{}, 6)
	ch := make(chan result, len(repos))
	for i, repo := range repos {
		owner, name := splitFullName(repo.FullName)
		sem <- struct{}{}
		go func(i int, owner, name string) {
			defer func() { <-sem }()
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			rel, err := s.gh.getLatestRelease(c, owner, name)
			ch <- result{idx: i, rel: rel, err: err}
		}(i, owner, name)
	}
	rels := make([]*GHRelease, len(repos))
	for range repos {
		res := <-ch
		if res.err == nil {
			rels[res.idx] = res.rel
		} else if !errors.Is(res.err, errNotFound) {
			log.Printf("获取 release 失败 %s: %v", repos[res.idx].FullName, res.err)
		}
	}

	views := make([]repoView, 0, len(repos))
	for i, repo := range repos {
		views = append(views, s.buildRepoView(repo, rels[i], byIdentifier))
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"repos": views})
}

type runView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	Branch      string `json:"branch"`
	Event       string `json:"event"`
	RunNumber   int    `json:"run_number"`
	HTMLURL     string `json:"html_url"`
	UpdatedAt   string `json:"updated_at"`
	DurationSec int64  `json:"duration_sec"`
}

type serviceView struct {
	FullName      string    `json:"full_name"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	HTMLURL       string    `json:"html_url"`
	Language      string    `json:"language"`
	Private       bool      `json:"private"`
	Archived      bool      `json:"archived"`
	WorkflowNames []string  `json:"workflow_names"`
	Platform      string    `json:"platform"` // 主体平台：cf / oci / pgyer，空表示判断不出
	Latest        *runView  `json:"latest"`
	Recent        []runView `json:"recent"`
}

func toRunView(r GHWorkflowRun) runView {
	var dur int64
	if t, err := time.Parse(time.RFC3339, r.RunStartedAt); err == nil {
		if end, err2 := time.Parse(time.RFC3339, r.UpdatedAt); err2 == nil {
			dur = int64(end.Sub(t).Seconds())
		}
	}
	status := r.Status
	if status == "completed" && r.Conclusion != "" {
		status = r.Conclusion
	}
	return runView{
		ID:          r.ID,
		Name:        r.Name,
		Title:       r.DisplayTitle,
		Status:      status,
		Conclusion:  r.Conclusion,
		Branch:      r.HeadBranch,
		Event:       r.Event,
		RunNumber:   r.RunNumber,
		HTMLURL:     r.HTMLURL,
		UpdatedAt:   r.UpdatedAt,
		DurationSec: dur,
	}
}

// handleDeployments 自动发现所有配置了 Actions workflow 的仓库，并返回最近运行状态。
func (s *server) handleDeployments(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	repos, err := s.gh.listRepos(ctx, s.cfg.GitHubOwner)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	const recentLimit = 5
	type result struct {
		idx int
		svc *serviceView
	}
	sem := make(chan struct{}, 6)
	ch := make(chan result, len(repos))
	for i, repo := range repos {
		owner, name := splitFullName(repo.FullName)
		sem <- struct{}{}
		go func(i int, repo GHRepo, owner, name string) {
			defer func() { <-sem }()
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			wfs, err := s.gh.listWorkflows(c, owner, name)
			if err != nil || len(wfs) == 0 {
				ch <- result{idx: i}
				return
			}
			names := make([]string, 0, len(wfs))
			for _, wf := range wfs {
				names = append(names, wf.Name)
			}

			// 读取 workflow 原文，判断主体部署位置（self-hosted=本机 OCI，wrangler=Cloudflare）；
			// 读不到就退回"产物是否分发在蒲公英"。都不影响卡片展示。
			texts := s.gh.workflowTexts(c, owner, name, wfs, 3)
			platform := servicePlatform(texts, s.store.Get(repo.FullName))

			runs, _ := s.gh.listRuns(c, owner, name, recentLimit)
			svc := &serviceView{
				FullName:      repo.FullName,
				Name:          repo.Name,
				Description:   repo.Description,
				HTMLURL:       repo.HTMLURL,
				Language:      repo.Language,
				Private:       repo.Private,
				Archived:      repo.Archived,
				WorkflowNames: names,
				Platform:      platform,
			}
			for _, run := range runs {
				svc.Recent = append(svc.Recent, toRunView(run))
			}
			if len(svc.Recent) > 0 {
				latest := svc.Recent[0]
				svc.Latest = &latest
			}
			ch <- result{idx: i, svc: svc}
		}(i, repo, owner, name)
	}

	picked := make([]*serviceView, len(repos))
	for range repos {
		res := <-ch
		if res.svc != nil {
			picked[res.idx] = res.svc
		}
	}
	services := make([]serviceView, 0, len(repos))
	for _, svc := range picked {
		if svc != nil {
			services = append(services, *svc)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"services": services})
}

func (s *server) buildRepoView(repo GHRepo, rel *GHRelease, byIdentifier map[string]pgyerApp) repoView {
	st := s.store.Get(repo.FullName)
	hasApk, apks := hasApkAsset(rel)

	var marked, markedSet bool
	if st != nil {
		marked = st.Marked
		markedSet = st.MarkedSet
	}
	isApp := hasApk
	if markedSet {
		isApp = marked
	}

	v := repoView{
		FullName:     repo.FullName,
		Name:         repo.Name,
		Description:  repo.Description,
		HTMLURL:      repo.HTMLURL,
		Language:     repo.Language,
		Private:      repo.Private,
		Archived:     repo.Archived,
		Fork:         repo.Fork,
		IsApp:        isApp,
		AutoDetected: hasApk,
		Marked:       marked,
		MarkedSet:    markedSet,
	}
	if rel != nil {
		v.LatestTag = rel.TagName
		v.LatestName = rel.Name
		v.LatestPublishedAt = rel.PublishedAt
		v.LatestHTMLURL = rel.HTMLURL
		for _, a := range apks {
			v.ApkAssets = append(v.ApkAssets, assetView{Name: a.Name, Size: a.Size, URL: a.BrowserDownloadURL})
		}
	}
	if st != nil {
		v.LastSyncedVersion = st.LastSyncedVersion
		v.LastSyncedTag = st.LastSyncedTag
		v.LastSyncedAt = st.LastSyncedAt
		v.LastSyncedAtUnix = st.LastSyncedAtUnix
		v.AutoSync = st.AutoSync
		// 已由本控制台同步过 → 产物分发在蒲公英
		if syncedToPgyer(st) {
			v.Platform = PlatformPgyer
		}
		if st.PgyerShortcutUrl != "" {
			v.PgyerShortcutUrl = st.PgyerShortcutUrl
		}
		if st.Identifier != "" {
			if app, ok := byIdentifier[st.Identifier]; ok {
				v.PgyerLinked = true
				v.PgyerVersion = app.BuildVersion
				v.PgyerName = app.BuildName
			}
		}
	}
	return v
}

func (s *server) handleMark(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FullName string `json:"full_name"`
		Marked   bool   `json:"marked"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.FullName) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("full_name 不能为空"))
		return
	}

	s.store.Update(req.FullName, func(st *RepoState) {
		st.Marked = req.Marked
		st.MarkedSet = true
	})
	if err := s.store.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (s *server) handleAutoSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FullName string `json:"full_name"`
		AutoSync bool   `json:"auto_sync"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.FullName) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("full_name 不能为空"))
		return
	}

	s.store.Update(req.FullName, func(st *RepoState) {
		st.AutoSync = req.AutoSync
	})
	if err := s.store.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (s *server) handleSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FullName string `json:"full_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.FullName) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("full_name 不能为空"))
		return
	}

	jobID := s.newJob(req.FullName)
	go s.runSyncJob(jobID, req.FullName)
	writeJSON(w, http.StatusAccepted, map[string]interface{}{"job_id": jobID})
}

var (
	errNoRelease = errors.New("该仓库没有已发布的 release")
	errNoApk     = errors.New("最新 release 中没有 .apk 资产")
)

type syncResult struct {
	BuildVersion    string `json:"build_version"`
	BuildName       string `json:"build_name"`
	BuildIdentifier string `json:"build_identifier"`
	BuildKey        string `json:"build_key"`
	AppKey          string `json:"app_key"`
	PageURL         string `json:"page_url"`
	QRCodeURL       string `json:"qr_code_url"`
}

type syncJob struct {
	FullName string      `json:"full_name"`
	Status   string      `json:"status"` // running | done | error
	Stage    string      `json:"stage"`  // prepare | download | upload | publish | done
	Percent  int         `json:"percent"`
	Done     int64       `json:"done"`
	Total    int64       `json:"total"`
	Message  string      `json:"message"`
	Result   *syncResult `json:"result,omitempty"`
}

func (s *server) newJob(fullName string) string {
	id := strconv.FormatInt(time.Now().UnixNano(), 10)
	s.jobsMu.Lock()
	s.jobs[id] = &syncJob{FullName: fullName, Status: "running", Stage: "prepare"}
	s.jobsMu.Unlock()
	return id
}

func (s *server) handleSyncProgress(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	s.jobsMu.Lock()
	j := s.jobs[id]
	s.jobsMu.Unlock()
	if j == nil {
		writeErr(w, http.StatusNotFound, errors.New("任务不存在"))
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *server) runSyncJob(jobID, fullName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	res, err := s.syncRepo(ctx, fullName, func(stage string, done, total int64) {
		s.updateJobProgress(jobID, stage, done, total)
	})

	s.jobsMu.Lock()
	j := s.jobs[jobID]
	if j == nil {
		j = &syncJob{FullName: fullName}
		s.jobs[jobID] = j
	}
	if err != nil {
		j.Status = "error"
		j.Stage = "error"
		j.Message = err.Error()
	} else {
		j.Status = "done"
		j.Stage = "done"
		j.Percent = 100
		j.Result = res
	}
	s.jobsMu.Unlock()
}

func (s *server) updateJobProgress(id, stage string, done, total int64) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	j := s.jobs[id]
	if j == nil {
		return
	}
	j.Stage = stage
	j.Done = done
	j.Total = total
	switch stage {
	case "download":
		if total > 0 {
			j.Percent = int(50 * done / total)
		}
	case "upload":
		if total > 0 {
			j.Percent = 50 + int(40*done/total)
		}
	case "publish":
		j.Percent = 95
	}
}

func (s *server) syncRepo(ctx context.Context, fullName string, prog func(stage string, done, total int64)) (*syncResult, error) {
	if !s.tryLock(fullName) {
		return nil, errors.New("该仓库正在同步中")
	}
	defer s.unlock(fullName)

	owner, name := splitFullName(fullName)

	rel, err := s.gh.getLatestRelease(ctx, owner, name)
	if errors.Is(err, errNotFound) {
		return nil, errNoRelease
	}
	if err != nil {
		return nil, err
	}

	_, apks := hasApkAsset(rel)
	if len(apks) == 0 {
		return nil, errNoApk
	}
	asset := apks[0]

	tmp, err := os.CreateTemp("", "cicd-*.apk")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())

	if prog != nil {
		prog("download", 0, asset.Size)
	}
	if err := s.gh.downloadAsset(ctx, asset, tmp, func(done, total int64) {
		if prog != nil {
			prog("download", done, total)
		}
	}); err != nil {
		return nil, fmt.Errorf("下载 apk 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	tok, err := s.pgyer.getCOSToken(ctx)
	if err != nil {
		return nil, err
	}

	var fileSize int64
	if fi, err := os.Stat(tmp.Name()); err == nil {
		fileSize = fi.Size()
	}
	if prog != nil {
		prog("upload", 0, fileSize)
	}
	if err := s.pgyer.uploadFile(ctx, tok, tmp.Name(), asset.Name, func(done, total int64) {
		if prog != nil {
			prog("upload", done, total)
		}
	}); err != nil {
		return nil, err
	}

	if prog != nil {
		prog("publish", 0, 0)
	}
	info, err := s.pgyer.pollBuildInfo(ctx, tok.Key, 10*time.Minute)
	if err != nil {
		return nil, err
	}

	appKey := ""
	if apps, err := s.pgyer.listMy(ctx); err == nil {
		for _, a := range apps {
			if a.BuildIdentifier != "" && a.BuildIdentifier == info.BuildIdentifier {
				appKey = a.AppKey
				break
			}
		}
	}

	now := time.Now()
	s.store.Update(fullName, func(st *RepoState) {
		st.Identifier = info.BuildIdentifier
		st.AppKey = appKey
		st.BuildKey = info.BuildKey
		st.LastSyncedVersion = info.BuildVersion
		st.LastSyncedTag = rel.TagName
		st.PgyerShortcutUrl = shortcutPage(info.BuildShortcutUrl)
		// 统一存 UTC + 带时区标记，展示时由前端换算成本地时间。
		st.LastSyncedAt = now.UTC().Format(time.RFC3339)
		st.LastSyncedAtUnix = now.Unix()
	})
	if err := s.store.Save(); err != nil {
		log.Printf("保存状态失败: %v", err)
	}

	return &syncResult{
		BuildVersion:    info.BuildVersion,
		BuildName:       info.BuildName,
		BuildIdentifier: info.BuildIdentifier,
		BuildKey:        info.BuildKey,
		AppKey:          appKey,
		PageURL:         shortcutPage(info.BuildShortcutUrl),
		QRCodeURL:       info.BuildQRCodeURL,
	}, nil
}

func (s *server) tryLock(fullName string) bool {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if s.syncing[fullName] {
		return false
	}
	s.syncing[fullName] = true
	return true
}

func (s *server) unlock(fullName string) {
	s.syncMu.Lock()
	delete(s.syncing, fullName)
	s.syncMu.Unlock()
}

func (s *server) startAutoSync() {
	go func() {
		ticker := time.NewTicker(s.cfg.AutoSyncInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.autoSyncOnce()
		}
	}()
}

func (s *server) autoSyncOnce() {
	s.store.mu.RLock()
	var targets []string
	for name, st := range s.store.Repos {
		if st.AutoSync {
			targets = append(targets, name)
		}
	}
	s.store.mu.RUnlock()

	for _, fullName := range targets {
		if err := s.autoSyncRepo(fullName); err != nil {
			log.Printf("自动同步 %s 失败: %v", fullName, err)
		}
	}
}

func (s *server) autoSyncRepo(fullName string) error {
	owner, name := splitFullName(fullName)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	rel, err := s.gh.getLatestRelease(ctx, owner, name)
	if err != nil {
		return err
	}
	if st := s.store.Get(fullName); st != nil && st.LastSyncedTag == rel.TagName {
		return nil // 已是最新，无需同步
	}

	_, err = s.syncRepo(ctx, fullName, nil)
	return err
}

func splitFullName(fullName string) (string, string) {
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) != 2 {
		return "", fullName
	}
	return parts[0], parts[1]
}

func shortcutPage(shortcut string) string {
	if shortcut == "" {
		return ""
	}
	return "https://www.pgyer.com/" + shortcut
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
