package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

func TestClassifyHealth(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
		want string
	}{
		{"正常 JSON", 200, `{"ok":true}`, "up"},
		{"普通 200", 200, `hello`, "up"},
		{"ok=false", 200, `{"ok":false,"error":"db down"}`, "down"},
		{"开放时段暂停", 200, `{"open":false,"reason":"工作日上班时间暂停开放"}`, "paused"},
		{"open=true", 200, `{"open":true}`, "up"},
		{"503 暂停", 503, `{"detail":"工作日上班时间暂停开放"}`, "paused"},
		{"503 无体", 503, ``, "paused"},
		{"500 故障", 500, `boom`, "down"},
		{"401 鉴权", 401, `{"detail":"需要两步验证"}`, "down"},
		{"302 跳转", 302, ``, "up"},
	}
	for _, c := range cases {
		state, _ := classifyHealth(c.code, []byte(c.body))
		if state != c.want {
			t.Errorf("%s: state=%q，期望 %q", c.name, state, c.want)
		}
	}
}

// 探测真实 HTTP 服务：验证耗时、状态码归类与提示文案
func TestMonitorProbe(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	paused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"detail":"工作日上班时间暂停开放（09:00-18:00），10-20 18:00 恢复"}`))
	}))
	defer paused.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	m := newMonitor(nil, time.Minute)

	c := m.probe(context.Background(), up.URL)
	if c.State != "up" || c.Code != 200 {
		t.Errorf("up: state=%q code=%d", c.State, c.Code)
	}

	c = m.probe(context.Background(), paused.URL)
	if c.State != "paused" || c.Code != 503 || c.Message == "" {
		t.Errorf("paused: state=%q code=%d msg=%q", c.State, c.Code, c.Message)
	}

	c = m.probe(context.Background(), bad.URL)
	if c.State != "down" || c.Code != 500 {
		t.Errorf("bad: state=%q code=%d", c.State, c.Code)
	}

	// 连不上的地址：归为 down 且带错误信息
	c = m.probe(context.Background(), "http://127.0.0.1:1/")
	if c.State != "down" || c.Message == "" {
		t.Errorf("unreachable: state=%q msg=%q", c.State, c.Message)
	}
}

// 历史记录：最多保留 60 条，uptime 按「up+paused」占比计算
func TestMonitorHistoryAndUptime(t *testing.T) {
	m := newMonitor([]monitorTarget{{Key: "x", Name: "x", URL: "http://x.test"}}, time.Minute)
	m.mu.Lock()
	m.history["x"] = []monitorCheck{
		{State: "up"}, {State: "up"}, {State: "paused"}, {State: "down"},
	}
	m.mu.Unlock()

	v := m.views()
	if len(v) != 1 {
		t.Fatalf("views 长度 = %d", len(v))
	}
	if v[0].State != "down" {
		t.Errorf("state 取最新一条，实际 %q", v[0].State)
	}
	if v[0].Uptime != 75 { // 3/4
		t.Errorf("uptime = %d，期望 75", v[0].Uptime)
	}

	// 写入超过上限后截断，仅保留最近 60 条
	target := monitorTarget{Key: "y", Name: "y", URL: "http://y.test"}
	m.targets = []monitorTarget{target}
	for i := 0; i < monitorHistorySize+10; i++ {
		m.mu.Lock()
		h := append(m.history["y"], monitorCheck{State: "up", At: int64(i)})
		if len(h) > monitorHistorySize {
			h = h[len(h)-monitorHistorySize:]
		}
		m.history["y"] = h
		m.mu.Unlock()
	}
	m.mu.RLock()
	n := len(m.history["y"])
	lastAt := m.history["y"][n-1].At
	m.mu.RUnlock()
	if n != monitorHistorySize {
		t.Errorf("历史长度 = %d，期望 %d", n, monitorHistorySize)
	}
	if lastAt != int64(monitorHistorySize+9) {
		t.Errorf("应保留最新记录，最后一条 at=%d", lastAt)
	}
}
