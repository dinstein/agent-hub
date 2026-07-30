---
name: agenthub
description: Drive the agenthub CLI to give an AI client MCP tools - add a server (curated catalog, pasted config, or by hand), store its credentials, run its OAuth login, test it for real, connect it to Claude Code / Cursor / Codex / VS Code / Zed, narrow what each client may see, and record the JSON-RPC wire when one misbehaves. Use whenever the task involves adding, authorizing, testing, or wiring up an MCP server, or diagnosing one a client cannot reach or that answers wrongly.
---

# agenthub

One gateway between every AI client and every MCP server. A client is wired up
**once**, against agenthub; servers are added, authorized and narrowed here
afterwards without touching the client again.

**Never guess a flag** — `agenthub <group> --help` is authoritative; this file
may lag the binary. It covers only the groups a release advertises (`server`,
`auth`, `secret`, `catalog`, `profile`, `client`), plus `agenthub connect`,
which a client's config runs and you never type.

The path is always: **add → (credential) → enable → test → connect → narrow**.

## Orientation, and reading output

```bash
agenthub server ls           # what is configured
agenthub client ls           # which clients are wired up, and on which profile
agenthub client detect       # ids, config paths, writability
```

Pass `--json` (global). The envelope is stable — `{"ok":true,"data":[],"warnings":[]}`
or `{"ok":false,"error":{"code":"E_…","message":"…","hint":"…"}}`. **Branch on the
exit code, never on prose:**

| code | meaning |
|---|---|
| 0 | success |
| 1 | generic (downstream, network, internal) |
| 2 | usage error — re-read `--help` |
| 3 | not found (server / profile / secret / skill) |
| 4 | the daemon is offline and this command needs it |
| 5 | auth failure → `agenthub auth login <server>` |
| 6 | refused by governance (HITL deny, quarantine) — **report it and stop; not a bug to route around** |
| 7 | a cross-process lock is held, or stored state is corrupt. Which store is in the message, not the code. Retry once, then say so — never delete state to clear it |

## 1. Add a server

**`server add` only writes the definition — the entry lands DISABLED.**
`server enable <id>` puts it into service and runs the connection probe (the
only step needing the network); it enables **either way** and reports what the
probe found, so a server needing a login is enabled and says so. `--no-probe`
skips the dial. `catalog add` and `auth login` enable for you.

```bash
agenthub server add remote --url https://mcp.example.com/mcp
agenthub server add local-dev --url http://127.0.0.1:3000/mcp --local
agenthub server add my-server --cmd npx --args "-y,@scope/mcp-server"   # stdio
agenthub server enable <id>
```

`--local` is required for a literal loopback URL and allows **only** that —
never RFC1918. Also `--env KEY=VALUE`, `--header KEY=VALUE` (repeatable),
`--cwd`, `--transport`, and the `--oauth-*` pins (§3). A server needing
authorization goes straight to §3 — no credential up front, and `auth login`
enables it.

