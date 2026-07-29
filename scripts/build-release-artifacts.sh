#!/usr/bin/env bash
#
# Cross-compile the release CLI artifacts into dist/.
#
#   scripts/build-release-artifacts.sh <x.y.z>
#
# ONE implementation, called by both the release workflow and the local
# release path. The alternative — the workflow holding its own copy of this
# loop — means two things that must produce identical bytes are maintained
# separately, and the day they diverge the difference is invisible: both
# produce a plausible tarball, and only the ldflags inside differ.
#
# What must not drift, specifically:
#
#   main.channel=release   sends a shipped binary to the REAL data directory.
#                          The default is dev, which is right for a working
#                          tree and wrong for a download. A release artifact
#                          built without it silently resolves AgentHubDev, and
#                          the user reports that their servers vanished.
#   main.version           x.y.z plus the commit, so an installed copy can say
#                          which build it is.
#   CGO_ENABLED=0          one runner cross-compiles everything.

set -euo pipefail

if [ $# -ne 1 ]; then
	echo "usage: $0 <x.y.z>" >&2
	exit 2
fi

release_version="$1"

# Not `git describe`: with a tag present it returns "v0.1.0-7-gabc1234", which
# after the release version reads "0.2.0-v0.1.0-7-gabc1234". Only the hash is
# wanted here.
hash="$(git rev-parse --short=7 HEAD)"

# A release artifact must be reproducible from a commit. Building one from
# uncommitted work produces a binary whose version string names a commit that
# does not contain the code inside it — the worst kind of wrong, because it
# looks precise.
if ! git diff --quiet HEAD 2>/dev/null; then
	echo "$0: the working tree has uncommitted changes" >&2
	echo "$0: refusing to build a release artifact that claims to be ${hash}" >&2
	exit 1
fi

version="${release_version}-${hash}"

mkdir -p dist

# Windows is built because `GOOS=windows go build ./...` is already a CI gate,
# but it has never run on real hardware (CLAUDE.md). It ships as a
# convenience, not as a supported platform — and deliberately has no Homebrew
# formula entry.
for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64; do
	os="${target%/*}"
	arch="${target#*/}"
	ext=""
	[ "${os}" = "windows" ] && ext=".exe"
	out="dist/agenthub${ext}"

	GOOS="${os}" GOARCH="${arch}" CGO_ENABLED=0 go build \
		-ldflags "-X main.version=${version} -X main.channel=release" \
		-o "${out}" ./cmd/agenthub

	tar -czf "dist/agenthub-${version}-${os}-${arch}.tar.gz" -C dist "agenthub${ext}"
	rm -f "${out}"
done

# `--` rather than `./*`: both silence the leading-dash glob warning, but
# ./* writes "./name" into the file, and anything that reads a name back
# out of it then builds a URL with a stray "/./" in the middle.
(cd dist && shasum -a 256 -- *.tar.gz > checksums-cli.txt)

echo "built ${version}:"
ls -1 dist/*.tar.gz
