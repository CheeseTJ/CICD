package main

// 鸿蒙（HarmonyOS / OpenHarmony）应用证书管理。
//
// 为什么需要这个模块
// ------------------
// 鸿蒙应用的签名由三部分组成，缺一不可：
//
//	keystore(.p12)  私钥，本地生成，可长期复用
//	cert(.cer)      证书链，由 AGC 签发，有有效期
//	profile(.p7b)   provisioning profile，由 AGC 签发，**也有有效期**
//
// 三者里 cert 和 profile 都会过期。过期后已装的应用仍能跑，但**无法再安装新包**
// （系统校验签名时直接拒绝）。华为的惯例是 cert 一年、profile 一年，实际以
// AGC 上显示的为准。
//
// 这里不调用 AGC API：那需要 client_id/client_secret 走 OAuth，权限申请繁琐，
// 而且证书文件本来就已经在本机（签名时要用）。直接解析文件既准确又不依赖网络
// —— 到期时间就在 `.cer` 的 X.509 notAfter 和 `.p7b` 的 validity.not-after 里。
//
// 重签名
// ------
// HAP 的签名是追加在 zip 尾部的独立块，重签时把旧块剥掉写新块，包内容完全不动。
// 所以只要换掉 cert+profile 就能续期，**不需要重新编译**（前提是 p12 私钥不变，
// 否则 cert 与私钥不匹配）。

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 证书状态等级：前端按这个决定徽标颜色与是否弹窗提示。
const (
	CertOK      = "ok"      // 剩余天数 > warnDays
	CertWarn    = "warn"    // 剩余天数 <= warnDays 但仍有效
	CertExpired = "expired" // 已过期
	CertUnknown = "unknown" // 拿不到有效期（文件缺失或解析失败）
)

// hmosCertFile 一套鸿蒙签名材料。
//
// 全部是文件路径而不是内容：证书文件本来就在磁盘上（构建/签名时要用），
// 控制台只负责读元信息、在到期时触发重签，不需要把私钥搬进内存或数据库。
type hmosCertFile struct {
	// CertFile 证书链 PEM（叶子 + 中间 + 根）。AGC 下载的通常是 .cer，内容就是
	// PEM 或 DER。
	CertFile string `json:"cert_file"`
	// ProfileFile 已签名的 provisioning profile（.p7b，PKCS#7）。
	// 它的 validity.not-after 与证书的 notAfter 往往不同，两个都要看。
	ProfileFile string `json:"profile_file"`
	// Keystore p12/jks 私钥库。重签时用，不会外泄给前端。
	Keystore string `json:"keystore"`
	// KeyAlias 私钥别名。
	KeyAlias string `json:"key_alias"`
}

// certInfo 解析出的证书元信息，下发给前端。
type certInfo struct {
	// 文件名（不带路径），便于用户确认自己传的是哪份
	CertName    string `json:"cert_name"`
	ProfileName string `json:"profile_name"`

	// 两个有效期分开报：谁先到期谁决定能不能装新包
	CertNotAfter    int64 `json:"cert_not_after"`    // unix 秒，0 表示未知
	ProfileNotAfter int64 `json:"profile_not_after"` // unix 秒，0 表示未知

	// EffectiveNotAfter = min(certNotAfter, profileNotAfter)，即真正失效时间
	EffectiveNotAfter int64 `json:"effective_not_after"`
	DaysLeft          int   `json:"days_left"`

	Status string `json:"status"` // ok / warn / expired / unknown

	// Subject / Issuer 便于人工核对是不是同一套材料
	Subject string `json:"subject"`
	Issuer  string `json:"issuer"`

	// BundleName 从 profile 里读出的 bundle-name，用于和 App 配置比对
	BundleName string `json:"bundle_name"`

	// ProfileType profile 的签发类型：debug / release。
	//
	// 这个字段决定了包能不能发给测试者：蒲公英只接受 release 类型
	// （AppGallery Connect 里叫「指定设备发布」或企业 Profile）。
	// debug 类型的 profile 即使签名完全正确，测试者也装不上 ——
	// 蒲公英页面会直接标红拒绝。所以后台必须把它显式报给前端。
	ProfileType string `json:"profile_type"`

	// ProfileDevices profile 里登记的设备数量。debug 类型靠 UDID 白名单
	// 限定安装范围，0 台意味着除了签名者自己谁也装不了。
	ProfileDevices int `json:"profile_devices"`

	// CertType 证书类型：debug（调试）/ release（发布）/ 空（未知）。
	//
	// 两者有效期差很多：调试证书 1 年，发布证书 3 年。换发布证书后
	// 用户需要能确认自己确实换对了，而不是又传了一张调试证书。
	// 数据来源是叶子证书 Subject —— AGC 会在里面写入 Development 或 Release。
	CertType string `json:"cert_type"`

	// Errors 非致命解析问题（例如 profile 里读不到 bundle-name）
	Errors []string `json:"errors,omitempty"`
}

