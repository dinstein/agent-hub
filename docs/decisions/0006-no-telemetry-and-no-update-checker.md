# 0006 — No telemetry, and no update checker

> **Status** active · **Behaviour** the absence of any such code; `test/buildrules` guards the egress claim

Neither is built. No telemetry — not even enum-only reporting, and no opt-in switch — and no update
checker.

This process holds every downstream credential, argument and result. "Enums only" would need a
`ScanForPII` gate to keep the promise, while not opening the channel costs nothing and is verifiable with
an empty packet capture.

**The implementation constraint is CI-checkable: there exists no outbound request anywhere in
`internal/*` to an agenthub-owned domain or version manifest.** Network egress falls into exactly three
categories — downstream MCP servers, OAuth authorization servers, and endpoints the user configured
explicitly. **Adding a fourth violates this decision**, and `agenthub self-update` would be exactly that;
shipping one means amending this record first.
