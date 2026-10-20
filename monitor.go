// 服务健康监控：由本服务集中轮询各服务的健康接口，前端只读聚合结果。
//
// 为什么在后端轮询而不是浏览器直接 fetch：
// 1) 被监控服务多数只绑回环地址（经 cloudflared 暴露），浏览器跨域访问会被 CORS 拦下；
// 2) 多个打开的面板会各poll 一份，把上游打出无谓的请求；集中轮询只占用一条采样流；
// 3) 结果带历史，可算「最近 N 次健康率」，浏览器端刷新就丢历史。
//
// 目标配置（HEALTH_TARGETS）：逗号分隔，每项 `名称=URL` 或只写 URL（名称取域名）：
//
//	HEALTH_TARGETS=jmreader=https://jm.june-t.top/api/schedule,music=https://music.june-t.top/api/health
//
// 状态判定：
//
//	up     2xx/3xx，或 JSON 里没有「对外关闭」标记
//	paused 503 或 JSON 中 open=false —— 服务活着但主动暂停（如 JMReader 的工作日时段限制）
//	down   网络错误/超时/4xx/5xx 其它状态码
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	monitorHistorySize = 60              // 每个目标保留最近 60 次探测
	monitorTimeout     = 8 * time.Second // 单次探测超时
	monitorBodyLimit   = 8 << 10         // 只读 8KB 响应体用于解析提示信息
)

// monitorTarget 一个被监控的服务
type monitorTarget struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// monitorCheck 一次探测结果
type monitorCheck struct {
	At        int64  `json:"at"`
	State     string `json:"state"`
	Code      int    `json:"code"`
	LatencyMS int64  `json:"latency_ms"`
	Message   string `json:"message"`
}

// monitorView 给前端的聚合视图
type monitorView struct {
	Key       string         `json:"key"`
	Name      string         `json:"name"`
	URL       string         `json:"url"`
	State     string         `json:"state"`
	Code      int            `json:"code"`
	LatencyMS int64          `json:"latency_ms"`
	Message   string         `json:"message"`
	CheckedAt int64          `json:"checked_at"`
	Uptime    int            `json:"uptime"` // 最近 N 次中「可用（up+paused）」占比
	Checks    []monitorCheck `json:"checks"`
}

// monitor 监控器：后台定时探测，结果只留内存
type monitor struct {
	http     *http.Client
	interval time.Duration
	targets  []monitorTarget

	mu      sync.RWMutex
	history map[string][]monitorCheck
}

// parseTargets 解析 HEALTH_TARGETS；解析不出的项跳过并记日志
func parseTargets(raw string) []monitorTarget {
	var out []monitorTarget
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, rawURL := "", part
		if i := strings.Index(part, "="); i > 0 {
			name, rawURL = strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
		}
		u, err := url.Parse(rawURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			log.Printf("[monitor] 忽略无效目标 %q", part)
			continue
		}
		if name == "" {
			name = u.Host
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, monitorTarget{Key: key, Name: name, URL: rawURL})
	}
	return out
}

func newMonitor(targets []monitorTarget, interval time.Duration) *monitor {
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	return &monitor{
		http:     &http.Client{Timeout: monitorTimeout},
		interval: interval,
		targets:  targets,
		history:  map[string][]monitorCheck{},
	}
}

// start 启动后台轮询（立即探测一次，之后按间隔）
func (m *monitor) start() {
	if len(m.targets) == 0 {
		return
	}
	go func() {
		m.checkAll(context.Background())
		t := time.NewTicker(m.interval)
		defer t.Stop()
		for range t.C {
			m.checkAll(context.Background())
		}
	}()
}

