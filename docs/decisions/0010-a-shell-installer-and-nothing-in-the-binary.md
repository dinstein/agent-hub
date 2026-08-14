# 0010 — A shell installer, and nothing inside the binary

> **Status** active · **Behaviour** `scripts/install.sh`, `scripts/release-manifest.sh`, `test/installer`

`brew` runs `git`, so it requires Xcode Command Line Tools; on a Mac without them the tap is not a slower
path, it is no path. `scripts/install.sh` covers macOS and Linux for the **CLI only**, using nothing but
base-system tools — sh, curl or wget, tar, awk, sed, one of shasum / sha256sum / openssl. The macOS app
stays a cask, because unpacking a DMG and owning `/Applications` is what a cask already does correctly.

It is driven by `manifest.json`, one stable-named asset per release rendered from the same checksums file
the formula reads. The manifest exists because the artifacts carry a build id in their names, which is
exactly what GitHub's `releases/latest/download/<name>` redirect cannot serve.

Three properties are not decoration: asset names and hashes are **read back** from the checksums file
rather than recomposed; the download is verified before anything is unpacked or moved, and there is **no
`--skip-verify`**; and the unpacked binary must identify itself as the manifest's version and **not** as
`(dev)` — the one failure a checksum cannot catch, whose only other symptom is a user's servers appearing
to vanish.

**The manifest's bytes are verified; its strings are not trusted.** Nothing checks the manifest itself —
it is the root — and an override variable exists so a mirror may serve it. Its strings then become a local
path, a file name and the syntax of the install receipt, so each is constrained where it enters: the asset
must be a plain file name, the version and commit must not carry what would leave the receipt unparseable,
and the unpacked binary must not be a symlink, which `[ -f ]` follows and `chmod` then acts on. Allow
lists in every case.

**Every action lives in a function, and the last line calls `main`.** The documented invocation pipes the
file into a shell, which runs what has arrived; a truncated download must define an installer and run none
of it rather than run the half it received. `test/installer` asserts that over every line-boundary prefix.

**What it does not buy.** The manifest ships from the release it describes, so it cannot vouch for those
bytes independently. This is the cask's chain of trust, not a stronger one; signing the artifacts is what
would change that, and there is none.

**[Decision 0006](0006-no-telemetry-and-no-update-checker.md) is not reopened by this.** The manifest is
fetched by a script the user ran on purpose, and no code under `internal/*` may request it.
