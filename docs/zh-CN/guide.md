# 使用 agenthub

> **回答** 怎么把 server、profile、client 接起来，以及出事之后该打开哪一份记录。
> **不在这里** 这些规则的含义 → [model.md](model.md)；内部怎么工作 → [architecture.md](architecture.md)。
> **由什么保证为真** `test/e2e`——它就是按你的用法驱动这些命令的。

这份文档写给使用 agenthub 的人。`docs/` 下的其它文档写给改它代码的人。

## 日常路径

```bash
# 1. 登记一个 server —— 写下来了，但还是关的
agenthub server add linear --url https://mcp.linear.app/mcp

# 2. 打开它；这一步会先连接，并告诉你还缺什么
agenthub server enable linear

# 3. 如果第 2 步要求登录（这一步同时会启用该 server，
#    所以一个你本来就要登录的 server 只需要第 1、3 步）
agenthub auth login linear

# 4. 接一个客户端 —— 一辈子只做一次
agenthub client connect claude-code --dry-run   # 先看
agenthub client connect claude-code
```

第一个 server 的完整流程就这些。第 4 步**每个客户端只做一次**：它写进去的条目运行的是
`agenthub connect --client claude-code`，所以之后你加的每一个 server 都会被自动带上，不需要再碰客户端的配置。

没有 profile 参与时，一个客户端看到的是所有已启用的 server。很多场景下这就是正确答案，到这里就可以停。

## 全局关掉一个工具

一个 server 默认提供它拥有的全部工具，包括它在后续版本里新增的。点名工具可以把集合钉死：

```bash
agenthub server ls                              # 今天的规则是什么
agenthub server tool allow github --only get_issue,list_prs
agenthub server tool allow github --none        # 这个 server 一个都不提供
agenthub server tool allow github --all         # 回到提供全部
```

`allow` 是**替换**而不是追加，而且你必须在三者中说明是哪一种：光写
`agenthub server tool allow github` 是用法错误，因为它本来会有的那个含义——"一个都不提供"——与本意正好相反，
而两者只差一个被漏掉的参数。

这一层对所有客户端同时生效，而且它是白名单：server 新增工具的那天，规则会把新工具挡在外面，直到你把它加进来。
任何 profile 都拿不回这一层拿走的东西。

**规则是什么和规则的效果是两个问题，有两个答案。** `agenthub server ls` 和 `server inspect github`
说的是规则**是什么**；`agenthub server tool ls` 列的是经过规则之后实际提供的工具，加 `--all` 会把被挡下的
也列出来。当某个工具在客户端里不见了、又看不出是哪一层拿掉的，
`agenthub server tool inspect github__get_issue` 会点名是哪一层。

## Profile：只想要一部分的时候

```bash
agenthub profile create research
agenthub profile server add research linear      # 先 <profile> 后 <server>
agenthub profile tool allow research linear --only list_issues,get_issue
agenthub client bind cursor research
```

和上一层是同样的三个命令，只是多一个参数。`agenthub client ls` 显示谁在哪个 profile 上、以及 agenthub
是否真的写进了它的配置；`agenthub client unbind cursor` 让它回到默认。

**改绑定是即时生效的。** 改绑定会作用在已经在跑的会话上——agenthub 会重算并推 `tools/list_changed`。
只有 `client connect`（它改的是客户端自己的文件）需要重启客户端。

**绑到不存在的 profile 会向关闭方向失败。** 绑一个不存在的名字会被接受、给出警告，并把该客户端解析为**空**
作用域：它什么都看不到。删掉一个 profile 绝不能悄悄放宽所有引用它的客户端。如果某个客户端突然一个工具都没有，
先看 `agenthub client ls` 里有没有 `MISSING` 标记。

注意 `--only` 是**相交**：写一个该 server 没有的名字，会让这个 server 一个工具都过不来。规则仍然会被存下来
——目录可能只是还没抓取过——但 agenthub 会拿这些名字和它记录的最后一份目录比对，并对缺失的名字告警，
让打错的字在被打出来的地方就说话。

### 默认 profile

没有绑定的客户端跟随全局生效的 profile：

```bash
agenthub profile use research     # 所有未绑定的客户端现在跟随它
agenthub profile use -            # 清掉：未绑定的客户端看到所有已启用的 server
```

没有"默认 profile"这个对象要管——不收紧就是默认。两个列表仍然会点出它，因为"一个我从没绑过的客户端会得到什么"
是它们必须回答的问题：

```
NAME                    ACTIVE  SERVERS  DISCOVERY         TOOL RULES
(default) -> research           linear   lazy (inherited)  linear: only list_issues,get_issue
research                *       linear   lazy (inherited)  linear: only list_issues,get_issue
```

星号标的是当前生效的那一行。`(default)` 是一个显示标记，不是对象：只有 `profile use` 能移动它，
且 profile 名字不允许以 `(` 开头。

