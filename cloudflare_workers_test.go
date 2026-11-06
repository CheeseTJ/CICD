package main

import "testing"

// 迁移期语义：同一站点同时存在于 Pages 与 Workers 时，以 Worker 为准。
func TestMergeWorkerOverPages_PrefersWorkerOnSameHost(t *testing.T) {
	pages := []pagesProjectView{
		{Name: "jmreader", CustomDomains: []string{"jm.taojun.top"}, Platform: "pages"},
		{Name: "land-page", CustomDomains: []string{"taojun.top"}, Platform: "pages"},
	}
	workers := []pagesProjectView{
		{Name: "jmreader", CustomDomains: []string{"jm.taojun.top"}, Platform: "worker"},
	}

	got := mergeWorkerOverPages(pages, workers)

	if len(got) != 2 {
		t.Fatalf("应保留 2 个站点，实际 %d", len(got))
	}
	byName := map[string]string{}
	for _, v := range got {
		byName[v.Name] = v.Platform
	}
	if byName["jmreader"] != "worker" {
		t.Errorf("jmreader 应取 Worker 视图，实际 platform=%q", byName["jmreader"])
	}
	if byName["land-page"] != "pages" {
		t.Errorf("land-page 无对应 Worker，应保留 Pages 视图，实际 platform=%q", byName["land-page"])
	}
}

// 自定义域大小写不同也要能匹配上（Cloudflare 侧名称大小写不统一）。
func TestMergeWorkerOverPages_HostMatchIgnoresCase(t *testing.T) {
	pages := []pagesProjectView{
		{Name: "wordrush-web", CustomDomains: []string{"Eng.TaoJun.Top"}, Platform: "pages"},
	}
	workers := []pagesProjectView{
		{Name: "wordrush-web", CustomDomains: []string{"eng.taojun.top"}, Platform: "worker"},
	}

	got := mergeWorkerOverPages(pages, workers)

	if len(got) != 1 {
		t.Fatalf("应合并为 1 个站点，实际 %d", len(got))
	}
	if got[0].Platform != "worker" {
		t.Errorf("应取 Worker 视图，实际 platform=%q", got[0].Platform)
	}
}

// 纯新增的 Worker（Pages 里完全没有）要出现在结果里。
func TestMergeWorkerOverPages_AppendsWorkerOnlySites(t *testing.T) {
	pages := []pagesProjectView{
		{Name: "center", CustomDomains: []string{"center.taojun.top"}, Platform: "pages"},
	}
	workers := []pagesProjectView{
		{Name: "remind-web", CustomDomains: []string{"remind.taojun.top"}, Platform: "worker"},
	}

	got := mergeWorkerOverPages(pages, workers)

	if len(got) != 2 {
		t.Fatalf("应为 2 个站点，实际 %d", len(got))
	}
	names := map[string]bool{}
	for _, v := range got {
		names[v.Name] = true
	}
	if !names["remind-web"] {
		t.Error("仅存在于 Worker 的 remind-web 应被追加")
	}
}

// 没有 Worker 数据时原样返回 Pages，不能丢失任何站点。
func TestMergeWorkerOverPages_EmptyWorkersKeepsPages(t *testing.T) {
	pages := []pagesProjectView{
		{Name: "a", CustomDomains: []string{"a.example.com"}},
		{Name: "b"},
	}

	got := mergeWorkerOverPages(pages, nil)

	if len(got) != len(pages) {
		t.Fatalf("应原样返回 %d 个，实际 %d", len(pages), len(got))
	}
}

// Worker deployment 记录到通用视图的转换。
func TestToWorkerDeploymentView(t *testing.T) {
	d := cfWorkerDeployment{
		ID:        "db6c30b3-236b-473e-a606-9ebe655aea8c",
		CreatedOn: "2026-09-28T09:38:44Z",
		Source:    "wrangler",
		Strategy:  "percentage",
	}
	got := toWorkerDeploymentView(d)

	if got.Status != "success" {
		t.Errorf("deployment 存在即已上线，status 应为 success，实际 %q", got.Status)
	}
	if got.TriggerType != "wrangler" {
		t.Errorf("trigger_type 应记录来源 wrangler，实际 %q", got.TriggerType)
	}
	if got.ShortID != "db6c30b3" {
		t.Errorf("short_id 应为前 8 位，实际 %q", got.ShortID)
	}
	if got.CreatedOn != "2026-09-28T09:38:44Z" {
		t.Errorf("created_on 应透传，实际 %q", got.CreatedOn)
	}
}
