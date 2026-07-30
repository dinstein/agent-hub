# 发布

两条发布路径，产物不同、受众不同：

| 路径 | 产物 | 给谁 |
|---|---|---|
| GUI App | `AgentHub.app`（内含 CLI）→ DMG | 想要图形界面的用户，装完即用 |
| 纯 CLI | `agenthub` 单文件 → tar.gz | 只用 Claude Code / Cursor 的用户 |

两者共用同一个 CLI 二进制，只是打包方式不同。

## 版本号

版本号的唯一来源是仓库根的 `VERSION` 文件。

```
0.1.0
```

构建时由 `-ldflags -X main.version=` 注入二进制，同时写进 `.app` 的
`Info.plist`（`CFBundleShortVersionString`）。**一处修改，三处一致** —— 二进制自报、
App 简介、Release 标题不会各说各话。

发布时 tag 必须与 `VERSION` 一致（`v` 前缀）：`VERSION=0.1.0` ⇒ tag `v0.1.0`。
CI 会校验这一点，不一致直接失败——发布物版本对不上是事后极难排查的事故。

未提交改动构建出的二进制版本号带 `-dirty` 后缀，这是刻意的：
一个无法回答「这是哪个 build」的版本号比没有版本号更危险。

## 本地发布 macOS

```bash
make release-macos
```

产出 `dist/AgentHub-<version>-macos-universal.dmg`，内含：

```
AgentHub.app/Contents/MacOS/
├── agenthub-gui      # GUI 主程序
└── agenthub          # CLI / daemon
```

**两个二进制必须同级**，这不是布局偏好：`api/dialorstart.go` 的
`defaultDaemonBinary()` 靠「自己可执行文件的同级目录」找 daemon。同级关系一旦破坏，
GUI 启动 daemon 会退回 `$PATH` 查找，用户看到的是 socket-missing 错误，
而不是那次从未发生的启动。

Windows / Linux 的容器不同（目录 → zip、AppDir → AppImage），但同级关系一致，
所以这条约定跨平台通用。

## 签名

当前只做 ad-hoc 签名（`codesign --sign -`）并清除 quarantine 属性。
**未做 Apple Developer ID 签名和 notarization**，用户首次打开会被 Gatekeeper 拦，
需要右键 → 打开，或 `xattr -cr /Applications/AgentHub.app`。

正式对外分发前应补上 notarization，它需要 Developer ID 证书和
App-specific password，两者都还没有。

## CLI 的安装位置

GUI App **不会**往 `/usr/local/bin` 写任何东西。理由：写系统目录需要提权、
沙箱应用无权限、且卸载 `.app` 后会留下悬空 symlink。

想在终端里用 `agenthub` 的用户，应该通过包管理器（Homebrew）安装 CLI 版本，
而不是从 App bundle 里掏那个二进制。

## 客户端配置里的路径

`agenthub client connect` 写进客户端配置的是**执行者的绝对路径**
（`internal/cli/cli.go` 的 `executable()`）。两种发布形态下都正确：

- 纯 CLI：`/usr/local/bin/agenthub`
- GUI App：`/Applications/AgentHub.app/Contents/MacOS/agenthub`

后者的代价是 **App 被移动或改名后配置失效**。daemon 侧的
`agenthubExecutable()` 留了 `NonRegistry.Executable` 覆盖点，GUI 可以注入
自己解析出的路径；失效检测与「重新连接」提示尚未实现。

## dev / release 双通道

隔离是**二进制自身的属性**，不是调用方式的属性：构建时不声明
`CHANNEL=release` 的二进制自己就解析到开发目录，没有环境变量要记，也就没有
忘记的可能。

| channel | 数据目录 | `--version` |
|---|---|---|
| release | `~/Library/Application Support/agenthub` | `0.1.0-abc1234` |
| dev | `~/Library/Application Support/AgentHubDev` | `0.1.0-abc1234 (dev)` |

两者是**兄弟目录**，不是父子：一个 dev 运行不能靠往上走一级去改到装好的
registry，对其中一个执行 `rm -rf` 也带不走另一个。

```bash
make bin              # dev 版（默认）→ bin/agenthub
make bin-release      # release 版 → bin/agenthub-release
make dev ARGS="status"
make dev-where        # 这个构建到底在用哪个目录
make release-run ARGS="--help"   # release 版的 make dev
```

