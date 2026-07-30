# CANONICAL — 架构约定的唯一真源

本文件登记**不能随手改的东西**：冻结标识符、包布局、依赖方向、命令名规则、工程约定，
以及每一条曾被认真裁决过的事项及其理由。改这里等于改架构约定。

系统怎么运作看 [architecture.md](architecture.md)，流程时序看 [flows.md](flows.md)，
逐包细节看 [modules/](modules/)，已定位到行的欠账看 [backlog.md](backlog.md)。

---

## 1. 冻结标识符（ABI，自 v1 起不可改）

| 项 | 值 |
|---|---|
| Go module | `github.com/dinstein/agent-hub` |
| 远程仓库 | `git@github.com:dinstein/agent-hub.git` |
| 必装二进制 | `agenthub` |
| 可选 GUI 二进制 | `agenthub-gui` |
| 数据目录名 | `AgentHub`（release） / `AgentHubDev`（dev，兄弟目录） |
| env 前缀 | `AGENTHUB_*`（下游 spawn 时整体 strip） |
| 控制 socket | `<run>/ctl.sock`；Windows `\\.\pipe\agenthub-ctl-<sha8(SID)>` |

仓库名 `agent-hub` 与产品/二进制名 `agenthub` 刻意不同——仓库名**不属于**冻结标识符集合。

数据目录：macOS `~/Library/Application Support/AgentHub`、Linux `${XDG_DATA_HOME:-~/.local/share}/AgentHub`、
Windows `%APPDATA%\AgentHub`（经 MSIX 探测 + loopback-UNC 孪生路径，已实现，
**未在真实 Windows/MSIX 环境验证**——见 [windows.md](windows.md)）。

**渠道分离是二进制的性质，不是环境变量的性质。** 开发构建解析到 `AgentHubDev`，
只有显式按 release 构建的才解析到安装位置（`main.channel`，默认 `"dev"`）。
忘了声明渠道的构建拿到 **dev** 目录——猜错这个方向多一个沙箱，猜错另一个方向会花掉用户真实安装里
那个一次性的 OAuth refresh token。显式 `AGENTHUB_DATA_DIR` 仍优先于两者。

---

## 2. 包布局

