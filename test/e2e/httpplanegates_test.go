package e2e_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The HTTP face is the one place in the product where a credential exists, so
// it is also the only place where the second line of defence — the agent
// token's operation tier — can fire at all (docs/architecture.md#what-a-call-passes-through; the tier
// gate returns nil outright for the empty tier a stdio session carries).
//
// This file is the fail-closed half of httpplane_test.go: the bind that must
// not happen, the call the tier refuses, the servers the token cannot reach,
// and the revocation a live session must not survive. Each of the four is
// stated as an invariant in internal/httpbridge's package documentation and
// none of them can be observed from inside a single process, because what
// they are about is one process deciding on a file another process wrote.

// annotatedTool is one fixture tool and the raw MCP annotations object that
// decides its tier.
type annotatedTool struct {
	name        string
	annotations string
}

// writeAnnotatedScript writes a fakemcp script whose tools carry annotations.
// The tier a tool lands in is derived from these and nothing else, so this is
// the only way to build a downstream a read-only credential may legitimately
// call.
func writeAnnotatedScript(t *testing.T, path string, tools ...annotatedTool) {
	t.Helper()
	type toolDef struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
		Annotations json.RawMessage `json:"annotations,omitempty"`
	}
	type tool struct {
		Def toolDef `json:"def"`
	}
	out := make([]tool, 0, len(tools))
	for _, spec := range tools {
		def := toolDef{
			Name:        spec.name,
			Description: "echoes its arguments back as text",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}
		if spec.annotations != "" {
			def.Annotations = json.RawMessage(spec.annotations)
		}
		out = append(out, tool{Def: def})
	}
	data, err := json.Marshal(map[string]any{"tools": out})
	if err != nil {
		t.Fatalf("marshal fakemcp script: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fakemcp script: %v", err)
	}
}

// TestHTTPPlaneRefusesToBindWithoutACredential pins the rule that binding the
// listener is ITSELF an authorization decision (httpbridge.AuthorizeBind): with
// no admin token, no active agent token and no registered client there is
// nobody who could legitimately connect, so the bind is refused.
//
// The direction matters more than the message. Binding anyway and rejecting
// each request would look equally safe and would not be: an open port answers
// scans, ages into a forgotten listener, and makes "since when has this been
// open" a question nobody can answer. Refusing is also the "delivered or
// refused" rule — an operator who typed --http-addr and got a daemon without a
// data plane would have to notice the absence to learn about it.
func TestHTTPPlaneRefusesToBindWithoutACredential(t *testing.T) {
	dataDir, _, env := sandbox(t)
	addr := freeLoopbackAddr(t)

	code, out := runAgenthubExitEnv(t, env, "", "daemon", "start", "--headless", "--http-addr", addr)
	if code == 0 {
		t.Fatalf("a data plane with no credential started anyway: %s", out)
	}
	// The refusal has to name a way forward: an operator reading it must not
	// have to guess whether the fix is a token or a flag.
	if !strings.Contains(out, "token") {
		t.Fatalf("the refusal does not say what would authorize the bind: %s", out)
	}

	// Nothing is listening. This is the assertion the test exists for — the
	// failure it guards against is a daemon that reports an error and leaves
	// a socket open behind it.
	if conn, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		_ = conn.Close()
		t.Fatalf("something is serving %s after the bind was refused", addr)
	}
	// And no daemon at all: the data plane is not an optional extra that can
	// be dropped, so its refusal takes the whole start with it.
	if _, err := os.Stat(filepath.Join(dataDir, "run", "daemon.json")); err == nil {
		t.Fatal("a refused data plane still left a running hub behind")
	}
}

