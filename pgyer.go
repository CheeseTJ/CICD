package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type pgyerClient struct {
	http   *http.Client
	apiKey string
}

type cosToken struct {
	Endpoint string `json:"endpoint"`
	Key      string `json:"key"`
	Params   struct {
		Signature         string `json:"signature"`
		XCOSSecurityToken string `json:"x-cos-security-token"`
		Key               string `json:"key"`
	} `json:"params"`
}

type buildInfoData struct {
	BuildKey         string `json:"buildKey"`
	BuildType        string `json:"buildType"`
	BuildName        string `json:"buildName"`
	BuildVersion     string `json:"buildVersion"`
	BuildVersionNo   string `json:"buildVersionNo"`
	BuildIdentifier  string `json:"buildIdentifier"`
	BuildShortcutUrl string `json:"buildShortcutUrl"`
	BuildQRCodeURL   string `json:"buildQRCodeURL"`
}

type pgyerApp struct {
	BuildKey         string `json:"buildKey"`
	AppKey           string `json:"appKey"`
	BuildName        string `json:"buildName"`
	BuildVersion     string `json:"buildVersion"`
	BuildVersionNo   string `json:"buildVersionNo"`
	BuildIdentifier  string `json:"buildIdentifier"`
	BuildShortcutUrl string `json:"buildShortcutUrl"`
}

