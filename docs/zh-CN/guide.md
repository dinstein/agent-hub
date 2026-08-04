# 使用 agenthub

这是面向使用者的手册：三个概念分别是什么、它们怎么拼在一起、以及你真正需要做的那几个决定。
`docs/` 下的其他文档是写给改 agenthub 代码的人看的，这一篇是写给用它的人看的。

## 整体形状

三个名词，一句话一个：

- **server** —— 你注册进来的一台下游 MCP server。「注册」和「打开」是两步：
  `server add` 只写下定义、保持关闭，`server enable` 才会去连接并把它投入使用。
  一台 server 默认提供它的全部 tool，除非你点名一个子集（`server tool allow`）。
  所有**已启用**的 server、应用了这些点名之后，就是 agenthub 有可能提供给任何人的全部东西。
- **profile** —— 上面那个集合的一个具名子集：包含哪些 server、这些 server 的哪些 tool、
  以及结果怎么呈现。
- **client** —— 一个 AI 应用（Claude Code、Cursor、Codex……）。一个 client **绑定**到一个
  profile 上，这条绑定就是「它能看见什么」的完整答案。

```
servers (enabled, + tool allow)   ← 上限：所有已启用的 server
   └── profile                    ← 你命名的一个子集
         └── client               ← 绑定到且只绑定到一个 profile
```

每一层都与它上面那层取交集，而且**没有任何一层能放宽**。这就是整套访问模型：
一个 client 能碰到什么，由它连上来之前你写下的配置决定，调用进行中不再决定任何事情。

由此推出两件事。**client 从不自己收窄**——它只是选一个 profile，而不是在 profile 之上再叠规则，
所以「这个 client 绑在哪个 profile 上」是一个完整答案，而不是半个。两个 client 需要不同的工具面，
就给它们两个 profile。以及**收窄只能收窄**：profile 永远不可能放出一台你禁用了的 server，
也变不出一个不存在的 tool——这让 `agenthub server disable` 成了一个无条件的总闸。

## 日常路径

```bash
# 1. 注册一台 server —— 只写下来，此时还是关着的
agenthub server add linear --url https://mcp.linear.app/mcp

# 2. 打开它；这一步会先连一次，并报出它还缺什么
agenthub server enable linear

# 3. 如果第 2 步要求登录，就登录（它自己也会把 server 打开，
#    所以一台注定要登录的 server，只走第 1、3 步即可）
agenthub auth login linear

# 4. 接上一个 client —— 一辈子只做一次
agenthub client connect claude-code --dry-run   # 先看一眼
agenthub client connect claude-code
```

第一台 server 的全部流程就这些。第 4 步**每个 client 只做一次**：它写进去的那条条目跑的是
`agenthub connect --client claude-code`，所以之后你再加的每一台 server 都会被自动带上，
不需要再动客户端的配置文件。

没有 profile 参与时，一个 client 看见的就是全部已启用的 server。很多场景下这就是对的答案，
读到这里就可以停了。

## 在全局关掉一个 tool

在 profile 之前，还有一件更钝的工具。一台 server 默认提供它拥有的一切，包括它日后版本里
新增的那些；点名 tool 会把这个集合钉死：

```bash
agenthub server ls                              # 先看现在的规则是什么
agenthub server tool allow github --only get_issue,list_prs
agenthub server tool allow github --none        # 这台 server 什么都不提供
agenthub server tool allow github --all         # 退回「提供全部」
```

写之前先读：`allow` 是**整条替换**、不是往上加，而且三个里必须明说一个——裸写
`server tool allow github` 会报用法错误，因为它本来会有的那个含义（「什么都不提供」）
跟你的本意之间只差一个忘了敲的参数。

这是**对所有 client 同时生效**的，是 `server disable` 在 tool 粒度上的孪生兄弟，
而且它是白名单、永远不是黑名单：server 新增一个 tool 的那天，有规则在的话，新 tool 会一直
待在外面直到你把它加进来。没有任何 profile 能把它拿走的东西放回来。

规则是什么、规则的效果是什么，这是两个问题，各有各的答案。`agenthub server ls`（只要有任何一台
server 带了规则，它就多出一列 TOOLS）和 `server inspect github` 说规则**是什么**；
`agenthub server tool ls` 列出规则生效之后真正提供的那些，`--all` 再把被挡下的也加上。
当某个 tool 在 client 里不见了、又看不出是哪一层拿走的，`agenthub server tool inspect
github__get_issue` 会指名道姓地说是哪一层。

## profile：当你想要的比「全部」少

```bash
agenthub profile create research
agenthub profile server add research linear      # 先 <profile> 再 <server>
agenthub profile tool allow research linear --only list_issues,get_issue
agenthub client bind cursor research
```

