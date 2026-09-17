package main

import (
	"log"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// 平台标识：当前出现的三类，检测到才贴对应徽章
const (
	PlatformPgyer = "pgyer" // 蒲公英：APK 分发
	PlatformCF    = "cf"    // Cloudflare：Pages 构建 / Workers / Tunnel 入口
	PlatformOCI   = "oci"   // 本机 OCI 主机：self-hosted runner 部署的容器
)

// platformOrder 徽章的固定展示顺序
var platformOrder = []string{PlatformPgyer, PlatformCF, PlatformOCI}

// detectPlatforms 从 workflow 文本推断涉及的平台：
//   - pgyer：出现蒲公英接口地址（仅凭 "pgyer" 字样会误判——有些仓库只是把
//     PGYER_API_KEY 传给构建，真正的上传由本控制台完成）
//   - cf：用 wrangler 或 Cloudflare API/Pages 部署
//   - oci：runs-on: self-hosted，即部署在本机；这类服务公网入口统一经
//     cloudflared 隧道，因此同时视为涉及 Cloudflare
func detectPlatforms(texts ...string) []string {
	seen := map[string]bool{}
	for _, t := range texts {
		if strings.Contains(t, "pgyer.com") || strings.Contains(t, "蒲公英") {
			seen[PlatformPgyer] = true
		}
		if strings.Contains(t, "wrangler") || strings.Contains(t, "api.cloudflare.com") ||
			strings.Contains(t, "cloudflare/pages") {
			seen[PlatformCF] = true
		}
		if strings.Contains(t, "self-hosted") {
			seen[PlatformOCI] = true
			seen[PlatformCF] = true
		}
	}
	return orderPlatforms(seen)
}

// orderPlatforms 按固定顺序输出平台集合（顺序稳定，便于前端一致展示）
func orderPlatforms(seen map[string]bool) []string {
	out := make([]string, 0, len(seen))
	for _, p := range platformOrder {
		if seen[p] {
			out = append(out, p)
		}
	}
	return out
}

// mergePlatforms 合并多组平台并去重排序
func mergePlatforms(lists ...[]string) []string {
	seen := map[string]bool{}
	for _, l := range lists {
		for _, p := range l {
			seen[p] = true
		}
	}
	return orderPlatforms(seen)
}

// tunnelConfigPath 本机 cloudflared 配置路径（容器内以只读方式挂载）
func tunnelConfigPath() string {
	if p := os.Getenv("CLOUDFLARED_CONFIG"); p != "" {
		return p
	}
	return "/etc/cloudflared/config.yml"
}

var (
	tunnelMu      sync.Mutex
	tunnelCached  map[string]bool
	tunnelExpires time.Time
)

// tunnelHosts 本机 cloudflared 隧道对外暴露的域名集合。
// 用于判断某个站点依赖的服务是否跑在本机（OCI）；配置缺失时返回空集合，不影响其它检测。
func tunnelHosts() map[string]bool {
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	if tunnelCached != nil && time.Now().Before(tunnelExpires) {
		return tunnelCached
	}

	set := map[string]bool{}
	if data, err := os.ReadFile(tunnelConfigPath()); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			idx := strings.Index(line, "hostname:")
			if idx < 0 {
				continue
			}
			if h := strings.TrimSpace(line[idx+len("hostname:"):]); h != "" {
				set[h] = true
			}
		}
	} else {
		log.Printf("[platforms] 读取 cloudflared 配置失败（站点依赖本机的判断将跳过）: %v", err)
	}

	tunnelCached = set
	tunnelExpires = time.Now().Add(time.Minute)
	return set
}

// tunnelHostList 隧道域名列表（排序，用于展示与诊断）
func tunnelHostList() []string {
	set := tunnelHosts()
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// hostFromURL 取 URL 的 host（不带端口）；解析失败返回空串
func hostFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// exposedByTunnel 判断这些地址里是否有指向本机隧道的（即依赖本机 OCI 服务）
func exposedByTunnel(urls []string) bool {
	hosts := tunnelHosts()
	for _, u := range urls {
		if h := hostFromURL(u); h != "" && hosts[h] {
			return true
		}
	}
	return false
}
