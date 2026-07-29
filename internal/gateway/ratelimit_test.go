package gateway

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/ratelimit"
	"github.com/dinstein/agent-hub/internal/registry"
	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

// seedRateLimits writes the quota rule set into governance.json — the GLOBAL
// layer, the only layer quotas live at (registry.GovernanceDoc.RateLimits
// explains why they are not a three-layer scope field).
func seedRateLimits(t *testing.T, resolver *platform.Resolver, rules ...registry.RateLimitRule) {
	t.Helper()
	docs := make([]registry.Doc[registry.RateLimitRule], 0, len(rules))
	for _, r := range rules {
		docs = append(docs, registry.Doc[registry.RateLimitRule]{V: r})
	}
	updateRegistry(t, externalRegistry(t, resolver), func(tx *registry.Tx) {
		tx.Governance.V.RateLimits = docs
	})
}

// callQuotaOutcome performs one tools/call and classifies it as granted or
// quota-denied. The transient "downstreams still connecting" error is
// retried: it is answered before the call ever reaches the pipeline, so it
// spends no token and must not be counted as either outcome.
func callQuotaOutcome(t *testing.T, c *testClient, tool string) (granted bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: tool, Arguments: []byte(`{}`)})
		switch {
		case resp.Error == nil:
			return true
		case resp.Error.Code == codeRetryBusy:
			time.Sleep(5 * time.Millisecond)
		case resp.Error.Code == ratelimit.JSONRPCCode:
			return false
		default:
			t.Fatalf("tools/call %s: unexpected error %+v", tool, resp.Error)
		}
	}
	t.Fatalf("tools/call %s never got past the busy error", tool)
	return false
}

