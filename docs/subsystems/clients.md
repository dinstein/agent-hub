# Writing into other applications' config files

> **Answers** how `client connect` finds, edits and verifies a config file agenthub does not own.
> **Not here** what a client is allowed to see afterwards → [../model.md](../model.md).
> **Kept true by** `FuzzBlankJSONC`, `FuzzSpliceEntryKeepsEverythingElse`, `FuzzScanTOMLServers`, and `TestWindowsUserPathsAreTheOnesClientsRead`.

`internal/clients` detects where an AI client keeps its config, writes the agenthub gateway entry in,
takes it back out, and reports what is actually there. Everything it touches belongs to someone else,
which is the source of every rule below.

## A shape-driven adapter table

Behaviour is driven by the **shape** of the config, not by a branch per product.

| Shape | Meaning | Rows | Clients |
|---|---|---|---|
| `ShapeServerMap` | a JSON file with `{"mcpServers": {…}}` at the top level | 7 | claude-code, claude-desktop, cursor, windsurf, cline, roo-code, gemini-cli |
| `ShapeNested` | the same map, buried under a key path inside a larger document | 2 | vscode (`mcp.servers`), zed (`context_servers`) |
| `ShapeTOML` | a TOML document — **detect only, never rewrite** | 1 | codex |
| `ShapeYAML` | a YAML document — **detect only, never rewrite** | 1 | continue |
| `ShapeRemote` | no config file on this machine at all | 1 | open-webui |

Adding a client is one more row in `table.go`, not one more code path. `Shape.Writable()` is true only
for the two JSON shapes. A row's `locs` is ordered project-first, but that is **read priority** — on a
duplicate name the project-level definition wins — not a write preference.

**Every row's `home` map answers for Windows too**, through one of three builders: `sameOnAll` for
CLI-shaped clients whose dotfile sits in the profile identically everywhere, `perOS` where the base
differs and not just the segments, and `vscodeUserDir` for the extension-hosted ones. Keeping the two
conventions apart is the point: copying one onto the other is how a write lands somewhere the client
never reads. Zed is where it bites (`.config/zed` on macOS and Linux, `%APPDATA%\Zed` there), and
`%APPDATA%` is read from the environment rather than rebuilt under the home directory. Claude Desktop
carries the one platform-specific branch of its own, because an MSIX install reads a virtualized
`%APPDATA%` and the documented path is one the packaged application never opens.

None of it has run on Windows hardware — [windows.md](../windows.md) names `client connect` writing a
file the client actually reads as an open verification item.

## Choosing the file

**Default writes go to the user level** (`DefaultPlacement = User`). The entry carries the absolute path
of this machine's agenthub binary, and project-level files are meant to be committed, so defaulting to
project would commit a path that holds only on one machine. Which servers a client can see is decided
by `internal/scope`, never by which file the entry was written into.

**An explicitly specified placement is either honoured exactly or refused.** `PathFor` returns `""` for
a client lacking the location and callers error on that; they never redirect the write elsewhere,
because writing the gateway entry into a file nobody named is worse than an up-front refusal. `--path`
plus `--placement` is a usage error, not a place to invent a precedence.

**`DisconnectDefault` is the backstop for the default write target having moved.** A disconnect with no
target checks the default first and only then, if no agenthub-owned entry is there, this client's other
location. It is not a search. A fallback location that fails for another reason — unparseable,
oversized, denied — **returns that error** rather than being skipped: a file agenthub refuses to touch
must not be reported as "nothing in there".

**`locationFor`'s match order guarantees the section is deterministic**: exact path equality, then equal
basename (what makes `--path /tmp/x/settings.json` behave like a real settings.json), then this client's
primary location. A path that does not match never guesses at another client's shape.

## Reading

**macOS TCC: `Detect` only stats, never reads.** Reading another application's data directory triggers
the privacy dialog, and a bulk scan popping a dozen of them is worse than not scanning. Content reading
happens only in `Inspect` and `Import`, single-client user actions where the dialog is explicable.

**"There is no such file" and "you are not allowed to look at this file" are never conflated.** A denied
access becomes a `*PermissionError` with remediation text and HTTP 403, not 404 — the two cases call for
opposite user actions. `classifyAccess` classifies as denied only on `errors.Is(err, fs.ErrPermission)`.

**A parse failure must error and must never destroy.** A file that exists but will not parse aborts the
whole operation with a `*ParseError`, leaving the file untouched. JSONC counts as unparseable on the
non-splice path, and the error carries the specific JSONC diagnostic — the most common reason a real
`settings.json` fails, where "invalid JSON" would read like a bug. Every `*ParseError` carries a
hand-pasteable snippet.

**Anything over `MaxConfigSize` (64 MiB) is refused before any read.** The stat size is checked first,
and `readLimited` catches it again with an `io.LimitReader` in case the file grows in between.

**`ConnectState` fails loud, and that is the whole point of it.** "Is agenthub wired into this client?"
has five answers: `connected` (some location holds an entry agenthub itself wrote — decided by
ownership, never by name), `not_connected` (every location was opened and understood, none had one),
`denied`, `unreadable` (there, but agenthub refuses to interpret it), and `unknown` (nobody looked). A
positive finding wins outright; after that the loudest doubt wins, so a location agenthub could not see
never degrades into "not connected". It returns the placements holding the entry alongside the state,
because "connected in the user file while the project file still holds one" is exactly what a disconnect
has to know.

## Writing

