---
name: agenthub
description: Drive the agenthub CLI to give an AI client MCP tools - add a server (curated catalog, pasted config, or by hand), store its credentials, run its OAuth login, test it for real, connect it to Claude Code / Cursor / Codex / VS Code / Zed, narrow what each client may see, and diagnose one a client cannot reach or that answers wrongly. Use whenever the task involves adding, authorizing, testing, wiring up, or troubleshooting an MCP server.
---

# agenthub

One gateway between every AI client and every MCP server. Wire a client up **once**; add, authorize, and narrow servers here afterwards without touching the client again.

**Never guess a flag** — `agenthub <group> --help` decides. This file covers exactly what a release advertises: `server`, `auth`, `secret`, `catalog`, `profile`, `client`.

The path is always: **add → (credential) → enable → test → connect → narrow**.

## Reading output

```bash
agenthub server ls           # configured servers
agenthub client ls           # wired clients, their profiles
agenthub client detect       # all known clients, config paths, writability
```

Pass `--json` globally. Envelope: `{"ok":true,"data":…}` or `{"ok":false,"error":{"code":"E_…","message":"…","hint":"…"}}`. **Branch on exit code:**

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

Default is every enabled server. Narrowing lives on the **profile**; a client selects one and never carries servers or tools of its own.

```bash
agenthub profile create research
agenthub profile server add research brave
agenthub profile tools research brave --only search      # or --all / --none
agenthub profile discovery research lazy                 # or grouped / full / -
agenthub client bind cursor research                     # takes effect on running sessions
agenthub client unbind cursor
agenthub profile use research    # fallback for every unbound client ("-" clears it)
```

- `profile tools` takes the server's **own** names (`search`), not the exposed `brave__search`. Get names from `server test <id> --tools`.
- A misspelled name is stored and only warned about. `--only` is an intersection — one typo lets **nothing** through. `server test --tools` first is what makes the check real.
- Binding to a nonexistent profile is accepted and **fail-closes to an empty scope**. Client seeing nothing → check `client ls` for `MISSING` before suspecting servers.

**Discovery** changes how tools are surfaced, not which are in scope:

| mode | `tools/list` returns |
|---|---|
| `lazy` | meta-tools (`search_tools` / `describe_tool` / `call_tool` / …) — **default** |
| `grouped` | one aggregate entry per server plus `call_tool` |
| `full` | every visible tool individually |

On a small surface where tools seem missing, check if the client is in `lazy` mode — tools are found via `search_tools`, not a handed list.

**`server disable <id>`** is the kill switch above every profile. **Prefer `disable` over `rm`** — `server rm` destroys the whole footprint (credential, profile membership, baselines, cached tool list; audit records survive) without confirmation. Secrets cannot be read back.

## 7. Diagnose

```bash
agenthub server inspect brave --tools   # recorded state, offline (the cache)
agenthub server test brave --tools      # live answer today
```

`server inspect` always prints a **visibility** section — the first command for "enabled, credential stored, client still sees nothing". Lists which profiles include and exclude the server, bound clients that can and cannot reach it, what an unbound client gets, and local tool-override count.

| symptom | look here |
|---|---|
| server will not connect | `server test <id>` — real error plus child's stderr tail |
| a server you added is invisible | `add` leaves it DISABLED — `server enable <id>` |
| a tool vanished | `server inspect <id>` visibility (§6) |
| client sees nothing | `server inspect` first; did the user restart the client? |
| exit 4 | that command needs a running daemon; nothing on this path does |
| exit 6 | governance denied it — report it, do not retry |

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
