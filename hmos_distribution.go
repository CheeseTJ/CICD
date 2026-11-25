package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Profile 分发类型与侧载可行性。
//
// 为什么单独成一个文件
// -------------------
// 这是整套鸿蒙证书管理里最容易判断错的一件事，而且判断错了会让用户
// 反复折腾（下载 → 装不上 → 以为是包坏了 → 重新签名 → 还是装不上）。
//
// 华为的 profile 里有两个字段决定「这个包能不能本地侧载」：
//
//	type                    debug / release
//	app-distribution-type   app_gallery / internaltesting / enterprise / ...
//
// 关键结论（实测 + 官方文档交叉验证）：
//
//	app_gallery    → 应用市场分发。**只能从华为应用市场安装**，
//	                 本地侧载会被系统拒绝（提示「应用未上架」或直接无响应）。
//	                 issuer 字段也会写成 app_gallery。
//	internaltesting→ 内部测试分发（即 AGC 的「指定设备发布」）。
//	                 允许侧载，但仅限 device-ids 里登记的 UDID。
//	enterprise     → 企业分发，允许侧载，通常有企业证书链。
//
// 也就是说「type=release + 0 台设备 = 不限设备所以能装」是**错的**。
// 0 台设备只说明它不靠 UDID 白名单限制，但 app_gallery 会用「必须走
// 应用市场」这条规则把侧载堵死。

const (
	// DistributionAppGallery 应用市场分发：禁止侧载。
	DistributionAppGallery = "app_gallery"
	// DistributionInternalTesting 内部测试分发：可侧载，限 UDID。
	DistributionInternalTesting = "internaltesting"
	// DistributionEnterprise 企业分发：可侧载。
	DistributionEnterprise = "enterprise"
)

// sideloadVerdict 侧载可行性判定结果。
type sideloadVerdict struct {
	// Allowed 是否能通过「下载 hap 直接装」的方式安装
	Allowed bool `json:"allowed"`
	// Reason 给用户看的一句话原因
	Reason string `json:"reason"`
	// Hint 下一步该怎么做（Allowed 时为空）
	Hint string `json:"hint,omitempty"`
}

// judgeSideload 判断这套 profile 能否支持本地侧载安装。
//
// 参数来自 p7b 解析结果：distribution 是 app-distribution-type，
// devices 是登记的设备数，ptype 是 type（debug/release）。
func judgeSideload(distribution, ptype string, devices int) sideloadVerdict {
	switch distribution {
	case DistributionAppGallery:
		return sideloadVerdict{
			Allowed: false,
			Reason:  "Profile 是「应用市场分发」类型（app_gallery），只能从华为应用市场安装，本地侧载会被系统拒绝",
			Hint:    "去 AGC 申请「指定设备发布」类型的 Profile（有效期 180 天），或把应用发布到应用市场",
		}
	case DistributionInternalTesting:
		if devices == 0 {
			// internaltesting 但没登记设备：装不了任何机器，多半是配置漏了
			return sideloadVerdict{
				Allowed: false,
				Reason:  "Profile 是「指定设备发布」但没有登记任何设备，没有设备能安装",
				Hint:    "到 AGC 的 Profile 里添加测试设备的 UDID 后重新下载",
			}
		}
		return sideloadVerdict{
			Allowed: true,
			Reason:  fmt.Sprintf("可侧载，仅限登记的 %d 台设备", devices),
		}
	case DistributionEnterprise:
		return sideloadVerdict{
			Allowed: true,
			Reason:  "企业分发 Profile，可侧载安装",
		}
	case "":
		// 老版本 AGC 可能不带这个字段，无法判断，不误报
		if ptype == "debug" {
			if devices > 0 {
				return sideloadVerdict{
					Allowed: true,
					Reason:  fmt.Sprintf("调试 Profile，仅限登记的 %d 台设备", devices),
				}
			}
			return sideloadVerdict{
				Allowed: false,
				Reason:  "调试 Profile 且未登记设备，没有设备能安装",
				Hint:    "到 AGC 添加测试设备 UDID",
			}
		}
		return sideloadVerdict{
			Allowed: false,
			Reason:  "Profile 缺少分发类型字段，无法判断能否侧载",
			Hint:    "建议重新从 AGC 下载 Profile",
		}
	default:
		return sideloadVerdict{
			Allowed: false,
			Reason:  "Profile 分发类型为 " + distribution + "，未验证支持侧载",
		}
	}
}

// distributionLabel 给界面显示用的短名。
func distributionLabel(distribution string) string {
	switch distribution {
	case DistributionAppGallery:
		return "应用市场"
	case DistributionInternalTesting:
		return "指定设备发布"
	case DistributionEnterprise:
		return "企业分发"
	case "":
		return ""
	default:
		return distribution
	}
}

// profileDistribution 从 p7b 原始 JSON 里读 app-distribution-type。
//
// 单独解析而不并进 certInfo，是因为这个字段只在「判断侧载」这一个场景用，
// 塞进 certInfo 会让「证书信息」这个结构混入分发策略的语义。
func profileDistribution(p7bPath string) string {
	if p7bPath == "" {
		return ""
	}
	data, err := os.ReadFile(p7bPath)
	if err != nil {
		return ""
	}
	raw, err := extractProfileJSON(data)
	if err != nil {
		return ""
	}
	var v struct {
		Distribution string `json:"app-distribution-type"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return strings.TrimSpace(v.Distribution)
}
