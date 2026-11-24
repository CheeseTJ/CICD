package main

import (
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

// leafPublicKey 要能从多张证书的链里挑出叶子，而不是错拿根 CA 的公钥。
// 拿错了会导致所有配对都判 mismatch —— 比不校验更糟。
func TestLeafPublicKeyPicksLeafFromChain(t *testing.T) {
	dir := t.TempDir()
	_, cer := makeKeyPairWithCert(t, dir, "k1")
	leaf, err := os.ReadFile(cer)
	if err != nil {
		t.Fatal(err)
	}

	// 在叶子后面追加一张无关的根证书，模拟「叶子→中间→根」的链文件
	_, otherCer := makeKeyPairWithCert(t, dir, "other")
	root, err := os.ReadFile(otherCer)
	if err != nil {
		t.Fatal(err)
	}
	chain := filepath.Join(dir, "chain.cer")
	if err := os.WriteFile(chain, append(append([]byte{}, leaf...), root...), 0o600); err != nil {
		t.Fatal(err)
	}

	pubChain, err := leafPublicKey(chain)
	if err != nil {
		t.Fatalf("解析证书链失败: %v", err)
	}
	pubLeaf, err := leafPublicKey(cer)
	if err != nil {
		t.Fatalf("解析单张证书失败: %v", err)
	}
	if string(pubChain) != string(pubLeaf) {
		t.Error("链文件里应取叶子证书公钥，实际取到了别的证书")
	}
}
