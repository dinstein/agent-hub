# Using agenthub

This is the user-facing guide: what the three concepts are, how they fit
together, and the decisions you actually have to make. The other documents in
`docs/` are for people changing agenthub's code; this one is for people using
it.

## The shape of it

Three nouns, one sentence each:

- **Server** — a downstream MCP server you registered. Registering and
  switching on are two steps: `server add` writes the definition and leaves it
  off, `server enable` connects and puts it into service. The set of *enabled*
  servers is everything agenthub could offer anyone.
- **Profile** — a named subset of that: which servers, which of their tools,
  and how the result is presented.
- **Client** — an AI application (Claude Code, Cursor, Codex, …). A client is
  **bound** to one profile, and that binding is the entire answer to what it
  can see.

```
servers (enabled)          ← the maximum: everything that exists
   └── profile             ← a subset you named
         └── client        ← bound to exactly one profile
```

Two things follow from this that are worth stating outright:

**A client never narrows on its own.** It selects a profile; it does not add
rules on top of one. If two clients need different surfaces, they get two
profiles. This is why "which profile is this client on" is a complete answer
rather than half of one.

**Narrowing only narrows.** A profile can only take capability away from the
enabled set — it can never grant a server that is disabled, or a tool that
does not exist. `agenthub server disable` is therefore an unconditional kill
switch: no profile can bring it back.

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

## Profiles: when you want less than everything

```bash
agenthub profile create research
agenthub profile server add research linear      # <profile> then <server>
agenthub profile tools research linear --only list_issues,get_issue
agenthub client bind cursor research
```

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
separate default-profile object to manage — the absence of narrowing is the
default.

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

The reason to care is context, not security. Forty servers in `full` mode
means a tool list the client re-reads on every turn; `lazy` turns that into
five names plus a search. Visibility is unchanged either way — a tool hidden
from the initial list is still callable if it is in scope, and a tool out of
scope is not callable no matter which mode you pick.

That is also why `lazy` is the default. Nobody revisits this setting when the
fourth server is added, so the default is what most installations run forever,
and `full` spends context in proportion to how much you are using the gateway
— one hosted server can bring fifty tools by itself. What `lazy` costs is
discoverability: the client has to search for a tool instead of being handed
its name. On a small surface that trade is not worth it, so say so:

```bash
agenthub config set discovery full
```

## Where things live

| what | where |
|---|---|
| server definitions | `servers.json` |
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
stale. `client ls` closes the loop on the other side, with both halves per client:
CONNECTED comes from the client's own config file, and PROFILE is what it
may see. When a row says something other than yes or no — `denied`,
`unreadable`, `?` — `client inspect <id>` says which file and why.

A written config file only shows intent. The confirmation is the client
itself: restart it and ask it to use a tool.

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
recording without being restarted, and the server it talks to is not
reconnected — the connection under investigation is left alone.

**The file holds raw responses.** Frames are captured at the connection,
before anything filters them, so whatever the server actually returned sits
in `logs/server-<id>.log`. That is the point of it, and the reason to turn it
off once you have your answer.

**It is per server, and it persists.** Nothing expires a trace, so one left
on keeps recording across restarts. `server ls` grows a `TRACE` column while
anything is being traced — that is where to look when you cannot remember.

## Common surprises

| symptom | likely cause |
|---|---|
| a server you added never shows up | `server add` leaves it switched off — check `server ls`, then `agenthub server enable <id>` |
| client sees no tools at all | bound to a profile that does not exist (`client ls` shows `MISSING`), or it was never restarted after `client connect` |
| a tool disappeared | a profile's `--only` list, or the server changed it under a pin and it was quarantined — check the profile before suspecting the server |
| a server works in `server test` but not in the client | the client has not been restarted, or its profile does not include that server |
| `client connect` seems to do nothing | it edits a file; the client reads that file at startup |
| a legacy `projects` block in `clients.json` | per-project bindings were retired. The block is preserved but inert — it used to narrow, so leaving it does not restrict anything now |
