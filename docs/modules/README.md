# Module documentation

Five documents, one per layer. Each opens with how that layer collaborates as a whole, then walks
through the packages one by one:
**one-line responsibility → key types and entry points → invariants and failure directions**.
Two more documents are organized by **external constraint** rather than by layer:
[oauth.md](oauth.md) and [gui.md](gui.md).

For a quick lookup of which package belongs to which layer, see
[../architecture.md §3 Core module map](../architecture.md#3-core-module-map).

The most valuable part of each package writeup is "invariants and failure directions" — it answers
"what must not be touched, and which way does this fall over when it breaks". Most of these
constraints are invisible from a function signature; a glance before you change code can save you an
incident.

| File | Packages covered |
|---|---|
| [foundation.md](foundation.md) | `platform`, `logx`, `tier`, `mcp` (+ four `transport` implementations), `registry` |
| [config.md](config.md) | `scope`, `session`, `event`, `secrets` (+ `secureenv`), `clients`, `skills` |
| [dataplane.md](dataplane.md) | `downstream`, `router`, `pipeline`, `gateway`, `discovery` (+ `toolsig`), `shaping` (+ `toonenc`), `ratelimit` |
| [security.md](security.md) | `guard` (`injection`/`spawnguard`/`netguard`/`leakguard`), `integrity`, `approval`, `audit`, `oauthflow` |
| [controlplane.md](controlplane.md) | `api`, `ctlapi`, `confops`, `catalog`, `daemon`, `httpbridge`, `cli` (+ `output`), the two `cmd/` binaries, `testutil/fakemcp`, `depguardtest`, `test/*` |
| [oauth.md](oauth.md) | Topic: how well `oauthflow` conforms to the MCP authorization spec, which provider deployment shapes are supported, known gaps; `oauthlogin` (the same flow, as a pollable session) |
| [gui.md](gui.md) | Topic: the GUI frontend's information architecture, state presentation, write operations, and what it deliberately does not do |

## How these documents are written

**Read the source first, then write.** These five documents were written against the code. Module
docs written from memory turn into misinformation after the first refactor, and misinformation is
more expensive than a blank page — a blank page sends people to the code, misinformation stops them
from going.

**A capability existing ≠ it being wired up.** Some packages are feature-complete and well tested,
but the assembly layer hasn't connected them yet. Those cases are called out explicitly under
"current assembly status" in the relevant package section rather than glossed over.
[../architecture.md §12](../architecture.md#12-assembly-status-implemented-but-not-yet-wired-up) has a summary.

**A gap already pinned down to a specific line belongs HERE**, in the section for the package that
owns it — say plainly that it is owed rather than describing it as done. It is read by whoever
touches that code; a list of its own would not be.
