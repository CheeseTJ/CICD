package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// 证书/Profile 到期提醒。
//
// 为什么自动重签解决不了这件事
// --------------------------
// autoResignOnce 只会用**现有** cert/profile 重签 HAP。Profile 快过期时它
// 照样能签出一个能装的包，但 30 天后那份 profile 失效，包就废了。真正要做的
// 是去 AGC 申请新 Profile —— 这一步没有 API，只能人工。
//
// 所以提醒的价值在于「在失效前让人知道自己得去点几下」，而不是「自动修复」。

// expiryNotice 一条到期记录。
type expiryNotice struct {
	AppName  string    `json:"app_name"`
	Bundle   string    `json:"bundle"`
	Kind     string    `json:"kind"` // 证书 / Profile
	DaysLeft int       `json:"days_left"`
	ExpireAt time.Time `json:"expire_at"`
	Hint     string    `json:"hint"`
}

// expiryMailState 按自然日去重。
//
// 检查跟着 10 分钟一次的 ticker 跑。不按天去重的话，一个快到期的应用
// 一天能发 144 封邮件，用户会直接把发件人拉黑。
//
// 存内存不落盘：重启后当天可能多一封，比为了这点细节引入持久化划算。
type expiryMailState struct {
	mu          sync.Mutex
	lastSentDay string
}

// collectExpiryNotices 扫描所有应用，挑出剩余天数 <= 阈值的 cert/profile。
//
// 即使 HAP 不存在也要报：证书到期后下次上传的新包会签不出来。
func collectExpiryNotices(apps []*HmosApp, warnDays int, uploadRoot string) []expiryNotice {
	now := time.Now()
	var out []expiryNotice

	for _, app := range apps {
		if app == nil {
			continue
		}
		certPath, profPath, _, _ := hmosAppPaths(uploadRoot, app.BundleName)
		info := collectCertInfo(hmosCertFile{
			CertFile:    certPath,
			ProfileFile: profPath,
		}, warnDays)

		name := strings.TrimSpace(app.Name)
		if name == "" {
			name = app.BundleName
		}

		add := func(kind string, unix int64, hint string) {
			if unix <= 0 {
				return // 文件缺失或解析失败，不算「到期」
			}
			deadline := time.Unix(unix, 0)
			days := int(deadline.Sub(now).Hours() / 24)
			if days > warnDays {
				return
			}
			out = append(out, expiryNotice{
				AppName:  name,
				Bundle:   app.BundleName,
				Kind:     kind,
				DaysLeft: days,
				ExpireAt: deadline,
				Hint:     hint,
			})
		}

		add("证书", info.CertNotAfter,
			"到 AGC 申请新证书。生成 CSR 时务必选「已有密钥」指向当前 p12，不要新建密钥库")
		add("Profile", info.ProfileNotAfter,
			"到 AGC 重新申请 Profile（类型选「指定设备发布」），下载后上传到 CICD 并重签")
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].DaysLeft != out[j].DaysLeft {
			return out[i].DaysLeft < out[j].DaysLeft
		}
		return out[i].Bundle < out[j].Bundle
	})
	return out
}

