# 0002 — The shaping cache is plain files, not an embedded database

> **Status** active · **Behaviour** [subsystems/shaping.md](../subsystems/shaping.md)

`<data>/cache/shaping/<sha256(owner)>/<cursor>.json`, written atomically — temp file in the same
directory, 0600, fsync, rename — swept by TTL, with a sweep at startup.

The access pattern is a single-key point lookup with no queries, transactions or cross-key consistency,
and a corrupted entry must cost exactly one cursor. A single-file database would need a recovery
mechanism to match that, and the house rule is zero new third-party dependencies.