## Discovery：暴露面怎么呈现

`discovery` 决定客户端看到多少个工具名，不决定它能调用哪些。它是 profile 的属性，因为它描述的是那一批工具：

```bash
agenthub profile discovery research lazy      # 或 grouped / full / -
agenthub config set discovery full            # 全局默认
```

| 模式 | `tools/list` 返回 | 什么时候用 |
|---|---|---|
| `full` | 每个可见工具一条 | 小暴露面，或客户端自己会过滤 |
| `grouped` | 每个 server 一条聚合入口，再加 `call_tool` | 中等规模 |
| `lazy` | 元工具（`status`、`search_tools`、`describe_tool`、`call_tool`、`fetch_result`）加上被钉住的工具 | 大暴露面。**没有任何一层设置模式时的默认值** |
| `-` | 清掉这个 profile 的覆盖 | 回落到全局默认 |

值得在意的原因是上下文，不是安全。四十个 server 在 `full` 模式下意味着客户端每一轮都要重读一份工具清单；
`lazy` 把它变成五个名字加一次搜索。两种模式下可见性完全一样。`profile ls` 打印的是每个 profile 实际会被
服务的模式，所以没设模式的那个显示 `lazy (inherited)`，而不是一个还要你自己去解析的横杠。

## 端到端验证

```bash
agenthub server test linear         # 真的开一个连接，看它答什么
agenthub server inspect linear --tools
agenthub client ls                  # 谁接好了，谁在哪个 profile 上
```

`server test` 是唯一一个**证明**而不是**汇报**的检查：它真的去连，所以通过意味着凭据、传输和 server 本身
此刻都是好的。`server inspect --tools` 列的是上一次接触时记录下来的内容，所以它秒回，也因此可能过期。

`client connect` 改的是你客户端自己的配置文件，其中两个客户端有特殊处理。Zed 和 VS Code 用的是 JSONC
（带注释的 JSON），所以 agenthub 只改自己那一条的字节：注释、键顺序和排版原样保留；如果这次编辑无法被证明
正确，它会拒绝并告诉你该粘贴什么。Codex 它根本不写，因为重新编码 TOML 会毁掉注释和排版；
`client connect codex` 会替你运行 `codex mcp add`，先备份文件、事后再读回来核对。加 `--manual`
可以让 agenthub 只打印命令而不执行。

写好的配置文件只能说明意图。真正的确认是客户端自己：重启它，然后让它用一个工具。

## 东西都放在哪

| 什么 | 在哪 |
|---|---|
| server 定义，以及它们的工具白名单 | `servers.json` |
| profile（server、工具、discovery） | `profiles.json` |
| client → profile 绑定 | `clients.json` |
| 全局开关、生效的 profile | `governance.json` |
| 凭据 | 操作系统钥匙串或加密保险库——**绝不**在注册表里 |

这些都不需要手改。只要有任何一个 server 存了凭据，`server ls` 就会多出一列 `AUTH`，说明这台机器上存的是
什么：有凭据时是 `oauth`、`token` 或 `secret`，需要你处理时是 `oauth:expired`、`oauth:revoked` 或
`secret:missing`，修复命令印在表格下面。`oauth:revoked` 是唯一不会自己好的那种：provider 已经拒绝了存着的
那次登录，后台不会再续期，只有重新登录才有用。它报告的是这台机器持有什么，不是 server 是否还接受它；
`agenthub server test <id>` 才是去问。

## 出事了而你没在看

三个文件回答三个不同的问题，拿错那一个，是一次事故排查从十分钟变成一小时的原因：

```bash
agenthub events --server linear          # 它**发生**了什么，封闭词表
agenthub logs --server linear            # 进程们**说**了什么，散文
agenthub calls tail --server linear      # 客户端在它上面**调用**了什么
```

三者接受同样的 `--since`（时长、RFC3339 时间点，或 `all`）、同样的 `--limit`（`0` 表示全部）和同样的 `-f`。
第四个是 `agenthub server logs <id>`，看的是一个下游会话的帧。

**先开 `events`。** 下游 server、网关或 daemon 的每一次状态变化都会以固定集合中的一个 `kind` 落进
`<data>/logs/events.jsonl`——`connected`、`circuit_open`、`respawned`、`secrets_missing` 等等。固定是关键：
你可以按这些值过滤和告警，而按日志文案的措辞做这件事是不安全的。它在没有 daemon 时照常工作，且默认开启。

```bash
agenthub events --since 24h                  # 全部，最新的在最后
agenthub events --kind circuit_open,health_down
agenthub events --class disruption           # 只看出过什么问题、以及它怎么结束的
agenthub events -f                           # 跟随；能活过 daemon 重启
```