```
github.com/dinstein/agent-hub
├── cmd/
│   ├── agenthub/            # 唯一必装二进制：cli / daemon / connect 三入口
│   └── agenthub-gui/        # Wails3（可选；Go 侧在 //go:build wails 之后）
├── api/                     # 控制面 DTO + Go client（GUI 与第三方唯一入口；只用标准库）
├── internal/
│   ├── mcp/                 # ★ MCP 协议唯一门面（只依赖标准库）
│   │   └── transport/       # stdio / streamablehttp / httpsse / docker
│   ├── platform/            # 数据/run 目录、socket 与 npipe 路径、Windows 包身份探测、渠道分离
│   ├── tier/                # read|write|destructive 词汇表（叶子包，只依赖标准库）
│   ├── logx/                # slog 初始化、字段规约、不可绕过的 scrubbing
│   ├── registry/            # 多文档 Doc[T]、原子写、generation、watch、自写抑制、运行标记
│   ├── scope/               # 三层解析链、Merge 纯函数、EffectiveScope（内容寻址）
│   ├── session/             # 会话身份、Overlay、只紧不松校验、TTL、SessionManager
│   ├── event/               # 进程内事件总线：合并器与 settled 去抖
│   ├── router/              # 命名空间聚合、RouteOf 唯一溯源、Provider、Catalog
│   ├── downstream/          # 连接生命周期、串行队列、断路器、重试、派生实例池、每 server 日志
│   ├── discovery/           # full/grouped/lazy、meta-tool、词法 ranker、SearchGuard、意图变体
│   │   └── toolsig/         # 紧凑签名文法
│   ├── shaping/             # 结果分页/预算/fetch_result cursor（内存与文件两种 store）
│   │   └── toonenc/         # TOON 编码（单向显示投影）
│   ├── pipeline/            # ★ 唯一执行管线：四门 + defend_and_shape + 参数自愈
│   ├── guard/
│   │   ├── injection/       # 归一化 + 短语/正则/base64/头尾双窗
│   │   ├── spawnguard/      # wrapper/解释器/env 走私与容器逃逸拦截
│   │   ├── netguard/        # SSRF 双向谓词 + DialContext 内筛查
│   │   └── leakguard/       # 敏感数据外泄检测
│   ├── integrity/           # 指纹 pin、drift 分级、quarantine、工具审批状态机
│   ├── approval/            # HITL broker + 网关侧 asker + 指纹 allowlist
│   ├── audit/               # audit / security / savings 三流 + inspect 环形
│   ├── secrets/             # 四级解析链；vault 键 (serverID, scopeName)，默认 "_global"
│   │   └── secureenv/       # 子进程环境 allowlist、登录 shell PATH 捕获
│   ├── oauthflow/           # 发现/DCR/PKCE/三模式回调 + 刷新（单飞与文件锁）
│   ├── confops/             # ★ registry 语义写的唯一实现（CLI 与控制面共用）；Precondition 乐观锁
│   ├── catalog/             # curated server 目录 + 粘贴的客户端配置解析（只产出提案，不写盘）
│   ├── ctlapi/              # 控制面 server：REST + SSE over UDS，peer-cred 鉴权
│   ├── httpbridge/          # streamable-http 暴露面、ingress 限额、agent token
│   ├── clients/             # 12 个客户端配置适配（按配置形态归类）
│   ├── skills/              # 库/安装两层、targets 表、OwnedDir/SentinelBlock、ApplyState
│   ├── gateway/             # stdio 网关装配与生命周期（connect 的实现体）
│   ├── daemon/              # daemon 装配：HTTP 面 + 协调面 + 控制面
│   ├── cli/                 # 全量命令树，仅依赖 api client + 各 internal 包
│   │   └── output/          # 人类表格与 --json 信封的同源渲染
│   ├── ratelimit/           # cooperative 配额（多进程文件锁 + 合并写）
│   ├── depguardtest/        # 证明四条依赖约束真的会拦的失败用例
│   └── testutil/fakemcp/    # 可编程 fake 下游（所有测试的地基）
├── test/e2e/                # 端到端回归：真实进程、真实 npx 下游
├── test/concurrency/        # 跨进程并发不变量
├── test/buildrules/         # 证明 Go 构建之外的那些规则和代码树没有脱节
└── go.mod
```

### 作废的旧名（早期材料里出现过，一律不要用）

| 旧名 | Canonical |
|---|---|
| `internal/controlapi`、`internal/control` | `internal/ctlapi`（DTO 与 client 在公共 `api` 包） |
| `internal/vault` | `internal/secrets` |
| `internal/secure/{integrity,injection,ssrf,audit}` | `internal/guard/*` + 平级 `internal/audit`、`internal/integrity` |
| `internal/gatewaymode` | `internal/gateway` |
| `internal/downstream/transport` | `internal/mcp/transport` |
| `package skill` | `package skills` |
| execute 管线在 `internal/gateway` 内 | 独立 `internal/pipeline`；`gateway`/`daemon` 只做装配 |
| `catalog.Snapshot`（工具目录快照） | `router.Catalog`；`internal/catalog` 专指 curated server 目录 |
| `session.ScopeOverlay` | `*scope.Overlay`（字段以 `internal/scope` 的 `ScopeLayer` 为准） |

### 依赖方向硬约束（depguard 编译期固化）

1. `cmd/agenthub-gui` 与 `api` **不得** import 任何 `internal/*`。
2. `internal/mcp` **只依赖标准库**——不得 import 任何第三方模块（裁决 #32）；
   其余 `internal/*` 不得 import 任何第三方 MCP 库。
