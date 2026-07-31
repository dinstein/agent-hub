// Package secrets is the credential vault of agenthub: a four-level
// resolution chain over environment variables, an encrypted file
// (secrets.enc) and the OS keyring (task M1).
//
// Resolution chain (first hit wins; empty / whitespace-only values count as
// unset at every level):
//
//  1. env AGENTHUB_SECRET_<KEY>
//  2. bare env <KEY> — only with the explicit opt-in
//     AGENTHUB_ALLOW_BARE_SECRET_ENV=1 (fail-closed by default: arbitrary
//     environment variables are never treated as secrets unless asked)
//  3. secrets.enc — active when AGENTHUB_SECRET_KEY is set (or via the dev
//     fallback below); the whole map is sealed with XChaCha20-Poly1305
//     under a random nonce and written atomically with 0600 permissions
//  4. OS keyring (zalando/go-keyring), hardened as follows:
//     availability probe uses a read, never a write (a Set probe triggers
//     the destructive macOS prompt); the probe result is cached for the
//     process lifetime; every operation runs under a hard timeout so a
//     hung keychain prompt cannot wedge the caller; and because OS
//     keyrings cannot enumerate, a self-managed key registry file mirrors
//     which storage keys live in the keyring.
//
// Composite vault key (ruling A.5 #26, promoted to M1): every entry is
// keyed by (ServerID, Scope) plus the entry Key, with Scope defaulting to
// "_global". The storage-key encoding is frozen and golden-tested — see
// Ref.StorageKey.
//
// Dev-mode fallback (ruling A.6 #5, decided): during development every
// `go build` produces a new unsigned binary, so the macOS keychain ACL
// re-prompts on each build. Ruling: when the keyring availability probe
// fails, or AGENTHUB_DEV_SECRETS=1 is set, writes automatically fall back
// to secrets.enc using an auto-generated 32-byte key persisted beside it
// (secrets.enc.key, 0600). Storing the key next to the ciphertext is
// obfuscation, not encryption at rest — an attacker who can read both
// files has the plaintext. That is acceptable for the dev fallback only;
// production paths use AGENTHUB_SECRET_KEY or the OS keyring.
//
// Concurrency: a Chain serializes its own operations with an in-process
// mutex, and THAT IS THE ONLY SERIALIZATION THERE IS. No cross-process lock
// guards a vault write, and both write paths are whole-file
// read-modify-write cycles — setLocked loads the entire secrets.enc map,
// mutates one key and saves it back, and registryAdd does the same to
// keyring-keys.json. Two processes writing different entries at once
// therefore lose one of them: the daemon's refresher rotating a token while
// an operator runs `agenthub secret set` is the shape that reaches it.
//
// The one cross-process lock near the vault is oauthflow's per-server
// <server>.refresh.lock (docs/modules/oauth.md), and it does not cover this:
// it serializes REFRESHES of one server, so it neither guards a plain write
// nor helps when the racing writers name different servers. Recorded, with
// the trade-off, in docs/modules/config.md under internal/secrets.
//
// This paragraph used to say the coordination "arrives with the M1 wiring".
// M1 shipped; nothing arrived.
//
// Tests never touch the real OS keychain: the keyring sits behind the
// Backend interface with a fake injected everywhere. Real-backend smoke
// tests run only under AGENTHUB_TEST_REAL_KEYRING=1.
package secrets
