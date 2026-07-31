---
name: agenthub
description: Drive the agenthub CLI to give an AI client MCP tools - add a server (curated catalog, pasted config, or by hand), store its credentials, run its OAuth login, test it for real, connect it to Claude Code / Cursor / Codex / VS Code / Zed, narrow what each client may see, and diagnose the installation with doctor when a client cannot reach a server or it answers wrongly. Use whenever the task involves adding, authorizing, testing, wiring up, or troubleshooting an MCP server.
---

# agenthub

One gateway between every AI client and every MCP server. Wire a client up **once**; add, authorize, and narrow servers here afterwards without touching the client again.

**Never guess a flag** — `agenthub <group> --help` decides. This file covers exactly what a release advertises: `server`, `auth`, `secret`, `catalog`, `profile`, `client`, `doctor`.

The path is always: **add → (credential) → enable → test → connect → narrow**.

## Reading output

```bash
agenthub server ls           # configured servers
agenthub client ls           # wired clients, their profiles
agenthub client detect       # all known clients, config paths, writability
```

Pass `--json` globally. Envelope: `{"ok":true,"data":…}` or `{"ok":false,"error":{"code":"E_…","message":"…","hint":"…"}}`. Four commands stream progress and emit NDJSON — `auth login`, `server test`, `server enable` (unless `--no-probe`) and `doctor`: progress events first, **envelope last**, so take the LAST line, never the whole stream as one object. Three of the four are on the everyday path below. **Branch on exit code:**

| code | meaning |
|---|---|
| 0 | success |
| 1 | generic (downstream, network, internal) |
| 2 | usage error |
| 3 | not found |
| 4 | that command needs a running daemon; nothing on this path does |
| 5 | auth failure → `agenthub auth login <server>` |
| 6 | refused by governance — **report and stop; not a bug to route around** |
| 7 | lock held or state corrupt — retry once, never delete state |

## 1. Add a server

**`server add` only writes the definition — the entry lands DISABLED.** `server enable <id>` puts it into service and runs a probe. `catalog add` and `auth login` enable automatically.

```bash
agenthub server add remote --url https://mcp.example.com/mcp
agenthub server add local-dev --url http://127.0.0.1:3000/mcp --local   # loopback only
agenthub server add my-server --cmd npx --args "-y,@scope/mcp-server"   # stdio
agenthub server enable <id>
```

Also: `--env KEY=VALUE`, `--header KEY=VALUE`, `--cwd`, `--transport`, `--oauth-*` pins. `--no-probe` skips the dial.

