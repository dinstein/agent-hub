#!/usr/bin/env bash
#
# Build this checkout the way a release ships it, and put the result where
# Homebrew's agenthub lives, so the `agenthub` on $PATH is the code in front of
# you.
#
#   scripts/install-to-brew.sh            install this checkout over Homebrew's
#   scripts/install-to-brew.sh --restore  give the entry back to Homebrew
#
# THIS IS NOT A RELEASE. Nothing is tagged, uploaded or written to the tap; the
# only machine that sees the result is this one. Cutting an actual release is
# scripts/release-local.sh, and the names are deliberately unalike — the two
# differ in whether other people find out.
#
# WHY NOT JUST `make bin-release`. That produces bin/agenthub-release, which is
# not on $PATH, and pointing a client at it there has a cost that only shows up
# later: `agenthub client connect` writes the ABSOLUTE PATH of the running
# executable into the client's own config (executable() in internal/cli/cli.go),
# and features are developed in worktrees that get removed once they land
# (AGENTS.md). The client is then wired to a binary that no longer exists, in a
# file this repository never touches again. Installing at the Homebrew path
# instead means a real client exercises the new build with no config change at
# all — which is also the only way to test what a released copy will actually
# do.
#
# A DIRTY TREE IS ALLOWED HERE, unlike scripts/build-release-artifacts.sh, which
# refuses one. That script produces bytes other people download, and a release
# artifact whose version names a commit it was not built from is the worst kind
# of wrong. This one produces a binary for the person who ran it, and testing
# uncommitted work is the entire point. The Makefile appends `-dirty` to the
# version either way, so the binary still says what it is.

set -euo pipefail

usage() {
	cat <<'EOF'
usage: scripts/install-to-brew.sh [--restore]

  (no argument)  build this checkout as a release ships it and install it at
                 $(brew --prefix)/bin/agenthub, shadowing Homebrew's copy
  --restore      undo that: relink Homebrew's own agenthub

Nothing is published. See the header of this script for what it replaces.
EOF
}

mode=install
case "${1:-}" in
"") ;;
--restore) mode=restore ;;
-h | --help)
	usage
	exit 0
	;;
*)
	usage >&2
	exit 2
	;;