// TestHTTPPlaneTokenTierRefusesAWritingCall is the second line of defence
// firing: a read-tier credential reaches a tool it can see and may not invoke.
//
// Visibility and authority are deliberately different questions here. The tool
// stays in tools/list — the token narrows no scope — and the refusal happens at
// the call, carrying E_TOKEN_TIER_DENIED so it stays distinguishable from a
// scope denial. Both are refusals decided before the call from what an
// operator wrote down, and an incident review that cannot tell them apart is
// looking for the wrong configuration file.
//
// The unannotated tool is the important one: a tool whose server declared no
// annotations counts as DESTRUCTIVE, so a downstream that says nothing about
// itself cannot be invoked by a read credential. That is tier.ToolTier failing
// closed, and it is what makes the gate useful against real servers, most of
// which annotate nothing.
func TestHTTPPlaneTokenTierRefusesAWritingCall(t *testing.T) {
	dataDir, socket, env := sandbox(t)

	script := filepath.Join(t.TempDir(), "annotated.json")
	writeAnnotatedScript(t, script,
		annotatedTool{name: "read_thing", annotations: `{"readOnlyHint":true}`},
		annotatedTool{name: "write_thing", annotations: `{"readOnlyHint":false,"destructiveHint":true}`},
		annotatedTool{name: "silent_thing"}, // says nothing: destructive by default
	)
	runAgenthubEnv(t, env, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--args", script, "--json")
	runAgenthubEnv(t, env, "", "server", "enable", "alpha", "--no-probe")
	runAgenthubEnv(t, env, "", "config", "set", "discovery", "full")

	token := mintToken(t, env, "reader", "--tier", "read")
	url := startHTTPDaemon(t, dataDir, socket, env)
	c := &httpPlaneClient{t: t, url: url, token: token}
	c.waitReady(30 * time.Second)
	c.waitForTool("alpha__write_thing", 45*time.Second)

	// Everything is visible: the token pins no profile and no server list, so
	// it narrows nothing. A tier that hid tools would be a different feature
	// and would make the two lines of defence indistinguishable from outside.
	for _, want := range []string{"alpha__read_thing", "alpha__write_thing", "alpha__silent_thing"} {
		if !slices.Contains(c.listTools(), want) {
			t.Fatalf("%s is not visible to a read token; the tier must not narrow scope: %v", want, c.listTools())
		}
	}

	// The read-only tool goes through.
	res, rpcErr := c.callTool("alpha__read_thing", map[string]any{"marker": "allowed"}, 45*time.Second)
	if rpcErr != nil {
		t.Fatalf("a read token could not invoke a readOnlyHint tool: %v", rpcErr)
	}
	if got := c.text(res); !strings.Contains(got, "allowed") {
		t.Fatalf("read_thing answered %q", got)
	}

	// The declared-destructive one does not, and neither does the one that
	// declared nothing at all.
	for _, tool := range []string{"alpha__write_thing", "alpha__silent_thing"} {
		_, rpcErr = c.callTool(tool, map[string]any{}, 45*time.Second)
		if rpcErr == nil {
			t.Fatalf("a read token invoked %s", tool)
		}
		if !strings.Contains(rpcErr.Message, "E_TOKEN_TIER_DENIED") {
			t.Fatalf("%s was refused, but not identifiably by the tier gate: %v", tool, rpcErr)
		}
	}
}

// TestHTTPPlaneTokenServerAllowlistCanOnlyNarrow pins the seam a token's
// --server list travels through: it becomes an ordinary scope layer
// (scope.Sources.Extra) folded in by the same Merge as the persisted ones,
// which is why it can only intersect.
//
// Two servers, because a list that narrowed nothing and a scope that was never
// applied look identical when there is only one thing to see. And the token
// names a server that is NOT enabled as well, which is the widening attempt:
// a credential must not be able to reach past what the machine offers by
// naming something itself.
func TestHTTPPlaneTokenServerAllowlistCanOnlyNarrow(t *testing.T) {
	dataDir, socket, env := sandbox(t)

	runAgenthubEnv(t, env, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	runAgenthubEnv(t, env, "", "server", "enable", "alpha", "--no-probe")
	runAgenthubEnv(t, env, "", "server", "add", "beta", "--cmd", fakemcpBin, "--json")
	runAgenthubEnv(t, env, "", "server", "enable", "beta", "--no-probe")
	// gamma is registered and left OFF: naming it in the token must not turn
	// it on for the credential's holder.
	runAgenthubEnv(t, env, "", "server", "add", "gamma", "--cmd", fakemcpBin, "--json")
	runAgenthubEnv(t, env, "", "config", "set", "discovery", "full")

	token := mintToken(t, env, "narrow", "--tier", "destructive", "--server", "alpha", "--server", "gamma")
	url := startHTTPDaemon(t, dataDir, socket, env)
	c := &httpPlaneClient{t: t, url: url, token: token}
	c.waitReady(30 * time.Second)
	c.waitForTool("alpha__echo", 45*time.Second)

	// Give beta the same chance alpha had to show up before concluding it is
	// absent: an assertion made too early would pass against a scope that
	// never narrowed anything.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(c.listTools(), "beta__echo") {
			t.Fatalf("a token restricted to alpha sees beta: %v", c.listTools())
		}
		time.Sleep(500 * time.Millisecond)
	}
	if names := c.listTools(); slices.Contains(names, "gamma__echo") {
		t.Fatalf("naming a disabled server in a token turned it on: %v", names)
	}

	// Not merely invisible: unreachable. A name that resolves nowhere and a
	// name the caller is not allowed to route to must both refuse.
	if _, rpcErr := c.callTool("beta__echo", map[string]any{}, 20*time.Second); rpcErr == nil {
		t.Fatal("a token restricted to alpha called beta")
	}
}

// TestHTTPPlaneRevocationEndsALiveSession pins the re-check httpbridge's
// Session documents: the caller identity is verified on EVERY request, as a
// whole, so a token narrowed or revoked after binding does not keep its old
// authority.
//
// Revocation that only affected new connections would be the feature not
// working: an operator revokes a credential because they want it to stop now,
// and the session already holding it is exactly the one they were worried
// about. This is the same claim `server disable` makes on the stdio side, and
// it is checked here for the same reason — the CLI writes a file, and only a
// live session can say whether the file was read again.
func TestHTTPPlaneRevocationEndsALiveSession(t *testing.T) {
	dataDir, socket, env := sandbox(t)

	runAgenthubEnv(t, env, "", "server", "add", "alpha", "--cmd", fakemcpBin, "--json")
	runAgenthubEnv(t, env, "", "server", "enable", "alpha", "--no-probe")
	runAgenthubEnv(t, env, "", "config", "set", "discovery", "full")

	token := mintToken(t, env, "doomed", "--tier", "destructive")
	url := startHTTPDaemon(t, dataDir, socket, env)
	c := &httpPlaneClient{t: t, url: url, token: token}
	c.waitReady(30 * time.Second)
	c.waitForTool("alpha__echo", 45*time.Second)

	runAgenthubEnv(t, env, "", "token", "revoke", "doomed")

	// The session id is still the one the endpoint minted and the daemon was
	// never restarted, so anything but a refusal here means the credential is
	// only checked at bind time.
	frame := []byte(`{"jsonrpc":"2.0","id":99,"method":"tools/list","params":{}}`)
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, _, body := c.do(frame)
		if status == 401 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a revoked token still answered HTTP %d after %s\n%s", status, 15*time.Second, body)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
