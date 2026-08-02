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
| Cross-process file locks | `LockFileEx`/`UnlockFileEx` on `kernel32.dll` via `syscall.NewLazyDLL`; lock byte at 1<<62 (past any plausible file length, because Windows locks are **mandatory** — locking over data would break reads). Used in the five packages that take one — `internal/httpbridge`, `internal/oauthflow`, `internal/ratelimit`, `internal/registry`, `internal/skills`. **`internal/jsonl` is not among them**: it serializes appenders with `O_APPEND` alone, and the flock its predecessor carried belonged to the security stream's cross-process dedup, which was retired with that stream | `test/buildrules/flockparity_test.go`; lock byte is a protocol between processes and must not move |
| Named-pipe control listener | `winio.ListenPipe` (go-winio v0.6.2) with the SDDL from `platform.CtlPipeSDDL()`; maps `ERROR_ALREADY_EXISTS`/`ERROR_PIPE_BUSY` to `ErrAlreadyRunning`. `internal/ctlapi` now branches to the pipe listener before the `peerCredSupported` gate | `TestListenRefusesAPipePathOffWindows` on non-Windows; `TestWindowsEndpointContract` in `internal/ctlapi` asserts the frozen pipe spellings |
| Dev-channel pipe separation | `platform.Resolver` holds an unexported `devChannel bool` set only by `DevResolver`; `windowsCtlEndpoint` returns the frozen dev-channel name when it is true. Cannot be derived from directory names — the two names are literal constants in both `internal/platform` and `api`, held together by contract tests | `TestDevResolverSeparatesFromRelease` (`endpointSeparates: true` for the windows row); `TestWindowsEndpointContract` |
| api path resolution and dialing | `api/paths.go` computes the Windows data directory, run directory, and pipe name independently (api cannot import internal/platform). `api/dial_windows.go` uses `winio.DialPipeContext` for pipe-shaped paths | `TestDefaultSocketPath` windows rows; `TestDevSocketPathSeparatesFromRelease`; `TestSIDHashMatchesTheFrozenDigest` |
| GUI dev-channel endpoint | `cmd/agenthub-gui/channel.go` sets `AGENTHUB_SOCKET` on Windows when the channel is dev, so the GUI reaches the dev pipe rather than the release one. Setting it only on Windows avoids freezing the Unix path at a dev-run value | `TestDevChannelPinsTheEndpointOnWindows`; `TestExplicitSocketWinsOverTheChannel`; `TestDevChannelDoesNotPinTheEndpointOffWindows` |
| System tray and close-to-tray | `cmd/agenthub-gui/tray_wails.go` drives the notification-area icon and the close hook; the icon is drawn at runtime rather than shipped, so `SetIcon`/`SetDarkModeIcon` get a light- and a dark-taskbar variant. The behaviour is decided in untagged, unit-tested code ([modules/gui.md](modules/gui.md) §1.2) | `TestTray*` in `cmd/agenthub-gui`; `make cross-windows-gui`. **The icon itself, the notification-area overflow and the dark-taskbar variant are unverified** |
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

The daemon starts, the CLI runs, and the GUI finds its daemon. The first two items below are holes in
that rather than polish — `daemon stop` and `client connect` each reach an unimplemented branch — and
the three after them are polish.

### `daemon stop` has no Windows implementation

`internal/cli/daemonproc_stub.go` (`//go:build !unix`) answers `ErrUnsupportedPlatform` for both
`daemonSignalStop` and `daemonKillGroup`, and `daemonAlive` answers false. So a Windows daemon can be
started and cannot be stopped by `agenthub daemon stop`, with or without `--force`; the user is left
killing the process by hand. What it needs is a Job Object, which is also what
`detachProcessGroup` would use — the two are one piece of work, and neither can be verified from here.

The failure is at least loud: the operation reports unsupported rather than reporting success and
signalling nothing.

### No client has a user-level location on Windows

`internal/clients/table.go` keys each client's user-level config path by GOOS, and `sameOnAll` /
`perOS` only ever populate `darwin` and `linux`. A GOOS absent from that map makes the location
unavailable rather than guessed, so on Windows `resolve` drops every user placement and returns only
the project-relative ones. `agenthub client connect claude-desktop` therefore finds nothing to write
for any client whose config is user-level only.

The dropping is deliberate and the right direction — inventing `%APPDATA%\Claude\...` and writing to
it unverified is worse than finding nothing. Filling the table in is the work, and it needs a real
machine to confirm each client's actual path.

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
- The tray icon appearing (including from the notification-area overflow), its menu opening, and the
  close button hiding the window rather than ending the process
- At least one end-to-end call through a downstream MCP server

---

## 4. How to check for yourself

```bash
make cross-windows                                # GOOS=windows build + vet, minus the Unix-only e2e suite
make gui-frontend && make cross-windows-gui       # the OTHER gate — see the note below about the first half
go test ./internal/platform/ ./api/ ./internal/ctlapi/  # injection-based Windows unit tests, run on macOS/Linux
make release-windows                              # build the portable zip (cross-compiles on any host)
```

**`cross-windows` alone is half the check.** The GUI is excluded from the default build ("the GUI is optional" is a
compile-time constraint), so its Windows build is a separate target — and it is the half where wails v3 diverges
most from the macOS build this project develops on. AGENTS.md names both as the complete set of Windows gates.

**`cross-windows-gui` needs the frontend bundle first, and says so badly.** `gui_main.go` embeds
`frontend/dist`, which is gitignored, so on a fresh checkout the target fails before it compiles anything Windows
at all:

```
cmd/agenthub-gui/gui_main.go:26:12: pattern all:frontend/dist: no matching files found
```

That is a missing `make gui-frontend`, not a Windows problem. `make ci-full` orders the two correctly, which is
why it is only ever seen by someone running the gate by hand.
