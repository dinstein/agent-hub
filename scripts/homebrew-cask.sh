#!/usr/bin/env bash
#
# Render the Homebrew cask for a released macOS GUI.
#
#   scripts/homebrew-cask.sh <release-tag> <checksums-macos.txt> > agenthub-gui.rb
#
# A CASK, NOT A SECOND FORMULA. The artifact is a .app inside a DMG: mounting
# the image, moving the bundle into /Applications, quitting the running app on
# upgrade and trashing user data on `--zap` are all cask DSL, and a formula
# that tried to do them by hand would reimplement four things Homebrew already
# knows, each one wrongly on the first machine that is not this one.
#
# WHY IT DOES NOT LINK THE CLI. The .app carries its own copy of agenthub —
# build/Taskfile.common.yml's layout contract requires it, because
# api/dialorstart.go resolves the daemon as the sibling of its own executable.
# The obvious next step, a `binary` stanza putting that copy on $PATH, is the
# one thing this cask must not do: the agenthub formula already owns
# $(brew --prefix)/bin/agenthub, and two packages claiming one path is an
# install-time error on every machine that has both. Guarding it with
# `conflicts_with formula:` is not an option either — the Cask Cookbook
# documents only `conflicts_with cask:`.
#
# So the cask declares `depends_on formula:` instead. Installing the GUI
# installs the CLI, one package owns the $PATH entry, and both come out of the
# same tap commit and therefore the same release. The bundle's copy stays where
# it is and serves the GUI; it is simply not the one a terminal finds.
#
# QUARANTINE. See the generated cask's own header — the reasoning has to travel
# with the file a user can actually read, not stay here.
#
# The asset name carries version-and-hash (AgentHub-0.19.0-abc1234-macos-
# universal.dmg), so it is read out of the checksums file rather than
# reconstructed: two places computing one name is how a cask ends up pointing
# at a URL that does not exist. The hash half is then handed to the cask as the
# second field of a comma-separated version, which is Homebrew's own spelling
# for version-plus-build and what lets the url be interpolated from `version`
# rather than pasted in whole.

set -euo pipefail

if [ $# -ne 2 ]; then
	echo "usage: $0 <release-tag> <checksums-macos.txt>" >&2
	exit 2
fi

tag="$1"
sums="$2"
# The repo whose Releases hold the artifacts. Same variable, same default, as
# release-local.sh and homebrew-formula.sh: if the three disagree this one
# writes a URL into the cask that points where nobody uploaded, and the first
# report comes from someone else's `brew install`. buildrules holds the three
# defaults together.
repo="${HOMEBREW_SOURCE_REPO:-dinstein/agent-hub}"
# The tap, needed for the fully qualified `depends_on formula:`. A bare
# "agenthub" would be resolved against every tap on the user's machine and
# against homebrew-core, so the day a core formula takes that name the GUI
# starts pulling in someone else's binary. There is no default: a cask exists
# only inside a tap, and guessing which one is exactly the mistake above.
tap_repo="${HOMEBREW_TAP_REPO:-}"

if [ ! -r "$sums" ]; then
	echo "$0: cannot read checksums file: $sums" >&2
	exit 1
fi

if [ -z "$tap_repo" ]; then
	echo "$0: HOMEBREW_TAP_REPO is unset" >&2
	echo "$0: the cask names its tap in depends_on; refusing to render an unqualified one" >&2
	exit 1
fi
case "$tap_repo" in
*/homebrew-*) ;;
*)
	echo "$0: HOMEBREW_TAP_REPO must be owner/homebrew-<name> (got ${tap_repo})" >&2
	exit 2
	;;
esac
# owner/homebrew-agenthub -> owner/agenthub, the name `brew` itself uses. Same
# expression as release-local.sh's closing hint, and it must stay that way: the
# two print the tap a user is told to tap and the tap the cask depends on.
tap="${tap_repo%/*}/${tap_repo##*/homebrew-}"

# The x.y.z half, without the leading v.
version="${tag#v}"

# sub(): a checksums file written with a ./* glob carries a "./" prefix on
# every name, which lands in the URL as a stray "/./" and 404s. Stripped here
# as well as at the source, because this reads a file it did not write.
dmg="$(awk '$2 ~ /-macos-universal\.dmg$/ {sub(/^\.\//, "", $2); print $2}' "$sums" | head -1)"
sha="$(awk '$2 ~ /-macos-universal\.dmg$/ {print $1}' "$sums" | head -1)"

if [ -z "$dmg" ] || [ -z "$sha" ]; then
	echo "$0: no macOS DMG in ${sums}" >&2
	echo "$0: the release is incomplete; refusing to render a cask that 404s" >&2
	exit 1
fi

# Split the build hash back out. The prefix is derived from the TAG rather than
# pattern-matched, so a DMG built at a different version than the one being
# released is caught here instead of being published under the wrong name.
prefix="AgentHub-${version}-"
suffix="-macos-universal.dmg"
# Matched whole before anything is stripped. Stripping first and comparing
# afterwards cannot tell "the prefix did not match" from "the hash is odd": the
# suffix comes off either way, so a DMG built at another version reaches the
# check below and is reported as a malformed hash instead of as the version
# disagreement it is.
case "$dmg" in
"$prefix"?*"$suffix") ;;
*)
	echo "$0: ${dmg} is not named for ${tag}" >&2
	echo "$0: expected ${prefix}<hash>${suffix}; the DMG and the tag disagree about" >&2
	echo "$0: what is being released" >&2
	exit 1
	;;
