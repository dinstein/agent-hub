#!/bin/sh
#
# Install or update the agenthub CLI without Homebrew.
#
#   curl -fsSL https://raw.githubusercontent.com/dinstein/agent-hub/main/scripts/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --version v0.27.2
#   curl -fsSL .../install.sh | sh -s -- --prefix /usr/local
#   curl -fsSL .../install.sh | sh -s -- --uninstall
#
# WHY THIS EXISTS. `brew` needs Xcode Command Line Tools — it runs git — so on
# a Mac without them the tap is not the slower path, it is no path at all.
# Everything below therefore uses only what the base system ships: sh, curl (or
# wget), tar, awk, sed, and one of shasum / sha256sum / openssl. No git, no
# jq, no python, no brew. That constraint is the whole design; a convenience
# that reintroduces a dependency defeats the file.
#
# POSIX sh, not bash: /bin/sh is dash on most Linuxes, and this runs on
# whatever the machine already has.
#
# EVERYTHING IS A FUNCTION, and the last line calls one. The invocation at the
# top of this comment pipes this file into a shell, which executes what has
# ARRIVED — a connection that drops halfway through leaves a shell running the
# first half of an installer. With every action inside a function, the prefix
# of this file defines things and does nothing, and the one line that acts is
# also the last line to arrive.
#
# WHAT IT TRUSTS. The manifest is fetched over HTTPS from the project's own
# GitHub Release and pins a sha256 per artifact, which is verified before
# anything is unpacked or moved. That is the SAME chain of trust the Homebrew
# cask has and no stronger: the manifest ships beside the artifacts it
# describes, so it cannot vouch for them independently. Signing the artifacts
# is what would change that, and there is none yet — do not read the checksum
# below as more than what it is.
#
# What that trust does NOT extend to is the manifest's strings. Nothing checks
# the manifest itself, AGENTHUB_INSTALL_MANIFEST_URL exists so that a mirror
# can serve it, and the names inside it become a local path, a file name and
# the syntax of the receipt. Each is constrained where it enters, at the
# comment that says why.
#
# WHAT IT DOES NOT DO. It does not edit your shell configuration, it never
# calls sudo for you, and the installed binary never phones home: there is no
# update check anywhere in agenthub
# (docs/decisions/0006-no-telemetry-and-no-update-checker.md), which is
# exactly why updating is this script's job and re-running it is the way.

set -eu

# Defaults, and the only statements outside a function: assignments, whose
# effect is confined to a shell that is about to exit if this file is cut off.
repo="${AGENTHUB_INSTALL_REPO:-dinstein/agent-hub}"
prefix="${AGENTHUB_INSTALL_PREFIX:-$HOME/.local}"
# Test injection, and the escape hatch for a mirror. Whatever it names must
# serve a manifest.json rendered by scripts/release-manifest.sh, and the
# base_url inside it is what the artifacts are then fetched from.
manifest_url="${AGENTHUB_INSTALL_MANIFEST_URL:-}"
pin=""
mode="install"
purge=0
force=0

usage() {
	cat <<'EOF'
Install or update the agenthub CLI.

  --version <vX.Y.Z>  install this release instead of the latest
  --prefix <dir>      install into <dir>/bin (default: ~/.local)
  --uninstall         remove what a previous run of this script installed
  --purge             with --uninstall, also delete the data directory
  --force             install even when another package manager owns agenthub
  --help              this

Environment:
  AGENTHUB_INSTALL_PREFIX        same as --prefix
  AGENTHUB_INSTALL_REPO          owner/repo whose Releases hold the artifacts
  AGENTHUB_INSTALL_MANIFEST_URL  full URL of a manifest.json to install from
EOF
}

die() {
	echo "install.sh: $*" >&2
	exit 1
}

note() { echo "$*" >&2; }

# parse_args <args…> — the only thing the command line may change. A variable
# assigned in a POSIX shell function is global, which is what lets the parse be
# a function at all.
parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		--version)
			[ $# -ge 2 ] || die "--version needs a release tag, e.g. --version v0.27.2"
			pin="$2"
			shift 2
			;;
		--prefix)
			[ $# -ge 2 ] || die "--prefix needs a directory"
			prefix="$2"
			shift 2
			;;
		--uninstall)
			mode="uninstall"
			shift
			;;
		--purge)
			purge=1
			shift
			;;
		--force)
			force=1
			shift
			;;
		--help | -h)
			usage
			exit 0
			;;
		*)
			usage >&2
			die "unknown option: $1"
			;;
		esac
	done
}