跟上一层是同样的三个命令，只是多一个参数：`agenthub profile tool ls research` 列出这个
profile 真正放行的那些——机器那一层的规则和 profile 自己的规则取交集，也就是绑在它上面的
client 实际拿到的东西；`--all` 会把被挡下的也列出来，逐个标出是哪一层拿走的。

`agenthub client ls` 列出谁绑在哪个 profile 上，以及 agenthub 到底有没有进它的配置。
`agenthub client unbind cursor` 把它退回默认。

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

要用 server 自己的 tool 名字，并且注意 `--only` 是一次**取交集**：写了一个这台 server 上没有的
名字，这台 server 就一个 tool 都放不出来。规则仍然会被存下来——它的 catalog 可能只是还没被取过
——但 agenthub 会拿这些名字去对最近一次记下的 catalog，对里面没有的那些**发出警告**，好让拼错在
敲下它的地方就说出来，而不是过后变成一份没人解释得清的空 tool 列表。

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

没有任何 profile 处于激活状态时，「未绑定」就等于「全部已启用的 server」。这里没有一个
「默认 profile」对象要你管理——不收窄本身就是默认值。

但两张列表仍然会点它的名，因为「我从没绑过的 client 到底拿到什么」是它们必须回答的问题。
`profile ls` 的表头第一行就是 `(default)`，`client ls` 的 PROFILE 列印的是同一个记号：

```
NAME                    ACTIVE  SERVERS  DISCOVERY         TOOL RULES
(default) -> research           linear   lazy (inherited)  linear: only list_issues,get_issue
research                *       linear   lazy (inherited)  linear: only list_issues,get_issue
```

星号标的是**当前生效的那一行**，而 `(default)` 直接把兜底解析成什么摆出来，不用你再去查一次。
它是一个显示记号，不是一个对象：只有 `agenthub profile use` 能挪动它，而且 profile 名字不允许以
`(` 开头。激活的 profile 不存在时，这一行会写 `MISSING -> empty scope`——和绑到不存在 profile 的
client 是同一个标记，也是同一个原因。

## discovery：工具面怎么呈现

`discovery` 决定的是一个 client 会被展示多少个 tool 名字，而不是它可以调用哪些 tool。它是
profile 的属性，因为它描述的正是**那一份工具集**：

```bash
agenthub profile discovery research lazy      # 或 grouped / full / -
```

| 模式 | `tools/list` 返回什么 | 什么时候用 |
|---|---|---|
| `full` | 每个可见 tool 一条 | 工具面小、或者客户端自己会筛的时候——要显式指定 |
| `grouped` | 每台 server 一条聚合条目，然后走 `call_tool` | 中等规模的集合——客户端先读每台 server 的条目，再分派 |
| `lazy` | 元工具（`status`、`search_tools`、`describe_tool`、`call_tool`、`fetch_result`）加上被 pin 住的 tool | 工具面很大的时候——客户端手里只握几个名字，而不是几百个。**谁都没设模式时的默认值** |
| `-` | 清除这个 profile 的覆盖 | 回落到全局默认值 |

`profile ls` 印的是每个 profile **实际会被服务的**模式：自己没设的那些显示继承来的值——
`lazy (inherited)`——而不是一个还要你自己去解析的短横线。表尾会说清楚这个继承值来自哪里：
内置默认值，还是 `config set discovery`。

值得在意它的理由是**上下文，不是安全**。四十台 server 跑在 `full` 模式下，意味着一份客户端
每轮都要重读一遍的工具清单；`lazy` 把它变成五个名字加一次搜索。两种模式下可见性都没变——
一个没出现在初始清单里的 tool，只要在作用域内就仍然调得动；而一个不在作用域内的 tool，
你选哪个模式它都调不动。

`lazy` 是默认值也是同一个理由：加到第四台 server 的时候没人会回头改这个设置，而 `full` 花掉的
上下文恰好与你把这个网关用得有多充分成正比。`lazy` 换来的代价是**可发现性**：客户端得自己搜，
而不是被直接递上工具名。工具面小的时候这笔交换不划算，那就明说：

```bash
agenthub config set discovery full
```

## 东西都放在哪

| 什么 | 在哪 |
|---|---|
| server 定义，以及它们的 tool 白名单 | `servers.json` |
| profile（server、tool、discovery） | `profiles.json` |
| client → profile 绑定 | `clients.json` |
| 全局开关、激活的 profile | `governance.json` |
| 凭据 | OS 钥匙串 / 金库——**绝不**在 registry 里 |

这些都不需要你手工去编辑——上面那些命令就是写它们的人，而 `server ls`、`client ls`、
`auth status` 分别把前三份读回来。

只要有任何一台 server 存了凭据，`server ls` 就会多出一列 `AUTH`，告诉你这台机器上存的是什么：
有凭据时是 `oauth`、`token`、`secret`，需要你出手时是 `oauth:expired`、`secret:missing`，
并在表格下面直接给出该跑哪条命令。它报告的是本机存了什么，不是对方还认不认——那个问题由
`agenthub server test <id>` 去真连一次来回答。

