package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HAP 直接下载。
//
// 为什么需要这个
// -------------
// 蒲公英那条路要求 Profile 是「指定设备发布」（180 天），而普通「发布」类型
// （app_gallery，3 年）会被它拒之门外。但华为自己的规则允许用发布型 Profile
// 签的 HAP 直接安装 —— 只要设备是「已注册测试设备」或走纯血鸿蒙的本地安装。
//
// 所以给 CICD 加一个下载入口：拿到 hap + p7b 直接装，绕开蒲公英的限制，
// 同时保留 3 年有效期。
//
// 一个必须说清的限制
// -----------------
// 下载 ≠ 能装。安装时会校验 Profile：
//   - type=release + device-ids 非空 → 只有列表里的 UDID 能装
//   - type=release + device-ids 为空 → 不限制设备（app_gallery 就是这种）
//   - type=debug                     → 只有登记的 UDID 能装
//
// 所以下载页必须把 bundleName、profile 类型、允许设备数一起告诉用户，
// 否则他装失败后完全不知道是「设备未注册」还是「包坏了」。

// handleHmosDownload 下发 HAP 文件。
//
// 路径：GET /hmos/download?id=<应用ID>
//
// 故意不走 /api/ 前缀：浏览器下载不希望被前端 fetch 拦截，直接让 <a href>
// 命中即可。同时保留鉴权 —— 包里有证书信息，不该对外裸奔。
func (s *server) handleHmosDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("只支持 GET"))
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少 id 参数"))
		return
	}

	app, ok := s.hmos.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("应用不存在: %s", id))
		return
	}

	// 路径一律由包名推导，与 buildHmosView 保持一致 ——
	// 老数据里可能存着用户手填的任意路径，不能信。
	_, _, _, hapPath := hmosAppPaths(s.cfg.HmosUploadDir, app.BundleName)

	f, err := os.Open(hapPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("HAP 文件不存在，请先上传"))
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// 文件名带包名和日期：手机下载目录里同名的多个版本会互相覆盖，
	// 带日期能一眼区分，也便于事后排查装的是哪一版。
	name := fmt.Sprintf("%s-%s.hap", app.BundleName, time.Now().Format("20060102-1504"))

	w.Header().Set("Content-Type", "application/octet-stream")
	// 用 RFC 5987 的 filename* 支持中文/特殊字符；
	// 同时保留 filename 给老浏览器兜底。
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		`attachment; filename="%s"; filename*=UTF-8''%s`, name, name))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", st.Size()))
	// 下载的包随时可能被重签替换，不让浏览器缓存
	w.Header().Set("Cache-Control", "no-store")

	http.ServeContent(w, r, filepath.Base(hapPath), st.ModTime(), f)
}

// handleHmosManifest 返回安装说明所需的元信息（JSON）。
//
// 前端在下载按钮旁展示「当前包能被哪些设备安装」。把这些信息放在服务端算，
// 是因为只有它知道 profile 的真实内容和 HAP 是否已签名。
func (s *server) handleHmosManifest(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少 id 参数"))
		return
	}
	app, ok := s.hmos.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("应用不存在: %s", id))
		return
	}

	v := buildHmosViewWithKey(app, s.cfg.HmosWarnDays, s.cfg.HmosUploadDir,
		os.Getenv("HMOS_KEYSTORE_PWD"), s.cfg.HmosJava)

	// 安装可行性判定：与蒲公英无关，只看华为侧规则。
	type installInfo struct {
		BundleName string `json:"bundle_name"`
		HapExists  bool   `json:"hap_exists"`
		HapSize    int64  `json:"hap_size"`
		// Signed HAP 是否已签名。未签名的包任何设备都装不上。
		Signed bool `json:"signed"`
		// ProfileType debug / release
		ProfileType string `json:"profile_type"`
		// Distribution app-distribution-type（app_gallery / internaltesting / ...）
		Distribution string `json:"distribution"`
		// DeviceCount profile 里登记的 UDID 数量
		DeviceCount int `json:"device_count"`
		// CanInstall 能否下载后直接侧载安装
		CanInstall bool `json:"can_install"`
		// InstallHint 一句话说明「能不能装、为什么」
		InstallHint string `json:"install_hint"`
		// ActionHint 装不上时的下一步建议
		ActionHint string `json:"action_hint,omitempty"`
	}

	info := installInfo{
		BundleName:   app.BundleName,
		HapExists:    v.HapExists,
		ProfileType:  v.Cert.ProfileType,
		Distribution: v.Distribution,
		DeviceCount:  v.Cert.ProfileDevices,
		CanInstall:   v.Sideload.Allowed,
		InstallHint:  v.Sideload.Reason,
		ActionHint:   v.Sideload.Hint,
	}
	if fi, err := os.Stat(v.HapFile); err == nil {
		info.HapSize = fi.Size()
	}
	info.Signed = v.HapExists && v.Cert.ProfileNotAfter > 0

	// 没传包/没传 profile 时，覆盖成更直白的提示
	if !v.HapExists {
		info.CanInstall = false
		info.InstallHint = "尚未上传 HAP"
		info.ActionHint = ""
	}

	writeJSON(w, http.StatusOK, info)
}
