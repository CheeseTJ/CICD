package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// 邮件通知：通过「内部邮件网关」发信，而不是在每个服务里直连 SMTP。
//
// 为什么走网关
// -----------
// 之前 CICD 里写了一份 SMTP 直连实现（照搬 Remind-api 的 mailer）。问题是
// 那样每加一个需要发信的服务，就要重复一遍：配置、465/587 兼容、AUTH
// PLAIN/LOGIN 降级、重试、MIME 拼装。账号密码也会散落在各自的部署配置里。
//
// 改成「所有服务调同一个 HTTP 接口」后：
//   - SMTP 凭据只在一处（网关）
//   - 换邮箱服务商只改网关
//   - 各服务只关心「发一封邮件」这一件事
//
// 接口契约（刻意做成极简的 JSON POST）
//
//	POST {MAIL_API_URL}
//	Authorization: Bearer {MAIL_API_TOKEN}   // 可选
//	{"to":["a@b.com"],"subject":"...","html":"..."}
//	→ 2xx 视为成功；非 2xx 或网络错误视为失败
//
// 配置缺失时静默跳过：没配网关就不发信，而不是报错。这样本地开发和
// 不需要通知的部署都不会被阻断。

// mailConfig 内部邮件网关的连接配置。
type mailConfig struct {
	// URL 网关地址。为空表示未配置，所有发送调用直接跳过。
	URL string
	// Token Bearer 认证令牌，为空则不带 Authorization 头。
	Token string
	// FromName 发件人显示名。为空则由网关用默认署名。
	//
	// 网关被多个系统共用，不指定的话所有邮件都署「岁记提醒」，
	// 收件人分不清这封是备忘还是证书提醒。
	FromName string
	// To 收件人列表。
	To []string
	// Timeout 单次请求超时，默认 15s。
	Timeout time.Duration
}

// enabled 是否配置完整到可以发信。
func (c mailConfig) enabled() bool {
	return strings.TrimSpace(c.URL) != "" && len(c.To) > 0
}

// mailClient 邮件网关客户端。
type mailClient struct {
	cfg  mailConfig
	http *http.Client
	// post 便于测试注入
	post func(ctx context.Context, url, token string, body []byte) error
}

func newMailClient(cfg mailConfig) *mailClient {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	c := &mailClient{cfg: cfg, http: &http.Client{Timeout: timeout}}
	c.post = c.doPost
	return c
}

// enabled 未配置网关时为 false —— 调用方据此跳过，不当作错误。
func (c *mailClient) enabled() bool {
	return c != nil && c.cfg.enabled()
}

// send 发一封 HTML 邮件给配置里的所有收件人。
//
// 一次请求带全部收件人：网关侧可以做成单封多收件人，减少往返。
func (c *mailClient) send(ctx context.Context, subject, html string) error {
	if !c.enabled() {
		return nil
	}
	payload := struct {
		To       []string `json:"to"`
		Subject  string   `json:"subject"`
		HTML     string   `json:"html"`
		FromName string   `json:"from_name,omitempty"`
	}{To: c.cfg.To, Subject: subject, HTML: html, FromName: c.cfg.FromName}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化邮件请求失败: %w", err)
	}
	return c.post(ctx, c.cfg.URL, c.cfg.Token, body)
}

// doPost 真正发 HTTP 请求。
func (c *mailClient) doPost(ctx context.Context, url, token string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造邮件请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("调用邮件网关失败: %w", err)
	}
	defer resp.Body.Close()

	// 网关可能返回较长的错误说明，截断后回传，避免把日志刷爆
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = resp.Status
		}
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return fmt.Errorf("邮件网关返回 %d: %s", resp.StatusCode, msg)
	}
	return nil
}

// testMailConfig 发一封测试邮件，供「设置」页验证配置。
//
// 错误信息要能直接告诉用户哪一步错了：未配置 / 网络不通 / 网关拒绝。
// 这类配置错误排查成本高，所以宁可把原因写细一点。
func testMailConfig(ctx context.Context, cfg mailConfig) error {
	if strings.TrimSpace(cfg.URL) == "" {
		return errors.New("未配置邮件网关地址（MAIL_API_URL）")
	}
	if len(cfg.To) == 0 {
		return errors.New("未配置收件人（MAIL_TO）")
	}
	c := newMailClient(cfg)
	body := `<div style="font-family:-apple-system,sans-serif;padding:16px">` +
		`<p style="font-size:14px;color:#374151">这是一封来自 CICD 控制台的测试邮件，` +
		`收到即说明邮件网关配置正常。</p>` +
		`<p style="font-size:12px;color:#9ca3af">发送时间：` +
		time.Now().Format("2006-01-02 15:04:05") + `</p></div>`
	if err := c.send(ctx, "【CICD】测试邮件", body); err != nil {
		return err
	}
	log.Printf("[mail] 测试邮件已发送")
	return nil
}
