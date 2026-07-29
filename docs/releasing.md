# Releasing

Two release paths, with different artifacts for different audiences:

| Path | Artifact | For whom |
|---|---|---|
| GUI app | `AgentHub.app` (with the CLI inside) → DMG | Users who want a graphical interface, working out of the box |
| CLI only | The single `agenthub` file → tar.gz | Users who only use Claude Code / Cursor |

Both ship the same CLI binary; only the packaging differs.

## Version numbering

The one source of the version number is the `VERSION` file at the repository root.

```
0.1.0
```

At build time it's injected into the binary via `-ldflags -X main.version=` and also written into the
`.app`'s `Info.plist` (`CFBundleShortVersionString`). **Change it in one place, three places agree** —
the binary's self-report, the app's About panel, and the Release title never contradict each other.

When releasing, the tag must match `VERSION` (with a `v` prefix): `VERSION=0.1.0` ⇒ tag `v0.1.0`.
CI verifies this and fails outright on a mismatch — a shipped artifact whose version doesn't line up is
an incident that's extremely hard to diagnose after the fact.

A binary built from uncommitted changes carries a `-dirty` suffix in its version. That's deliberate: a
version number that can't answer "which build is this?" is more dangerous than no version number at all.

## Releasing macOS locally

```bash
make release-macos
```

This produces `dist/AgentHub-<version>-macos-universal.dmg`, containing:

```
AgentHub.app/Contents/MacOS/
├── agenthub-gui      # GUI main program
└── agenthub          # CLI / daemon
```

**The two binaries must be siblings**, and this isn't a layout preference: `defaultDaemonBinary()` in
`api/dialorstart.go` finds the daemon via "the directory alongside my own executable." Break that
relationship and the GUI's daemon launch falls back to a `$PATH` lookup, so what the user sees is a
socket-missing error rather than the startup that never happened.

Windows and Linux use different containers (directory → zip, AppDir → AppImage), but the sibling
relationship is identical, so the convention is cross-platform.

## Signing

Today we only do ad-hoc signing (`codesign --sign -`) and clear the quarantine attribute.
**Apple Developer ID signing and notarization are not done**, so Gatekeeper blocks the user's first
open; they need to right-click → Open, or run `xattr -cr /Applications/AgentHub.app`.

Notarization should be added before any real public distribution. It requires a Developer ID certificate
and an app-specific password, neither of which we have yet.

## Where the CLI gets installed

The GUI app writes **nothing** into `/usr/local/bin`. Reasons: writing to system directories requires
elevation, sandboxed apps don't have permission, and uninstalling the `.app` would leave a dangling
symlink behind.

Anyone who wants `agenthub` on their `$PATH` should install the CLI build through a package manager
(Homebrew) rather than digging the binary out of the app bundle.

## Paths in client configs

What `agenthub client connect` writes into the client config is **the absolute path of the running
executable** (`executable()` in `internal/cli/cli.go`). It's correct in both release shapes:

- CLI only: `/usr/local/bin/agenthub`
- GUI app: `/Applications/AgentHub.app/Contents/MacOS/agenthub`

The cost of the latter is that **the config breaks if the app is moved or renamed**. On the daemon side,
`agenthubExecutable()` leaves a `NonRegistry.Executable` override point so the GUI can inject the path it
resolved itself; staleness detection and a "reconnect" prompt aren't implemented yet.

## The dev / release dual channel

Isolation is **a property of the binary itself**, not of how you invoke it: a binary built without
declaring `CHANNEL=release` resolves to the development directory on its own. There's no environment
variable to remember, and therefore none to forget.

| channel | Data directory | `--version` |
|---|---|---|
| release | `~/Library/Application Support/agenthub` | `0.1.0-abc1234` |
| dev | `~/Library/Application Support/AgentHubDev` | `0.1.0-abc1234 (dev)` |

The two are **siblings, not parent and child**: a dev run can't reach the installed registry by walking
up one level, and `rm -rf` on one won't take the other with it.

```bash
make bin              # dev build (default) → bin/agenthub
make bin-release      # release build → bin/agenthub-release
make dev ARGS="status"
make dev-where        # which directory is this build actually using
make release-run ARGS="--help"   # the release build's equivalent of make dev
```

The two flavors land at **different paths**, so you can keep both around and compare. Their differences
go beyond the data directory: the release build's `--version` has no `(dev)`, and its `--help` doesn't
list the governance command groups (approval / grant / config / audit). "Which one am I holding?" no
longer needs a test run to guess.

**The default is dev rather than release, because the two failure directions cost asymmetrically.** A
release build mislabeled as dev costs an empty sandbox — visible to the user and easy to fix. A dev build
mislabeled as release writes into the installed registry and burns its one-shot OAuth refresh token —
unrecoverable even once you notice. So `go build`, `go run`, running from an IDE, and forgetting the flag
all land on the safe side.

An explicit `AGENTHUB_DATA_DIR` still takes precedence over the channel. CI, e2e, and anyone debugging two
sandboxes at once depend on it, and a build that quietly ignored it would make those scenarios look
broken on their own.

The socket lives at `<data>/run/ctl.sock` and follows the data directory, so the two channels' daemons
can't see each other. `TestDevResolverSeparatesFromRelease` pins this down — if it ever stopped holding,
the two channels would share one daemon and the isolation would be skin-deep.

Release artifacts are always release builds: the Taskfile's `common:cli` and `darwin:build:universal`,
and the release workflow, all pass `main.channel=release` explicitly.

## GitHub Actions

Pushing a tag (`v*`) triggers `.github/workflows/release.yml`:

- **CLI**: an ubuntu runner cross-compiles every platform, pure Go with no cgo
- **GUI**: a macOS runner builds a universal binary natively (Windows / Linux not yet enabled)

The repository is currently **private**, so Release assets require authentication to download —
`curl | sh` style installation isn't available, and the CLI-only path won't fully make sense until the
repository goes public.
