# Using agenthub

> **Answers** how to wire servers, profiles and clients up, and which record to open when something broke.
> **Not here** what the rules mean → [model.md](model.md); how any of it works inside → [architecture.md](architecture.md).
> **Kept true by** `test/e2e`, which drives these commands the way you do.

This is the guide for people using agenthub. Everything else under `docs/` is for people changing its
code.

## The everyday path

```bash
# 1. register a server — written down, still switched off
agenthub server add linear --url https://mcp.linear.app/mcp

# 2. switch it on; this connects first, and reports what it still needs
agenthub server enable linear

# 3. sign in, if step 2 asked for it (this enables the server too, so a
#    server you were always going to log into needs only steps 1 and 3)
agenthub auth login linear

# 4. connect a client — once, ever
agenthub client connect claude-code --dry-run   # look first
agenthub client connect claude-code
```

That is the whole loop for a first server. Step 4 happens **once per client**: the entry it writes runs
`agenthub connect --client claude-code`, so every server you add later is picked up without touching the
client's config again.

With no profile in play, a client sees every enabled server. For many setups that is the right answer
and you can stop here.

## Switching a tool off everywhere

A server offers everything it has, including anything it gains in a later version. Naming tools fixes
the set:

```bash
agenthub server ls                              # what the rules are today
agenthub server tool allow github --only get_issue,list_prs
agenthub server tool allow github --none        # offer nothing from this server
agenthub server tool allow github --all         # back to offering everything
```

`allow` **replaces** the list rather than adding to it, and you must say which of the three you mean: a
bare `agenthub server tool allow github` is a usage error, because the reading it would otherwise have
had — "offer nothing" — sits one forgotten argument from the opposite of the intent.

This is for every client at once, and it is an allow list: on the day the server adds a tool, a rule
keeps the new one out until you add it. No profile can put back what this takes away.

**The rules and their effect are two questions with two answers.** `agenthub server ls` and
`server inspect github` say what the rules *are*; `agenthub server tool ls` lists what is actually
offered after them, with `--all` adding what they hold back. When one tool is missing from a client and
it is not clear which layer took it, `agenthub server tool inspect github__get_issue` names the one that
did.

## Profiles: when you want less than everything

```bash
agenthub profile create research
agenthub profile server add research linear      # <profile> then <server>
agenthub profile tool allow research linear --only list_issues,get_issue
agenthub client bind cursor research
```

The same three commands one argument deeper. `agenthub client ls` shows who is on which profile and
whether agenthub is in their config at all; `agenthub client unbind cursor` returns it to the default.

**Rebinding is live.** Changing a binding takes effect on sessions that are already running — agenthub
recomputes and pushes `tools/list_changed`. Only `client connect`, which edits the client's own file,
needs a restart.

**A missing profile fails closed.** Binding to a name that does not exist is accepted, warns, and
resolves that client to an *empty* scope: it sees nothing. Deleting a profile must not silently widen
every client that referenced it. If a client suddenly sees no tools, check `agenthub client ls` for a
`MISSING` marker first.

Mind that `--only` is an **intersection**: a name the server does not have lets *nothing* through for
that server. The rule is stored anyway — the catalogue may simply not have been fetched yet — but
agenthub checks the names against the last catalogue it recorded and warns about any that are missing,
so a typo says so where it was typed.

### The default profile

A client with no binding follows the globally active profile:

```bash
agenthub profile use research     # every unbound client now follows it
agenthub profile use -            # clear it: unbound clients see every enabled server
```

There is no default-profile object to manage — the absence of narrowing is the default. Both listings
still name it, because "what does a client I never bound get" is a question they have to answer:

```
NAME                    ACTIVE  SERVERS  DISCOVERY         TOOL RULES
(default) -> research           linear   lazy (inherited)  linear: only list_issues,get_issue
research                *       linear   lazy (inherited)  linear: only list_issues,get_issue
```

The star marks whichever row is in force. `(default)` is a display token, not an object: only
`profile use` moves it, and a profile name may not start with `(`.

## Discovery: how the surface is presented

`discovery` decides how many tool names a client is shown, not which tools it may call. It is a property
of the profile, because it describes that profile's tool set:

```bash
agenthub profile discovery research lazy      # or grouped / full / -
agenthub config set discovery full            # the global default
```

| mode | `tools/list` returns | use when |
|---|---|---|
| `full` | every visible tool, one entry each | small surfaces, or a client that filters for itself |
| `grouped` | one aggregate entry per server, then `call_tool` | a mid-sized set |
| `lazy` | the meta-tools (`status`, `search_tools`, `describe_tool`, `call_tool`, `fetch_result`) plus any pinned tools | large surfaces. **The default when nothing sets a mode** |
| `-` | clears the profile's override | fall back to the global default |

Nothing pins a tool yet — no field and no command names one — so `lazy` today returns those five names
and nothing else.

The reason to care is context, not security. Forty servers in `full` mode means a tool list the client
re-reads on every turn; `lazy` turns that into five names plus a search. Visibility is unchanged either
way. `profile ls` prints the mode each profile will actually be served in, so one that sets none shows
`lazy (inherited)` rather than a dash you would have to resolve yourself.

## Verifying it end to end

```bash
agenthub server test linear         # open a real connection and see what it answers
agenthub server inspect linear --tools
agenthub client ls                  # who is wired up, and who is on which profile
```

`server test` is the one check that proves something rather than reporting it: it connects for real, so
a pass means credentials, transport and the server itself all work right now. `server inspect --tools`
lists what was recorded at the last contact, which is why it answers instantly and can be stale.

`client connect` edits your client's own config file, and two clients get special handling. Zed and
VS Code keep theirs as JSONC — JSON with comments — so agenthub edits only the bytes of its own entry:
comments, key order and formatting come back exactly as they were, and if the edit cannot be proved
correct it refuses and tells you what to paste instead. Codex it does not write at all, because
re-encoding TOML would cost you the comments and layout; `client connect codex` runs `codex mcp add` for
you, backing the file up first and reading it back after. Pass `--manual` to have agenthub print the
command rather than run it.

A written config file only shows intent. The confirmation is the client itself: restart it and ask it to
use a tool.

## Where things live

| what | where |
|---|---|
| server definitions, and their tool allow lists | `servers.json` |
| profiles (servers, tools, discovery) | `profiles.json` |
| client → profile bindings | `clients.json` |
| global switches, active profile | `governance.json` |
| credentials | the OS keychain or the encrypted vault — **never** the registry |

None of it needs editing by hand. `server ls` grows an `AUTH` column as soon as any server has a
credential, saying what is stored here: `oauth`, `token` or `secret` when there is one, `oauth:expired`,
`oauth:revoked` or `secret:missing` when something needs you, with the command that fixes it printed
under the table. `oauth:revoked` is the one that will not sort itself out — the provider has refused the
stored sign-in, so nothing is renewed in the background and only a fresh login helps. It reports what
this machine holds, not whether the server still accepts it; `agenthub server test <id>` goes and asks.

## When something broke and you were not watching

Three files answer three different questions, and reaching for the wrong one is what makes an incident
take an hour:

```bash
agenthub events --server linear          # what HAPPENED to it, in a closed vocabulary
agenthub logs --server linear            # what the processes SAID about it, as prose
agenthub calls tail --server linear      # what a client CALLED on it
```

All three take the same `--since` (a duration, an RFC3339 time, or `all`), the same `--limit` (`0` for
all of them) and the same `-f`. `agenthub server logs <id>` is the fourth, for the frames of one
downstream conversation.

**`events` is the one to open first.** Every state change of a downstream server, a gateway or the
daemon lands in `<data>/logs/events.jsonl` with a `kind` from a fixed set — `connected`, `circuit_open`,
`respawned`, `secrets_missing` and the rest. Fixed is the point: you can filter and alert on those
values, which you cannot safely do with the wording of a log message. It works with no daemon running,
and it is on by default.

```bash
agenthub events --since 24h                  # everything, newest last
agenthub events --kind circuit_open,health_down
agenthub events --class disruption           # only what went wrong, and how it ended
agenthub events -f                           # tail it; survives a daemon restart
```

