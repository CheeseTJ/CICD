// 鸿蒙签名材料的文件上传接口。
//
// 为什么需要这个
// --------------
// 证书、profile、keystore、HAP 这些文件原本在开发机的本地目录（Windows 上
// C:\Users\<你>\.ohos\config\），而控制台跑在 Linux 服务器上。没有上传接口时，
// 唯一的办法是 SSH/SCP 传文件再手填绝对路径 —— 每次证书续期都要来一遍，很容易
// 因为路径打错而失败。
//
// 这里允许在浏览器里直接选文件上传：服务端把内容写到 /app/data/hmos/<包名>/ 下，
// 返回容器内路径，前端自动填进表单。
//
// 安全考虑
// --------
// 1. 包名做白名单校验（只允许字母数字点下划线横线），拒绝路径穿越。
// 2. 按文件后缀决定落盘名，不信任客户端给的 filename。
// 3. 每个文件限制大小，避免把磁盘写满。
// 4. 覆盖写之前不备份 —— 这些文件本来就是从 AGC 重新下载的，旧的无价值。
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// 允许上传的文件类型 → 落盘时的固定文件名（不带扩展名，扩展名由类型决定）。
//
// 用固定名而不是原始文件名：一来避免同目录放一堆 cert(1).cer，
// 二来重签时代码里引用的是稳定路径，续期时上传覆盖即可，不用改配置。
var hmosUploadKinds = map[string]struct {
	FileName string
	MaxSize  int64
	Required bool
}{
	"cert":     {FileName: "cert", MaxSize: 1 << 20, Required: true},    // 1MB，证书链文本
	"profile":  {FileName: "profile", MaxSize: 1 << 20, Required: true}, // 1MB，p7b
	"keystore": {FileName: "key", MaxSize: 8 << 20, Required: true},     // 8MB，p12/jks
	"hap":      {FileName: "app", MaxSize: 512 << 20, Required: false},  // 512MB
}

// bundleNamePattern 包名白名单：鸿蒙 bundleName 是反向域名，实际只会用到
// 字母数字点下划线横线。拒绝 `/`、`..` 等路径穿越字符。
var bundleNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)

