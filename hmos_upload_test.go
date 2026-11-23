package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newUploadServer 造一个只用于上传测试的 server：禁鉴权、临时目录。
func newUploadServer(t *testing.T) (*server, string) {
	t.Helper()
	dir := t.TempDir()
	s := &server{
		cfg: &config{
			HmosUploadDir: dir,
			HmosWarnDays:  30,
			DataFile:      filepath.Join(dir, "data.json"),
		},
	}
	return s, dir
}

// buildUploadReq 构造 multipart 请求。
func buildUploadReq(t *testing.T, bundle, kind, filename string, content []byte) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("bundle_name", bundle)
	_ = mw.WriteField("kind", kind)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("写内容: %v", err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/hmos/upload-file", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestUploadWritesFile(t *testing.T) {
	s, dir := newUploadServer(t)
	rec := httptest.NewRecorder()
	s.handleHmosUploadFile(rec, buildUploadReq(t, "com.junet.music", "cert", "cert.cer", []byte("PEMDATA")))

	if rec.Code != http.StatusOK {
		t.Fatalf("应成功，got %d: %s", rec.Code, rec.Body.String())
	}
	// 落盘名固定为 cert.<ext>，与原始文件名无关
	got := filepath.Join(dir, "com.junet.music", "cert.cer")
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("文件未落盘: %v", err)
	}
	if string(data) != "PEMDATA" {
		t.Errorf("内容不符: %q", data)
	}
}

// 包名必须挡掉路径穿越。
func TestUploadRejectsPathTraversal(t *testing.T) {
	s, _ := newUploadServer(t)
	for _, bad := range []string{"../evil", "a/b", "", "..", "a b"} {
		rec := httptest.NewRecorder()
		s.handleHmosUploadFile(rec, buildUploadReq(t, bad, "cert", "c.cer", []byte("x")))
		if rec.Code == http.StatusOK {
			t.Errorf("包名 %q 应被拒绝", bad)
		}
	}
}

// 后缀必须与类型匹配：把 p12 当 cert 传应被拒绝。
func TestUploadRejectsWrongExt(t *testing.T) {
	s, _ := newUploadServer(t)
	rec := httptest.NewRecorder()
	s.handleHmosUploadFile(rec, buildUploadReq(t, "com.a.b", "cert", "key.p12", []byte("x")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("错误后缀应返回 400，got %d", rec.Code)
	}
}

// 未知 kind 应被拒绝。
func TestUploadRejectsUnknownKind(t *testing.T) {
	s, _ := newUploadServer(t)
	rec := httptest.NewRecorder()
	s.handleHmosUploadFile(rec, buildUploadReq(t, "com.a.b", "exe", "x.exe", []byte("x")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未知 kind 应返回 400，got %d", rec.Code)
	}
}

// 空文件应被拒绝（多半是用户选错了）。
func TestUploadRejectsEmpty(t *testing.T) {
	s, _ := newUploadServer(t)
	rec := httptest.NewRecorder()
	s.handleHmosUploadFile(rec, buildUploadReq(t, "com.a.b", "cert", "c.cer", nil))
	if rec.Code == http.StatusOK {
		t.Error("空文件应被拒绝")
	}
}

// 同类型再上传时，应删掉旧的（换扩展名场景）。
func TestUploadReplacesOldExtension(t *testing.T) {
	s, dir := newUploadServer(t)

	rec := httptest.NewRecorder()
	s.handleHmosUploadFile(rec, buildUploadReq(t, "com.a.b", "cert", "old.cer", []byte("OLD")))
	if rec.Code != http.StatusOK {
		t.Fatalf("第一次上传失败: %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	s.handleHmosUploadFile(rec2, buildUploadReq(t, "com.a.b", "cert", "new.pem", []byte("NEW")))
	if rec2.Code != http.StatusOK {
		t.Fatalf("第二次上传失败: %d", rec2.Code)
	}

	target := filepath.Join(dir, "com.a.b")
	if _, err := os.Stat(filepath.Join(target, "cert.cer")); err == nil {
		t.Error("旧的 cert.cer 应被清理")
	}
	if _, err := os.Stat(filepath.Join(target, "cert.pem")); err != nil {
		t.Error("新的 cert.pem 应存在")
	}
}

// 上传真实证书时应返回解析出的到期信息。
func TestUploadReturnsCertInfo(t *testing.T) {
	s, _ := newUploadServer(t)
	certPath := writePEMCert(t, t.TempDir(), "c.pem", time.Now().Add(100*24*time.Hour))
	certBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.handleHmosUploadFile(rec, buildUploadReq(t, "com.a.b", "cert", "c.pem", certBytes))
	if rec.Code != http.StatusOK {
		t.Fatalf("上传失败: %s", rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"days_left":99`) && !strings.Contains(body, `"days_left":100`) {
		t.Errorf("应返回剩余天数，got: %s", body)
	}
	if !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("100 天应为 ok 状态，got: %s", body)
	}
}
