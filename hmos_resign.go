// HAP 重签名：证书/profile 到期后，用新的签名材料给已有 HAP 续期。
//
// 为什么不重新编译
// ----------------
// HAP 的签名是追加在 zip 尾部的独立签名块（Hap Signature Block）。重签时把旧块
// 剥掉写新块，zip 里的业务内容（module.json / libs / resources）一个字节都不动。
// 所以只要换掉 cert+profile，同一份包就能再装一年，不需要 Flutter 源码、
// 不需要 DevEco、不需要重新编译。
//
// 硬约束
// ------
//  1. p12 私钥必须与 cert 匹配。换证书时如果连私钥一起换，旧包重签会失败
//     （profile 里绑定的公钥与新私钥不配对）。所以**首次配置时就要把 p12 固定
//     下来**，以后只在 AGC 重新下载 cert/profile。
//  2. bundleName 必须与 profile 里的 bundle-name 一致，否则系统拒装。
//  3. HAP 里的 minAPIVersion 要传给签名工具作 compatibleVersion。
//
// 工具
// ----
// 用华为的 hapsigntoolv2.jar。实测要点（跑通过才敢写进代码）：
//   - 子命令是 `sign`，不是 `sign-app`
//   - mode 是 `localjks`（不是 localSign / localFileSign）
//   - 参数名是 `-profile`（不是 -profileFile）、`-inputFile` / `-outputFile`
//     （不是 -inFile / -outFile）、`-privatekey`（不能省，值同 keystore 路径）
//   - JDK 17+ 需要加 `--add-exports java.base/sun.security.pkcs=ALL-UNNAMED`，
//     否则报 IllegalAccessError（工具内部用了 sun.security.pkcs.PKCS7）
//
// 注意：这个 jar **不支持 sign-profile**，所以不能本地生成 p7b。
// profile 必须从 AGC 下载（这就是为什么需要用户手动去华为开发者中心）。
package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// resignConfig 重签名所需参数，全部可由环境变量配置。
type resignConfig struct {
	// SignTool 华为 hapsigntoolv2.jar 的路径
	SignTool string
	// Java java 可执行文件路径（JDK 17+）
	Java string
	// WorkDir 临时工作目录，空则用系统临时目录
	WorkDir string
}

// hapModuleInfo 从 HAP 的 module.json 里读出的关键字段。
type hapModuleInfo struct {
	BundleName    string `json:"bundleName"`
	VersionName   string `json:"versionName"`
	VersionCode   int    `json:"versionCode"`
	MinAPIVersion int    `json:"minAPIVersion"`
}

// readHapModuleInfo 读取 HAP 里的 module.json。
//
// 只读取需要的几个字段用于预检（bundleName 是否与 profile 匹配、compatibleVersion
// 该填多少）。这里不解整个包，用 zip reader 流式取那一个 entry。
func readHapModuleInfo(hapPath string) (*hapModuleInfo, error) {
	zr, err := zip.OpenReader(hapPath)
	if err != nil {
		return nil, fmt.Errorf("打开 hap 失败: %w", err)
	}
	defer zr.Close()

	// HAP 的顶层有 module.json，也可能在 module.json 之外还有 pack.info。
	// bundleName 只有 module.json 里 app.bundleName 一个来源。
	// 注意：HCSP 格式里字段嵌在 app 对象下，不是顶层。
	for _, f := range zr.File {
		if f.Name != "module.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		var wrapper struct {
			App hapModuleInfo `json:"app"`
		}
		err = json.NewDecoder(rc).Decode(&wrapper)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("解析 module.json 失败: %w", err)
		}
		return &wrapper.App, nil
	}
	return nil, fmt.Errorf("%s 里没有 module.json，不是合法的 HAP", filepath.Base(hapPath))
}

// resignResult 重签结果，直接下发给前端。
type resignResult struct {
	OutputPath string `json:"output_path"`
	BundleName string `json:"bundle_name"`
	Version    string `json:"version"`
	Size       int64  `json:"size"`
}