3. `internal/pipeline` 不得 import `internal/ctlapi`（数据面不依赖控制面）。
4. `internal/mcp`、`internal/platform`、`internal/logx`、`internal/guard/*` 为零业务依赖底座。

这四条不是评审约定，是 CI 失败条件，且每条都要有能证明它真的会拦的失败用例。

### 关于第 2 条：MCP 协议门面完全自研

`internal/mcp`（含 `transport` 子包）是全仓库唯一允许触碰 MCP 协议实现的包，且**它自己只用标准库**——
不 `go get` 官方 `modelcontextprotocol/go-sdk`、`mark3labs/mcp-go` 或任何其他第三方 MCP 库。

理由：有界读（16MB）、`notifications/cancelled` 转发、反向 RPC 内联答复、stdio stderr 尾 4KB
都是要精确控制的协议层不变量；而 JSON-RPC 编解码本身工作量不大，为它绑一条外部演进节奏不划算。
门面存在的意义是让这个选择**将来可逆**——真要换实现时改动被封在一个包里，而不是现在就先借一个。

---

## 3. 命令名规则

- **资源组一律单数为规范名，复数为 cobra alias**：
  `server` / `profile` / `client` / `session` / `tool` / `skill` / `secret` / `approval` / `grant`
  （`secrets`、`approvals`、`skills` 等复数写法保留为 alias）
- **动作/流组保持原样**：`daemon`、`connect`、`auth`、`audit`、`activity`、`events`、`config`、`doctor`
- **没有 `scope` 组。** 收窄本身就是 profile 的定义，所以它归 `profile`
  （`profile server` / `profile tools` / `profile discovery`）；把一个面交出去则归 `client`
  （`client bind <client> <profile>` / `client unbind` / `client ls`）。作废的那个组，命令一一对应：
  `scope set --client X --profile P` → `client bind X P`，`scope clear --client X` → `client unbind X`，
  `scope ls` → `client ls`
- OAuth 组规范名是 **`auth`**，不是 `oauth`
- session 级 flag 统一：`--enable-server` / `--disable-server` / `--tools s:t1,t2` / `--discovery` / `--reset`
  （没有 `--persist`：session overlay 的修改一律易失，要永久生效只能改 profile）
- `client` 组：`ls | detect | inspect | connect | disconnect | bind | unbind`。`detect` 只 stat、
  `inspect` 才读文件，这条区分是全部要点（macOS TCC，见 internal/clients）；`ls` 每个客户端同时
  给出 connect 与 bind 两个答案。没有 `import`：它已被删除，客户端已有的 server 改用粘贴配置的方式接管
- `skill` 组：`add | ls | inspect | rm | enable | disable | install-to | sync | update | verify`
  （`install-to` = 单条物化，`sync` = 按 scope 批量物化，两者并存）
- 列表子命令一律 `ls`
- 每条命令都必须有 `--json`，人类输出与机器输出由同一数据结构渲染

### `add` 与 `enable` 是两个独立原语，而且要一直如此

`server add` 只写定义，**别的什么都不做**：不连接、不探测，写入的条目处于 **disabled** 状态。
把一个 server 真正投入使用的是 `server enable`，连接探测就住在那里。

两者回答的是不同的问题。`add` 记录一个 server **是什么**——纯配置，不碰网络，确定性的，即使
下游此刻恰好不可达也照样可以脚本化调用。`enable` 声明的是运维**想用**它，而这才是「我们究竟连不
连得上？」唯一值得问的时刻。把两者合并，`enabled` 就同时意味着*用户想要它*和*它通过了探测*；于此
之后，一个仅仅在 add 时正处于部署中途的下游，就和一个从来没被添加过的下游再也分辨不出来了。

有两条推论，日后不许以「简化」的名义去掉：

- **探测只汇报，绝不否决。** enable 是被明确要求的动作，它总会发生。需要登录的 server 一样会被
  enable，并把这件事说出来。否决意味着把运维明确 enable 过的条目搁置在半空，也意味着把一次临时的
  网络故障变成了一次配置变更。
