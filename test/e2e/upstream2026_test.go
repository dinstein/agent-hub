package e2e_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// stdio2026_test.go covers agenthub as a CLIENT — negotiating 2026-07-28 with
// a downstream. This file covers the other face, agenthub as a SERVER, which
// had no end-to-end coverage at all: docs/mcp-2026-07-28.md §5 describes a
// whole second protocol generation on the upstream side, and every claim in
// it was proven only in-process.
//
// It is the half that faces real software. A downstream is something an
// operator chooses and can roll back; the upstream face is what every AI
// client speaks, and they will arrive at 2026-07-28 on their own schedule
// without asking. The e2e client also declared "2025-06-18" as a literal in
// one place, so of four supported versions exactly one was ever exercised
// through the real binary.
//
// The version strings here are written out rather than imported from
// internal/mcp, for the reason lazyMetaTools is: this suite is the outside,
// and these are the bytes an external client puts on the wire.

// meta2026 is the per-request _meta a 2026-07-28 client carries. Presence of
// this member is what flips a session stateless — there is no handshake to do
// it, which is the change 2026-07-28 made.
func meta2026() map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		"io.modelcontextprotocol/clientInfo": map[string]any{
			"name": "agenthub-e2e-2026", "version": "0",
		},
	}
}

// TestServerDiscoverAnswersTheUpstreamFace drives the 2026 handshake, which
// is a single request and no handshake state at all.
//
// It is sent before anything else — no initialize, no notifications — because
// that is how a 2026 client opens a session, and because "discover works once
// you have initialized" would describe a server no such client can talk to.
func TestServerDiscoverAnswersTheUpstreamFace(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "fake", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "fake")

	c := startGateway(t, dataDir, "e2e-discover")
	res, rpcErr := c.call("server/discover", map[string]any{"_meta": meta2026()}, 30*time.Second)
	if rpcErr != nil {
		c.fatalf("server/discover failed: %v", rpcErr)
	}
	var out struct {
		ResultType        string   `json:"resultType"`
		SupportedVersions []string `json:"supportedVersions"`
		TtlMs             *int64   `json:"ttlMs"`
		CacheScope        string   `json:"cacheScope"`
		Meta              struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"io.modelcontextprotocol/serverInfo"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		c.fatalf("discover result: %v\n%s", err, res)
	}

	// Every version this tree speaks, newest first. Exact, because the list
	// is a promise made to clients: a version silently dropped from it is a
	// client that stops being able to connect.
	want := []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}
	if !slices.Equal(out.SupportedVersions, want) {
		c.fatalf("supportedVersions = %v, want %v", out.SupportedVersions, want)
	}
	if out.ResultType != "complete" {
		c.fatalf("resultType = %q, want complete", out.ResultType)
	}
	// A DiscoverResult is a CacheableResult: these are required members of
	// the shape, not optional hints.
	if out.TtlMs == nil || out.CacheScope != "private" {
		c.fatalf("discover is not a CacheableResult: ttlMs=%v cacheScope=%q", out.TtlMs, out.CacheScope)
	}
	// The identity belongs in _meta on this face, not in a top-level member.
	if out.Meta.ServerInfo.Name != "agenthub" {
		c.fatalf("discover _meta carries no serverInfo: %s", res)
	}
	c.close()
}

// statelessList is a tools/list answer on the 2026 face: the catalog plus the
// members that face requires and the stateful one must never carry.
type statelessList struct {
	ResultType string `json:"resultType"`
	TtlMs      *int64 `json:"ttlMs"`
	CacheScope string `json:"cacheScope"`
	Tools      []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

// TestAPerRequestMetaFlipsTheSessionStateless is the 2026 session in full,
// and it never sends `initialize` or `notifications/initialized` — those are
// what the stateless protocol removed.
//
// A session that only worked after the handshake it abolished would be
// unusable by exactly the clients this face exists for, so "the catalog
// arrives without one" is the assertion, not a detail.
func TestAPerRequestMetaFlipsTheSessionStateless(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "fake", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "fake")

	c := startGateway(t, dataDir, "e2e-stateless")

	// tools/list carrying _meta, with no handshake before it. Polled because
	// the downstream connects in the background exactly as it does on the
	// stateful path — the session being stateless changes the protocol, not
	// the readiness of a catalog.
	var list statelessList
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, rpcErr := c.call("tools/list", map[string]any{"_meta": meta2026()}, 30*time.Second)
		if rpcErr != nil {
			c.fatalf("tools/list on a stateless session: %v", rpcErr)
		}
		list = statelessList{}
		if err := json.Unmarshal(res, &list); err != nil {
			c.fatalf("tools/list result: %v\n%s", err, res)
		}
		if len(list.Tools) > 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if len(list.Tools) == 0 {
		c.fatalf("the stateless session never saw a catalog")
	}
	// 2026-07-28 requires resultType and the freshness hints on a list result.
	if list.ResultType != "complete" {
		c.fatalf("tools/list resultType = %q, want complete", list.ResultType)
	}
	if list.TtlMs == nil || *list.TtlMs != 60_000 || list.CacheScope != "private" {
		c.fatalf("tools/list freshness hints = ttlMs:%v cacheScope:%q, want 60000 / private",
			list.TtlMs, list.CacheScope)
	}

	// And a call result carries resultType too — replyResult is the single
	// normalization point, so this is the same rule reaching a different
	// answer shape, one built from what a downstream returned.
	res := c.callTool2026(t, "fake__echo", map[string]any{"marker": "stateless"})
	var call struct {
		ResultType string `json:"resultType"`
	}
	if err := json.Unmarshal(res, &call); err != nil {
		c.fatalf("tools/call result: %v\n%s", err, res)
	}
	if call.ResultType != "complete" {
		c.fatalf("tools/call resultType = %q on a stateless session, want complete", call.ResultType)
	}
	if !strings.Contains(c.textContent(res), "stateless") {
		c.fatalf("the stateless call did not reach the downstream: %s", res)
	}
	c.close()
}

