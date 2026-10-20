package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseTargets(t *testing.T) {
	cases := []struct {
		raw  string
		want int
		url  string
		name string
	}{
		{"jm=https://a.test/api/schedule", 1, "https://a.test/api/schedule", "jm"},
		{"https://b.test/api/health", 1, "https://b.test/api/health", "b.test"},
		{"a=https://a.test, b=https://b.test;c=https://c.test", 3, "", ""},
		{"a=https://a.test,a=https://dup.test", 1, "", ""}, // 同名去重
		{"ftp://x.test", 0, "", ""},                        // 协议不支持
		{"", 0, "", ""},
	}
	for _, c := range cases {
		got := parseTargets(c.raw)
		if len(got) != c.want {
			t.Errorf("parseTargets(%q) = %d 项，期望 %d", c.raw, len(got), c.want)
			continue
		}
		if c.url != "" && got[0].URL != c.url {
			t.Errorf("URL = %q，期望 %q", got[0].URL, c.url)
		}
		if c.name != "" && got[0].Name != c.name {
			t.Errorf("Name = %q，期望 %q（未写名称时取域名）", got[0].Name, c.name)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:8765/health": true,
		"http://localhost:8090/":       true,
		"http://[::1]:8080/health":     true,
		"https://jm.example.com/":      false,
		"https://10.0.0.5/health":      false, // 内网地址仍算非回环：浏览器可能够不到，
		// 但这里只回答「是不是本机」，由跨域探测自行判定
	}
	for u, want := range cases {
		if got := isLoopback(u); got != want {
			t.Errorf("isLoopback(%q) = %v，期望 %v", u, got, want)
		}
	}
}

// 跨域判定：对方回显 * 或回显本控制台来源 = 允许；不回显 = 受限
func TestProbeCORS(t *testing.T) {
	// httptest 跑在 127.0.0.1，会被「回环不可直连」短路；这里临时关掉该规则
	old := loopbackFn
	loopbackFn = func(string) bool { return false }
	defer func() { loopbackFn = old }()

	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"通配符", "*", true},
		{"精确回显", "https://cicd.example.com", true},
		{"不返回头", "", false},
		{"回显了别的来源", "https://other.example.com", false},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 有头才写：模拟真实服务对未授权来源不回 CORS 头
			if c.header != "" {
				w.Header().Set("Access-Control-Allow-Origin", c.header)
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}))
		s := &server{
			cfg:     &config{PublicOrigin: "https://cicd.example.com"},
			monHTTP: srv.Client(),
		}
		if got := s.probeCORS(context.Background(), srv.URL); got != c.want {
			t.Errorf("%s: probeCORS = %v，期望 %v", c.name, got, c.want)
		}
		srv.Close()
	}
}

// 本机地址一律判为不可直连（浏览器访问不到），不再发起请求
func TestProbeCORSLoopback(t *testing.T) {
	s := &server{cfg: &config{}, monHTTP: &http.Client{}}
	if s.probeCORS(context.Background(), "http://127.0.0.1:1/health") {
		t.Error("回环地址应判为跨域受限")
	}
}

// selfOrigin：配置优先，未配置时兜底 *
func TestSelfOrigin(t *testing.T) {
	s := &server{cfg: &config{PublicOrigin: "https://cicd.example.com/"}}
	if got := s.selfOrigin(); got != "https://cicd.example.com" {
		t.Errorf("selfOrigin = %q，期望去掉尾部斜杠", got)
	}
	s = &server{cfg: &config{}}
	if got := s.selfOrigin(); got != "*" {
		t.Errorf("未配置时应兜底 *，实际 %q", got)
	}
}

// refreshCrossOrigin 把跨域判定结果写回目标清单
func TestRefreshCrossOrigin(t *testing.T) {
	old := loopbackFn
	loopbackFn = func(u string) bool { return u == "http://127.0.0.1:1/health" }
	defer func() { loopbackFn = old }()

	allow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}))
	defer allow.Close()
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("no cors"))
	}))
	defer deny.Close()

	s := &server{
		cfg: &config{
			PublicOrigin: "https://cicd.example.com",
			HealthTargets: []monitorTarget{
				{Key: "allow", Name: "allow", URL: allow.URL},
				{Key: "deny", Name: "deny", URL: deny.URL},
				{Key: "local", Name: "local", URL: "http://127.0.0.1:1/health"},
			},
		},
		monHTTP: allow.Client(),
	}
	s.refreshCrossOrigin()

	want := map[string]bool{"allow": true, "deny": false, "local": false}
	for _, tg := range s.cfg.HealthTargets {
		if tg.CrossOrigin != want[tg.Key] {
			t.Errorf("%s: CrossOrigin = %v，期望 %v", tg.Key, tg.CrossOrigin, want[tg.Key])
		}
	}
}

// 同源相对路径：浏览器按本页同源请求，不存在 CORS，理应直接判为可直连
func TestIsSameOrigin(t *testing.T) {
	if !isSameOrigin("/api/health") {
		t.Error("相对路径应判为同源")
	}
	if isSameOrigin("https://x.test/api/health") {
		t.Error("绝对地址不应判为同源")
	}
}

func TestParseTargetsSameOrigin(t *testing.T) {
	got := parseTargets("/api/health")
	if len(got) != 1 || got[0].URL != "/api/health" {
		t.Fatalf("相对路径目标应被接受，实际 %+v", got)
	}
	// 未写名称时用路径去掉斜杠兜底
	if got[0].Name != "api/health" {
		t.Errorf("Name = %q，期望 api/health", got[0].Name)
	}
}

func TestRefreshCrossOriginSameOrigin(t *testing.T) {
	s := &server{cfg: &config{HealthTargets: []monitorTarget{
		{Key: "self", Name: "self", URL: "/api/health"},
	}}}
	s.refreshCrossOrigin()
	if !s.cfg.HealthTargets[0].CrossOrigin {
		t.Error("同源相对路径应标记为可直连")
	}
}
