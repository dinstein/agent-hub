# OAuth / 授权互操作性

`internal/oauthflow` 是 agenthub 作为 **OAuth 客户端**去认证下游 MCP server 的实现。
本文回答两个问题：**我们对着哪些规范写的**，以及**哪些真实部署形态能跑通、哪些跑不通**。

包的内部结构（不变量、失败方向、两个 `http.Client` 的分工）在
[security.md](security.md) 的 oauthflow 一节，这里不重复。

## 规范基线

我们声明的 MCP 协议版本是 `2025-11-25`（`internal/oauthflow/client.go` 的
`mcpProtocolVersion`，canonical.md 5b）。该修订版的授权章节相对 `2025-06-18` 有实质改动，
下面的差距表按它对齐。

| 规范 | 用在哪 | 我们的状态 |
|---|---|---|
| OAuth 2.1 draft-13 | 整体流程、PKCE、HTTPS、redirect URI | ✅ |
| RFC 8414 AS Metadata | `MetadataCandidates`，5 种候选形态 | ✅ 顺序即契约，golden test |
| OIDC Discovery 1.0 | `openid-configuration` 两种形态 | ✅ 与 8414 同一候选链 |
| RFC 9728 Protected Resource Metadata | `ProtectedResourceCandidates`、`WWW-Authenticate` | ✅ 含 origin 根兜底 |
| RFC 8707 Resource Indicators | `resource` 参数，authorize + token 双发 | ✅ `canonicalResource` |
| RFC 7636 PKCE | S256 强制 | ✅ 仅 S256，`plain` 直接拒 |
| RFC 7591 DCR | `NewDCRRegistrar` | ✅ 但上游已降级为 MAY |
| RFC 8628 Device Flow | `StartDevice` / `DevicePoller` | ✅ 非 MCP 要求，我们额外支持 |
| RFC 6750 §3 `scope` 挑战 | 401 里的 `scope` 参数 | ✅ 优先级第 1 级 |
| draft-ietf-oauth-client-id-metadata-document-00 (CIMD) | `NewClientIDMetadataRegistrar` | ⚠️ 只有缝，`Register` 返回 `ErrNotImplemented` |

## 支持的部署形态

每一行都是一种真实存在的 provider 行为，不是假想的排列组合。

### 授权服务器发现

| 形态 | 支持 | 机制 |
|---|---|---|
| 401 的 `WWW-Authenticate` 带 `resource_metadata` | ✅ | `ProbeChallenge` 主动探一次，同时取回 `scope`；指针经 SSRF screen 后**优先**于候选列表 |
| 401 不带 `resource_metadata`（仅 `realm` / `error`） | ✅ | 回落到 RFC 9728 候选列表 |
| PRM 在路径插入位（`/.well-known/oauth-protected-resource/a/b`） | ✅ | 候选 1 |
| PRM 在路径追加位（`/a/b/.well-known/oauth-protected-resource`） | ✅ | 候选 2 |
| PRM 在 origin 根（resource identifier 是裸 origin） | ✅ | 候选 3，`e8cbb28` 修的回归 |
| **完全没有 PRM，但 AS metadata 挂在 RS 自己 origin 上** | ✅ | `fetchResourceOriginMetadata`，`8d58c9f`。真实遇到过：per-resource 的 `authorization_endpoint` 只发布在 RS 域名上，issuer 域名那份是通用默认值 |
| issuer 带路径，metadata 在插入位 | ✅ | `MetadataCandidates` 候选 1/2 |
| issuer 带路径，metadata 在追加位（只实现 OIDC 的老 provider） | ✅ | 候选 3 |
| 什么 metadata 都没有，但 `/authorize` `/token` `/register` 真的在 issuer 下 | ✅ | `DefaultEndpoints` 合成兜底，记为 `DiscoveryDefaults` |
| PRM 列多个 `authorization_servers` | ⚠️ | **只取第一个**。刻意为之：逐个尝试会放大恶意 RS 能让我们联系的主机数 |
| AS metadata 声明的 `issuer` 与取到它的位置不一致 | ⚠️ | 不校验，但记为 `DiscoveryResourceOrigin`，见下面的安全边界 |

