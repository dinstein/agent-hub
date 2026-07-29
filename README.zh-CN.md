# AgentHub

本地 Agent 服务枢纽：一份配置、一套凭据、一条治理管线，供全部 AI 客户端
（Claude Code、Cursor、Codex、Open WebUI 等）共享。

*[English documentation](README.md)*

- 单一必装二进制 `agenthub`：`connect`（stdio 网关，每 client 一进程）/
  `daemon`（HTTP 共享池 + 控制面 + 协调面）/ CLI 管理子命令
- 可选 GUI `agenthub-gui`（Wails3，仅消费控制面 API）

**状态**：功能相对设计已完整。CI 双矩阵全绿，
真实 Claude Code 经网关调用真实下游 MCP server 的端到端验收通过。
平台：macOS + Linux 已验证；Windows **尚不可用**（见下）。

## 能力

| 面 | 内容 |
|---|---|
| 网关 | stdio（每 client 一进程）+ streamable-http（daemon 共享池）；下游 transport 三种：stdio / streamable-http / legacy HTTP+SSE，协议目标版本 `2025-11-25` 带向下协商降级 |
| 发现 | `full` / `grouped` / `lazy` 三模式；lazy 模式五件套 meta-tool（`status`、`search_tools`、`describe_tool`、`call_tool`、`fetch_result`）+ 意图变体；紧凑签名文法 + 二段 describe |
| 治理 | 三层 scope 解析链（global / profile / session；client 只选一个 profile，收窄只住在 profile 上）、per-server 工具选择器三态语义、悬垂 profile fail-closed 空集 |
| 安全 | 注入扫描（归一化 + 短语/正则/base64/头尾双窗）、spawn guard（反走私）、SSRF 双向谓词 + DialContext 内筛查、leakguard、integrity 指纹 pin + drift 分级 + quarantine、HITL 审批状态机（fail-closed 全家桶） |
| 隔离 | **Docker 隔离 Spawner**：`runtime: host\|docker`，默认无网络、只挂载显式声明的目录（默认只读）、资源限额、密钥不进 argv（无网络、只读挂载、资源限额） |
| 结果整形 | 分页 / 预算 / `fetch_result` 缓存 / TOON 单向投影编码（never-larger + 数字保真两条构造性保证） |
| 凭据 | 四级解析链（env → 显式 bare env → `secrets.enc` → OS keyring）、vault 复合键 `(serverID, scopeName)`、headless OAuth 三模式回调 + 刷新协调 |
| 客户端 | 12 种客户端配置适配（Format 驱动）、skills 库/安装两层管理、skills-over-MCP 供给 |
| 运维 | `agenthub doctor` 全面体检、每 server 独立日志 + stderr 尾窗嵌入错误、四条审计流、X-Request-Id 全链路 |

## 文档

| 文件 | 内容 |
|---|---|
| [docs/architecture.md](docs/architecture.md) | **先看这个**：进程模型、核心模块地图、分层与依赖约束、一次调用穿过什么、三条数据流向、三道防线 |
| [docs/flows.md](docs/flows.md) | 七个关键流程的时序图与失败分支 |
| [docs/modules/](docs/modules/) | 逐包文档：职责、关键类型、不变量与失败方向 |
| [docs/canonical.md](docs/canonical.md) | 架构约定的唯一真源：冻结标识符、包布局、依赖约束、命令名规则、全部裁决记录 |
| [docs/windows.md](docs/windows.md) | Windows 现状：已实现什么、未验证什么、缺什么 |
| [docs/backlog.md](docs/backlog.md) | 已确认但未修复的缺口：症状、根因（指到行）、做法、验证方式 |

## 开发

需要 Go 1.26+ 与 golangci-lint v2。

```bash
make build   # go build ./...
make test    # go test ./...
make lint    # golangci-lint run
make ci      # build + test + lint
```

### GUI（可选，默认不构建）

`make build` / `make lint` **不包含 GUI**：链接 webview 需要 GTK/WebKit 开发包，
CI runner 上没有。因此 `cmd/agenthub-gui` 的 Wails 代码全部带 `//go:build wails`
标签，默认构建拿到的是一个提示如何构建 GUI 的占位 main（裁决见 [canonical.md](docs/canonical.md) §7 第 3 项）。

```bash
make gui            # 前端 npm install + vite build，然后 go build -tags wails
make gui-frontend   # 只构建前端（产物 cmd/agenthub-gui/frontend/dist，被 embed 进二进制）
make gui-go         # 只构建 Go 侧（要求 dist 已存在）
```

前端是 vanilla TS + Vite，唯一运行时依赖是 `@wailsio/runtime`；Health 契约的
Level/AdminState/Action 常量由 `go generate ./cmd/agenthub-gui/...` 从 `api` 包生成到
`frontend/src/generated/health.ts`，golden 测试盯着它防三端漂移。
GUI 只能通过 `api` 包访问控制面——它没有任何 CLI 没有的能力。

依赖方向硬约束（违反即 CI 失败，详见 [canonical.md](docs/canonical.md) §2）：

1. `cmd/agenthub-gui` 与 `api` 不得 import 任何 `internal/*`
2. `internal/mcp` 只依赖标准库；其余包不得 import 任何第三方 MCP 库
3. `internal/pipeline` 不得 import `internal/ctlapi`
4. `internal/mcp`、`internal/platform`、`internal/logx`、`internal/guard/*` 是零业务依赖底座

## 平台

| 平台 | 状态 |
|---|---|
| macOS | ✅ 支持，CI 常跑 |
| Linux | ✅ 支持，CI 常跑 |
| Windows | ⚠️ **跑不起来**：路径解析已实现，但 registry 锁与 named pipe 仍是 stub |

Windows 上 `%APPDATA%\agenthub` 数据目录、MSIX 包身份探测与 loopback-UNC 孪生路径逃逸、
控制面 named pipe 路径 `\\.\pipe\agenthub-ctl-<sha8(SID)>` 与它的 SDDL 都已实现，
接缝按裁决收敛在 `internal/platform`，CI 有 `GOOS=windows go build ./...` 门禁——
但**没有任何一行在 Windows 机器上跑过**，而且还差两块前置件：registry 的跨进程锁与控制面的
named pipe 监听器目前都是返回 unsupported 的 stub，所以 Windows 上既读不了配置也起不了 daemon。
现状、待办与验收标准见 [docs/windows.md](docs/windows.md)。

## 隐私：不收集任何数据

AgentHub **不收集任何数据**。没有遥测、没有崩溃上报、没有使用统计、没有更新检查器，
默认关闭与手动开启都不存在——这条通道根本没有实现。

进程的出站网络连接只有三类，全部由你的配置决定：

1. 你在 `servers.json` 里配置的下游 MCP server；
2. 这些 server 的 OAuth 授权服务器（仅在你执行 `agenthub auth login` 后）；
3. 你显式指定的 endpoint（例如 `server add --url`）。

不存在指向 AgentHub 自有域名的请求。审计流（`audit.jsonl` / `security.jsonl` /
`savings.jsonl`）与每 server 日志（`logs/server-<name>.log`）**只写本地磁盘**，
任何人都不会读走它们。版本更新交给你的包管理器。

裁决记录见 [canonical.md](docs/canonical.md) §7 第 6 项。

## License

MIT © 2026 dinstein（设计参考来源见 [NOTICE](NOTICE)）
