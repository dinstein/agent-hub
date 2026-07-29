package cli

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// parseHTTPFlags binds the flags onto a fresh command and parses argv through
// them — i.e. exactly what the forked child does with what args() produced.
func parseHTTPFlags(t *testing.T, argv []string) httpFlags {
	t.Helper()
	var f httpFlags
	cmd := &cobra.Command{Use: "start", RunE: func(*cobra.Command, []string) error { return nil }}
	f.bind(cmd)
	if err := cmd.Flags().Parse(argv); err != nil {
		t.Fatalf("the child could not parse %v: %v", argv, err)
	}
	return f
}

// TestHTTPFlagsSurviveTheBackgroundFork is the invariant `daemon start` rests
// on: the parent parses the operator's flags, then re-renders them into argv
// for a child that re-parses them. Anything args() fails to emit is a setting
// the operator asked for and the running daemon does not have.
//
// Both directions of that failure are bad and neither is visible. Losing
// --http-addr means the data plane the operator asked for never listens, while
// the parent reports the daemon started. Losing --http-allow-remote turns a
// deliberate non-loopback bind into a refusal at a layer they cannot see. The
// only reason a lost --insecure-loopback is survivable is that it fails
// closed — and relying on that is not the same as forwarding it.
func TestHTTPFlagsSurviveTheBackgroundFork(t *testing.T) {
	cases := []httpFlags{
		{},
		{addr: "127.0.0.1:8080"},
		{addr: "0.0.0.0:9000", allowRemote: true},
		{addr: "127.0.0.1:1", insecureLoopback: true},
		{addr: "[::1]:7777", allowRemote: true, insecureLoopback: true},
		{allowRemote: true},
		{insecureLoopback: true},
		{allowRemote: true, insecureLoopback: true},
	}
	for _, want := range cases {
		t.Run(want.addr+"/"+boolPair(want.allowRemote, want.insecureLoopback), func(t *testing.T) {
			got := parseHTTPFlags(t, want.args())
			if got != want {
				t.Fatalf("round trip lost settings: sent %+v, child parsed %+v (argv %v)",
					want, got, want.args())
			}
		})
	}
}

func boolPair(a, b bool) string {
	s := map[bool]string{true: "y", false: "n"}
	return s[a] + s[b]
}

// TestHTTPFlagsDefaultForksAPlainCommand: a plain `daemon start` must fork a
// plain `daemon start --foreground`. If args() emitted a default value the
// "no address, no listener" default would stop being a default — it would be
// something the parent asserts on every start, and a change to the default
// would silently not reach the daemon.
func TestHTTPFlagsDefaultForksAPlainCommand(t *testing.T) {
	if got := (httpFlags{}).args(); len(got) != 0 {
		t.Fatalf("the zero value rendered %v, want no arguments at all", got)
	}
}

// TestEveryHTTPFlagIsForwarded catches the failure this pair is actually prone
// to: a flag added to bind() and forgotten in args(). The parent would keep
// accepting it, the child would never see it, and nothing would fail — the
// daemon would just run with a setting the operator did not choose.
//
// Discovering the flag set from bind() rather than listing it here is the
// point: a new flag joins this assertion by existing.
func TestEveryHTTPFlagIsForwarded(t *testing.T) {
	var probe httpFlags
	cmd := &cobra.Command{Use: "start"}
	probe.bind(cmd)

	// Everything non-default, so args() has a reason to emit every flag.
	all := httpFlags{addr: "127.0.0.1:8080", allowRemote: true, insecureLoopback: true}
	emitted := map[string]bool{}
	for _, a := range all.args() {
		if len(a) > 2 && a[:2] == "--" {
			emitted[a[2:]] = true
		}
	}

	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if !emitted[f.Name] {
			t.Errorf("flag --%s is accepted by the parent but never forwarded to the forked daemon", f.Name)
		}
	})
}
