---
name: agenthub
description: Drive the agenthub CLI to give an AI client MCP tools - add a server (curated catalog, pasted config, or by hand), store its credentials, run its OAuth login, test it for real, connect it to Claude Code / Cursor / Codex / VS Code / Zed, and narrow what each client may see. Use whenever the task involves adding, authorizing, testing, or wiring up an MCP server, or diagnosing one that a client cannot reach.
---

# agenthub

One gateway between every AI client and every MCP server. A client is
configured **once**, against agenthub; servers are added, authorized and
narrowed here afterwards without touching the client again.

Everything below was verified against the binary. If a command you want does
not appear here, run `agenthub <group> --help` — **do not guess a flag.** A
plausible-looking command that does not exist is worse than no command,
because it fails after the user has pasted it somewhere that matters.

## Before anything else

```bash
agenthub doctor              # what is installed, where its data lives, what is broken
agenthub server ls           # what is already configured
agenthub client detect       # which AI clients exist on this machine
```

`doctor --fix` performs safe repairs only.

### Always use `--json`, and branch on the exit code

`--json` is a global flag on every command. The envelope is stable:

```json
{"ok":true,"data":[],"warnings":[]}
{"ok":false,"error":{"code":"E_SERVER_NOT_FOUND","message":"no server \"nope\"","hint":"run 'agenthub server ls' to see configured servers"}}
```

Exit codes are frozen by design. **Branch on these, never on prose** — the
wording of a message may change, these may not:

| code | meaning |
|---|---|
| 0 | success |
| 1 | generic (downstream, network, internal) |
| 2 | usage error — you got the flags wrong, re-read `--help` |
| 3 | not found (server / profile / secret / skill) |
| 4 | the daemon is offline and this command needs it |
| 5 | authentication failure → the server needs `agenthub auth login` |
| 6 | refused by governance (HITL deny, quarantine) — **not** a bug to route around |
| 7 | registry lock contention or corruption |

Exit 6 deserves care: something deliberately said no. Report it and stop;
disabling the control that produced it is not a fix.

## 1. Add a server

Try these **in order**. The first that fits is the least that can go wrong.

### a. From the curated catalog — always try this first

```bash
agenthub catalog search github          # or: agenthub catalog ls
agenthub catalog show filesystem        # description, target, params, the exact add line
agenthub catalog add fetch              # entries that need nothing: one command, done
agenthub catalog add filesystem --param directory=/Users/me/projects
agenthub catalog add github --name gh   # --name when the default id collides
```

`catalog show` prints the add command with its parameters filled as
`<placeholders>`. Read it rather than composing your own.

### b. From a config the user already has

If they paste a `mcpServers` block, a `claude mcp add-json` line, or another
client's config, feed it in whole instead of translating it by hand:

```bash
echo "$JSON" | agenthub server add my-server --stdin
```

Unknown keys are **rejected**, not silently dropped. An error here means the
paste really does contain something agenthub does not model — read it, do not
strip fields to make it pass.

To adopt everything a client already has:

```bash
agenthub client import cursor           # source becomes imported:cursor
```

### c. By hand

```bash
# stdio (a process on this machine)
agenthub server add my-server --cmd npx --args "-y,@scope/mcp-server"

# HTTP
agenthub server add remote --url https://mcp.example.com/mcp
agenthub server add local-dev --url http://127.0.0.1:3000/mcp --local
```

`--local` is required for a literal loopback URL and allows **only** that; it
never opens up RFC1918 addresses. Other useful flags: `--env KEY=VALUE`,
`--header KEY=VALUE` (both repeatable), `--cwd`, `--transport`, and the
`--oauth-*` pins in §3.

### Container isolation

```bash
agenthub server add sketchy --runtime docker --image ghcr.io/x/server:tag \
  --mount /Users/me/data:/data:ro --network none --memory 512m
```

`--runtime docker` is honoured or the command **fails** — it never quietly
degrades to running on the host. Default network is `none`; mounts are
read-only unless you write `:rw`.

## 2. Credentials

The registry is plain configuration and must never hold a secret. Put a
**reference** in the definition and the value in the vault:

```bash
agenthub server add brave --cmd npx --args "-y,@modelcontextprotocol/server-brave-search" \
  --env "BRAVE_API_KEY=\${SECRET_BRAVE_API_KEY}"

printf %s "$TOKEN" | agenthub secret set brave BRAVE_API_KEY --stdin
```

**Never put a credential in an argv.** It lands in shell history and in every
`ps` on the machine. `secret set` reads no-echo from the terminal, or from
stdin with `--stdin` for pipelines.

`agenthub secret ls` lists key names and backends. There is no read path —
values cannot be printed back, by design. If a user asks you to show a stored
secret, the honest answer is that nothing can.

## 3. OAuth login

**You cannot complete this for the user.** It needs a browser and a human.
Your job is to run the command that produces the URL and hand it over.

```bash
agenthub auth status                    # who is authorized, never any credential
agenthub auth login github              # auto-selects a flow
agenthub auth login github --manual     # prints the URL, reads the pasted callback
agenthub auth login github --device     # RFC 8628, for a machine with no browser
agenthub auth refresh github
agenthub auth logout github
```

In a non-interactive session prefer `--manual` or `--device`: `--loopback`
wants to open a browser on this machine and will simply time out (180s
default) where there is none.

