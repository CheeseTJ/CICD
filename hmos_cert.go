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
	NotBefore  int64  `json:"not-before"`
	NotAfter   int64  `json:"not-after"`
	BundleName string `json:"bundle-name"`
	Type       string `json:"type"` // debug / release
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
func extractProfileJSON(data []byte) ([]byte, error) {
	start := -1
	for i, b := range data {
		if b == '{' {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, errors.New("p7b 里找不到 profile JSON 起始位置")
	}
	// 从 last '}' 往前找，配对第一个 '{'：JSON 原文是整段连续的
	end := -1
	for i := len(data) - 1; i > start; i-- {
		if data[i] == '}' {
			end = i + 1
			break
		}
	}
	if end <= start {
		return nil, errors.New("p7b 里找不到 profile JSON 结束位置")
	}
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
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("解析 profile JSON 失败: %w", err)
	}
	return &profileValidity{
		NotBefore:  v.Validity.NotBefore,
		NotAfter:   v.Validity.NotAfter,
		BundleName: v.BundleInfo.BundleName,
		Type:       v.Type,
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

	if f.CertFile != "" {
		notAfter, leaf, err := parseCERNotAfter(f.CertFile)
		if err != nil {
			info.Errors = append(info.Errors, "读取证书失败: "+err.Error())
		} else {
			info.CertNotAfter = notAfter.Unix()
			if leaf != nil {
				info.Subject = leaf.Subject.String()
				info.Issuer = leaf.Issuer.String()
			}
		}
	}

	if f.ProfileFile == "" {
		info.Errors = append(info.Errors, "未配置 profile 文件")
	} else {
		pv, err := parseProfileP7B(f.ProfileFile)
		if err != nil {
			info.Errors = append(info.Errors, "读取 profile 失败: "+err.Error())
		} else {
			info.ProfileNotAfter = pv.NotAfter
			info.BundleName = pv.BundleName
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

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
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
