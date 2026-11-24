package main

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// 证书与 keystore 的配对校验。
//
// 为什么需要这个：AGC 签发证书时只签 CSR 里带的那把公钥，而私钥始终留在
// 你本地的 .p12 里。如果申请证书时误点了「新建密钥库」而不是「选择已有」，
// AGC 会正常签发一张看起来完全没问题的 .cer —— 但它和你的 p12 不是一对。
//
// 这个错误在整个链路上都不会提前暴露：
//   - AGC 控制台显示证书「生效」，没有任何异常
//   - 上传到本控制台也一切正常，能解析出有效期
//   - 直到重签 HAP 才失败，而 hapsigntool 的报错只说「签名验证失败」
//
// 用户此时根本不知道该改什么。所以在上传证书的那一刻就比对公钥，
// 不匹配立刻拒绝，并明确告诉他是「证书和 keystore 不是一对」。
//
// 实现上复用 JRE 自带的 keytool —— 已经为了 hapsigntool 装了 JRE，
// 不额外引依赖。注意 keytool 输出的是「该别名对应的证书」，它的公钥
// 就等于私钥的公钥，所以比对公钥即可。

// keyMatchResult 证书与 keystore 的公钥比对结果。
type keyMatchResult struct {
	// Status ok / mismatch / unknown
	Status string `json:"status"`
	// Detail 给用户看的原因，ok 时为空
	Detail string `json:"detail,omitempty"`
}

const (
	KeyMatchOK       = "ok"
	KeyMatchMismatch = "mismatch"
	KeyMatchUnknown  = "unknown"
)

// keyMatchCache 缓存校验结果。
//
// 比对要 fork 一个 JVM，单次约 200-400ms。而 buildHmosView 在每次列表
// 请求时都会跑，不缓存的话页面会明显变慢。缓存键带上两个文件的 mtime+size，
// 任一文件被替换就自动失效 —— 不需要在上传逻辑里手动清缓存。
var keyMatchCache sync.Map // map[string]keyMatchResult

// keystoreCertCache 缓存从 p12 读出的公钥（DER），避免重复起 JVM。
var keystoreCertCache sync.Map // map[string][]byte

// verifyKeyPair 校验证书文件里的叶子公钥，是否与 keystore 中 keyAlias 的
// 私钥配对。
//
// 任一环节缺信息（密码没配、别名为空、keytool 不在）都返回 unknown，
// 而不是报错 —— 控制台不能因为「运维没配密码」就整页打不开。
func verifyKeyPair(certPath, keystorePath, password, alias, javaHome string) keyMatchResult {
	if certPath == "" || keystorePath == "" {
		return keyMatchResult{Status: KeyMatchUnknown, Detail: "缺少证书或 keystore 文件"}
	}
	if password == "" {
		return keyMatchResult{Status: KeyMatchUnknown, Detail: "未配置 keystore 密码，无法校验"}
	}
	if alias == "" {
		return keyMatchResult{Status: KeyMatchUnknown, Detail: "未配置密钥别名，无法校验"}
	}

	// 用文件状态做缓存键：内容一变键就变，天然失效
	ck, ok1 := fileFingerprint(certPath)
	kk, ok2 := fileFingerprint(keystorePath)
	if !ok1 || !ok2 {
		return keyMatchResult{Status: KeyMatchUnknown, Detail: "文件不存在，无法校验"}
	}
	fullKey := ck + "|" + kk + "|" + alias
	if v, hit := keyMatchCache.Load(fullKey); hit {
		return v.(keyMatchResult)
	}

	certPub, err := leafPublicKey(certPath)
	if err != nil {
		return keyMatchResult{Status: KeyMatchUnknown, Detail: "读取证书公钥失败: " + err.Error()}
	}

	storePub, err := keystorePublicKey(keystorePath, password, alias, javaHome, kk)
	if err != nil {
		// keytool 失败（密码错、别名不存在）本身就很值得告诉用户，
		// 但归到 unknown 而不是 mismatch —— 我们没拿到公钥，不能说「不匹配」
		return keyMatchResult{Status: KeyMatchUnknown, Detail: "读取 keystore 公钥失败: " + err.Error()}
	}

	r := keyMatchResult{Status: KeyMatchOK}
	if !bytes.Equal(certPub, storePub) {
		r = keyMatchResult{
			Status: KeyMatchMismatch,
			Detail: "证书与 keystore 不是一对：证书公钥和 key.p12 里别名 " + alias +
				" 的私钥不匹配。申请证书时的 CSR 必须由这个 p12 生成，" +
				"否则重签会失败。",
		}
	}
	keyMatchCache.Store(fullKey, r)
	return r
}