// validBundleName 在正则之外补充检查。
//
// 单靠上面的正则挡不住 ".." 和 "." —— 点号本身是合法包名字符（com.a.b），
// 但整个名字全是点就意味着路径穿越。这类名字虽然不像真实包名，但必须显式
// 拒绝，否则 filepath.Join(dir, "..") 会把文件写到父目录。
func validBundleName(name string) bool {
	if !bundleNamePattern.MatchString(name) {
		return false
	}
	// 全是点 → 路径穿越
	if strings.Trim(name, ".") == "" {
		return false
	}
	// 显式的 . / .. 段（理论上进不来，防御性判断）
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// handleHmosUploadFile POST /api/hmos/upload-file
//
// multipart/form-data：bundle_name + kind + file。
// 返回落盘路径（容器内），前端直接写进表单的对应字段。
func (s *server) handleHmosUploadFile(w http.ResponseWriter, r *http.Request) {
	// 512MB 的 hap 需要相应放宽内存上限。这里用 32MB 内存 + 其余落临时文件，
	// 避免大包把容器内存打满。
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("解析上传请求失败: %w", err))
		return
	}

	bundle := strings.TrimSpace(r.FormValue("bundle_name"))
	if !validBundleName(bundle) {
		writeErr(w, http.StatusBadRequest, errors.New("bundle_name 非法"))
		return
	}

	kind := strings.TrimSpace(r.FormValue("kind"))
	spec, ok := hmosUploadKinds[kind]
	if !ok {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("kind 必须是 cert / profile / keystore / hap 之一，收到 %q", kind))
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少 file 字段: %w", err))
		return
	}
	defer file.Close()

	if header.Size > spec.MaxSize {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("文件过大：%d 字节，上限 %d 字节", header.Size, spec.MaxSize))
		return
	}

	// 按上传内容的原始文件名决定扩展名，但文件主体名固定。
	// 保留扩展名是为了人肉排查时能看出是什么文件。
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext == "" {
		ext = defaultExtFor(kind)
	}
	if !allowedExtFor(kind, ext) {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("%s 不接受 %s 后缀（可用：%s）", kind, ext, allowedExtList(kind)))
		return
	}

	// 落盘目录：/app/data/hmos/<bundleName>/
	// 放在持久化卷里，容器重建后文件还在。
	dir := filepath.Join(s.cfg.HmosUploadDir, bundle)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("创建目录失败: %w", err))
		return
	}

	// 同类型的旧文件先删掉：换证书时可能从 .cer 变 .pem，
	// 留着旧的会让「用哪份」变得含糊。
	clearOldKind(dir, spec.FileName)

	dst := filepath.Join(dir, spec.FileName+ext)

	// 先写临时文件再 rename：上传中断时不会留下半截文件，
	// 而半截的 p12/cer 会让后续签名报难以理解的解密错误。
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("创建文件失败: %w", err))
		return
	}

	written, copyErr := io.Copy(out, io.LimitReader(file, spec.MaxSize+1))
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(tmp)
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("写入失败: %w", copyErr))
		return
	}
	if closeErr != nil {
		os.Remove(tmp)
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("关闭文件失败: %w", closeErr))
		return
	}
	if written > spec.MaxSize {
		os.Remove(tmp)
		writeErr(w, http.StatusBadRequest, errors.New("文件超出大小限制"))
		return
	}
	if written == 0 {
		os.Remove(tmp)
		writeErr(w, http.StatusBadRequest, errors.New("文件为空"))
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("保存失败: %w", err))
		return
	}

	// 上传完立刻解析一次，把到期时间直接返回 —— 用户不用再点保存就能确认
	// 传对了文件（尤其是 cert 和 profile 传反这种常见错误）。
	var info *certInfo
	switch kind {
	case "cert":
		info = collectCertInfo(hmosCertFile{CertFile: dst}, s.cfg.HmosWarnDays)
	case "profile":
		info = collectCertInfo(hmosCertFile{ProfileFile: dst}, s.cfg.HmosWarnDays)
	}

	// 上传完成后异步触发重签：换 Profile/证书的完整流程里，「重签」这一步
	// 没有任何决策 —— 材料齐了就必须做。忘了反而会让分发页上的包还是旧签名。
	//
	// 放在 writeJSON 之前调：autoResignAfterUpload 只排队不阻塞
	// （真正的重签在 goroutine 里），所以不会拖慢响应。
	s.autoResignAfterUpload(bundle, kind)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":     true,
		"path":   dst,
		"kind":   kind,
		"size":   written,
		"cert":   info,
		"bundle": bundle,
		// auto_resign 告诉前端「已排入自动重签队列」，它据此开始轮询状态。
		// 材料不齐时这里是 false（还缺文件，重签会失败，不触发）。
		"auto_resign": s.resignQ != nil && s.resignQ.status(bundle).State == resignStateRunning,
	})
}

// clearOldKind 删掉同主体名、不同扩展名的旧文件。
func clearOldKind(dir, base string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, base+".") && !strings.HasSuffix(name, ".part") {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

func defaultExtFor(kind string) string {
	switch kind {
	case "cert":
		return ".pem"
	case "profile":
		return ".p7b"
	case "keystore":
		return ".p12"
	case "hap":
		return ".hap"
	}
	return ""
}

func allowedExtFor(kind, ext string) bool {
	switch kind {
	case "cert":
		return ext == ".cer" || ext == ".pem" || ext == ".crt"
	case "profile":
		return ext == ".p7b" || ext == ".p7c"
	case "keystore":
		return ext == ".p12" || ext == ".jks" || ext == ".pfx"
	case "hap":
		return ext == ".hap"
	}
	return false
}

func allowedExtList(kind string) string {
	switch kind {
	case "cert":
		return ".cer .pem .crt"
	case "profile":
		return ".p7b .p7c"
	case "keystore":
		return ".p12 .jks .pfx"
	case "hap":
		return ".hap"
	}
	return ""
}
