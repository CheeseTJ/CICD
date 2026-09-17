package main

import "strings"

// 平台标识：卡片主体的部署位置，当前出现这三类
const (
	PlatformPgyer = "pgyer" // 蒲公英：APK 内测分发
	PlatformCF    = "cf"    // Cloudflare：Pages / Workers
	PlatformOCI   = "oci"   // 本机 OCI 主机：self-hosted 部署
)

// detectWorkflowPlatform 从 workflow 原文判断仓库主体部署在哪里：
// self-hosted → 本机（OCI）；wrangler 或 Cloudflare API/Pages → Cloudflare。
// 判断不出返回空串（卡片不显示徽章）。
func detectWorkflowPlatform(texts ...string) string {
	cf := false
	for _, t := range texts {
		if strings.Contains(t, "self-hosted") {
			return PlatformOCI
		}
		if strings.Contains(t, "wrangler") || strings.Contains(t, "api.cloudflare.com") ||
			strings.Contains(t, "cloudflare/pages") {
			cf = true
		}
	}
	if cf {
		return PlatformCF
	}
	return ""
}

// syncedToPgyer 该仓库的产物是否已由本控制台分发到蒲公英
func syncedToPgyer(st *RepoState) bool {
	if st == nil {
		return false
	}
	return st.LastSyncedTag != "" || st.Identifier != "" || st.PgyerShortcutUrl != ""
}

// servicePlatform 服务卡片的主体平台：workflow 反映的部署位置优先，
// 其次是"产物分发在蒲公英"（如只出 APK 的仓库）
func servicePlatform(texts []string, st *RepoState) string {
	if p := detectWorkflowPlatform(texts...); p != "" {
		return p
	}
	if syncedToPgyer(st) {
		return PlatformPgyer
	}
	return ""
}
