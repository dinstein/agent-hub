# 已知缺口（开发计划）

> 本文记录**已经确认存在、但尚未修复**的缺口。每一条都在写入时对着代码核过，
> 并附了复现方式与验证命令——不是凭印象列的愿望清单。
>
> 收录标准：**代码里能指到具体位置**的缺口。泛泛的「可以做得更好」不进这里。
> 修完一条就从本文删掉，并在对应的 `modules/` 文档里改成描述现状。

与 [windows.md](windows.md) 的分工：那篇讲一个平台的整体状态，这篇讲跨平台的、
零散的、已定位到行的缺口。

---

## 缺口一览

### 一、`internal/daemon` HTTP 数据面用例在重负载下偶发超时

**症状。** 制造 CPU 争抢（24 个 `yes` 进程）后跑
`go test ./internal/daemon/ -count=20 -race`，偶发命中 `waitFor` 的 10s 超时：
`TestHTTPDataPlaneServesRealCall`（tools/list 始终不含 `fake__echo`）与
`TestHTTPDataPlaneTokenTierIsEnforced`（tier 门始终不拒）。空闲机器上连跑 8 轮全绿，
所以它确实只在负载下出现。

**关于原先那条记录的更正。** 本条原本记的是「`TestHTTPDataPlaneRejectsBadCredentials`
偶发失败」，并猜测根因是**连接复用与 401 判定之间的时序**。这两点都不成立：

- 复现出来的是 `waitFor` 超时，不是凭据判定出错。压测里从未见过
  `RejectsBadCredentials` 失败。原记录只留了测试名没留失败输出，
  而 `initialize()` 的失败会被算在父测试头上——很可能当初看到的就是同一类超时。
- 认证与连接复用无关：`Authenticator.Authenticate`
  （`internal/httpbridge/auth.go:113`）逐请求解析 `Authorization` 头，
  没有任何按连接缓存的身份。

**现在证据齐了。** `waitForDetail`（`internal/daemon/daemon_test.go`）在超时时打印
最后一次观测（tools/list 实际返回了哪些工具、门实际返回了什么错误）以及**全部 goroutine 栈**
——daemon 是**进程内**跑的，所以这等价于 e2e 那套 SIGQUIT 取栈。

**下一步。** 拿一次带栈的失败，判定是「真卡住」还是「10s 对 `-count=20 -race` 下的
20 次 daemon 冷启动确实不够」。**在那之前不要动 `testTimeout`**：
调大超时会把这个问题变成「更慢地偶发失败」，并且把区分这两种可能的唯一线索删掉。

**复现尝试记录（别重复做无用功）。**

| 日期 | 条件 | 结果 |
|---|---|---|
| 2026-07-29 | 24 个 `yes` + `-count=20 -race`，跑 3 轮 | **复现 2 次**（`ServesRealCall`、`TokenTierIsEnforced`） |
| 2026-07-29（晚些） | 同上，4 轮 | 未复现 |
| 2026-07-29（晚些） | 28 个 `yes` + 全包 `-count=8 -race`，6 轮（≈48 次） | 未复现 |

**未复现不等于已修好**，所以这条继续留着。期间落地的改动没有一条明显指向它
（run 目录跟随数据目录只影响 Linux，这些用例走的是显式 `AGENTHUB_SOCKET`；
crash marker 在 daemon 启动时多了一次文件读写；`waitForDetail` 每轮轮询多做一次
字符串格式化——如果有影响，方向是让超时**更容易**发生而不是更难）。
最可能的解释是机器状态（散热、后台负载）不同。真要判定，需要的是**一次带栈的失败**，
不是又一轮没失败的运行。

---

### 二、Windows 上 dev 与 release 共用同一个控制管道

**症状。** 同一用户下，开发构建与已安装的 release 构建解析到**同一个**控制端点
`\\.\pipe\agenthub-ctl-<sha8(SID)>`。数据目录已经按渠道分开了（`AgentHub` /
`AgentHubDev`），端点没有——于是两个 daemon 抢一个管道，谁先 bind 谁赢，
输的那个客户端去跟一个持有另一份 registry 的 daemon 说话。

这是把 Linux 那条同类缺口（run 目录跟随数据目录，已修）补上时，在
`TestDevResolverSeparatesFromRelease` 的跨平台表里发现的：Windows 行是唯一一个
无法断言端点分离的。测试因此显式带了一个 `endpointSeparates: false` 字段并指回本条，
而不是把 Windows 从表里删掉——删掉会让这条缺口重新变成无人看守。

**根因。** `windowsCtlEndpoint`（`internal/platform/windows.go:202`）只由 SID 决定管道名。
Unix 侧的渠道分离是**间接**成立的：端点是 `<run>/ctl.sock`，run 目录跟随数据目录，
数据目录跟随渠道。Windows 的端点不是文件系统路径，这条传导链在它上面根本不存在。

**做法（注意别踩上一版踩过的坑）。** 管道名是**冻结标识符**（CANONICAL §1/§2），
release 的那个名字不能动；而且它当初就**不能**由 `dirName` 推导——那样做过一次，
结果是「重命名数据目录」静默变成了「重命名协议」。所以正确形状是给 dev 渠道
一个**另外一个同样冻结的**名字（例如 `agenthub-ctl-dev-<sha8(SID)>`），
而不是把渠道拼进现有名字的推导里。这需要 Resolver 知道渠道，
也就是 Unix 侧刻意避开的那种「按构建渠道判定」——在 Windows 上避不开，
因为没有环境变量能承载它。

**为什么这轮不修。** Windows 全线未在真机验证过（`docs/windows.md`），
本仓只有交叉编译门禁。在一个无法运行的平台上新增一个冻结标识符，
等于把一个不可验证的猜测固化成 ABI。等有真机时与命名管道监听器一起做。

**验证。** `TestDevResolverSeparatesFromRelease` 的 windows 行把
`endpointSeparates` 改成 `true` 后必须通过。

---

## 附：这些缺口是怎么发现的

2026-07-27 一次会话中，接入两台真实 MCP 服务器（均走同一套企业 SSO OAuth）
的过程里暴露的。同批发现的三个**已修复**问题：

| commit | 问题 |
|---|---|
| `5272d44` | `--issuer` 对所有远程服务器完全失效（`ResourceURL` 无条件优先） |
| `f2ac941` | `server add --stdin` 静默丢弃 `oauth` 块（`stdinEntry` 未建模该字段） |
| `e8cbb28` | RFC 9728 候选漏了 origin 根路径 |

值得记下的教训：`normalizeStdin` **此前零测试覆盖**，这正是静默丢字段能长期存活的原因。
修复时一并加了 `DisallowUnknownFields`——未建模的键现在直接报错，
而不是无声丢弃后让失败推迟到几步之外、以看似无关的面貌出现。