- **组合是调用方的事。** `catalog add` 和 GUI 可以在这两个操作之上提供单个动作——`catalog add`
  正是这么做的，而 `auth login` 会把它刚授权完的 server 直接 enable（这也正是 OAuth 路径只要两条
  命令而不是三条的原因）。底下的原语保持分开。

---

## 4. 已知的能力边界

这些不是待办清单，是**当前实现的诚实分档**，改动相关代码时要知道自己站在哪一档：

| 项 | 现状 |
|---|---|
| Windows | `internal/platform` 的路径/包身份解析完整并有 `GOOS=windows` 交叉编译门禁，但**跑不起来也未在真机验证**：registry 的跨进程锁与控制面 named pipe 监听器都还是返回 unsupported 的 stub。见 [windows.md](windows.md) |
| GUI | 功能完整但**不参与默认构建**（webview 需要 GTK/WebKit，CI runner 没有）；`make gui` 单独构建 |
| skills 物化 | 只能到 **client 粒度**，不是 per-session——文件在 agenthub 的读取路径之外 |
| git 来源的 skills | 记录并 pin revision，但**不执行 git、不联网**；没有本地 checkout 的 update 返回类型化的 unsupported 错误，绝不谎报「已是最新」 |
| TOON | **单向显示投影，没有 decoder**（§7 第 4 项）；需要往返的数据永不进这个编码器 |
| teams | 刻意未实现；`policy` 层预留了 `Effective()`（own OR forced，只紧不松）挂点 |
| 遥测 / 更新检查器 | 已裁决不做（§7 第 6 项）——不收集任何数据 |

### 三项绝不能事后补的东西

这三项都曾差点被漏掉，而且每一项事后再加的代价都不成比例。它们已经就位，保持下去。

1. **vault 复合键** `(serverID, scopeName)`，默认 `"_global"`
   —— 事后改要动 token store / 回调 server / 刷新协调器全部单例。
2. **registry 自写抑制**：写前把 payload 登记进有界 TTL 集合，watcher 命中即忽略
   —— 缺它则每次自写触发一轮空重载。
3. **X-Request-Id 全链路**：响应头在 handler 前先写、错误体携带、audit 记录携带。

---

## 5. 参考代码策略

**两个参考实现都只读不抄。全部代码用本项目的风格重新设计实现。**

