# Windows status

> **Windows support has not been verified in a real environment.**
> Every code path described here cross-compiles (CI runs `GOOS=windows go build ./...` and
> `GOOS=windows go vet`) and has unit tests that execute on macOS/Linux through injection hooks, but
> **not a single line has ever run on a Windows machine**, let alone inside an MSIX container.
> Behavior that contradicts this document should be treated as an expected unknown, not a regression.

---

## 1. Implemented (cross-compiles, unit tested, untested in practice)

| Capability | Implementation | How it's verified |
|---|---|---|
| Data directory | `%APPDATA%\AgentHub`; falls back to `<home>\AppData\Roaming\AgentHub` when `APPDATA` is missing | `TestPathResolution` / `TestWindowsUnpackagedUsesAppData` |
| MSIX package identity detection | `kernel32!GetCurrentPackageFamilyName` via `syscall.NewLazyDLL` (**without pulling in `golang.org/x/sys/windows`**: `internal/platform` is a zero-dependency foundation, and depguard only permits `$gostd`) | Only verifiable by cross-compilation; the logic branches are covered via an injected `Resolver.PackageIdentity` |
| loopback-UNC twin path | `C:\Users\a\...` → `\\127.0.0.1\C$\Users\a\...`; **probed for reachability before adoption** | `TestWindowsPackagedAdoptsLoopbackUNC` |
| Loud warning when unreachable | Falls back to the local path plus one stderr warning (stderr, not stdout: the stdio gateway's stdout is a stream of JSON-RPC frames) | `TestWindowsPackagedUnreachableTwinWarnsLoudly` |
| Control-plane named pipe path | `\\.\pipe\agenthub-ctl-<sha8(SID)>` (release) / `\\.\pipe\agenthub-ctl-dev-<sha8(SID)>` (dev — see channel separation below). A frozen identifier (canonical.md §1); `platform.IsPipePath` lets callers tell "this is not a file path" | `TestWindowsCtlPipePath` / `TestWindowsDevCtlPipePath` |
| The pipe's SDDL | `platform.CtlPipeSDDL(sid)` = `D:P(A;;GA;;;<SID>)` — the current user only, **not Administrators, not SYSTEM** | `TestCtlPipeSDDL` |
| run directory | `<data>\run` (holds only `daemon.json`; the control endpoint is a pipe, not a file) | `TestPathResolution` |
| Cross-process file locks | `LockFileEx`/`UnlockFileEx` on `kernel32.dll` via `syscall.NewLazyDLL`; lock byte at 1<<62 (past any plausible file length, because Windows locks are **mandatory** — locking over data would break reads). It is one half of a seam whose other half is `flock(2)` in `filelock_unix.go`: the seven packages that take a lock — `internal/calllog`, `internal/httpbridge`, `internal/oauthflow`, `internal/ratelimit`, `internal/registry`, `internal/secrets`, `internal/skills` — each own a `flock.go` tagged `darwin \|\| linux \|\| windows` that calls `LockFile`/`TryLockFile`/`UnlockFile`/`IsLockBusy` and never names an OS. **`internal/jsonl` is not among them**: it serializes appenders with `O_APPEND` alone | `test/buildrules/flockparity_test.go` walks for `flock.go`, demands it reach `internal/platform` and have a `flock_stub.go` beside it, and fails any package outside `internal/platform` that calls `syscall.Flock` or `LockFileEx` itself — so the set is discovered rather than listed, and a private copy cannot drift out of parity again; the lock byte is a protocol between processes and must not move |
| Named-pipe control listener | `winio.ListenPipe` (go-winio v0.6.2) with the SDDL from `platform.CtlPipeSDDL()`; maps `ERROR_ALREADY_EXISTS`/`ERROR_PIPE_BUSY` to `ErrAlreadyRunning`. `internal/ctlapi` now branches to the pipe listener before the `peerCredSupported` gate | `TestListenRefusesAPipePathOffWindows` on non-Windows; `TestWindowsEndpointContract` in `internal/ctlapi` asserts the frozen pipe spellings |
| Dev-channel pipe separation | `platform.Resolver` holds an unexported `devChannel bool` set only by `DevResolver`; `windowsCtlEndpoint` returns the frozen dev-channel name when it is true. Cannot be derived from directory names — the two names are literal constants in both `internal/platform` and `api`, held together by contract tests | `TestDevResolverSeparatesFromRelease` (`endpointSeparates: true` for the windows row); `TestWindowsEndpointContract` |
| api path resolution and dialing | `api/paths.go` computes the Windows data directory, run directory, and pipe name independently (api cannot import internal/platform). `api/dial_windows.go` uses `winio.DialPipeContext` for pipe-shaped paths | `TestDefaultSocketPath` windows rows; `TestDevSocketPathSeparatesFromRelease`; `TestSIDHashMatchesTheFrozenDigest` |
| GUI dev-channel endpoint | `cmd/agenthub-gui/channel.go` sets `AGENTHUB_SOCKET` on Windows when the channel is dev, so the GUI reaches the dev pipe rather than the release one. Setting it only on Windows avoids freezing the Unix path at a dev-run value | `TestDevChannelPinsTheEndpointOnWindows`; `TestExplicitSocketWinsOverTheChannel`; `TestDevChannelDoesNotPinTheEndpointOffWindows` |
| System tray and close-to-tray | `cmd/agenthub-gui/tray_wails.go` drives the notification-area icon and the close hook; the icon is drawn at runtime rather than shipped, so `SetIcon`/`SetDarkModeIcon` get a light- and a dark-taskbar variant. The behaviour is decided in untagged, unit-tested code ([modules/gui.md](modules/gui.md) §1.5) | `TestTray*` in `cmd/agenthub-gui`; `make cross-windows-gui`. **The icon itself, the notification-area overflow and the dark-taskbar variant are unverified** |
| Portable zip packaging | `build/windows/Taskfile.yml` cross-compiles both architectures (amd64, arm64) on any host, embeds icon + manifest + version resource via `wails3 generate syso`, and packs `AgentHub/{agenthub-gui.exe, agenthub.exe, README.txt}` into a zip. Builds automatically in the release workflow | `make release-windows`; layout matches the sibling contract in `api/dialorstart.go` |

### Why MSIX detection fails in the "assume packaged" direction

`GetCurrentPackageFamilyName` indicates "no package identity" only when it returns
`APPMODEL_ERROR_NO_PACKAGE` (15700). Any other outcome — including unanticipated error codes, or the
export not existing on an older system — is treated as **packaged**.

The reason is that the two wrong guesses cost asymmetrically. Guessing "unpackaged" inside a container
silently redirects the data directory into some client's private shadow directory, quietly forking the
user's configuration per client. Guessing "packaged" outside a container costs one extra UNC reachability
probe, which succeeds, and the twin path points at the same directory — no loss at all.

### Why Windows mandatory locks use byte 1<<62

Windows file locks are MANDATORY, not advisory. Locking over any byte that an actual read could reach
would make the file unreadable to other handles. The lock byte at 1<<62 sits past any plausible file
length, so two agenthub processes can both have the file open while the loser waits for the lock. This
offset is a **protocol between processes** and must not move.

### Why the pipe name encodes a hash rather than the raw SID

Pipe names live in a machine-global namespace (`\\.\pipe\`). Two users simultaneously running agenthub
would otherwise contend for `\\.\pipe\agenthub-ctl` — the loser connects to the winner's daemon and
operates against the wrong configuration. The `sha8(SID)` suffix makes each user's pipe name
distinct. The actual access control is the SDDL `D:P(A;;GA;;;<SID>)`, which is stricter than Windows
convention: `NOT Administrators, NOT SYSTEM` — because the control plane hands out every downstream
credential.

### Why dev and release channels need separate frozen pipe names

On Unix, channel separation is derived: the dev endpoint is `<run>/ctl.sock` and the dev data
directory is `AgentHubDev`, so moving the data directory automatically moves the endpoint. Windows
pipes are not filesystem paths — there is no derivation chain. The correct shape is two literal
constants, with the `Resolver.devChannel` flag selecting between them. Splicing "dev" into the release
name's derivation was tried once and is wrong: "rename the data directory" would then silently mean
"rename the protocol". See canonical.md §1 on frozen identifiers.

### Directory permissions on Windows

`platform.EnsureDir` **does not tighten permissions** on Windows: Go's 0700 bits and Windows ACLs are not
the same thing (`os.Chmod` there only toggles the read-only attribute). `%APPDATA%` is already a
per-user directory, and the control endpoint's protection comes from the pipe's SDDL rather than the
directory mode. Explicitly tightening the data directory's ACL is an open item (below).

---

## 2. Open items

The daemon starts, the CLI runs, the GUI finds its daemon, and the two items that were holes rather
than polish — `daemon stop` and `client connect` — are implemented. What is left below is polish,
plus the one thing none of it substitutes for: a machine.

**A Job Object was the wrong answer, and this document was the one proposing it.** It said `daemon
stop` "needs a Job Object", which does not fit a detached daemon started by a short-lived CLI: the
job dies with the last handle, and the CLI exits immediately. A console control event is no better —
`GenerateConsoleCtrlEvent` reaches only a process group sharing the CALLER's console, so it cannot
reach a daemon started from another terminal or by a windowless GUI. What the graceful stop actually
uses is the control plane: `POST /v1/daemon/stop`, over the socket `daemon stop` is already holding
when it needs it. `--force` is `TerminateProcess`, and `daemonAlive` delegates to
`platform.ProcessAlive`, which had been implemented here all along while the CLI stub answered false.

### The owner watch has one mechanism here instead of two

A hub belongs to the application that started it and must not outlive it
(`internal/daemon/owner.go`). On unix that is enforced twice: a **lifeline** pipe whose close the
kernel guarantees however the owner dies, and a pid **poll** as the backstop. Windows gets the poll
alone — `os/exec` cannot hand a child an extra descriptor there, so `api.newLifeline` returns nothing
and the child is started without `--owner-lifeline-fd`. The poll itself is implemented
(`platform.ProcessAlive` opens the process and reads its exit code, because a pid `OpenProcess`
accepts may name one that has already exited) and has never run on a real machine.

Two consequences, in opposite directions. A hub can outlive its owner by up to one poll interval,
which is the harmless direction — the next launch finds it and adopts nothing. And
`api.Supervised.Stop` **kills rather than asks** here, because there is no SIGTERM to deliver: the
alternative to a hard stop is no stop at all, leaving a hub running with the application gone and
nothing on the machine that will ever end it. The Job Object that fixes `daemon stop` above fixes
this too — one piece of work, and neither half is verifiable from here.

### The client paths are filled in, and unconfirmed

`internal/clients/table.go` now answers for Windows in every row. Two conventions, kept apart because
copying one onto the other is how a write lands somewhere the client never reads: the CLI-shaped
clients keep a dotfile in the profile and are identical on all three platforms, while the
application-shaped ones live under `%APPDATA%` — a different BASE, not just different segments. Zed
is where the distinction bites (`.config/zed` on macOS and Linux, `%APPDATA%\Zed` here), and
`%APPDATA%` is read from the environment rather than rebuilt under the home directory, because a
roaming profile or a managed desktop redirects it.

One redirect, and it is a probe rather than a guess. An MSIX-installed Claude Desktop reads a
VIRTUALIZED `%APPDATA%`: its configuration lives under the package's `LocalCache`, and the documented
`%APPDATA%\Claude` file is one the packaged application never opens. Writing there would parse,
verify and change nothing observable. The probe only ever redirects toward a file that EXISTS, so an
ordinary install keeps the documented path — but the package family name it looks for
(`Claude_pzs8sxrjxfjjc`) comes from published reports, not from this project having seen one.

What is owed is confirmation: every path in that table came from vendor documentation and community
reports, and a wrong one fails in the way this table can least report — the write succeeds.

### Tighten the data directory's ACL

`EnsureDir` leaves `%APPDATA%\AgentHub` with `%APPDATA%`'s default ACL rather than an explicit
owner-only ACL. The per-user-only protection today comes from `%APPDATA%` itself being a per-user
directory. Explicitly narrowing it would be belt-and-braces defense and is the right thing to do once
real hardware is available to verify the effect.

### POSIX helpers stub out on Windows

Several helpers in `internal/cli` (`setsid`, `noecho`) have `_stub.go` branches whose behavior is
"do nothing". This is not dangerous — they are session-control and terminal-echo helpers used in
optional flow paths — but it means those behaviors are silently absent on Windows rather than
reported as unsupported.

### test/e2e doesn't compile for GOOS=windows

The suite dials Unix sockets and kills daemons with platform-specific signals throughout; it does not
compile for `GOOS=windows` and would prove nothing there if it did. Windows behavior is covered by
unit tests through injected seams (a fake SID, a fake package identity, a forced goos) and by nothing
else. This is by design; the exclusion is in `make cross-windows`.

---

## 3. Acceptance criteria (not done until there's a Windows machine)

These are the checks that would graduate Windows from "experimental" to "supported":

- Two users on a multi-user Windows box running daemons simultaneously without interference
- A non-owner user being refused when connecting to the named pipe
- The data directory landing correctly after spawning the gateway from a real MSIX-packaged client
- The GUI starting the daemon successfully (pipe listener arming, `run/daemon.json` written)
- `agenthub daemon stop` ending a daemon started from ANOTHER terminal, and from the GUI — the two
  cases a console control event cannot reach, and the reason the graceful stop goes over the socket
- `agenthub client connect <id>` writing a file each client then actually reads, for at least one
  dotfile client and one `%APPDATA%` one. The Claude Desktop row is the one to check twice: an MSIX
  install reads a virtualized `%APPDATA%`, and the redirect probe is the only thing standing between
  a correct write and one that succeeds and is never read
- The tray icon appearing (including from the notification-area overflow), its menu opening, and the
  close button hiding the window rather than ending the process
- At least one end-to-end call through a downstream MCP server

---

## 4. How to check for yourself

```bash
make cross-windows                                # GOOS=windows build + vet, minus the Unix-only e2e and installer suites
make gui-frontend && make cross-windows-gui       # the OTHER gate — see the note below about the first half
go test ./internal/platform/ ./api/ ./internal/ctlapi/  # injection-based Windows unit tests, run on macOS/Linux
make release-windows                              # build the portable zip (cross-compiles on any host)
```

**`cross-windows` alone is half the check.** The GUI is excluded from the default build ("the GUI is
optional" is a compile-time constraint), so its Windows build is a separate target — and it is the
half where wails v3 diverges most from the macOS build this project develops on. Both are the
complete set of Windows gates.

**`cross-windows-gui` needs the frontend bundle first, and says so badly.** `gui_main.go` embeds the
gitignored `frontend/dist`, so on a fresh checkout the target fails before compiling anything Windows
at all — `pattern all:frontend/dist: no matching files found`. That is a missing `make gui-frontend`,
not a Windows problem. `make ci-full` orders the two correctly, which is why only someone running the
gate by hand ever sees it.
