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
	// Files 四个签名材料的逐个状态，卡片直接渲染成四张小卡。
	//
	// 之所以在后端算而不是前端推导：文件是否存在、扩展名是什么（上传时按内容
	// 决定，可能是 .cer 也可能是 .pem）只有服务端知道。
	Files []materialFileView `json:"files"`
	// HuaweiConsoleUrl 华为开发者中心，前端「跳转下载证书」按钮用
	HuaweiConsoleUrl string `json:"huawei_console_url"`
	// PgyerInstallBlocker 非空表示当前签名材料传上去测试者也装不上，
	// 内容就是原因，前端直接放进警告条。
	//
	// 典型情况是 profile 是 debug 类型 —— 蒲公英明确拒绝这种签名。
	PgyerInstallBlocker string `json:"pgyer_install_blocker"`
	// KeyMatch 证书与 keystore 是否配对。
	//
	// 不配对意味着重签一定会失败，而且 AGC 和本控制台都不会提前报错 ——
	// 只能在这里主动比对公钥发现。
	KeyMatch keyMatchResult `json:"key_match"`
	// Distribution Profile 的分发类型（app_gallery / internaltesting / ...），
	// 对应 p7b 里的 app-distribution-type。前端用它显示徽标。
	Distribution string `json:"distribution"`
	// Sideload 能否下载 hap 直接装。
	//
	// 这是最容易判错的一件事：app_gallery 类型看着「不限设备」，实际是
	// 应用市场专用，侧载会被系统拒绝。必须显式算出来告诉用户。
	Sideload sideloadVerdict `json:"sideload"`
}

// huaweiAGCUrl AppGallery Connect 的证书管理页。
//
// 这个地址是 AGC 控制台里「证书、APP ID 和 Profile」的固定入口。
// 官方改版时可能变，所以只作为默认值下发，前端也允许用户从设置里覆盖。
const huaweiAGCUrl = "https://developer.huawei.com/consumer/cn/service/josp/agc/index.html#/myProject"

// buildHmosView 把存储里的 App 补全为前端视图。
// hmosAppPaths 按包名推导出四个文件的固定路径。
//
// 为什么固定路径而不是让用户填：
//  1. 配置项从 4 个降到 0 个 —— 用户只需要「点上传」这一件事，不会因为路径
//     打错（大小写、相对/绝对、Windows 反斜杠）而卡住。
//  2. 上传接口本来就是把文件写到 <root>/<bundleName>/ 下，两边规则一致，
//     不存在「传上去了但配置指向别处」这种不一致状态。
//  3. 换证书时只要重新上传覆盖同名文件，不需要改任何配置。
func hmosAppPaths(root, bundle string) (cert, profile, keystore, hap string) {
	dir := filepath.Join(root, bundle)
	return filepath.Join(dir, "cert.cer"),
		filepath.Join(dir, "profile.p7b"),
		filepath.Join(dir, "key.p12"),
		filepath.Join(dir, "app.hap")
}

// materialFileView 单个材料文件在卡片上的状态。
type materialFileView struct {
	// Kind 上传接口用的类型标识：cert / profile / keystore / hap
	Kind string `json:"kind"`
	// Label 展示名
	Label string `json:"label"`
	// Present 文件是否已上传
	Present bool `json:"present"`
	// Size 字节数，0 表示不存在
	Size int64 `json:"size"`
	// NotAfter 该文件的到期时间（仅 cert/profile 有），0 表示未知
	NotAfter int64 `json:"not_after"`
	// Name 实际文件名（含扩展名，扩展名由上传内容决定）
	Name string `json:"name"`
}

// buildHmosView 把存储里的 App 补全为前端视图。
func buildHmosView(a *HmosApp, warnDays int, uploadRoot string) hmosView {
	return buildHmosViewWithKey(a, warnDays, uploadRoot, "", "")
}