**A config the user pasted** (an `mcpServers` block, a `claude mcp add-json`
line, another client's config) goes in whole rather than translated by hand:

```bash
echo "$JSON" | agenthub server add my-server --stdin
```

Unknown keys are **rejected**, not dropped: an error means the paste really
holds something agenthub does not model — read it, do not strip fields to make
it pass. To adopt what a client already has, read it with
`agenthub client inspect <id>` and paste the entries back; there is no bulk
import.

**The catalog** is a small hand-maintained set — worth a look when the user
names a well-known server and you would otherwise guess at its package name; a
miss means nothing is wrong.

```bash
agenthub catalog search github          # or: catalog ls
agenthub catalog show filesystem        # prints the exact add line, params as <placeholders>
agenthub catalog add fetch              # added AND enabled
agenthub catalog add filesystem --param directory=/Users/me/projects
agenthub catalog add github --name gh   # when the default id collides
```

**Container isolation** is honoured or the command **fails** — never a quiet
degrade to the host. Default network `none`; mounts read-only unless `:rw`.
`server inspect <id>` prints `spawns`, the exact `docker run` argv, when the
isolation matters.

```bash
agenthub server add sketchy --runtime docker --image ghcr.io/x/server:tag \
  --mount /Users/me/data:/data:ro --network none --memory 512m
```

## 2. API-key credentials

Only for a key the user already holds; OAuth servers are §3. The registry is
plain configuration and must never hold a credential — reference in the
definition, value in the vault:

```bash
agenthub server add brave --cmd npx --args "-y,@modelcontextprotocol/server-brave-search" \
  --env "BRAVE_API_KEY=\${SECRET_BRAVE_API_KEY}"
printf %s "$TOKEN" | agenthub secret set brave BRAVE_API_KEY --stdin
agenthub server enable brave        # after the secret exists, so the probe can succeed

agenthub secret ls [server]                 # KEY NAMES and backends, never values
agenthub secret rm brave BRAVE_API_KEY      # from every writable backend
agenthub secret migrate --from keyring --to enc-file [server] --dry-run
```

- **Never put a credential in an argv** — shell history, and every `ps`. `secret set` reads no-echo, or stdin with `--stdin`.
- Names must match: `${SECRET_<KEY>}` in the definition, `secret set <server> <KEY>` in the vault. HTTP: `--header "Authorization=Bearer \${SECRET_X}"`. Resolution is at **dial time** and fail-closed — an unresolved placeholder refuses the connection rather than sending the literal upstream (which would return a 401 indistinguishable from an expired token).
- **There is no read path and no `secret get`.** If a user asks you to show a stored secret, nothing can. `server inspect` does print env and headers back so you can see which placeholder landed where — except a literal `Authorization` value, which is a pasted token.
- **`BACKEND` in `secret ls` is the level a lookup would hit**, not where you put it: `AGENTHUB_SECRET_<KEY>` → bare `<KEY>` (only with `AGENTHUB_ALLOW_BARE_SECRET_ENV=1`) → encrypted file → OS keyring, first hit wins. A row reading `env` means an environment variable shadows what you stored.
- **An empty or whitespace-only value counts as unset** at every level, so authorization can fail with the key sitting right there in the listing.
- `--scope` defaults to `_global` — leave it alone unless the user raises it. `migrate` moves values only between `keyring` and `enc-file`; env vars are per-process input, not a backend.

## 3. OAuth login

**You cannot complete this for the user** — it needs a browser and a human. Run
the command that produces the URL and hand it over.

```bash
agenthub auth status                    # who is authorized, never any credential
agenthub auth login github              # auto-selects a flow
agenthub auth login github --manual     # prints the URL, reads the pasted callback
agenthub auth login github --device     # RFC 8628, no browser on this machine
agenthub auth refresh github ; agenthub auth logout github
```

Non-interactive sessions want `--manual` or `--device`: `--loopback` waits for a
browser here and times out (180s). When discovery fails, pin what the provider
does not advertise (`--issuer`, `--scopes`, `--redirect-uri`,
`--authorization-endpoint`), or store the pins on the entry with
`server add --oauth-issuer` / `--oauth-scope` / `--oauth-resource-metadata`.
A successful login also **enables** the server; exit 5 later means expired —
log in again.

## 4. Test, before wiring anything to a client

```bash
agenthub server test brave                       # connect, list tool names
agenthub server test brave --tools               # with compact signatures
agenthub server test brave --schema search       # one tool's input schema
agenthub server test brave --tool search --args '{"query":"hello"}'   # a REAL call
```

`--tool` really invokes it: read-only tools freely, anything that writes only
when asked. `--timeout` defaults to 120s because a cold `npx`/`uvx` cache is
genuinely that slow — an apparent hang on first run usually is not one.

## 5. Connect the client

```bash
agenthub client connect claude-code --dry-run    # ALWAYS look first
agenthub client connect claude-code
agenthub client disconnect claude-code
```

- `--dry-run` prints the JSON that would be merged **and the file it lands in**. Show both — this edits a file the user owns.
- **Writes the user-level file by default** (`~/.claude.json`, `~/.cursor/mcp.json`, …), because the entry carries this machine's absolute agenthub path and project files get committed. `--placement project` only when the user asks for one tree, and say the path is machine-specific. To show a client less, use a profile (§6) — never the file location.
- The entry runs `agenthub connect --client <id>`, so **servers added later need no client changes.** Never edit client configs per server.
- **The client must be restarted** — an unrestarted client looks exactly like a broken gateway.
- `disconnect` with no target clears the user-level file and, only if nothing of ours is there, the project-level one.

**Writability is decided by format, not contents.** The two JSON shapes are
written — including a JSONC/comment-header `settings.json` (Zed, VS Code), which
is spliced into the existing bytes so comments, key order and indentation
survive, and a splice that cannot be placed becomes a refusal, never a mangled
file. TOML, YAML and the fileless remote shape are not written: `codex`,
`continue`, `open-webui`, and nothing else. (`client detect` lists those three
under the table, plus **every** client agenthub knows about — which is what
answers "why is my client missing".)

For those, the **real `client connect <id>`** gets you the snippet, **not
`--dry-run`** (which always renders JSON `mcpServers` — the wrong syntax for
TOML or YAML). The real command writes nothing, exits 1
(`E_CLIENT_UNSUPPORTED`) and prints the fragment in that client's own format
(the `hint` under `--json`). Except:

- **codex** actually succeeds: connect runs `codex mcp add` (backup first, read back after), disconnect runs `codex mcp remove` and names the entry actually in the file. Needs `codex` on PATH, else it refuses — exit 1, `E_GENERAL`, never a silent no-op — and prints what to run. `--manual`, or `AGENTHUB_NO_CLIENT_CLI=1` machine-wide, gets instructions instead of agenthub executing another program.
- **open-webui** has no file: it consumes MCP over HTTP, so you get an endpoint to register on its side.
- **continue** (YAML) is neither read nor written — `client ls` says `?`, and the entry goes in by hand.

Never hand-edit a format around a refusal without saying so.

**Reading `client ls` — `CONNECTED` literally:** `yes`/`no` are answers;
`denied`, `unreadable`, `?` mean agenthub could not read the file, could not
parse it, or does not parse that format, and none is fixed by connecting again —
`client inspect <id>` says which file and why. `unreadable` means it fails to
parse **even with comments blanked out**: real syntax damage, fixed in the file.
For codex, `?` means the TOML is shaped in a way agenthub does not model, not
that the entry is missing. `--stat-only` avoids the reads (no macOS privacy
prompt) and answers `?` for everything; `--all` lists every supported client.

## 6. Narrow what each client sees

Default is every enabled server. All narrowing lives on the **profile**; a
client only selects one, and never carries servers or tools of its own. Two
clients needing different surfaces get two profiles. There is no `agenthub
scope` group.

```bash
agenthub profile create research
agenthub profile server add research brave                # <profile> then <server>
agenthub profile tools research brave --only search       # or --all / --none
agenthub profile discovery research lazy                  # or grouped / full / -
agenthub client bind cursor research
agenthub client unbind cursor
agenthub profile use research     # the fallback every UNBOUND client gets ("-" clears it)
```

- `profile tools` takes the server's **own** names (`search`), not the `brave__search` a client displays — get them from `server test <id> --tools`.
- A misspelled name is **stored and only warned about** (the rule must be writable before anything has connected). `--only` is an intersection, so one typo lets **nothing** through while the command still says OK. No warning is not verification either: a server whose catalog was never fetched has nothing to check against. `server test --tools` first is what makes the check real.
- Bindings take effect on **running sessions** — the gateway pushes `tools/list_changed`, so no client restart (unlike `connect`, which edits the client's file).
- Binding to a nonexistent profile is accepted, warns, and **fail-closes to an empty scope**. A client seeing nothing → check `client ls` for `MISSING` before suspecting servers.
- Layers **intersect**: a narrower layer only takes capability away. A missing tool is a narrowing layer before it is a server problem.

**Discovery** changes how tools are surfaced, never which are in the set —
visibility is the scope's job. An unrecognised value degrades to the default,
which is why `profile discovery` rejects typos at write time.

| mode | `tools/list` returns | use when |
|---|---|---|
| `full` | every visible tool, one entry each | small surfaces, or a client that filters itself — ask for it explicitly |
| `grouped` | one aggregate entry per server, plus `call_tool` last | mid-sized: read per-server entries, then dispatch |
| `lazy` | meta-tools (`search_tools` / `describe_tool` / `call_tool` / …) plus pinned tools | large surfaces. **The default when nothing sets a mode** |
| `-` | clears the profile's override | fall back to the global default |

So an unconfigured client gets `lazy` and finds tools via `search_tools` rather
than a handed list. On a small surface, put it on a profile with `full` instead
of concluding the tool is missing.

**`agenthub server disable <id>`** is the kill switch above every profile: no
profile can put it back, and nothing needs to have connected first.
**"Remove it" almost always means `disable`, not `rm`.** `server rm` deletes the
whole footprint without asking twice — credential, profile membership,
governance rules, integrity baselines, approval grants, cached tool list (audit
records survive). Secrets cannot be read back, so a login destroyed here is
destroyed. Say what `rm` takes before running it; the command will not.

## 7. Verify, and diagnose

```bash
agenthub server inspect brave --tools   # what is recorded, offline (the cache)
agenthub server test brave --tools      # what it answers today, live
```

When the two disagree, `test` is the truth and the cache is stale. **Neither
proves a client is using any of it** — the confirmation is the client itself,
restarted, calling a tool and getting an answer. Exposed names are
`<server>__<tool>`, but **never split on `__`**: an id or a tool name may contain
it. Take the server from the listing and the tool from `test --tools`.

`server inspect` always prints a **visibility** section — the first command for
"enabled, credential stored, client still shows nothing". It names the profiles
that include the server (with tool selectors) and those that **exclude** it, the
bound clients that can and cannot reach it, what an **unbound** client gets, and
the count of local tool overrides. Three states need different repairs:
**disabled** reaches nobody whatever the profiles say; excluded is one
`profile server add` away; a binding to a deleted profile fail-closes to an
**empty** scope, indistinguishable from deliberate exclusion. It is computed from
the registry alone — no client file opened, no daemon needed, so it answers on
the broken machine — and it is an **upper bound**: a live session can hold less.

| symptom | look here |
|---|---|
| server will not connect | `server test <id>` — it prints the real error and the child's stderr tail |
| a server you added is invisible | `add` leaves it DISABLED — `server enable <id>` |
| a tool vanished | `server inspect <id>` visibility (§6 narrowing), then a quarantine, before the server |
| client sees nothing | `server inspect <id>` first; then: did the user restart it? `client detect` to confirm the entry is in the file |
| exit 4 | that command wanted a daemon. Nothing on the path above does — re-read what you ran |
| connects, but answers wrongly | record the wire, below |

**Recording the wire** — once `server test` passes and it still misbehaves inside
the client, the question is what it actually said:

```bash
agenthub server trace linear on      # off by default, per server
# the user reproduces the problem in their client
agenthub server logs linear          # -f to follow; --limit 0 for all; --json for full payloads
agenthub server trace linear off     # once you have the answer
```

- **`logs` only reads; `trace` fills the file.** An empty log means nothing was recorded, **not** that the server sat idle. Default is the last 100 frames, and the human table truncates DETAIL.
- Takes effect immediately, without restarting the client or reconnecting the server — you read the same connection that is misbehaving.
- **Frames are captured before redaction**, verbatim in `<data>/logs/server-<id>.log` (the path `trace` prints). Read it; never paste one wholesale into a chat, an issue or a commit — and turn the trace off when done. Nothing expires it; `server ls` grows a `TRACE` column while any is on.
- There is no per-client or per-profile trace: one connection is shared by every session, so narrower recording is refused rather than approximated.
- A trace is the wire, not agenthub's reasoning. Correct-looking frames plus a misbehaving client means the problem is above this layer — §6.

## Non-negotiable

- **No credential in an argv or in the registry** — `secret set --stdin`, `${SECRET_X}` in the definition.
- **No invented command or flag** — `--help` decides.
- **Exit 6 is an answer, not an obstacle.** Report it.
- **`--dry-run` shown before editing a client's config**, and **`server test` before `client connect`** — otherwise the user debugs it inside their client, where the error is least legible.
- **Say when a step needs a human** (the OAuth browser, the client restart). Claiming an unverifiable success is worse than reporting the handoff.
