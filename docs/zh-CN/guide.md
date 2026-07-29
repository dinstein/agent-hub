# 使用 agenthub

这是面向使用者的手册：三个概念分别是什么、它们怎么拼在一起、以及你真正需要做的那几个决定。
`docs/` 下的其他文档是写给改 agenthub 代码的人看的，这一篇是写给用它的人看的。

## 整体形状

三个名词，一句话一个：

- **server** —— 你注册进来的一台下游 MCP server。所有**已启用**的 server 合起来，就是
  agenthub 有可能提供给任何人的全部东西。
- **profile** —— 上面那个集合的一个具名子集：包含哪些 server、这些 server 的哪些 tool、
  以及结果怎么呈现。
- **client** —— 一个 AI 应用（Claude Code、Cursor、Codex……）。一个 client **绑定**到一个
  profile 上，这条绑定就是「它能看见什么」的完整答案。

```
servers (enabled)          ← 上限：所有已启用的 server
   └── profile             ← 你命名的一个子集
         └── client        ← 绑定到且只绑定到一个 profile
```

由此直接推出两件值得说明白的事：

**client 从不自己收窄。** 它只是选一个 profile，而不是在 profile 之上再叠规则。两个 client
需要不同的工具面，就给它们两个 profile。这正是「这个 client 绑在哪个 profile 上」是一个完整
答案、而不是半个答案的原因。

**收窄只能收窄。** profile 只能从已启用集合里拿走能力，它永远不可能放出一台被禁用的 server，
也变不出一个不存在的 tool。因此 `agenthub server disable` 是一个无条件的总闸：没有任何
profile 能把它拉回来。

## 日常路径

```bash
# 1. 注册一台 server
agenthub server add linear --url https://mcp.linear.app/mcp

# 2. 需要授权的话，登录它
agenthub auth login linear

# 3. 在任何 client 依赖它之前，先证明它真的能用
agenthub server test linear

# 4. 接上一个 client —— 一辈子只做一次
agenthub client connect claude-code --dry-run   # 先看一眼
agenthub client connect claude-code
```

第一台 server 的全部流程就这些。第 4 步**每个 client 只做一次**：它写进去的那条条目跑的是
`agenthub connect --client claude-code`，所以之后你再加的每一台 server 都会被自动带上，
不需要再动客户端的配置文件。

没有 profile 参与时，一个 client 看见的就是全部已启用的 server。很多场景下这就是对的答案，
读到这里就可以停了。

## profile：当你想要的比「全部」少

```bash
agenthub profile create research
agenthub profile server add research linear      # 先 <profile> 再 <server>
agenthub profile tools research linear --only list_issues,get_issue
agenthub client bind cursor research
```

`agenthub client ls` 列出谁绑在哪个 profile 上。`agenthub client unbind cursor` 把它退回默认。

有三个细节决定了这套东西的行为：

**改绑是实时的。** 改动绑定会作用到**已经在跑**的会话上——agenthub 会重算并推一条
`tools/list_changed`。只有 `client connect`（它改的是客户端自己的文件）才需要重启。

**tool 选择是三态的，而空状态是关闭的：**

| 你写的 | 它的意思 |
|---|---|
| 这台 server 上没有规则 | 它的全部 tool |
| `--only a,b` | 恰好这两个 |
| `--none` | 一个都不给，但 server 仍然列出来 |
| `--all` | 删掉这条规则（回到全部 tool） |

**profile 不存在时 fail-closed。** 绑到一个不存在的名字上是被接受的，会给出警告，并把这个
client 解析成一份**空**作用域——它什么都看不见。这是刻意的：删掉一个 profile 绝不能让引用过它的
client 全部被静默放宽。如果某个 client 突然一个 tool 都看不见，先去
`agenthub client ls` 里找 `MISSING` 标记。

### 默认 profile

没有绑定的 client 跟随**全局激活的 profile**：

