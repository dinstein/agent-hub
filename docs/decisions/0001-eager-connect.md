# 0001 — Eager connect, not lazy

> **Status** active · **Behaviour** [subsystems/execution.md](../subsystems/execution.md), [flows.md#gateway-startup](../flows.md#gateway-startup)

Keep eager connect plus "answer from cache first" at startup. Lazy connect is not adopted.

The N×M process cost that motivated it is solved by the daemon's shared streamable-http pool instead. A
cold npx or uvx cache — ten seconds to minutes, which is why `DefaultConnectTimeout` is 120s — is better
spent in the startup window, where `tools/list` answers from cache, than inside the first `tools/call`,
where it reads as an unexplainable hang.

The escape hatch stays open (`downstream.Deps.Dial` plus the tool cache), and the trigger to reopen this
should be measured cost, not a derivation.