两个 flavour 落在**不同路径**，所以可以同时留着对照跑。它们的差别不止数据目录：
release 版 `--version` 不带 `(dev)`，`--help` 也不列 Daemon 组
（daemon / session / events / token）与 Manage 组
（approval / grant / config / tool / audit / activity / skill）。
留下的是日常那条完整路径，外加唯一一条能说清它断在哪一步的命令：注册并授权一台 server、
搭一个 profile、把 client 绑上去，以及其中某步不通时跑的 `doctor`——`profile` 因此**必须**
留在页面上，一个能接客户端却说不出这个客户端将会看见什么的构建只教了半个模型；`doctor`
同理单独成一个 Diagnose 组，一个教了路径却不说路径失败时该做什么的帮助页也是半个。
这些命令全部照常注册、照常可运行：收窄的是这个二进制**教什么**，不是它能做什么。
「手上这个到底是哪一个」因此不再需要靠跑一次去猜。

**默认是 dev 而不是 release，因为两个方向的失败代价不对称。** release 被误标成
dev，代价是一个空沙箱，用户看得见也修得掉；dev 被误标成 release，会写进装好那份
的 registry、花掉属于它的一次性 OAuth refresh token —— 事后发现也无法挽回。
所以 `go build`、`go run`、IDE 里跑、忘了传 flag，全都落在安全的一侧。

显式的 `AGENTHUB_DATA_DIR` 仍然优先于 channel。CI、e2e、同时调两个沙箱都靠它，
一个悄悄忽略它的构建会让那些场景看起来像是自己坏了。

socket 在 `<data>/run/ctl.sock`，跟着数据目录走，所以两个 channel 的 daemon
互相看不见。这一点由 `TestDevResolverSeparatesFromRelease` 钉死 —— 如果哪天不再
成立，两个 channel 会共用一个 daemon，隔离就只剩表面。

发布产物一律是 release：Taskfile 的 `common:cli`、`darwin:build:universal`
和 release workflow 都显式传了 `main.channel=release`。

## 让当前 checkout 跑在 Homebrew 装出来的那个位置上

```bash
make install-to-brew                     # 按发布的样子构建，装到 $(brew --prefix)/bin/agenthub
scripts/install-to-brew.sh --restore     # 把这个入口还给 Homebrew
```

**什么都不会发布** —— 不打 tag、不建 Release、不动 tap。它就是 `make bin-release` 加一个
目标路径，而目标路径正是重点：`agenthub client connect` 写进客户端配置的是**当前可执行文件的
绝对路径**，而功能都在 worktree 里开发、落地之后 worktree 就会被删掉。指向
`bin/agenthub-release` 的客户端，于是指着一条将来不存在的路径，写在一个这个仓库再也不会碰的
文件里。装到 Homebrew 那个位置，真实客户端不改任何配置就能跑到新构建上。

这里允许工作区是脏的，和 `scripts/build-release-artifacts.sh` 相反 —— 测未提交的改动本来就是
跑它的理由，而版本号照样带 `-dirty`。脚本真正拒绝的是 **dev 通道的二进制**，和 formula 的
`test do` 是同一条断言：装在那个位置上的 dev 构建会解析到 `AgentHubDev`，而每个客户端仍然调用
同一个命令名，于是通过 release 配好的 server 直接消失，且没有任何东西报错。

它替换掉的是 Homebrew 留在 `$(brew --prefix)/bin/agenthub` 的那个符号链接，换成一个普通文件
—— 这同时也是下次运行时不靠任何状态就能分辨两者的办法：Homebrew 从不在那里放普通文件。指向别处
的符号链接属于别的东西，脚本拒绝处理而不是猜。keg 本身没被动过，所以 `brew list --versions` 仍然
报的是发布版本，而 `agenthub --version` 报的是真正在跑的那个；脚本会把这件事明说出来。
`brew upgrade agenthub` 会自己重新链接、盖掉本地构建。

换掉文件不等于换掉**进程**：用上一个二进制起的 daemon 会一直服务所有客户端，直到
`agenthub daemon restart`。新 CLI 配旧 daemon 看起来就像被测改动本身有 bug，所以脚本会检查并
说明它遇到的是哪种情况。

