package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 证书与 keystore 配对校验的测试。
//
// 这个校验的目的很具体：用户从 AGC 申请发布证书时，如果在 DevEco 里
// 点了「新建密钥库」而不是「选择已有」，AGC 会签出一张完全正常的 cer，
// 但它和本地 p12 的私钥不是一对 —— 直到重签 HAP 才失败，报错还只说
// 「签名验证失败」。所以必须在上传时就把这种情况拦下来。
//
// 这里用 keytool 现场生成两套真实材料来验证：同一套必须判 ok，
// 交叉组合必须判 mismatch。

const testStorePass = "TestPass123!"

// makeKeyPairWithCert 生成一个 p12 并把它自带的证书导出成 cer 文件。
// 返回 p12 路径与 cer 路径。
func makeKeyPairWithCert(t *testing.T, dir, alias string) (p12, cer string) {
	t.Helper()
	p12 = filepath.Join(dir, alias+".p12")
	cer = filepath.Join(dir, alias+".cer")

	// keytool 生成自签名证书，直接落在 p12 里
	gen := exec.Command("keytool",
		"-genkeypair",
		"-keystore", p12,
		"-storetype", "PKCS12",
		"-storepass", testStorePass,
		"-keypass", testStorePass,
		"-alias", alias,
		"-keyalg", "EC",
		"-groupname", "secp256r1",
		"-dname", "CN=test-"+alias+",O=Test,C=CN",
		"-validity", "365",
	)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("环境没有可用 keytool，跳过: %v\n%s", err, out)
	}

	// 导出该别名的证书为 PEM
	exp := exec.Command("keytool",
		"-exportcert", "-rfc",
		"-keystore", p12,
		"-storepass", testStorePass,
		"-alias", alias,
		"-file", cer,
	)
	if out, err := exp.CombinedOutput(); err != nil {
		t.Fatalf("导出证书失败: %v\n%s", err, out)
	}
	return p12, cer
}

func TestVerifyKeyPairMatch(t *testing.T) {
	dir := t.TempDir()
	p12, cer := makeKeyPairWithCert(t, dir, "k1")

	got := verifyKeyPair(cer, p12, testStorePass, "k1", "")
	if got.Status != KeyMatchOK {
		t.Fatalf("同一套材料应判 ok，得到 %+v", got)
	}
	if got.Detail != "" {
		t.Errorf("ok 时不该有 Detail, 得到 %q", got.Detail)
	}
}

// 这是核心用例：证书 A + 私钥 B 必须被识别为不配对。
func TestVerifyKeyPairMismatch(t *testing.T) {
	dir := t.TempDir()
	p12a, _ := makeKeyPairWithCert(t, dir, "ka")
	_, cerB := makeKeyPairWithCert(t, dir, "kb")

	got := verifyKeyPair(cerB, p12a, testStorePass, "ka", "")
	if got.Status != KeyMatchMismatch {
		t.Fatalf("不同密钥对必须判 mismatch，得到 %+v", got)
	}
	if !strings.Contains(got.Detail, "不是一对") {
		t.Errorf("Detail 应说明证书与 keystore 不是一对, 实际 %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "CSR") {
		t.Errorf("Detail 应提示 CSR 必须由该 p12 生成, 实际 %q", got.Detail)
	}
}

// 密码错、别名不存在都属于「拿不到公钥」，必须归 unknown 而不是 mismatch。
// 归错会让用户以为配对有问题，跑去重新申请证书 —— 白折腾。
func TestVerifyKeyPairUnknownCases(t *testing.T) {
	dir := t.TempDir()
	p12, cer := makeKeyPairWithCert(t, dir, "k1")

	cases := []struct {
		name     string
		cert     string
		key      string
		password string
		alias    string
	}{
		{"空密码", cer, p12, "", "k1"},
		{"空别名", cer, p12, testStorePass, ""},
		{"空证书路径", "", p12, testStorePass, "k1"},
		{"空 keystore 路径", cer, "", testStorePass, "k1"},
		{"密码错误", cer, p12, "WrongPassword!", "k1"},
		{"别名不存在", cer, p12, testStorePass, "no-such-alias"},
		{"证书文件不存在", filepath.Join(dir, "nope.cer"), p12, testStorePass, "k1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := verifyKeyPair(c.cert, c.key, c.password, c.alias, "")
			if got.Status != KeyMatchUnknown {
				t.Errorf("应判 unknown, 得到 %+v", got)
			}
			if got.Detail == "" {
				t.Error("unknown 必须给出原因，否则用户不知道为什么不校验")
			}
		})
	}
}

