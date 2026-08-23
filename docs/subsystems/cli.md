# The CLI

> **Answers** how the command tree is built, what each exit code means, and which commands work with no daemon.
> **Not here** the naming rules → [conventions.md#command-naming](../conventions.md#command-naming); what a command writes → [registry.md](registry.md).
> **Kept true by** `internal/cli/tree_test.go`, the frozen error golden files, and `TestAShippedPageNeverRecommendsWhatItWithholds`.

`internal/cli` is the entire `agenthub` command tree: offline registry editing, online control-plane
operations, and one set of exit codes and `--json` envelope over both. `main.go` is deliberately thin, so
everything testable lives here and the tree can be driven hermetically.

`Main(Options) int` is the **only** place that classifies errors and reports them; every `RunE` returns
typed errors and never prints.

## The exit code table is frozen

| Code | Meaning | Triggered by |
|---|---|---|
| 0 | success | — |
| 1 | generic error | downstream, network, internal |
| 2 | usage error | arguments, unknown flag, unknown subcommand |
| 3 | resource not found | server, profile, secret, skill, session, tool, token |
| 4 | daemon offline and required | `DaemonDownf` |
| 5 | authentication failure | the OAuth flows; a downstream answering 401/403 to `server test`; a secret file that will not decrypt |
| 6 | refused by a guard | a skill's integrity pin, the spawn guard screening a generated `docker run` |
| 7 | lock contention, or a state file corrupt and **unable to self-heal** | the three lock ladders, `registry.UnreadableError`, the skills corrupt-state path |

The mapping lives in exactly one place, `ExitCodeFor`.

**A non-zero exit has two paths, and only one of them carries an error envelope.** The `Triggered by`
column above names the typed constructors — the errors `ExitCodeFor` classifies. `silentExitError` is
the other path: five commands render their outcome through the output layer and then return a bare
code, so `--json` still answers `{"ok":true,…}` while the exit code carries the verdict. `doctor`,
`skill verify` and `secret migrate` exit 1; `calls verify` exits 7; `daemon status` exits 4. For those
five the code is the only place the verdict lives, and `ok` answers whether the command produced its
report rather than whether the report is good news — a distinction `skill.go` states at its own call
site and `skills/agenthub/SKILL.md` now states for all five.

Two of the five read oddly against the table read literally, and deliberately so: `daemon status`
exits 4 to *report* that the daemon is down, which is its answer and not a command that needed one,
and `calls verify` exits 7 for a ledger that did not verify, which is a stable verdict rather than the
retryable contention the row describes.

**"A cobra parse error = exit 2" is guaranteed by construction**: `SetFlagErrorFunc` funnels flag errors
into `Usagef`, typed argument validators replace cobra's, and every group uses `cobra.ArbitraryArgs` plus
`groupRunE` so an unmatched subcommand becomes a typed usage error rather than cobra's untyped "unknown
command".

**The help-flag hole has two doors, and closing one is not closing it.** cobra answers a help flag *before*
`RunE`, so `agenthub secret get --help` printed the group's page and exited 0 — the answer a real
subcommand gives, making a nonexistent command look like one that exists — and `agenthub help secret get`
is the same question spelled differently. `helpRequest` reduces both to one path, scoped to that hole
alone, and a test walks the tree from the other side in all three spellings, because a check running before
cobra could break `--help` everywhere.

**"Already-healed quarantine" degrades to a warning and does not consume exit 7.** Exit 7 means corrupt
**and unable to self-heal**.

**Error text is frozen by golden tests**: machine code, exit code, message and hint, because agents and
scripts use all four.

## Online and offline

Every `session` command requires the daemon — a session is a runtime object, never persisted — and offline
is exit 4, never an invented answer. `calls`, `events` and `logs` **work offline**, because those records
describe what already happened; sharpest for `logs`, since a stdio gateway writes its log with no daemon in
the picture and requiring one would refuse exactly the installation with the most to explain.
`server tool` is offline-capable too: choosing what a server offers must not require starting it.

Registry writes go direct and offline through `registry.Store.Update`.

## Credentials are never printed, guaranteed at the type level

The `secret` group's result types have **no value field at all**, `auth status` reports only issuer,
expiry, mode and refresh-token-presence, and there is no `--show`. `token create` is the one exception,
since its plaintext must leave the process once.

`readNoEcho` **errors rather than reading from a non-terminal fd**, and a defer alone did not restore the
terminal: `ISIG` stays enabled so Ctrl-C works at a hidden prompt, and Go's default SIGINT disposition
terminates without running deferred functions, so the restore happens **before** the signal is re-raised.

**`server ls` can display header values verbatim**, because a registry entry never holds a credential —
values are `${SECRET_X}` placeholders resolved at connect time. Which vault keys an entry needs comes from
`downstream.SecretKeysIn`, not a local `${…}` scan. **One exception**: a *literal* `Authorization` value is
a pasted token, so the human view refuses to read it back to a terminal — a deliberately narrow test, since
guessing which other header authenticates would start hiding ordinary configuration.

**The browser is launched with detached streams AND a detached environment.** Streams, because a chatty
handler on stdout would corrupt the NDJSON progress stream; environment, because `auth login` runs holding
`AGENTHUB_SECRET_KEY`, every `AGENTHUB_SECRET_*` value and any bare secret the operator opted in — and the
browser is the one child whose descendants this process does not control. `browserEnvNames` is an **allow
list**, and `browserEnv` never returns nil, because `os/exec` reads a nil `Env` as "inherit everything".
A non-`http(s)` URL is refused outright, since it came from a metadata document.

## What the listings promise

**The `AUTH` column reports what is STORED, never whether it works** — the line the ban on a persisted
`needsAuth` draws. On its first-match-wins ladder a missing secret outranks everything, so the CLI and
`ComputeHealth` cannot disagree about one server; a literal `Authorization` header outranks the stored
credential, because the bearer is never sent past it; and the last rung does **not** guess. Reading it is
**index-first**, a cost rule rather than an optimization: a command that pops a keychain dialog is one
people stop running. Failure direction is fail-open for the listing, fail-visible for the cells — an
unreadable vault still prints every registry fact, but its cells read `error`, never `-`.

**The action and the hint on one row must name the same repair, and one predicate decides both.** An OAuth
expiry reported `action: "login"` whatever was stored, while the sentence printed beside it already offered
`auth refresh` when a refresh token existed — so a caller reading the field that exists *for* callers was
sent to a browser for a renewal that runs unattended. Both now answer from the same `HasRefreshToken`.

**`auth status`, `server ls` and `GET /v1/auth` are three wire shapes over ONE decision**
(`ctlapi.OAuthLifecycleOf`). The shapes stay separate because the surfaces genuinely differ; what is not
different, and was written three times anyway, is the lifecycle in the middle — refused grant, token
present, past its deadline, inside the grace, renewable without a human. The decision lives in
`internal/ctlapi`, which `internal/cli` already imports, because a copy in the CLI is invisible to the HTTP
face — which is exactly how the `revoked` state reached one and not the other.

**`visibility` is the arithmetic behind "everything is healthy and my client still sees no tools".** Three
states stay distinct because they need different repairs: a **disabled** server reaches nobody whatever the
profiles say, a profile that **excludes** it is named, and a binding naming a **missing** profile
fail-closes to an empty scope, which from outside looks exactly like deliberate exclusion. It is computed
from the registry alone, so the answer survives on the broken machine, and it is an upper bound: an agent
token's own allowlist can take more away.

**`server inspect`'s `spawns` line is the exact `docker run` argv the spawner would execute**, rendered by
the same translator the spawn guard screens, so "isolation a config claims must be delivered" is checkable
by reading. Two neighbours exist for the same reason: the tool allow list prints on **every** report
including "all", since the absence of a rule is what a missing line cannot express, and the cache line
distinguishes "no catalogue stored" from "0 tools".

**`server tool ls` reads the catalogue with the gateway's own ranker** and merges `ServerToolsLayer`, so
the listing screens through `ScopeAllows` exactly as a live call does rather than re-deciding visibility.
`profile tool ls` stacks the pinned-profile layer on top and attributes the difference between the two
merges to the layer that closed it, so the blame comes from the same merge as the verdict.

**`(default)` is a display row, not an object.** A profile name may not start with `(` so the token cannot
be shadowed, and the default object stays out of the JSON array so a script walking it keeps getting names
it can pass back to `profile rm`.

**The two dangling directions are reported in different places, and both must stay reported**: a client
bound to a missing profile is flagged per row, while a missing *active* profile fail-closes every client
that follows it and no row can carry that, so it lives on the listing.

## The daemon commands

**An ownerless start is refused.** `daemon start` requires either the owner handshake or an explicit
`--headless`; anything else is `E_DAEMON_UNOWNED`. The check cannot be deferred to the daemon: "nobody owns
me" and "my owner has not claimed me yet" are the same state at different moments, and a daemon that
guesses is either unstoppable or shuts itself down during an ordinary launch. `daemon restart` checks
admission **before** it stops anything.

**A backgrounded start writes the child's stderr to a file rather than a pipe** — the parent exits once the
child is ready, and a pipe with no reader would SIGPIPE the daemon — and reports a child that died before
readiness with its real failure rather than a timeout.

**The pid `stop` signals comes from the ping, never from `run/daemon.json`.** That file names a process but
does not identify one, and the OS may reuse the number — which is how `daemon stop` came to SIGTERM an
unrelated process and, with `--force`, its whole group. A successful ping proves both that a daemon is there
and that it is naming itself over a 0600 peer-credentialed socket. **Nothing answering means nothing can be
verified, so nothing is signalled**, even with `--force`: the deliberate cost is that a badly wedged daemon
can no longer be stopped from the CLI.

**Shutdown deletes the run directory's shared paths only while they are still this daemon's.** `Shutdown`
closes the listener *before* draining and Go unlinks the socket on close, so for up to the grace period the
run directory looks free: a replacement can bind and write its own `daemon.json`, and the departing
daemon's cleanup would then unlink a **live** socket. `ownsRunFiles` gates both removes on `daemon.json`
still naming this pid, and every doubt answers false. Every exit goes through that one cleanup, including a
data plane that refused to come up.

**`daemon status` reports the owner**, taken from the ping rather than the file. Omitting the field for a
headless hub would read as "this build does not know", which is the state an operator wants to tell apart.

**`doctor` only reads, never writes**, and deliberately does not call `registry.Open`: opening the store
would create the directory, its documents and a lock file, turning a diagnostic into a writer that
incidentally fixes what it reports on. `--fix` does only safe self-healing; destructive repairs are
suggested, never executed. `registry:quarantined` is the only check reporting "data was set aside", and it
has to exist separately, because quarantine writes an empty new document after which `registry:servers`
reports "readable" — true, and at the moment of "all my servers are gone" the worst thing to read.

## The help page

Grouped by task phase — Setup, Wire up, Configure, Daemon, Manage, Diagnose, Observe, plus the machine
entry point `connect` — and a release build withholds **Daemon and Manage**, narrowing what the binary
*teaches* while every withheld command stays registered and runnable.

Two rules decide membership. **The withheld half is split on one testable question, does this command need
a running daemon.** And **a command a shipped page recommends must be a command that page teaches**,
learned four times over — each time a `Short`, a `Long` or an `Example` written elsewhere by someone not
thinking about the release page at all, which is precisely what an argument in a comment cannot catch.
That second rule is now a check: it walks the release page's visible commands and fails on a help text
quoting `agenthub <withheld>`.

Runtime error hints stay out of scope: "run `agenthub session ls`" inside a `session` error is read by
someone who already found `session`.

**`agenthub manual` prints the binary's own SKILL.md**, compiled in, so the copy an AI client reads and the
binary it drives are one artifact. It is a command rather than a flag, and deliberately not in the `skill`
group: that group manages a library the operator imports from elsewhere and every invariant it has is built
for text agenthub did not write, while this document is the binary describing itself. Output is the bytes
and nothing else — a header or a trailing hint lands inside the file the caller redirects into.

## internal/cli/output

The CLI's only rendering layer: human output and the `--json` envelope are fed by **the same data value**,
so the two cannot drift semantically. `Data` has one method, `Human(w) error`, and `Printer.Emit` marshals
that same value into the envelope's `data` field.

**In JSON mode the whole envelope is one line on stdout**, so scripts parse line by line; in human mode
warnings and errors go to stderr, leaving stdout for tables and snippets. The envelope shape is frozen:
success always has `data` and `warnings` (never null), failure always has `error` with at least `code` and
`message`.

**Four commands stream progress**, and the list decides how a script must parse them: `auth login`,
`server test`, `server enable` and `doctor`. A consumer treating any of these as a single JSON object
instead of NDJSON fails on the first progress line. In JSON mode each step is a compact object on its own
stdout line and **the final envelope is always last**; in human mode progress goes to stderr, because
progress is not a result.

**Neither `Progress` nor `Fail` returns an error.** A progress line that cannot be written must not stop
the command from reporting its real result, and when reporting a failure itself fails there is no better
remedy than best-effort.

## Owed

**The `events` table hides one of two identities.** `eventSubject` fills a single SUBJECT column from the
first non-empty of server, client, session, so a record carrying a server AND the client whose gateway
observed it shows only the server — and every server-scope event is such a record. The reader also cannot
tell which of the three kinds a name is. The GUI's Events table already gives Server, Client and the
session-as-client their own columns, and its reasoning transfers word for word. Nothing is lost from
`--json`, so this is a rendering gap rather than a recording one; adding a column changes the shape of a
command's output, which is a feature branch and not a tidy pass.

**`logs -f` can re-print a record.** `readLogBatch` takes the file size from a `stat` *before* reading, then
reads from the old offset to EOF and stores that pre-read size as the new offset. Anything a writer appends
between the stat and the reader reaching EOF is printed on this tick and again on the next. The fix is to
advance the offset by what the read consumed. Established by reading, not by a test: reproducing it needs a
write interleaved inside the read, which nothing in the suite can currently schedule.

**All three timestamp followers carry the RECORD's `time.Time`, never the stamp they printed.** Taking the
cursor back out of the rendered row is a real defect: a row renders its timestamp at second precision, so
the cursor would advance a whole second and the next scan would discard every record of that second not yet
printed. Two followers lost records that way before this rule. `ScanFramesSince` is deliberately inclusive
of its bound — that bound is what lets it skip whole day partitions — so the tie is dropped in the reader,
where nanoseconds are in scope.

**`daemon logs` does not share the record readers' flag vocabulary.** `logs`, `events`, `calls tail` and
`server logs` each take a `--since` that accepts a duration, an RFC3339 time or `all`, and a `--limit`
whose `0` means all — the set `skills/agenthub/SKILL.md` teaches as shared across "the four record
readers". `daemon logs` takes a plain `time.Duration` and has no `--limit` at all, so `--since all`
answers `invalid duration "all"` and an RFC3339 stamp answers `unknown unit "-"`, which is Go's duration
parser talking to somebody who typed a timestamp. It is the older single-process view of `daemon.log` and
predates the merged reader, and the skill does describe it separately — but a reader who learned the
vocabulary on `logs` meets the difference as a parse error rather than as a documented narrowing.
Widening the flag changes what a command accepts, which is a feature branch and not a tidy pass.

**The four record readers do not agree on which zone a stamp is printed in.** Three treatments, not one
rule: `.UTC()` in `events.go` and `serverlogs.go`, `.Local()` on `calls_read.go`'s human row, and no
conversion at all in `logs.go`, which formats whatever offset the record was parsed with. So `events`
and `server logs` print `Z` in both faces, `logs` prints the local offset in both, and `calls tail`
prints `Z` in `--json` and the local offset in its table.

The pair that straddles it is the pair the ledger's own join is documented for: a call's ledger row and
its wire frames share a call id and print different calendar days for the same instant, eight hours
apart on this machine. One envelope straddles it alone — `calls tail --json` echoes its `since` bound
with a local offset beside events stamped `Z`.

Nothing here is ambiguous: every stamp carries its offset, and `--since` takes an RFC3339 instant in
any zone. It is a correlation cost, not a correctness one, and settling it changes what four commands
print — a feature branch, not a tidy pass.
