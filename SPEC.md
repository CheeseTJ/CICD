# Spec：UI 改版为「CI/CD 管理平台」风格

参考图：深色 CI/CD 控制台（左侧导航 + 顶部工具条 + 统计卡 + 应用卡片网格）。

## 1. 目标

把现有的单页（顶部按钮 + 卡片网格）改造成参考图的控制台布局与视觉风格，**只做真实存在的功能**，不为对齐视觉而新增虚假入口。

## 2. 布局

```
┌────────────┬─────────────────────────────────────────┐
│  侧边导航   │  顶部工具条（搜索 / 刷新 / 添加应用）        │
│  240px     ├─────────────────────────────────────────┤
│  品牌       │  页面标题 + 副标题                        │
│  应用 (n)   │  统计卡 ×4                                │
│  设置       │  分区头（列表标题 + 筛选 + 排序）           │
│            │  应用卡片网格                             │
└────────────┴─────────────────────────────────────────┘
```

## 3. 侧边导航

| 项 | 图标 | 说明 |
|---|---|---|
| 应用 | box | 主视图，右侧显示 App 数量徽标，默认激活 |
| 设置 | gear | 展示服务配置状态（不回显任何密钥值） |

激活态：蓝色半透明底 + 蓝色描边 + 白色文字。

## 4. 顶部工具条

- 搜索框「搜索应用名称...」：实时按仓库名/描述过滤
- 刷新按钮：重新拉取 `/api/repos`
- 添加应用（主按钮，＋）：打开仓库勾选弹窗

## 5. 统计卡（4 张，数据全部来自 `/api/repos` 实时计算）

| 卡片 | 取值 |
|---|---|
| 应用 | `is_app` 数量 |
| 已同步 | 存在 `last_synced_tag` 的数量 |
| 自动同步 | `auto_sync` 为真的数量 |
| 蒲公英已关联 | `pgyer_linked` 为真的数量 |

每张：图标块 + 标签 + 大号数字 + 右侧 ›。**不使用「成功率」等无数据来源的指标。**

## 6. 应用卡片

```
┌──────────────────────────────────────────────────┐
│ [渐变图标块] 仓库名                        ⋮      │
│              描述                                │
│  ● 已同步                                        │
│  v1.1.1  →  v1.1.2                               │
│  🕐 10 分钟前同步                                 │
│  [进度条 · 仅同步中显示]                           │
│  自动同步 [开关]              [同步]              │
└──────────────────────────────────────────────────┘
```

- **图标块**：按仓库主语言映射图标，按仓库名 hash 选取蓝色系渐变
- **状态徽标**：已同步（绿）/ 待同步（灰）/ 已停止（仓库已归档，灰）
- **版本行**：左 = 蒲公英当前版本（muted），右 = GitHub 最新 tag（加粗）；未同步时左显示 `—`
- **时间行**：由后端返回的 `last_synced_at_unix` 前端换算相对时间
- **⋮ 菜单**：查看 GitHub Release / 打开蒲公英下载页 / 移除
- **同步按钮**：即「同步到蒲公英」，沿用异步任务 + 进度条
- 同步失败：卡片内仅显示「同步失败」+ 详情链接，完整错误走弹窗（悬停也可看）

## 7. 分区头

- 左侧：`应用列表` + `共 N 个应用`
- 右侧：筛选 pills（全部 / 已同步 / 未同步）+ 排序下拉（最近更新 / 名称 / 最新发布）

## 8. 设置视图

只读展示：GITHUB_TOKEN 是否配置、PGYER_API_KEY 是否配置、自动同步间隔、数据文件路径、GITHUB_OWNER。密钥一律只显示「已配置 / 未配置」。

## 9. 视觉规范

| 项 | 值 |
|---|---|
| 页面底色 | `#070b16`（右下角叠一层蓝色径向光晕） |
| 侧栏底色 | `#0a0f1e` |
| 卡片底色 | `#0d1424` |
| 描边 | `#1c2438` |
| 主色 | `#3b82f6` / 深 `#2563eb` |
| 成功绿 | `#22c55e` |
| 失败红 | `#ef4444` |
| 主文字 | `#e8ecf5` |
| 次文字 | `#8b93a8` |
| 圆角 | 卡片 14px / 按钮·输入框 10px / 图标块 12px |
| 图标 | 线性 SVG，`stroke-width: 1.8`，`currentColor` |

