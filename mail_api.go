package main

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// 邮件通知的配置状态与测试入口。
//
// 为什么需要这个接口
// ----------------
// 邮件网关的配置错误（URL 打错、令牌过期、收件人为空）在正常运行中
// **完全静默** —— checkAndNotifyExpiry 失败只打日志，而日志要等到有
// 证书快过期时才出现，可能是半年后。那时才发现配错了就太晚了。
//
// 所以给一个「立刻发一封测试邮件」的入口，让配完当场就能验证。

// handleMailStatus GET /api/mail/status
//
// 返回配置是否完整、收件人列表、以及当前有多少项即将到期。
// 只暴露「有没有配」和收件人，不返回 URL 和令牌 —— 那两项属于凭据。
func (s *server) handleMailStatus(w http.ResponseWriter, r *http.Request) {
	expiring := collectExpiryNotices(s.hmos.List(), s.cfg.HmosWarnDays, s.cfg.HmosUploadDir)

	resp := struct {
		Configured bool `json:"configured"`
		// Recipients 收件人。前端要显示「将发送到 xxx」
		Recipients []string `json:"recipients"`
		// HasToken 是否配了 Bearer 令牌（不返回令牌本身）
		HasToken bool `json:"has_token"`
		// WarnDays 阈值
		WarnDays int `json:"warn_days"`
		// ExpiringCount 当前需要提醒的项目数
		ExpiringCount int `json:"expiring_count"`
		// Expiring 明细，方便前端直接展示「现在就会发哪些」
		Expiring []expiryNotice `json:"expiring"`
	}{
		Configured:    s.mail != nil && s.mail.enabled(),
		Recipients:    s.cfg.MailTo,
		HasToken:      s.cfg.MailAPIToken != "",
		WarnDays:      s.cfg.HmosWarnDays,
		ExpiringCount: len(expiring),
		Expiring:      expiring,
	}
	if resp.Recipients == nil {
		resp.Recipients = []string{}
	}
	if resp.Expiring == nil {
		resp.Expiring = []expiryNotice{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMailTest POST /api/mail/test
//
// 立刻发一封测试邮件。配置不全时返回明确的错误原因，
// 而不是静默成功 —— 这个接口的存在意义就是暴露配置问题。
func (s *server) handleMailTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("只支持 POST"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cfg := mailConfig{
		URL:   s.cfg.MailAPIURL,
		Token: s.cfg.MailAPIToken,
		To:    s.cfg.MailTo,
	}
	if err := testMailConfig(ctx, cfg); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"recipients": s.cfg.MailTo,
	})
}

// handleMailPreview GET /api/mail/preview
//
// 返回「下一封提醒邮件」的主题与 HTML，供前端预览。
//
// 用途：证书还没到期时，用户无法验证邮件长什么样。这个接口用真实数据
// 渲染一份（没有到期项时用占位数据），让他能先看看效果再决定要不要启用。
func (s *server) handleMailPreview(w http.ResponseWriter, r *http.Request) {
	notices := collectExpiryNotices(s.hmos.List(), s.cfg.HmosWarnDays, s.cfg.HmosUploadDir)

	if len(notices) == 0 {
		// 没有到期项时造一份示例，让用户能看到真实排版
		now := time.Now()
		notices = []expiryNotice{
			{
				AppName:  "示例应用",
				Bundle:   "com.example.app",
				Kind:     "Profile",
				DaysLeft: 12,
				ExpireAt: now.AddDate(0, 0, 12),
				Hint:     "到 AGC 重新申请 Profile（类型选「指定设备发布」），下载后上传到 CICD 并重签",
			},
			{
				AppName:  "示例应用",
				Bundle:   "com.example.app",
				Kind:     "证书",
				DaysLeft: 26,
				ExpireAt: now.AddDate(0, 0, 26),
				Hint:     "到 AGC 申请新证书。生成 CSR 时务必选「已有密钥」指向当前 p12",
			},
		}
	}

	subject, body := buildExpiryMail(notices, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{
		"subject": subject,
		"html":    body,
		// Sample 为 true 表示当前没有真实到期项，内容为示例
		"sample": len(notices) > 0 && s.isSampleNotices(notices),
	})
}

// isSampleNotices 判断是否用了占位数据（包名以 com.example 开头）。
func (s *server) isSampleNotices(notices []expiryNotice) bool {
	for _, n := range notices {
		if n.Bundle == "com.example.app" {
			return true
		}
	}
	return false
}
