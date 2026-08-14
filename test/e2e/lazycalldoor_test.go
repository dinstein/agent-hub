package e2e_test

import (
	"strings"
	"testing"
	"time"
)

// In lazy mode a session is shown five meta-tools and no downstream names at
// all, so the profile narrowing it can look self-enforcing: what is not in
// the list cannot be typed. It can. `call_tool` takes the tool name as a
// STRING argument, and the routing prefix is the server id — a client that
// was never shown beta__echo can still ask for it by spelling it.
//
// That makes call_tool the single execute door of a lazy session, and the
// only thing behind it is Surface.ResolveCall resolving against the VISIBLE
// set. lazy_test.go drives that door forward — search, call, page a result —
// and never once drives it somewhere it should not go. Until this file the
// end-to-end suite could not tell a lazy session that enforces its profile
// from one that hands every enabled tool to anyone who guesses a name, and
// the second is what a broken resolve produces: search stays clean (a
// different code path over the same filtered set), so nothing else looks
// wrong.
//
// The refusal is a RESULT with isError, not a JSON-RPC error, because it is
// the meta-tool answering rather than the transport failing — hence
// errorText rather than callToolRefused.

// TestLazyCallToolCannotReachPastTheProfile drives the door at a name the
// profile withholds, then widens the profile and drives it at the same name
// again.
//
// The second half is not decoration. beta is enabled and connected either
// way, so a refusal alone is equally consistent with a downstream that never
// came up — and the version of this test without it would keep passing after
// beta stopped working entirely.
func TestLazyCallToolCannotReachPastTheProfile(t *testing.T) {
	dataDir := t.TempDir()
	twoServerProfile(t, dataDir, "narrow", "lazygateclient")
	// twoServerProfile pins the global mode to full (enableServer); the
	// profile's own override is what this session runs under, and it is
	// also the setting the client cannot see or change.
	runProfile(t, dataDir, "discovery", "narrow", "lazy")

	c := startGateway(t, dataDir, "lazygateclient")
	c.initialize()
	waitTools(t, c, 30*time.Second, "the five meta-tools", func(names []string) bool {
		return equalStrings(names, lazyMetaTools)
	})

	// The visible half works, which puts the catalog demonstrably live: from
	// here a refusal cannot be "still connecting" (call_tool answers that
	// with the retryable busy error, which callTool waits out).
	if got := c.textContent(c.callTool("call_tool", map[string]any{
		"tool":      "alpha__echo",
		"arguments": map[string]any{"marker": "visible-half"},
	}, 30*time.Second)); !strings.Contains(got, "visible-half") {
		c.fatalf("call_tool did not reach the visible alpha__echo: %q", got)
	}

	// search_tools must not name the hidden server either: it reads the same
	// filtered set, and a leak there would hand the agent the string it
	// needs without it having to guess.
	for _, hit := range c.searchHits(c.callTool("search_tools",
		map[string]any{"query": "echo"}, 30*time.Second)) {
		if hit == "beta__echo" {
			c.fatalf("search_tools named the hidden beta__echo")
		}
	}

	// The door, at the name it must not open.
	hidden := c.errorText(c.callTool("call_tool", map[string]any{
		"tool":      "beta__echo",
		"arguments": map[string]any{"marker": "should-not-run"},
	}, 30*time.Second))

	// And the anti-probing rule (docs/model.md#how-the-surface-is-presented: "every tool id that
	// can't be shown — nonexistent, out of scope, or outside its server's
	// allow list — returns the same copy, or describe_tool becomes an
	// enumeration oracle"). A hidden REAL tool and a name that exists
	// nowhere must be indistinguishable, or call_tool becomes the oracle
	// instead: an agent could map the whole installation one guess at a time
	// without ever calling anything.
	absent := c.errorText(c.callTool("call_tool", map[string]any{
		"tool":      "beta__no_such_tool",
		"arguments": map[string]any{},
	}, 30*time.Second))
	if a, b := blankTool(hidden, "beta__echo"), blankTool(absent, "beta__no_such_tool"); a != b {
		c.fatalf("a hidden tool and an absent one are distinguishable:\n hidden: %q\n absent: %q", a, b)
	}

	// Widen, and the door opens on the same name. This is what proves the
	// refusal above was the scope and not beta.
	runProfile(t, dataDir, "server", "add", "narrow", "beta")
	waitLazyCall(t, c, "beta__echo reachable once the profile allows it", func() bool {
		res := c.callTool("call_tool", map[string]any{
			"tool":      "beta__echo",
			"arguments": map[string]any{"marker": "now-allowed"},
		}, 30*time.Second)
		return strings.Contains(string(res), "now-allowed")
	})
	c.close()
}

// blankTool replaces the tool name inside a refusal with a fixed token, so
// two refusals about different names can be compared for everything else.
// Comparing the raw strings would always differ; comparing only a prefix
// would pass on a message that appended the reason.
func blankTool(msg, name string) string {
	return strings.ReplaceAll(msg, name, "<tool>")
}

// waitLazyCall polls a call_tool attempt until it succeeds. A profile edit
// reaches a running gateway through the registry watch, and in lazy mode
// there is no tools/list change to wait on — the meta-tool surface is
// identical before and after, which is the whole point of the mode. The
// only observable is the door itself.
func waitLazyCall(t *testing.T, c *gatewayClient, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	c.fatalf("call_tool never satisfied %q within 30s", what)
}