```bash
agenthub profile use research     # 所有未绑定的 client 现在都跟随它
agenthub profile use -            # 清除：未绑定的 client 看到全部已启用的 server
```

没有任何 profile 处于激活状态时，「未绑定」就等于「全部已启用的 server」。这里不存在一个
额外的「默认 profile」对象要你管理——不收窄本身就是默认值。

## discovery：工具面怎么呈现

`discovery` 决定的是一个 client 会被展示多少个 tool 名字，而不是它可以调用哪些 tool。它是
profile 的属性，因为它描述的正是**那一份工具集**：

```bash
agenthub profile discovery research lazy      # 或 grouped / full / -
```

| 模式 | `tools/list` 返回什么 | 什么时候用 |
|---|---|---|
| `full` | 每个可见 tool 一条 | 工具面小的时候。谁都没设模式时的默认值 |
| `grouped` | 每台 server 一条聚合条目，然后走 `call_tool` | 中等规模的集合——客户端先读每台 server 的条目，再分派 |
| `lazy` | 元工具（`status`、`search_tools`、`describe_tool`、`call_tool`、`fetch_result`）加上被 pin 住的 tool | 工具面很大的时候——客户端手里只握几个名字，而不是几百个 |
| `-` | 清除这个 profile 的覆盖 | 回落到 `agenthub config set discovery` |

值得在意它的理由是**上下文，不是安全**。四十台 server 跑在 `full` 模式下，意味着一份客户端
每轮都要重读一遍的工具清单；`lazy` 把它变成五个名字加一次搜索。两种模式下可见性都没变——
一个没出现在初始清单里的 tool，只要在作用域内就仍然调得动；而一个不在作用域内的 tool，
你选哪个模式它都调不动。

## session：临时改动

一个活着的会话可以在不动任何配置的前提下被收窄：

```bash
agenthub session ls
agenthub session scope <sid> --disable-server linear
```

这类改动是**易失的**：它们只活在内存里，随会话一起消失。没有任何办法把它们持久化，这是刻意的
——永久性的改动属于 profile，在那里你以后还看得见它。反方向（放宽一个活着的会话）不是会话能对
自己做的事：它只能提交一份有 TTL 的授权申请，由人用 `agenthub grant approve` 批准。

## 东西都放在哪

| 什么 | 在哪 |
|---|---|
| server 定义 | `servers.json` |
| profile（server、tool、discovery） | `profiles.json` |
| client → profile 绑定 | `clients.json` |
| 全局开关、激活的 profile | `governance.json` |
| 凭据 | OS 钥匙串 / 金库——**绝不**在 registry 里 |

`agenthub doctor` 会把上面这些全部体检一遍，出问题时第一步就该跑它。但它不是例行动作：
它会去探测每一台已配置的 server，所以是**有症状之后**才跑，不是平时就跑。

## 端到端验证

```bash
agenthub tool ls        # client 将会看到的聚合后目录
agenthub audit          # 真正到达网关的调用
```

`audit` 是唯一诚实的证据。写好的配置文件说明的是意图；一条审计记录说明的是一次真的到达网关的
调用。重启客户端、让它用一次某个 tool，然后看到一条新的审计行——那才是确认。

## 常见的意外

| 症状 | 多半是因为 |
|---|---|
| client 一个 tool 都看不见 | 绑到了一个不存在的 profile（`client ls` 里显示 `MISSING`），或者 `client connect` 之后从没重启过 |
| 某个 tool 消失了 | 某个 profile 的 `--only` 列表、`agenthub tool disable`、或者 drift 隔离——先查这三样，再去怀疑 server |
| `server test` 通得过，但客户端里用不了 | 客户端没重启，或者它的 profile 里没有这台 server |
| `client connect` 看起来什么都没干 | 它改的是一个文件；客户端在启动时才读那个文件 |
| `clients.json` 里有遗留的 `projects` 块 | per-project 绑定已经退役。这个块被保留下来但不生效，`doctor` 会为它报警——它当初是用来收窄的，所以留着它现在不会限制任何东西 |
