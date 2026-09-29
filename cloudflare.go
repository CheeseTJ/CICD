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

// cloudflareClient 只读访问 Cloudflare Pages API，用于在控制台展示前端站点的部署记录。
type cloudflareClient struct {
	http      *http.Client
	token     string   // CLOUDFLARE_API_TOKEN（只需 Pages:Read）
	accountID string   // CLOUDFLARE_ACCOUNT_ID
	projects  []string // 可选白名单（CF_PAGES_PROJECTS），为空表示展示全部 Pages 项目
}

func (c *cloudflareClient) ready() bool {
	return c != nil && c.token != "" && c.accountID != ""
}

const cfAPIBase = "https://api.cloudflare.com/client/v4"

type cfAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
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
		msg := resp.Status
		if len(env.Errors) > 0 {
			msg = env.Errors[0].Message
		}
		return fmt.Errorf("Cloudflare API 错误: %s", msg)
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
	Name             string                `json:"name"`
	Subdomain        string                `json:"subdomain"`
	CustomDomains    []string              `json:"custom_domains"`
	SiteURL          string                `json:"site_url"`
	DashboardURL     string                `json:"dashboard_url"`
	ProductionBranch string                `json:"production_branch"`
	Platform         string                `json:"platform"` // 站点主体在 Cloudflare Pages
	Latest           *pagesDeploymentView  `json:"latest"`
	Recent           []pagesDeploymentView `json:"recent"`
}

func buildPagesProjectView(p cfProject, deps []cfDeployment, accountID string) pagesProjectView {
	v := pagesProjectView{
		Name:             p.Name,
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

type cfWorkerBinding struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type cfWorkerSettings struct {
	Bindings []cfWorkerBinding `json:"bindings"`
	// 纯静态 Worker 的 assets 配置出现在 settings 顶层（也可能以 assets binding 出现），
	// 用 RawMessage 接住，存在即视为静态站。
	Assets json.RawMessage `json:"assets"`
}

// isStaticWorker 判断该 Worker 是否托管静态资产（前端站点）：
// API 型 Worker（如 music-worker）没有 assets，不会混进前端站点页。
func isStaticWorker(s cfWorkerSettings) bool {
	if len(s.Assets) > 0 && string(s.Assets) != "null" {
		return true
	}
	for _, b := range s.Bindings {
		if b.Type == "assets" {
			return true
		}
	}
	return false
}

func (c *cloudflareClient) listWorkerSettings(ctx context.Context, script string) (cfWorkerSettings, error) {
	var out cfWorkerSettings
	err := c.get(ctx, "/accounts/"+c.accountID+"/workers/scripts/"+url.PathEscape(script)+"/settings", nil, &out)
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
func buildWorkerProjectView(w cfWorkerScript, domains []cfWorkerDomain, subdomain string, versions []cfWorkerVersion, accountID string) pagesProjectView {
	v := pagesProjectView{
		Name:         w.ID,
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
	if len(v.Recent) > 0 {
		latest := v.Recent[0]
		v.Latest = &latest
	}
	return v
}