esac
build="${dmg#"$prefix"}"
build="${build%"$suffix"}"
# A dirty tree stamps "<hash>-dirty" into the DMG name (Taskfile.yml's
# GIT_HASH). That artifact is not reproducible from any commit, so it must not
# become the thing a tap serves.
case "$build" in
*[!0-9a-f]*)
	echo "$0: build id ${build} is not a commit hash" >&2
	echo "$0: a -dirty or otherwise unidentifiable build must not reach the tap" >&2
	exit 1
	;;
esac

# NOTE ON ESCAPING: this heredoc is unquoted so the substitutions below expand,
# which makes a trailing backslash a line continuation. The one Ruby needs is
# therefore written \\ — dropping to a single backslash silently joins the two
# url lines into one and the cask stops parsing.
cat <<EOF
# Generated by scripts/homebrew-cask.sh from the ${tag} release. Do not edit by
# hand: the next release overwrites it, and a hand-edited sha256 that no longer
# matches its URL fails at install time on someone else's machine.
#
# QUARANTINE. The postflight below clears com.apple.quarantine from the bundle
# Homebrew just staged. This app is ad-hoc signed and NOT notarized, so with
# the flag left in place macOS refuses to launch it and the user is sent to
# System Settings to override Gatekeeper by hand — an install that has not
# installed.
#
# What that gives up, and what stands in for it: Gatekeeper answers "did these
# bytes come from a known party, unmodified". Here that answer comes from the
# sha256 below instead — pinned in a tap the user chose to add, fetched over
# HTTPS from a Release built by CI. It is a different chain of trust rather
# than an absent one, but it IS weaker than notarization, and it is why this
# cask can live in this tap and nowhere else.
#
# When Developer ID signing and notarization land, the postflight block is
# deleted and nothing else in this file changes.
cask "agenthub-gui" do
  version "${version},${build}"
  sha256 "${sha}"

  url "https://github.com/${repo}/releases/download/v#{version.csv.first}/" \\
      "AgentHub-#{version.csv.first}-#{version.csv.second}-macos-universal.dmg"
  name "AgentHub"
  desc "Local gateway between AI clients and MCP servers"
  homepage "https://github.com/${repo}"

  # The tap is regenerated by the release that publishes the DMG, so this
  # block buys no freshness. It is here because \`brew audit --online\` runs a
  # livecheck whether or not one is declared, and the default it infers reads
  # the release TAG — "0.19.0", which can never equal a version carrying a
  # build id. Left out, the audit fails on every release, and a check that is
  # always red is one nobody reads. Reading the asset names instead answers in
  # the same shape the version is written in.
  livecheck do
    url :url
    strategy :github_latest do |json|
      json["assets"]&.map do |asset|
        match = asset["name"]&.match(/^AgentHub-(\d+(?:\.\d+)+)-(\h+)-macos-universal\.dmg$/i)
        next if match.blank?

        "#{match[1]},#{match[2]}"
      end
    end
  end

  # A bare symbol is "this release or newer" — the upper bound has its own
  # stanza (maximum_macos), and \`brew style\` rewrites ">= :big_sur" to this.
  depends_on macos: :big_sur # = the bundle's LSMinimumSystemVersion 11.0
  # The CLI. Installing the GUI installs it; \$PATH's agenthub then has exactly
  # one owner, which the app bundle's private copy deliberately is not.
  depends_on formula: "${tap}/agenthub"

  app "AgentHub.app"

  postflight do
    # -c rather than -d com.apple.quarantine: -d fails on a file that does not
    # carry the attribute, and a recursive pass over a bundle meets plenty.
    system_command "/usr/bin/xattr", args: ["-c", "-r", "#{appdir}/AgentHub.app"]
  end

  # The app is not the only process it starts. A daemon launched from inside
  # the bundle would outlive an uninstall holding a deleted binary; stopping it
  # is safe because the next call starts one again (api.DialOrStart), from the
  # formula's copy this time.
  #
  # Run through /bin/sh, not the bundled binary directly: by the time someone
  # uninstalls, the .app may already have been dragged to the Trash by hand,
  # and an uninstall stanza that cannot find its executable is not a thing to
  # gamble a user's uninstall on. must_succeed: false covers the rest — there
  # may be no daemon running, and that is not a failed uninstall.
  uninstall quit:   "com.dinstein.agenthub",
            script: {
              executable:   "/bin/sh",
              args:         ["-c", "'#{appdir}/AgentHub.app/Contents/MacOS/agenthub' daemon stop >/dev/null 2>&1"],
              must_succeed: false,
            }

  # The release channel's directory only. A machine that also ran development
  # builds has an AgentHubDev beside it, which belongs to a checkout rather
  # than to this package — zapping it would delete someone's working state on
  # the strength of an uninstall they meant for the app.
  zap trash: "~/Library/Application Support/AgentHub"

  caveats <<~EOS
    The app is ad-hoc signed but not notarized, so this cask cleared the
    quarantine flag macOS puts on downloads; it is the pinned sha256 above,
    not Gatekeeper, that vouches for the bytes.

    Credentials kept in the macOS keychain survive \`brew uninstall --zap\`.
    Remove them from Keychain Access if you want them gone.
  EOS
end
EOF
