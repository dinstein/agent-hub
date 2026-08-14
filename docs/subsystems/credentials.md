# The credential vault

> **Answers** where a downstream's credential is read from, in what order, and what happens when a level is broken.
> **Not here** how an OAuth grant is obtained → [oauth.md](../modules/oauth.md); how a credential is injected → [downstream.md](downstream.md).
> **Kept true by** `internal/secrets/multiproc_test.go` (the N-process acceptance test), `TestBackendIgnoresEnvironmentLevels`, and a golden test on the storage-key prefix.

`internal/secrets` is a four-level chain over environment variables, an XChaCha20-Poly1305 encrypted
file and the OS keyring. Every entry is addressed by `(ServerID, Scope) + Key`, with `Scope` defaulting
to `_global`. `internal/secrets/secureenv` builds the environment a spawned child receives.

The discipline both share, and the one to keep when editing: **if you cannot read it, error; if you
cannot change it, refuse; if you do not understand it, do not write it.**

## The chain

Four levels, first hit wins.

| Level | Source | Active when |
|---|---|---|
| 1 | environment variable `AGENTHUB_SECRET_<KEY>` | always |
| 2 | bare environment variable `<KEY>` | explicit opt-in `AGENTHUB_ALLOW_BARE_SECRET_ENV=1` |
| 3 | `secrets.enc`, XChaCha20-Poly1305 | `AGENTHUB_SECRET_KEY` set, or the dev-fallback pair of files already exists |
| 4 | OS keyring | the availability probe passes |

**An empty or whitespace-only value counts as unset at the two environment levels.** `envValue` trims
before accepting, so an exported-but-empty variable falls through rather than shadowing the vault.

**Owed: levels 3 and 4 do not apply that trim.** `Get` returns whatever the enc file or the keyring
holds and `Set` accepts any value, so a stored empty string is reported as present and shadows the
keyring level beneath it. The consequence is caught one layer up instead: `downstream.expandSecrets`
treats `!ok || val == ""` as unresolved, which is what stops a header expanding to nothing and turning
an authenticated endpoint anonymous. That guard is narrower than the rule above — it tests `== ""`, not
a trim — so a whitespace-only vault entry is refused at levels 1–2 and delivered at levels 3–4. Owed
rather than fixed here because tightening `Get` changes what `secret get`, `server inspect` and the
control plane answer for an already-stored value.

**Level 2 being off by default is fail-closed**: no arbitrary environment variable is a credential
unless the user asks. Even when on, `envValue` never resolves a variable starting with `AGENTHUB_`
through the bare path — the opt-in must not become a way to read out our own control variables. An
entry named `key` would map to `AGENTHUB_SECRET_KEY`, the key material for the encrypted file, so that
name is skipped explicitly: key material must never be readable through the credential chain.

**"Could not read it" and "read something broken" must be distinguished.** A file that will not
decrypt, or a keyring reporting anything other than not-found, is an **error**, never a miss carried
further down the chain. A mistyped `AGENTHUB_SECRET_KEY` or a broken keychain must be visible, not
degraded into "that credential is not set". The only exception is a keyring whose availability probe
fails: that machine has no such level, so it is skipped without error and writes land in the encrypted
file instead.

## Keyring hardening

Three measures, none optional.

- **The availability probe reads only, never writes.** A `Set` probe triggers the destructive macOS
  confirmation dialog. It reads a well-known nonexistent account, and both success and
  `ErrKeyringNotFound` prove the backend alive, while a timeout or any other error marks it
  unavailable.
- **The probe's conclusion is cached for the process lifetime**, so an unavailable keyring flips the
  chain to the encrypted file without re-prompting per call.
- **Every operation has a hard timeout** (3s default), after which the worker goroutine is deliberately
  abandoned: a stuck keychain prompt cannot be cancelled, so abandoning it is the only way to unblock
  the caller. The result travels over a buffered channel, so an abandoned worker can never collide with
  the caller's return value.

**The OS keyring cannot be enumerated, so there is a self-managed registry.** `keyring-keys.json`
mirrors key **names** only, never values, and is modified only in sync with a successful keyring
mutation, so it neither claims keys the keyring has lost nor misses keys it still holds.

