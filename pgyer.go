package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
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
	form := url.Values{}
	form.Set("_api_key", p.apiKey)
	form.Set("buildType", "android")
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
