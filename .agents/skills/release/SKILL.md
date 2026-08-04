---
name: release
description: Cut an AgentHub release using the repository's preflight, versioning, changelog, tagging, and post-release verification procedure. Use when preparing or publishing a release.
---

# Releasing

One tag push does the work. Everything before it is local and reversible; everything after it is
public within seconds, so the cheap checks come first.

---

## 0. Preflight

```bash
git fetch origin
git status --short                      # must print nothing
git log --oneline -1 origin/main        # must equal local main
git worktree list                       # anything here is NOT being released
```

**A release ships `main` exactly as it stands.** Nothing is cherry-picked. If a worktree holds work
that belongs in this version, land it first by the [new-feature skill](../new-feature/SKILL.md) step 5 and start over
at step 0. Otherwise say out loud which branches are being left out, so "I thought that was in" is
caught now rather than by a user.

## 1. Choose the number

Against the **user-visible surface**, not the size of the diff. When two rows could apply, take the
lower one.

| What changed | Bump |
|---|---|
| Docs, comments, tests, internal refactors; help text corrections | patch — `0.13.0` → `0.13.1` |
| A new command or flag, a new capability, a changed default | minor — `0.13.1` → `0.14.0` |
| A removed command or flag, or a moved frozen identifier ([canonical.md](../../../docs/canonical.md) §1) | major |

A pre-release carries a `-` (`0.14.0-rc1`) and the workflow marks the Release accordingly.

## 2. Bump it in all three places

```bash
new=0.13.2 ; old=$(cat VERSION)
printf '%s\n' "$new" > VERSION
sed -i '' "s|version-${old}-blue|version-${new}-blue|" README.md README.zh-CN.md
git diff --stat                         # exactly 3 files
```

`VERSION` reaches the binary, the `.app`'s `Info.plist` and the Release title on its own. The two
README badges derive from nothing and are edited by hand — `TestReadmeBadgesMatchVERSION` fails when
they disagree, which is why step 4 runs the tests before the tag exists.

## 3. Write the changelog

**The changelog is the release commit message.** There is no `CHANGELOG.md`. The published notes come
from three sources; only the first is yours — the commit list is GitHub's
(`generate_release_notes: true`), and the install instructions already live in
`.github/workflows/release.yml`.

```bash
git log --oneline "$(git describe --tags --abbrev=0)"..main
```

The message answers "does this version affect me?". Lead with the one thing a user would feel; if
nothing is observable, say so in a line — that is useful, and "various fixes and improvements" is
not. GitHub prints the commit list underneath you, so restating it is wasted.

```
release: 0.13.1

Documentation, comments and three new build-rule guards. No behaviour change
beyond two help strings that were shipping internal milestone names to users.

The one item a user actually feels is in the skill the tap ships: it named
two streaming commands where there are four, so an agent following it parsed
NDJSON as a single object and failed on the first progress line.
```

## 4. Verify, commit, push

```bash
make ci                                 # must be green BEFORE the tag exists
git add VERSION README.md README.zh-CN.md
git commit                              # message from step 3
git push origin main
```

## 5. Tag and push

The tag is the trigger and the claim about what is being released, so it must equal `VERSION`.

```bash
git tag -a "v${new}" -m "v${new}"
git push origin "v${new}"
```

## 6. Watch the workflow

```bash
gh run list --workflow=release.yml --limit 1
gh run watch <run-id> --exit-status
```

Six jobs, about four minutes: `verify` (checks tag against `VERSION` and the tap token; gates the
rest), `cli` (darwin/linux/windows tarballs), `gui-macos` (universal DMG), `gui-windows` (amd64 and
arm64 zips), `publish` (the Release with every artifact), `homebrew` (formula, cask and skill to the
tap, one commit — skipped, and saying so, when the `HOMEBREW_TAP_REPO` variable is unset).

## 7. Check what shipped

A green workflow is a weaker claim than a correct release.

```bash
gh release view "v${new}" --json isDraft,isPrerelease -q '.isDraft, .isPrerelease'   # false false
gh release view "v${new}" --json assets -q '.assets[].name' | wc -l                  # 12
```

Twelve assets: four CLI tarballs (darwin/linux × amd64/arm64), the macOS DMG, a Windows amd64
tarball, two Windows zips, three `checksums-*.txt`, and `manifest.json`.

That last one carries the whole Homebrew-less install path, and its absence is the invisible
failure: the Release publishes, the tap updates, and only `curl … | sh` breaks.

```bash
curl -fsSL https://raw.githubusercontent.com/dinstein/agent-hub/main/scripts/install.sh \
  | sh -s -- --prefix /tmp/agenthub-check
/tmp/agenthub-check/bin/agenthub --version          # ${new}, and must NOT say (dev)
rm -rf /tmp/agenthub-check
```

`test/installer` runs that script end to end against a served release on every `make ci`; the two
lines above are the half it cannot cover — that the real Release holds what the script goes looking
for.

Then the tap, a different repository that fails on its own:

```bash
for f in Formula/agenthub.rb Casks/agenthub-gui.rb; do
  gh api "repos/dinstein/homebrew-agenthub/contents/${f}" -q .content \
    | base64 -d | grep -E 'version|url|depends_on formula' | head -4
done
```

The `version` must be the new one in both — the cask's carries the build hash after a comma — and
every `url` must point at **`dinstein/agent-hub`**, not at the tap. Both files land in one commit, so
one of them being stale is a bug in `tap-sync.sh`, not a re-run.

The cask is the only artifact whose install is not exercised by anything in CI. Once per release, on
a machine that has not installed it before — a fresh user account is enough, the quarantine flag is
per-download and this machine's copies are already cleared:

```bash
brew install --cask dinstein/agenthub/agenthub-gui   # pulls the formula with it
open -a AgentHub                                     # must launch with no Gatekeeper prompt
which agenthub && agenthub --version                 # from the formula, and must NOT say (dev)
brew uninstall --cask --zap dinstein/agenthub/agenthub-gui
```

`brew audit --cask --strict --online dinstein/agenthub/agenthub-gui` grades the file; only the four
lines above grade the install.

## When it goes wrong

| Symptom | Meaning | Do |
|---|---|---|
| `verify` fails on the tag | Tag and `VERSION` disagree; nothing was built or published | `git tag -d` and `git push --delete`, fix `VERSION`, re-tag |
| A build job fails | Usually toolchain, not code — `v0.13.0` failed on `wails3` needing `CGO_ENABLED=0` | Fix on `main`, move the tag to the new commit, force-push it. No Release exists yet |
| `publish` green, `homebrew` red | The Release is complete; only the tap lagged | Re-run that job. **Do not re-cut the release** |
| A wrong Release is already public | `gh release delete` leaves the tag behind | Cut the next patch forward rather than deleting |
| GitHub Actions unavailable | — | `scripts/release-local.sh vX.Y.Z` — without `--push` it stops before the two irreversible steps and prints them |

Rehearse the whole pipeline without a tag via `workflow_dispatch`: every job runs and `publish` skips
itself.

---

## Not a release: run this checkout where Homebrew installed one

Publishes nothing — no tag, no Release, no tap commit. Use it to exercise a build with a real client
before releasing it.

```bash
make install-to-brew                     # release-channel build → $(brew --prefix)/bin/agenthub
scripts/install-to-brew.sh --restore     # give the path back to Homebrew
```

A dirty tree is fine here; the version carries `-dirty`. A dev-channel binary is refused. Replacing
the file does not replace a running daemon: quit and reopen AgentHub if the application is running
one, or `agenthub daemon restart --headless` if an operator is.
