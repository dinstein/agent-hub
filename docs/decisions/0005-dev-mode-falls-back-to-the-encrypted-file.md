# 0005 — Dev mode falls back to `secrets.enc` automatically

> **Status** active · **Behaviour** [subsystems/credentials.md](../subsystems/credentials.md)

Every `go build` produces a new unsigned binary, so the macOS keychain ACL prompts again each time. When
keyring detection fails, or `AGENTHUB_DEV_SECRETS=1` is set, writes fall back to `secrets.enc` with a
32-byte key persisted beside it in `secrets.enc.key` (0600).

**A key next to the ciphertext is obfuscation, not encryption at rest** — true of the dev fallback only.
Production uses `AGENTHUB_SECRET_KEY` or the OS keyring.

Detection must **read, never write** — a `Set` probe triggers macOS's destructive confirmation dialog —
caches its conclusion per process, and gives every operation a hard timeout.
