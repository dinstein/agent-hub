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
list the Daemon or Manage groups (daemon / session / events / token, approval / grant / config / tool /
audit / activity / skill). `doctor` is the one back-half command a release still shows, in a Diagnose
group of its own — a help page that teaches the everyday path has to name what to run when a step of it
fails. "Which one am I holding?" no longer needs a test run to guess.

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

## Running this checkout where Homebrew installed one

```bash
make install-to-brew                     # build as a release ships it, install at $(brew --prefix)/bin/agenthub
scripts/install-to-brew.sh --restore     # give the entry back to Homebrew
```

**Nothing is published** — no tag, no Release, no tap commit. This is `make bin-release` plus a
destination, and the destination is the point: `agenthub client connect` writes **the absolute path of
the running executable** into the client's config, and features are developed in worktrees that get
removed once they land. A client wired to `bin/agenthub-release` is therefore wired to a path that
stops existing, in a file this repository never touches again. At the Homebrew path a real client
exercises the new build with no config change at all.

A dirty tree is allowed here, unlike `scripts/build-release-artifacts.sh` — testing uncommitted work is
the whole reason to run it, and the version still carries `-dirty`. What the script does refuse is a
**dev-channel binary**, the same assertion the formula's `test do` block makes: at the installed path a
dev build resolves `AgentHubDev` while every client keeps invoking the same command name, so the
servers configured through the release are simply gone and nothing reports an error.

What it replaces is the symlink Homebrew keeps at `$(brew --prefix)/bin/agenthub`, with a regular file
— which is also how it tells the two apart on the next run without keeping state anywhere, since
Homebrew never puts a regular file there. A symlink pointing somewhere else belongs to something else
and is refused rather than guessed at. The keg itself is untouched, so `brew list --versions` keeps
reporting the released version while `agenthub --version` reports what actually runs; the script says so
out loud. `brew upgrade agenthub` relinks over the local build on its own.

Replacing the file does not replace the **process**: a daemon started from the previous binary keeps
serving every client until `agenthub daemon restart`. New CLI against old daemon reads as a bug in
whatever is being tested, so the script checks and says which case it found.

## GitHub Actions

Pushing a tag (`v*`) triggers `.github/workflows/release.yml`:

- **CLI**: an ubuntu runner cross-compiles every platform, pure Go with no cgo
- **GUI**: a macOS runner builds a universal binary natively (Windows / Linux not yet enabled)

`workflow_dispatch` rehearses the whole thing without a tag; the publish job is the only one that
notices, and it skips.

## Where the artifacts live

**Here.** This repository is public, so `brew install` fetches the tarball with no credentials and the
artifacts sit beside the source they were built from.

That destination is **one decision with two readers**: the upload target, and the download URLs written
into the formula. Both used to be left to a default, and the two defaults disagreed — the upload fell
back to this repository, `homebrew-formula.sh`'s default named the tap. The combination fails in the
worst way available: every job green, valid Ruby, real sha256s, and a 404 on the first `brew install`
run anywhere but here. The workflow now spells `${{ github.repository }}` in both places, and
`TestReleaseWorkflowUploadsWhereTheFormulaPoints` and `TestReleaseScriptsAgreeOnTheArtifactRepo` hold
the workflow and the scripts to their halves of that agreement.

**The tap still holds the assets for `v0.11.0` and `v0.12.0`, and they stay there.** Those releases
shipped while this repository was private, and the formula each of them shipped pins the sha256 of
*that* upload. A same-named tarball rebuilt here hashes differently, so a URL and the hash beside it
must always come from one upload — the tap's old assets are not interchangeable with anything.

Two settings control the tap, and neither touches the Release:

| Setting | Kind | Value |
|---|---|---|
| `HOMEBREW_TAP_REPO` | repository variable | `dinstein/homebrew-agenthub` |
| `HOMEBREW_TAP_TOKEN` | repository secret | a token with `contents: write` on the tap |

The token cannot be `GITHUB_TOKEN`, which reaches only this repository; the Release itself needs no
such token, since it is written here. **A variable with no token fails in `verify`, before anything is
built** — with the variable set somebody is expecting the tap to be updated, and finding out after the
DMG is packaged costs twenty minutes. Neither set is a supported state: the Release publishes and the
tap keeps serving the previous version, with a warning in the run summary saying so.

## The Homebrew tap

Two files travel to the tap, and `scripts/tap-sync.sh` puts both there as **one commit**:

| File | What it is |
|---|---|
| `Formula/agenthub.rb` | Generated by `scripts/homebrew-formula.sh`; installs the prebuilt binary |
| `skills/agenthub/SKILL.md` | Copied from this repository; tells an AI client how to drive that binary |

They are not independent. The skill is written against a specific released surface — it says so in its
own opening — so a tap where the two land in separate commits has a window in which either one
describes a version the other does not.

**The skill is maintained here and generated into the tap.** It used to be edited in the tap directly,
which is what a second copy always becomes: the copy an agent happened to read decided which CLI
surface it believed in. The tap's copy carries a generated-do-not-edit banner after its frontmatter,
and that banner is the only difference between the two.

`scripts/tap-sync.sh <tap-checkout> <tag>` with the formula argument omitted syncs **only the skill**.
The skill's revisions land faster than releases do, and without that path the only way to publish a doc
fix would be to cut a version that changes no code.

Both release paths — the workflow and `scripts/release-local.sh` — go through that one script, which
`TestBothReleasePathsSyncTheTapThroughOneScript` enforces. A caller that inlined its own copy would
still commit, still push and still go green; what it would do is leave whichever file it forgot at the
previous release.