## 端到端验证

```bash
agenthub server test linear         # 真的连一次，看它怎么答
agenthub server inspect linear --tools
agenthub client ls                  # 谁接上了，以及谁绑在哪个 profile 上
```

`server test` 是这里唯一**证明**而不是**转述**的检查：它会真的建立一次连接，所以通过就意味着
凭据、传输、server 本身此刻全都是好的。`server inspect --tools` 列的是上一次接触时记录下来的
东西，所以它答得飞快，也因此可能是过期的。

`client connect` 改的是客户端自己的配置文件，其中两个客户端要特殊对待。Zed 和 VS Code 的配置是
JSONC（带注释的 JSON），所以 agenthub 只改自己那条的字节：你的注释、键序、排版原样返回；
而只要这次编辑有一点证明不了是对的，它就拒绝写，并告诉你该贴什么。codex 它干脆不写——重编码
TOML 会毁掉注释和排版——`client connect codex` 改成替你跑 `codex mcp add`：动手前先备份，
事后再读一遍确认。加 `--manual`（或设 `AGENTHUB_NO_CLIENT_CLI=1`）就让 agenthub 只把命令印出来，
不去运行它。

`client ls` 补上另一侧，每个客户端两个答案：CONNECTED 来自客户端自己的配置文件，
PROFILE 是它**可以看见什么**——它自己的 profile，或者你从没绑过它时的 `(default)`，也就是
`profile ls` 里同名的那一行。某一行给出的不是 yes/no 而是 `denied`、`unreadable`、`?` 时，
`client inspect <id>` 会说清楚是哪个文件、为什么。

写好的配置文件说明的只是意图。真正的确认是客户端自己：重启它，让它用一次某个 tool。

## 保留调用历史

**元数据是常开的**，没有任何开关：客户端向 agenthub 发起的每一次请求——`tools/call`，也包括
`initialize`、`tools/list`、`ping`——都会留下「问的是什么、落到哪台 server 的哪个工具、怎么
结束的、花了多久」。这一半不需要密钥，也永远不会拒绝一次调用；所以它不该是你必须先做的决定：
「这个客户端到底连上没有、做了什么」这个问题不应该以一次配置为前提。

**要开的是内容那一半。** `agenthub calls enable` 建立密钥，把请求参数、实际下游参数、结果，
以及（对开了 trace 的 server）帧内容，封进本地加密 pack。记录发生在门禁之前，所以被拒绝的调用
同样在历史里。

**两半都不会拒绝一次调用。** 密钥缺失或存储上限已满时，丢的是记录：调用照常执行，gateway 以
Error 记一条 `ledger record dropped`——`agenthub logs --level error` 就是历史出现空洞时该看的
地方。一个能拦住你用工具的账本，等于把磁盘写满摆在 hub 所有事情的前面，而且它也救不回它想保护
的那条记录：写失败的那一刻，那条记录就已经没了。

```bash
agenthub calls enable
agenthub calls status
agenthub calls tail --since 24h
agenthub calls tail -f                        # 保持打开，新调用到达就打印
agenthub calls show <call-id>                 # 默认只看元数据
agenthub calls show <call-id> --payloads      # 显式解密
agenthub calls stats --since 7d
agenthub calls verify
```

默认保留 30 个 UTC 日、总量上限 5 GiB，并为所在文件系统预留 1 GiB 空间；改它们用
`config set calls.retentionDays`、`calls.maxBytes`、`calls.minFreeBytes`。
`calls prune --dry-run` 先预览过期的日分区，`calls prune` 再整日删除——正常写入在自己那次容量
检查之前也会跑同一套清理。

导出和关掉它之前有两件事要知道。`calls export --output history.jsonl` 只把元数据写进一个新的
0600 文件，并拒绝覆盖已有文件；确实需要明文参数与结果时才加 `--payloads`，因为导出文件从此就带着
凭据离开了那个有界账本。以及 `calls disable` 只停止新增记录，不删历史也不删密钥，
正如 `audit rotate-key` 会留着旧密钥，让已有历史仍然读得出来。

`audit verify` 能发现元数据被改、payload 损坏和引用被调包。但所有证据都在本地，所以它无法证明
某个完整日目录从未被删过；删除证据对你重要的话，再接一个外部归档。

## 服务器出问题，而你当时没在看

三个文件回答三个不同的问题，拿错了就是一次故障从十分钟拖到一小时的原因：

```bash
agenthub events --server linear          # 它「发生了什么」，闭集词汇
agenthub logs --server linear            # 各进程「怎么描述它」，散文
agenthub calls tail --server linear      # 客户端「调了它什么」
```

