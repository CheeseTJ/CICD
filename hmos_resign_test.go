package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 这些测试验证调 hap-sign-tool.jar 时的**参数拼装**。
//
// 为什么用假的 java 而不是真的签名工具：
//   真实签名要求 AGC 签发的 profile（p7b 是华为专有结构，openssl 造的不认），
//   本地无法复现。但参数名写错会导致「命令跑了但报 Missing parameter」这类
//   静默失败，必须在 CI 里挡住 —— 参数名全是试出来的，很容易改回错的。
//
// 假 java 的做法：写一个 shell 脚本，把收到的参数记进文件，再断言。
// 这比在 Go 里 mock exec.Command 更贴近真实（能验证参数分隔、顺序）。

// writeMockJava 生成一个记录参数的假 java，返回其路径与日志路径。
func writeMockJava(t *testing.T) (bin string, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "args.log")
	bin = filepath.Join(dir, "mockjava")

	// 脚本行为：
	//   sign-app   -> 复制 inFile 到 outFile（模拟追加签名块）
	//   verify-app -> 创建两个输出文件
	// 所有调用都把完整参数追加进日志。
	script := `#!/bin/sh
LOG="` + logPath + `"
echo "$*" >> "$LOG"

CMD=""
INPUT=""
OUTPUT=""
OUTCERT=""
OUTPROF=""
OUTPROOF=""
prev=""
for a in "$@"; do
  case "$a" in
    sign|sign-app|verify|verify-app) CMD="$a" ;;
    -inputFile|-inFile) INPUT_NEXT=1 ;;
  esac
  case "$prev" in
    -inputFile|-inFile) INPUT="$a" ;;
    -outputFile|-outFile) OUTPUT="$a" ;;
    -outcertchain|-outCertchain|-outCertChain) OUTCERT="$a" ;;
    -outprofile|-outProfile) OUTPROF="$a" ;;
    -outproof|-outProof) OUTPROOF="$a" ;;
  esac
  prev="$a"
done

if [ "$CMD" = "sign" ] || [ "$CMD" = "sign-app" ]; then
  if [ -z "$INPUT" ] || [ -z "$OUTPUT" ]; then
    echo "ERROR: Missing parameter" >&2
    exit 1
  fi
  cp "$INPUT" "$OUTPUT" || exit 1
  exit 0
fi

if [ "$CMD" = "verify" ] || [ "$CMD" = "verify-app" ]; then
  # 缺输出参数时按真实工具的行为报错，验证代码有没有传全
  if [ -z "$OUTCERT" ]; then
    echo "ERROR: Missing parameter: outcertchain" >&2
    exit 1
  fi
  if [ -z "$OUTPROF" ]; then
    echo "ERROR: Missing parameter: outprofile" >&2
    exit 1
  fi
  : > "$OUTCERT"
  : > "$OUTPROF"
  exit 0
fi

echo "ERROR: Unsupported command" >&2
exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("写 mock java 失败: %v", err)
	}
	return bin, logPath
}

// fakeSignTool 创建一个真实存在的假 jar 文件。
//
// resignHap 会先用 os.Stat 校验签名工具存在（避免把「路径写错」报成 exec 错误），
// 所以测试必须给一个真实文件，不能只给一个字符串路径。
func fakeSignTool(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hap-sign-tool.jar")
	if err := os.WriteFile(p, []byte("fake jar"), 0o600); err != nil {
		t.Fatalf("创建假 jar 失败: %v", err)
	}
	return p
}

// makeSigningMaterial 准备一套能被代码接受的材料（内容不校验，只要是文件）。
func makeSigningMaterial(t *testing.T) (hapPath string, cert hmosCertFile) {
	t.Helper()
	dir := t.TempDir()

	hapPath = filepath.Join(dir, "app.hap")
	// 必须是合法 zip，因为 readHapModuleInfo 会解析 module.json
	writeTestHap(t, hapPath, "com.test.app")

	certPath := filepath.Join(dir, "cert.pem")
	profPath := filepath.Join(dir, "profile.p7b")
	keyPath := filepath.Join(dir, "key.p12")

	os.WriteFile(certPath, []byte("CERT"), 0o600)
	os.WriteFile(profPath, buildProfileBytes(t, "com.test.app", 100), 0o600)
	os.WriteFile(keyPath, []byte("KEY"), 0o600)

	return hapPath, hmosCertFile{
		CertFile:    certPath,
		ProfileFile: profPath,
		Keystore:    keyPath,
		KeyAlias:    "debugKey",
	}
}

// 核心断言：sign 的参数名必须是实测通过的那一套。
func TestResignPassesCorrectSignArgs(t *testing.T) {
	mockJava, logPath := writeMockJava(t)
	hapPath, cert := makeSigningMaterial(t)
	outPath := filepath.Join(filepath.Dir(hapPath), "out.hap")

	cfg := resignConfig{SignTool: fakeSignTool(t), Java: mockJava}
	if _, err := resignHap(context.Background(), cfg, hapPath, outPath, cert); err != nil {
		t.Fatalf("resignHap 失败: %v", err)
	}

	log := readFile(t, logPath)

	// 参数名来自官方文档（developtools_hapsigner README），逐个断言防止改坏。
	// 子命令或参数名写错时工具只报 "Unsupported command"/"Missing parameter"，
	// 不指出正确拼法，所以用测试锁住。
	mustContain := []string{
		"sign-app", // 不是 sign
		"-mode localSign",
		"-keyAlias debugKey",
		"-signAlg SHA256withECDSA",
		"-profileFile " + cert.ProfileFile,
		"-appCertFile " + cert.CertFile,
		"-inFile " + hapPath,
		"-outFile " + outPath,
		"-keystoreFile " + cert.Keystore,
		"-signCode 1",
	}
	for _, s := range mustContain {
		if !strings.Contains(log, s) {
			t.Errorf("签名参数缺少 %q\n实际: %s", s, log)
		}
	}

	// 纯 Java 工具不需要 --add-exports（那是依赖 sun.security.* 私有 API 的工具才要的）
	if strings.Contains(log, "--add-exports") {
		t.Errorf("hap-sign-tool.jar 不需要 --add-exports\n实际: %s", log)
	}

	// 产物应存在（mock 会复制输入）
	if _, err := os.Stat(outPath); err != nil {
		t.Errorf("重签产物未生成: %v", err)
	}
}

// verify 的参数名与 sign 不同：用 verify-app + 驼峰输出参数。
func TestVerifyPassesLowercaseOutputArgs(t *testing.T) {
	mockJava, logPath := writeMockJava(t)
	hapPath, _ := makeSigningMaterial(t)

	cfg := resignConfig{SignTool: fakeSignTool(t), Java: mockJava}
	if err := verifyHap(context.Background(), cfg, hapPath); err != nil {
		t.Fatalf("verifyHap 失败: %v", err)
	}

	log := readFile(t, logPath)
	// 官方 hap-sign-tool.jar 用 verify-app + 驼峰参数（与旧的 v2 正好相反）
	if !strings.Contains(log, "verify-app") {
		t.Errorf("应调用 verify-app 子命令: %s", log)
	}
	// -outCertChain 的 C 必须大写：写成 -outCertchain 官方工具报 "Param is not trusted"
	for _, flag := range []string{"-outCertChain", "-outProfile"} {
		if !strings.Contains(log, flag) {
			t.Errorf("verify 缺少 %q\n实际: %s", flag, log)
		}
	}
	// 写成全小写会被当成未知参数，报 "Param is not trusted"
	if strings.Contains(log, "-outcertchain") {
		t.Errorf("不该用全小写 -outcertchain，要求 -outCertChain\n实际: %s", log)
	}
	// verify-app 没有 -outProof 这个参数
	if strings.Contains(log, "-outProof") {
		t.Errorf("verify-app 不支持 -outProof\n实际: %s", log)
	}
	// 输入参数名：官方工具用 -inFile
	if !strings.Contains(log, "-inFile ") {
		t.Errorf("verify 应使用 -inFile\n实际: %s", log)
	}
	// 临时输出文件应被清理
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(hapPath), ".verify-*"))
	if len(matches) > 0 {
		t.Errorf("verify 的临时文件未清理: %v", matches)
	}
}

// bundleName 不一致时必须拒绝（否则签出来装不上，报错还完全不指向真实原因）。
func TestResignRejectsBundleMismatch(t *testing.T) {
	mockJava, _ := writeMockJava(t)
	dir := t.TempDir()
	hapPath := filepath.Join(dir, "app.hap")
	writeTestHap(t, hapPath, "com.music.app")

	certPath := filepath.Join(dir, "cert.pem")
	profPath := filepath.Join(dir, "profile.p7b")
	os.WriteFile(certPath, []byte("CERT"), 0o600)
	// profile 里是另一个包名
	os.WriteFile(profPath, buildProfileBytes(t, "com.other.app", 100), 0o600)

	cert := hmosCertFile{
		CertFile:    certPath,
		ProfileFile: profPath,
		Keystore:    certPath,
		KeyAlias:    "k",
	}
	cfg := resignConfig{SignTool: fakeSignTool(t), Java: mockJava}
	_, err := resignHap(context.Background(), cfg, hapPath, filepath.Join(dir, "o.hap"), cert)
	if err == nil {
		t.Fatal("包名不一致时应报错")
	}
	if !strings.Contains(err.Error(), "不一致") {
		t.Errorf("错误信息应说明包名不一致，got: %v", err)
	}
}

// 缺签名工具时应给出明确提示，而不是让 exec 报一个看不懂的错。
func TestResignFailsClearlyWithoutSignTool(t *testing.T) {
	hapPath, cert := makeSigningMaterial(t)
	cfg := resignConfig{SignTool: "", Java: "java"}
	_, err := resignHap(context.Background(), cfg, hapPath, hapPath+".out", cert)
	if err == nil {
		t.Fatal("缺签名工具时应报错")
	}
	// 必须是真实的环境变量名，写错会让人照着配一个不生效的变量
	if !strings.Contains(err.Error(), "HMOS_SIGN_TOOL") {
		t.Errorf("错误应提到 HMOS_SIGN_TOOL，got: %v", err)
	}
}

func TestResignFailsWhenHapMissing(t *testing.T) {
	mockJava, _ := writeMockJava(t)
	_, cert := makeSigningMaterial(t)
	cfg := resignConfig{SignTool: fakeSignTool(t), Java: mockJava}
	_, err := resignHap(context.Background(), cfg, "/nonexistent/app.hap", "/tmp/o.hap", cert)
	if err == nil {
		t.Fatal("源包不存在时应报错")
	}
}

// readFile 读文件并对失败 fail。
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return string(b)
}

// writeTestHap 生成一个最小的合法 HAP（zip + module.json）。
//
// 只要能被 zip.OpenReader 打开、里面有 module.json 且 app.bundleName 正确，
// readHapModuleInfo 就能解析 —— 这样测试不依赖真实的 Flutter 产物。
func writeTestHap(t *testing.T, path, bundle string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建 hap 失败: %v", err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	defer zw.Close()

	w, err := zw.Create("module.json")
	if err != nil {
		t.Fatalf("写 module.json 失败: %v", err)
	}
	payload := map[string]interface{}{
		"app": map[string]interface{}{
			"bundleName":    bundle,
			"versionName":   "1.0.0",
			"versionCode":   1,
			"minAPIVersion": 26,
		},
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("编码 module.json 失败: %v", err)
	}
}

// buildProfileBytes 生成一个能被 parseProfileP7B 读到的 profile 字节串。
//
// 真实 p7b 是 PKCS#7 DER，这里只需保证「字节流里有一段连续 JSON」——
// 解析器就是这么实现的（见 hmos_cert.go extractProfileJSON）。
func buildProfileBytes(t *testing.T, bundle string, daysLeft int) []byte {
	t.Helper()
	notAfter := time.Now().Add(time.Duration(daysLeft) * 24 * time.Hour).Unix()
	payload := map[string]interface{}{
		"version-name": "2.0.0",
		"version-code": 2,
		"validity": map[string]int64{
			"not-before": notAfter - 365*86400,
			"not-after":  notAfter,
		},
		"type": "release",
		"bundle-info": map[string]interface{}{
			"bundle-name": bundle,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化 profile 失败: %v", err)
	}
	out := append([]byte{0x30, 0x82, 0x01, 0x00}, body...)
	return append(out, 0x00, 0x01)
}