## 10. 后端改动（尽量小）

1. `GHRepo` 增加 `language` 字段
2. `RepoState` 增加 `last_synced_at_unix`
3. `repoView` 增加 `language`、`last_synced_at_unix`
4. `/api/health` 增加 `auto_sync_interval`、`data_file`
5. 复用现有 `/api/repos/mark` 实现「移除」

## 11. 明确不做

- 不新增无实现支撑的导航与视图（「前端站点」有 Cloudflare Pages API 数据支撑，见第 12 节）
- 不做「成功率」等无数据来源的指标
- 不做列表/网格视图切换、通知铃铛、头像等无功能装饰

## 12. 前端站点（Cloudflare Pages，只读）

数据来源：Cloudflare Pages API
（`GET /accounts/{account_id}/pages/projects` 与 `GET /accounts/{account_id}/pages/projects/{name}/deployments?per_page=6`），
后端聚合为 `GET /api/pages`，「前端站点」视图展示每个站点的最近部署：

- 状态徽标：成功 / 失败 / 构建中 / 排队中 / 已跳过（CF 的 `latest_stage.status` 归一化为 GitHub 运行态后复用「部署」视图的配色）
- meta 行：环境（生产 / 预览）· 分支 · commit 短 hash · 最后阶段名
- commit 首行信息、相对时间与耗时（`created_on` → `modified_on`）
- 最近 6 次部署的状态点；⋮ 菜单可打开站点 / Cloudflare 控制台 / 该次部署
- 统计卡：站点 / 最近成功 / 最近失败 / 构建中；支持按名称与域名搜索、按状态筛选

约束与环境变量：

- **只读**：不触发构建、不改动 Pages 配置，CF 侧的 push 自动构建保持原样
- `CLOUDFLARE_API_TOKEN`：Cloudflare API Token，权限 `Pages:Read` 即可；未配置时 `/api/pages` 返回 `configured=false`，视图给出提示而不是报错
- `CLOUDFLARE_ACCOUNT_ID`：Cloudflare 账号 ID
- `CF_PAGES_PROJECTS`：可选，逗号分隔的项目白名单，留空表示展示账号下全部 Pages 项目
- `CLOUDFLARED_CONFIG`：可选，cloudflared 配置路径（默认 `/etc/cloudflared/config.yml`，容器内只读挂载），用于平台检测

## 13. 平台徽章（检测而非固定）

卡片底部展示该对象**实际涉及**的平台徽章，当前平台集合为：蒲公英 / Cloudflare / OCI。
徽章不是按卡片类型写死的，而是按可用信号检测后贴出：

| 卡片 | 检测信号 | 判定 |
|---|---|---|
| 应用 | 状态文件里已有同步记录 / 关联信息（`last_synced_tag`、`identifier`、`pgyer_shortcut_url`） | 蒲公英 |
| 应用 | 仓库 workflow 原文 | 见下方通用规则 |
| 部署 | 仓库 workflow 原文（最多读 3 个） | 见下方通用规则 |
| 前端站点 | 它本身就是 Cloudflare Pages 项目 | Cloudflare |
| 前端站点 | 构建环境变量里的后端地址（如 `VITE_API_BASE`）命中本机 cloudflared 隧道域名 | OCI |

通用 workflow 规则（`detectPlatforms`）：

- 出现 `pgyer.com` / `蒲公英` → 蒲公英（只凭 `pgyer` 字样会误判：`music-flutter`、
  `cicd` 只是把 `PGYER_API_KEY` 传给构建，真正上传由本控制台完成）
- 出现 `wrangler` / `api.cloudflare.com` / `cloudflare/pages` → Cloudflare
- 出现 `self-hosted` → OCI，同时含 Cloudflare（这类服务的公网入口统一经 cloudflared 隧道）

检测失败（读不到 workflow、读不到 cloudflared 配置）不报错、不阻断展示，只是该卡片少一个徽章；
「设置」页展示读取到的隧道域名便于排查。