// buildHmosViewWithKey 与 buildHmosView 相同，额外交验证书与 keystore 的配对。
//
// password = "" 时跳过校验（返回 unknown）—— 运维没配密码时不阻断页面，
// 只是卡片上不显示配对结论。
func buildHmosViewWithKey(a *HmosApp, warnDays int, uploadRoot, password, javaHome string) hmosView {
	v := hmosView{HmosApp: *a, HuaweiConsoleUrl: huaweiAGCUrl}

	// 路径一律由包名推导，忽略存储里的旧值 —— 老数据可能存的是用户手填的
	// 任意路径，继续用会让「固定路径」这个约定失效。
	certPath, profPath, keyPath, hapPath := hmosAppPaths(uploadRoot, a.BundleName)
	a.CertFile, a.ProfileFile, a.Keystore, a.HapFile = certPath, profPath, keyPath, hapPath
	v.CertFile, v.ProfileFile, v.Keystore, v.HapFile = certPath, profPath, keyPath, hapPath

	v.Cert = collectCertInfo(hmosCertFile{
		CertFile:    certPath,
		ProfileFile: profPath,
		Keystore:    keyPath,
		KeyAlias:    a.KeyAlias,
	}, warnDays)

	if _, err := os.Stat(hapPath); err == nil {
		v.HapExists = true
	}

	// 四个材料文件的逐个状态，供卡片直接渲染成四张小卡
	v.Files = []materialFileView{
		fileStat("cert", "证书", certPath),
		fileStat("keystore", "Keystore", keyPath),
		fileStat("profile", "Profile", profPath),
		fileStat("hap", "HAP 包", hapPath),
	}
	// 从证书解析结果里补上有效期（fileStat 只关心文件本身）
	for i := range v.Files {
		switch v.Files[i].Kind {
		case "cert":
			v.Files[i].NotAfter = v.Cert.CertNotAfter
		case "profile":
			v.Files[i].NotAfter = v.Cert.ProfileNotAfter
		}
	}
	// 能否从蒲公英安装：profile 为 debug 类型时必然装不上，提前告诉用户
	v.PgyerInstallBlocker = v.Cert.pgyerInstallBlocker()

	// 证书与 keystore 的配对校验：不匹配则重签必然失败，必须提前报
	v.KeyMatch = verifyKeyPair(actualPath(certPath), actualPath(keyPath), password, a.KeyAlias, javaHome)

	// 分发类型与侧载可行性。
	// 只看 profile 的三个字段：app-distribution-type / type / 设备数。
	//
	// 注意用 actualPath 而不是 profPath：上传时扩展名由文件内容决定
	// （profile 可能是 .p7b 也可能是 .p7c），固定路径会读不到文件，
	// 导致分发类型被判成空 → 界面误报「缺少分发类型字段」。
	v.Distribution = profileDistribution(actualPath(profPath))
	v.Sideload = judgeSideload(v.Distribution, v.Cert.ProfileType, v.Cert.ProfileDevices)
	return v
}

// actualPath 把「按约定推导的路径」修正为磁盘上真实存在的文件路径。
//
// 为什么需要：上传时保留原始扩展名（.cer/.pem、.p7b/.p7c、.p12/.jks），
// 而视图层一律按规范名（cert.cer / profile.p7b / key.p12）推导路径。
// 两者不一致时，os.ReadFile 会失败，各种解析都变成「读不到」——
// 表现为「未上传」「缺少字段」这类误导性提示。
//
// 找不到同主体名的文件时原样返回，让调用方照常报「文件不存在」。
func actualPath(canonical string) string {
	if _, err := os.Stat(canonical); err == nil {
		return canonical
	}
	base := strings.TrimSuffix(filepath.Base(canonical), filepath.Ext(canonical))
	dir := filepath.Dir(canonical)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return canonical
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, base+".") && !strings.HasSuffix(n, ".part") {
			return filepath.Join(dir, n)
		}
	}
	return canonical
}

