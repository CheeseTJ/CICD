package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 蒲公英对鸿蒙包的签名要求：p12 + release 类型 profile。
// 只要 profile 是 debug，HAP 传得上去但测试者装不上，蒲公英页面标红
// "当前版本使用调试 Profile 签名，测试者无法通过蒲公英安装"。
//
// 这些用例锁定判定逻辑，避免以后改解析时把这条关键分支弄丢。

func TestDecodeProfileJSONTypeAndDevices(t *testing.T) {
	// 真实 AGC debug profile 的结构：设备清单在 debug-info 里，不在顶层。
	raw := []byte(`{
		"version-name": "2.0.0",
		"type": "debug",
		"debug-info": {
			"device-ids": ["B4B1F08B24", "AABBCCDD"],
			"device-id-type": "udid"
		},
		"validity": {"not-before": 1700000000, "not-after": 1800000000},
		"bundle-info": {"bundle-name": "com.junet.music"}
	}`)

	pv, err := decodeProfileJSON(raw)
	if err != nil {
		t.Fatalf("decodeProfileJSON 失败: %v", err)
	}
	if pv.Type != "debug" {
		t.Errorf("Type = %q, 期望 debug", pv.Type)
	}
	if len(pv.DeviceIDs) != 2 {
		t.Errorf("DeviceIDs 数量 = %d, 期望 2（必须从 debug-info 读）", len(pv.DeviceIDs))
	}
	if pv.BundleName != "com.junet.music" {
		t.Errorf("BundleName = %q", pv.BundleName)
	}
}

// 顶层误放 device-ids 的情况不该被当成有效设备清单 —— 真实 AGC 文件
// 不会这么写，但早期测试数据可能这么构造，容易误判成「有设备」。
func TestDecodeProfileJSONIgnoresTopLevelDeviceIDs(t *testing.T) {
	raw := []byte(`{
		"type": "debug",
		"device-ids": ["TOPLEVEL"],
		"validity": {"not-before": 1, "not-after": 2},
		"bundle-info": {"bundle-name": "com.x"}
	}`)
	pv, err := decodeProfileJSON(raw)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if len(pv.DeviceIDs) != 0 {
		t.Errorf("顶层 device-ids 不应被识别，得到 %d 项", len(pv.DeviceIDs))
	}
}

func TestDecodeProfileJSONNoDevices(t *testing.T) {
	// release 类 profile 不登记设备，字段可能完全缺失
	raw := []byte(`{
		"type": "release",
		"validity": {"not-before": 1, "not-after": 2},
		"bundle-info": {"bundle-name": "com.x"}
	}`)
	pv, err := decodeProfileJSON(raw)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if pv.Type != "release" {
		t.Errorf("Type = %q", pv.Type)
	}
	if len(pv.DeviceIDs) != 0 {
		t.Errorf("不该有设备, 得到 %d", len(pv.DeviceIDs))
	}
}

func TestPgyerInstallBlocker(t *testing.T) {
	cases := []struct {
		name    string
		info    *certInfo
		wantSub string // 空串表示期望可安装
	}{
		{
			name:    "nil 证书",
			info:    nil,
			wantSub: "尚未上传",
		},
		{
			name:    "release 可安装",
			info:    &certInfo{ProfileType: "release", ProfileNotAfter: 1},
			wantSub: "",
		},
		{
			name:    "debug 有设备 不可安装",
			info:    &certInfo{ProfileType: "debug", ProfileNotAfter: 1, ProfileDevices: 3},
			wantSub: "debug",
		},
		{
			name:    "debug 无设备 不可安装",
			info:    &certInfo{ProfileType: "debug", ProfileNotAfter: 1, ProfileDevices: 0},
			wantSub: "未登记任何设备",
		},
		{
			name:    "未上传 profile",
			info:    &certInfo{},
			wantSub: "尚未上传 Profile",
		},
		{
			name:    "老 profile 无 type 字段不误报",
			info:    &certInfo{ProfileNotAfter: 123, ProfileType: ""},
			wantSub: "",
		},
		{
			name:    "未知类型",
			info:    &certInfo{ProfileType: "adhoc", ProfileNotAfter: 1},
			wantSub: "adhoc",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.info.pgyerInstallBlocker()
			if c.wantSub == "" {
				if got != "" {
					t.Errorf("期望可安装，但返回 %q", got)
				}
				return
			}
			if !strings.Contains(got, c.wantSub) {
				t.Errorf("返回值 %q 不包含 %q", got, c.wantSub)
			}
		})
	}
}

// debug profile 带设备清单时，提示里要写明设备数 —— 用户看到「仅限 3 台」
// 才知道自己不是配置错，而是这种 profile 天然只能给登记过的机器装。
func TestPgyerInstallBlockerMentionsDeviceCount(t *testing.T) {
	c := &certInfo{ProfileType: "debug", ProfileNotAfter: 1, ProfileDevices: 3}
	got := c.pgyerInstallBlocker()
	if !strings.Contains(got, "3") {
		t.Errorf("提示里应包含设备数 3, 实际: %s", got)
	}
}

// collectCertInfo 要把 profile 的 type 和设备数一路带到 certInfo。
func TestCollectCertInfoCarriesProfileType(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "profile.p7b")
	raw := `{"type":"debug","debug-info":{"device-ids":["A","B","C"]},
		"validity":{"not-before":1700000000,"not-after":4900000000},
		"bundle-info":{"bundle-name":"com.junet.music"}}`
	if err := os.WriteFile(prof, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	info := collectCertInfo(hmosCertFile{ProfileFile: prof}, 30)
	if info.ProfileType != "debug" {
		t.Errorf("ProfileType = %q, 期望 debug", info.ProfileType)
	}
	if info.ProfileDevices != 3 {
		t.Errorf("ProfileDevices = %d, 期望 3", info.ProfileDevices)
	}
}

// 下发给前端的 JSON 必须带这两个字段，否则前端无法渲染警告。
func TestHmosViewJSONHasBlocker(t *testing.T) {
	v := hmosView{
		Cert:                &certInfo{ProfileType: "debug", ProfileDevices: 1, ProfileNotAfter: 1},
		PgyerInstallBlocker: "x",
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, k := range []string{`"pgyer_install_blocker"`, `"profile_type"`, `"profile_devices"`} {
		if !strings.Contains(s, k) {
			t.Errorf("下发 JSON 缺少 %s", k)
		}
	}
}
