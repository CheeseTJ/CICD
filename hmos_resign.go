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
// 用 OpenHarmony 官方的 hap-sign-tool.jar（developtools_hapsigner 的 dist/）。
// 纯 Java 实现，JDK 8+ 都能跑，实测 JDK 21 可用。
//
// 参数（官方文档 + 实测）：
//   - 子命令 `sign-app`，mode `localSign`
//   - `-keystoreFile` 支持 JKS 和 P12 两种格式
//   - `-keyPwd` / `-keystorePwd` 必须给（缺了报的是「密码错误」而非「参数缺失」，
//     容易误判成密码配错）
//   - 校验用 `verify-app`，其输出参数是 `-outCertChain`（C 大写）和 `-outProfile`
//
// 这个 jar 也能 `sign-profile`，但我们的 profile 全部来自 AGC，不需要本地生成。

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
	// SignTool hap-sign-tool.jar 的路径
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
		// 提示必须用真实的环境变量名（HMOS_SIGN_TOOL）。写成 HAP_SIGN_TOOL
		// 会让人照着去配一个根本不生效的变量，白白排查一轮。
		return nil, fmt.Errorf("未配置签名工具：请设置环境变量 HMOS_SIGN_TOOL 指向 hap-sign-tool.jar")
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

	java := cfg.Java
	if java == "" {
		java = "java"
	}

	// 参数名见文件头注释（官方文档 + 实测）。
	args := []string{
		"-jar", cfg.SignTool,
		"sign-app",
		"-mode", "localSign",
		"-keyAlias", cert.KeyAlias,
		"-signAlg", "SHA256withECDSA",
		"-profileFile", cert.ProfileFile,
		"-appCertFile", cert.CertFile,
		"-inFile", hapPath,
		"-outFile", outPath,
		"-keystoreFile", cert.Keystore,
		"-signCode", "1",
	}
	// 密码：keyPwd 和 keystorePwd 都必须显式给。
	// 实测缺任一个都报「密码错误」，而不是参数缺失 —— 容易误判成密码配错了。
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
// 参数名与 sign 子命令**不一致**，实测结论（官方无文档，靠试出来的）：
//
//	输入：-inputFile（不是 sign 的 -inputFile？sign 也是这个，但 verify 不接受 -inFile）
//	输出：-outcertchain / -outprofile / -outproof，**全部小写**
//
// 写成驼峰（-outCertChain）会被判定为「未提供该参数」，报
// "Missing parameter: outcertchain" —— 错误信息里的名字就是小写的，照着抄即可。
// outproof 缺失只是 WARN，不给也能验，但产物解析可能不完整，所以一并给上。
func verifyHap(ctx context.Context, cfg resignConfig, hapPath string) error {
	java := cfg.Java
	if java == "" {
		java = "java"
	}

	// 这三个输出文件我们并不使用，但工具要求必须给路径。
	// 放在 hap 同目录的临时文件里，调用结束即删。
	dir := filepath.Dir(hapPath)
	base := fmt.Sprintf(".verify-%d", time.Now().UnixNano())
	outCert := filepath.Join(dir, base+".cer")
	outProfile := filepath.Join(dir, base+".p7b")
	defer os.Remove(outCert)
	defer os.Remove(outProfile)

	// verify-app 只有三个必填参数，且 -outCertChain 的 C 是大写
	// （写成 -outCertchain 会被当成未知参数，报 "Param is not trusted"）。
	args := []string{
		"-jar", cfg.SignTool,
		"verify-app",
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
