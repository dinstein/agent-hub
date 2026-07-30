# Windows 现状（M2）

> **Windows 支持未在真实环境验证。**
> 本文所有代码路径都能交叉编译（CI 跑 `GOOS=windows go build ./...`）、
> 都有在 macOS/Linux 上通过注入钩子执行的单元测试，但**没有任何一行在 Windows 机器上跑过**，
> 更没有在 MSIX 容器里跑过。遇到与本文描述不符的行为，视为预期内的未知，而不是回归。

裁决出处：canonical.md §7 ——
「M1 只做 macOS + Linux；Windows（named pipe / SDDL / MSIX 逃逸）推 M2，**接缝收敛在
`internal/platform`**」，以及 A.3 #3「需要在多用户 Windows 与 MSIX 容器两种环境实测」。
实测环境本机没有，所以 M2 交付的是「实现 + 明确标注未实测」，不是「已支持」。

---

## 1. 已实现（可交叉编译、有单测、未实测）

落点全部在 `internal/platform`：`windows.go`（跨平台可测的解析逻辑）、
`packageid_windows.go`（真 syscall）、`packageid_other.go`（非 Windows 替身）。

| 能力 | 实现 | 验证方式 |
|---|---|---|
| 数据目录 | `%APPDATA%\agenthub`；`APPDATA` 缺失时回落 `<home>\AppData\Roaming\agenthub` | `TestPathResolution` / `TestWindowsUnpackagedUsesAppData` |
| MSIX 包身份探测 | `kernel32!GetCurrentPackageFamilyName`，经 `syscall.NewLazyDLL`（**不引 `golang.org/x/sys/windows`**：`internal/platform` 是零依赖底座，depguard 只允许 `$gostd`） | 只能交叉编译验证；逻辑分支由注入的 `Resolver.PackageIdentity` 覆盖 |
| loopback-UNC 孪生路径 | `C:\Users\a\...` → `\\127.0.0.1\C$\Users\a\...`；**先探活再采用** | `TestWindowsPackagedAdoptsLoopbackUNC` |
| 不可达时大声告警 | 落回本地路径 + 一条 stderr 警告（写 stderr 不写 stdout：stdio 网关的 stdout 是 JSON-RPC 帧流） | `TestWindowsPackagedUnreachableTwinWarnsLoudly` |
| 控制面 named pipe 路径 | `\\.\pipe\agenthub-ctl-<sha8(SID)>`（canonical.md §1 冻结标识符）；`platform.IsPipePath` 供调用方区分「这不是文件路径」 | `TestWindowsCtlPipePath` |
| pipe 的 SDDL | `platform.CtlPipeSDDL(sid)` = `D:P(A;;GA;;;<SID>)` —— 只给当前用户，**不给 Administrators、不给 SYSTEM** | `TestCtlPipeSDDL` |
| run 目录 | `<data>\run`（只放 `daemon.json`；控制端点是 pipe 不是文件） | `TestPathResolution` |

### 为什么 MSIX 探测的失败方向是「当作已打包」

`GetCurrentPackageFamilyName` 只有返回 `APPMODEL_ERROR_NO_PACKAGE`(15700) 才代表「没有包身份」。
其余任何结果——包括没预料到的错误码、老系统上没有这个导出——一律按**已打包**处理。

理由是两种猜错的代价不对称：在容器里猜「没打包」→ 数据目录被静默重定向到某个客户端的私有影子
目录，用户的配置按客户端悄悄分叉；不在容器里猜「已打包」→ 多做一次 UNC 探活，探活成功，
孪生路径指向的是同一个目录，没有任何损失。

### Windows 上的目录权限

`platform.EnsureDir` 在 Windows 上**不收紧权限**：Go 的 0700 位与 Windows ACL 不是一回事
（`os.Chmod` 在那里只切换只读属性）。`%APPDATA%` 本身已是每用户目录，控制端点的保护来自 pipe
的 SDDL 而不是目录 mode。显式收紧数据目录 ACL 属于下面的待办。

---

## 2. 未实现

这两项决定了 Windows 上**目前什么都跑不起来**：没有 registry 就没有配置，
没有 named pipe 就没有 daemon。它们不是打磨项，是 Windows 支持的前置条件。

### 控制面 named pipe 监听器

`internal/ctlapi.Listen` 在 Windows 上**fail-closed**：`peerCredSupported == false`，
直接返回 `platform.ErrUnsupportedPlatform`。也就是说 Windows 上 daemon 起不来，
`agenthub connect`（stdio 网关）走的是不依赖控制面的降级路径。