**Unknown fields and foreign entries are preserved byte for byte** — every level from the document root
down to the server map is held as `map[string]json.RawMessage`.

**Backups are centralized, not in-place.** The original goes to
`<data>/backups/clients/<client>-<ts>Z.json` (0600, rotated at `DefaultKeepBackups = 10`) rather than a
sidecar: a project-level `.mcp.json` lives in a git working tree, and a backup beside it would dirty
`git status` on every connect and risk committing someone else's credentials. Rotation is best-effort,
since failing to delete an old copy must never fail a connect whose new backup already landed. **If the
backup cannot be written the whole operation fails and the target is untouched.**

**`Disconnect` identifies by ownership, never by name.** `ownedBy` requires the entry's args to contain
both the `connect` subcommand and a `--client` value equal to this client's id. An entry that merely
happens to be named `agenthub` is not ours; an entry the user renamed that still points at our gateway
is.

**Writes are atomic and preserve the original permissions.** New files are created 0644 — a
project-level `.mcp.json` is meant to be committed, unlike registry documents at 0600 — and existing
files keep their own mode. A result byte-identical to the current content returns `Changed: false` and
does not write, so repeated connects are idempotent.

**Only rewrite documents that round-trip losslessly.** The rule is about **re-encoding**: TOML and YAML
re-encoders drop comments, key order and anchors, which is a config-destruction machine wearing a
helpful hat. Those clients get detection plus one precise manual snippet, and `Connect` fails loudly
with the snippet rather than half-working.

**Delegation: agenthub does not re-encode the document, it asks the tool that owns the format.** A row
may carry a `delegate`, and codex does, so `Connect` runs `codex mcp add` instead of printing advice.
Three properties make it delegation rather than a shrug: the file is **backed up first**, exactly as
agenthub's own writes are; the result is **verified by re-reading it**, so a CLI that exits 0 having
written nothing is a failure; and a delegate that is absent, fails or leaves the wrong state **falls
back to the instructions** rather than reporting a success nobody checked. Execution is refusable per
invocation (`--manual`) or per machine (`AGENTHUB_NO_CLIENT_CLI=1`), and the environment can only ever
**forbid** it — a variable that could switch execution back on for a caller that passed `NoDelegate`
would let a program run other programs behind its caller's back.

## The JSONC splice

Zed's `settings.json` ships with a comment header and VS Code's is JSONC by convention, so the default
install of both was a client agenthub refused to touch. `jsonc.go` reads them with a comment-blanking
pass — comments and trailing commas become spaces of the **same length**, so offsets in the blanked copy
are offsets in the original — and writes by replacing the bytes of agenthub's own entry and nothing
else. The user's comments, key order and indentation survive.

**The safety does not come from the locator being right; it comes from proving the result.**
`verifySplice` runs before anything reaches the disk: the edit must parse, must differ from the original
in exactly the entries agenthub meant to change (deep-compared with those entries removed from both
sides), and must carry byte-identical comments. Any doubt leaves the file untouched. A shape the locator
cannot walk is refused rather than guessed at.

**A duplicated key resolves to the LAST occurrence, because that is the one the file's owner reads.**
`encoding/json` keeps the last and so do the client applications, but the locator once walked to the
first, so an edit landed in a section nobody reads — invisibly, because the document came back
byte-identical and `verifySplice` could not object when both of its decodes went through `encoding/json`
and agreed with each other. This is the one case where proving the result cannot save a wrong locator,
which is why the rule lives here rather than in the verifier.

**Owed: `dropChanged` unwinds only one created section level, which breaks the default
`agenthub client connect vscode`.** VS Code's user placement is the two-level section
`["mcp","servers"]`. Against a `settings.json` with comments (so the splice path is taken) and no `mcp`
key, `spliceEntry` correctly inserts `"mcp": {"servers": {"agenthub": …}}`, but `dropChanged` deletes
only `parent["servers"]`, so the created `"mcp": {}` survives in the after-document while `before` has no
`mcp` at all: the deep comparison fails and the connect is refused, accusing the edit of changing
something it did not. The fix is to walk the section path back up, dropping every ancestor the deletion
left empty. The single-level cases pass, which is why the tests and the splice fuzzer — all one-element
sections — miss it.

**Owed: an unchecked read-to-rename window** (`jsonfile.go`). The rendered result is renamed over the
target without confirming the target is still the file that was read. VS Code, Zed and Cursor all
rewrite their settings on their own schedule, so a concurrent edit between the read into `c.orig` and
the rename is lost — and lost from the backup too, which preserves the stale `c.orig` rather than what
was actually on disk. Re-reading and comparing immediately before the rename would let it refuse and
back up what it observed.

## Reading a format we will not write

A row may carry `readTable`, and codex does: `scanTOMLServers` answers "is our entry in here?" for
`~/.codex/config.toml` while `Connect` still refuses it. Refusing to read bought nothing — it made
`client ls` say "?" for a plainly connected client, and made doctor assert "no agenthub gateway entry"
about a file it had never opened.

The scanner models exactly `[mcp_servers.NAME]` tables and the four keys agenthub uses, and **refuses
the whole document** on anything else: array-of-tables, an inline `mcp_servers = {…}`, an unterminated
string, an escape outside TOML's set. `ok=false` is the contract — callers report "unknown", and nothing
this scanner cannot read is ever converted into "the entry is not there". Scanned entries are re-rendered
as the JSON shape the package already speaks and go back through `summarise`/`ownedBy`, so ownership has
one implementation for every format.
