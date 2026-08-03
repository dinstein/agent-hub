# Using agenthub

This is the user-facing guide: what the three concepts are, how they fit
together, and the decisions you actually have to make. The other documents in
`docs/` are for people changing agenthub's code; this one is for people using
it.

## The shape of it

Three nouns, one sentence each:

- **Server** — a downstream MCP server you registered. Registering and
  switching on are two steps: `server add` writes the definition and leaves it
  off, `server enable` connects and puts it into service. A server offers all
  of its tools unless you name a subset (`server tool allow`). The set of *enabled*
  servers, with those names applied, is everything agenthub could offer anyone.
- **Profile** — a named subset of that: which servers, which of their tools,
  and how the result is presented.
- **Client** — an AI application (Claude Code, Cursor, Codex, …). A client is
  **bound** to one profile, and that binding is the entire answer to what it
  can see.

```
servers (enabled, + tool allow)   ← the maximum: everything that exists
   └── profile                    ← a subset you named
         └── client               ← bound to exactly one profile
```

Every level intersects with the one above it and **none of them can widen**.
That is the whole access model: what a client may reach is settled by what you
wrote down before it connected, and nothing is decided while a call is in
flight.

Two things follow. **A client never narrows on its own** — it selects a
profile, it does not add rules on top of one, so "which profile is this client
on" is a complete answer rather than half of one. If two clients need different
surfaces, they get two profiles. And **narrowing only narrows**: a profile can
never grant a server you disabled, or a tool that does not exist, which makes
`agenthub server disable` an unconditional kill switch.

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

That is the whole loop for a first server. Step 4 happens **once per client**:
the entry it writes runs `agenthub connect --client claude-code`, so every
server you add later is picked up without touching the client's config again.

With no profile in play, a client sees every enabled server. For many setups
that is the right answer and you can stop here.

## Switching off a tool everywhere

Before profiles, there is the blunter tool. A server offers everything it has,
including anything it gains in a later version; naming tools fixes the set:

```bash
agenthub server ls                              # what the rules are today
agenthub server tool allow github --only get_issue,list_prs
agenthub server tool allow github --none        # offer nothing from this server
agenthub server tool allow github --all         # back to offering everything
```

Read the rule before writing one: `allow` **replaces** the list rather than
adding to it, and you must say which of the three you mean — a bare `server
tool allow github` is a usage error, because the reading it would otherwise
have had ("offer nothing") sits one forgotten argument from the opposite of the
intent.

This is **for every client at once**, the tool-level twin of `server disable`,
and it is an allow list and never a deny list: on the day the server adds a
tool, a rule keeps the new one out until you add it. No profile can put back
what this takes away.

The rules and their effect are two questions with two answers. `agenthub server
ls` (which grows a TOOLS column once any rule exists) and `server inspect
github` say what the rules *are*; `agenthub server tool ls` lists what is
actually offered after them, with `--all` adding what they hold back. When one
tool is missing from a client and it is not clear which layer took it,
`agenthub server tool inspect github__get_issue` names the one that did.

## Profiles: when you want less than everything

```bash
agenthub profile create research
agenthub profile server add research linear      # <profile> then <server>
agenthub profile tool allow research linear --only list_issues,get_issue
agenthub client bind cursor research
```

The same three commands as one layer up, one argument deeper: `agenthub profile
tool ls research` lists what the profile actually lets through — the machine's
rules and the profile's own, intersected, which is what a bound client gets —
and `--all` adds the ones held back, each with the layer that took it.

`agenthub client ls` shows who is on which profile, and whether agenthub is
in their config at all. `agenthub client unbind cursor` returns it to the
default.

Three details that decide how this behaves:

**Rebinding is live.** Changing a binding takes effect on sessions that are
already running — agenthub recomputes and pushes `tools/list_changed`. Only
`client connect` (which edits the client's own file) needs a restart.

**Tool selection is three-state, and the empty state is closed:**

| you write | it means |
|---|---|
| no rule for that server | every tool of it |
| `--only a,b` | exactly those two |
| `--none` | none of them, server still listed |
| `--all` | removes the rule (back to every tool) |

Use the server's own tool names, and mind that `--only` is an **intersection**:
a name the server does not have lets *nothing* through for that server. The
rule is stored anyway — the catalog may simply not have been fetched yet — but
agenthub checks the names against the last catalog it recorded and **warns**
about any that are missing, so a typo says so where it was typed rather than as
an unexplained empty tool list later.

**A missing profile fails closed.** Binding to a name that does not exist is
accepted, warns, and resolves that client to an *empty* scope — it sees
nothing. That is deliberate: deleting a profile must not silently widen every
client that referenced it. If a client suddenly sees no tools, check
`agenthub client ls` for a `MISSING` marker first.

### The default profile

A client with no binding follows the **globally active profile**:

```bash
agenthub profile use research     # every unbound client now follows it
agenthub profile use -            # clear it: unbound clients see every enabled server
```

With nothing active, "unbound" means "everything enabled". There is no
default-profile object to manage — the absence of narrowing is the default.

Both listings still name it, because "what does a client I never bound get" is
a question they have to answer. `profile ls` heads its table with a `(default)`
row and `client ls` prints the same token in its PROFILE column:

```
NAME                    ACTIVE  SERVERS  DISCOVERY         TOOL RULES
(default) -> research           linear   lazy (inherited)  linear: only list_issues,get_issue
research                *       linear   lazy (inherited)  linear: only list_issues,get_issue
```

The star marks whichever row is in force, and `(default)` shows what the
fallback resolves to rather than making you look it up. It is a display token,
not an object: only `agenthub profile use` moves it, and a profile name may not
start with `(`. If the active profile does not exist, that row says `MISSING ->
empty scope` — the same marker, for the same reason, that a client bound to a
missing profile gets.

## Discovery: how the surface is presented

`discovery` decides how many tool names a client is shown, not which tools it
may call. It is a property of the profile, because it describes that
profile's tool set:

```bash
agenthub profile discovery research lazy      # or grouped / full / -
```

| mode | what `tools/list` returns | use when |
|---|---|---|
| `full` | every visible tool, one entry each | small surfaces, or a client that filters for itself — ask for it explicitly |
| `grouped` | one aggregate entry per server, then `call_tool` | a mid-sized set — the client reads per-server entries, then dispatches |
| `lazy` | the meta-tools (`status`, `search_tools`, `describe_tool`, `call_tool`, `fetch_result`) plus any pinned tools | large surfaces — the client holds a handful of names instead of hundreds. **The default when nothing sets a mode** |
| `-` | clears the profile's override | fall back to the global default |

`profile ls` prints the mode each profile will actually be served in, so a
profile that sets none shows the inherited one — `lazy (inherited)` — rather
than a dash you would have to resolve yourself. The footer names where that
inherited mode comes from: the built-in default, or `config set discovery`.

The reason to care is context, not security. Forty servers in `full` mode
means a tool list the client re-reads on every turn; `lazy` turns that into
five names plus a search. Visibility is unchanged either way — a tool hidden
from the initial list is still callable if it is in scope, and a tool out of
scope is not callable no matter which mode you pick.

`lazy` is the default because nobody revisits this setting when the fourth
server is added, and `full` spends context in proportion to how much you use
the gateway. What `lazy` costs is discoverability: the client has to search for
a tool instead of being handed its name. On a small surface that trade is not
worth it, so say so:

```bash
agenthub config set discovery full
```

## Where things live

| what | where |
|---|---|
| server definitions, and their tool allow lists | `servers.json` |
| profiles (servers, tools, discovery) | `profiles.json` |
| client → profile bindings | `clients.json` |
| global switches, active profile | `governance.json` |
| credentials | the OS keychain / vault — **never** the registry |

None of it needs editing by hand — the commands above are what write it, and
`server ls`, `client ls` and `auth status` read the first three back.

`server ls` grows an `AUTH` column as soon as any server has a credential,
saying what is stored here: `oauth`, `token` or `secret` when there is one,
`oauth:expired` or `secret:missing` when something needs you — with the
command that fixes it printed under the table. It reports what this machine
holds, not whether the server still accepts it; `agenthub server test <id>`
goes and asks.

## Verifying it end to end

```bash
agenthub server test linear         # open a real connection and see what it answers
agenthub server inspect linear --tools
agenthub client ls                  # who is wired up, and who is on which profile
```

`server test` is the one check that proves something rather than reporting
it: it connects for real, so a pass means credentials, transport and the
server itself all work right now. `server inspect --tools` lists what was
recorded at the last contact, which is why it answers instantly and can be
stale.

`client connect` edits your client's own config file, and two clients get
special handling. Zed and VS Code keep theirs as JSONC — JSON with comments —
so agenthub edits only the bytes of its own entry: comments, key order and
formatting come back exactly as they were, and if the edit cannot be proved
correct it refuses and tells you what to paste instead. Codex it does not write
at all, because re-encoding TOML would cost you the comments and layout;
`client connect codex` runs `codex mcp add` for you, backing the file up first
and reading it back after. Pass `--manual` (or set `AGENTHUB_NO_CLIENT_CLI=1`)
to have agenthub print the command rather than run it.

`client ls` closes the loop on the other side, with both halves per client:
CONNECTED comes from the client's own config file, and PROFILE is what it
may see — its own profile, or the `(default)` row of `profile ls` when you
never bound it. When a row says something other than yes or no — `denied`,
`unreadable`, `?` — `client inspect <id>` says which file and why.

A written config file only shows intent. The confirmation is the client
itself: restart it and ask it to use a tool.

## Keeping a tools/call history

The access ledger is off until you enable it. Once enabled, every tools/call
attempt records its request parameters and effective arguments into encrypted
local packs, with a truncated copy of the result — the recording starts before
the gates run, so a denied call is in the history too. Recording is strict: if
the key or
the bounded storage is unavailable, the call is **refused** rather than executed
with a hole in the history.

```bash
agenthub calls enable
agenthub calls status
agenthub calls tail --since 24h
agenthub calls show <call-id>                 # metadata only
agenthub calls show <call-id> --payloads      # explicit decryption
agenthub calls stats --since 7d
agenthub calls verify
```

The defaults retain 30 UTC days, cap the ledger at 5 GiB and reserve 1 GiB of
free space; change them with `config set audit.retentionDays`,
`audit.maxBytes` and `audit.minFreeBytes`. `audit prune --dry-run` previews the
expired day partitions and `audit prune` removes them; ordinary writes run the
same pruning before their own capacity check.

Two things to know before exporting or turning it off. `audit export --output
history.jsonl` writes metadata to a new 0600 file and refuses to overwrite one;
add `--payloads` only when you truly need decrypted arguments and results,
because the exported file then carries credentials outside the bounded ledger.
And `audit disable` stops new capture without deleting history or keys, just as
`audit rotate-key` keeps the old keys so existing history stays readable.

`audit verify` detects modified metadata, corrupted payloads and swapped
references. All the evidence is local, so it cannot prove a whole day directory
was deleted; if deletion evidence matters to you, archive externally.

## When a server misbehaves and you were not watching

Three files answer three different questions, and reaching for the wrong one
is what makes an incident take an hour:

```bash
agenthub events --server linear          # what HAPPENED to it, in a closed vocabulary
agenthub logs --server linear            # what the processes SAID about it, as prose
agenthub calls tail --server linear      # what a client CALLED on it
```

`events` is the one to open first. Every state change of a downstream server,
a gateway or the daemon lands in `<data>/logs/events.jsonl` with a `kind` from
a fixed set — `connected`, `circuit_open`, `respawned`, `secrets_missing` and
the rest. Fixed is the point: you can filter and alert on those values, which
you cannot safely do with the wording of a log message.

```bash
agenthub events --since 24h                  # everything, newest last
agenthub events --server linear              # one downstream's whole history
agenthub events --kind circuit_open,health_down
agenthub events --scope daemon               # restarts and config reloads
agenthub events -f                           # tail it; survives a daemon restart
```

It works with no daemon running, and that is not a fallback: a stdio gateway
writes this file on its own, so the installation with no daemon is the ordinary
case here. It is **on by default** — the one switch is `agenthub config set
events.enabled false` — where `calls` is off until you ask for it, because that
one records call arguments and results while this records only that something
changed.

`logs` is the prose alongside it, merged across every process — `daemon.log`
plus one `gateway-<client>.log` per connected client — in one time-ordered
stream. That merge is the reason it exists: a daemon restart and the six
gateways that lost their connections two seconds later is one story told in
seven files.

```bash
agenthub logs --level warn --since 1h        # what went wrong recently, anywhere
agenthub logs --client claude-code -f        # follow one client's gateway
agenthub logs --source daemon                # or just the daemon
```

`agenthub daemon logs` remains the single-process view of `daemon.log` alone.

## When you need to see the wire

Once a server passes `server test` but still behaves wrongly in the client,
the question stops being "is it reachable" and becomes "what exactly did it
say". Recording the traffic answers that:

```bash
agenthub server trace linear on
# reproduce the problem in your client
agenthub server logs linear
agenthub server trace linear off
```

Three things are worth knowing before turning it on:

**It takes effect immediately.** A client that is already running starts
recording without being restarted, and the connection under investigation is
not reconnected.

**The frames go into the call ledger, and each one names the call it belongs to.**
`agenthub server logs <id>` reads one server's conversation out of it, and
`agenthub calls show <call-id>` shows one call's whole story — the request, the
frames it produced, the retries, and the result. A call that retried twice reads
as three attempts under one id rather than as three unrelated exchanges.

**Bodies are captured at the connection, before anything filters them, and they
need a key.** Frame bodies go into the ledger's encrypted pack, which is what
`agenthub calls enable` sets up; without it you get every frame's method, size,
duration and outcome, but not what it said. That is the trade: unredacted
downstream traffic is never written in the clear.

**It is per server, and it persists.** Nothing expires a trace, so one left on
keeps recording across restarts. `server ls` grows a `TRACE` column while
anything is being traced — that is where to look when you cannot remember.

## Common surprises

| symptom | likely cause |
|---|---|
| a server you added never shows up | `server add` leaves it switched off — check `server ls`, then `agenthub server enable <id>` |
| client sees no tools at all | bound to a profile that does not exist (`client ls` shows `MISSING`), or it was never restarted after `client connect` |
| a tool disappeared | an allow list took it — `agenthub server tool inspect <exposed-name>` names which layer, and answers it in one command rather than reading `server ls` and `profile ls` against each other |
| a server works in `server test` but not in the client | the client has not been restarted, or its profile does not include that server |
| `client connect` seems to do nothing | it edits a file; the client reads that file at startup |
| a legacy `projects` block in `clients.json` | per-project bindings were retired. The block is preserved but inert — it used to narrow, so leaving it does not restrict anything now |