// parseCERNotAfter 读取证书链里**最后一张**（叶子证书）的 notAfter。
//
// 为什么取最后一张：AGC 导出的链是「叶子 → 中间 → 根」，叶子在最前；
// 但也有工具按「根 → 中间 → 叶子」导出。两种顺序下更稳妥的做法是：
// 取所有证书里 notAfter 最早的那个 —— 先过期的决定整体可用期。
// 这里直接返回最早值，顺序无关。
func parseCERNotAfter(path string) (time.Time, *x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, nil, err
	}
	certs, err := parseCerts(data)
	if err != nil {
		return time.Time{}, nil, err
	}
	// 用最早的 notAfter 作为整体有效期；同时把 Subject 里信息最全的那张
	// （叶子通常在末尾或含 CN）返回给调用方做展示。
	var earliest time.Time
	var leaf *x509.Certificate
	for i, c := range certs {
		if earliest.IsZero() || c.NotAfter.Before(earliest) {
			earliest = c.NotAfter
			// 记录最早到期的那张，作为「决定有效期」的证书
			leaf = certs[i]
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil, errors.New("证书文件里没有可解析的证书")
	}
	return earliest, leaf, nil
}

// parseCerts 同时兼容 PEM 与 DER 两种编码。
func parseCerts(data []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	// PEM：可能有多段
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	if len(out) > 0 {
		return out, nil
	}
	// DER：整个文件是一张证书
	if c, err := x509.ParseCertificate(data); err == nil {
		return []*x509.Certificate{c}, nil
	}
	return nil, errors.New("既不是 PEM 也不是 DER 证书")
}

// profileValidity 从 p7b 里读出的关键字段。
type profileValidity struct {
	NotBefore  int64    `json:"not-before"`
	NotAfter   int64    `json:"not-after"`
	BundleName string   `json:"bundle-name"`
	Type       string   `json:"type"` // debug / release
	DeviceIDs  []string `json:"device-ids"`
}

// parseProfileP7B 从 PKCS#7 包装里取出 profile 的 JSON 原文并解析有效期。
//
// .p7b 是「JSON 原文 + 签名」的 PKCS#7 SignedData（DER 编码）。这里不解签名，
// 只把内嵌的 content 抠出来 —— 签名有效性由 hap-sign-tool 在签名时校验，
// 控制台只需要知道到期时间和 bundleName。
//
// 实现方式：直接在 DER 字节里找 `{`...`}` 的 JSON 片段。相比引入
// go.mozilla.org/pkcs7 依赖，这个方法够用且零依赖 —— profile 的 JSON 是
// PKCS#7 的 encapContent，必然是连续的一段 UTF-8。
func parseProfileP7B(path string) (*profileValidity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := extractProfileJSON(data)
	if err != nil {
		return nil, err
	}
	return decodeProfileJSON(raw)
}

// extractProfileJSON 从 PKCS#7 DER 里定位那段 JSON。
//
// 不能用「最后一个 } 就是结尾」这种启发式：p7b 的签名块是二进制，里面
// 完全可能出现 0x7D（'}'）字节。实测 AGC 导出的真实 profile 就会踩到 ——
// 尾部签名数据里有个 '}'，导致截出的片段后面跟着一堆乱码，解析报
// "invalid character '\u00a0' after top-level value"（错误信息完全不指向
// 真实原因，很容易误判成编码问题）。
//
// 正确做法：找到起点后用 json.Decoder 逐层解码，它天然知道 JSON 在哪里结束，
// 且能跳过尾部垃圾字节。json.Decoder 会读入缓冲区，所以不用担心中间截断。
func extractProfileJSON(data []byte) ([]byte, error) {
	start := bytes.IndexByte(data, '{')
	if start < 0 {
		return nil, errors.New("p7b 里找不到 profile JSON 起始位置")
	}

	// 用 Decoder 解析一份到 RawMessage，借此得到「JSON 实际消耗到哪里」。
	// 注意不能用 Decoder.InputOffset() 之外的偏移猜测 —— offset 是精确的。
	dec := json.NewDecoder(bytes.NewReader(data[start:]))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("p7b 里的 profile JSON 不完整: %w", err)
	}
	end := start + int(dec.InputOffset())
	return data[start:end], nil
}