三者共用同一套 `--since`（时长、RFC3339 时间，或 `all`）、同一套 `--limit`（`0` 表示全部）
和同一个 `-f`，所以在一个上能用的写法在另外两个上也能用。`agenthub server logs <id>` 是第四个，
看某条下游连接的帧。

先开 `events`。下游服务器、gateway、daemon 的每一次状态变化都会写进
`<data>/logs/events.jsonl`，带一个来自固定集合的 `kind`——`connected`、`circuit_open`、
`respawned`、`secrets_missing` 等等。固定正是要点：这些值可以拿来过滤和告警，
而日志消息的措辞不能。

```bash
agenthub events --since 24h                  # 全部，最新的在最后
agenthub events --server linear              # 单个下游的完整历史
agenthub events --kind circuit_open,health_down
agenthub events --scope daemon               # 重启与配置重载
agenthub events -f                           # 跟随；daemon 重启也不会断
```

没有 daemon 也能用，而且这不是降级路径：stdio gateway 自己就会写这个文件，所以「没有 daemon」
在这里是常态。它**默认开启**——唯一的开关是 `agenthub config set events.enabled false`。

最值得知道的过滤器是 `--class`：它把「hub 按预期在跑」和「hub 正在应对出问题的东西」分开，而且
一次故障的**恢复事件也归在 disruption 里**——所以一次中断不会读起来像还没结束：

```bash
agenthub events --class disruption           # 只看出问题的，以及它是怎么结束的
agenthub events --class routine              # 连接、attach、配置生效
```

它不是日志级别。级别是一维的，所以 `health_down` 是 warning 而 `health_up` 不是，于是
`logs --level warn` 会显示服务器掉下去再也没起来。class 问的是一条记录属于哪个**故事**，
而一个故事包含它的结局。

`logs` 是与之并排的散文视图，跨进程归并 —— `daemon.log` 加上每个已连接客户端的
`gateway-<client>.log` —— 输出成一条按时间排序的流。归并就是它存在的理由：daemon
重启、两秒后六个 gateway 掉线，这是一个故事，却分散在七个文件里。

```bash
agenthub logs --level warn --since 1h        # 最近哪里出了问题，不限进程
agenthub logs --client claude-code -f        # 跟随某个客户端的 gateway
agenthub logs --source daemon                # 或者只看 daemon
```

`agenthub daemon logs` 仍然是只看 `daemon.log` 的那个单进程视图。

## 当你需要看到线上的原始流量

一台 server 过得了 `server test`、在客户端里却表现不对时，问题就不再是「连不连得上」，而是
「它到底回了什么」。把流量录下来能回答这个：

```bash
agenthub server trace linear on
# 在你的客户端里复现问题
agenthub server logs linear
agenthub server trace linear off
```

打开之前有三件事值得知道：

**立刻生效。** 已经在跑的客户端不用重启就开始记录，而正在被调查的那条连接不会被重连。

**帧写进调用账本，每一帧都带着它所属的那次调用。** `agenthub server logs <id>` 从账本里读出某台
server 的对话，`agenthub calls show <call-id>` 则给出一次调用的完整经过——请求、它产生的帧、
重试、以及结果。重试两次的调用读起来是同一个 id 下的三次尝试，而不是三段互不相干的往返。

**内容在连接处捕获、早于任何过滤，但需要密钥。** 帧的内容进的是账本的加密 pack，也就是
`agenthub calls enable` 建立的那把钥匙；没有它，你仍然拿得到每一帧的方法、大小、耗时和结果，
只是拿不到它说了什么。这就是这笔交易：未脱敏的下游流量绝不会以明文落盘。

**按 server 生效，而且会持久。** 没有任何机制让 trace 过期，开着不管就会跨重启一直录。只要还有
东西在被 trace，`server ls` 就会多出一列 `TRACE`——想不起来开过哪台时就去那里看。

## 常见的意外

| 症状 | 多半是因为 |
|---|---|
| 加过的 server 一直不出现 | `server add` 之后它是关着的——看 `server ls`，再 `agenthub server enable <id>` |
| client 一个 tool 都看不见 | 绑到了一个不存在的 profile（`client ls` 里显示 `MISSING`），或者 `client connect` 之后从没重启过 |
| 某个 tool 消失了 | 是某一层的白名单拿走的——`agenthub server tool inspect <暴露名>` 会直接说是哪一层，不用再拿 `server ls` 和 `profile ls` 对着读 |
| `server test` 通得过，但客户端里用不了 | 客户端没重启，或者它的 profile 里没有这台 server |
| `client connect` 看起来什么都没干 | 它改的是一个文件；客户端在启动时才读那个文件 |
| `clients.json` 里有遗留的 `projects` 块 | per-project 绑定已经退役。这个块被保留下来但不生效——它当初是用来收窄的，所以留着它现在不会限制任何东西 |