// fileFingerprint 生成「路径+大小+修改时间」的指纹，用作缓存键。
func fileFingerprint(path string) (string, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%s:%d:%d", path, st.Size(), st.ModTime().UnixNano()), true
}

// leafPublicKey 从证书文件里取叶子证书的公钥，返回 PKIX DER。
//
// 「叶子」判定：AGC 导出的链是「叶子 → 中间 → 根」，叶子 subject 与
// issuer 不同且不是自签名。这里取第一个非自签名证书；如果全部是
// 自签名（单张证书文件很常见），就用第一张。
func leafPublicKey(certPath string) ([]byte, error) {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	certs, err := parseCerts(data)
	if err != nil {
		return nil, err
	}
	if len(certs) == 0 {
		return nil, errors.New("证书文件里没有证书")
	}
	leaf := certs[0]
	for _, c := range certs {
		if c.Subject.String() != c.Issuer.String() {
			leaf = c
			break
		}
	}
	der, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("序列化公钥失败: %w", err)
	}
	return der, nil
}

// keystorePublicKey 用 keytool 读出 p12 里指定别名的证书公钥。
//
// keytool -list -rfc 输出该别名的完整证书链（PEM），第一段就是与私钥
// 配对的叶子证书。这里只需要第一段的公钥。
func keystorePublicKey(keystorePath, password, alias, javaHome, cacheKey string) ([]byte, error) {
	short := keystorePath + "|" + alias + "|" + cacheKey
	if v, hit := keystoreCertCache.Load(short); hit {
		return v.([]byte), nil
	}

	cmd := keytoolCmd(javaHome)
	// 只读操作；-rfc 让输出变成 PEM，避免解析 keytool 的表格格式
	args := []string{
		"-list", "-rfc",
		"-keystore", keystorePath,
		"-storepass", password,
		"-alias", alias,
	}
	cmd.Args = append(cmd.Args, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, keytoolError(err, out)
	}

	// 从输出里挑第一段 CERTIFICATE
	rest := out
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		der, err := x509.MarshalPKIXPublicKey(c.PublicKey)
		if err != nil {
			return nil, err
		}
		keystoreCertCache.Store(short, der)
		return der, nil
	}
	return nil, errors.New("keytool 输出里没有可解析的证书")
}

// keytoolCmd 找到可用的 keytool 命令。
//
// 优先用 HMOS_JAVA（它指向容器内的 java 路径，同一目录下就有 keytool），
// 其次靠 PATH。容器里 java 和 keytool 都在同一个 bin 目录，所以从 java
// 路径反推 keytool 是最稳的 —— 容器镜像里没有 JAVA_HOME。
func keytoolCmd(javaHome string) *exec.Cmd {
	if javaHome != "" {
		dir := filepath.Dir(javaHome)
		cand := filepath.Join(dir, "keytool")
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return exec.Command(cand)
		}
	}
	return exec.Command("keytool")
}

// keytoolError 把 keytool 的报错翻译成人能懂的原因。
//
// keytool 的错误输出格式极不友好（大部分是一大段 help 文本），直接回显
// 会把前端淹没。这里只认几个高频原因，其余截断成一行。
func keytoolError(err error, out []byte) error {
	s := string(out)
	switch {
	case strings.Contains(s, "password was incorrect"),
		strings.Contains(s, "keystore password was incorrect"):
		return errors.New("keystore 密码错误（请检查 HMOS_KEYSTORE_PWD）")
	case strings.Contains(s, "does not exist"):
		return errors.New("keystore 里没有这个别名（请检查密钥别名）")
	case strings.Contains(s, "Invalid keystore format"):
		return errors.New("不是有效的 p12/keystore 文件")
	}
	line := strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if line == "" {
		line = err.Error()
	}
	if len(line) > 160 {
		line = line[:160] + "…"
	}
	return errors.New(line)
}