**`HasUnreadableEnc` exists so an exhaustive caller can fail loud.** With an enc file on disk this
process has no key for, `List` silently returns only the keyring half, and an empty answer is
indistinguishable from an empty vault — a credential purge built on it reports success while a refresh
token survives, and re-adding the same server id revives it. Any doubt, including a stat error, answers
**true**.

## The dev-mode fallback

Every `go build` produces a new unsigned binary and the macOS keychain ACL re-prompts each time, so a
failed keyring probe (or `AGENTHUB_DEV_SECRETS=1`) sends writes to `secrets.enc` under a key
auto-generated and persisted **beside it** in `secrets.enc.key` (0600).

Say the quiet part out loud: a key stored next to the ciphertext is obfuscation, not encryption at
rest, and an attacker who reads both files has the plaintext. Acceptable for the dev fallback only.
`encForRead` requires **both** files to exist before using that key, so data written by the dev backend
stays readable once the keyring probe passes again.

## Persistence

The whole map is sealed under a single random nonce. The AAD is `"agenthub/secrets/v1"`, binding the
ciphertext to the format version so a v2 envelope can never be replayed as a v1. Writes go through the
atomic ladder — temp file in the same directory, 0600, write, fsync, rename, fsync parent. A missing
file is an empty map, not an error.

**`storageKeyPrefix = "agenthub/v1"` is frozen and golden-tested.** Changing it orphans every stored
credential. Within a component only `%` and `/` are percent-escaped — the only two bytes that break the
delimiter structure — so `secret ls` stays readable. `ParseStorageKey` errors on an unknown prefix or
malformed escaping rather than dropping a key it cannot decode from an enumeration.

## Migration

`Migrate` goes read old → write new → read back and verify → delete old, and a failure at any step
leaves two copies: a duplicated credential is recoverable, a lost one is not.

It **must** be passed backend-level stores (`Chain.Backend`), never a `*Chain`: `*Chain.Get` consults
environment variables first, so one environment variable could satisfy the read-back while the new
backend stored nothing — and passing verification is exactly what deletes the old entry.
`Chain.Backend` resolves availability eagerly, returning `ErrBackendUnavailable` on the spot, because
discovering halfway through that the destination cannot be written is how a half-migrated vault
happens.

It stays a command rather than automatic behaviour: backend availability changes underneath the
operator, and moving credentials nobody asked to move is worse than leaving them. The environment
levels have no `Store` and are not in `BackendKinds()` — they are per-process input, not storage.

## Concurrency: two locks, in one order

`Chain` serializes its own operations with an in-process mutex, and every **write** additionally takes a
cross-process lock — a dedicated `vault.lock`, held across the whole read-modify-write cycle. The
in-process mutex is taken **outside** the file lock, so goroutines of one process queue in memory and
only one ever competes for the file; the reverse order would have each open its own descriptor and
contend through the filesystem for no gain, since `flock` is per-open-file-description.

**All six write paths take it**, not just the two the API leads with: `Chain.Set`, `Chain.Delete`, and
the four backend-level methods behind `Chain.Backend`. The backend stores need it most — their caller is
`Migrate`, so a racing writer that clobbers the destination between the write and the read-back turns a
verified handover into the deletion of the last remaining copy.

**The lock covers key selection, not only the map update.** `encForWrite`'s dev branch calls
`loadOrCreateDevKey`, a read-then-create of `secrets.enc.key`; two processes reaching it unguarded each
generate a key and the second overwrites the first, leaving an enc file **neither can open** — the whole
vault rather than one entry.

**A dedicated lock file, never the data files.** `secrets.enc`, `keyring-keys.json` and
`secrets.enc.key` are all replaced by rename, so a lock on one of those inodes guards nothing: the
winner renames a new file over the path and the two processes hold locks on different inodes.
`internal/ratelimit` reached the same conclusion for its counter file.

**Reads and the announcement stay outside it, deliberately.** Writers publish by rename, so an unlocked
reader sees one whole version or the next, never a splice — and `Get` sits on the hot path of every
`${SECRET_X}` expansion.

