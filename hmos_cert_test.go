package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePEMCert 生成一张自签证书并写成 PEM，返回路径与 notAfter。
func writePEMCert(t *testing.T, dir, name string, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "Test Leaf"},
		Issuer:       pkix.Name{CommonName: "Test CA"},
		NotBefore:    notAfter.Add(-365 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建文件失败: %v", err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("写 PEM 失败: %v", err)
	}
	return path
}

// writeProfile 造一个「JSON 外面套 PKCS#7 外壳」的 p7b 近似文件。
//
// 真实的 p7b 是 DER 编码的 SignedData，JSON 作为 encapContent 嵌在里面。
// 解析器只做「在字节流里找一段 JSON」，所以这里构造 [前缀][JSON][后缀] 即可
// 覆盖解析逻辑；签名有效性由 hap-sign-tool 校验，不是本模块的职责。
func writeProfile(t *testing.T, dir, name, bundle string, notAfter int64) string {
	t.Helper()
	payload := map[string]interface{}{
		"version-name": "2.0.0",
		"version-code": 2,
		"validity": map[string]int64{
			"not-before": notAfter - 365*86400,
			"not-after":  notAfter,
		},
		"type": "release",
		"bundle-info": map[string]interface{}{
			"bundle-name": bundle,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化 profile 失败: %v", err)
	}
	// 模拟 DER 头尾，验证解析器能跳过非 JSON 字节
	out := append([]byte{0x30, 0x82, 0x01, 0x00}, body...)
	out = append(out, 0x00, 0x01, 0x02)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("写 profile 失败: %v", err)
	}
	return path
}

func TestParseCertNotAfter(t *testing.T) {
	dir := t.TempDir()
	want := time.Date(2027, 6, 15, 12, 0, 0, 0, time.UTC)
	path := writePEMCert(t, dir, "cert.pem", want)

	got, leaf, err := parseCERNotAfter(path)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !got.Equal(want.UTC().Truncate(time.Second)) {
		t.Errorf("notAfter 不符: got %v want %v", got, want)
	}
	if leaf == nil || leaf.Subject.CommonName != "Test Leaf" {
		t.Errorf("叶子证书不对: %+v", leaf)
	}
}

// 链里有多张证书时，应取**最早**的 notAfter —— 先过期的决定整体可用期。
func TestParseCertPicksEarliest(t *testing.T) {
	dir := t.TempDir()
	early := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	p1 := writePEMCert(t, dir, "a.pem", late)
	p2 := writePEMCert(t, dir, "b.pem", early)
	// 拼接成一个文件，模拟证书链
	a, _ := os.ReadFile(p1)
	b, _ := os.ReadFile(p2)
	chain := filepath.Join(dir, "chain.pem")
	if err := os.WriteFile(chain, append(a, b...), 0o600); err != nil {
		t.Fatal(err)
	}

	got, _, err := parseCERNotAfter(chain)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !got.Equal(early) {
		t.Errorf("应取最早的 notAfter，got %v want %v", got, early)
	}
}

func TestParseProfile(t *testing.T) {
	dir := t.TempDir()
	notAfter := time.Now().Add(100 * 24 * time.Hour).Unix()
	path := writeProfile(t, dir, "profile.p7b", "com.test.app", notAfter)

	pv, err := parseProfileP7B(path)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if pv.NotAfter != notAfter {
		t.Errorf("not-after 不符: got %d want %d", pv.NotAfter, notAfter)
	}
	if pv.BundleName != "com.test.app" {
		t.Errorf("bundleName 不符: got %q", pv.BundleName)
	}
}

// collectCertInfo 的状态分级：ok / warn / expired / unknown。
func TestCollectCertInfoStatus(t *testing.T) {
	dir := t.TempDir()
	const warnDays = 30

	cases := []struct {
		name     string
		daysLeft int
		want     string
	}{
		{"充足", 200, CertOK},
		{"刚好阈值", 30, CertWarn},
		{"即将过期", 5, CertWarn},
		{"已过期", -3, CertExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			notAfter := time.Now().Add(time.Duration(c.daysLeft)*24*time.Hour - time.Hour)
			certPath := writePEMCert(t, dir, "c-"+c.name+".pem", notAfter)
			profPath := writeProfile(t, dir, "p-"+c.name+".p7b", "com.test.app", notAfter.Unix())

			info := collectCertInfo(hmosCertFile{
				CertFile:    certPath,
				ProfileFile: profPath,
				KeyAlias:    "k",
			}, warnDays)

			if info.Status != c.want {
				t.Errorf("状态不符: got %s want %s (days=%d)", info.Status, c.want, info.DaysLeft)
			}
		})
	}
}

// 文件缺失时不应报错，而是标记为 unknown 并给出原因 —— 页面要能正常渲染。
func TestCollectCertInfoMissingFiles(t *testing.T) {
	info := collectCertInfo(hmosCertFile{}, 30)
	if info.Status != CertUnknown {
		t.Errorf("缺文件时应为 unknown，got %s", info.Status)
	}
	if len(info.Errors) == 0 {
		t.Error("应该有错误说明")
	}
	// 不能因为没文件就 panic 或给出负的有效期
	if info.EffectiveNotAfter != 0 {
		t.Errorf("无文件时 EffectiveNotAfter 应为 0，got %d", info.EffectiveNotAfter)
	}
}

