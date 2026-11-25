package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 分发类型与侧载判定。
//
// 这组测试锁死一个重要结论：app_gallery **不能**侧载。
//
// 之前我按「type=release + 设备数 0 = 不限设备所以能装」写过一次判断，
// 那是错的 —— 0 台设备只说明不靠 UDID 白名单限制，app_gallery 会用
// 「必须走应用市场」这条规则把侧载彻底堵死。这个错判会让用户在
// 「下载 → 装不上 → 重新签名 → 还是装不上」里来回打转。

func TestJudgeSideloadAppGalleryIsBlocked(t *testing.T) {
	// 这就是用户当前的实际配置：release + 0 台设备 + app_gallery
	v := judgeSideload(DistributionAppGallery, "release", 0)
	if v.Allowed {
		t.Fatal("app_gallery 必须判为不可侧载 —— 它只能从应用市场安装")
	}
	if v.Reason == "" {
		t.Error("不可侧载时必须说明原因")
	}
	if v.Hint == "" {
		t.Error("必须给出下一步该怎么做，否则用户不知道要改什么")
	}
}

// 即使 device-ids 有值，app_gallery 依然不能侧载 —— 分发类型是硬约束，
// 跟设备数无关。防止有人把「设备数 > 0」当成可装的条件。
func TestJudgeSideloadAppGalleryBlockedEvenWithDevices(t *testing.T) {
	v := judgeSideload(DistributionAppGallery, "release", 3)
	if v.Allowed {
		t.Error("app_gallery 无论登记多少设备都不能侧载")
	}
}

func TestJudgeSideloadInternalTesting(t *testing.T) {
	// 有设备 → 可装，仅限白名单
	v := judgeSideload(DistributionInternalTesting, "release", 2)
	if !v.Allowed {
		t.Error("指定设备发布 + 有设备，应可侧载")
	}

	// 无设备 → 一台都装不了，多半是 AGC 里漏配了
	v0 := judgeSideload(DistributionInternalTesting, "release", 0)
	if v0.Allowed {
		t.Error("指定设备发布但 0 台设备，实际没有设备能安装")
	}
	if v0.Hint == "" {
		t.Error("应提示去 AGC 添加设备 UDID")
	}
}

func TestJudgeSideloadEnterprise(t *testing.T) {
	v := judgeSideload(DistributionEnterprise, "release", 0)
	if !v.Allowed {
		t.Error("企业分发应支持侧载")
	}
}

// 老 profile 可能不带 app-distribution-type，这时退回看 type。
// 不能直接判 false —— 会让本来能装的调试包被误判。
func TestJudgeSideloadFallsBackToProfileType(t *testing.T) {
	// debug + 有设备 → 可装
	v := judgeSideload("", "debug", 1)
	if !v.Allowed {
		t.Error("调试 profile + 已登记设备，应可侧载")
	}

	// debug + 无设备 → 装不了
	v0 := judgeSideload("", "debug", 0)
	if v0.Allowed {
		t.Error("调试 profile 但 0 台设备，一台都装不上")
	}

	// 什么都没有 → 不能瞎猜成可装
	empty := judgeSideload("", "", 0)
	if empty.Allowed {
		t.Error("信息不足时不能判为可侧载")
	}
}

