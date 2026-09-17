package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

var errNotFound = errors.New("not found")

type githubClient struct {
	http  *http.Client
	token string
}

type GHRepo struct {
	FullName      string `json:"full_name"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	HTMLURL       string `json:"html_url"`
	Language      string `json:"language"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
	DefaultBranch string `json:"default_branch"`
	UpdatedAt     string `json:"updated_at"`
}

type GHAsset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	URL                string `json:"url"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	ContentType        string `json:"content_type"`
}

type GHRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	PublishedAt string    `json:"published_at"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	Assets      []GHAsset `json:"assets"`
	HTMLURL     string    `json:"html_url"`
}

type GHWorkflow struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
}

type GHWorkflowRun struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	DisplayTitle string `json:"display_title"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	HeadBranch   string `json:"head_branch"`
	Event        string `json:"event"`
	RunNumber    int    `json:"run_number"`
	HTMLURL      string `json:"html_url"`
	RunStartedAt string `json:"run_started_at"`
	UpdatedAt    string `json:"updated_at"`
	HeadCommit   struct {
		Message string `json:"message"`
	} `json:"head_commit"`
}

func (c *githubClient) getJSON(ctx context.Context, url string, out interface{}) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, fmt.Errorf("github api %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// listRepos 拉取仓库列表。owner 为空时使用当前登录用户（含私有仓库），
// 否则拉取指定用户/组织的公开仓库。
func (c *githubClient) listRepos(ctx context.Context, owner string) ([]GHRepo, error) {
	var all []GHRepo
	for page := 1; page <= 10; page++ {
		var u string
		if owner == "" {
			u = fmt.Sprintf("https://api.github.com/user/repos?per_page=100&page=%d&sort=updated", page)
		} else {
			u = fmt.Sprintf("https://api.github.com/users/%s/repos?per_page=100&page=%d&sort=updated", owner, page)
		}
		var repos []GHRepo
		if _, err := c.getJSON(ctx, u, &repos); err != nil {
			return nil, err
		}
		all = append(all, repos...)
		if len(repos) < 100 {
			break
		}
	}
	return all, nil
}

// getLatestRelease 获取最新一条已发布的 release（含预发布，不含草稿）。
func (c *githubClient) getLatestRelease(ctx context.Context, owner, repo string) (*GHRelease, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases?per_page=1", owner, repo)
	var rels []GHRelease
	status, err := c.getJSON(ctx, u, &rels)
	if err != nil {
		if status == http.StatusNotFound {
			return nil, errNotFound
		}
		return nil, err
	}
	if len(rels) == 0 {
		return nil, errNotFound
	}
	return &rels[0], nil
}

// countingWriter 统计写入字节数，用于下载进度。
type countingWriter struct {
	w     io.Writer
	n     int64
	total int64
	cb    func(done, total int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if c.cb != nil {
		c.cb(c.n, c.total)
	}
	return n, err
}

// downloadAsset 通过资产 API 下载文件（私有时需要 token，302 跳转到已签名的对象存储地址）。
func (c *githubClient) downloadAsset(ctx context.Context, asset GHAsset, w io.Writer, progress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("下载资产失败: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	_, err = io.Copy(&countingWriter{w: w, total: asset.Size, cb: progress}, resp.Body)
	return err
}

// listWorkflows 列出仓库配置的 Actions workflow（未开启 Actions 的仓库返回空）。
func (c *githubClient) listWorkflows(ctx context.Context, owner, repo string) ([]GHWorkflow, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/workflows?per_page=100", owner, repo)
	var resp struct {
		TotalCount int          `json:"total_count"`
		Workflows  []GHWorkflow `json:"workflows"`
	}
	status, err := c.getJSON(ctx, u, &resp)
	if err != nil {
		if status == http.StatusNotFound || status == http.StatusForbidden {
			return nil, nil
		}
		return nil, err
	}
	// 过滤 GitHub 自动生成的工作流（如 dependency graph），只保留仓库自己配置的。
	kept := make([]GHWorkflow, 0, len(resp.Workflows))
	for _, wf := range resp.Workflows {
		if strings.HasPrefix(wf.Path, "dynamic/") {
			continue
		}
		kept = append(kept, wf)
	}
	return kept, nil
}

// listRuns 列出最近的 workflow 运行记录。
func (c *githubClient) listRuns(ctx context.Context, owner, repo string, limit int) ([]GHWorkflowRun, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs?per_page=%d", owner, repo, limit)
	var resp struct {
		WorkflowRuns []GHWorkflowRun `json:"workflow_runs"`
	}
	status, err := c.getJSON(ctx, u, &resp)
	if err != nil {
		if status == http.StatusNotFound || status == http.StatusForbidden {
			return nil, nil
		}
		return nil, err
	}
	return resp.WorkflowRuns, nil
}

// getWorkflowContent 拉取 workflow 原文件内容（默认分支），用于判断实际部署平台。
func (c *githubClient) getWorkflowContent(ctx context.Context, owner, repo, path string) ([]byte, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", owner, repo, strings.TrimPrefix(path, "/"))
	var file struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if _, err := c.getJSON(ctx, u, &file); err != nil {
		return nil, err
	}
	if file.Encoding == "base64" {
		return base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	}
	return []byte(file.Content), nil
}

// workflowTexts 读取仓库前 limit 个 workflow 的原文件（供平台检测）。
// 单个失败只记录日志并跳过，不影响其余。
func (c *githubClient) workflowTexts(ctx context.Context, owner, repo string, wfs []GHWorkflow, limit int) []string {
	var out []string
	for i, wf := range wfs {
		if i >= limit {
			break
		}
		content, err := c.getWorkflowContent(ctx, owner, repo, wf.Path)
		if err != nil {
			log.Printf("[platforms] 读取 workflow 失败 %s/%s %s: %v", owner, repo, wf.Path, err)
			continue
		}
		out = append(out, string(content))
	}
	return out
}

// hasApkAsset 判断最新 release 是否包含 .apk 资产。
func hasApkAsset(rel *GHRelease) (bool, []GHAsset) {
	if rel == nil {
		return false, nil
	}
	var apks []GHAsset
	for _, a := range rel.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), ".apk") {
			apks = append(apks, a)
		}
	}
	return len(apks) > 0, apks
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 0, // 长耗时下载/上传由调用方通过 context 控制
	}
}
