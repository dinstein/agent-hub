<div align="center">

# AgentHub

**一个 MCP server 只配一次。每个 AI 客户端都拿得到它——连同凭据，并且只带上你允许的那些工具。**

Claude Code · Cursor · Codex · Zed · Open WebUI · 以及另外 7 种 —— 一个二进制，不需要账号，没有遥测

[![CI](https://github.com/dinstein/agent-hub/actions/workflows/ci.yml/badge.svg)](https://github.com/dinstein/agent-hub/actions/workflows/ci.yml)
[![Go 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)
[![Version](https://img.shields.io/github/v/release/dinstein/agent-hub?label=version&color=blue)](https://github.com/dinstein/agent-hub/releases/latest)
[![Platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux%20%7C%20Windows%20(exp.)-lightgrey.svg)](#平台)
[![Telemetry: none](https://img.shields.io/badge/telemetry-none-brightgreen.svg)](#隐私不收集任何数据)

**[English documentation](README.md)** · [架构](docs/zh-CN/architecture.md) · [使用指南](docs/zh-CN/guide.md) · [流程](docs/flows.md)

</div>

---

## 它消掉的那些麻烦

你手上不止一个 AI 客户端。于是同一个 MCP server 被写进了四份配置文件、四种格式，其中第四份已经
过期了；同一个 API key 也在那四份文件里，轮换它意味着在整个 home 目录里做一次搜索替换；登录是逐
客户端的，前提还得是那个客户端支持 OAuth；而你启用的每个工具都出现在每个客户端的每一轮上下文里，
不管那个客户端是不是本来就该用到它。

AgentHub 就是同时持有 server、凭据和规则的那一个本地进程，并且只把你决定给它看的那一面递给每个
客户端。

```
   Claude Code ──┐                                   ┌── linear
   Cursor ───────┤      ┌──────────────────┐         ├── github
   Codex ────────┼─────►│     AgentHub     │────────►┼── postgres
   Open WebUI ───┤      │  一份配置        │         ├── filesystem
   … 另外 8 种 ──┘      │  一套凭据        │         └── …
                        │  一条治理管线    │
                        └──────────────────┘
```

|  | 没有 AgentHub | 有 AgentHub |
|---|---|---|
| 加一个 server | 每个客户端各改一遍，各按各的格式 | `agenthub server add`，一次 |
| 轮换一个 key | 先得找出它的每一份副本 | 一个 vault，键是 `(server, scope)` |
| 登录 | 逐客户端，且以它支持 OAuth 为前提 | `agenthub auth login`，headless，共用 |
| 上下文开销 | 每个 server 的每个工具，每一轮 | 五个 meta-tool，其余按需搜索 |
| 收回一个 server | 每个客户端各改一次，再各重启一次 | `agenthub server disable`，无条件 |
| 某次调用出错了 | 客户端碰巧记了什么就只有什么 | `calls` · `logs` · `events` · 逐 server 报文抓取 |

## 安装

**macOS 与 Linux，不需要包管理器**——同一条命令既是安装也是升级：

```bash
curl -fsSL https://raw.githubusercontent.com/dinstein/agent-hub/main/scripts/install.sh | sh
```

它在解包之前先用该版本锁定的 sha256 校验下载，把 `agenthub` 装到 `~/.local/bin`（`--prefix`
可改），并且只打印需要追加的 `PATH` 那一行，不去改你的 shell 配置。`--uninstall` 卸载，`--help`
列出其余选项；不习惯把脚本直接管道进 shell 的话，[脚本本身](scripts/install.sh)值得先读一遍。

**用 Homebrew**——也是拿到 macOS 应用的唯一途径：

```bash
brew tap dinstein/agenthub

brew install agenthub                 # CLI，装它就够了
brew install --cask agenthub-gui      # macOS 应用，CLI 随它一起装上
```

两条路选一条：它们都会往一个自认为归自己管的路径上放二进制，而且事后谁也发现不了对方。应用只做了
ad-hoc 签名，**没有做公证（notarization）**——cask 会清掉 macOS 给下载文件打的 quarantine 标记，
替 Gatekeeper 作保的是它锁定的 sha256。Windows 目前没有包，从
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
所以之后你再加的每个 server 都会被自动带上，不用再动那份配置。想确认它生效了，打开 Claude Code
跑一下 `/mcp`。整套模型——profile、收窄、发现模式——见
[docs/zh-CN/guide.md](docs/zh-CN/guide.md)。

想让 agent 自己来驱动这个 CLI，就把二进制自带的那份 skill 交给它：

```bash
mkdir -p ~/.claude/skills/agenthub && agenthub manual > ~/.claude/skills/agenthub/SKILL.md
```

`agenthub manual` 打印的是编进你刚装的这个二进制里的那一份，所以文档和它描述的 CLI 一定是同一个
版本。

## 它不一样在哪

- **它是个 CLI，而 GUI 没有任何它没有的能力。** 一个 Go 二进制：`connect` 是 stdio 网关（每
  client 一进程），`daemon` 是承载控制面与协调面的 HTTP 共享池，其余都是管理子命令。没有运行时要
  装，没有账号要注册，没有后台更新器。可选的 `agenthub-gui` 只是同一套控制面 API 的一个视图，凡是
  能点的都能脚本化。
- **登录一次，而不是每个客户端各登一次。** 凭据按环境变量 → 显式 bare env → 加密的 `secrets.enc`
  → OS keyring 四级解析，键是复合的 `(serverID, scopeName)`。OAuth 是 headless 的，刷新在进程间
  协调，因此四个客户端指向同一个 server 时不会为一个 token 互相踩踏，任何 key 也不会被复制进某个
  客户端的配置文件。
- **几百个工具，上下文里只有五个名字。** `lazy` 发现给客户端的是 `status`、`search_tools`、
  `describe_tool`、`call_tool`、`fetch_result`，其余全部靠一套紧凑签名文法按需查。`grouped` 和
  `full` 留给那些小到不必在意这件事的面。
- **一个客户端能够到什么，在它连上来之前就定了。** server 的工具子集、profile、client 三层取交
  集，没有哪一层能放宽，悬垂引用 fail-closed 到空集。没有任何事是在调用飞行途中决定的——没有审批
  队列，没有运行时改 scope——这正是 `agenthub server disable` 是一个无条件的总闸而不是一句建议的
  原因。
- **一个来路不明的 server，可以在没有网络、什么都没挂载的情况下跑。** `runtime: docker` 让一个
  stdio server 默认没有网络，只挂载你声明过的目录（除非另行指定，否则只读），受显式的资源限额约
  束，并且密钥不进 argv。配置声称的隔离要么兑现要么拒绝，绝不悄悄退回到宿主机上跑。

## 能力

上面五条是理由，这里是矩阵。

| 面 | 内容 |
|---|---|
| 协议 | MCP `2026-07-28`（无状态逐请求 `_meta`、`server/discover`、MRTR、`subscriptions/listen`）加上 `2025-11-25` / `2025-06-18` / `2025-03-26`，双面各自协商。只代理工具——resource、prompt 与 extension 能力都不转发（[细节](docs/status/mcp-2026-07-28.md)，英文） |
| 网关 | stdio（每 client 一进程）+ streamable-http（daemon 共享池）；下游走 stdio / streamable-http / legacy HTTP+SSE |
| 发现 | `full` / `grouped` / `lazy`；五件套 meta-tool 加意图变体，紧凑签名文法，二段式 describe |
| 安全 | spawn guard（反走私）、`DialContext` 内筛查的 SSRF 双向谓词、分级为 read/write/destructive 的 agent token、协作式调用配额 |
| 结果整形 | 分页、预算、`fetch_result` 缓存、TOON 投影编码（never-larger 与数字保真两条构造性保证） |
| 客户端 | 12 种客户端的配置适配（Format 驱动）、skills 库/安装两层管理、skills-over-MCP 供给 |
| 运维 | `doctor` 体检、`calls` 背后加密且有硬边界的调用账本、把各进程归并成一条流的 `logs`、闭集词汇的 `events`、逐 server 报文抓取、全链路 X-Request-Id |

## 文档

| 文件 | 内容 |
|---|---|
| [docs/zh-CN/guide.md](docs/zh-CN/guide.md) | **怎么用**——server / profile / client、日常路径，以及你真正要做的那几个决定 |
| [docs/zh-CN/architecture.md](docs/zh-CN/architecture.md) | **要改代码先看这个**——进程模型、模块地图、一次调用穿过什么、两道防线 |
| [docs/flows.md](docs/flows.md) | 七个运行时流程的时序图与失败分支（英文） |
| [docs/subsystems/](docs/subsystems/) | 每个接缝的不变量与失败方向（英文） |
| [docs/conventions.md](docs/conventions.md) | 冻结标识符、依赖约束、命名规则、全部裁决记录（英文） |
| [docs/status/windows.md](docs/status/windows.md) | Windows 现状：已实现什么、哪些还未验证、验收标准（英文） |

## 平台

**功能相对设计已完整。** CI 双矩阵全绿，真实 Claude Code 经网关调用真实下游 MCP server
的端到端验收通过。

| 平台 | 状态 |
|---|---|
| macOS | ✅ 支持，CI 常跑 |
| Linux | ✅ 支持，CI 常跑 |
| Windows | 🧪 **实验性**：每一项能力都已实现（`LockFileEx` 跨进程锁、带 SDDL 的 named pipe 控制面、api 拨号、`daemon stop`、`client connect`、便携 zip 打包），CI 门禁为 `GOOS=windows` build + vet，每次 release 附带两个架构的 zip，但从未在真实 Windows 机器上跑过。[详见](docs/status/windows.md) |

## 隐私：不收集任何数据

AgentHub **不收集任何数据**——没有遥测、没有崩溃上报、没有使用统计、没有更新检查器。默认关闭与
手动开启都不存在：这条通道根本没有实现，也不存在指向 AgentHub 自有域名的请求。

出站连接只有你的配置点了名的那些：`servers.json` 里的下游 server、这些 server 的 OAuth 授权服务器
（仅在你执行 `agenthub auth login` 后），以及你显式给出的 endpoint。调用账本**只写本地磁盘**，
版本更新交给你的包管理器——裁决记录见 [docs/conventions.md](docs/conventions.md) §7 第 6 项。

## 开发

需要 Go 1.26+ 与 golangci-lint v2。

```bash
make         # 列出全部 target，每个一行
make ci      # build + test + lint
make gui     # GUI 单独构建 —— 它不在默认构建里
```

贡献规则——worktree 分支流程、commit 约定，以及 CI 强制的四条依赖方向约束——都在
[AGENTS.md](AGENTS.md)（英文），其背后的裁决记录在 [docs/conventions.md](docs/conventions.md)。

## License

MIT © 2026 dinstein（设计参考来源见 [NOTICE](NOTICE)）