// resignHap 用给定材料给 hapPath 重新签名，输出到 outPath。
//
// 调用方负责传**已校验过**的材料：这里只做签名，不做「证书是否过期」这类判断
// （那是 collectCertInfo 的职责，两者职责分开便于测试）。
func resignHap(ctx context.Context, cfg resignConfig, hapPath, outPath string, cert hmosCertFile) (*resignResult, error) {
	if cfg.SignTool == "" {
		return nil, fmt.Errorf("未配置 HAP_SIGN_TOOL（hapsigntoolv2.jar 路径）")
	}
	if _, err := os.Stat(cfg.SignTool); err != nil {
		return nil, fmt.Errorf("找不到签名工具 %s: %w", cfg.SignTool, err)
	}
	if cert.CertFile == "" || cert.ProfileFile == "" {
		return nil, fmt.Errorf("证书与 profile 都必须配置")
	}
	for _, p := range []string{cert.CertFile, cert.ProfileFile, cert.Keystore} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("找不到文件 %s: %w", p, err)
		}
	}
	if cert.KeyAlias == "" {
		return nil, fmt.Errorf("未配置 key_alias")
	}

	// 预检：bundleName 必须与 profile 一致，否则签出来装不上（系统报 pkcs7 校验失败，
	// 错误信息完全不指向真实原因，先在这里挡住能省很多排查时间）。
	mi, err := readHapModuleInfo(hapPath)
	if err != nil {
		return nil, err
	}
	if pv, err := parseProfileP7B(cert.ProfileFile); err == nil {
		if pv.BundleName != "" && pv.BundleName != mi.BundleName {
			return nil, fmt.Errorf(
				"profile 绑定的 bundleName（%s）与 HAP 里的（%s）不一致，签出来系统会拒装",
				pv.BundleName, mi.BundleName)
		}
	}

	compatible := mi.MinAPIVersion
	if compatible <= 0 {
		compatible = 26 // 鸿蒙 7 的默认值，兜底用
	}

	java := cfg.Java
	if java == "" {
		java = "java"
	}

	// 实测必需的参数名与 mode，见文件头注释。
	args := []string{
		// JDK 17+ 模块系统限制：工具内部反射用了 sun.security.pkcs.PKCS7
		"--add-exports", "java.base/sun.security.pkcs=ALL-UNNAMED",
		"-jar", cfg.SignTool,
		"sign",
		"-mode", "localjks",
		"-keyAlias", cert.KeyAlias,
		"-signAlg", "SHA256withECDSA",
		"-profile", cert.ProfileFile,
		"-appCertFile", cert.CertFile,
		"-privatekey", cert.Keystore,
		"-inputFile", hapPath,
		"-outputFile", outPath,
		"-keystoreFile", cert.Keystore,
		"-compatibleVersion", fmt.Sprintf("%d", compatible),
	}
	// 密码通过环境变量之外的参数传入；若配置了就用，没配就用空（部分 keystore 无密码）
	if pwd := os.Getenv("HMOS_KEYSTORE_PWD"); pwd != "" {
		args = append(args, "-keystorePwd", pwd, "-keyPwd", pwd)
	}

	cmd := exec.CommandContext(ctx, java, args...)
	// 工具输出里可能有密码回显，统一截断后只在失败时回传尾部
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := tailString(string(out), 1200)
		return nil, fmt.Errorf("签名失败: %w\n%s", err, tail)
	}

	fi, err := os.Stat(outPath)
	if err != nil {
		return nil, fmt.Errorf("签名命令成功但产物不存在: %w\n%s", err, tailString(string(out), 800))
	}

	return &resignResult{
		OutputPath: outPath,
		BundleName: mi.BundleName,
		Version:    mi.VersionName,
		Size:       fi.Size(),
	}, nil
}

// verifyHap 调工具的 verify 子命令确认产物可被系统接受。
//
// 单独做成一步而不是塞进 resignHap：签名成功但校验失败是有可能的（材料不匹配），
// 分开调用能让调用方决定「校验失败是否阻断上传」。
//
// 参数说明（实测）：verify 必须同时给 outCertChain 与 outSignedProfile 两个
// **输出**路径，工具会把解出来的证书链与 profile 写到那里。不给会直接报
// "Missing parameter: outcertchain"，而不是像文档暗示的那样可选。
func verifyHap(ctx context.Context, cfg resignConfig, hapPath string) error {
	java := cfg.Java
	if java == "" {
		java = "java"
	}

	// 这两个输出文件我们并不使用，但工具要求必须给路径。
	// 放在 hap 同目录的临时文件里，调用结束即删。
	dir := filepath.Dir(hapPath)
	base := fmt.Sprintf(".verify-%d", time.Now().UnixNano())
	outCert := filepath.Join(dir, base+".cer")
	outProfile := filepath.Join(dir, base+".p7b")
	defer os.Remove(outCert)
	defer os.Remove(outProfile)

	args := []string{
		"--add-exports", "java.base/sun.security.pkcs=ALL-UNNAMED",
		"-jar", cfg.SignTool,
		"verify",
		"-inFile", hapPath,
		"-outCertChain", outCert,
		"-outProfile", outProfile,
	}
	cmd := exec.CommandContext(ctx, java, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("校验失败: %w\n%s", err, tailString(string(out), 1000))
	}
	return nil
}

// tailString 截取字符串末尾 n 个字符（按 rune 计，避免切坏中文）。
func tailString(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return "…" + string(runes[len(runes)-n:])
}

// resignTimeout 单次重签的超时。签名本身很快（秒级），主要留给大包 IO。
const resignTimeout = 5 * time.Minute