// callTool2026 invokes a tool with the per-request _meta, retrying the
// gateway's transient busy error the way callTool does.
func (c *gatewayClient) callTool2026(t *testing.T, name string, args map[string]any) json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, rpcErr := c.call("tools/call", map[string]any{
			"name": name, "arguments": args, "_meta": meta2026(),
		}, 30*time.Second)
		if rpcErr == nil {
			return res
		}
		if rpcErr.Code != codeRetryBusy || !time.Now().Before(deadline) {
			c.fatalf("tools/call %s failed: %v", name, rpcErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestAStatefulSessionNeverSeesResultType is the mirror, and the half that
// makes the case above mean something.
//
// resultType is what tells a 2026 client how to READ a result, and a stateful
// client never agreed to it. The gateway normalizes to the SESSION's
// generation rather than the downstream's, so the member must be absent here
// even though the very same code path stamps it above.
func TestAStatefulSessionNeverSeesResultType(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "fake", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "fake")

	c := startGateway(t, dataDir, "e2e-stateful")
	c.initialize()
	c.waitForTool("fake__echo", 30*time.Second)

	res, rpcErr := c.call("tools/list", map[string]any{}, 30*time.Second)
	if rpcErr != nil {
		c.fatalf("tools/list: %v", rpcErr)
	}
	// Decoded into a map rather than a struct: the assertion is that the KEY
	// is absent, and a struct field would read an absent member and a member
	// present-but-empty identically.
	var listRaw map[string]json.RawMessage
	if err := json.Unmarshal(res, &listRaw); err != nil {
		c.fatalf("tools/list result: %v\n%s", err, res)
	}
	for _, k := range []string{"resultType", "ttlMs", "cacheScope"} {
		if _, ok := listRaw[k]; ok {
			c.fatalf("a stateful tools/list carries %q, which its client never agreed to: %s", k, res)
		}
	}

	callRes := c.callTool("fake__echo", map[string]any{"marker": "stateful"}, 30*time.Second)
	var callRaw map[string]json.RawMessage
	if err := json.Unmarshal(callRes, &callRaw); err != nil {
		c.fatalf("tools/call result: %v\n%s", err, callRes)
	}
	if _, ok := callRaw["resultType"]; ok {
		c.fatalf("a stateful tools/call carries resultType: %s", callRes)
	}
	c.close()
}

// TestTheStatelessFaceRefusesAVersionItCannotServe pins the rule most likely
// to be got wrong by reading the supported list alone.
//
// 2025-11-25 IS a version this gateway serves — through `initialize`. It is
// not one it can serve STATELESSLY, so a request declaring it in _meta is
// refused rather than answered, because answering would promise per-request
// semantics the session does not have. The refusal's payload therefore lists
// 2026-07-28 ALONE, not `supportedVersions`: the client is being told what to
// retry with on this face, and handing it the full list would send it back
// with a version that fails the same way.
func TestTheStatelessFaceRefusesAVersionItCannotServe(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "fake", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "fake")

	c := startGateway(t, dataDir, "e2e-badversion")

	meta := meta2026()
	meta["io.modelcontextprotocol/protocolVersion"] = "2025-11-25"
	_, rpcErr := c.call("tools/list", map[string]any{"_meta": meta}, 30*time.Second)
	if rpcErr == nil {
		c.fatalf("a _meta declaring 2025-11-25 was ANSWERED; the stateless face must refuse it")
	}
	// -32022 UnsupportedProtocolVersion, the code the specification reserves.
	if rpcErr.Code != -32022 {
		c.fatalf("refusal code = %d, want -32022: %v", rpcErr.Code, rpcErr)
	}
	c.close()
}

// TestInitializeNegotiatesTheStatefulFamilyOnly walks every version this
// gateway advertises through the legacy handshake.
//
// Until now the suite declared one of them, as a literal, in one helper — so
// the echo rule was exercised at a single value and the interesting one was
// not it. 2026-07-28 is the case that matters: `initialize` is the handshake
// 2026 REMOVED, so a client declaring it here has contradicted itself, and
// echoing the claim back would promise per-request `_meta` semantics on a
// session that just used the handshake those semantics replaced.
func TestInitializeNegotiatesTheStatefulFamilyOnly(t *testing.T) {
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "fake", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "fake")

	for _, tc := range []struct{ asked, want string }{
		{"2025-11-25", "2025-11-25"},
		{"2025-06-18", "2025-06-18"},
		{"2025-03-26", "2025-03-26"},
		// Not echoed: answered with the gateway's stateful default.
		{"2026-07-28", "2025-11-25"},
		// Not a version at all: same rule, same answer.
		{"1999-01-01", "2025-11-25"},
	} {
		t.Run(tc.asked, func(t *testing.T) {
			c := startGateway(t, dataDir, "e2e-negotiate-"+tc.asked)
			if got := c.initializeAs(tc.asked); got != tc.want {
				c.fatalf("initialize declaring %q negotiated %q, want %q", tc.asked, got, tc.want)
			}
			// Whatever it negotiated, the session works: a version echo that
			// left the session unusable would be worse than refusing.
			c.waitForTool("fake__echo", 30*time.Second)
			c.close()
		})
	}
}
