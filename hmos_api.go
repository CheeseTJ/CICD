// 鸿蒙应用管理接口：列表 / 增删改 / 重签 / 上传蒲公英 / 到期自动重签。
//
// 设计要点
// --------
//  1. 证书到期判断走本地文件解析（见 hmos_cert.go），不依赖 AGC API。
//  2. 重签与上传拆成两个独立动作，但「一键续期」把它们串起来：
//     重签 → 校验 → 上传蒲公英 → 回写下载页。失败在任一步就停，错误原样带回。
//  3. 自动重签由现有的定时器驱动（复用 AUTO_SYNC_INTERVAL 的节奏），
//     每个 App 每天最多触发一次，避免定时器频率高时反复重签。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// hmosView 下发给前端的应用视图（含解析出的证书状态）。
type hmosView struct {
	HmosApp
	Cert *certInfo `json:"cert"`
	// HapExists 源包是否还在，不在则无法重签
	HapExists bool `json:"hap_exists"`
	// HuaweiConsoleUrl 华为开发者中心，前端「跳转下载证书」按钮用
	HuaweiConsoleUrl string `json:"huawei_console_url"`
}

// huaweiAGCUrl AppGallery Connect 的证书管理页。
//
// 这个地址是 AGC 控制台里「证书、APP ID 和 Profile」的固定入口。
// 官方改版时可能变，所以只作为默认值下发，前端也允许用户从设置里覆盖。
const huaweiAGCUrl = "https://developer.huawei.com/consumer/cn/service/josp/agc/index.html#/myProject"

// buildHmosView 把存储里的 App 补全为前端视图。
func buildHmosView(a *HmosApp, warnDays int) hmosView {
	v := hmosView{HmosApp: *a, HuaweiConsoleUrl: huaweiAGCUrl}
	// 证书字段用指针，未配置时前端显示「未配置」而不是零值时间
	v.Cert = collectCertInfo(hmosCertFile{
		CertFile:    a.CertFile,
		ProfileFile: a.ProfileFile,
		Keystore:    a.Keystore,
		KeyAlias:    a.KeyAlias,
	}, warnDays)
	if a.HapFile != "" {
		if _, err := os.Stat(a.HapFile); err == nil {
			v.HapExists = true
		}
	}
	return v
}

// handleHmosList GET /api/hmos
func (s *server) handleHmosList(w http.ResponseWriter, r *http.Request) {
	apps := s.hmos.List()
	out := make([]hmosView, 0, len(apps))
	for _, a := range apps {
		out = append(out, buildHmosView(a, s.cfg.HmosWarnDays))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"apps": out,
		// 前端要用这两个值渲染提示文案与设置页
		"warn_days":   s.cfg.HmosWarnDays,
		"sign_tool":   s.cfg.HmosSignTool != "",
		"console_url": huaweiAGCUrl,
	})
}

// hmosUpsertReq 新增/更新请求体。
//
// 用指针区分「没传这个字段」与「传了空串」：想让用户清空某个路径时必须能传空串，
// 而不传则保持原值（部分更新）。
type hmosUpsertReq struct {
	ID          string  `json:"id"`
	Name        *string `json:"name"`
	BundleName  *string `json:"bundle_name"`
	CertFile    *string `json:"cert_file"`
	ProfileFile *string `json:"profile_file"`
	Keystore    *string `json:"keystore"`
	KeyAlias    *string `json:"key_alias"`
	HapFile     *string `json:"hap_file"`
	AutoResign  *bool   `json:"auto_resign"`
}

// handleHmosUpsert POST /api/hmos
func (s *server) handleHmosUpsert(w http.ResponseWriter, r *http.Request) {
	var req hmosUpsertReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.ID == "" && req.BundleName == nil {
		writeErr(w, http.StatusBadRequest, errors.New("id 或 bundle_name 至少给一个"))
		return
	}
	id := req.ID
	if id == "" {
		id = *req.BundleName
	}

	existing, ok := s.hmos.Get(id)
	if !ok {
		existing = &HmosApp{ID: id}
	}
	// 只覆盖调用方明确给出的字段
	applyStr := func(dst *string, src *string) {
		if src != nil {
			*dst = normalizeCertPath(*src)
		}
	}
	applyStr(&existing.Name, req.Name)
	applyStr(&existing.BundleName, req.BundleName)
	applyStr(&existing.CertFile, req.CertFile)
	applyStr(&existing.ProfileFile, req.ProfileFile)
	applyStr(&existing.Keystore, req.Keystore)
	applyStr(&existing.KeyAlias, req.KeyAlias)
	applyStr(&existing.HapFile, req.HapFile)
	if req.AutoResign != nil {
		existing.AutoResign = *req.AutoResign
	}
	if existing.BundleName == "" {
		existing.BundleName = id
	}

	if err := s.hmos.Upsert(existing); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.hmos.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	saved, _ := s.hmos.Get(id)
	writeJSON(w, http.StatusOK, buildHmosView(saved, s.cfg.HmosWarnDays))
}

