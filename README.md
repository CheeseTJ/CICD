<div align="center">
  <h1>CICD · 自托管 CI/CD 管理控制台</h1>
  <p>一个自托管的多仓库发布与部署监控面板：GitHub Release → 蒲公英分发、Actions 部署状态、Cloudflare Pages / Workers、服务健康探测</p>
</div>

<div align="center">

![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8)
![Docker](https://img.shields.io/badge/Docker-ready-2496ED)
![Cloudflare](https://img.shields.io/badge/Cloudflare-Pages%20%7C%20Workers-F38020)
![License](https://img.shields.io/badge/License-MIT-green)

</div>

## 项目定位

把「我有一堆项目，每个都要发版、部署、盯状态」这件事收敛到一个页面里。后台是一个
Go 单二进制，前端是它内嵌的一个 `index.html`（零构建、零 node_modules），跑起来只需
要一个 `docker compose up`。

```text
浏览器 ──> cicd 控制台 (Go + 内嵌前端)
              ├──> GitHub API       仓库列表 / Release / Actions 运行记录
              ├──> 蒲公英 API         iOS 内测分发（上传 + 二维码）
              ├──> Cloudflare API    Pages 部署记录 / Workers 绑定域名
              └──> 各服务健康接口     浏览器直连探测（侧边栏「服务」视图）
```

状态全部落在文件系统（`data/` 下的 JSON），不依赖数据库。

## 功能

### 仓库总览
- 自动同步账号下全部仓库，按「主体在哪」渲染平台徽章（Actions / Pages / Workers /
  Release）
- 统计卡：仓库数、最近发布、部署失败数、服务在线数，全部由 `/api/repos` 实时计算
- 支持手动标记（置顶/忽略）与自动同步间隔配置

### 发布与部署
- **GitHub Release** —— 列表、详情、资产下载量
- **蒲公英** —— 上传 ipa/apk、生成安装二维码、查看历史版本
- **Actions** —— 每个仓库最近若干次 workflow run 的状态与耗时
- **Cloudflare Pages** —— 部署记录、分支、提交信息（只读）
- **Cloudflare Workers** —— 采集 Worker 脚本及其绑定的自定义域，用于判定前端站点
  是否已从 Pages 迁到 Workers

### 服务监控
- 配置若干「名称=健康接口 URL」，浏览器侧并发探测，纯静态页面也能用
- 针对自托管场景做了取舍：容器虽走 host 网络，探测仍统一走公网域名，顺带验证隧道与
  证书链

### 鸿蒙应用（证书到期与重签）

侧边栏「鸿蒙」视图管理 HarmonyOS 应用的签名材料生命周期：

- **到期倒计时** —— 分别展示证书（`.cer`）与 profile（`.p7b`）的有效期，
  取较早者作为真正失效时间；剩余 ≤ `HMOS_WARN_DAYS`（默认 30 天）标黄，
  过期标红
- **一键跳转华为开发者中心** —— 证书到期后去 AGC 重新下载 cert / profile
- **重签** —— 用新的 cert+profile 给已有 HAP 续期。HAP 的签名是追加在 zip 尾部的
  独立块，重签只换签名块，**包内容不动，不需要重新编译**
- **同步蒲公英** —— 重签后上传（`buildType=harmonyos`），回写下载页地址，
  手机扫码即可安装
- **到期自动重签** —— 开启后每天检查一次，临近过期就自动重签 + 上传

> 有效期直接读本地文件的 X.509 `notAfter` 与 profile 的 `validity.not-after`，
> 不调用 AGC API，所以不依赖网络、也不需要 client_secret。
>
> ⚠️ **keystore 不要跟着换**。重签必须用同一把私钥，否则 cert 与私钥不匹配，
> 签出来的包装上就报 pkcs7 校验失败（错误信息完全不指向真实原因）。

```bash
# 签名工具（重签功能需要，不配则只显示倒计时）
curl -O https://repo.huaweicloud.com/harmonyos/develop_tools/hapsigntoolv2.jar
```

### 鉴权
- 接入统一身份中心（JWT + JWKS 校验），支持 `USER_CENTER_URL` / `USER_CENTER_APP`
- 邮箱白名单，在「角色必须是 admin」之上再收窄一层

## 快速开始

```bash
git clone https://github.com/CheeseTJ/CICD.git
cd CICD

cp .env.example .env      # 按需填写，全部留空也能起（功能降级）
docker compose up -d --build

curl http://localhost:8090/api/health
```

访问 `http://localhost:8090`。

### 配置项

全部通过环境变量注入，见 `.env.example`。关键几项：

| 变量 | 用途 | 未配置时 |
|---|---|---|
| `GITHUB_OWNER` / `GH_TOKEN` | 拉取仓库、Release、Actions | 仓库视图为空 |
| `PGYER_API_KEY` | 蒲公英上传与查询 | 蒲公英功能隐藏 |
| `CLOUDFLARE_API_TOKEN` | Pages 部署记录、Workers 脚本 | `/api/pages` 返回 `configured=false` 并提示 |
| `CF_ACCOUNT_ID` | 同上 | 同上 |
| `HEALTH_TARGETS` | 服务健康探测目标 | 监控视图关闭 |
| `HMOS_SIGN_TOOL` | `hapsigntoolv2.jar` 路径 | 鸿蒙重签不可用，倒计时仍显示 |
| `HMOS_KEYSTORE_PWD` | p12/jks 密码 | 重签报错提示缺密码 |
| `HMOS_WARN_DAYS` | 剩余多少天标「即将过期」（默认 30） | 用默认值 |
| `USER_CENTER_URL` / `USER_CENTER_APP` | 登录鉴权 | **鉴权关闭，接口对外开放** |
| `ALLOWED_EMAILS` | 邮箱白名单 | 任意 admin 可操作 |

> ⚠️ `USER_CENTER_URL` 留空等于关鉴权。对外暴露前务必配置。

## 本地开发

无需前端构建，改完 `web/index.html` 重新构建镜像即可：

```bash
go build -o /tmp/cicd . && /tmp/cicd     # 直接跑二进制，默认监听 :8090
go test ./...
```

## 目录结构

```text
main.go            HTTP 路由 + 启动 + 内嵌前端
github.go          GitHub API 客户端
pgyer.go           蒲公英 API 客户端（APK / HAP 两种 buildType）
cloudflare.go      Pages / Workers 采集
platforms.go       平台徽章判定
monitor.go         服务健康探测
hmos_cert.go       鸿蒙证书解析（X.509 与 p7b 有效期）
hmos_resign.go     HAP 重签名（调 hapsigntoolv2.jar）
hmos_store.go      鸿蒙应用清单持久化
hmos_api.go        鸿蒙相关接口 + 到期自动重签
store.go           data/ 下的 JSON 持久化
auth.go            JWT / JWKS 校验与中间件
web/index.html     内嵌前端（单文件，无构建）
SPEC.md            UI 与接口规格
```

## 部署

仓库自带 `.github/workflows/deploy.yml`：push 到 `main` 后在 self-hosted runner 上
`docker compose up -d --build`，带健康检查与失败回滚。

> 该 job 含 `if: github.repository_owner == 'CheeseTJ'` 门禁——self-hosted runner 与
> 生产服务同机，这道门禁防止外部 fork/PR 触发在服务器上执行代码。

## License

[MIT](./LICENSE) © 2026 JuneT