原因：Go 标准库没有 named pipe 服务端。要做需要 `github.com/Microsoft/go-winio`
（`winio.ListenPipe` 支持传入 SDDL），而本次任务的约束是**不动 `go.mod`**。

接缝已经就位，M2+ 补齐时要做的是：

1. `go get github.com/Microsoft/go-winio`；
2. 在 `internal/ctlapi` 加 `listener_windows.go`：
   `winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: sddl, MessageMode: false})`，
   其中 `path` = `platform.CtlSocketPath()`、`sddl` = `platform.Resolver.CtlPipeSDDL()`；
3. peer 身份不再需要 `SO_PEERCRED` 等价物——SDDL 已经在内核侧把非本用户挡在连接之前，
   这是 Windows 上比 Unix 更强的一层（Unix 的目录 0700 是可被误配的，SDDL 不是）。
   `credListener` 在 Windows 分支应当直接放行，并在注释里写明「授权发生在 ListenPipe，不是 Accept」；
4. `api` 包的拨号侧同样需要 `winio.DialPipe`；`platform.IsPipePath` 就是给它判分支用的。

**验收标准（没有 Windows 机器就不算完成）**：多用户 Windows 上两个用户同时起 daemon 互不串扰、
非属主用户连 pipe 被拒、以及在一个真实 MSIX 打包客户端里 spawn 网关后数据目录落点正确。

### registry 的跨进程锁

`internal/registry/flock_stub.go`（`!darwin && !linux`）的两个函数都返回
`errors.ErrUnsupported`，所以 Windows 上 `registry.Open` / `Update` / `Reload` **全部失败**。
这比 named pipe 更靠前：没有 registry 就没有配置，网关与 CLI 在 Windows 上都无法工作。
该文件的注释还停留在「Windows（LockFileEx）scheduled for M2」——M2 已交付而这一项没做，
注释先于代码过期了。

补齐要用 `LockFileEx` / `UnlockFileEx`（`golang.org/x/sys/windows` 已经在依赖里，
无需新增模块），语义要对齐 Unix 侧：独占、非阻塞、失败可判定为「被别人持有」。
`internal/skills` 与 `internal/audit` 各有一份同形状的 stub，同一次补齐。

### 控制管道不区分构建渠道

`windowsCtlEndpoint` 只由 SID 决定管道名，所以同一用户下 dev 与 release 抢同一个
`\\.\pipe\agenthub-ctl-<sha8(SID)>`——数据目录已按渠道分开，端点没有。
Unix 侧的渠道分离是靠「端点是 `<run>/ctl.sock` → run 目录跟随数据目录」传导的，
Windows 的端点不是文件系统路径，这条链在它上面不存在。

注意此前踩过的坑。管道名是**冻结标识符**（canonical.md §1/§2），release 名字不能动；而且**不能**从
`dirName` 派生——试过一次，结果是「重命名数据目录」悄悄变成了「重命名协议」。正确形状是给 dev 渠道
**另一个同样冻结**的名字（例如 `agenthub-ctl-dev-<sha8(SID)>`），而不是把渠道拼进现有名字的派生里。
这要求 Resolver 知道构建渠道，也就是 Unix 侧刻意回避的「按构建渠道决定」——在这里无法回避，因为没有
环境变量可以携带它。

等有真机时和命名管道监听器一起做：在一个从未跑起来过的平台上新增冻结标识符，等于把一个无法验证的猜测
冻进 ABI。验证方式是把 `TestDevResolverSeparatesFromRelease` 的 windows 行改成 `endpointSeparates: true`
并要求通过。

### 其他待办

- 数据目录 ACL 显式收紧（当前只依赖 `%APPDATA%` 的默认每用户权限）。
- `internal/cli` 的若干 POSIX 辅助（`setsid`、`noecho`）已有 `_stub.go` 分支，行为是「不做」，未实测。
- `api/dialorstart_test.go` 与 `test/e2e` 里用到 `syscall.Kill` 的测试在 `GOOS=windows` 下编译不过。
  这不影响 `GOOS=windows go build ./...`（CI 门禁），但意味着 `GOOS=windows go vet ./...` 会红。
  真上 Windows CI 时这些测试需要加平台标签。

---

## 3. 怎么自查

```bash
GOOS=windows go build ./...       # 必须全绿（CI 门禁）
go test ./internal/platform/      # Windows 解析逻辑的注入式单测，在 macOS/Linux 上跑
```