`--class` 是最值得知道的过滤器。它把"按预期在跑"和"在对出错做出反应"分开，而且一次故障会保留结束它的那次恢复
——所以一次已经过去的中断不会读起来像还在进行。它不是日志级别：级别是一维的，所以 `health_down` 是 warning
而 `health_up` 不是，这就是为什么 `logs --level warn` 会让你看到一个 server 掉下去、再也没起来。

`logs` 是它旁边的散文，跨所有进程合并——`daemon.log` 加上每个已连接客户端一份的 `gateway-<client>.log`
——排成一条按时间有序的流。这个合并就是它存在的理由：一次 daemon 重启和两秒后六个网关失去连接，是七个文件里
的同一个故事。

```bash
agenthub logs --level warn --since 1h        # 最近哪里出了问题，不限来源
agenthub logs --client claude-code -f        # 跟随某一个客户端的网关
```

## 留一份调用历史

**元数据一直在记录**，没有需要打开的开关：客户端向 agenthub 发起的每一个请求——`tools/call`，也包括
`initialize`、`tools/list` 和 `ping`——都会留下问了什么、到达了哪个 server 和工具、怎么结束、花了多久。

**要你打开的是"内容"那一半。** `agenthub calls enable` 会准备好那把密钥，用它把请求参数、生效参数、结果，
以及被追踪 server 的帧，封进本地的加密包。记录发生在闸链之前，所以被拒绝的调用也在历史里。

**两半都不能拒绝一次调用。** 密钥缺失或者达到存储上限时，丢的是记录：调用照常跑，网关以 Error 记下
`ledger record dropped`。`agenthub logs --level error` 就是历史里出现空洞时该看的地方。

```bash
agenthub calls enable
agenthub calls tail --since 24h
agenthub calls tail -f
agenthub calls show <call-id>                 # 只看元数据
agenthub calls show <call-id> --payloads      # 显式解密
agenthub calls stats --since 7d
agenthub calls verify
```

默认保留 30 个 UTC 天、账本上限 5 GiB、保留 1 GiB 空闲空间；用 `config set calls.retentionDays`、
`calls.maxBytes`、`calls.minFreeBytes` 改。`calls prune --dry-run` 会先给你看哪些日分区已过期。

导出或者关掉它之前有两件事要知道。`calls export --output history.jsonl` 把元数据写进一个新的 0600 文件，
并拒绝覆盖已有文件；只有在你确实需要解密后的参数和结果时才加 `--payloads`，因为导出的文件会把凭据带到那个
有界的账本之外。另外 `calls disable` 只停止新的抓取，不删历史也不删密钥，就像 `calls rotate-key` 会保留旧
密钥，让已有历史仍然可读。

`calls verify` 能发现被改过的元数据、损坏的载荷和被调包的引用。所有证据都在本地，所以它无法证明某一整天的
目录被删掉了；如果"删除也要留证"对你重要，请另外归档。

## 需要看到线上的字节时

当一个 server 已经能通过 `server test`、但在客户端里仍然表现不对时，问题就不再是"能不能连上"，
而是"它到底说了什么"：

```bash
agenthub server trace linear on
# 在你的客户端里复现问题
agenthub server logs linear
agenthub server trace linear off
```

**它立即生效**——已经在跑的客户端不需要重启就开始记录，被调查的这条连接也不会被重连。

**帧进的是调用账本，而且每一帧都点名它属于哪一次调用。** `agenthub calls show <call-id>`
会给出一次调用的完整故事——请求、它产生的帧、重试，以及结果——所以一次重试了两遍的调用读起来是同一个 id
下的三次尝试。

**帧体是在连接处抓的，在任何过滤之前，而且它需要密钥。** 没有 `calls enable`，你能拿到每一帧的方法、大小、
耗时和结果，但拿不到它说了什么。未脱敏的下游流量绝不会以明文写下。

**它是按 server 的，而且会持久。** 没有任何东西会让追踪过期，所以忘了关的追踪会跨重启一直记录。
有东西在被追踪时，`server ls` 会多出一列 `TRACE`。

## 常见的意外

| 现象 | 多半是 |
|---|---|
| 加了的 server 从来没出现 | `server add` 只登记不启用——先看 `server ls`，再 `agenthub server enable <id>` |
| 客户端一个工具都看不到 | 绑到了不存在的 profile（`client ls` 会显示 `MISSING`），或者 `client connect` 之后从没重启过 |
| 某个工具消失了 | 是某个白名单拿走的——`agenthub server tool inspect <暴露名>` 会点名是哪一层 |
| `server test` 好用但客户端里不行 | 客户端没重启，或者它的 profile 不包含这个 server |
| `client connect` 好像什么都没做 | 它改的是文件；客户端在启动时才读那个文件 |
| `clients.json` 里遗留的 `projects` 块 | 按项目绑定已经退役。这个块被保留但完全无效——它过去是用来收紧的，所以留着它不会限制任何东西 |
