<div align="center">

# AgentHub

**一份配置、一套凭据、一个聚合点，供全部 AI 客户端共享。**

Claude Code · Cursor · Codex · Open WebUI · 以及另外 8 种

[![CI](https://github.com/dinstein/agent-hub/actions/workflows/ci.yml/badge.svg)](https://github.com/dinstein/agent-hub/actions/workflows/ci.yml)
[![Go 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)
[![Version](https://img.shields.io/badge/version-0.32.0-blue.svg)](VERSION)
[![Platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux%20%7C%20Windows%20(exp.)-lightgrey.svg)](#平台)
[![Telemetry: none](https://img.shields.io/badge/telemetry-none-brightgreen.svg)](#隐私不收集任何数据)

**[English documentation](README.md)** · [架构](docs/zh-CN/architecture.md) · [使用指南](docs/zh-CN/guide.md) · [流程](docs/flows.md)

</div>

---

每个 AI 客户端都想有自己那份 MCP server 清单、自己那份 API key、以及自己那套「工具能干什么」的
判断。AgentHub 就是同时持有这三样的那一个地方，并且只把你决定给它看的那一面递给每个客户端。

```
   Claude Code ──┐                                   ┌── linear
   Cursor ───────┤      ┌──────────────────┐         ├── github
   Codex ────────┼─────►│     AgentHub     │────────►┼── postgres
   Open WebUI ───┤      └──────────────────┘         ├── filesystem
   … +8 ─────────┘                                   └── …

                一份配置 · 一套凭据 · 一条治理管线
```

- **单一必装二进制 `agenthub`** —— `connect`（stdio 网关，每 client 一进程）、
  `daemon`（HTTP 共享池 + 控制面 + 协调面），以及 CLI 管理子命令
- **可选 GUI `agenthub-gui`** —— Wails3，仅消费控制面 API；它没有任何 CLI 没有的能力

> **状态：功能相对设计已完整。** CI 双矩阵全绿，真实 Claude Code 经网关调用真实下游 MCP server
> 的端到端验收通过。macOS + Linux 已验证；Windows 为**实验性**——平台层已补齐，
> 尚有两个命令未实现，且从未在真实机器上跑过（[详见](#平台)）。

## 安装

**CLI，macOS 与 Linux，不需要包管理器**——同一条命令既是安装也是升级：

```bash
curl -fsSL https://raw.githubusercontent.com/dinstein/agent-hub/main/scripts/install.sh | sh
```

`brew` 自己要跑 `git`，因此需要 Xcode Command Line Tools；这条路只用系统自带的东西。
它把 `agenthub` 装到 `~/.local/bin`（`--prefix` 可改），解包之前先用该版本锁定的 sha256
校验下载，并且只打印需要追加的 `PATH` 那一行，不去改你的 shell 配置。`--uninstall` 卸载，
`--help` 列出其余选项；不习惯把脚本直接管道进 shell 的话，[脚本本身](scripts/install.sh)
值得先读一遍。

**用 Homebrew**——也是拿到 macOS 应用的唯一途径：

```bash
brew tap dinstein/agenthub

brew install agenthub                 # CLI，装它就够了
brew install --cask agenthub-gui      # macOS 应用，CLI 随它一起装上
```

两条路选一条：它们都会往一个自认为归自己管的路径上放二进制，而且事后谁也发现不了对方。
cask 装的是 `AgentHub.app`，并且依赖上面那个 formula，所以走这条路 `$PATH` 上的
`agenthub` 同样只有一个归属。应用只做了 ad-hoc 签名，**没有做公证（notarization）**：
cask 会清掉 macOS 给下载文件打的 quarantine 标记，替 Gatekeeper 作保的是它锁定的
sha256——脚本那边的校验和是同一件事，也不比它更强，因为两者都随它们所描述的那个 release
一起发布。Windows 目前没有包，从
[Releases](https://github.com/dinstein/agent-hub/releases) 取 `.zip`。

## 快速开始

```bash
# 1. 注册一个下游 MCP server —— 只写下来，此时还是关着的
agenthub server add linear --url https://mcp.linear.app/mcp

# 2. 打开它；这一步会先连一次，并报出它还缺什么
agenthub server enable linear

# 3. 如果第 2 步要求登录，就登录（登录本身也会把 server 打开）
agenthub auth login linear

# 4. 接一个客户端 —— 一辈子只做一次
agenthub client connect claude-code --dry-run   # 先看一眼
agenthub client connect claude-code
```

第 4 步**每个客户端只做一次**：它写进去的条目跑的是 `agenthub connect --client claude-code`，
所以之后你再加的每个 server 都会被自动带上，不用再动客户端的配置。
完整走法（profile、收窄、整套模型）见 [docs/guide.md](docs/guide.md)。

## 能力

| 面 | 内容 |
|---|---|
| 协议 | MCP `2026-07-28`（无状态逐请求 `_meta`、`server/discover`、MRTR、`subscriptions/listen`）加上有状态各版本 `2025-11-25` / `2025-06-18` / `2025-03-26`，**双面都支持**：对下游，`server/discover` 协商双方都支持的最高版本、失败回退 `initialize`；对上游，网关按每个 client 讲的那一代作答。只代理工具——resource 和 prompt 不代理，extension 能力不转发（fail closed）。细节见 [docs/mcp-2026-07-28.md](docs/mcp-2026-07-28.md)（英文） |
| 网关 | stdio（每 client 一进程）+ streamable-http（daemon 共享池）；下游 transport 三种：stdio / streamable-http / legacy HTTP+SSE |
| 发现 | `full` / `grouped` / `lazy` 三模式；lazy 模式五件套 meta-tool（`status`、`search_tools`、`describe_tool`、`call_tool`、`fetch_result`）+ 意图变体；紧凑签名文法 + 二段 describe |
| 访问控制 | 事先决定，永不在调用时决定：server 开或关、提供全部工具或指定子集；profile 取 server 的子集并可进一步收窄其工具；client 绑定 profile。各层取交集，任何一层都不能放宽；悬垂 profile 引用 fail-closed 到空集 |
| 安全 | spawn guard（反走私）、SSRF 双向谓词 + DialContext 内筛查、HTTP 面的 agent token 分级（read/write/destructive）、协作式调用配额。它们拒绝的是目的地和进程，与谁发起无关——没有一项会检查下游返回了什么 |
| 隔离 | **Docker 隔离 Spawner**：`runtime: host\|docker`，默认无网络、只挂载显式声明的目录（默认只读）、资源限额、密钥不进 argv（无网络、只读挂载、资源限额） |
| 结果整形 | 分页 / 预算 / `fetch_result` 缓存 / TOON 单向投影编码（never-larger + 数字保真两条构造性保证） |
| 凭据 | 四级解析链（env → 显式 bare env → `secrets.enc` → OS keyring）、vault 复合键 `(serverID, scopeName)`、headless OAuth 三模式回调 + 刷新协调 |
| 客户端 | 12 种客户端配置适配（Format 驱动）、skills 库/安装两层管理、skills-over-MCP 供给 |
| 运维 | `agenthub doctor` 全面体检、`agenthub calls` 背后加密且有硬边界的 tools/call 历史、`agenthub logs` 把各进程日志归并成一条时间线、`agenthub events` 以闭集词汇记录 server/gateway/daemon 的状态变更、每 server 的 JSON-RPC 报文抓取（默认关，`server trace`）、X-Request-Id 全链路 |

## 文档

| 文件 | 内容 |
|---|---|
| [docs/zh-CN/guide.md](docs/zh-CN/guide.md) | **怎么用**：三个名词（server / profile / client）、日常路径，以及你真正要做的那几个决定 |
| [docs/zh-CN/architecture.md](docs/zh-CN/architecture.md) | **要改代码先看这个**：进程模型、核心模块地图、分层与依赖约束、一次调用穿过什么、三条数据流向、两道防线 |
| [docs/flows.md](docs/flows.md) | 六个关键流程的时序图与失败分支（英文） |
| [docs/modules/](docs/modules/) | 逐包文档：职责、关键类型、不变量与失败方向（英文） |
| [docs/canonical.md](docs/canonical.md) | 架构约定的唯一真源：冻结标识符、依赖约束、命令名规则、全部裁决记录（英文） |
| [docs/windows.md](docs/windows.md) | Windows 现状：已实现什么、还有什么完全不能用、验收标准（英文） |

中文只覆盖上面两篇——讲产品怎么用、怎么切分的那一层。其余几篇跟着代码一起变，一份中文镜像
就是每次改行为都要记得同步的第二个文件，而忘掉同步的那一份看上去和最新的一模一样。

## 平台

| 平台 | 状态 |
|---|---|
| macOS | ✅ 支持，CI 常跑 |
| Linux | ✅ 支持，CI 常跑 |
| Windows | 🧪 **实验性**：平台层已补齐（`LockFileEx` 跨进程锁、带 SDDL 的 named pipe 控制面、api 拨号、便携 zip 打包），CI 门禁为 `GOOS=windows` build + vet，每次 release 附带两个架构的 zip。仍有两处不能用 —— `daemon stop` 和 `client connect` 的用户级路径；且从未在真实 Windows 机器上跑过。[详见](docs/windows.md) |

## 隐私：不收集任何数据

AgentHub **不收集任何数据**。没有遥测、没有崩溃上报、没有使用统计、没有更新检查器，
默认关闭与手动开启都不存在——这条通道根本没有实现。不存在指向 AgentHub 自有域名的请求。

进程的出站连接只有三类，全部由你的配置决定：你在 `servers.json` 里配置的下游 MCP server、
这些 server 的 OAuth 授权服务器（仅在你执行 `agenthub auth login` 后），以及你显式指定的
endpoint（例如 `server add --url`）。

调用账本——生命周期记录、开了 trace 的 server 的帧，以及两者的加密内容——**只写本地磁盘**。
版本更新交给你的包管理器。裁决记录见 [canonical.md](docs/canonical.md) §7 第 6 项。

## 开发

需要 Go 1.26+ 与 golangci-lint v2。

```bash
make         # 列出全部 target，每个一行
make build   # go build ./...
make test    # go test ./...
make lint    # golangci-lint run
make ci      # build + test + lint
make gui     # GUI 单独构建（见下）
```

GUI **不在默认构建里**：链接 webview 需要 CI runner 上没有的 GTK/WebKit 开发包，所以 Wails
代码全部带 `//go:build wails` 标签，默认构建拿到的是占位 main；`make gui-frontend` /
`make gui-go` 分别构建前后两半。前端是 vanilla TS + Vite，唯一运行时依赖是 `@wailsio/runtime`，
且只能通过 `api` 包访问控制面——它没有任何 CLI 没有的能力。

四条依赖方向硬约束由 CI 保证（详见 [canonical.md](docs/canonical.md) §2）：`cmd/agenthub-gui`
与 `api` 不得 import `internal/*`；`internal/mcp` 只依赖标准库、且是唯一的 MCP 协议门面；
`internal/pipeline` 不得 import `internal/ctlapi`；`internal/mcp`、`internal/platform`、
`internal/logx`、`internal/guard/*` 保持零业务依赖。

## License

MIT © 2026 dinstein（设计参考来源见 [NOTICE](NOTICE)）