// decodeProfileJSON 解析 profile JSON 到 profileValidity。
//
// 只取需要的字段：validity / bundle-info.bundle-name / type。
// 这里不用 encoding/json 反序列化整个对象，是因为 profile 里还有
// development-certificate 这种超长 PEM 字符串，解析整个对象没必要且易受
// 转义差异影响 —— 用 json.Unmarshal 到局部结构体最稳。
func decodeProfileJSON(raw []byte) (*profileValidity, error) {
	var v struct {
		Validity struct {
			NotBefore int64 `json:"not-before"`
			NotAfter  int64 `json:"not-after"`
		} `json:"validity"`
		Type       string `json:"type"`
		BundleInfo struct {
			BundleName string `json:"bundle-name"`
		} `json:"bundle-info"`
		// 设备清单不在顶层，而是在 debug-info 里。
		//
		// 实测 AGC 导出的 debug profile 结构是：
		//   "debug-info": { "device-ids": ["B4B1..."], "device-id-type": "udid" }
		// 顶层并没有 device-ids 字段 —— 按顶层解析会永远得到 0 台，
		// 反而误报成「未登记任何设备」。
		DebugInfo struct {
			DeviceIDs    []string `json:"device-ids"`
			DeviceIDType string   `json:"device-id-type"`
		} `json:"debug-info"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("解析 profile JSON 失败: %w", err)
	}
	return &profileValidity{
		NotBefore:  v.Validity.NotBefore,
		NotAfter:   v.Validity.NotAfter,
		BundleName: v.BundleInfo.BundleName,
		Type:       v.Type,
		DeviceIDs:  v.DebugInfo.DeviceIDs,
	}, nil
}

// collectCertInfo 汇总一套签名材料的到期信息。
//
// 设计取舍：任一文件读不出来都不让整体失败，而是把问题记进 Errors ——
// 证书文件缺失是常见状态（还没上传），页面要能正常展示「未知」而不是报错。
func collectCertInfo(f hmosCertFile, warnDays int) *certInfo {
	info := &certInfo{
		CertName:    filepath.Base(f.CertFile),
		ProfileName: filepath.Base(f.ProfileFile),
		Status:      CertUnknown,
	}
	if f.CertFile == "" {
		info.Errors = append(info.Errors, "未配置证书文件")
	}

	// 扩展名由上传内容决定（.cer/.pem、.p7b/.p7c），而调用方按规范名
	// 传路径。这里统一解析到磁盘上真实存在的文件，否则换过扩展名后
	// 会静默变成「未配置证书」。
	certPath := actualPathIfSet(f.CertFile)
	profPath := actualPathIfSet(f.ProfileFile)

	if certPath != "" {
		notAfter, leaf, err := parseCERNotAfter(certPath)
		if err != nil {
			info.Errors = append(info.Errors, "读取证书失败: "+err.Error())
		} else {
			info.CertNotAfter = notAfter.Unix()
			if leaf != nil {
				info.Subject = leaf.Subject.String()
				info.Issuer = leaf.Issuer.String()
				info.CertType = classifyCertType(leaf.Subject.String())
			}
		}
	}

	if profPath == "" {
		info.Errors = append(info.Errors, "未配置 profile 文件")
	} else {
		pv, err := parseProfileP7B(profPath)
		if err != nil {
			info.Errors = append(info.Errors, "读取 profile 失败: "+err.Error())
		} else {
			info.ProfileNotAfter = pv.NotAfter
			info.BundleName = pv.BundleName
			info.ProfileType = pv.Type
			info.ProfileDevices = len(pv.DeviceIDs)
		}
	}

	// 真正失效时间 = 两者较早的那个；只有一个已知时就用它
	switch {
	case info.CertNotAfter > 0 && info.ProfileNotAfter > 0:
		info.EffectiveNotAfter = minInt64(info.CertNotAfter, info.ProfileNotAfter)
	case info.CertNotAfter > 0:
		info.EffectiveNotAfter = info.CertNotAfter
	case info.ProfileNotAfter > 0:
		info.EffectiveNotAfter = info.ProfileNotAfter
	}

	if info.EffectiveNotAfter > 0 {
		// 用 UTC 日期差，避免时区把「还剩 1 天」算成 0 天
		now := time.Now()
		deadline := time.Unix(info.EffectiveNotAfter, 0)
		info.DaysLeft = int(deadline.Sub(now).Hours() / 24)
		switch {
		case info.DaysLeft < 0:
			info.Status = CertExpired
		case info.DaysLeft <= warnDays:
			info.Status = CertWarn
		default:
			info.Status = CertOK
		}
	}

	return info
}

// actualPathIfSet 空路径原样返回（保持「未配置」语义），
// 非空则解析到磁盘上真实存在的同主体名文件。
func actualPathIfSet(p string) string {
	if p == "" {
		return ""
	}
	return actualPath(p)
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// classifyCertType 从证书 Subject 判断是调试证书还是发布证书。
//
// AGC 签发的证书在 Subject 的 OU 后面追加一个标记：
//
//	CN=xxx,Development,OU=...   → 调试证书，1 年
//	CN=xxx,Release,OU=...       → 发布证书，3 年
//
// 这个标记不在独立的 RDN 里，而是拼在 CN 后面用反斜杠转义逗号，
// 所以只能靠子串匹配 —— 想「解析」出 OU 字段反而会因为转义规则
// 出错。子串匹配在这里足够可靠：两个标记词不会同时出现。
//
// 判不出来时返回空串，让前端显示「未知」而不是瞎猜 —— 用户可能上传的是
// 别处导出的老证书，误标成「发布」会让他白折腾一轮。
func classifyCertType(subject string) string {
	switch {
	case strings.Contains(subject, "Development"):
		return "debug"
	case strings.Contains(subject, "Release"):
		return "release"
	default:
		return ""
	}
}

// pgyerInstallBlocker 判断这套签名材料能否让测试者从蒲公英安装。
//
// 返回非空字符串表示装不上，内容就是给用户看的原因。
//
// 为什么需要这个判定：HAP 上传成功 ≠ 测试者能装。蒲公英要用上传的 p12 给
// 安装清单签名，并且只认 release 类型的 profile。debug 类型的 profile 绑定
// 固定 UDID 白名单，蒲公英签出来的清单对普通测试者无效 —— 页面会直接标红
//
//	"当前版本使用调试 Profile 签名，测试者无法通过蒲公英安装"
//
// 这个错误在华为侧不会报，只有传到蒲公英才暴露，所以后台要提前算出来，
// 免得用户反复重传 40MB 的包。
func (c *certInfo) pgyerInstallBlocker() string {
	if c == nil {
		return "尚未上传签名材料"
	}
	switch c.ProfileType {
	case "release":
		// release 类型不限制设备，蒲公英可正常签名分发
		return ""
	case "debug":
		if c.ProfileDevices == 0 {
			return "Profile 是 debug 类型且未登记任何设备，测试者无法从蒲公英安装；" +
				"请到 AGC 重新申请「指定设备发布」或企业 Profile"
		}
		return fmt.Sprintf("Profile 是 debug 类型（仅限 %d 台登记设备），测试者无法从蒲公英安装；"+
			"请到 AGC 重新申请「指定设备发布」或企业 Profile", c.ProfileDevices)
	case "":
		if c.ProfileNotAfter == 0 {
			return "尚未上传 Profile"
		}
		// 老版本 AGC 导出的 profile 可能不带 type 字段，无法判定，不误报
		return ""
	default:
		return fmt.Sprintf("Profile 类型为 %s，蒲公英仅支持 release 类型", c.ProfileType)
	}
}

// certStatusLabel 给日志与提示用。
func certStatusLabel(status string) string {
	switch status {
	case CertOK:
		return "有效"
	case CertWarn:
		return "即将过期"
	case CertExpired:
		return "已过期"
	default:
		return "未知"
	}
}

// normalizeCertPath 把用户填的路径规整一下：去掉首尾空白，空串保持空串。
// 不做绝对路径校验 —— 相对路径按控制台工作目录解析，这是运维场景下的常态。
func normalizeCertPath(p string) string {
	return strings.TrimSpace(p)
}