// checkAll 并发探测全部目标
func (m *monitor) checkAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range m.targets {
		wg.Add(1)
		go func(t monitorTarget) {
			defer wg.Done()
			res := m.probe(ctx, t.URL)
			m.mu.Lock()
			h := append(m.history[t.Key], res)
			if len(h) > monitorHistorySize {
				h = h[len(h)-monitorHistorySize:]
			}
			m.history[t.Key] = h
			m.mu.Unlock()
		}(t)
	}
	wg.Wait()
}

// probe 请求一次并归类状态
func (m *monitor) probe(ctx context.Context, target string) monitorCheck {
	ctx, cancel := context.WithTimeout(ctx, monitorTimeout)
	defer cancel()
	start := time.Now()
	res := monitorCheck{At: start.Unix(), State: "down"}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		res.Message = err.Error()
		return res
	}
	req.Header.Set("User-Agent", "cicd-console-monitor/1")
	req.Header.Set("Accept", "application/json, text/plain, */*")

	resp, err := m.http.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Message = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.Code = resp.StatusCode

	body, _ := io.ReadAll(io.LimitReader(resp.Body, monitorBodyLimit))
	state, msg := classifyHealth(resp.StatusCode, body)
	res.State = state
	res.Message = msg
	return res
}

// classifyHealth 依据状态码与响应体判定状态。
// 额外识别「服务活着但主动暂停」：JMReader 的 /api/schedule 在封锁时段返回
// open=false（HTTP 仍 200），其余受限接口返回 503。
func classifyHealth(code int, body []byte) (string, string) {
	var payload struct {
		Detail string `json:"detail"`
		Reason string `json:"reason"`
		Error  string `json:"error"`
		Open   *bool  `json:"open"`
		OK     *bool  `json:"ok"`
	}
	parsed := json.Unmarshal(body, &payload) == nil

	note := ""
	if parsed {
		switch {
		case payload.Reason != "":
			note = payload.Reason
		case payload.Detail != "":
			note = payload.Detail
		case payload.Error != "":
			note = payload.Error
		}
	}

	switch {
	case code >= 200 && code < 400:
		if parsed && payload.Open != nil && !*payload.Open {
			return "paused", firstNonEmpty(note, "服务暂停开放")
		}
		if parsed && payload.OK != nil && !*payload.OK {
			return "down", firstNonEmpty(note, "服务上报异常")
		}
		return "up", note
	case code == http.StatusServiceUnavailable:
		// 503 在多数服务里表示「上游不可用」，但在本平台也用于「主动暂停」，
		// 两者都算服务进程活着：状态记为 paused，由文案区分。
		return "paused", firstNonEmpty(note, "服务不可用（503）")
	default:
		return "down", firstNonEmpty(note, fmt.Sprintf("HTTP %d", code))
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// views 生成聚合视图（newest 在前，checks 由旧到新）
func (m *monitor) views() []monitorView {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]monitorView, 0, len(m.targets))
	for _, t := range m.targets {
		h := m.history[t.Key]
		v := monitorView{Key: t.Key, Name: t.Name, URL: t.URL, State: "unknown", Checks: h}
		if len(h) > 0 {
			last := h[len(h)-1]
			v.State = last.State
			v.Code = last.Code
			v.LatencyMS = last.LatencyMS
			v.Message = last.Message
			v.CheckedAt = last.At
			alive := 0
			for _, c := range h {
				if c.State == "up" || c.State == "paused" {
					alive++
				}
			}
			v.Uptime = alive * 100 / len(h)
		}
		out = append(out, v)
	}
	return out
}

// handleMonitor 监控数据。默认返回缓存结果；force=1 时同步再探测一次（最多约 8s）。
func (s *server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	if s.mon == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"configured": false,
			"interval":   0,
			"targets":    []monitorView{},
		})
		return
	}
	if r.URL.Query().Get("force") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), monitorTimeout+4*time.Second)
		defer cancel()
		s.mon.checkAll(ctx)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"configured": true,
		"interval":   int(s.mon.interval.Seconds()),
		"targets":    s.mon.views(),
		"history":    monitorHistorySize,
	})
}