When discovery fails, pin what the provider does not advertise —
`--issuer`, `--scopes`, `--redirect-uri`, `--authorization-endpoint` — or
store the pins on the entry with `server add --oauth-issuer` /
`--oauth-scope` / `--oauth-resource-metadata` so every later login uses them.

Exit 5 anywhere else means the credential expired: `auth login` again.

## 4. Test — before wiring anything to a client

This is the step that separates "the definition is stored" from "it works".

```bash
agenthub server test brave                       # connect, list tool names
agenthub server test brave --tools               # with compact signatures
agenthub server test brave --schema search       # one tool's full input schema
agenthub server test brave --tool search --args '{"query":"hello"}'   # a REAL call
```

`--timeout` defaults to 120s because a cold `npx`/`uvx` cache genuinely is
that slow — a first run that seems to hang usually is not hung.

`--tool` really invokes the tool. Treat it as you would any other side effect:
read-only tools freely, anything that writes only when the user asked for it.

## 5. Connect the client

```bash
agenthub client detect                       # ids, config paths, writability
agenthub client connect claude-code --dry-run    # ALWAYS look first
agenthub client connect claude-code
```

`--dry-run` prints the exact JSON that would be merged **and the file it would
land in**. Show both to the user before writing — this edits a file they own.

**Connect writes the user-level file by default** (`~/.claude.json`,
`~/.cursor/mcp.json`, …), because the entry carries this machine's absolute
agenthub path and a project-level file is meant to be committed. Pass
`--placement project` only when the user asks to wire up one tree — and say
that the path inside it is machine-specific. To narrow what a client sees, use
a profile (§6), never the file location.

The written entry runs `agenthub connect --client <id>`, so **servers added
later need no further client changes**. That is the whole point of the
gateway; do not go back and edit client configs per server.

`client detect` reports `writable`. A client marked `no` (Codex writes TOML)
must be edited by the user; say so rather than failing silently.

**The client must be restarted to pick up the change.** Tell the user — an
unrestarted client looks exactly like a broken gateway.

`agenthub client disconnect <id>` reverses it. With no target named it clears
the user-level file and, only if nothing of ours is there, the project-level
one — so an entry written by an older agenthub is still removed.

## 6. Narrow what each client sees

Default is every enabled server. Narrow with a profile, then bind clients:

```bash
agenthub profile create research
agenthub profile server research add brave
agenthub profile tools research brave --only search       # or --all / --none
agenthub profile discovery research lazy                  # or grouped / full / -
agenthub client bind cursor research
agenthub client ls                                        # who is on which profile
agenthub client unbind cursor                             # back to the active profile
```

All narrowing lives on the **profile**; a client only selects one. There is no
`agenthub scope` group, and a client binding never carries servers or tools of
its own.

Layers **intersect** for security fields: a narrower layer can only take
capability away, never add it. If a tool is unexpectedly missing, look for a
narrowing layer before suspecting the server.

Kill switches, which apply everywhere regardless of profile:

```bash
agenthub server disable brave       # whole server, every client
agenthub tool disable brave search  # one tool, every client
agenthub tool quarantine ls         # tools isolated by drift detection
```

`tool disable` answers `E_TOOL_NOT_FOUND` (exit 3) for a tool that has never
been seen — the switch acts on the cached catalog, so the server must have
connected at least once. Run `agenthub server test <id>` first, then disable.
`server disable` has no such requirement and is the blunter instrument if the
server has never come up at all.

## 7. Verify end to end

```bash
agenthub tool ls                    # the aggregated catalog a client will see
agenthub tool ls --search search    # ranked against a keyword query
agenthub audit                      # calls that actually arrived
```

`audit` is the only honest proof: it shows calls that **reached the gateway**,
not that a config file was written. After the user restarts their client and
asks it to use a tool, a new audit line is the confirmation.

Exposed names are `<server>__<tool>`, but **never split on `__` to recover
them** — a server id or a tool name may contain it. Use `tool ls` output.

## 8. When something does not work

| symptom | look here |
|---|---|
| exit 4 | `agenthub daemon status`; start it with `agenthub daemon start` |
| exit 5 | `agenthub auth status`, then `auth login <server>` |
| exit 6 | governance refused. `agenthub audit`, `tool quarantine ls`. Do not route around it |
| exit 3 | check the id: `server ls`, `catalog ls`, `profile ls` |
| exit 7 | another process holds the registry lock, or it is corrupt. `doctor` |
| server will not connect | `server test <id>` first — it prints the real error and the child's stderr tail |
| a tool vanished | a narrowing layer (§6) or a quarantine, before suspecting the server |
| client sees nothing | did the user restart it? `client detect` to confirm the entry is in the file |

`agenthub server logs <id>` is the JSON-RPC trace; `agenthub daemon logs` is
the daemon's own structured log.

## Rules that are not style preferences

- **Never write a credential into an argv or into the registry.** `secret set --stdin`, and `${SECRET_X}` in the definition.
- **Never invent a command or a flag.** `--help` is authoritative; this file may lag the binary.
- **Never treat exit 6 as an obstacle.** Something refused on purpose. Report it.
- **Show `--dry-run` output before editing a user's client config.**
- **`server test` before `client connect`.** Wiring an unverified server means the user debugs it inside their client, where the error is least legible.
- **Say when a step needs a human** — the OAuth browser flow and the client restart both do. Claiming an unverifiable success is worse than reporting the handoff.