// 密码错误时的提示要指名环境变量，否则运维不知道该改哪里。
func TestVerifyKeyPairWrongPasswordMessage(t *testing.T) {
	dir := t.TempDir()
	p12, cer := makeKeyPairWithCert(t, dir, "k1")

	got := verifyKeyPair(cer, p12, "DefinitelyWrong!", "k1", "")
	if !strings.Contains(got.Detail, "HMOS_KEYSTORE_PWD") {
		t.Errorf("密码错误提示应点名 HMOS_KEYSTORE_PWD, 实际 %q", got.Detail)
	}
}

// 结果要缓存：buildHmosView 每次列表请求都会调，不缓存则每请求 fork 一个 JVM。
// 但文件被替换必须失效 —— 否则换完证书页面还显示旧的配对结论。
func TestVerifyKeyPairCacheInvalidatesOnFileChange(t *testing.T) {
	dir := t.TempDir()
	p12a, cerA := makeKeyPairWithCert(t, dir, "ka")

	// 先建立缓存：ok
	if got := verifyKeyPair(cerA, p12a, testStorePass, "ka", ""); got.Status != KeyMatchOK {
		t.Fatalf("初始应为 ok, 得到 %+v", got)
	}

	// 造一套新密钥，用第二套替换 cer 文件内容（模拟用户重新上传证书）
	p12b, cerB := makeKeyPairWithCert(t, dir, "kb")
	newCer, err := os.ReadFile(cerB)
	if err != nil {
		t.Fatal(err)
	}
	// 推进 mtime，确保指纹变化（某些文件系统 mtime 精度较粗）
	if err := os.WriteFile(cerA, newCer, 0o600); err != nil {
		t.Fatal(err)
	}
	tm := time.Now().Add(2 * time.Second)
	os.Chtimes(cerA, tm, tm)

	got := verifyKeyPair(cerA, p12a, testStorePass, "ka", "")
	if got.Status != KeyMatchMismatch {
		t.Errorf("证书被替换后应重新校验并判 mismatch（缓存未失效），得到 %+v", got)
	}
	_ = p12b
}

