package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// cloudflareClient 只读访问 Cloudflare Pages / Workers API，用于在控制台展示前端站点的部署记录。
type cloudflareClient struct {
	http        *http.Client
	token       string   // CLOUDFLARE_API_TOKEN（Pages:Read + Workers Scripts:Read）
	accountID   string   // CLOUDFLARE_ACCOUNT_ID
	projects    []string // 可选白名单（CF_PAGES_PROJECTS），为空表示展示全部 Pages 项目
	workerSites []string // 可选强制包含名单（CF_WORKERS_SITES）；静态站默认由 has_assets 自动识别
}

func (c *cloudflareClient) ready() bool {
	return c != nil && c.token != "" && c.accountID != ""
}

const cfAPIBase = "https://api.cloudflare.com/client/v4"

type cfAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// cfHTTPError 保留 HTTP 状态码与 CF 错误码，便于上层区分「权限不足」与「真故障」。
type cfHTTPError struct {
	Status  int
	Code    int
	Message string
}

func (e *cfHTTPError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("Cloudflare API 错误(%d/%d): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("Cloudflare API 错误(%d): %s", e.Status, e.Message)
}


// get 发起一次 GET 请求，并把 envelope 里的 result 解到 out。
func (c *cloudflareClient) get(ctx context.Context, path string, query map[string]string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfAPIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if len(query) > 0 {
		q := req.URL.Query()
		for k, v := range query {
			q.Set(k, v)
		}
		req.URL.RawQuery = q.Encode()
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var env struct {
		Success bool            `json:"success"`
		Errors  []cfAPIError    `json:"errors"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("Cloudflare 响应解析失败: %w", err)
	}
	if !env.Success {
		he := &cfHTTPError{Status: resp.StatusCode, Message: resp.Status}
		if len(env.Errors) > 0 {
			he.Code = env.Errors[0].Code
			he.Message = env.Errors[0].Message
		}
		return he
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("Cloudflare 数据解析失败: %w", err)
	}
	return nil
}

/* ---------- API 数据结构（字段取自 CF Pages API 实际返回） ---------- */

type cfProject struct {
	Name             string        `json:"name"`
	Subdomain        string        `json:"subdomain"`
	Domains          []string      `json:"domains"`
	ProductionBranch string        `json:"production_branch"`
	CreatedOn        string        `json:"created_on"`
	LatestDeployment *cfDeployment `json:"latest_deployment"`
}

type cfStage struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	StartedOn string `json:"started_on"`
	EndedOn   string `json:"ended_on"`
}

type cfDeployment struct {
	ID          string   `json:"id"`
	ShortID     string   `json:"short_id"`
	Environment string   `json:"environment"` // production | preview
	URL         string   `json:"url"`
	CreatedOn   string   `json:"created_on"`
	ModifiedOn  string   `json:"modified_on"`
	IsSkipped   bool     `json:"is_skipped"`
	Aliases     []string `json:"aliases"`
	LatestStage cfStage  `json:"latest_stage"`

	DeploymentTrigger struct {
		Type     string `json:"type"` // github:push | ad_hoc | ...
		Metadata struct {
			Branch        string `json:"branch"`
			CommitHash    string `json:"commit_hash"`
			CommitMessage string `json:"commit_message"`
			CommitDirty   bool   `json:"commit_dirty"`
		} `json:"metadata"`
	} `json:"deployment_trigger"`
}

func (c *cloudflareClient) listProjects(ctx context.Context) ([]cfProject, error) {
	var all []cfProject
	if err := c.get(ctx, "/accounts/"+c.accountID+"/pages/projects", nil, &all); err != nil {
		return nil, err
	}
	if len(c.projects) == 0 {
		return all, nil
	}
	keep := make(map[string]bool, len(c.projects))
	for _, name := range c.projects {
		keep[name] = true
	}
	out := make([]cfProject, 0, len(all))
	for _, p := range all {
		if keep[p.Name] {
			out = append(out, p)
		}
	}
	return out, nil
}

func (c *cloudflareClient) listDeployments(ctx context.Context, project string, limit int) ([]cfDeployment, error) {
	query := map[string]string{}
	if limit > 0 {
		query["per_page"] = strconv.Itoa(limit)
	}
	var out []cfDeployment
	err := c.get(ctx, "/accounts/"+c.accountID+"/pages/projects/"+url.PathEscape(project)+"/deployments", query, &out)
	return out, err
}

/* ---------- 给前端的视图 ---------- */

type pagesDeploymentView struct {
	ID          string   `json:"id"`
	ShortID     string   `json:"short_id"`
	Environment string   `json:"environment"`
	URL         string   `json:"url"`
	Aliases     []string `json:"aliases"`

	// status 归一化成 GitHub Actions 的运行态，前端复用同一套状态样式
	Status string `json:"status"`
	Stage  string `json:"stage"` // 最后执行的阶段，如 build / deploy

	Branch        string `json:"branch"`
	CommitHash    string `json:"commit_hash"`
	CommitShort   string `json:"commit_short"`
	CommitMessage string `json:"commit_message"`
	CommitDirty   bool   `json:"commit_dirty"`
	TriggerType   string `json:"trigger_type"`

	CreatedOn   string `json:"created_on"`
	DurationSec int64  `json:"duration_sec"`
}

type pagesProjectView struct {
	Name string `json:"name"`
	// DisplayName 是对应 GitHub 仓库的真实名称（大小写正确，如 Remind-web）。
	// Cloudflare 侧名称一律小写（remind-web），展示时优先用它，查不到时同 Name。
	DisplayName      string                `json:"display_name"`
	Subdomain        string                `json:"subdomain"`
	CustomDomains    []string              `json:"custom_domains"`
	SiteURL          string                `json:"site_url"`
	DashboardURL     string                `json:"dashboard_url"`
	ProductionBranch string                `json:"production_branch"`
	Platform         string                `json:"platform"` // 站点主体在 Cloudflare Pages
	Latest           *pagesDeploymentView  `json:"latest"`
	Recent           []pagesDeploymentView `json:"recent"`
}

func buildPagesProjectView(p cfProject, deps []cfDeployment, accountID string, repoNames map[string]string) pagesProjectView {
	v := pagesProjectView{
		Name:             p.Name,
		DisplayName:      lookupRepoDisplayName(repoNames, p.Name),
		Subdomain:        p.Subdomain,
		ProductionBranch: p.ProductionBranch,
		SiteURL:          "https://" + p.Subdomain,
		DashboardURL:     "https://dash.cloudflare.com/" + accountID + "/pages/view/" + p.Name,
		Platform:         PlatformCF, // 站点主体托管在 Cloudflare Pages
		Recent:           []pagesDeploymentView{},
	}
	for _, d := range p.Domains {
		if !strings.HasSuffix(d, ".pages.dev") {
			v.CustomDomains = append(v.CustomDomains, d)
		}
	}
	// 有自定义域名时优先打开自定义域名
	if len(v.CustomDomains) > 0 {
		v.SiteURL = "https://" + v.CustomDomains[0]
	}

	// 部署列表拉取失败时退回项目自带的 latest_deployment，至少还能显示一条
	if len(deps) == 0 && p.LatestDeployment != nil {
		deps = []cfDeployment{*p.LatestDeployment}
	}
	for _, d := range deps {
		v.Recent = append(v.Recent, toPagesDeploymentView(d))
	}
	if len(v.Recent) > 0 {
		latest := v.Recent[0]
		v.Latest = &latest
	}
	return v
}

func toPagesDeploymentView(d cfDeployment) pagesDeploymentView {
	vm := pagesDeploymentView{
		ID:          d.ID,
		ShortID:     d.ShortID,
		Environment: d.Environment,
		URL:         d.URL,
		Aliases:     d.Aliases,
		Status:      pagesRunStatus(d),
		Stage:       d.LatestStage.Name,
		Branch:      d.DeploymentTrigger.Metadata.Branch,
		CommitHash:  d.DeploymentTrigger.Metadata.CommitHash,
		CommitDirty: d.DeploymentTrigger.Metadata.CommitDirty,
		TriggerType: d.DeploymentTrigger.Type,
		CreatedOn:   d.CreatedOn,
	}
	if m := d.DeploymentTrigger.Metadata.CommitMessage; m != "" {
		vm.CommitMessage = m
	}
	if len(vm.CommitHash) >= 7 {
		vm.CommitShort = vm.CommitHash[:7]
	} else {
		vm.CommitShort = vm.CommitHash
	}
	start, okStart := parseRFC3339(d.CreatedOn)
	end, okEnd := parseRFC3339(d.ModifiedOn)
	if okStart && okEnd && end.After(start) {
		vm.DurationSec = int64(end.Sub(start).Seconds())
	}
	return vm
}

// pagesRunStatus 把 CF 的阶段状态映射为前端已支持的 GitHub 运行态。
func pagesRunStatus(d cfDeployment) string {
	if d.IsSkipped {
		return "skipped"
	}
	switch d.LatestStage.Status {
	case "success":
		return "success"
	case "failure":
		return "failure"
	case "canceled", "cancelled":
		return "cancelled"
	case "active":
		return "in_progress"
	case "queued", "idle", "":
		// 已结束但最后阶段没状态时，按最终结果兜底
		if d.LatestStage.EndedOn != "" {
			return "success"
		}
		return "queued"
	default:
		return d.LatestStage.Status
	}
}

func parseRFC3339(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

/* ---------- Workers 静态托管（Workers Static Assets） ----------
 *
 * 前端站点除了 Pages，还有一类是「纯静态 Worker」（wrangler.jsonc 配 assets，
 * 如 remind-web）。它们不在 Pages API 里，部署记录走 Workers Versions API。
 * 这里把这类 Worker 也转成 pagesProjectView，与 Pages 站点同列表展示。
 */

type cfWorkerScript struct {
	ID         string `json:"id"`
	CreatedOn  string `json:"created_on"`
	ModifiedOn string `json:"modified_on"`
}

// listWorkerScripts 列出账号下全部 Worker。Token 需要 Workers Scripts:Read。
func (c *cloudflareClient) listWorkerScripts(ctx context.Context) ([]cfWorkerScript, error) {
	var out []cfWorkerScript
	err := c.get(ctx, "/accounts/"+c.accountID+"/workers/scripts", nil, &out)
	return out, err
}

type cfWorkerDomain struct {
	Hostname string `json:"hostname"`
	Service  string `json:"service"`
}

// listWorkerDomains 拿全部 Workers 自定义域，一次拉全再按 service（script 名）分组。
func (c *cloudflareClient) listWorkerDomains(ctx context.Context) ([]cfWorkerDomain, error) {
	var out []cfWorkerDomain
	err := c.get(ctx, "/accounts/"+c.accountID+"/workers/domains", nil, &out)
	return out, err
}

// getWorkerSubdomain 拿 workers.dev 子域（workers_dev=false 的站点用不上，失败不致命）。
func (c *cloudflareClient) getWorkerSubdomain(ctx context.Context) (string, error) {
	var out struct {
		Subdomain string `json:"subdomain"`
	}
	err := c.get(ctx, "/accounts/"+c.accountID+"/workers/subdomain", nil, &out)
	return out.Subdomain, err
}

/* ---------- Workers 服务详情（用于识别静态托管） ----------
 *
 * GET /accounts/<id>/workers/services/<name> 返回的 script 里有 has_assets 字段，
 * 这是「该 Worker 是否托管静态资产」的官方标志：
 *
 *   music-worker  has_assets=false  纯 API Worker
 *   user-center   has_assets=false  纯 API Worker
 *   remind-web    has_assets=true   静态站点（Workers Static Assets）
 *
 * 实测 2026-11-06。有了它就不必再靠 CF_WORKERS_SITES 白名单人工声明。
 */

type cfWorkerServiceDetail struct {
	DefaultEnvironment struct {
		Script struct {
			ID                 string `json:"id"`
			HasAssets          bool   `json:"has_assets"`
			LastDeployedFrom   string `json:"last_deployed_from"`
			CompatibilityDate  string `json:"compatibility_date"`
		} `json:"script"`
	} `json:"default_environment"`
}

// getWorkerService 取单个 Worker 的服务详情（含 has_assets）。
func (c *cloudflareClient) getWorkerService(ctx context.Context, script string) (*cfWorkerServiceDetail, error) {
	var out cfWorkerServiceDetail
	err := c.get(ctx, "/accounts/"+c.accountID+"/workers/services/"+url.PathEscape(script), nil, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

/* ---------- Workers Builds（CF 侧构建） ----------
 *
 * ⚠️ 重要结论（实测 2026-11-06）：Workers Builds 目前**没有对 API Token 开放的
 * REST 端点**。以下路径全部返回 401 code 12006「Invalid token」：
 *
 *   GET /accounts/<id>/builds/workers
 *   GET /accounts/<id>/builds
 *   GET /accounts/<id>/builds/workers/<script>/builds
 *
 * 同时 `/accounts/<id>/workers/builds_config` 返回 404 code 10404
 * 「No route for that URI」，可确认该 API 未公开。
 *
 * 这个 401 与「权限不足」无关——同一 token 访问 r2/buckets、billing/profile、
 * workers/scripts 等全部 200，即使给 token 授予 account 级全部 286+ 权限项，
 * builds/* 仍然 401。它是内部/未发布 API，要靠 Cloudflare 后续开放。
 *
 * 因此采集侧不依赖 Workers Builds，改用两条**已开放**的官方链路：
 *
 *   1. GET /accounts/<id>/workers/services/<name>
 *        → script.has_assets 判断是否静态托管（替代人工白名单）
 *   2. GET /accounts/<id>/workers/scripts/<name>/versions
 *        → 版本记录，部署成功即产生一条
 *   3. GET /accounts/<id>/workers/scripts/<name>/deployments
 *        → 部署记录，含 source（wrangler / 其他）与 created_on
 *
 * 三条都在现有 token 权限内。若 Cloudflare 日后开放 builds API，只需在此
 * 追加实现，上层视图结构无需改动（pagesDeploymentView 已通用）。
 */

/* ---------- Workers 部署记录 ----------
 *
 * 用 /workers/scripts/<name>/deployments（已开放）取部署历史。
 * 返回的 source 字段标明部署来源：wrangler（CI 或本地推）/ api 等，
 * 配合 annotations 可区分是否为自动构建。
 *
 * 注意：Workers Builds 的 REST API 未公开（见上文说明），所以这里拿不到
 * 「构建中」这种瞬时状态；deployment 一旦出现即代表已成功上线。
 */

type cfWorkerDeployment struct {
	ID        string `json:"id"`
	CreatedOn string `json:"created_on"`
	Source    string `json:"source"` // wrangler | api | ...
	Strategy  string `json:"strategy"`
	Annotations struct {
		WorkersTriggeredBy string `json:"workers/triggered_by"`
		Author             string `json:"workers/author"`
	} `json:"annotations"`
}

// listWorkerDeployments 取某个 Worker 的最近部署（新→旧）。
func (c *cloudflareClient) listWorkerDeployments(ctx context.Context, script string, limit int) ([]cfWorkerDeployment, error) {
	var resp struct {
		Deployments []cfWorkerDeployment `json:"deployments"`
	}
	query := map[string]string{}
	if limit > 0 {
		query["per_page"] = strconv.Itoa(limit)
	}
	err := c.get(ctx, "/accounts/"+c.accountID+"/workers/scripts/"+url.PathEscape(script)+"/deployments", query, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Deployments, nil
}

type cfWorkerVersion struct {
	ID     string `json:"id"`
	Number int64  `json:"number"`
	// created_on 在 metadata 里（顶层只有 id/number），见 CF API 文档
	Metadata struct {
		CreatedOn string `json:"created_on"`
	} `json:"metadata"`
}

// listWorkerVersions 取静态 Worker 的最近部署版本（新→旧）。
// Workers 版本只有部署成功才会存在，状态一律视为 success。
func (c *cloudflareClient) listWorkerVersions(ctx context.Context, script string, limit int) ([]cfWorkerVersion, error) {
	var resp struct {
		Items []cfWorkerVersion `json:"items"`
	}
	query := map[string]string{}
	if limit > 0 {
		query["per_page"] = strconv.Itoa(limit)
	}
	err := c.get(ctx, "/accounts/"+c.accountID+"/workers/scripts/"+url.PathEscape(script)+"/versions", query, &resp)
	return resp.Items, err
}

// buildWorkerProjectView 把 Workers 静态站转成与 Pages 相同的视图，前端复用一套渲染。
//
// 部署记录优先用 deployments（含 source 信息），为空时退回 versions。
func buildWorkerProjectView(w cfWorkerScript, domains []cfWorkerDomain, subdomain string, versions []cfWorkerVersion, deps []cfWorkerDeployment, accountID string, repoNames map[string]string) pagesProjectView {
	v := pagesProjectView{
		Name:         w.ID,
		DisplayName:  lookupRepoDisplayName(repoNames, w.ID),
		SiteURL:      "",
		DashboardURL: "https://dash.cloudflare.com/" + accountID + "/workers/services/view/" + w.ID,
		Platform:     PlatformCF,
		Recent:       []pagesDeploymentView{},
	}
	for _, d := range domains {
		if d.Service == w.ID {
			v.CustomDomains = append(v.CustomDomains, d.Hostname)
		}
	}
	if subdomain != "" {
		v.Subdomain = w.ID + "." + subdomain
	}
	// workers_dev=false（如 remind-web）时没有 workers.dev 地址，优先自定义域
	if len(v.CustomDomains) > 0 {
		v.SiteURL = "https://" + v.CustomDomains[0]
	} else if v.Subdomain != "" {
		v.SiteURL = "https://" + v.Subdomain
	}

	if len(deps) > 0 {
		for _, d := range deps {
			v.Recent = append(v.Recent, toWorkerDeploymentView(d))
		}
	} else {
		for _, ver := range versions {
			vm := pagesDeploymentView{
				ID:          ver.ID,
				Environment: "production",
				// 版本存在即部署成功；Workers 没有独立 build 阶段，stage 记为 deploy
				Status:    "success",
				Stage:     "deploy",
				CreatedOn: ver.Metadata.CreatedOn,
			}
			// 版本号即部署 ID 前缀，可当作短标识；commit 对应关系由 Actions 页承担，这里留空
			if len(ver.ID) > 8 {
				vm.CommitShort = ver.ID[:8]
			} else {
				vm.CommitShort = ver.ID
			}
			v.Recent = append(v.Recent, vm)
		}
	}
	if len(v.Recent) > 0 {
		latest := v.Recent[0]
		v.Latest = &latest
	}
	return v
}

// toWorkerDeploymentView 把 Workers deployment 记录转成通用部署视图。
func toWorkerDeploymentView(d cfWorkerDeployment) pagesDeploymentView {
	vm := pagesDeploymentView{
		ID:          d.ID,
		Environment: "production",
		// deployment 存在即已上线；Workers 部署是原子的，没有中间态
		Status:      "success",
		Stage:       "deploy",
		TriggerType: d.Source,
		CreatedOn:   d.CreatedOn,
	}
	if len(d.ID) > 8 {
		vm.ShortID = d.ID[:8]
		vm.CommitShort = d.ID[:8]
	} else {
		vm.ShortID = d.ID
		vm.CommitShort = d.ID
	}
	return vm
}