// buildExpiryMail 生成邮件主题与正文。
func buildExpiryMail(notices []expiryNotice, now time.Time) (subject, body string) {
	expired := 0
	for _, n := range notices {
		if n.DaysLeft < 0 {
			expired++
		}
	}

	if expired > 0 {
		subject = fmt.Sprintf("【CICD】%d 项签名材料已过期，需立即处理", expired)
	} else {
		subject = fmt.Sprintf("【CICD】鸿蒙签名材料即将到期（最快 %d 天）", notices[0].DaysLeft)
	}

	var b strings.Builder
	b.WriteString(`<div style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;`)
	b.WriteString(`max-width:640px;margin:0 auto;padding:24px;color:#1f2937">`)
	b.WriteString(`<h2 style="margin:0 0 6px;font-size:18px">鸿蒙签名材料到期提醒</h2>`)
	b.WriteString(`<p style="margin:0 0 18px;color:#9ca3af;font-size:12px">`)
	b.WriteString(now.Format("2006-01-02 15:04"))
	b.WriteString(` · CICD 控制台自动检查</p>`)

	b.WriteString(`<table style="width:100%;border-collapse:collapse;font-size:13px">`)
	b.WriteString(`<thead><tr style="text-align:left;color:#6b7280">`)
	for _, th := range []string{"应用", "类型", "剩余", "到期日"} {
		fmt.Fprintf(&b, `<th style="padding:8px 6px;border-bottom:1px solid #e5e7eb">%s</th>`, th)
	}
	b.WriteString(`</tr></thead><tbody>`)

	for _, n := range notices {
		color, days := "#6b7280", fmt.Sprintf("%d 天", n.DaysLeft)
		switch {
		case n.DaysLeft < 0:
			color = "#dc2626"
			days = fmt.Sprintf("已过期 %d 天", -n.DaysLeft)
		case n.DaysLeft <= 7:
			color = "#d97706"
		}
		fmt.Fprintf(&b, `<tr>`+
			`<td style="padding:8px 6px;border-bottom:1px solid #f3f4f6">%s`+
			`<br><span style="color:#9ca3af;font-size:11px">%s</span></td>`+
			`<td style="padding:8px 6px;border-bottom:1px solid #f3f4f6">%s</td>`+
			`<td style="padding:8px 6px;border-bottom:1px solid #f3f4f6;color:%s;font-weight:600">%s</td>`+
			`<td style="padding:8px 6px;border-bottom:1px solid #f3f4f6;color:#6b7280">%s</td>`+
			`</tr>`,
			h(n.AppName), h(n.Bundle), h(n.Kind), color, h(days),
			n.ExpireAt.Format("2006-01-02"))
	}
	b.WriteString(`</tbody></table>`)

	// 处理建议按类型去重，避免同一句话重复出现
	b.WriteString(`<div style="margin-top:20px;padding:14px 16px;background:#f9fafb;border-radius:8px;font-size:13px;color:#374151">`)
	b.WriteString(`<strong style="display:block;margin-bottom:8px">怎么处理</strong>`)
	seen := map[string]bool{}
	for _, n := range notices {
		if seen[n.Kind] {
			continue
		}
		seen[n.Kind] = true
		fmt.Fprintf(&b, `<div style="margin-bottom:6px"><strong>%s</strong>：%s</div>`, h(n.Kind), h(n.Hint))
	}
	b.WriteString(`</div>`)
	b.WriteString(`<p style="margin:18px 0 0;font-size:12px;color:#9ca3af">`)
	b.WriteString(`这封邮件每天最多发一次，处理完后提醒会自动停止。</p></div>`)
	return subject, b.String()
}

// checkAndNotifyExpiry 检查到期并在需要时发邮件。
// 返回实际发送成功的邮件数（0 或 1），便于测试断言。
func (s *server) checkAndNotifyExpiry() int {
	if s.mail == nil || !s.mail.enabled() {
		return 0 // 没配网关：静默跳过，不当错误
	}
	notices := collectExpiryNotices(s.hmos.List(), s.cfg.HmosWarnDays, s.cfg.HmosUploadDir)
	if len(notices) == 0 {
		return 0
	}

	today := time.Now().Format("2006-01-02")
	s.expiryMail.mu.Lock()
	if s.expiryMail.lastSentDay == today {
		s.expiryMail.mu.Unlock()
		return 0
	}
	s.expiryMail.mu.Unlock()

	subject, body := buildExpiryMail(notices, time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.mail.send(ctx, subject, body); err != nil {
		// 失败不标记「今天已发」，让下一轮 ticker 重试
		log.Printf("[hmos] 到期提醒邮件发送失败: %v", err)
		return 0
	}

	s.expiryMail.mu.Lock()
	s.expiryMail.lastSentDay = today
	s.expiryMail.mu.Unlock()
	log.Printf("[hmos] 到期提醒邮件已发送（%d 个应用，共 %d 项）",
		countNoticeApps(notices), len(notices))
	return 1
}

// h 转义用户可控内容（包名、应用名），防止 <> 破坏邮件 HTML 结构。
func h(s string) string { return html.EscapeString(s) }

func countNoticeApps(notices []expiryNotice) int {
	seen := map[string]bool{}
	for _, n := range notices {
		seen[n.Bundle] = true
	}
	return len(seen)
}
