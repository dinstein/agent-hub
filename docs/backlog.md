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

**Empty.** No cross-platform gap is currently pinned to a line.

This is not a claim that the code is without fault — it means nothing meets the bar above right now.
Two entries were cleared on 2026-07-30:

- **`internal/daemon` HTTP data-plane tests timing out under heavy load.** Reproduced twice on
  2026-07-29 under manufactured CPU contention (24 `yes` processes, `-count=20 -race`), then not again
  across roughly 48 further runs. Closed as not worth further tracking rather than as diagnosed. The
  evidence path is still in place if it returns: on timeout `waitForDetail`
  (`internal/daemon/daemon_test.go`) prints the last observation plus every goroutine stack. Should it
  resurface, get **one failure with stacks** before touching `testTimeout` — raising the timeout would
  turn it into "fails intermittently, but slower" and delete the only clue separating a genuine hang
  from 20 cold daemon starts simply needing more than 10s.
- **The Windows control pipe not distinguishing build channels.** Still true in the code
  (`windowsCtlEndpoint`, `internal/platform/windows.go`), and now recorded in full — including why the
  channel must not be spliced into the existing pipe name — in
  [windows.md](windows.md#the-control-pipe-doesnt-distinguish-build-channels), which is where the rest
  of the unverified-platform work already lives. `TestDevResolverSeparatesFromRelease` continues to
  carry `endpointSeparates: false` for the windows row, so the gap stays watched.

---

## Appendix: how gaps get found

The entries this file has held surfaced during a session on 2026-07-27, while connecting two real MCP
servers (both going through the same enterprise SSO OAuth). Three problems found in the same batch, all
**already fixed**:

| commit | Problem |
|---|---|
| `5272d44` | `--issuer` was entirely ineffective for all remote servers (`ResourceURL` unconditionally took precedence) |
| `f2ac941` | `server add --stdin` silently discarded the `oauth` block (`stdinEntry` didn't model the field) |
| `e8cbb28` | The RFC 9728 candidate list was missing the origin root path |

A lesson worth recording: `normalizeStdin` **had zero test coverage** before this, which is exactly why a
silently dropped field survived so long. The fix also added `DisallowUnknownFields` — unmodeled keys now
raise an error outright, instead of being dropped in silence so the failure shows up several steps later
wearing an unrelated face.