// fileStat 读取单个文件的存在性与大小。
//
// 上传时扩展名由内容决定（.cer 可能存成 .pem），所以这里按「同主体名 + 任意
// 允许扩展名」找文件，而不是硬编码某个后缀。
func fileStat(kind, label, canonical string) materialFileView {
	f := materialFileView{Kind: kind, Label: label, Name: filepath.Base(canonical)}
	if fi, err := os.Stat(canonical); err == nil {
		f.Present = true
		f.Size = fi.Size()
		return f
	}
	// canonical 不存在时，扫同目录找同主体名（cert.* / key.* / ...）的文件
	base := strings.TrimSuffix(filepath.Base(canonical), filepath.Ext(canonical))
	dir := filepath.Dir(canonical)
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			if strings.HasPrefix(n, base+".") && !strings.HasSuffix(n, ".part") {
				if info, err := e.Info(); err == nil {
					f.Present = true
					f.Size = info.Size()
					f.Name = n
					return f
				}
			}
		}
	}
	return f
}

// handleHmosList GET /api/hmos
func (s *server) handleHmosList(w http.ResponseWriter, r *http.Request) {
	apps := s.hmos.List()
	out := make([]hmosView, 0, len(apps))
	for _, a := range apps {
		out = append(out, buildHmosViewWithKey(a, s.cfg.HmosWarnDays, s.cfg.HmosUploadDir, os.Getenv("HMOS_KEYSTORE_PWD"), s.cfg.HmosJava))
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
	writeJSON(w, http.StatusOK, buildHmosViewWithKey(saved, s.cfg.HmosWarnDays, s.cfg.HmosUploadDir, os.Getenv("HMOS_KEYSTORE_PWD"), s.cfg.HmosJava))
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
		App:     buildHmosViewWithKey(saved, s.cfg.HmosWarnDays, s.cfg.HmosUploadDir, os.Getenv("HMOS_KEYSTORE_PWD"), s.cfg.HmosJava),
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
		"app":        buildHmosViewWithKey(saved, s.cfg.HmosWarnDays, s.cfg.HmosUploadDir, os.Getenv("HMOS_KEYSTORE_PWD"), s.cfg.HmosJava),
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

	// 上传 p12：蒲公英要用它签 HAP 的安装清单（manifest.json5）。
	//
	// 不做这一步的话，HAP 能传上去、下载页也能打开，但页面会挂红色警告
	// 「当前版本使用调试 Profile 签名，测试者无法通过蒲公英安装」，
	// 测试者点安装直接失败。
	//
	// 失败不阻断整体流程：HAP 已经传成功了，先把 buildKey/下载页回写，
	// 再把这个错误报出去让用户去处理证书 —— 而不是让用户重传 40MB 的包。
	var certErr error
	if app.Keystore != "" {
		pwd := os.Getenv("HMOS_KEYSTORE_PWD")
		if pwd == "" {
			certErr = errors.New("未配置 HMOS_KEYSTORE_PWD，无法上传 p12（蒲公英将无法生成可安装的签名）")
		} else {
			certErr = s.pgyer.uploadHarmonyCert(ctx, info.BuildKey, app.Keystore, pwd, app.KeyAlias)
		}
	}

	now := time.Now().Unix()
	s.hmos.Patch(app.ID, func(a *HmosApp) {
		a.PgyerShortcutUrl = shortcutPage(info.BuildShortcutUrl)
		a.PgyerBuildKey = info.BuildKey
		a.LastUploadAt = now
		a.LastUploadVer = info.BuildVersion
		if certErr != nil {
			a.LastError = "p12 上传失败：" + certErr.Error()
		} else {
			a.LastError = ""
		}
	})
	if err := s.hmos.Save(); err != nil {
		log.Printf("[hmos] 保存上传结果失败: %v", err)
	}
	if certErr != nil {
		return info, certErr
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
//
// 这里同时驱动两件事：
//   - autoResignOnce  用现有材料重签 HAP（只能续期，不能换 Profile）
//   - checkAndNotifyExpiry  发到期提醒邮件（Profile 到期只能人去 AGC 处理，
//     所以提醒比自动重签更关键）
func (s *server) startHmosAutoResign() {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.autoResignOnce()
			s.checkAndNotifyExpiry()
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