### 客户端身份

按 2025-11-25 的优先级顺序（pre-registration → CIMD → DCR → 手工输入）：

| 形态 | 支持 | 机制 |
|---|---|---|
| 操作员预配 `client_id` (+ secret) | ✅ | `NewStaticRegistrar`，`--client-id` |
| CIMD（https URL 当 `client_id`） | ❌ | 有缝无实现。需要 agenthub 先有稳定 https 托管点 |
| RFC 7591 DCR | ✅ | `NewDCRRegistrar`，默认路径 |
| 复用已注册的 `client_id` | ✅ | `State.ClientID`，避免每次登录都注册 |

**这是当前最大的规范差距**：2025-11-25 把 DCR 从 SHOULD 降到 **MAY**（「included for
backwards compatibility」），把 CIMD 升到 **SHOULD**。我们的默认路径正好是被降级的那个。
对只实现新 spec、不开 DCR 的 AS，我们目前只能靠 `--client-id` 预配。

### 交互模式

| 形态 | 支持 |
|---|---|
| loopback + 随机端口 | ✅ 默认 |
| loopback + 固定 redirect URI（预注册 client 的 allowlist 要求逐字节匹配） | ✅ `--redirect-uri` |
| 手工粘贴回调（无浏览器主机） | ✅ `ModeManual` |
| Device flow (RFC 8628) | ✅ 自动选择：AS 广告了 `device_authorization_endpoint` 就用 |
| 浏览器打开失败自动降级到手工 | ✅ 只在 `ModeAuto` |