| 来源 | 许可 | 用法 |
|---|---|---|
| [smart-mcp-proxy/mcpproxy-go](https://github.com/smart-mcp-proxy/mcpproxy-go) | MIT / Go | **只作参考，不复制代码**。继承的是它踩出来的**问题清单**——哪些边界情况存在、失败长什么样、正确行为是什么；实现一律重写 |
| [tsouth89/toolport](https://github.com/tsouth89/toolport) | MIT / Rust | 跨语言，同样只作设计参考 |

本项目已有自成体系的结构约定（`internal/mcp` 协议门面、`Doc[T]` 泛型信封、每 server owner
goroutine + `calls chan` 串行化、`EffectiveScope` 内容寻址、失败方向注释），贴入外来实现会在这些
约定上撕出裂缝。参考实现的价值是问题清单；实现已按它落地，现在以代码为准。

根 `NOTICE` 记录设计参考来源（学术诚实，非许可义务）。

---

## 5b. MCP 协议范围

- **目标版本 `2025-11-25`**（当前正式版），`initialize` 声明该版本
- 对只支持 `2025-06-18` / `2025-03-26` 的下游做**向下协商降级**
- Transport 方向不对称：
  - **读取侧**（连下游）：`stdio` + `streamable-http` + **legacy HTTP+SSE**
  - **暴露侧**（daemon 对上游客户端）：只提供 `streamable-http`，不新增 SSE 暴露面

### 上游 deprecation 跟踪

全部按现状实现（最早移除均在 2027-07-28 之后），但迁移接缝提前就位。
使用点一律加 `// DEPRECATED-UPSTREAM(<feature>, earliest-removal: <date>)` 注释，便于将来一次 grep 捞全。

| 特性 | Deprecated in | 依赖点 | 迁移接缝 |
|---|---|---|---|
| Roots | `2026-07-28` | `${ROOT}`、派生实例键控（per-project 层退役后，root 已不进 scope 解析，也不再进解析器缓存键） | **M0 就位**：`RootSource` 接口，roots 协议与 `clients.json` 显式 root 各一实现 |
| Sampling | `2026-07-28` | 1.1 隔离论据之一 | 无需接缝（结论由凭据/连接参数/故障三条独立支撑） |
| DCR | `2026-07-28` | OAuth 发现链、DCR 凭据随 token 持久化 | **M1 就位**：`ClientRegistrar` 接口，DCR 与 Client ID Metadata Documents 各一实现 |
| Logging | `2026-07-28` | 下游日志转发 | 无需接缝（日志面本就自建） |
| HTTP+SSE transport | `2025-03-26` | 三 transport 之一 | 保留读取侧，不新增暴露侧 |

---

## 5c. 配置热更新链路（实现时别做错的两点）

GUI/CLI 改 profile → 对应 gateway 自动更新，链路见 [flows.md](flows.md) 第 4 节。两个必须做对的点：

1. **自写抑制**：daemon 自己写 `profiles.json` 时 fsnotify 同样报事件，不抑制就会对自己的写做一轮
   空重载。写前把 payload 登记进有界 TTL 集合（多槽位、10s 过期、写失败即撤、外部变更即清），
   watcher 命中即忽略。
2. **generation 判据**：控制连接推的 `Change{Kind, Rev}` 只是**通知**，不带快照——网关仍要自己重读
   文件。判据是**读到的 generation ≥ 已应用 generation 即采用**，不是「等于事件 Rev」；否则多次
   快速连续写时会卡在旧版本，等一个永远不会再来的事件。

---

## 5d. 协作约定

- 直接提交到 `main`，**逐个子任务一个 commit**（每个 commit 必须可编译、可测试）
- commit message 用英文
- 每个 commit 完成即推送

---

## 6. 工具链与工程约定

- **Go 1.26+**
- **LICENSE：MIT**（`Copyright (c) 2026 dinstein`）
- `cobra`（CLI）、`fsnotify`（watch）、`zalando/go-keyring`（keyring）、`log/slog`
- `golangci-lint`（**v2 配置格式**）+ **depguard**（固化第 2 节的四条依赖约束），
  以及 `gofmt` 和 `goimports` —— 这两个声明在 `formatters:` 下，与 `linters:` 是彼此独立的两节，
  但同样是 CI 的失败条件
- CI：GitHub Actions，macOS + Linux 双矩阵跑 build / test / lint

### 约定的路径

| | |
|---|---|
| fake 下游 MCP server | `internal/testutil/fakemcp` |
| 参考仓库 clone 位置 | `~/Develop/_refs/`（**仓库外**，避免污染 git 历史） |
| 每 server 独立日志 | `<data>/logs/server-<name>.log` |

### depguard 约束必须有失败用例

四条依赖约束不能只写在 `.golangci.yml` 里——每条都要有一个能证明它真的会拦的用例
（例如 `testdata/depguard/` 下的违规样例 + 一个跑 lint 并断言其失败的测试）。
配置写了但没生效的 lint 规则比没有更危险。

### 测试基建（M0 交付物）

可编程的 **fake 下游 MCP server**，能按脚本注入：慢响应与超时、半写/畸形 JSON-RPC 帧、
超大 payload（撞 16MB 有界读）、握手期崩溃、`tools/list_changed` 风暴、协议违规。

从第一天进 CI 的三类测试：

1. **golden test** —— 签名文法、搜索排序、错误文案（「确定性即契约」）
2. **跨进程并发测试** —— O_APPEND 单行写、pins/quarantine 文件锁、generation 单调
3. **daemon `kill -9` 注入测试** —— stdio 数据面不受影响、HITL 确实 fail-closed

---

## 7. 裁决记录（原「待裁决」六项，均已定案）

登记在此以免被默默跳过。**六项已全部裁决**（M2 收口时补齐了第 1 与第 5 项）：

1. ~~**lazy-connect 是否提前到 M1**（0.4 表已按「M1（待确认）」登记）~~ → **已裁决（M2）：不做。**
   保持 eager connect + 「缓存先答」快启动，`0.4` 表里的「M1（待确认）」条目作废。

   理由：
   - **原动机已被别的东西解决了**。lazy-connect 要治的是「每 client 一进程 × 每 server 一实例」的
     N×M 进程成本。真正解决这条的是 daemon 的 **streamable-http 共享池**——能走 HTTP 的客户端共用
     一套下游连接；stdio 网关本来就是给「只会 stdio」的客户端的降级路径，为它单独做一套惰性连接
     状态机是在成本最小的那一侧优化。
   - **代价落在最不该落的地方**。npx/uvx 冷缓存首启动是 10s–数分钟（`DefaultConnectTimeout` 定成
     120s 就是为此）。eager connect 把这段等待放在网关启动期——`tools/list` 由工具缓存立即回答，
     agent 完全不阻塞；lazy-connect 把它挪进**第一次 tools/call 的中途**，在 agent 回合里表现为
     一次无法解释的长挂起，还要撞客户端自己的超时。把可见的启动耗时换成不可见的调用期挂起是负优化。
   - **和 fail-closed 的门禁冲突**。7.5 审批状态机与 integrity 指纹 pin 的判据来自**活连接的
     tools/list**；惰性连接意味着一段时间内 agent 看到的是缓存工具，而这些工具的指纹本会话从未
     校验过。要么在首调时同步补校验（等于把 lazy-connect 的延迟再放大），要么放宽门禁（不可接受）。

   逃生口留着：接缝是 `downstream.Deps.Dial` + 工具缓存，真要做时改动封在
   `gateway.connectAll` 与一个 per-server「首调才连」闸门里，调用点不动。触发条件应该是实测的
   进程/内存成本，不是推导。
2. ~~**shaping 落盘缓存选型**（bbolt vs 纯文件）~~ → **已裁决：纯文件**。
   `<data>/cache/shaping/<sha256(owner)>/<cursor>.json`，原子写（同目录 temp → 0600 → fsync →
   rename）+ TTL 清扫 + 启动清扫。理由：M0–M1 全程零新依赖是既定风格；访问模式是单键点查，无查询、
   无事务、无跨键一致性要求；损坏条目只损失一个 cursor（跳过该文件即可），单文件数据库要靠恢复机制
   才能给同样的性质。详见 `internal/shaping` 包 doc
3. ~~**Wails3 版本与前端技术栈**（v3 仍 alpha/beta，需要备选方案）~~ → **已裁决（M1-G）**：
   `wails/v3 v3.0.0-alpha2.118` + **vanilla TS + Vite**（唯一前端运行时依赖 `@wailsio/runtime`）。
   备选方案不是「换框架」，而是**把对 alpha 的依赖压缩到一个文件**：
   - GUI 的 Wails 代码全部带 `//go:build wails` 标签，默认构建是占位 main
     （`cmd/agenthub-gui/main.go`，`//go:build !wails`）。CI 的 `go build ./...` 与
     `golangci-lint run` 因此完全不碰 webview 依赖——ubuntu runner 没有 GTK/WebKit 开发包，
     这不是权宜之计而是「GUI 非必须」的第二重证据。`go.mod` 里有 wails 无妨：下载不需要系统库。
   - 依赖 Wails 的**只有** `cmd/agenthub-gui/gui_main.go` 与
     `cmd/agenthub-gui/services/service_wails.go`（约 50 行装配）。服务体
     `services/hub.go`（全部 api 调用 + SSE→事件桥）**不带标签**，在 CI 上编译、vet、跑单测。
     alpha 破坏性变更时要改的是那两个文件，页面逻辑与 api 层不受影响。
   - 前端不用 `wails3 generate bindings`，而是用 `Call.ByName(<Go FQN>)` +
     `Events.On`（`frontend/src/bridge.ts`）——少一个代码生成步骤、少一处随 alpha 漂移的产物。
   - 构建入口：`make gui`（= `gui-frontend` + `gui-go`），不进 `make build`/`ci`。
   - Health 的 Level/AdminState/Action 常量由 `go generate ./cmd/agenthub-gui/...`
     从 `api` 包源码解析生成到 `frontend/src/generated/health.ts`，golden 测试断言
     「生成结果 == 已提交文件 == Go 常量」，防 7.4 说的三端漂移。
4. ~~**TOON 文法范围与 golden 用例集**（无现成 Go 库，需自研）~~ → **已裁决（M1.5）**。
   两处「确定性即契约」的文法一并冻结，各自配 golden 语料：

   **(a) TOON（`internal/shaping/toonenc`）是单向投影，不做 round-trip、不提供 decoder。**
   编码只供 LLM 阅读；要往返的东西（`structuredContent`、工具参数、cursor）一律留在 JSON，
   永远不进这个包。理由：round-trip 需要给每个标量打类型标记（裸 `1` 与 `"1"`、裸 `true` 与
   `"true"` 否则不可区分），而那些标记恰好等于这层编码想省下的 token；契约改为**带内声明**——
   首个被重编码的 text block 前置一行 `#toon/1 (display encoding; send tool arguments as JSON)`。

   文法范围：标量原样（数字用 `json.Decoder.UseNumber` 走字面量，绝不经过 float64）；对象
   `key: value` 缩进块，键按字节序排序（Go 解码后 JSON 成员序已丢失，排序是唯一确定性选择）；
   数组 `- ` 前缀；**同构对象数组用表格** `key[N]{c1,c2}:` + 逗号分隔行（判据：≥2 元素、全为
   非空对象、键集完全相同、值全为标量、列数 ≤32，任一不满足退化为 `- ` 列表）；空对象 `{}`、
   空数组 `[]`；字符串仅在「空 / 首尾空白 / 含 `,` `:` `"` `\` `#` 或控制字符 / 以 `[` `{` `- `
   开头 / 会被读成数字或 true/false/null」时加引号（`strconv.Quote`）；键额外在含内部空白时加引号；
   无注释、无锚点、无引用。深度超过 12 层退化为单行 compact JSON。

   两条构造性保证：**never-larger**（`Consider` 不达 `MinSavingsPct`（默认 10%）即原样返回，
   调用方无需自己判大小）与**数字保真**。预算截断按整行切，末行是冻结的
   `…truncated by agenthub: %d of %d lines`。

   golden 语料：`internal/shaping/toonenc/testdata/*.toon` —— 标量（含 2^53+1、30 位整数）、
   嵌套、表格（对象内/根位置/被拒的三种退化）、全部引号触发条件、列表（含嵌套与混合）、
   根标量、header、预算截断、非默认缩进。

   接入：`shaping.Options.Format`（`shaping.ParseFormat` 映射 governance `result_format:
   json|toon`，**默认 json**，无法识别的值一律回落 json）。`shaping.ShapeResult` 是新的完整
   入口：**先重编码后分预算**（预算花在更便宜的表示上；留存余量与 agent 看到的文本同一种记法），
   trailer 由截断步骤追加因而**永远最后**。只重写 text block，`structuredContent` 绝不重编码。
   顺序不变量：重编码在交付路径上，即在 pipeline 的注入/泄漏扫描**之后**（7.6 的
   「leakguard 扫描的是编码前文本」由此结构性成立）。

   **(b) 紧凑签名（`internal/discovery/toolsig`）文法**：
   `name(p1:str, p2?:int=3, p3?~:obj{a,b}) -> str`。`?`=可选、`~`=有损、`(~)`=schema 不可解析。
   `?` 标**可选**参数（可选参数是少数，标记更少、行更短），`~` 标有损。类型缩写 `str/int/num/bool/null/obj/arr/any`、`obj{k,k}`（顶层展开一层，
   更深折叠为 `obj` 并置 `~`）、`arr<T>`、`enum{a|b}`（超 6 个截断并置 `~`）。参数序 = **required
   数组原序 → 其余按字节序**；超长预算（默认 200B）**从尾部丢弃**（因而必填优先）并以 `…+N more`
   收尾，工具名永不截断。`$ref` 不解析（router 已内联），残留者渲染为 `any~`。
   golden：`internal/discovery/toolsig/testdata/signatures.golden`。

   describe 二段：新 meta-tool `describe_tool{tool | tools:[≤5]}`，lazy 模式与既有四件套并列
   **成五件套**（冻结序：`status, search_tools, describe_tool, call_tool, fetch_result`）。
   搜索命中项不再带 schema，改带 `sig` + `lossy`。describe 的可见性谓词就是 `Surface.byExposed`
   ——与 search/tools\_list/call 同一个集合，结构上不可能更宽；**只发一种逐 id 错误 `not_found`**
   （不存在 / scope 外 / quarantined / disabled 同文案，防探测，与 `fetch_result` 同规则）。
5. ~~**macOS keychain ACL 与开发期未签名二进制**的体验方案（dev 模式默认走 `secrets.enc`？）~~
   → **已裁决（M1，实现见 `internal/secrets` 包 doc）：是，dev 模式自动落 `secrets.enc`。**
   开发期每次 `go build` 都产生一个新的未签名二进制，macOS keychain ACL 因此每次重新弹窗。
   裁决：**keyring 可用性探测失败**、或显式 `AGENTHUB_DEV_SECRETS=1` 时，写入自动回落到
   `secrets.enc`，密钥自动生成 32 字节并持久化在旁边（`secrets.enc.key`，0600）。

   诚实分档写进包注释：**把密钥放在密文旁边是混淆不是静态加密**——能读到两个文件的人就有明文。
   这只对 dev 回落成立；生产路径走 `AGENTHUB_SECRET_KEY` 或 OS keyring。
   探测本身按 7.11 加固：**用读不用写**（`Set` 探测会触发 macOS 的破坏性确认弹窗）、
   结果按进程缓存、每操作硬超时（卡死的钥匙串弹窗否则会挂住调用方）。
6. ~~**遥测与更新检查器**是否做、默认开关如何设~~ → **已裁决：都不做**。
   AgentHub **不收集任何数据**：没有遥测（不做 enum-only 版本化上报，也没有 opt-in 开关），
   没有更新检查器（不做渠道探测、不做启动期网络请求）。7.11「可选件」里的这两项从 M2 计划中移除。

   理由：
   - 这个进程手里握着全部下游凭据、全部工具调用参数与结果、以及用户的项目路径。
     一条「只发 enum、绝不发自由文本」的上报通道要靠 `ScanForPII` 测试门禁来维持它的承诺——
     承诺越强，维持成本越高，而**根本不开这条通道**的成本是零且不可能退化。
   - 「不收集任何数据」是可以一句话讲清、可以被用户当场验证（抓包为空）的性质；
     「我们只收集匿名 enum」不是。安全产品的信任预算花在这里最划算。
   - 更新检查器要么在启动路径上加一次网络往返（stdio 网关每个 client 一进程，代价按进程数放大），
     要么需要一个常驻探测器——两者都在为一个包管理器已经解决的问题重新造轮子。
     发行走 Homebrew / 包管理器 / GitHub Releases，版本比对交给它们。

   实现约束（等价于一条 CI 可查的性质）：`internal/*` 里**不存在**任何指向 agenthub 自有域名或
   版本清单的出站请求；网络出口只有三类——下游 MCP server、OAuth 授权服务器、用户显式配置的
   endpoint。新增第四类即为违反本裁决。