## GitHub Actions

tag 推送（`v*`）触发 `.github/workflows/release.yml`：

- **CLI**：ubuntu runner 交叉编译全平台，纯 Go 无 cgo
- **GUI**：macOS runner 原生构建 universal（Windows / Linux 尚未启用）

`workflow_dispatch` 可以不打 tag 就把整条链路彩排一遍；只有 publish job 会察觉，它会 skip。

## 产物存放在哪里

**就在本仓库。** 本仓库是 public，`brew install` 不带任何凭据就能取到 tarball，产物也就和构建
它的源码待在一起。

这个去向是**一个决定、两个读者**：上传目标，以及写进 formula 的下载 URL。两者过去都靠各自的默认
值，而两个默认值并不一致 —— 上传回落到本仓库，`homebrew-formula.sh` 的默认值指向 tap。这种组合
的出错方式是能挑出来最安静的一种：所有 job 全绿、formula 是合法 Ruby、sha256 都是真的，然后在
除了这台机器以外的第一次 `brew install` 上 404。现在 workflow 在两处都写明
`${{ github.repository }}`，`TestReleaseWorkflowUploadsWhereTheFormulaPoints` 和
`TestReleaseScriptsAgreeOnTheArtifactRepo` 分别把 workflow 和脚本钉在各自那一半上。

**tap 上仍然挂着 `v0.11.0` 和 `v0.12.0` 的产物，它们留在原地。** 那两个版本是在本仓库还是
private 时发的，各自随之发布的 formula 钉的是**那一次上传**的 sha256。在这边重新构建出的同名
tarball 哈希不一样，所以 URL 和它旁边的哈希必须来自同一次上传 —— tap 上那批旧产物和任何东西
都不可互换。

有两项配置管 tap，都不影响 Release 本身：

| 配置项 | 类型 | 值 |
|---|---|---|
| `HOMEBREW_TAP_REPO` | repository variable | `dinstein/homebrew-agenthub` |
| `HOMEBREW_TAP_TOKEN` | repository secret | 对 tap 有 `contents: write` 的 token |

token 不能用 `GITHUB_TOKEN`，它只够得着本仓库；而 Release 本身不需要这种 token，因为它就写在
本仓库。**配了变量却没有 token，会在 `verify` 里失败，早于任何构建** —— 变量一旦配上，就说明
有人在等 tap 被更新，而等打完 DMG 才发现要多花二十分钟。两项都不配是受支持的状态：Release 照发，
tap 继续提供上一个版本，运行摘要里有一条 warning 说明这件事。

## Homebrew tap

有两个文件要送到 tap，`scripts/tap-sync.sh` 把它们作为**一个 commit** 一起放进去：

| 文件 | 是什么 |
|---|---|
| `Formula/agenthub.rb` | 由 `scripts/homebrew-formula.sh` 生成；安装预编译好的二进制 |
| `skills/agenthub/SKILL.md` | 从本仓库拷过去；教 AI 客户端怎么驱动那个二进制 |

两者并不独立。skill 是针对某个具体的已发布表面写的 —— 它自己开头就这么说 —— 所以如果两者分成
两个 commit 落地，中间就有一个窗口，其中任一个描述的版本另一个并不成立。

**skill 在本仓库维护，生成到 tap。** 它过去是直接在 tap 里改的，而这正是第二份副本一定会变成的
样子：agent 碰巧读到哪一份，就由哪一份决定它相信的 CLI 表面。tap 那份在 frontmatter 之后带一条
"生成物、勿手改"的横幅，而那条横幅是两份之间**唯一**的差别。

`scripts/tap-sync.sh <tap-checkout> <tag>` 省掉 formula 参数就是**只同步 skill**。skill 的修订
比 release 落地得快，没有这条路径的话，发一个文档修正就只能去切一个不改任何代码的版本。

两条发布路径 —— workflow 和 `scripts/release-local.sh` —— 都走这一个脚本，这一点由
`TestBothReleasePathsSyncTheTapThroughOneScript` 强制。一个内联了自己那份拷贝的调用方，照样会
commit、照样会 push、照样全绿；它做的事是把它忘掉的那个文件留在上一个 release。