func TestDistributionLabel(t *testing.T) {
	cases := map[string]string{
		DistributionAppGallery:      "应用市场",
		DistributionInternalTesting: "指定设备发布",
		DistributionEnterprise:      "企业分发",
		"":                          "",
		"something":                 "something",
	}
	for in, want := range cases {
		if got := distributionLabel(in); got != want {
			t.Errorf("distributionLabel(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// profileDistribution 要能从真实 p7b 结构里取出 app-distribution-type。
// 这里构造一段带 PKCS#7 尾部的字节，验证它不会把尾部垃圾当 JSON。
func TestProfileDistributionFromP7B(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "profile.p7b")

	// 前面放点二进制噪声，模拟 DER 头；JSON 后面再放含 } 的垃圾，
	// 复现真实 p7b「签名块里有 0x7D」的情况
	head := []byte{0x30, 0x82, 0x01, 0x00}
	body := []byte(`{"app-distribution-type":"internaltesting","type":"release",` +
		`"validity":{"not-before":1,"not-after":2},` +
		`"bundle-info":{"bundle-name":"com.x"}}`)
	tail := []byte{0x00, 0x7D, 0xAB, 0x7D, 0xFF}

	data := append(append(append([]byte{}, head...), body...), tail...)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if got := profileDistribution(p); got != DistributionInternalTesting {
		t.Errorf("profileDistribution = %q, 期望 %q", got, DistributionInternalTesting)
	}
}

func TestProfileDistributionMissingFile(t *testing.T) {
	if got := profileDistribution(""); got != "" {
		t.Errorf("空路径应返回空串，得到 %q", got)
	}
	if got := profileDistribution("/nonexistent/p.p7b"); got != "" {
		t.Errorf("文件不存在应返回空串，得到 %q", got)
	}
}

// 下发给前端的 JSON 必须带这两个字段，否则前端无法渲染警告。
func TestHmosViewJSONHasSideloadFields(t *testing.T) {
	v := hmosView{
		Sideload:     judgeSideload(DistributionAppGallery, "release", 0),
		Distribution: DistributionAppGallery,
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"sideload"`, `"distribution"`, `"allowed"`, `"reason"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("下发 JSON 缺少 %s", k)
		}
	}
}

// 扩展名不一致时也必须能读到 profile。
//
// 踩过的 bug：上传 .p7c 后文件存成 profile.p7c，而视图层按规范名
// profile.p7b 推导路径 → os.ReadFile 失败 → 分发类型读成空
// → 界面误报「Profile 缺少分发类型字段，无法判断能否侧载」。
// 用户看到这句话完全不知道是扩展名的问题。
func TestProfileDistributionHandlesAltExtension(t *testing.T) {
	dir := t.TempDir()
	// 实际文件叫 profile.p7c
	p7c := filepath.Join(dir, "profile.p7c")
	body := []byte(`{"app-distribution-type":"internaltesting","type":"release"}`)
	if err := os.WriteFile(p7c, body, 0o600); err != nil {
		t.Fatal(err)
	}

	// 调用方传的是规范名 profile.p7b
	canonical := filepath.Join(dir, "profile.p7b")
	got := profileDistribution(actualPath(canonical))
	if got != DistributionInternalTesting {
		t.Errorf("应通过 actualPath 找到 .p7c，得到 %q", got)
	}
}

// actualPath 找不到同主体名的文件时原样返回，让调用方正常报「文件不存在」。
func TestActualPathFallsBackToCanonical(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "profile.p7b")
	if got := actualPath(canonical); got != canonical {
		t.Errorf("无匹配文件时应原样返回，得到 %q", got)
	}
}

// 规范名存在时直接用，不去扫目录。
func TestActualPathPrefersCanonical(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "profile.p7b")
	os.WriteFile(canonical, []byte("x"), 0o600)
	// 同时存在另一个同主体名的文件
	os.WriteFile(filepath.Join(dir, "profile.p7c"), []byte("y"), 0o600)

	if got := actualPath(canonical); got != canonical {
		t.Errorf("应优先用规范名，得到 %q", got)
	}
}

// .part 临时文件不能被当成正式文件。
func TestActualPathSkipsPartFiles(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "profile.p7b")
	// 只有 .part（上传中断的残留）
	os.WriteFile(filepath.Join(dir, "profile.p7b.part"), []byte("x"), 0o600)

	if got := actualPath(canonical); got != canonical {
		t.Errorf(".part 不该被选中，得到 %q", got)
	}
}

// collectCertInfo 也要能处理备用扩展名。
func TestCollectCertInfoHandlesAltExtensions(t *testing.T) {
	dir := t.TempDir()
	// profile 用 .p7c
	p7c := filepath.Join(dir, "profile.p7c")
	now := time.Now().Unix()
	raw := []byte(`{"type":"release","validity":{"not-before":` +
		itoa(now-86400) + `,"not-after":` + itoa(now+86400*100) +
		`},"bundle-info":{"bundle-name":"com.x"}}`)
	os.WriteFile(p7c, raw, 0o600)

	// 传规范名
	info := collectCertInfo(hmosCertFile{
		ProfileFile: filepath.Join(dir, "profile.p7b"),
	}, 30)

	if info.ProfileNotAfter == 0 {
		t.Error("应通过 actualPath 读到 .p7c 的有效期")
	}
	if info.ProfileType != "release" {
		t.Errorf("ProfileType = %q", info.ProfileType)
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
