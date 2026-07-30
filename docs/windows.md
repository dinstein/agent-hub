# Windows status (M2)

> **Windows support has not been verified in a real environment.**
> Every code path described here cross-compiles (CI runs `GOOS=windows go build ./...`) and has unit
> tests that execute on macOS/Linux through injection hooks, but **not a single line has ever run on a
> Windows machine**, let alone inside an MSIX container. Behavior that contradicts this document should
> be treated as an expected unknown, not a regression.

Source of the decision: canonical.md §7 — "M1 covers macOS + Linux only; Windows (named pipes / SDDL /
MSIX escape) is deferred to M2, with **the seams confined to `internal/platform`**" — plus A.3 #3,
"needs real-world testing in both multi-user Windows and MSIX container environments." We have no such
test environment on hand, so what M2 delivers is "implemented, and explicitly marked untested," not
"supported."

---

## 1. Implemented (cross-compiles, unit tested, untested in practice)

Everything lands in `internal/platform`: `windows.go` (cross-platform testable resolution logic),
`packageid_windows.go` (the real syscall), and `packageid_other.go` (the non-Windows stand-in).

| Capability | Implementation | How it's verified |
|---|---|---|
| Data directory | `%APPDATA%\agenthub`; falls back to `<home>\AppData\Roaming\agenthub` when `APPDATA` is missing | `TestPathResolution` / `TestWindowsUnpackagedUsesAppData` |
| MSIX package identity detection | `kernel32!GetCurrentPackageFamilyName` via `syscall.NewLazyDLL` (**without pulling in `golang.org/x/sys/windows`**: `internal/platform` is a zero-dependency foundation, and depguard only permits `$gostd`) | Only verifiable by cross-compilation; the logic branches are covered via an injected `Resolver.PackageIdentity` |
| loopback-UNC twin path | `C:\Users\a\...` → `\\127.0.0.1\C$\Users\a\...`; **probed for reachability before adoption** | `TestWindowsPackagedAdoptsLoopbackUNC` |
| Loud warning when unreachable | Falls back to the local path plus one stderr warning (stderr, not stdout: the stdio gateway's stdout is a stream of JSON-RPC frames) | `TestWindowsPackagedUnreachableTwinWarnsLoudly` |
| Control-plane named pipe path | `\\.\pipe\agenthub-ctl-<sha8(SID)>` (a frozen identifier, canonical.md §1); `platform.IsPipePath` lets callers tell "this is not a file path" | `TestWindowsCtlPipePath` |
| The pipe's SDDL | `platform.CtlPipeSDDL(sid)` = `D:P(A;;GA;;;<SID>)` — the current user only, **not Administrators, not SYSTEM** | `TestCtlPipeSDDL` |
| run directory | `<data>\run` (holds only `daemon.json`; the control endpoint is a pipe, not a file) | `TestPathResolution` |

### Why MSIX detection fails in the "assume packaged" direction

`GetCurrentPackageFamilyName` indicates "no package identity" only when it returns
`APPMODEL_ERROR_NO_PACKAGE` (15700). Any other outcome — including unanticipated error codes, or the
export not existing on an older system — is treated as **packaged**.

The reason is that the two wrong guesses cost asymmetrically. Guessing "unpackaged" inside a container
silently redirects the data directory into some client's private shadow directory, quietly forking the
user's configuration per client. Guessing "packaged" outside a container costs one extra UNC reachability
probe, which succeeds, and the twin path points at the same directory — no loss at all.

### Directory permissions on Windows

`platform.EnsureDir` **does not tighten permissions** on Windows: Go's 0700 bits and Windows ACLs are not
the same thing (`os.Chmod` there only toggles the read-only attribute). `%APPDATA%` is already a
per-user directory, and the control endpoint's protection comes from the pipe's SDDL rather than the
directory mode. Explicitly tightening the data directory's ACL is in the to-do list below.

---

## 2. Not implemented

These two are why **nothing runs on Windows today**: no registry means no configuration, and no named
pipe means no daemon. They aren't polish items; they're prerequisites for Windows support.

### The control-plane named pipe listener

`internal/ctlapi.Listen` **fails closed** on Windows: `peerCredSupported == false`, and it returns
`platform.ErrUnsupportedPlatform` directly. In other words the daemon can't start on Windows, and
`agenthub connect` (the stdio gateway) takes the degraded path that doesn't depend on the control plane.

The reason: the Go standard library has no named pipe server. Building one requires
`github.com/Microsoft/go-winio` (`winio.ListenPipe` accepts an SDDL), and the constraint on this task was
**don't touch `go.mod`**.

The seams are already in place; filling this in for M2+ means:

1. `go get github.com/Microsoft/go-winio`;
2. Add `listener_windows.go` to `internal/ctlapi`:
   `winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: sddl, MessageMode: false})`,
   where `path` = `platform.CtlSocketPath()` and `sddl` = `platform.Resolver.CtlPipeSDDL()`;
3. Peer identity no longer needs an `SO_PEERCRED` equivalent — the SDDL already keeps non-owning users
   out at the kernel level before the connection is made, which is a stronger layer than Unix offers
   (a 0700 directory on Unix can be misconfigured; an SDDL can't).
   The Windows branch of `credListener` should simply admit, with a comment stating "authorization
   happens in ListenPipe, not in Accept";
4. The dialing side in the `api` package likewise needs `winio.DialPipe`; `platform.IsPipePath` exists
   precisely so it can pick the branch.

**Acceptance criteria (not done until there's a Windows machine)**: two users on a multi-user Windows box
running daemons simultaneously without interference, a non-owner user being refused when connecting to
the pipe, and the data directory landing correctly after spawning the gateway from a real MSIX-packaged
client.

### The registry's cross-process lock

Both functions in `internal/registry/flock_stub.go` (`!darwin && !linux`) return `errors.ErrUnsupported`,
so `registry.Open` / `Update` / `Reload` **all fail** on Windows. This sits even earlier than the named
pipe: no registry means no configuration, so neither the gateway nor the CLI works on Windows. That
file's comment still says "Windows (LockFileEx) scheduled for M2" — M2 shipped without this item, so the
comment expired ahead of the code.

Filling it in means using `LockFileEx` / `UnlockFileEx` (`golang.org/x/sys/windows` is already a
dependency; no new module needed), with semantics aligned to the Unix side: exclusive, non-blocking, and
a failure that can be identified as "held by someone else." `internal/skills` and `internal/audit` each
have a stub of the same shape; fill them all in at once.

### The control pipe doesn't distinguish build channels

`windowsCtlEndpoint` derives the pipe name from the SID alone, so under the same user, dev and release
compete for the same `\\.\pipe\agenthub-ctl-<sha8(SID)>` — the data directories are already separated by
channel, but the endpoint isn't. Channel separation on Unix is transmitted via "the endpoint is
`<run>/ctl.sock` → the run directory follows the data directory," and since the Windows endpoint isn't a
filesystem path, that chain simply doesn't exist there.

Mind the trap a previous attempt fell into. The pipe name is a **frozen identifier** (canonical.md
§1/§2), so the release name can't move; and it **must not** be derived from `dirName` — that was tried
once, and the result was that "rename the data directory" silently became "rename the protocol." The
correct shape is to give the dev channel **a second, equally frozen** name (for example
`agenthub-ctl-dev-<sha8(SID)>`) rather than splicing the channel into the existing name's derivation.
That requires the Resolver to know the build channel, i.e. exactly the "decide by build channel" the
Unix side deliberately avoids — unavoidable here, because there is no environment variable to carry it.

Do it alongside the named pipe listener once real hardware is available: adding a new frozen identifier
on a platform that has never been run is freezing an unverifiable guess into the ABI. To verify, flip
the windows row of `TestDevResolverSeparatesFromRelease` to `endpointSeparates: true` and require it to
pass.

### Other to-dos

- Explicitly tighten the data directory's ACL (today it relies solely on `%APPDATA%`'s default per-user permissions).
- Several POSIX helpers in `internal/cli` (`setsid`, `noecho`) already have `_stub.go` branches whose behavior is "do nothing," untested in practice.
- Tests using `syscall.Kill` in `api/dialorstart_test.go` and `test/e2e` don't compile under `GOOS=windows`.
  This doesn't affect `GOOS=windows go build ./...` (the CI gate), but it does mean `GOOS=windows go vet ./...` goes red.
  When Windows CI is actually stood up, those tests will need platform tags.

---

## 3. How to check for yourself

```bash
GOOS=windows go build ./...       # must be fully green (the CI gate)
go test ./internal/platform/      # injection-based unit tests for the Windows resolution logic, run on macOS/Linux
```
