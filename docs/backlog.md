# Known gaps (development plan)

> This file records gaps that are **confirmed to exist but not yet fixed**. Every entry was checked
> against the code when it was written, and comes with a reproduction and a verification command — this
> is not a wish list written from memory.
>
> Bar for inclusion: gaps you can **point to a specific location in the code** for. Vague "could be
> better" items don't belong here.
> Delete an entry once it's fixed, and update the corresponding `modules/` doc to describe reality.

Division of labor with [windows.md](windows.md): that document covers one platform's overall state; this
one covers cross-platform, scattered gaps already pinned to a line.

---

## Gap index

### 1. `internal/daemon` HTTP data-plane tests time out intermittently under heavy load

**Symptom.** With CPU contention manufactured (24 `yes` processes), running
`go test ./internal/daemon/ -count=20 -race` intermittently hits `waitFor`'s 10s timeout:
`TestHTTPDataPlaneServesRealCall` (tools/list never contains `fake__echo`) and
`TestHTTPDataPlaneTokenTierIsEnforced` (the tier gate never refuses). Eight consecutive runs on an idle
machine were all green, so it really does only show up under load.

**Correction to the earlier entry.** This entry originally recorded "`TestHTTPDataPlaneRejectsBadCredentials`
fails intermittently" and guessed the root cause was **a timing issue between connection reuse and the
401 decision**. Neither holds:

- What reproduces is a `waitFor` timeout, not a credential misjudgment. `RejectsBadCredentials` was never
  seen failing under load testing. The original entry recorded only the test name, not the failure
  output, and a failure inside `initialize()` gets attributed to the parent test — quite likely what was
  seen back then was the same class of timeout.
- Authentication has nothing to do with connection reuse: `Authenticator.Authenticate`
  (`internal/httpbridge/auth.go:113`) parses the `Authorization` header per request, with no
  per-connection identity cache.

**The evidence is now in place.** On timeout, `waitForDetail` (`internal/daemon/daemon_test.go`) prints
the last observation (which tools tools/list actually returned, what error the gate actually returned)
plus **every goroutine stack** — the daemon runs **in-process**, so this is the equivalent of e2e's
SIGQUIT stack capture.

**Next step.** Get one failure with stacks and determine whether it's a genuine hang or whether 10s
simply isn't enough for 20 cold daemon starts under `-count=20 -race`. **Don't touch `testTimeout` before
that**: raising the timeout would turn this into "fails intermittently, but slower," and would delete the
only clue that distinguishes the two possibilities.

**Reproduction attempts (don't repeat wasted effort).**

| Date | Conditions | Result |
|---|---|---|
| 2026-07-29 | 24 `yes` processes + `-count=20 -race`, 3 rounds | **Reproduced twice** (`ServesRealCall`, `TokenTierIsEnforced`) |
| 2026-07-29 (later) | Same, 4 rounds | Not reproduced |
| 2026-07-29 (later) | 28 `yes` processes + all packages `-count=8 -race`, 6 rounds (≈48 runs) | Not reproduced |

**Not reproducing isn't the same as fixed**, so this entry stays. None of the changes landed in the
meantime obviously points at it (the run directory following the data directory only affects Linux, and
these tests use an explicit `AGENTHUB_SOCKET`; the crash marker adds one file read/write at daemon
startup; `waitForDetail` does one extra string format per polling round — if any of these matter, the
direction is to make the timeout **more** likely, not less). The most likely explanation is machine state
(thermals, background load). Settling it requires **one failure with stacks**, not another run that
didn't fail.

---

### 2. On Windows, dev and release share the same control pipe

**Symptom.** Under the same user, a development build and an installed release build resolve to **the
same** control endpoint, `\\.\pipe\agenthub-ctl-<sha8(SID)>`. The data directories are already separated
by channel (`AgentHub` / `AgentHubDev`), but the endpoint isn't — so two daemons compete for one pipe,
whoever binds first wins, and the losing client ends up talking to a daemon holding a different registry.

This was found while fixing the analogous gap on Linux (the run directory following the data directory,
now fixed), in the cross-platform table of `TestDevResolverSeparatesFromRelease`: the Windows row was the
only one that couldn't assert endpoint separation. The test therefore carries an explicit
`endpointSeparates: false` field pointing back at this entry, rather than dropping Windows from the table
— dropping it would leave this gap unwatched again.

**Root cause.** `windowsCtlEndpoint` (`internal/platform/windows.go:202`) derives the pipe name from the
SID alone. Channel separation on Unix holds **indirectly**: the endpoint is `<run>/ctl.sock`, the run
directory follows the data directory, and the data directory follows the channel. The Windows endpoint
isn't a filesystem path, so that chain of transmission simply doesn't exist there.

**Approach (mind the trap a previous attempt fell into).** The pipe name is a **frozen identifier**
(CANONICAL §1/§2), so the release name can't move; and it **must not** be derived from `dirName` — that
was tried once, and the result was that "rename the data directory" silently became "rename the
protocol." So the correct shape is to give the dev channel **a second, equally frozen** name (for example
`agenthub-ctl-dev-<sha8(SID)>`) rather than splicing the channel into the existing name's derivation.
This requires the Resolver to know the channel, i.e. exactly the "decide by build channel" that the Unix
side deliberately avoids — unavoidable on Windows, because there's no environment variable to carry it.