// TestRateLimitRejectsCallsOverQuota is the acceptance test for the wiring:
// with a rule in governance.json, the calls past the limit are refused with
// an identifiable error carrying a retry hint, and the shared counter file
// exists on disk.
func TestRateLimitRejectsCallsOverQuota(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	resolver := testResolver(dataDir)
	seedRegistry(t, resolver, "fake")
	// A long window keeps the expectation exact: no meaningful refill can
	// happen while the test runs.
	seedRateLimits(t, resolver, registry.RateLimitRule{Server: "fake", Limit: 2, Window: "1h"})

	_, c, _ := startGateway(t, Config{
		ClientID: "quota-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")

	for i := 0; i < 2; i++ {
		if !callQuotaOutcome(t, c, "fake__echo") {
			t.Fatalf("call %d was rejected inside the limit of 2", i)
		}
	}

	resp := c.call(mcp.MethodToolsCall, mcp.CallToolParams{Name: "fake__echo", Arguments: []byte(`{}`)})
	if resp.Error == nil {
		t.Fatal("the third call must be rejected by the quota")
	}
	if resp.Error.Code != ratelimit.JSONRPCCode {
		t.Fatalf("rejection code = %d, want %d", resp.Error.Code, ratelimit.JSONRPCCode)
	}
	// The stable rejection code travels in the message, like every other
	// pipeline rejection, so log greps and clients classify it the same way.
	if !strings.Contains(resp.Error.Message, ratelimit.CodeRateLimited) {
		t.Fatalf("rejection message %q must carry %s", resp.Error.Message, ratelimit.CodeRateLimited)
	}
	var data struct {
		Rule         string `json:"rule"`
		RetryAfterMs int64  `json:"retryAfterMs"`
	}
	if err := json.Unmarshal(resp.Error.Data, &data); err != nil {
		t.Fatalf("decode rejection data %s: %v", resp.Error.Data, err)
	}
	if data.Rule != "*/fake/*" {
		t.Errorf("rule = %q, want the pattern id of the configured rule", data.Rule)
	}
	// A retry hint of 0 invites the immediate retry that caused the denial.
	if data.RetryAfterMs <= 0 {
		t.Errorf("retryAfterMs = %d, must be > 0", data.RetryAfterMs)
	}

	if _, err := os.Stat(filepath.Join(dataDir, "state", ratelimit.StateFileName)); err != nil {
		t.Fatalf("the shared counter file must exist once a quota is enforced: %v", err)
	}
}

// TestWithoutRateLimitsNothingChanges pins the zero-regression half: with no
// rule configured the gateway has no limiter at all, so the assembled
// CallRequest is untouched, nothing is counted, and no counter file is ever
// created.
func TestWithoutRateLimitsNothingChanges(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	resolver := testResolver(dataDir)
	seedRegistry(t, resolver, "fake")

	g, c, _ := startGateway(t, Config{
		ClientID: "unlimited-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")

	for i := 0; i < 5; i++ {
		if !callQuotaOutcome(t, c, "fake__echo") {
			t.Fatalf("call %d rejected although no quota is configured", i)
		}
	}
	if lim := g.limiter.Load(); lim != nil {
		t.Fatal("no rule configured must leave the limiter nil: a non-nil one would wrap every call")
	}
	if g.rlStore != nil {
		t.Fatal("no rule configured must not open the counter store")
	}
	for _, name := range []string{ratelimit.StateFileName, ratelimit.LockFileName} {
		if _, err := os.Stat(filepath.Join(dataDir, "state", name)); !os.IsNotExist(err) {
			t.Fatalf("%s exists (err %v): an unconfigured quota must touch nothing", name, err)
		}
	}
}

// TestRateLimitCountersAreSharedAcrossGateways proves the multi-process
// property survived the wiring: two gateway assemblies over the same data
// dir hold NO shared memory (separate Store, separate file descriptors) and
// must still add up to exactly the configured limit — the same merge the
// re-exec'd helper processes of internal/ratelimit's
// TestMultiProcessCountersMerge prove across real OS processes.
//
// The rule is scoped per RULE (one machine-wide bucket) and the two gateways
// use different client ids, so a per-process counter would grant 2*limit.
func TestRateLimitCountersAreSharedAcrossGateways(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	seedRateLimits(t, resolver, registry.RateLimitRule{
		Limit: 3, Window: "1h", Scope: "rule",
	})

	grants := func(clientID string, attempts int) int {
		t.Helper()
		_, c, _ := startGateway(t, Config{
			ClientID: clientID,
			Resolver: resolver,
			Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
		})
		c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
		waitForTools(t, c, "fake__echo")
		n := 0
		for i := 0; i < attempts; i++ {
			if callQuotaOutcome(t, c, "fake__echo") {
				n++
			}
		}
		return n
	}

	first := grants("gateway-a", 2)
	second := grants("gateway-b", 3)
	if first+second != 3 {
		t.Fatalf("gateways were granted %d+%d calls, want exactly 3 in total\n"+
			"more = the counter file was overwritten instead of merged; fewer = admissions were lost",
			first, second)
	}
}

// TestRateLimitHotReload: a quota added to governance.json while the gateway
// runs takes effect without a restart — a tightened limit must not wait for
// the client to reconnect.
func TestRateLimitHotReload(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")

	g, c, _ := startGateway(t, Config{
		ClientID: "reload-client",
		Resolver: resolver,
		Dial:     scriptedDial(map[string]*fakemcp.Script{"fake": fakemcp.Minimal("echo")}),
	})
	c.initialize(mcp.ProtocolVersion, mcp.ClientCapabilities{})
	waitForTools(t, c, "fake__echo")
	if !callQuotaOutcome(t, c, "fake__echo") {
		t.Fatal("no quota configured yet: the call must pass")
	}

	seedRateLimits(t, resolver, registry.RateLimitRule{Server: "fake", Limit: 1, Window: "1h"})
	waitFor(t, "the new rule set to be applied", func() bool { return g.limiter.Load() != nil })

	if !callQuotaOutcome(t, c, "fake__echo") {
		t.Fatal("the first call under the new rule must pass (a fresh bucket starts full)")
	}
	if callQuotaOutcome(t, c, "fake__echo") {
		t.Fatal("the second call must be rejected by the hot-reloaded quota")
	}
}

// TestUnusableRateLimitRulesRefuseToStart: a configuration that CLAIMS a
// quota must be honoured or reported. A rule set the process cannot
// interpret fails startup instead of degrading into silently unlimited.
func TestUnusableRateLimitRulesRefuseToStart(t *testing.T) {
	t.Parallel()
	resolver := testResolver(t.TempDir())
	seedRegistry(t, resolver, "fake")
	// A bare number is not a duration: 60 is ambiguous between seconds,
	// millis and nanos, which is a 1000x wrong quota.
	seedRateLimits(t, resolver, registry.RateLimitRule{Server: "fake", Limit: 5, Window: "60"})

	_, err := newGateway(Config{
		ClientID: "bad-quota-client",
		Resolver: resolver,
		In:       strings.NewReader(""),
		Out:      io.Discard,
	})
	if err == nil {
		t.Fatal("an uninterpretable rule set must abort startup, not start unlimited")
	}
	if !strings.Contains(err.Error(), "duration string") {
		t.Fatalf("startup error %q must name the unusable rule", err)
	}
}
