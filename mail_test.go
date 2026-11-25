package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 邮件网关客户端与到期提醒的测试。
//
// 这套设计的关键取舍是「没配网关就静默跳过」，所以相当一部分测试在验证
// 各种「不该发信」的情况 —— 那是最容易出问题的地方：如果误发，用户会
// 在没配邮件的情况下收到莫名其妙的错误日志，或者本地开发时被邮件轰炸。

func TestMailConfigEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  mailConfig
		want bool
	}{
		{"完整配置", mailConfig{URL: "https://mail.test/send", To: []string{"a@b.c"}}, true},
		{"缺 URL", mailConfig{To: []string{"a@b.c"}}, false},
		{"缺收件人", mailConfig{URL: "https://mail.test/send"}, false},
		{"全空", mailConfig{}, false},
		{"URL 只有空白", mailConfig{URL: "   ", To: []string{"a@b.c"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.enabled(); got != c.want {
				t.Errorf("enabled() = %v, 期望 %v", got, c.want)
			}
		})
	}
}

// 未配置时 send 必须直接返回 nil，不能报错 —— 否则调用方会以为出问题了。
func TestMailClientDisabledSendIsNoop(t *testing.T) {
	c := newMailClient(mailConfig{})
	called := false
	c.post = func(context.Context, string, string, []byte) error {
		called = true
		return nil
	}
	if err := c.send(context.Background(), "s", "b"); err != nil {
		t.Fatalf("未配置时不该报错: %v", err)
	}
	if called {
		t.Error("未配置时不该真的发请求")
	}
}

// 请求体必须包含 to / subject / html 三个字段，网关按这个契约解析。
func TestMailClientPayload(t *testing.T) {
	c := newMailClient(mailConfig{
		URL:   "https://mail.test/send",
		Token: "tk",
		To:    []string{"a@b.c", "d@e.f"},
	})
	var gotURL, gotToken string
	var gotBody []byte
	c.post = func(_ context.Context, url, token string, body []byte) error {
		gotURL, gotToken, gotBody = url, token, body
		return nil
	}

	if err := c.send(context.Background(), "主题", "<b>正文</b>"); err != nil {
		t.Fatal(err)
	}
	if gotURL != "https://mail.test/send" {
		t.Errorf("URL = %q", gotURL)
	}
	if gotToken != "tk" {
		t.Errorf("Token = %q", gotToken)
	}

	var payload struct {
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		HTML    string   `json:"html"`
	}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	if len(payload.To) != 2 || payload.To[0] != "a@b.c" {
		t.Errorf("收件人不符: %v", payload.To)
	}
	if payload.Subject != "主题" {
		t.Errorf("主题 = %q", payload.Subject)
	}
	if payload.HTML != "<b>正文</b>" {
		t.Errorf("正文 = %q", payload.HTML)
	}
}

// ---------------------------------------------------------------- 到期收集

