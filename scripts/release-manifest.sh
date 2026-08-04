#!/usr/bin/env bash
#
# Render the release manifest for a released version.
#
#   scripts/release-manifest.sh <release-tag> <checksums-cli.txt> > manifest.json
#
# WHY A MANIFEST EXISTS AT ALL. `brew` needs Xcode Command Line Tools, so on a
# machine without them the Homebrew path is not "slower", it is absent. The
# installer that covers those machines (scripts/install.sh) has to learn three
# things before it can download anything: which version is current, what the
# asset for this platform is called, and what it must hash to. The asset names
# carry version-and-hash (agenthub-0.27.2-f4cb04e-darwin-arm64.tar.gz), so
# GitHub's own `releases/latest/download/<name>` redirect — which only works
# for a name known in advance — cannot serve them. This file is the one asset
# whose name never changes, and it answers all three questions.
#
# NAMES ARE READ BACK, NEVER RECOMPOSED. Same rule as homebrew-formula.sh and
# homebrew-cask.sh, for the same reason: two places computing one asset name is
# how a download URL ends up pointing at a file nobody uploaded, and the report
# comes from someone else's machine weeks later.
#
# THE INSTALLER BUILDS NO ARTIFACT URLS. `base_url` below is the only place a
# download URL is assembled, which is why the installer needs to know just one
# thing about this project — where the manifest is — and nothing about how
# assets are laid out. The pair of them can then never disagree.
#
# WINDOWS IS ABSENT ON PURPOSE. The checksums file lists it and this manifest
# skips it: the installer is a POSIX shell script, so a windows entry would be
# a promise nothing can keep. It becomes an additive change to the schema on
# the day a PowerShell installer exists.

set -euo pipefail

if [ $# -ne 2 ]; then
	echo "usage: $0 <release-tag> <checksums-cli.txt>" >&2
	exit 2
fi

tag="$1"
sums="$2"
# The repo whose Releases hold the artifacts. The same variable and the same
# default as release-local.sh, homebrew-formula.sh and homebrew-cask.sh:
# releaserepo_test.go fails the moment two of those four disagree, because the
# disagreement is otherwise only ever discovered by someone installing.
#
# scripts/install.sh carries a fifth copy of the string under a DIFFERENT name,
# AGENTHUB_INSTALL_REPO — an installer whose whole point is avoiding Homebrew
# should not be configured through a variable named for it. Same value,
# deliberately not the same name, so neither the variable nor the check above
# reaches it: TestInstallerAgreesWithTheManifestItReads in test/buildrules is
# what compares those two defaults, and exporting HOMEBREW_SOURCE_REPO on a
# machine that is installing does nothing at all.
repo="${HOMEBREW_SOURCE_REPO:-dinstein/agent-hub}"

if [ ! -r "$sums" ]; then
	echo "$0: cannot read checksums file: $sums" >&2
	exit 1
fi

# The x.y.z half, without the leading v.
version="${tag#v}"

# sha_for / asset_for <os> <arch> — the checksum line for one platform.
#
# sub(): a checksums file written with a ./* glob carries a "./" prefix on
# every name, which lands in a URL as a stray "/./" and 404s. Stripped here as
# well as at the source, because this reads a file it did not write.
sha_for() {
	awk -v pat="-$1-$2.tar.gz" '$2 ~ pat {print $1}' "$sums" | head -1
}
asset_for() {
	awk -v pat="-$1-$2.tar.gz" '$2 ~ pat {sub(/^\.\//, "", $2); print $2}' "$sums" | head -1
}

targets="darwin/amd64 darwin/arm64 linux/amd64 linux/arm64"

# Every platform the installer offers must be present. A manifest silently
# missing one arch does not fail the release; it fails on the machine that has
# that arch, which is the only place the omission is visible.
missing=""
for target in $targets; do
	os="${target%/*}"
	arch="${target#*/}"
	[ -n "$(sha_for "$os" "$arch")" ] || missing="$missing $target"
done
if [ -n "$missing" ]; then
	echo "$0: no checksum for:$missing" >&2
	echo "$0: the release is incomplete; refusing to render a manifest that 404s" >&2
	exit 1
fi

# The build id, split back out of an asset name rather than taken from git:
# this script runs in a job that downloaded the artifacts and may not have the
# commit. The prefix is derived from the TAG, so an artifact built at a
# different version is caught here instead of being described as this one.
first="$(asset_for darwin arm64)"
prefix="agenthub-${version}-"
suffix="-darwin-arm64.tar.gz"
# Matched whole before anything is stripped: strip first and a name built at
# another version reaches the hash check below and is reported as a malformed
# hash rather than as the version disagreement it is.
case "$first" in
"$prefix"?*"$suffix") ;;
*)
	echo "$0: asset ${first} was not built at ${version}" >&2
	echo "$0: refusing to describe it as the ${tag} release" >&2
	exit 1
	;;
esac
commit="${first#"$prefix"}"
commit="${commit%"$suffix"}"
case "$commit" in
*[!0-9a-f]*)
	echo "$0: ${first} carries a build id that is not a commit hash (${commit})" >&2
	echo "$0: a -dirty or otherwise unidentifiable build must not be published" >&2
	exit 1
	;;
esac

# The JSON is hand-written, so every value that reaches it is constrained
# above: a version and a commit matched against patterns, asset names and
# hashes read out of a checksums file this project generated. None can carry a
# quote or a backslash, and buildrules parses the result with encoding/json on
# every run.
#
# `cli` is an ARRAY of records rather than an object keyed by "os/arch". The
# consumer is a POSIX shell script with no JSON parser: a flat record carrying
# its own os and arch can be found by scanning, while a nested key would have
# to be located by position.
printf '{\n'
printf '  "schema": 1,\n'
printf '  "version": "%s",\n' "$version"
printf '  "commit": "%s",\n' "$commit"
printf '  "channel": "release",\n'
printf '  "base_url": "https://github.com/%s/releases/download/%s",\n' "$repo" "$tag"
printf '  "cli": [\n'
sep=""
for target in $targets; do
	os="${target%/*}"
	arch="${target#*/}"
	# %b, not %s: the separator carries an escape that has to be interpreted.
	printf '%b' "$sep"
	printf '    {\n'
	printf '      "os": "%s",\n' "$os"
	printf '      "arch": "%s",\n' "$arch"
	printf '      "asset": "%s",\n' "$(asset_for "$os" "$arch")"
	printf '      "sha256": "%s"\n' "$(sha_for "$os" "$arch")"
	printf '    }'
	sep=",\n"
done
printf '\n  ]\n'
printf '}\n'