func (p *pgyerClient) postForm(ctx context.Context, u string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("pgyer api %s: %s: %s", u, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (p *pgyerClient) getCOSToken(ctx context.Context) (*cosToken, error) {
	return p.getCOSTokenFor(ctx, "android")
}

// getCOSTokenFor 按 buildType 申请上传凭证。
//
// 蒲公英按 buildType 走不同的上传管线：android 收 APK，harmonyos 收 HAP。
// 混用会被服务端拒绝或生成无法安装的条目，所以类型必须显式传，不能默认。
func (p *pgyerClient) getCOSTokenFor(ctx context.Context, buildType string) (*cosToken, error) {
	form := url.Values{}
	form.Set("_api_key", p.apiKey)
	form.Set("buildType", buildType)
	body, err := p.postForm(ctx, "https://www.pgyer.com/apiv2/app/getCOSToken", form)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Code    int      `json:"code"`
		Message string   `json:"message"`
		Data    cosToken `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("getCOSToken 失败: code=%d message=%s", resp.Code, resp.Message)
	}
	return &resp.Data, nil
}

// countingReader 统计读取字节数，用于上传进度。
type countingReader struct {
	r     io.Reader
	n     int64
	total int64
	cb    func(done, total int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.cb != nil {
		c.cb(c.n, c.total)
	}
	return n, err
}

// uploadFile 将本地文件以 multipart/form-data 方式上传到 COS 预上传地址。
// 请求体先落到临时文件，以便携带明确的 Content-Length：部分对象存储会拒绝
// chunked 编码的 POST，返回 MalformedPOSTRequest。
func (p *pgyerClient) uploadFile(ctx context.Context, tok *cosToken, filePath, fileName string, progress func(done, total int64)) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	fileSize := stat.Size()

	body, err := os.CreateTemp("", "pgyer-body-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(body.Name())
	defer body.Close()

	mw := multipart.NewWriter(body)
	writeField := func(k, v string) error { return mw.WriteField(k, v) }
	for _, kv := range [][2]string{
		{"key", tok.Key},
		{"signature", tok.Params.Signature},
		{"x-cos-security-token", tok.Params.XCOSSecurityToken},
		{"x-cos-meta-file-name", fileName},
	} {
		if err := writeField(kv[0], kv[1]); err != nil {
			return err
		}
	}
	fw, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	bodySize, err := body.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// 用 countingReader 包住请求体，回调反映的是真实发送进度。
	br := &countingReader{r: body, total: bodySize, cb: nil}
	if progress != nil {
		br.cb = func(done, _ int64) {
			// 实际发送字节包含 multipart 头部，按文件大小换算为 0~1。
			ratio := float64(done) / float64(bodySize)
			sent := int64(ratio * float64(fileSize))
			if sent > fileSize {
				sent = fileSize
			}
			progress(sent, fileSize)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tok.Endpoint, br)
	if err != nil {
		return err
	}
	req.ContentLength = bodySize
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("上传文件失败: %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	return nil
}

type buildInfoRaw struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    buildInfoData `json:"data"`
}

func (p *pgyerClient) buildInfo(ctx context.Context, buildKey string) (*buildInfoRaw, error) {
	u := "https://www.pgyer.com/apiv2/app/buildInfo?" + url.Values{
		"_api_key": {p.apiKey},
		"buildKey": {buildKey},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("buildInfo: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var raw buildInfoRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	return &raw, nil
}

// pollBuildInfo 轮询发布结果，直到成功或失败。
func (p *pgyerClient) pollBuildInfo(ctx context.Context, buildKey string, timeout time.Duration) (*buildInfoData, error) {
	deadline := time.Now().Add(timeout)
	for {
		raw, err := p.buildInfo(ctx, buildKey)
		if err != nil {
			return nil, err
		}
		switch raw.Code {
		case 0:
			return &raw.Data, nil
		case 1247: // 正在发布
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("发布超时")
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(4 * time.Second):
			}
		case 1216:
			return nil, fmt.Errorf("应用发布失败 (code=1216)")
		default:
			return nil, fmt.Errorf("buildInfo 异常: code=%d message=%s", raw.Code, raw.Message)
		}
	}
}

// listMy 获取当前账户在蒲公英上的应用列表。
func (p *pgyerClient) listMy(ctx context.Context) ([]pgyerApp, error) {
	form := url.Values{"_api_key": {p.apiKey}}
	body, err := p.postForm(ctx, "https://www.pgyer.com/apiv2/app/listMy", form)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			List []pgyerApp `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("listMy 失败: code=%d message=%s", resp.Code, resp.Message)
	}
	return resp.Data.List, nil
}

// uploadHarmonyCert 把 p12 私钥上传给蒲公英，供它签 HAP 的安装清单。
//
// 为什么必须传这一步
// ------------------
// 蒲公英的 HarmonyOS 分发是「三段签名」模型：
//
//	.hap 本体      → 我们本地用 AGC 证书签（resignHap 做的）
//	.p7b Profile   → AGC 下载，我们随包提供
//	manifest.json5 → **蒲公英生成**，并由这里上传的 p12 签
//
// 缺了 p12，蒲公英能收下 HAP、也能生成下载页，但页面会挂一条红色警告
// 「当前版本使用调试 Profile 签名，测试者无法通过蒲公英安装」—— 包根本装不上。
//
// 硬性约束（蒲公英要求，违反则测试者仍装不上）
//   - p12 必须是 AGC 的**发布证书**，不能是调试证书
//   - p7b 必须是「指定设备发布」类型，且含测试设备 UDID
//   - p12 必须与签 .hap 用的是同一把私钥
//
// 接口与错误码（实测自项目里的 pgyer_upload.ps1）
//
//	POST https://upload.pgyer.com/apiv2/app/uploadHarmonyCert
//	fields: _api_key / file / password / buildKey / keyAlias
//	3006 p12 未生效或已过期  3008 密码错误或不是有效 p12  3014 该版本没有签名信息
func (p *pgyerClient) uploadHarmonyCert(ctx context.Context, buildKey, p12Path, password, keyAlias string) error {
	if buildKey == "" {
		return errors.New("缺少 buildKey，无法上传证书")
	}
	if p12Path == "" {
		return errors.New("未配置 p12 路径")
	}

	f, err := os.Open(p12Path)
	if err != nil {
		return fmt.Errorf("打开 p12 失败: %w", err)
	}
	defer f.Close()

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)

	fields := [][2]string{
		{"_api_key", p.apiKey},
		{"password", password},
		{"buildKey", buildKey},
	}
	if keyAlias != "" {
		fields = append(fields, [2]string{"keyAlias", keyAlias})
	}
	for _, kv := range fields {
		if err := mw.WriteField(kv[0], kv[1]); err != nil {
			return err
		}
	}

	// 文件名用 p12 原名：蒲公英只按内容解析，名字仅供人工排查
	part, err := mw.CreateFormFile("file", filepath.Base(p12Path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://upload.pgyer.com/apiv2/app/uploadHarmonyCert", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("上传 p12 请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}

	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	// 服务端可能返回非 JSON（网关错误页），此时按状态码报错
	if err := json.Unmarshal(raw, &out); err != nil {
		if resp.StatusCode >= 400 {
			return fmt.Errorf("上传 p12 失败: %s: %s", resp.Status,
				strings.TrimSpace(string(raw)))
		}
		return fmt.Errorf("上传 p12 返回无法解析: %s", strings.TrimSpace(string(raw)))
	}

	if out.Code != 0 {
		hint := map[int]string{
			3006: "（p12 尚未生效或已过期）",
			3008: "（密码错误或不是有效 p12）",
			3014: "（该版本没有签名信息）",
		}[out.Code]
		return fmt.Errorf("上传 p12 失败: code=%d message=%s%s",
			out.Code, out.Message, hint)
	}
	return nil
}
