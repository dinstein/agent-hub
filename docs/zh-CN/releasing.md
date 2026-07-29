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
release 版 `--version` 不带 `(dev)`，`--help` 也不列治理命令组
（approval / grant / config / audit）。「手上这个到底是哪一个」因此不再需要靠
跑一次去猜。

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

## GitHub Actions

tag 推送（`v*`）触发 `.github/workflows/release.yml`：

- **CLI**：ubuntu runner 交叉编译全平台，纯 Go 无 cgo
- **GUI**：macOS runner 原生构建 universal（Windows / Linux 尚未启用）

仓库当前是 **private**，Release asset 需要认证才能下载 ——
`curl | sh` 式安装不可用，纯 CLI 路径的意义要等转 public 后才完整成立。