**Failure direction: fail closed.** Every acquisition failure, including a build with no `flock`
implementation, returns without the lock and the write reports that it could not run. A write that says
it did not happen is recoverable; a credential that silently vanished is not. Both cases of the
N-process test were run against a build with the lock removed: the first loses entries, the second
reports `cannot decrypt secrets.enc`.

**Tests never touch the real keychain.** The keyring sits behind the `Backend` interface with fakes
injected everywhere; the real-backend smoke test runs only under `AGENTHUB_TEST_REAL_KEYRING=1`.

**Owed: a keyring credential is committed before its enumeration record.** `setLocked` writes the value
to the OS keyring and only then calls `registryAdd`. If `keyring-keys.json` cannot be created, synced or
renamed, the caller gets an error while the credential survives in the keyring, unlisted: `List` reads
that registry, so exhaustive server removal and later migration both miss it, and reusing the same
server id can resurrect it. Either pre-register the key and keep that conservative record on an
ambiguous write, or roll back a confirmed keyring write when `registryAdd` fails.

## internal/secrets/secureenv

Pure functions building a hardened environment for a downstream about to be spawned: allowlist
admission, login-shell PATH capture, proxy-variable userinfo redaction.

**Only the PATH half is wired.** `LoginPATH` and `MergePATH` are called from `internal/downstream`'s
`widenPATHIfNeeded`, so a stdio child whose command cannot be found under the PATH it would be given is
retried against the login shell's. `Filter`, `Config`, `RedactProxyValue` and `CaptureLoginPATH` have no
caller outside this package's tests.

What a spawned downstream actually receives today is the parent environment minus the `AGENTHUB_`
prefix, stripped by `internal/downstream`'s own `buildEnv` — a deny list, the opposite shape from the
allowlist below. Recorded rather than fixed because admitting only the allowlist changes the
environment of every spawned server, which can break a downstream that reads a variable nobody
enumerated.

**Deny by default**, in `Filter`, which nothing calls yet. **The `AGENTHUB_` prefix is a hard deny that
`Config` cannot override**, and that half IS in force, because `internal/downstream` strips the prefix
itself: the two were designed to stack idempotently and today only the second runs.

**Proxy variables are not forwarded by default**, since proxy endpoints frequently embed credentials.
With `ForwardProxy` on, values go through `RedactProxyValue`: `NO_PROXY` is a plain host list and passes
verbatim, a value with no `@` passes verbatim, and a value containing `@` that cannot be positively
identified and stripped as URL userinfo is **dropped outright**. We never forward a value we cannot
prove is credential-free.

**`LoginPATH` is deliberately fail-open.** Processes launched by launchd or systemd inherit a truncated
PATH, and the login shell's is the one an interactive user actually has. Capture takes the last
non-empty line of output — a profile may print a greeting first — with a 5-second hard timeout across
both modes and `cmd.WaitDelay = 1s` to force the pipes closed, since otherwise the login shell's
children inherit the stdout pipe and `Output` blocks until every descendant exits. Any failure falls
back to the current process's PATH, so the worst case is keeping the truncated PATH we already had.

**`-l` alone is not enough, which is why `captureModes` is a list**: `-i -l -c 'echo $PATH'` first,
plain `-l -c` as fallback. A login shell sources only the login profile, while the line that puts
Homebrew, nvm or pyenv on PATH conventionally lives in the **interactive** rc file — so the directory
holding `npx` is exactly the one `-l` does not find. Measuring this from a terminal proves nothing:
`zsh -l -c 'echo $PATH'` prints a complete PATH only because it inherited the interactive shell's.

**`MergePATH` appends and never reorders.** `base` is preserved byte for byte and only the directories
of `extra` it does not already list are appended, so the result is a strict superset in which every
command that already resolved under `base` resolves to the same file — which is what lets a caller apply
it unconditionally rather than behind a guess at whether the current PATH looks truncated. Empty entries
in `extra` are dropped, since POSIX reads one as the current directory; an empty entry already in `base`
is left alone, since removing it would change what `base` resolves.
