# build/

Platform-specific GUI packaging. Each subdirectory is a [Task](https://taskfile.dev)
namespace included by the root `Taskfile.yml`.

## The layout contract

Every platform's package must place the GUI binary and the CLI binary in the
**same directory**:

| Platform | Location |
|----------|----------|
| macOS    | `AgentHub.app/Contents/MacOS/{agenthub-gui, agenthub}` |
| Windows  | `AgentHub/{agenthub-gui.exe, agenthub.exe}` |
| Linux    | `AppDir/usr/bin/{agenthub-gui, agenthub}` |

This is not a packaging preference. `api/dialorstart.go` resolves the daemon as
"the sibling of my own executable", falling back to `$PATH`. Break the sibling
relationship and the GUI silently fails to find the daemon: the user sees a
socket-missing error describing a launch that never happened. The layout is
codified in `build/Taskfile.common.yml`.

## Adding a new platform

A platform Taskfile earns its include in the root `Taskfile.yml` when it:

1. Satisfies the layout contract above.
2. Has a `package` task that writes to `{{.DIST}}/` (the `dist/` directory at
   the repo root) and cleans up any intermediate staging directories.
3. Stamps `{{.VERSION}}` into the binary and bundle metadata.

Nothing in the Makefile may depend on any file in this directory — "the GUI is
optional by construction" (AGENTS.md) means `make ci` must pass if `Taskfile.yml`
is deleted.

## Subdirectories

### `darwin/`

macOS packaging. Builds a universal `.app` (amd64 + arm64 merged by `lipo`),
signs and notarises if credentials are present, wraps in a DMG. **macOS only**
(uses Objective-C cgo through wails; cannot cross-compile).

Entry point: `make release-macos` (→ `wails3 task darwin:package`).

### `windows/`

Windows packaging. Produces one portable zip per architecture (amd64, arm64).
The zip unpacks to `AgentHub/{agenthub-gui.exe, agenthub.exe, README.txt}`.
**Cross-compiles on any host** (WebView2 is loaded at runtime through COM;
wails v3's Windows backend is pure Go, no cgo). Requires `wails3` on `$PATH`
for `wails3 generate syso` (icon + manifest + version resource).

Entry point: `make release-windows` (→ `wails3 task windows:package`).