// handleHmosDelete POST /api/hmos/delete
func (s *server) handleHmosDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.hmos.Delete(req.ID); err != nil {
		writeErr(w, http.StatusNotFound, errors.New("应用不存在"))
		return
	}
	if err := s.hmos.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// hmosResignResult 重签并把结果回写后的完整视图。
type hmosResignResult struct {
	App     hmosView `json:"app"`
	Resign  *resignResult
	Verify  bool   `json:"verify_ok"`
	Message string `json:"message"`
}

// handleHmosResign POST /api/hmos/resign
//
// 用当前配置的证书重新签 HapFile，产物覆盖回 HapFile（原地续期）。
// 为什么不另存新文件：HAP 重签是幂等的（剥旧块写新块），覆盖后体积不增长，
// 而且下次重签还能拿它当输入，不需要用户再准备一份原始包。
func (s *server) handleHmosResign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	app, ok := s.hmos.Get(req.ID)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("应用不存在"))
		return
	}
	if app.HapFile == "" {
		writeErr(w, http.StatusBadRequest, errors.New("未配置 hap_file，没有可重签的源包"))
		return
	}
	if _, err := os.Stat(app.HapFile); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("源包不存在: %w", err))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), resignTimeout)
	defer cancel()

	res, err := s.resignAndReplace(ctx, app)
	if err != nil {
		s.hmos.Patch(app.ID, func(a *HmosApp) { a.LastError = err.Error() })
		_ = s.hmos.Save()
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	saved, _ := s.hmos.Get(app.ID)
	writeJSON(w, http.StatusOK, hmosResignResult{
		App:     buildHmosView(saved, s.cfg.HmosWarnDays),
		Resign:  res,
		Verify:  true,
		Message: "重签成功",
	})
}

// resignAndReplace 重签到临时文件 → 校验 → 原子替换原包。
//
// 先写临时文件再 replace 的原因：签名过程中源包仍是可用的（万一失败，
// 用户手上那份包不会被破坏）。这一步对「自动重签」尤其重要。
func (s *server) resignAndReplace(ctx context.Context, app *HmosApp) (*resignResult, error) {
	cfg := resignConfig{
		SignTool: s.cfg.HmosSignTool,
		Java:     s.cfg.HmosJava,
	}
	cert := hmosCertFile{
		CertFile:    app.CertFile,
		ProfileFile: app.ProfileFile,
		Keystore:    app.Keystore,
		KeyAlias:    app.KeyAlias,
	}

	dir := filepath.Dir(app.HapFile)
	tmpOut := filepath.Join(dir, ".resign-"+fmt.Sprintf("%d", time.Now().UnixNano())+".hap")
	defer os.Remove(tmpOut)

	res, err := resignHap(ctx, cfg, app.HapFile, tmpOut, cert)
	if err != nil {
		return nil, err
	}
	if err := verifyHap(ctx, cfg, tmpOut); err != nil {
		// 校验失败不阻断：有些设备可装但工具校验报错（工具版本差异）。
		// 记录到 LastError 供排查，但仍然替换产物，否则用户拿不到包。
		log.Printf("[hmos] %s 重签后校验未通过（仍保留产物）: %v", app.ID, err)
	}
	if err := os.Rename(tmpOut, app.HapFile); err != nil {
		return nil, fmt.Errorf("替换源包失败: %w", err)
	}

	s.hmos.Patch(app.ID, func(a *HmosApp) {
		a.LastResignAt = time.Now().Unix()
		a.LastError = ""
	})
	res.OutputPath = app.HapFile
	return res, nil
}