**Pasted config** (an `mcpServers` block, another client's JSON):

```bash
echo "$JSON" | agenthub server add my-server --stdin
```

Unknown keys are **rejected**, not dropped — read the error rather than stripping fields.

**Catalog** — curated set; worth checking when the user names a well-known server:

```bash
agenthub catalog search github
agenthub catalog show filesystem        # prints the exact add line
agenthub catalog add fetch              # added AND enabled
agenthub catalog add filesystem --param directory=/Users/me/projects
```

**Container isolation** is honoured or the command **fails** — never a silent degrade:

```bash
agenthub server add sketchy --runtime docker --image ghcr.io/x/server:tag \
  --mount /Users/me/data:/data:ro --network none --memory 512m
```

## 2. API-key credentials

**Never put a credential in an argv or in the registry.** Use `${SECRET_X}` in the definition, store the value separately:

```bash
agenthub server add brave --cmd npx --args "-y,@modelcontextprotocol/server-brave-search" \
  --env "BRAVE_API_KEY=\${SECRET_BRAVE_API_KEY}"
printf %s "$TOKEN" | agenthub secret set brave BRAVE_API_KEY --stdin
agenthub server enable brave

agenthub secret ls [server]                 # key names and backends, never values
agenthub secret rm brave BRAVE_API_KEY
agenthub secret migrate --from keyring --to enc-file [server] --dry-run
```

- Names must match: `${SECRET_<KEY>}` in the definition, `secret set <server> <KEY>` in the vault. HTTP headers: `--header "Authorization=Bearer \${SECRET_X}"`.
- Resolution at **dial time**, fail-closed — an unresolved placeholder refuses the connection rather than sending the literal.
- **No read path, no `secret get`.** `server inspect` shows which placeholder landed where (except a literal `Authorization` value).
- `BACKEND` in `secret ls` is where a lookup would hit: env var → enc-file → keyring, first hit wins. A row reading `env` means a variable shadows what was stored.
- An empty or whitespace-only value counts as unset at every level.

## 3. OAuth login

**You cannot complete this** — it needs a browser and a human. Run the command and hand over the URL.

```bash
agenthub auth status
agenthub auth login github              # auto-selects a flow
agenthub auth login github --manual     # prints URL, reads pasted callback
agenthub auth login github --device     # RFC 8628, no browser on this machine
agenthub auth refresh github
agenthub auth logout github
```

Non-interactive sessions: `--manual` or `--device`. When discovery fails, pin with `--issuer`, `--scopes`, `--authorization-endpoint`, or store pins with `server add --oauth-*`. A successful login also **enables** the server.

## 4. Test before wiring

```bash
agenthub server test brave                               # connect, list tool names
agenthub server test brave --tools                       # with compact signatures
agenthub server test brave --schema search               # one tool's input schema
agenthub server test brave --tool search --args '{"query":"hello"}'   # real call
```

`--tool` really invokes the tool — read-only freely, anything that writes only when asked. `--timeout` defaults to 120s because cold `npx`/`uvx` caches are genuinely that slow.

## 5. Connect the client

```bash
agenthub client connect claude-code --dry-run    # ALWAYS look first
agenthub client connect claude-code
agenthub client disconnect claude-code
```

- `--dry-run` prints the JSON that would be merged **and the file it lands in**.
- Writes the **user-level** file by default; `--placement project` only when explicitly asked (path is machine-specific, file gets committed).
- The entry runs `agenthub connect --client <id>` — **servers added later need no client changes.**
- **The client must be restarted** — an unrestarted client looks exactly like a broken gateway.

**Writability by format:** JSON and JSONC/comment-header files (VS Code settings.json, Zed) are written — JSONC is spliced to preserve comments, key order, and indentation. TOML, YAML, and fileless shapes are not written: `codex`, `continue`, `open-webui`. For those, the **real `client connect`** (not `--dry-run`) exits 1 and prints the fragment in the client's own format.

Exceptions:
- **codex** actually succeeds: runs `codex mcp add`; needs `codex` on PATH, else exits 1 and prints what to run. `--manual` or `AGENTHUB_NO_CLIENT_CLI=1` prints instructions instead.
- **open-webui**: no file; you get an HTTP endpoint to register on its side.
- **continue**: neither read nor written; entry goes in by hand.

`CONNECTED` in `client ls`: `yes`/`no` are definitive; `denied`/`unreadable`/`?` means agenthub could not read or parse the file — `client inspect <id>` says which file and why. `--stat-only` skips reads (no macOS privacy prompt).

## 6. Narrow what each client sees

Default is every enabled server, offering every tool. **Two layers narrow it, they intersect, and neither can widen the other**: the server's own allow list applies to this machine (every client at once), a profile narrows further for the clients bound to it. Same command, same flags, two altitudes:

```bash
agenthub server  tool allow brave          --only search   # machine-wide
agenthub profile tool allow research brave --only search   # inside one profile
```

**Layer 1 — the server.** `--only a,b` offers exactly those, `--none` offers nothing (the server stays registered), `--all` drops the rule.

```bash
agenthub server ls                       # READ FIRST: the TOOLS column is every server's rule
agenthub server inspect brave            # that one rule spelled out, names included
agenthub server tool ls brave            # what it actually offers now
agenthub server tool ls brave --all      # plus what the rule holds back, state per tool
agenthub server tool allow brave --only search
```

- **Read the rule before writing.** `allow` **replaces** the list, it does not add to it. The rule lives on the server (`server ls` / `server inspect`); `server tool ls` is its effect.
- A bare `server tool allow brave` is a **usage error**, not "allow nothing" — say `--none`.
- `server tool inspect brave__search` says which layer blocks one tool, when a client cannot see it.

**Layer 2 — the profile.** Same three flags; a client selects a profile and never carries servers or tools of its own.

```bash
agenthub profile create research
agenthub profile server add research brave
agenthub profile tool allow research brave --only search    # or --all / --none
agenthub profile tool ls research                           # what it lets through, both layers intersected
agenthub profile tool ls research --all                     # plus what was held back, and by which layer
agenthub profile discovery research lazy                    # or grouped / full / -
agenthub client bind cursor research                        # takes effect on running sessions
agenthub client unbind cursor
agenthub profile use research    # fallback for every unbound client ("-" clears it)
```

- Both layers take the server's **own** names (`search`), not the exposed `brave__search`. Get names from `server tool ls <id>` or `server test <id> --tools`.
- `ls | inspect | allow` exist at both altitudes and take the same flags. `profile tool ls --all` names the layer that took each tool — `global`, `profile:servers` (the profile omits that server) or `profile:tools` — because the three need different repairs.
- A misspelled name is stored and only warned about. `--only` is an intersection — one typo lets **nothing** through. It shows up as `pending` in the `tool ls` of whichever layer holds it, and marked `!` in `server ls` / `server inspect`; nothing else ever mentions it again.
- **These are allow lists.** A tool the server adds tomorrow stays out until you add it, and there is no `deny` form at either layer.
- Binding to a nonexistent profile is accepted and **fail-closes to an empty scope**. Client seeing nothing → check `client ls` for `MISSING` before suspecting servers. An active profile that does not exist fail-closes every *unbound* client the same way, marked on the `(default)` row.
- `(default)` in either listing is the fallback an unbound client follows, not a profile: `profile ls` heads its table with it, `client ls` prints it in the PROFILE column (`(default) -> research` when one is active), and `profile use` is the only thing that moves it. Profile names may not start with `(`.

**Discovery** changes how tools are surfaced, not which are in scope. `profile ls` prints the mode each profile is actually served in — `lazy (inherited)` means the profile sets none and the global default decides:

| mode | `tools/list` returns |
|---|---|
| `lazy` | meta-tools (`search_tools` / `describe_tool` / `call_tool` / …) — **default** |
| `grouped` | one aggregate entry per server plus `call_tool` |
| `full` | every visible tool individually |

On a small surface where tools seem missing, check if the client is in `lazy` mode — tools are found via `search_tools`, not a handed list.

**`server disable <id>`** is the kill switch above every profile. **Prefer `disable` over `rm`** — `server rm` destroys the whole footprint (credential, profile membership, baselines, cached tool list; audit records survive) without confirmation. Secrets cannot be read back.

## 7. Diagnose

```bash
agenthub doctor                         # the whole installation, every check
agenthub server inspect brave --tools   # recorded state, offline (the cache)
agenthub server test brave --tools      # live answer today
```

**`doctor` when you do not yet know which part is broken; `server test <id>` when you do.** Doctor checks directories, the registry documents, every enabled server's handshake, the credential vault, the skill library, and each client's configuration — so it is the right first command for a vague report ("it stopped working") and the wrong one for "does brave connect".

- **Exit 1 means at least one check failed**, not that doctor failed. The report is on stdout either way; read the `fail` lines.
- **`--json` emits NDJSON**: progress events first, then the result envelope as the **last line**. Take the last line as the result; do not parse the whole stream as one object.
- Progress goes to **stderr** in human mode, so `agenthub doctor > out.txt` still captures only the report.
- **It is read-only without `--fix`.** With `--fix` it performs only safe repairs (recreating a missing directory, repointing a stale client entry); anything destructive is printed as a command for the user to run, never executed.
- Probing every enabled server takes **up to 8s each**, 8 at a time — a run with many servers is slow on purpose, not hung. The progress lines name each server as its probe finishes.

| symptom | look here |
|---|---|
| "it stopped working", cause unknown | `doctor` — then follow the failing check |
| server will not connect | `server test <id>` — real error plus child's stderr tail |
| a server you added is invisible | `add` leaves it DISABLED — `server enable <id>` |
| a tool vanished | `server inspect <id>` visibility (§6) |
| client sees nothing | `server inspect` first; did the user restart the client? |
| a client points at an old binary | `doctor` reports it; `--fix` repoints it |
| exit 4 | that command needs a running daemon; nothing on this path does |
| exit 6 | governance denied it — report it, do not retry |

`server inspect` always prints a **visibility** section — the first command for "enabled, credential stored, client still sees nothing". Lists which profiles include and exclude the server, bound clients that can and cannot reach it, what an unbound client gets, and local tool-override count.

## 8. Wire trace

**Wire trace** — for when `server test` passes but a tool misbehaves inside the client:

```bash
agenthub server trace linear on     # off by default, per server
# user reproduces the problem in their client
agenthub server logs linear         # last 100 frames; -f to follow; --limit 0 for all
agenthub server trace linear off    # turn off when done
```

- `logs` reads; `trace` fills the file. An empty log means nothing was recorded, not that the server was idle.
- Frames are captured **before any redaction**, verbatim in `<data>/logs/server-<id>.log`. Read the file; never paste it wholesale into a chat or commit.
- Takes effect immediately — no client restart or server reconnect needed.
- `server ls` grows a `TRACE` column while any trace is on.
- Correct-looking frames plus a misbehaving client → the problem is above this layer (§6 narrowing, client restart).

## Non-negotiable

- **No credential in an argv or in the registry** — `secret set --stdin`, `${SECRET_X}` in the definition.
- **No invented command or flag** — `--help` decides.
- **Exit 6 is an answer, not an obstacle.** Report it.
- **`--dry-run` before editing a client's config; `server test` before `client connect`.**
- **Say when a step needs a human** (OAuth browser, client restart).