// 真实证书链测试：用 openssl 现场生成「根 CA → 中间 CA → 叶子」三段链。
//
// 这是修复的那个 bug 的核心场景。AGC 导出的链就是这个形状，而修复前的
// 实现用「第一个 Subject != Issuer」找叶子，会命中中间 CA —— 真配对
// 被误判成 mismatch，用户会以为证书白申请了。
func TestLeafPublicKeyRealCAChain(t *testing.T) {
	dir := t.TempDir()

	// 根 CA（自签名）
	rootKey := filepath.Join(dir, "root.key")
	rootCrt := filepath.Join(dir, "root.crt")
	runOpenSSL(t, "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", rootKey)
	runOpenSSL(t, "req", "-x509", "-new", "-key", rootKey, "-sha256", "-days", "3650",
		"-out", rootCrt, "-subj", "/CN=Test Root CA/O=Test/C=CN")

	// 中间 CA（根签的）
	intKey := filepath.Join(dir, "int.key")
	intCsr := filepath.Join(dir, "int.csr")
	intCrt := filepath.Join(dir, "int.crt")
	runOpenSSL(t, "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", intKey)
	runOpenSSL(t, "req", "-new", "-key", intKey, "-out", intCsr,
		"-subj", "/CN=Test Intermediate CA/O=Test/C=CN")

	extFile := filepath.Join(dir, "ca.ext")
	if err := os.WriteFile(extFile, []byte("basicConstraints=critical,CA:TRUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runOpenSSL(t, "x509", "-req", "-in", intCsr, "-CA", rootCrt, "-CAkey", rootKey,
		"-CAcreateserial", "-days", "3000", "-sha256", "-extfile", extFile, "-out", intCrt)

	// 叶子（中间签的）
	leafKey := filepath.Join(dir, "leaf.key")
	leafCsr := filepath.Join(dir, "leaf.csr")
	leafCrt := filepath.Join(dir, "leaf.crt")
	runOpenSSL(t, "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", leafKey)
	runOpenSSL(t, "req", "-new", "-key", leafKey, "-out", leafCsr,
		"-subj", "/CN=Test Leaf,Release/O=Test/C=CN")
	runOpenSSL(t, "x509", "-req", "-in", leafCsr, "-CA", intCrt, "-CAkey", intKey,
		"-CAcreateserial", "-days", "1095", "-sha256", "-out", leafCrt)

	// 叶子公钥作为期望值
	want := pubKeyDER(t, leafCrt)

	// 按 AGC 的顺序拼链：根 → 中间 → 叶子
	var chain []byte
	for _, f := range []string{rootCrt, intCrt, leafCrt} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		chain = append(chain, b...)
	}
	chainFile := filepath.Join(dir, "chain.cer")
	if err := os.WriteFile(chainFile, chain, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := leafPublicKey(chainFile)
	if err != nil {
		t.Fatalf("解析链失败: %v", err)
	}
	if string(got) != string(want) {
		t.Error("应取叶子公钥，实际取到了中间 CA 或根 CA —— 这会导致配对误判 mismatch")
	}
}

// 顺序颠倒（叶子 → 中间 → 根）也必须取到叶子，不能靠位置猜。
func TestLeafPublicKeyReversedChain(t *testing.T) {
	dir := t.TempDir()
	rootKey := filepath.Join(dir, "root.key")
	rootCrt := filepath.Join(dir, "root.crt")
	runOpenSSL(t, "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", rootKey)
	runOpenSSL(t, "req", "-x509", "-new", "-key", rootKey, "-sha256", "-days", "3650",
		"-out", rootCrt, "-subj", "/CN=Test Root CA/O=Test/C=CN")

	objKey := filepath.Join(dir, "obj.key")
	objCsr := filepath.Join(dir, "obj.csr")
	objCrt := filepath.Join(dir, "obj.crt")
	runOpenSSL(t, "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", objKey)
	runOpenSSL(t, "req", "-new", "-key", objKey, "-out", objCsr,
		"-subj", "/CN=Test Object Release/O=Test/C=CN")
	runOpenSSL(t, "x509", "-req", "-in", objCsr, "-CA", rootCrt, "-CAkey", rootKey,
		"-CAcreateserial", "-days", "1095", "-sha256", "-out", objCrt)

	want := pubKeyDER(t, objCrt)

	// 反序：叶子在前，根在后
	var chain []byte
	for _, f := range []string{objCrt, rootCrt} {
		b, _ := os.ReadFile(f)
		chain = append(chain, b...)
	}
	chainFile := filepath.Join(dir, "rev.cer")
	if err := os.WriteFile(chainFile, chain, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := leafPublicKey(chainFile)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if string(got) != string(want) {
		t.Error("反序链也应取到叶子公钥")
	}
}

func runOpenSSL(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("openssl", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("openssl %v 失败: %v\n%s", args, err, out)
	}
}

func pubKeyDER(t *testing.T, certFile string) []byte {
	t.Helper()
	out, err := exec.Command("openssl", "x509", "-in", certFile, "-noout", "-pubkey").Output()
	if err != nil {
		t.Fatalf("读 %s 公钥失败: %v", certFile, err)
	}
	cmd := exec.Command("openssl", "pkey", "-pubin", "-outform", "DER")
	cmd.Stdin = bytes.NewReader(out)
	d, err := cmd.Output()
	if err != nil {
		t.Fatalf("公钥转 DER 失败: %v", err)
	}
	return d
}