esac
if [ $# -gt 1 ]; then
	usage >&2
	exit 2
fi

here="$(cd "$(dirname "$0")/.." && pwd)"
cd "$here"

if ! command -v brew >/dev/null 2>&1; then
	echo "$0: brew not found on \$PATH" >&2
	echo "$0: there is no Homebrew location to install to; use 'make bin-release' instead" >&2
	exit 1
fi
prefix="$(brew --prefix)"
dest="${prefix}/bin/agenthub"

# Where `make bin-release` is told to write. Passed TO make rather than read
# back out of it: RELEASE_BIN is an overridable variable, and a script that
# hardcoded the default would install a stale binary the moment someone
# overrode it — silently, because the build it just ran still succeeded.
RELEASE_BIN="${RELEASE_BIN:-bin/agenthub-release}"

# occupant classifies whatever is at $dest, with no state kept anywhere:
#
#   brew     a symlink into the Cellar — Homebrew's own entry, restorable
#            with `brew link`
#   local    a regular file. Homebrew never puts one here, so this is a
#            previous run of this script (or a hand-placed download, which a
#            re-download or `brew install` restores just as well)
#   absent   nothing there
#   foreign  a symlink to somewhere that is not the Cellar. Something else
#            owns this path; refuse rather than guess at how to put it back
occupant() {
	if [ -L "$dest" ]; then
		local link resolved
		link="$(readlink "$dest")"
		case "$link" in
		/*) resolved="$link" ;;
		*) resolved="$(dirname "$dest")/$link" ;;
		esac
		case "$resolved" in
		*"/Cellar/agenthub/"*) echo brew ;;
		*) echo foreign ;;
		esac
	elif [ -e "$dest" ]; then
		echo local
	else
		echo absent
	fi
}

# version_of prints what a binary calls itself, or nothing if it will not run.
# Never allowed to fail the script: it is asked about the copy being REPLACED,
# which may be anything at all, including a file that is not executable.
version_of() {
	"$1" --version 2>/dev/null || true
}

state="$(occupant)"

if [ "$mode" = restore ]; then
	case "$state" in
	brew)
		echo "$dest is already Homebrew's own entry ($(version_of "$dest"))"
		exit 0
		;;
	foreign)
		echo "$0: $dest is a symlink to something that is not the Cellar" >&2
		echo "$0: this script did not put it there and will not remove it" >&2
		exit 1
		;;
	esac
	if brew list --formula --versions agenthub >/dev/null 2>&1; then
		# unlink BEFORE link, and this order is the whole of it. Homebrew
		# records separately whether a keg is linked, and replacing the file
		# underneath does not disturb that record — so `brew link` (with or
		# without --overwrite) answers "Already linked", changes nothing, and
		# exits 0. Paired with removing the local build, that leaves NOTHING
		# at $dest and a success message saying the opposite; it is what the
		# first version of this script did.
		#
		# `brew unlink` clears the record and removes the symlinks brew owns.
		# The regular file this script installed is not one of them, hence the
		# rm as well.
		brew unlink agenthub >/dev/null
		rm -f "$dest"
		brew link agenthub >/dev/null
		# Verified rather than announced. The failure above was silent in
		# exactly this spot, and a restore that reports success while the
		# command has vanished from $PATH is worse than one that fails.
		if [ ! -x "$dest" ]; then
			echo "$0: brew link left nothing runnable at $dest" >&2
			echo "$0: try: brew reinstall agenthub" >&2
			exit 1
		fi
		echo "restored Homebrew's agenthub: $(version_of "$dest")"
	elif [ "$state" = local ]; then
		rm -f "$dest"
		echo "removed the local build from $dest"
		echo "Homebrew has no agenthub keg to relink, so nothing is at that path now"
	else
		echo "nothing to restore: $dest does not exist"
	fi
	exit 0
fi

case "$state" in
foreign)
	echo "$0: $dest is a symlink to something that is not the Cellar" >&2
	echo "$0: refusing to replace it; remove it yourself if that is what you want" >&2
	exit 1
	;;
brew) echo "==> shadowing Homebrew's $(version_of "$dest")" ;;
local) echo "==> replacing a local build: $(version_of "$dest")" ;;
absent) echo "==> nothing installed at $dest yet" ;;
esac

echo
echo "==> building"
# Through make, so the two link-time flags that decide what this binary IS —
# main.channel and main.version — stay defined in exactly one place. A second
# copy of them here is how the channel silently reverts to dev.
make bin-release RELEASE_BIN="$RELEASE_BIN"

built="$("$RELEASE_BIN" --version)"

# The same assertion the Homebrew formula's `test do` block makes, run here
# because here is where it can still be acted on. A dev-channel binary at the
# installed path resolves the AgentHubDev directory while every client keeps
# invoking the same command name, so the servers configured through the release
# are simply not there — and nothing reports an error, because nothing is wrong
# as far as the binary is concerned.
case "$built" in
*"(dev)"*)
	echo "$0: the build says '${built}' — that is a DEV binary" >&2
	echo "$0: it would resolve the development data directory from the installed path" >&2
	echo "$0: refusing to install it; check CHANNEL in the Makefile" >&2
	exit 1
	;;
esac

echo
echo "==> installing to ${dest}"
if [ ! -w "$(dirname "$dest")" ]; then
	echo "$0: $(dirname "$dest") is not writable by $(id -un)" >&2
	echo "$0: fix the Homebrew prefix's ownership rather than running this under sudo" >&2
	exit 1
fi
# Written beside the destination and moved into place, not copied over it. A
# copy truncates the file a running process is executing — ETXTBSY on Linux,
# and on macOS a daemon that keeps running out of a file that no longer holds
# the code it was started from. The move replaces the directory entry instead,
# which leaves anything already running on the old inode, intact, until it exits.
tmp="${dest}.install-to-brew.$$"
trap 'rm -f "$tmp"' EXIT
install -m 0755 "$RELEASE_BIN" "$tmp"
mv -f "$tmp" "$dest"
trap - EXIT

echo
echo "installed: $(version_of "$dest")"
if keg="$(brew list --formula --versions agenthub 2>/dev/null)" && [ -n "$keg" ]; then
	# Said out loud because it is the one thing this arrangement gets wrong:
	# brew's bookkeeping still describes the keg, which is now not what runs.
	echo "brew still records '${keg}' — its keg is untouched, only the link is"
fi

# Replacing the file does not replace the process. A daemon started from the
# previous binary keeps serving every client until it is restarted, and the
# resulting picture — new CLI, old daemon — reads as a bug in the change being
# tested. Exit 4 is "daemon offline", which is the quiet case.
rc=0
"$dest" daemon status >/dev/null 2>&1 || rc=$?
if [ "$rc" -eq 0 ]; then
	echo
	echo "a daemon from the PREVIOUS binary is still running:"
	echo "  agenthub daemon restart"
fi

echo
echo "to give the entry back to Homebrew:"
echo "  scripts/install-to-brew.sh --restore"