`--class` is the filter worth knowing. It splits the hub running as intended from the hub reacting to
something that went wrong, and a disruption keeps the recovery that ended it — so an outage never reads
as still open. It is not the log level: a level is one-dimensional, so `health_down` is a warning and
`health_up` is not, which is why `logs --level warn` shows a server going down and never coming back.

`logs` is the prose alongside it, merged across every process — `daemon.log` plus one
`gateway-<client>.log` per connected client — in one time-ordered stream. That merge is the reason it
exists: a daemon restart and the six gateways that lost their connections two seconds later is one story
told in seven files.

```bash
agenthub logs --level warn --since 1h        # what went wrong recently, anywhere
agenthub logs --client claude-code -f        # follow one client's gateway
```

## Keeping a call history

**Metadata is always recorded**, and there is nothing to switch on: every request a client makes of
agenthub — `tools/call`, but also `initialize`, `tools/list` and `ping` — leaves what was asked, which
server and tool it reached, how it ended and how long it took.

**The bodies are the part you enable.** `agenthub calls enable` sets up the key that seals request
parameters, effective arguments, results and — for a traced server — the frames, into encrypted local
packs. Recording starts before the gates run, so a denied call is in the history too.

**Neither half can refuse a call.** If the key is missing or the storage bound is reached, the record is
what is lost: the call runs, and the gateway logs `ledger record dropped` at Error.
`agenthub logs --level error` is where a hole in the history shows up.

```bash
agenthub calls enable
agenthub calls tail --since 24h
agenthub calls tail -f
agenthub calls show <call-id>                 # metadata only
agenthub calls show <call-id> --payloads      # explicit decryption
agenthub calls stats --since 7d
agenthub calls verify
```

The defaults retain 30 UTC days, cap the ledger at 5 GiB and reserve 1 GiB of free space; change them
with `config set calls.retentionDays`, `calls.maxBytes` and `calls.minFreeBytes`. `calls prune
--dry-run` previews the expired day partitions.

Two things to know before exporting or turning it off. `calls export --output history.jsonl` writes
metadata to a new 0600 file and refuses to overwrite one; add `--payloads` only when you truly need
decrypted arguments and results, because the exported file then carries credentials outside the bounded
ledger. And `calls disable` stops new capture without deleting history or keys, just as
`calls rotate-key` keeps the old keys so existing history stays readable.

`calls verify` detects modified metadata, corrupted payloads and swapped references. All the evidence is
local, so it cannot prove a whole day directory was deleted; if deletion evidence matters to you,
archive externally.

## When you need to see the wire

Once a server passes `server test` but still behaves wrongly in the client, the question stops being "is
it reachable" and becomes "what exactly did it say":

```bash
agenthub server trace linear on
# reproduce the problem in your client
agenthub server logs linear
agenthub server trace linear off
```

**It takes effect immediately** — a client that is already running starts recording without being
restarted, and the connection under investigation is not reconnected.

**The frames go into the call ledger, and each one names the call it belongs to.** `agenthub calls show
<call-id>` shows one call's whole story — the request, the frames it produced, the retries, and the
result — so a call that retried twice reads as three attempts under one id.

**Bodies are captured at the connection, before anything filters them, and they need a key.** Without
`calls enable` you get every frame's method, size, duration and outcome, but not what it said.
Unredacted downstream traffic is never written in the clear.

**It is per server, and it persists.** Nothing expires a trace, so one left on keeps recording across
restarts. `server ls` grows a `TRACE` column while anything is being traced.

## Common surprises

| symptom | likely cause |
|---|---|
| a server you added never shows up | `server add` leaves it switched off — check `server ls`, then `agenthub server enable <id>` |
| client sees no tools at all | bound to a profile that does not exist (`client ls` shows `MISSING`), or it was never restarted after `client connect` |
| a tool disappeared | an allow list took it — `agenthub server tool inspect <exposed-name>` names which layer |
| a server works in `server test` but not in the client | the client has not been restarted, or its profile does not include that server |
| `client connect` seems to do nothing | it edits a file; the client reads that file at startup |
| a legacy `projects` block in `clients.json` | per-project bindings were retired. The block is preserved but inert — it used to narrow, so leaving it does not restrict anything now |
