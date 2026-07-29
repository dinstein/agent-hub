package cli

import (
	"strings"
	"testing"
)

// TestForChannelDecidesEveryBuildFlavourConsequence pins the channel -> Options
// mapping in BOTH directions. The line this replaces lived in cmd/agenthub,
// a main package with no test binary, where setting it to a constant of
// either sense stayed green forever.
//
// The dev rows are not padding. A build that forgets to declare its channel
// must land on the recoverable side of all three consequences: its own data
// directory, a version string that says so, and the MORE visible help page.
func TestForChannelDecidesEveryBuildFlavourConsequence(t *testing.T) {
	for _, tc := range []struct {
		channel string
		release bool
	}{
		{"release", true},
		{"dev", false},
		{"", false},        // -ldflags omitted entirely
		{"Release", false}, // the comparison is exact, not case-folded
		{"release-candidate", false},
		{"prod", false}, // a plausible-looking value that is still not the one
	} {
		t.Run("channel="+tc.channel, func(t *testing.T) {
			opts := Options{Version: "1.2.3"}
			opts.ForChannel(tc.channel)

			if opts.ReducedHelp != tc.release {
				t.Errorf("ReducedHelp = %v, want %v", opts.ReducedHelp, tc.release)
			}
			// A release build resolves the real data directory (nil resolver =
			// platform.Default in Main); a dev build must be handed the
			// development one, or it writes to the installed copy's registry.
			if gotDev := opts.Resolver != nil; gotDev == tc.release {
				t.Errorf("dev resolver installed = %v, want %v", gotDev, !tc.release)
			}
			if gotSuffix := strings.HasSuffix(opts.Version, " (dev)"); gotSuffix == tc.release {
				t.Errorf("Version = %q, dev suffix = %v, want %v", opts.Version, gotSuffix, !tc.release)
			}
			if !strings.HasPrefix(opts.Version, "1.2.3") {
				t.Errorf("Version = %q, want it to keep the stamped version", opts.Version)
			}
		})
	}
}

// TestForChannelKeepsItsConsequencesTogether: the data directory, the version
// suffix and the help page are three answers to one question, "is this a
// shipped build". Deriving them from separate comparisons is how two of them
// end up disagreeing after an edit touches only one. Asserting they flip as a
// set is cheap; discovering the drift on a shipped binary is not.
func TestForChannelKeepsItsConsequencesTogether(t *testing.T) {
	release := Options{Version: "v"}
	release.ForChannel(ChannelRelease)
	dev := Options{Version: "v"}
	dev.ForChannel("dev")

	if release.ReducedHelp == dev.ReducedHelp ||
		(release.Resolver != nil) == (dev.Resolver != nil) ||
		release.Version == dev.Version {
		t.Errorf("release and dev Options must differ in all three channel-derived fields;\n"+
			"release: hide=%v devResolver=%v version=%q\ndev:     hide=%v devResolver=%v version=%q",
			release.ReducedHelp, release.Resolver != nil, release.Version,
			dev.ReducedHelp, dev.Resolver != nil, dev.Version)
	}
}

// TestForChannelTouchesNothingElse: ForChannel is the build-flavour decision
// and only that. A future consequence added to it must not quietly acquire the
// power to rewrite the caller's args, streams or lock timeout.
func TestForChannelTouchesNothingElse(t *testing.T) {
	stdin := strings.NewReader("")
	opts := Options{Version: "1.0", Args: []string{"server", "ls"}, Stdin: stdin, LockTimeout: 42}
	opts.ForChannel(ChannelRelease)

	if strings.Join(opts.Args, " ") != "server ls" {
		t.Errorf("Args = %v, want them untouched", opts.Args)
	}
	if opts.Stdin != stdin {
		t.Error("Stdin was replaced")
	}
	if opts.LockTimeout != 42 {
		t.Errorf("LockTimeout = %v, want it untouched", opts.LockTimeout)
	}
}