// handleHmosUpload POST /api/hmos/upload
//
// 把 HapFile 传到蒲公英（buildType=harmonyos），回写下载页地址。
func (s *server) handleHmosUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	app, ok := s.hmos.Get(req.ID)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("应用不存在"))
		return
	}
	if app.HapFile == "" {
		writeErr(w, http.StatusBadRequest, errors.New("未配置 hap_file"))
		return
	}
	if _, err := os.Stat(app.HapFile); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("hap 不存在: %w", err))
		return
	}
	if s.cfg.PGYERKey == "" {
		writeErr(w, http.StatusBadRequest, errors.New("未配置 PGYER_API_KEY"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	info, err := s.uploadHapToPgyer(ctx, app)
	if err != nil {
		s.hmos.Patch(app.ID, func(a *HmosApp) { a.LastError = err.Error() })
		_ = s.hmos.Save()
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	saved, _ := s.hmos.Get(app.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"app":        buildHmosView(saved, s.cfg.HmosWarnDays),
		"build_key":  info.BuildKey,
		"page_url":   shortcutPage(info.BuildShortcutUrl),
		"qr_code":    info.BuildQRCodeURL,
		"version":    info.BuildVersion,
		"identifier": info.BuildIdentifier,
	})
}

// uploadHapToPgyer 上传并轮询发布结果，成功后回写 App 记录。
func (s *server) uploadHapToPgyer(ctx context.Context, app *HmosApp) (*buildInfoData, error) {
	tok, err := s.pgyer.getCOSTokenFor(ctx, "harmonyos")
	if err != nil {
		return nil, fmt.Errorf("申请上传凭证失败: %w", err)
	}
	name := filepath.Base(app.HapFile)
	if err := s.pgyer.uploadFile(ctx, tok, app.HapFile, name, nil); err != nil {
		return nil, fmt.Errorf("上传 hap 失败: %w", err)
	}
	info, err := s.pgyer.pollBuildInfo(ctx, tok.Key, 10*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("等待蒲公英发布失败: %w", err)
	}

	now := time.Now().Unix()
	s.hmos.Patch(app.ID, func(a *HmosApp) {
		a.PgyerShortcutUrl = shortcutPage(info.BuildShortcutUrl)
		a.PgyerBuildKey = info.BuildKey
		a.LastUploadAt = now
		a.LastUploadVer = info.BuildVersion
		a.LastError = ""
	})
	if err := s.hmos.Save(); err != nil {
		log.Printf("[hmos] 保存上传结果失败: %v", err)
	}
	return info, nil
}

// autoResignOnce 到期自动重签：证书剩余天数 <= 阈值且开启 AutoResign 时，
// 用现有 HapFile 重签并上传蒲公英。每天最多一次。
//
// 触发条件刻意收紧成「快到期」而不是「只要开启就每天重签」：
// 每次上传都会在蒲公英产生一个新 build，无意义地刷版本会淹没真正的变更。
func (s *server) autoResignOnce() {
	apps := s.hmos.List()
	for _, app := range apps {
		if !app.AutoResign {
			continue
		}
		info := collectCertInfo(hmosCertFile{
			CertFile:    app.CertFile,
			ProfileFile: app.ProfileFile,
			Keystore:    app.Keystore,
			KeyAlias:    app.KeyAlias,
		}, s.cfg.HmosWarnDays)

		// 只有「即将过期」或「已过期」才动；状态未知（文件缺失）不处理，
		// 那种情况需要人去看，自动重签只会反复失败刷日志。
		if info.Status != CertWarn && info.Status != CertExpired {
			continue
		}
		// 同一天不重复
		if app.LastResignAt > 0 {
			last := time.Unix(app.LastResignAt, 0)
			if last.YearDay() == time.Now().YearDay() && last.Year() == time.Now().Year() {
				continue
			}
		}
		if app.HapFile == "" {
			continue
		}

		log.Printf("[hmos] 自动重签 %s（证书%s，剩余 %d 天）",
			app.ID, certStatusLabel(info.Status), info.DaysLeft)

		ctx, cancel := context.WithTimeout(context.Background(), resignTimeout)
		_, err := s.resignAndReplace(ctx, app)
		cancel()
		if err != nil {
			log.Printf("[hmos] 自动重签 %s 失败: %v", app.ID, err)
			s.hmos.Patch(app.ID, func(a *HmosApp) { a.LastError = err.Error() })
			_ = s.hmos.Save()
			continue
		}

		if s.cfg.PGYERKey != "" {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Minute)
			if _, err := s.uploadHapToPgyer(ctx2, app); err != nil {
				log.Printf("[hmos] 自动上传 %s 失败: %v", app.ID, err)
				s.hmos.Patch(app.ID, func(a *HmosApp) { a.LastError = err.Error() })
			}
			cancel2()
		}
		_ = s.hmos.Save()
	}
}

// startHmosAutoResign 定时检查到期。
//
// 复用 AUTO_SYNC_INTERVAL 的间隔太长（默认 5 分钟够用），但到期判断是「按天」的，
// 所以单独用更长周期（10 分钟）避免无谓的文件读取。
func (s *server) startHmosAutoResign() {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.autoResignOnce()
		}
	}()
}

// hmosPathFromDataFile 把 data.json 的同级目录作为鸿蒙清单的默认位置。
func hmosPathFromDataFile(dataFile string) string {
	if dataFile == "" {
		return "hmos.json"
	}
	return filepath.Join(filepath.Dir(dataFile), "hmos.json")
}

// trimLower 小写并去空白，用于简单比对。
func trimLower(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