# ---------------------------------------------------------------- environment

have() { command -v "$1" >/dev/null 2>&1; }

# fetch <url> <dest> — curl if present, wget otherwise. Both fail the script on
# an HTTP error rather than writing the error page to disk: curl needs -f for
# that, wget does it by default (--content-on-error, off unless asked for).
#
# A URL that arrived as https stays https through every redirect. The manifest
# is the one download nothing verifies afterwards — everything else is checked
# against it — so a redirect that leaves TLS is a manifest written by whoever
# is on the path. The pin FOLLOWS THE SCHEME rather than being unconditional:
# AGENTHUB_INSTALL_MANIFEST_URL may legitimately name a plain-http mirror, and
# that is a decision made off this machine, about a network this script cannot
# see.
#
# curl only, deliberately. `wget` on Alpine is busybox's, which accepts almost
# none of GNU wget's long options — --https-only there is not a stricter
# install, it is no install at all.
fetch() {
	if have curl; then
		case "$1" in
		https://*) curl -fsSL --proto '=https' --proto-redir '=https' "$1" -o "$2" ;;
		*) curl -fsSL "$1" -o "$2" ;;
		esac
	elif have wget; then
		wget -q -O "$2" "$1"
	else
		die "neither curl nor wget is available"
	fi
}

# sha256_of <file> — empty when this machine cannot hash at all, which is
# treated as a hard failure by the one caller. There is deliberately no
# --skip-verify: an installer that will run unverified when the check is
# inconvenient provides no verification at all.
sha256_of() {
	if have shasum; then
		shasum -a 256 "$1" | cut -d' ' -f1
	elif have sha256sum; then
		sha256sum "$1" | cut -d' ' -f1
	elif have openssl; then
		openssl dgst -sha256 "$1" | awk '{print $NF}'
	else
		echo ""
	fi
}

detect_platform() {
	case "$(uname -s)" in
	Darwin) os="darwin" ;;
	Linux) os="linux" ;;
	*) die "unsupported operating system: $(uname -s). Windows has a zip on the Releases page." ;;
	esac
	case "$(uname -m)" in
	arm64 | aarch64) arch="arm64" ;;
	x86_64 | amd64) arch="amd64" ;;
	*) die "unsupported architecture: $(uname -m)" ;;
	esac
	# Under Rosetta every uname in the process tree reports x86_64, including
	# this shell's. Installing what it says would put a translated binary on an
	# Apple Silicon machine and keep it there through every future update,
	# because the next run asks the same question the same way.
	if [ "$os" = "darwin" ] && [ "$arch" = "amd64" ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = "1" ]; then
		arch="arm64"
	fi
}

# data_dir — where a release-channel build keeps its state, for --purge only.
# Mirrors internal/platform; AGENTHUB_DATA_DIR wins there and so it wins here.
data_dir() {
	if [ -n "${AGENTHUB_DATA_DIR:-}" ]; then
		echo "$AGENTHUB_DATA_DIR"
	elif [ "$(uname -s)" = "Darwin" ]; then
		echo "$HOME/Library/Application Support/AgentHub"
	else
		echo "${XDG_DATA_HOME:-$HOME/.local/share}/AgentHub"
	fi
}

# ------------------------------------------------------------------- manifest

# top_field <file> <key> — a top-level string in the manifest.
top_field() {
	sed -n 's/.*"'"$2"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$1" | head -1
}

# cli_field <file> <os> <arch> <key> — a field of the record for one platform.
#
# The manifest's `cli` is an array of flat records, each carrying its own os
# and arch, precisely so this can be a scan rather than a path expression: a
# shell has no JSON parser, and locating a nested key by position is how a
# reformatted file silently starts returning the wrong platform's hash.
cli_field() {
	awk -v os="$2" -v arch="$3" -v field="$4" '
		/^[[:space:]]*\{[[:space:]]*$/ { buf = ""; next }
		/^[[:space:]]*\}/ {
			if (buf ~ "\"os\":[ ]*\"" os "\"" && buf ~ "\"arch\":[ ]*\"" arch "\"") {
				if (match(buf, "\"" field "\":[ ]*\"[^\"]*\"")) {
					v = substr(buf, RSTART, RLENGTH)
					sub("^\"" field "\":[ ]*\"", "", v)
					sub("\"$", "", v)
					print v
					exit
				}
			}
			buf = ""
			next
		}
		{ buf = buf $0 }
	' "$1"
}