**Why not fixed this round.** Windows has never been verified on real hardware anywhere
(`docs/windows.md`); this repo only has a cross-compile gate. Adding a new frozen identifier on a
platform that can't run is freezing an unverifiable guess into the ABI. Do it alongside the named pipe
listener once real hardware is available.

**Verification.** With the windows row of `TestDevResolverSeparatesFromRelease` changed to
`endpointSeparates: true`, the test must pass.

---

### 3. `profile tools --only` accepts tool names no server has

**Symptom.** `agenthub profile tools <profile> <server> --only <typo>` succeeds. The selector is stored
verbatim, and because an allow-list is an intersection, a name matching nothing lets *nothing* through
for that server. The operator asked for one tool and silently got zero.

**Root cause.** `confops.SetProfileTools` (`internal/confops/profile.go:273-284`) validates that the
profile exists and that the server exists, then calls `applySelector` with the names as given. Nothing
compares them against the server's cached tool catalog.

**Why it is not urgent.** The direction is fail-closed: a typo costs visibility, never grants it. That
is the right way round, and it is why this is a gap rather than a bug.

**Why it is still worth fixing.** It is invisible. The command prints success, `profile ls` shows the
rule as written, and the tool is simply missing in the client — a symptom nobody traces back to a
spelling mistake in a command that reported OK. `server add --stdin` had the same shape (a silently
dropped `oauth` block) and it survived for months; the fix there was `DisallowUnknownFields`, i.e. make
the mismatch loud.

**Approach.** The catalog is already cached per server (`agenthub server test <id> --tools` reads it),
so `SetProfileTools` can warn — not refuse — when a name is absent from it. A warning rather than an
error because the cache may be cold: a server that has never connected has no tool list, and refusing
there would block a legitimate rule written ahead of the first connection.

**Verification.** A test that sets `--only ghost_tool` against a server with a populated cache must
come back with a warning naming `ghost_tool`, and must still store the rule.

---

## Appendix: how these gaps were found

They surfaced during a session on 2026-07-27, while connecting two real MCP servers (both going through
the same enterprise SSO OAuth). Three problems found in the same batch, all **already fixed**:

| commit | Problem |
|---|---|
| `5272d44` | `--issuer` was entirely ineffective for all remote servers (`ResourceURL` unconditionally took precedence) |
| `f2ac941` | `server add --stdin` silently discarded the `oauth` block (`stdinEntry` didn't model the field) |
| `e8cbb28` | The RFC 9728 candidate list was missing the origin root path |

A lesson worth recording: `normalizeStdin` **had zero test coverage** before this, which is exactly why a
silently dropped field survived so long. The fix also added `DisallowUnknownFields` — unmodeled keys now
raise an error outright, instead of being dropped in silence so the failure shows up several steps later
wearing an unrelated face.