**有两个调用方在驱动这个流程，而流程只有一份实现。** `agenthub auth login` 在前台跑它；控制面把它当
成一个会话跑给图形前端用（`internal/oauthlogin`，见
[controlplane.md](controlplane.md#面上唯一的长流程交换交互式登录)）。唯一的行为差异在
`LoginRequest.Open`：CLI 在那里打开浏览器，而会话路径**记下 URL 并返回成功**，让调用方去打开——
daemon 可能是无头的，也可能不是用户所在的那台机器。

这个反转有一个从代码上看不出来的后果，值得点明：会话路径上 `Paste` 是 nil，所以 `SelectMode` 永远
选不到 `ModeManual`，上表最后一行那个「loopback 自动降级到手工」**不可能触发**。手工模式要从终端读
回粘贴的 callback，而 HTTP API 后面没有终端。真的开不了浏览器的宿主，要回落到 CLI，而不是回落到一个
会永远等一个没人能粘贴的输入的模式。

### Token 处理

| 形态 | 支持 |
|---|---|
| `resource` 参数绑定 audience | ✅ authorize 与 token 都发，无论 AS 是否声明支持 |
| refresh token 轮换 | ✅ |
| refresh 并发单飞 | ✅ `Coordinator` + `Group[T]` |
| `expires_in` 是字符串（非规范但常见） | ✅ 容错 |
| 省略 `scope` 表示「不变」(RFC 6749 §5.1) | ✅ 不会误判为降权 |
| 401/403 触发刷新一次 | ✅ 403 也算，多个 provider 用 403 表达 token 过期 |

## Scope 选择

登录请求哪些 scope，按 spec 的 Scope Selection Strategy 三级取第一个命中的：

| 优先级 | 来源 | 说明 |
|---|---|---|
| 0 | **操作员的 `--scopes` / `oauth.scopes`** | 非空则原样发送，**完全覆盖**下面三级 |
| 1 | 401 `WWW-Authenticate` 的 `scope` 参数 | spec 称其对当前请求「authoritative」 |
| 2 | **PRM** 的 `scopes_supported` | 资源服务器自己声明的最小集 |
| 3 | 都没有 → **不发 `scope` 参数** | |

两个刻意的裁决，都有 mutation test 钉死：

**显式配置不与发现结果合并。** `--scopes` 最常见的用途是**收窄** provider 的默认值
（对着一台 metadata 里同时广告 write 的 server 只要只读 token）。把发现到的集合并回去，
会正好放大操作员坐下来专门限制的那个授权。

**第 3 级不回退到 AS metadata 的 `scopes_supported`。** 两份文档回答的是不同问题：
PRM 说「访问**我**需要什么」，AS metadata 说「我**总共**能发什么」。
无 PRM 时拿 AS 那份兜底，等于替一台从没要求过这些权限的资源服务器，
把 provider 提供的一切（含 write / admin）全要一遍。不发才是 fail-closed 方向，
而且与「scope 发现存在之前」的行为一致——今天能用的 provider 明天照样能用。

真实环境的验证：grafana 有 PRM，自动请求 `profile email openid`；
server-a / server-b 无 PRM，不发 scope，**没有**拿到 AS 广告的 `[... read write]`。

## 已知差距

按影响排序。这些是真实的不合规，不是「设计选择」。

### 1. CIMD 未实现（spec SHOULD）

见上。需要先决定 agenthub 的客户端元数据文档托管在哪。

### 2. 403 `insufficient_scope` 不做 step-up

`ShouldRefreshOnStatus` 把 403 当作「刷新一次」，但 spec 要求的是解析
`error="insufficient_scope"` + `scope=...` 然后带更大的 scope 集重新授权。
拿同样的 scope 去刷新，对 `insufficient_scope` 是必然再失败一次。

### 3. PKCE 缺失元数据时 fail-open，与 2025-11-25 相反

`SupportsS256`（[pkce.go:61](../../../internal/oauthflow/pkce.go#L61)）在
`code_challenge_methods_supported` 缺失时返回 `true`。注释写明了理由（省略很常见，
RFC 7636 要求服务端支持 S256）。但 2025-11-25 明确反过来：

> If `code_challenge_methods_supported` is absent, the authorization server does not
> support PKCE and MCP clients **MUST** refuse to proceed.

这是**故意保留的偏离**，但必须记在这里而不是只写在代码注释里：改成 fail-closed 会
挡掉一批现存 provider，属于要单独评估的兼容性决策。

### 4. 多个 `authorization_servers` 只取第一个

RFC 9728 §7.6 说选择责任在客户端。我们的策略是「取第一个」，理由是限制恶意 RS 的
横向探测面。真实世界里列多个的很少见；如果遇到第一个不可用而后面可用的部署，
现在只能用 `--issuer` 手工指定。

## 凭据生命周期

保险库按 **server id** 索引（`agenthub/v1/<serverID>/<scope>/<key>`），一台 server 可能有：

| 条目 | 内容 |
|---|---|
| `__oauth_state__` | OAuth state JSON：refresh token、client_id/secret、token_endpoint |
| `__http_auth__` | access token（OAuth 与手工粘贴的 token 共用这个槽） |
| 任意自定义 key | `secret set <server> <KEY>` 存的，可能在非默认 scope 下 |

删除路径：

| 命令 | 效果 |
|---|---|
| `auth logout <id>` | 只删 `__oauth_state__` + `__http_auth__`，注册表条目保留 |
| `server rm <id>` | 删注册表条目**并清掉该 server 的全部凭据**（跨 scope、跨 key），连同它其余的痕迹 —— 见 `confops.RemoveServer` |
| `server disable <id>` | 条目和凭据都保留，只是这台 server 不再被使用 |

没有 `--keep-credentials`。删掉一台 server 就意味着删掉它被授予的一切；
想让定义消失但保住 token，那描述的是 `server disable`。

**默认清理是有意的**，两个后果各修一半：

1. refresh token 通常比 access token 活得久得多。只删条目会把它留在钥匙串里，
   而注册表里已经没有任何东西提示它的存在。
2. 更隐蔽的一条：因为按 id 索引，`server rm foo` 之后再 `server add foo`
   （哪怕指向完全不同的 URL、完全不同的 provider）会**静默复用**旧凭据，不重新登录。

**失败方向**：先提交注册表删除，清理失败只报 warning，不失败整个操作。
反过来做都更糟——先清理会在 precondition 失败时销毁一个根本没发生的删除所对应的凭据；
而把钥匙串错误升级成操作失败，会让「钥匙串锁着」变成「这台 server 删不掉」。
server 无论如何已经删了，warning 里点名哪些残留 + 让操作员用 `auth logout` 收尾。

清理是 scope-blind 的（走 `List` 全量筛 `ServerID`），不是只删两个 well-known key ——
否则非默认 scope 下的凭据会被漏掉，而那正好是上面同名复活路径的燃料。

**唯一清不掉的情况改为报警。** 只要设了 `AGENTHUB_SECRET_KEY`，`secret set` 就写进
`secrets.enc`；而 `List` 只有在同一个 key 在进程里时才看得见这个文件。于是在没有该变量的
shell 里删 server，会枚举不到任何东西、删不掉任何东西 —— 在 `secrets.Chain.HasUnreadableEnc`
出现之前，这条路径会在一个存活的 refresh token 之上报告「干净删除」，正好喂给上面那条复活路径。
这个判断把「什么都没存」和「可能存了但我看不见」区分开，purge 现在会报 warning 并指明收尾方式
（带着 key 重跑，或 `auth logout <id>`）。它在任何存疑时都答 TRUE：
误报一次 warning 的代价，远小于静默留下一份凭据。

## 安全边界（不要顺手放宽）

- **`overlay` 永不落盘**同理，`--authorization-endpoint` 的 pin 是 **fail-closed** 的：
  pin 了但 URL 不合法/被 SSRF 拦，直接中止登录，不回落到发现值。
  静默换一个 endpoint 授权是更坏的意外。
- **一切从网络拿到的 URL 都过 `checkURL`**，包括 401 里的 `resource_metadata` 指针
  （它来自未认证响应，是 attacker-influenceable 的提示，不是指令）。
- **`DiscoveryResourceOrigin` 不等于 `DiscoveryOK`**：从 RS origin 取到的 AS metadata，
  其 `issuer` 没有被来源位置验证过。这正是 mix-up 攻击的形状，所以它有独立状态值，
  一次失败的诊断结论完全不同。
- **resource-origin 兜底不合成端点**：那一跳刻意不调 `DefaultEndpoints`。在 RS 自己
  origin 上猜 `/authorize` 会把用户浏览器送到没有任何 provider 声明过的 URL。
  这条有 mutation-test 钉死（`TestDiscoverFromResourceOriginDoesNotSynthesize`）。

## 排障：先看 `DiscoveryStatus`

失败时第一件事是看 `FlowError.Discovery`，它决定后续错误该怎么读：

| 状态 | 含义 | 「DCR 403」在这个状态下意味着 |
|---|---|---|
| `ok` | 元数据来自 issuer 下的正规位置 | provider 真的拒绝了 DCR |
| `protected_resource_metadata` | 9728 跳成功，8414 跳失败 | — |
| `resource_origin_metadata` | 元数据来自 RS 自己 origin，`issuer` 未经位置验证 | 端点可能属于另一个部署 |
| `fell_back_to_default_endpoints` | 端点是猜的，没有元数据文档 | `/register` 可能根本不存在 |
| `pinned_authorization_endpoint` | authorize 地址是操作员给的，不是 provider 广告的 | 同意页 400 要先怀疑 pin |
| `failed` | 什么都没发现 | — |

## 参考

- [MCP 2025-11-25 Authorization](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization)
- [RFC 8414](https://datatracker.ietf.org/doc/html/rfc8414) /
  [RFC 9728](https://datatracker.ietf.org/doc/html/rfc9728) /
  [RFC 8707](https://www.rfc-editor.org/rfc/rfc8707.html)
- [CIMD draft-00](https://datatracker.ietf.org/doc/html/draft-ietf-oauth-client-id-metadata-document-00)
</content>
</invoke>