// 证书与 profile 两个有效期取较早者。
func TestCollectCertInfoTakesEarlier(t *testing.T) {
	dir := t.TempDir()
	// 证书 200 天，profile 10 天 → 实际有效期应是 10 天
	certPath := writePEMCert(t, dir, "c.pem", time.Now().Add(200*24*time.Hour))
	profNotAfter := time.Now().Add(10 * 24 * time.Hour).Unix()
	profPath := writeProfile(t, dir, "p.p7b", "com.test.app", profNotAfter)

	info := collectCertInfo(hmosCertFile{
		CertFile:    certPath,
		ProfileFile: profPath,
	}, 30)

	if info.EffectiveNotAfter != profNotAfter {
		t.Errorf("应取较早的 profile 有效期: got %d want %d", info.EffectiveNotAfter, profNotAfter)
	}
	if info.Status != CertWarn {
		t.Errorf("10 天应判为 warn，got %s", info.Status)
	}
}

// 存储的增删改查与落盘。
func TestHmosStoreCRUD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hmos.json")

	s, err := NewHmosStore(path)
	if err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	app := &HmosApp{
		BundleName: "com.test.app",
		Name:       "Test",
		CertFile:   "/tmp/cert.pem",
	}
	if err := s.Upsert(app); err != nil {
		t.Fatalf("Upsert 失败: %v", err)
	}
	if app.ID != "com.test.app" {
		t.Errorf("ID 应回填为 bundleName，got %q", app.ID)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	// 重新加载验证持久化
	s2, err := NewHmosStore(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	got, ok := s2.Get("com.test.app")
	if !ok {
		t.Fatal("重新加载后应能查到")
	}
	if got.Name != "Test" || got.CertFile != "/tmp/cert.pem" {
		t.Errorf("字段丢失: %+v", got)
	}

	// Patch 局部更新
	if _, err := s2.Patch("com.test.app", func(a *HmosApp) { a.LastResignAt = 123 }); err != nil {
		t.Fatalf("Patch 失败: %v", err)
	}
	got2, _ := s2.Get("com.test.app")
	if got2.LastResignAt != 123 {
		t.Errorf("Patch 未生效")
	}
	if got2.Name != "Test" {
		t.Errorf("Patch 不应清掉其他字段")
	}

	// 删除
	if err := s2.Delete("com.test.app"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	if _, ok := s2.Get("com.test.app"); ok {
		t.Error("删除后不应还能查到")
	}
}

func TestHmosStoreRejectsEmptyBundle(t *testing.T) {
	s, _ := NewHmosStore(filepath.Join(t.TempDir(), "h.json"))
	if err := s.Upsert(&HmosApp{}); err == nil {
		t.Error("bundle_name 为空时应报错")
	}
}

// Get 返回副本：外部修改不应影响存储内部状态。
func TestHmosStoreGetReturnsCopy(t *testing.T) {
	s, _ := NewHmosStore(filepath.Join(t.TempDir(), "h.json"))
	_ = s.Upsert(&HmosApp{BundleName: "a.b", Name: "orig"})

	got, _ := s.Get("a.b")
	got.Name = "hacked"

	again, _ := s.Get("a.b")
	if again.Name != "orig" {
		t.Errorf("Get 应返回副本，内部状态被污染: %q", again.Name)
	}
}

// p7b 尾部的签名块是二进制，里面可能含 '}' 字节。
//
// 之前的实现用「最后一个 '}' 就是 JSON 结尾」这种启发式，遇到真实 AGC
// profile 时截出的片段带着尾部乱码，报
// "invalid character '\u00a0' after top-level value" —— 这个错误信息
// 完全不指向真实原因（用户会以为是编码或空格问题）。
//
// 这个测试用真实文件的结构复现：JSON 后面跟一段含 0x7D 的二进制。
func TestParseProfileWithBinaryTailContainingBrace(t *testing.T) {
	dir := t.TempDir()
	notAfter := time.Now().Add(200 * 24 * time.Hour).Unix()

	payload := map[string]interface{}{
		"version-name": "2.0.0",
		"version-code": 2,
		"validity": map[string]int64{
			"not-before": notAfter - 365*86400,
			"not-after":  notAfter,
		},
		"type": "debug",
		"bundle-info": map[string]interface{}{
			"bundle-name": "com.junet.music",
			"device-ids":  []string{"AABBCC"},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	// 构造：DER头 + JSON + 含 '}' 与 0xA0 的签名块
	blob := []byte{0x30, 0x82, 0x01, 0x00}
	blob = append(blob, body...)
	blob = append(blob, 0x7D, 0xC2, 0xA0, 0x00, 0x7D, 0x7D, 0xFF)

	path := filepath.Join(dir, "real-like.p7b")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	pv, err := parseProfileP7B(path)
	if err != nil {
		t.Fatalf("含 '}' 的尾部不应导致解析失败: %v", err)
	}
	if pv.NotAfter != notAfter {
		t.Errorf("not-after 不符: got %d want %d", pv.NotAfter, notAfter)
	}
	if pv.BundleName != "com.junet.music" {
		t.Errorf("bundle-name 不符: got %q", pv.BundleName)
	}
}
