#!/usr/bin/env bash
#
# Cut a release from this machine: build, publish the GitHub Release, update
# the Homebrew tap.
#
#   scripts/release-local.sh <vX.Y.Z> [--push]
#
# Without --push it does everything except the two irreversible steps, and
# prints exactly what those would do. A release and a tap commit are both
# visible to other people the moment they happen, and a wrong one cannot be
# quietly taken back — `gh release delete` leaves the tag, and a tap commit is
# already in someone's `brew update` by the time you notice.
#
# WHY THIS EXISTS. The release workflow is the normal path and this is not a
# replacement for it. It is the way to ship while GitHub Actions cannot run —
# which is the situation this was written in. Both call
# scripts/build-release-artifacts.sh, so the bytes are the same either way;
# what differs is only who runs it.
#
# WHAT IT NEEDS
#   gh, authenticated with contents:write on the source repo and on the tap
#   a clean working tree at the commit the tag names
#
# Environment:
#   HOMEBREW_TAP_REPO   owner/homebrew-<name>. Unset skips the tap step.
#   HOMEBREW_SOURCE_REPO  the repo whose Releases HOLD THE ARTIFACTS, which is
#       what the formula's download URLs point at. Defaults to the tap, because
#       that is where they go: dinstein/agent-hub is private and has never held
#       a release, and `brew install` must be able to fetch the tarball without
#       credentials. It used to default to dinstein/agent-hub, so every release
#       had to remember to override it — and forgetting produced a formula whose
#       URLs 404 for everyone but the person who published it.

set -euo pipefail

tag="${1:-}"
push="${2:-}"

if [ -z "$tag" ]; then
	echo "usage: $0 <vX.Y.Z> [--push]" >&2
	exit 2
fi
case "$tag" in
v*) ;;
*)
	echo "$0: tag must start with v (got ${tag})" >&2
	exit 2
	;;
esac

repo="${HOMEBREW_SOURCE_REPO:-dinstein/homebrew-agenthub}"
tap="${HOMEBREW_TAP_REPO:-}"
version="${tag#v}"
here="$(cd "$(dirname "$0")/.." && pwd)"
cd "$here"

# The tag is a claim about what is being released; VERSION is the other half of
# that claim. A Release whose assets disagree with its title is discovered by a
# user months later and is close to impossible to diagnose from outside.
file_version="$(cat VERSION)"
if [ "$version" != "$file_version" ]; then
	echo "$0: ${tag} disagrees with VERSION (${file_version})" >&2
	echo "$0: bump VERSION and commit, or use v${file_version}" >&2
	exit 1
fi

# --repo, or this guard asks the wrong repository and can never fire: releases
# live in $repo, and the directory this runs in is the source tree, which holds
# none. Without it the check passed on a tag that already existed, and the run
# got as far as pushing the tag before `gh release create` refused — leaving a
# tag published for a release that does not exist.
if gh release view "$tag" --repo "$repo" >/dev/null 2>&1; then
	echo "$0: release ${tag} already exists on ${repo}" >&2
	echo "$0: releases are not overwritten; bump VERSION for the next one" >&2
	exit 1
fi

echo "==> building (clean-tree check is inside)"
rm -rf dist
scripts/build-release-artifacts.sh "$version"

echo
echo "==> rendering the formula"
# Rendered BEFORE publishing so a malformed one stops the release rather than
# leaving a Release out there with no way to install it.
scripts/homebrew-formula.sh "$tag" dist/checksums-cli.txt > dist/agenthub.rb
ruby -c dist/agenthub.rb >/dev/null && echo "formula syntax OK"

if [ "$push" != "--push" ]; then
	echo
	echo "==> dry run: nothing was published"
	echo "would create release ${tag} on ${repo} with:"
	find dist -maxdepth 1 \( -name '*.tar.gz' -o -name 'checksums-cli.txt' \) -exec echo '  {}' \;
	if [ -n "$tap" ]; then
		echo "would push Formula/agenthub.rb to ${tap}"
	else
		echo "HOMEBREW_TAP_REPO unset: the tap step would be skipped"
	fi
	echo
	echo "re-run with --push to publish"
	exit 0
fi

echo
echo "==> tagging and publishing"
git tag -a "$tag" -m "agenthub ${version}" 2>/dev/null || echo "tag ${tag} already exists locally"
git push origin "$tag"
gh release create "$tag" \
	--repo "$repo" \
	--title "agenthub ${version}" \
	--generate-notes \
	dist/*.tar.gz dist/checksums-cli.txt

if [ -z "$tap" ]; then
	echo
	echo "HOMEBREW_TAP_REPO unset: skipping the tap"
	echo "the release is published; the tap still serves whatever it served before"
	exit 0
fi

echo
echo "==> updating the tap ${tap}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
gh repo clone "$tap" "$work/tap" -- --depth 1
mkdir -p "$work/tap/Formula"
cp dist/agenthub.rb "$work/tap/Formula/agenthub.rb"
(
	cd "$work/tap"
	if git diff --quiet -- Formula/agenthub.rb; then
		echo "formula already matches ${tag}; nothing to push"
		exit 0
	fi
	git add Formula/agenthub.rb
	git commit -m "agenthub ${tag}"
	git push
)

echo
echo "done. verify from a clean machine:"
echo "  brew tap ${tap%/*}/${tap##*/homebrew-}"
echo "  brew install agenthub"
echo "  agenthub --version    # must NOT say (dev)"