# ------------------------------------------------------------------ uninstall

do_uninstall() {
	# The receipt names what THIS script installed. Without one, the default
	# prefix is a guess, and removing a binary on a guess is how an installer
	# deletes a copy somebody else's package manager owns.
	target="$bindir/agenthub"
	if [ -r "$receipt" ]; then
		recorded="$(top_field "$receipt" bin)"
		# What comes back out of here is EXECUTED and then removed, out of a
		# plain file that an older copy of this script — or anything else —
		# may have written. One check that the path could have been an install
		# is the difference between removing this script's own install and
		# removing whatever the file happens to say.
		case "$recorded" in
		"") ;;
		/*/agenthub) target="$recorded" ;;
		*)
			die "the receipt at $receipt names \"$recorded\", which is not a path this script could have installed; remove it by hand"
			;;
		esac
	elif [ ! -x "$target" ]; then
		die "no install receipt at $receipt and nothing at $target; if agenthub came from Homebrew, use \`brew uninstall agenthub\`"
	fi

	if [ -x "$target" ]; then
		# Before the binary goes: a daemon started from it keeps running with
		# a deleted executable, and the next client call would start another.
		"$target" daemon stop >/dev/null 2>&1 || true
		rm -f "$target"
		note "removed $target"
	fi
	rm -f "$receipt"

	if [ "$purge" = "1" ]; then
		dir="$(data_dir)"
		# This is the only `rm -rf` in the file, and AGENTHUB_DATA_DIR reaches
		# it as the argument. It is the user's own variable, so the directory
		# it names is not second-guessed — but a relative path, `/`, or a bare
		# $HOME is a typo rather than an intention, and it is not one the exit
		# status can be traded back for.
		case "$dir" in
		"" | "/" | "$HOME" | "$HOME/")
			die "refusing to delete $dir; AGENTHUB_DATA_DIR names a directory agenthub created, not this"
			;;
		[!/]*)
			die "refusing to delete a relative path ($dir); AGENTHUB_DATA_DIR must be absolute"
			;;
		esac
		if [ -d "$dir" ]; then
			rm -rf "$dir"
			note "removed $dir"
		fi
		note "credentials in the OS keyring are NOT removed by this; delete them there if you want them gone"
	else
		note "kept $(data_dir) — pass --purge to delete it too"
	fi
}

# -------------------------------------------------------------------- install

do_install() {
	detect_platform

	if [ -z "$manifest_url" ]; then
		if [ -n "$pin" ]; then
			manifest_url="https://github.com/$repo/releases/download/$pin/manifest.json"
		else
			# `latest/download/<name>` resolves only for a name known in
			# advance, which is the entire reason the manifest exists: the
			# artifacts it describes carry a version and a build id in their
			# names.
			#
			# It also skips pre-releases, so an -rc has to be asked for by tag.
			manifest_url="https://github.com/$repo/releases/latest/download/manifest.json"
		fi
	fi
	# Everything below is checked against the manifest, and nothing checks the
	# manifest. Said out loud where it can still change someone's mind, rather
	# than refused: the only way to get here is to have set the variable.
	case "$manifest_url" in
	https://*) ;;
	*) note "warning: the manifest is not being fetched over https; nothing vouches for what it says" ;;
	esac

	tmp="$(mktemp -d "${TMPDIR:-/tmp}/agenthub-install.XXXXXX")"
	cleanup() { rm -rf "$tmp"; }
	trap cleanup EXIT INT TERM

	note "==> fetching $manifest_url"
	fetch "$manifest_url" "$tmp/manifest.json" ||
		die "cannot fetch the release manifest. If this release predates it, install with Homebrew or download a tarball from https://github.com/$repo/releases"

	schema="$(sed -n 's/.*"schema"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$tmp/manifest.json" | head -1)"
	[ "$schema" = "1" ] || die "this release's manifest is schema ${schema:-unknown}, which this installer does not understand; fetch a newer install.sh"

	version="$(top_field "$tmp/manifest.json" version)"
	commit="$(top_field "$tmp/manifest.json" commit)"
	channel="$(top_field "$tmp/manifest.json" channel)"
	base_url="$(top_field "$tmp/manifest.json" base_url)"
	[ -n "$version" ] && [ -n "$commit" ] && [ -n "$base_url" ] ||
		die "the manifest is missing version, commit or base_url"
	# Both are pasted into the hand-written JSON receipt at the end of this
	# file, and there is no encoder here to escape them on the way. top_field
	# captures [^"]*, so a quote cannot reach it — but a trailing BACKSLASH
	# can, and it escapes the quote that was meant to close the string. The
	# receipt is then unparseable, and it is the only record of what was
	# installed and where. Constrained where the strings enter rather than
	# where they are written: version and commit are read in four other places
	# in between.
	case "$version$commit" in
	*[!A-Za-z0-9.+_-]*)
		die "the manifest's version or commit carries something that is not part of one"
		;;
	esac
	# A build whose channel is not `release` resolves the DEVELOPMENT data
	# directory, and the symptom is every server appearing to vanish. It is a
	# link-time flag, so nothing downstream of here could correct it.
	[ "$channel" = "release" ] || die "the manifest describes a ${channel:-unknown} build, not a release"

	asset="$(cli_field "$tmp/manifest.json" "$os" "$arch" asset)"
	want_sha="$(cli_field "$tmp/manifest.json" "$os" "$arch" sha256)"
	[ -n "$asset" ] && [ -n "$want_sha" ] ||
		die "release $version has no $os/$arch build"
	# The asset is the one manifest string that becomes a LOCAL PATH, and it
	# does so at the download below — which happens BEFORE the checksum,
	# because it is what the checksum is computed over. A `../` in it therefore
	# writes bytes nothing has verified yet to a path the user never named, and
	# the refusal that follows still prints "nothing was installed". The
	# manifest arrives over HTTPS from the project's own Release, but
	# AGENTHUB_INSTALL_MANIFEST_URL exists precisely so it does not have to.
	#
	# An allow list of characters, not a search for `..`: this must also hold
	# for whatever a manifest names next year, and the two answer that arrival
	# in opposite directions.
	case "$asset" in
	*[!A-Za-z0-9._-]* | .*)
		die "the manifest names \"$asset\" for $os/$arch, which is not a plain file name"
		;;
	esac

	# Refuse to fight another package manager for one path. Two owners of
	# $bindir/agenthub is a state neither can detect afterwards, and
	# `brew upgrade` silently reinstating an older binary over this one is the
	# good outcome.
	existing="$(command -v agenthub 2>/dev/null || true)"
	if [ -n "$existing" ] && [ "$existing" != "$bindir/agenthub" ] && [ "$force" != "1" ]; then
		# One hop: a Homebrew shim in bin/ is a symlink into the Cellar, and
		# `readlink -f` is not portable (macOS's readlink has no -f).
		resolved="$existing"
		[ -L "$existing" ] && resolved="$(readlink "$existing")"
		case "$resolved" in
		*/Cellar/agenthub/* | */Homebrew/*)
			die "$existing is installed by Homebrew. Run \`brew uninstall agenthub\` first, or pass --force to install alongside it (both will then claim the name)"
			;;
		esac
	fi

	note "==> agenthub $version ($commit) for $os/$arch"
	fetch "$base_url/$asset" "$tmp/$asset" || die "cannot download $base_url/$asset"

	got_sha="$(sha256_of "$tmp/$asset")"
	[ -n "$got_sha" ] || die "no shasum, sha256sum or openssl on this machine; refusing to install an unverified download"
	if [ "$got_sha" != "$want_sha" ]; then
		die "checksum mismatch for $asset
  expected $want_sha
  got      $got_sha
nothing was installed"
	fi

	tar -xzf "$tmp/$asset" -C "$tmp" || die "cannot unpack $asset"
	# -L first, and it is not redundant: [ -f ] FOLLOWS a symlink, so an
	# archive holding nothing but a link named agenthub passes that test.
	# Everything after it then acts on the link's target instead — chmod
	# rewrites the mode of a file this script never named, and the copy
	# installs that file's contents under the name agenthub. A checksum cannot
	# see any of it: those are the pinned bytes.
	[ ! -L "$tmp/agenthub" ] || die "$asset holds a symlink named agenthub, not a binary"
	[ -f "$tmp/agenthub" ] || die "$asset does not contain an agenthub binary"
	chmod 0755 "$tmp/agenthub"

	# What the artifact CLAIMS is what the manifest said; this is the artifact
	# answering for itself. It catches the two failures a checksum cannot: an
	# archive built for another platform (which fails to exec here rather than
	# on first use), and a binary linked without -X main.channel=release, whose
	# only other symptom is a user's data appearing to disappear.
	got_version="$("$tmp/agenthub" --version 2>/dev/null || true)"
	case "$got_version" in
	*"$version-$commit"*) ;;
	*) die "the downloaded binary reports \"${got_version:-nothing}\", not $version-$commit" ;;
	esac
	case "$got_version" in
	*"(dev)"*) die "the downloaded binary is a dev build; it would use the development data directory" ;;
	esac

	mkdir -p "$bindir" || die "cannot create $bindir"
	[ -w "$bindir" ] || die "$bindir is not writable. Choose another --prefix, or re-run under a shell that can write it (this script never calls sudo for you)."

	# A daemon holds the old binary open and outlives the file; stopping it
	# first means the replacement takes effect on the next call rather than
	# whenever the user next reboots. Nothing restarts it: api.DialOrStart
	# starts one on demand.
	if [ -x "$bindir/agenthub" ]; then
		"$bindir/agenthub" daemon stop >/dev/null 2>&1 || true
	fi

	# Staged inside the target directory, not in $tmp: a rename is only atomic
	# within one filesystem, and /tmp is frequently not the same one. Copying
	# straight over the destination is the thing to avoid — it truncates a
	# binary another process may be executing.
	staged="$bindir/.agenthub.install.$$"
	cp "$tmp/agenthub" "$staged" || die "cannot write to $bindir"
	chmod 0755 "$staged"
	mv -f "$staged" "$bindir/agenthub" || {
		rm -f "$staged"
		die "cannot replace $bindir/agenthub"
	}

	mkdir -p "$(dirname "$receipt")"
	cat >"$receipt" <<EOF
{
  "method": "script",
  "version": "$version",
  "commit": "$commit",
  "repo": "$repo",
  "manifest_url": "$manifest_url",
  "prefix": "$prefix",
  "bin": "$bindir/agenthub",
  "installed_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF

	note "installed $bindir/agenthub"

	# PATH is reported, never edited. Rewriting someone's shell configuration
	# is the one step here that cannot be undone by re-running with
	# --uninstall.
	case ":$PATH:" in
	*":$bindir:"*)
		found="$(command -v agenthub 2>/dev/null || true)"
		if [ -n "$found" ] && [ "$found" != "$bindir/agenthub" ]; then
			note ""
			note "note: \`agenthub\` still resolves to $found, which comes earlier in \$PATH"
		fi
		;;
	*)
		note ""
		note "$bindir is not on your PATH. Add it:"
		note "    echo 'export PATH=\"$bindir:\$PATH\"' >> ~/.zshrc   # or ~/.bashrc"
		;;
	esac

	note ""
	note "next: agenthub server add <id> --url <url>, then agenthub client connect claude-code"
	note "update: re-run this script. uninstall: re-run it with --uninstall"
}

main() {
	parse_args "$@"

	bindir="$prefix/bin"
	# The receipt follows the INSTALL, not the data directory. The data
	# directory belongs to the user's servers and credentials and outlives any
	# particular copy of the binary; a receipt kept there would be deleted by a
	# purge that was meant to keep the install, and would describe an install
	# for a channel it knows nothing about.
	receipt="$prefix/share/agenthub/install.json"

	if [ "$mode" = "uninstall" ]; then
		do_uninstall
		return
	fi
	do_install
}

# The only line in this file that does anything, and the last one to arrive
# down a pipe. See the note at the top: a truncated `curl … | sh` must define
# an installer and run none of it, rather than run the half it received.
main "$@"