// makeNoticeApp 造一个带真实 profile 文件的应用。
//
// 只写 profile 不写证书：collectCertInfo 对证书缺失会记进 Errors，
// 但不影响 profile 到期时间的计算 —— 而本组测试关心的正是各类材料
// 的到期判定，用 profile 就够。
func makeNoticeApp(t *testing.T, root, bundle, name string, profDays int) *HmosApp {
	t.Helper()
	_, profPath, _, _ := hmosAppPaths(root, bundle)
	if err := os.MkdirAll(filepath.Dir(profPath), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(profPath, buildProfileBytes(t, bundle, profDays), 0o600); err != nil {
		t.Fatalf("写 profile 失败: %v", err)
	}
	return &HmosApp{ID: bundle, BundleName: bundle, Name: name}
}

func TestCollectExpiryNoticesFiltersByThreshold(t *testing.T) {
	root := t.TempDir()

	// 剩余 10 天 → 在 30 天阈值内，应被收集
	near := makeNoticeApp(t, root, "com.near", "快到期", 10)
	// 剩余 100 天 → 超出阈值，不该出现
	far := makeNoticeApp(t, root, "com.far", "还早", 100)

	got := collectExpiryNotices([]*HmosApp{near, far}, 30, root)
	if len(got) != 1 {
		t.Fatalf("应只收集 1 条，实际 %d 条: %+v", len(got), got)
	}
	if got[0].Bundle != "com.near" {
		t.Errorf("收集到了错误的应用: %s", got[0].Bundle)
	}
	if got[0].Kind != "Profile" {
		t.Errorf("类型 = %q", got[0].Kind)
	}
	if got[0].DaysLeft < 9 || got[0].DaysLeft > 10 {
		t.Errorf("剩余天数 = %d, 期望约 10", got[0].DaysLeft)
	}
}

func TestCollectExpiryNoticesIncludesExpired(t *testing.T) {
	root := t.TempDir()
	// -5 天 → 已过期，必须报（负数 < 阈值）
	overdue := makeNoticeApp(t, root, "com.old", "已过期", -5)

	got := collectExpiryNotices([]*HmosApp{overdue}, 30, root)
	if len(got) != 1 {
		t.Fatalf("已过期必须被收集，实际 %d 条", len(got))
	}
	if got[0].DaysLeft >= 0 {
		t.Errorf("剩余天数应为负，实际 %d", got[0].DaysLeft)
	}
}

// 文件缺失（没上传过证书）不算到期 —— 那不是「快过期」，是「还没配」。
func TestCollectExpiryNoticesSkipsMissingFiles(t *testing.T) {
	root := t.TempDir()
	app := &HmosApp{ID: "com.none", BundleName: "com.none", Name: "空"}

	got := collectExpiryNotices([]*HmosApp{app}, 30, root)
	if len(got) != 0 {
		t.Errorf("文件缺失时不该产生到期项，得到 %+v", got)
	}
}

// 最快到期的排前面，方便用户在邮件里先看最紧急的。
func TestCollectExpiryNoticesSortedByUrgency(t *testing.T) {
	root := t.TempDir()
	a := makeNoticeApp(t, root, "com.a", "A", 20)
	b := makeNoticeApp(t, root, "com.b", "B", 3)
	c := makeNoticeApp(t, root, "com.c", "C", -1)

	got := collectExpiryNotices([]*HmosApp{a, b, c}, 30, root)
	if len(got) != 3 {
		t.Fatalf("应收集 3 条，实际 %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].DaysLeft > got[i].DaysLeft {
			t.Errorf("未按紧急度排序: %d 在 %d 前面", got[i-1].DaysLeft, got[i].DaysLeft)
		}
	}
}

// ---------------------------------------------------------------- 邮件内容

func TestBuildExpiryMailSubject(t *testing.T) {
	now := time.Now()

	// 有已过期项 → 主题要突出「已过期」
	expired := []expiryNotice{
		{AppName: "A", Bundle: "com.a", Kind: "Profile", DaysLeft: -3, ExpireAt: now},
		{AppName: "B", Bundle: "com.b", Kind: "证书", DaysLeft: 20, ExpireAt: now},
	}
	subj, _ := buildExpiryMail(expired, now)
	if !strings.Contains(subj, "已过期") {
		t.Errorf("有过期项时主题应含「已过期」，实际 %q", subj)
	}

	// 都还没过期 → 主题给最快剩余天数
	warn := []expiryNotice{
		{AppName: "A", Bundle: "com.a", Kind: "Profile", DaysLeft: 12, ExpireAt: now},
	}
	subj2, _ := buildExpiryMail(warn, now)
	if !strings.Contains(subj2, "12") {
		t.Errorf("主题应含最快剩余天数 12，实际 %q", subj2)
	}
}

func TestBuildExpiryMailContent(t *testing.T) {
	now := time.Now()
	notices := []expiryNotice{
		{AppName: "音乐", Bundle: "com.junet.music", Kind: "Profile", DaysLeft: 5,
			ExpireAt: now.AddDate(0, 0, 5), Hint: "去 AGC 重新申请"},
	}
	_, body := buildExpiryMail(notices, now)

	for _, want := range []string{"音乐", "com.junet.music", "Profile", "去 AGC 重新申请"} {
		if !strings.Contains(body, want) {
			t.Errorf("正文缺少 %q", want)
		}
	}
}

// 包名里的尖括号必须被转义，否则会破坏 HTML 结构（甚至撑破表格）。
func TestBuildExpiryMailEscapesHTML(t *testing.T) {
	now := time.Now()
	notices := []expiryNotice{
		{AppName: "<script>x</script>", Bundle: "com.<evil>", Kind: "Profile",
			DaysLeft: 5, ExpireAt: now},
	}
	_, body := buildExpiryMail(notices, now)

	if strings.Contains(body, "<script>") {
		t.Error("应用名里的 <script> 未被转义")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("应转义为 &lt;script&gt;")
	}
	if strings.Contains(body, "com.<evil>") {
		t.Error("包名里的尖括号未被转义")
	}
}

// 同类材料的处理建议只出现一次，避免邮件里重复三遍同一句话。
func TestBuildExpiryMailDedupesHints(t *testing.T) {
	now := time.Now()
	notices := []expiryNotice{
		{AppName: "A", Bundle: "com.a", Kind: "Profile", DaysLeft: 5,
			ExpireAt: now, Hint: "重新申请 Profile"},
		{AppName: "B", Bundle: "com.b", Kind: "Profile", DaysLeft: 8,
			ExpireAt: now, Hint: "重新申请 Profile"},
	}
	_, body := buildExpiryMail(notices, now)
	if n := strings.Count(body, "重新申请 Profile"); n != 1 {
		t.Errorf("同类建议应只出现 1 次，实际 %d 次", n)
	}
}

// ---------------------------------------------------------------- 每日去重

// 同一天内只能发一封。检查跟着 10 分钟 ticker 跑，不去重会一天 144 封。
func TestCheckAndNotifySendsOncePerDay(t *testing.T) {
	root := t.TempDir()
	app := makeNoticeApp(t, root, "com.junet.music", "Music", 5)

	s := &server{
		cfg:  &config{HmosWarnDays: 30, HmosUploadDir: root},
		hmos: newTestHmosStore(t, app),
	}
	var sends int
	s.mail = newMailClient(mailConfig{URL: "https://x/send", To: []string{"a@b.c"}})
	s.mail.post = func(context.Context, string, string, []byte) error {
		sends++
		return nil
	}

	if n := s.checkAndNotifyExpiry(); n != 1 {
		t.Fatalf("第一次应发送 1 封，实际 %d", n)
	}
	if n := s.checkAndNotifyExpiry(); n != 0 {
		t.Fatalf("同一天第二次不该再发，实际 %d", n)
	}
	if sends != 1 {
		t.Errorf("实际请求次数 = %d, 期望 1", sends)
	}
}

// 发送失败时不能标记「今天已发」，否则当天再也不会重试。
func TestCheckAndNotifyRetriesAfterFailure(t *testing.T) {
	root := t.TempDir()
	app := makeNoticeApp(t, root, "com.junet.music", "Music", 5)

	s := &server{
		cfg:  &config{HmosWarnDays: 30, HmosUploadDir: root},
		hmos: newTestHmosStore(t, app),
	}
	var attempts int
	s.mail = newMailClient(mailConfig{URL: "https://x/send", To: []string{"a@b.c"}})
	s.mail.post = func(context.Context, string, string, []byte) error {
		attempts++
		return errors.New("网关挂了")
	}

	if n := s.checkAndNotifyExpiry(); n != 0 {
		t.Fatalf("失败时应返回 0，实际 %d", n)
	}
	// 再试一次：失败没被标记成「已发」，所以还会尝试
	if n := s.checkAndNotifyExpiry(); n != 0 {
		t.Fatalf("仍应返回 0，实际 %d", n)
	}
	if attempts != 2 {
		t.Errorf("失败后应允许重试，实际尝试 %d 次", attempts)
	}
}

// 没配网关时完全不动作，也不报错。
func TestCheckAndNotifyNoMailConfig(t *testing.T) {
	root := t.TempDir()
	app := makeNoticeApp(t, root, "com.junet.music", "Music", 5)

	s := &server{
		cfg:  &config{HmosWarnDays: 30, HmosUploadDir: root},
		hmos: newTestHmosStore(t, app),
		mail: newMailClient(mailConfig{}),
	}
	if n := s.checkAndNotifyExpiry(); n != 0 {
		t.Errorf("未配置网关时不该发送，实际 %d", n)
	}
}

// 没有临近到期的应用时不发信 —— 不然用户天天收空提醒。
func TestCheckAndNotifyNoExpiringApps(t *testing.T) {
	root := t.TempDir()
	app := makeNoticeApp(t, root, "com.junet.music", "Music", 365)

	s := &server{
		cfg:  &config{HmosWarnDays: 30, HmosUploadDir: root},
		hmos: newTestHmosStore(t, app),
	}
	s.mail = newMailClient(mailConfig{URL: "https://x/send", To: []string{"a@b.c"}})
	sent := false
	s.mail.post = func(context.Context, string, string, []byte) error {
		sent = true
		return nil
	}

	if n := s.checkAndNotifyExpiry(); n != 0 {
		t.Errorf("没有到期项不该发信，实际 %d", n)
	}
	if sent {
		t.Error("不该真的发请求")
	}
}

// ---------------------------------------------------------------- 测试工具

// newTestHmosStore 造一个只含指定应用的临时 store。
func newTestHmosStore(t *testing.T, apps ...*HmosApp) *HmosStore {
	t.Helper()
	st, err := NewHmosStore(filepath.Join(t.TempDir(), "hmos.json"))
	if err != nil {
		t.Fatalf("创建 store 失败: %v", err)
	}
	for _, a := range apps {
		if err := st.Upsert(a); err != nil {
			t.Fatalf("写入测试应用失败: %v", err)
		}
	}
	return st
}
