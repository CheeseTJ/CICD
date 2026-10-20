// 服务健康监控：探测在浏览器端完成，服务端只提供「要监控谁」的清单。
//
// 为什么最终选了浏览器直连（而不是服务端轮询）：
// 1) 没人看的时候天然零流量——不打开监控页就一个请求都不会发，不用额外做
//    「空闲停机」这类复杂逻辑；
// 2) 看到的就是「你自己当前的网络能否访问该服务」，比服务器视角更贴近真实体验；
// 3) 服务端不必为每个目标建立长连接/定时器，也省掉一次公网往返的中转。
//
// 代价与应对：
// 1) 跨域（CORS）：实测本平台多数服务返回 Access-Control-Allow-Origin，但
//    imgbed / wordrush / music-worker / 本控制台自身不返回。浏览器会拦下读取，
//    此时无法区分「服务坏了」与「不允许跨域」。前端把这些服务标成「跨域受限」，
//    并提示可改成同源路径或让对方放行来源——本清单里的 cross_origin 字段给出
//    服务端预先判定的结果，避免前端把 CORS 失败误报成服务宕机。
// 2) 历史与可用率：浏览器端没有历史，故不提供「最近 N 次可用率」指标，
//    只展示当前状态与延迟（不再编造无数据来源的百分比）。
// 3) 服务端无法在没人看时收集数据，因此不提供「服务端视角」的历史曲线。

package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// monitorProbeTimeout 跨域判定用的单次探测超时
const monitorProbeTimeout = 8 * time.Second

// monitorTarget 一个被监控的服务；全部字段都是非敏感信息，登录后可读。
type monitorTarget struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	URL  string `json:"url"`
	// CrossOrigin 服务端预先探测过：目标是否回显了允许浏览器跨域读取的头。
	// false 表示浏览器直接 fetch 会被 CORS 拦下（前端展示为「跨域受限」）。
	CrossOrigin bool `json:"cross_origin"`
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
		// 相对路径（如 /api/health）按本页同源请求，无需校验 scheme/host
		if !isSameOrigin(rawURL) {
			u, err := url.Parse(rawURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				log.Printf("[monitor] 忽略无效目标 %q", part)
				continue
			}
			if name == "" {
				name = u.Host
			}
		} else if name == "" {
			name = strings.Trim(rawURL, "/")
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

// isLoopback 判断地址是否指向本机：本机目标浏览器访问不到，只能由服务端代探。
func isLoopback(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// loopbackFn 便于测试：默认用 isLoopback，测试里可替换
// （httptest 服务跑在 127.0.0.1，若不做替换会被「回环不可直连」规则短路）
var loopbackFn = isLoopback

// isSameOrigin 判断目标是否为相对路径（如 /api/health）。
// 相对路径由浏览器按本页同源请求，不存在 CORS 问题，也不需要服务端预判。
func isSameOrigin(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	return strings.HasPrefix(rawURL, "/")
}

// probeCORS 由服务端代浏览器发一次带 Origin 的 GET，看对方是否回显了
// Access-Control-Allow-Origin。带 Origin 时对方若不做处理，浏览器就会拦读取。
func (s *server) probeCORS(ctx context.Context, target string) bool {
	if isSameOrigin(target) {
		return true // 同源相对路径：浏览器可直接读取
	}
	if _, err := url.Parse(target); err != nil {
		return false
	}
	if loopbackFn(target) {
		return false // 浏览器够不到本机地址，必然只能算跨域受限
	}
	ctx, cancel := context.WithTimeout(ctx, monitorProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	// Origin 用本控制台自身的对外地址：与真实浏览器从该页面发起时一致
	req.Header.Set("Origin", s.selfOrigin())
	req.Header.Set("User-Agent", "cicd-console-cors-probe/1")
	req.Header.Set("Accept", "application/json, text/plain, */*")

	resp, err := s.monHTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	acao := resp.Header.Get("Access-Control-Allow-Origin")
	if acao == "" {
		return false
	}
	return acao == "*" || strings.EqualFold(strings.TrimRight(acao, "/"), s.selfOrigin())
}

// selfOrigin 本控制台对外访问来源（浏览器发起探测时的 Origin）。
// 由 PUBLIC_ORIGIN 覆盖；未配置时按请求 Host 推断，最终兜底为 *。
func (s *server) selfOrigin() string {
	if v := strings.TrimSpace(s.cfg.PublicOrigin); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "*"
}

// refreshCrossOrigin 启动时与服务定时刷新时，标记每个目标浏览器能否直连。
func (s *server) refreshCrossOrigin() {
	if len(s.cfg.HealthTargets) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), monitorProbeTimeout+5*time.Second)
	defer cancel()
	for i := range s.cfg.HealthTargets {
		t := s.cfg.HealthTargets[i]
		// 同源相对路径天然可读，无需探测
		if isSameOrigin(t.URL) {
			t.CrossOrigin = true
			s.cfg.HealthTargets[i] = t
			continue
		}
		if loopbackFn(t.URL) {
			continue
		}
		if s.probeCORS(ctx, t.URL) {
			t.CrossOrigin = true
			s.cfg.HealthTargets[i] = t
		}
	}
}

// startCORSRefresh 定时刷新跨域判定（服务上线/改配置后无需重启控制台）
func (s *server) startCORSRefresh() {
	if len(s.cfg.HealthTargets) == 0 {
		return
	}
	go func() {
		s.refreshCrossOrigin()
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			s.refreshCrossOrigin()
		}
	}()
}

// handleMonitor 返回监控目标清单。探测本身由浏览器完成（见文件头说明），
// 服务端只负责「监控谁」与「浏览器能否直连」这两件必须站外判定的事。
func (s *server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	targets := s.cfg.HealthTargets
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"configured":       false,
			"client_side":      true,
			"public_origin":    s.selfOrigin(),
			"refresh_interval": 15,
			"targets":          []monitorTarget{},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"configured":       true,
		"client_side":      true, // 前端据此走浏览器直连，不走服务端轮询
		"public_origin":    s.selfOrigin(),
		"refresh_interval": 15, // 前端停留本页时的探测间隔（秒）
		"targets":          targets,
	})
}
