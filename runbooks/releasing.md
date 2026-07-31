# Releasing

A runbook: every step is a command to run or a fact to check.

One tag push does the work. Everything before it is local and reversible; everything after it is
public within seconds, so the cheap checks come first.

---

## Cut a release

### 0. Preflight

```bash
git fetch origin
git status --short                      # must print nothing
git log --oneline -1 origin/main        # must equal local main
git worktree list                       # anything here is NOT being released
```

**A release ships `main` exactly as it stands.** Nothing is cherry-picked and no branch is landed as
part of releasing. If a worktree holds work that belongs in this version, land it first by
[new-feature.md](new-feature.md) step 5 — rebase, `make ci-landing`, force-push, `git merge
--ff-only`, push, confirm its PR closed, remove the worktree — and then start at step 0 again. Otherwise say out loud which branches are being
left out, so "I thought that was in" is caught now rather than by a user.

### 1. Choose the number

Against the **user-visible surface**, not the size of the diff. When two rows could apply, take the
lower one.

| What changed | Bump |
|---|---|
| Docs, comments, tests, internal refactors; help text corrections | patch — `0.13.0` → `0.13.1` |
| A new command or flag, a new capability, a changed default | minor — `0.13.1` → `0.14.0` |
| A removed command or flag, or a moved frozen identifier ([canonical.md](../docs/canonical.md) §1) | major |

A pre-release carries a `-` (`0.14.0-rc1`) and the workflow marks the GitHub Release accordingly.

### 2. Bump it in all three places

```bash
new=0.13.2 ; old=$(cat VERSION)
printf '%s\n' "$new" > VERSION
sed -i '' "s|version-${old}-blue|version-${new}-blue|" README.md README.zh-CN.md
git diff --stat                         # exactly 3 files
```

`VERSION` reaches the binary, the `.app`'s `Info.plist` and the Release title on its own. The two
README badges do not derive from anything and are edited by hand. `TestReadmeBadgesMatchVERSION`
fails when they disagree, which is why step 4 runs the tests before the tag exists.

### 3. Write the changelog

**The changelog is the release commit message.** There is no `CHANGELOG.md`.

The published Release notes are assembled from three sources; only the first is yours:

| Part | From | Written by |
|---|---|---|
| What changed and whether it affects the reader | the `release: X.Y.Z` commit body | you |
| The commit list since the previous tag | `generate_release_notes: true` | GitHub |
| Per-platform install instructions | the `body:` block in `.github/workflows/release.yml` | already written; edit only when packaging changes |

Read the material first, then write for a user — GitHub prints the commit list underneath you, so
restating it is wasted:

```bash
git log --oneline "$(git describe --tags --abbrev=0)"..main
```

The message has to answer "does this version affect me?". Lead with the one thing a user would feel;
if nothing is observable, say so in a line — that is useful information, and "various fixes and
improvements" is not.

```
release: 0.13.1

Documentation, comments and three new build-rule guards. No behaviour change
beyond two help strings that were shipping internal milestone names to users.

The one item a user actually feels is in the skill the tap ships: it named
two streaming commands where there are four, so an agent following it parsed
NDJSON as a single object and failed on the first progress line.
```

### 4. Verify, then commit and push

```bash
make ci                                 # must be green BEFORE the tag exists
git add VERSION README.md README.zh-CN.md
git commit                              # message from step 3
git push origin main
```

### 5. Tag and push

The tag is the trigger and the claim about what is being released, so it must equal `VERSION`.

```bash
git tag -a "v${new}" -m "v${new}"
git push origin "v${new}"
```

### 6. Watch the workflow

```bash
gh run list --workflow=release.yml --limit 1
gh run watch <run-id> --exit-status
```

Six jobs, about four minutes:

| Job | Does |
|---|---|
| `verify` | checks the tag against `VERSION`, and the tap token if a tap is configured; gates the rest |
| `cli` | cross-compiles darwin/linux/windows tarballs |
| `gui-macos` | universal DMG |
| `gui-windows` | amd64 and arm64 zips |
| `publish` | creates the GitHub Release with every artifact attached |
| `homebrew` | pushes the formula and the skill to the tap, in one commit |

### 7. Check what shipped

A green workflow is a weaker claim than a correct release.

```bash
gh release view "v${new}" --json isDraft,isPrerelease -q '.isDraft, .isPrerelease'   # false false
gh release view "v${new}" --json assets -q '.assets[].name' | wc -l                  # 11
```

Eleven assets: four CLI tarballs (darwin/linux × amd64/arm64), the macOS DMG, a Windows amd64
tarball, two Windows zips, three `checksums-*.txt`.

Then the tap, a different repository that fails on its own:

```bash
gh api repos/dinstein/homebrew-agenthub/contents/Formula/agenthub.rb -q .content \
  | base64 -d | grep -E 'version|url' | head -4
```

The `version` must be the new one, and every `url` must point at **`dinstein/agent-hub`**, not at the
tap.

### When it goes wrong

| Symptom | Meaning | Do |
|---|---|---|
| `verify` fails on the tag | tag and `VERSION` disagree; nothing was built or published | `git tag -d` and `git push --delete`, fix `VERSION`, re-tag |
| A build job fails | usually toolchain, not code — `v0.13.0` failed on `wails3` needing `CGO_ENABLED=0` | fix on `main`, move the tag to the new commit, force-push it. No Release exists yet |
| `publish` green, `homebrew` red | the Release is complete; only the tap lagged | re-run that job. **Do not re-cut the release** |
| A wrong Release is already public | `gh release delete` leaves the tag behind | cut the next patch forward rather than deleting |
| GitHub Actions unavailable | — | `scripts/release-local.sh vX.Y.Z` — without `--push` it stops before the two irreversible steps and prints them |

Rehearse the whole pipeline without a tag by running the workflow with `workflow_dispatch`: every job
runs and `publish` skips itself.

---

## Not a release: run this checkout where Homebrew installed one

Publishes nothing — no tag, no Release, no tap commit. Use it to exercise a build with a real client
before releasing it.

```bash
make install-to-brew                     # release-channel build → $(brew --prefix)/bin/agenthub
scripts/install-to-brew.sh --restore     # give the path back to Homebrew
```

A dirty tree is fine here; the version carries `-dirty`. A dev-channel binary is refused. Replacing
the file does not replace a running daemon — `agenthub daemon restart` does.
