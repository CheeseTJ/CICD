package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// cloudflareClient 只读访问 Cloudflare Pages / Workers API，用于在控制台展示前端站点的部署记录。
type cloudflareClient struct {
	http        *http.Client
	token       string   // CLOUDFLARE_API_TOKEN（Pages:Read + Workers Scripts:Read）
	accountID   string   // CLOUDFLARE_ACCOUNT_ID
	projects    []string // 可选白名单（CF_PAGES_PROJECTS），为空表示展示全部 Pages 项目
	workerSites []string // 显式标识（CF_WORKERS_SITES）：哪些 Worker 是前端静态站，为空则不展示 Workers 站点

	// buildsUnavailable 一旦探测到 token 缺 Workers Builds 权限就置 true，
	// 后续调用直接降级，不再对每个站点重复打 401 请求。
	buildsUnavailable atomic.Bool
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

/* ---------- Workers Builds（CF 侧构建） ----------
 *
 * Pages 迁移到 Workers 后，若继续由 Cloudflare 监听 GitHub push 自动构建，
 * 部署记录走 Workers Builds API：
 *
 *   GET /accounts/<id>/builds/workers/<script>/builds
 *
 * 与 Pages 不同，这条链路需要独立的 Workers Builds:Read 权限，且是**新的**
 * token 权限项。旧 token 未授权时返回 401（code 12006 Invalid token）。
 *
 * 因此这里按「能力探测」处理：首次 401 即标记 unavailable，整个进程生命周期
 * 不再重试，调用方降级到 Workers Versions API（只反映"已部署成功"的版本，
 * 没有构建中的状态）。权限补齐后重启服务即可自动启用，无需改代码。
 */

var errWorkersBuildsUnavailable = errors.New("当前 token 缺少 Workers Builds:Read 权限")

type cfWorkerBuild struct {
	ID        string `json:"id"`
	CreatedOn string `json:"created_on"`
	UpdatedOn string `json:"updated_on"`
	Status    string `json:"status"` // queued | running | success | failure | canceled
	// 构建的触发来源，GitHub 集成时 type=github
	Trigger struct {
		Type     string `json:"type"`
		Metadata struct {
			Branch        string `json:"branch"`
			CommitHash    string `json:"commit_hash"`
			CommitMessage string `json:"commit_message"`
		} `json:"metadata"`
	} `json:"trigger"`
}

// listWorkerBuilds 取某个 Worker 的最近构建（新→旧）。
// 权限不足时返回 errWorkersBuildsUnavailable，调用方据此降级。
func (c *cloudflareClient) listWorkerBuilds(ctx context.Context, script string, limit int) ([]cfWorkerBuild, error) {
	if c.buildsUnavailable.Load() {
		return nil, errWorkersBuildsUnavailable
	}
	query := map[string]string{}
	if limit > 0 {
		query["per_page"] = strconv.Itoa(limit)
	}
	var resp []cfWorkerBuild
	err := c.get(ctx, "/accounts/"+c.accountID+"/builds/workers/"+url.PathEscape(script)+"/builds", query, &resp)
	if err != nil {
		// 401/403 视为权限缺失，打标记后全程降级，避免每个站点都打一次日志
		if isAuthError(err) {
			if c.buildsUnavailable.CompareAndSwap(false, true) {
				log.Printf("Workers Builds 不可用（token 缺 Workers Builds:Read），" +
					"部署记录降级为 Workers Versions API")
			}
			return nil, errWorkersBuildsUnavailable
		}
		return nil, err
	}
	return resp, nil
}

// isAuthError 判断 CF API 错误是否为鉴权类（401/403 / code 12006）。
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	var ae *cfHTTPError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden ||
			ae.Code == 12006 || ae.Code == 10000
	}
	return false
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
// builds 非空时优先用 Workers Builds 的记录（含构建中/失败等完整状态）；
// 为空（token 缺 Workers Builds:Read）则退回 versions（只有成功的部署）。
func buildWorkerProjectView(w cfWorkerScript, domains []cfWorkerDomain, subdomain string, versions []cfWorkerVersion, builds []cfWorkerBuild, accountID string, repoNames map[string]string) pagesProjectView {
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

	if len(builds) > 0 {
		for _, b := range builds {
			v.Recent = append(v.Recent, toWorkerBuildView(b))
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

// toWorkerBuildView 把 Workers Builds 记录转成通用部署视图。
func toWorkerBuildView(b cfWorkerBuild) pagesDeploymentView {
	vm := pagesDeploymentView{
		ID:            b.ID,
		Environment:   "production",
		Status:        workerBuildStatus(b.Status),
		Stage:         "build",
		Branch:        b.Trigger.Metadata.Branch,
		CommitHash:    b.Trigger.Metadata.CommitHash,
		CommitMessage: b.Trigger.Metadata.CommitMessage,
		TriggerType:   b.Trigger.Type,
		CreatedOn:     b.CreatedOn,
	}
	if len(vm.CommitHash) >= 7 {
		vm.CommitShort = vm.CommitHash[:7]
	} else {
		vm.CommitShort = vm.CommitHash
	}
	if len(vm.ID) > 8 {
		vm.ShortID = vm.ID[:8]
	} else {
		vm.ShortID = vm.ID
	}
	start, okStart := parseRFC3339(b.CreatedOn)
	end, okEnd := parseRFC3339(b.UpdatedOn)
	if okStart && okEnd && end.After(start) {
		vm.DurationSec = int64(end.Sub(start).Seconds())
	}
	return vm
}

// workerBuildStatus 把 Workers Builds 的状态映射为前端已支持的运行态。
func workerBuildStatus(s string) string {
	switch strings.ToLower(s) {
	case "success", "succeeded":
		return "success"
	case "failure", "failed":
		return "failure"
	case "canceled", "cancelled":
		return "cancelled"
	case "running", "active":
		return "in_progress"
	case "queued", "pending":
		return "queued"
	case "skipped":
		return "skipped"
	default:
		if s == "" {
			return "queued"
		}
		return strings.ToLower(s)
	}
}

