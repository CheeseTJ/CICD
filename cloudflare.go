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
