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
# scripts/build-release-artifacts.sh to produce the bytes and
# scripts/tap-sync.sh to deliver the tap's files, so both the artifacts and the
# tap commit are the same either way; what differs is only who runs it.
#
# WHAT IT NEEDS
#   gh, authenticated with contents:write on the source repo and on the tap
#   a clean working tree at the commit the tag names
#
# Environment:
#   HOMEBREW_TAP_REPO   owner/homebrew-<name>. Unset skips the tap step.
#   HOMEBREW_SOURCE_REPO  the repo whose Releases HOLD THE ARTIFACTS, which is
#       what the formula's download URLs point at. Defaults to dinstein/agent-hub:
#       it is public, so `brew install` fetches the tarball with no credentials,
#       and the artifacts then sit beside the source they were built from.
#       It defaulted to the TAP for as long as this repository was private, and
#       the assets already attached to v0.11.0 and v0.12.0 there stay where they
#       are — the formula each of those versions shipped pins their sha256, and
#       a same-named tarball built on another machine hashes differently. A URL
#       always keeps the hash from its own upload.

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

repo="${HOMEBREW_SOURCE_REPO:-dinstein/agent-hub}"
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

echo
echo "==> rendering the install manifest"
# The other install path's half of the same step. This one goes UP with the
# release rather than into the tap: scripts/install.sh reads it to learn what
# to download on a machine that has no Homebrew — which is most machines
# without Xcode Command Line Tools, since brew needs those itself. Rendered
# before the irreversible step for the reason above: a Release published
# without it can be installed by brew and by nothing else.
scripts/release-manifest.sh "$tag" dist/checksums-cli.txt > dist/manifest.json

if [ "$push" != "--push" ]; then
	echo
	echo "==> dry run: nothing was published"
	echo "would create release ${tag} on ${repo} with:"
	find dist -maxdepth 1 \( -name '*.tar.gz' -o -name 'checksums-cli.txt' -o -name 'manifest.json' \) -exec echo '  {}' \;
	if [ -n "$tap" ]; then
		echo "would push Formula/agenthub.rb and skills/agenthub/SKILL.md to ${tap}"
		# Said before the irreversible step rather than discovered after it.
		# This path builds no DMG, so it has no cask to render; the tap keeps
		# serving the GUI it already served.
		echo "would NOT touch Casks/agenthub-gui.rb: no DMG is built here, so the tap"
		echo "  would serve the previous GUI beside this release's CLI"
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
	dist/*.tar.gz dist/checksums-cli.txt dist/manifest.json

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
# Which files go to the tap is decided in exactly one place, shared with the
# release workflow — see the header of tap-sync.sh.
scripts/tap-sync.sh "$work/tap" "$tag" dist/agenthub.rb

echo
echo "done. verify from a clean machine:"
echo "  brew tap ${tap%/*}/${tap##*/homebrew-}"
echo "  brew install agenthub"
echo "  agenthub --version    # must NOT say (dev)"
