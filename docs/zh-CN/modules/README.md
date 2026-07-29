# 模块文档

按层分五篇，每篇开头讲这一层的整体协作，然后逐包展开：
**一句话职责 → 关键类型与入口 → 不变量与失败方向**。
另有两篇按**外部约束**而非按层组织的专题：[oauth.md](oauth.md) 与 [gui.md](gui.md)。

包与层的对应关系速查见 [../architecture.md §3 核心模块地图](../architecture.md#3-核心模块地图)。

最值得读的是每包的「不变量与失败方向」——它回答的是「什么不能碰、出错时往哪个方向倒」，
这些约束多数不能从函数签名看出来，改代码之前扫一眼能省掉一次事故。

| 文件 | 覆盖的包 |
|---|---|
| [foundation.md](foundation.md) | `platform`、`logx`、`tier`、`mcp`（+`transport` 四种实现）、`registry` |
| [config.md](config.md) | `scope`、`session`、`event`、`secrets`（+`secureenv`）、`clients`、`skills` |
| [dataplane.md](dataplane.md) | `downstream`、`router`、`pipeline`、`gateway`、`discovery`（+`toolsig`）、`shaping`（+`toonenc`）、`ratelimit` |
| [security.md](security.md) | `guard`（`injection`/`spawnguard`/`netguard`/`leakguard`）、`integrity`、`approval`、`audit`、`oauthflow` |
| [controlplane.md](controlplane.md) | `api`、`ctlapi`、`confops`、`catalog`、`daemon`、`httpbridge`、`cli`（+`output`）、两个 `cmd/`、`testutil/fakemcp`、`depguardtest`、`test/*` |
| [oauth.md](oauth.md) | 专题：`oauthflow` 对 MCP 授权规范的符合度、支持的 provider 部署形态、已知差距 |
| [gui.md](gui.md) | 专题：GUI 前端的信息架构、状态呈现、写操作与 HITL 表现层、明确不做的事 |

## 这套文档的写法

**先读源码再写。** 这五篇是照着代码写的。凭印象写的模块文档会在第一次重构后变成误导，
而误导比空白贵——空白会让人去读代码，误导会让人不去读。

**能力存在 ≠ 已接线。** 有些包功能完整、测试充分，但装配层还没把它接上——
这类情况在对应包里用「当前装配现状」明确标出，不会含糊过去。
[../architecture.md §12](../architecture.md#12-装配现状已实现但尚未接线的部分) 有一份汇总。

**已定位到行的缺口写进 [../backlog.md](../backlog.md)，不写进这里。** 这里描述现状，那里描述欠账。
