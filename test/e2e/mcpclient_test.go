package e2e_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/internal/mcp"
)

// gatewayClient is a hand-written MCP stdio client: it spawns the real
// `agenthub connect --client <id>` gateway process and speaks
// newline-delimited JSON-RPC to it, exactly like Claude Code would. It is
// deliberately built on encoding/json alone (no internal/mcp import) so the
// suite exercises the wire format from the outside.
type gatewayClient struct {
	t        *testing.T
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	msgs     chan []byte // frames read from the gateway's stdout
	rdDone   chan struct{}
	dispDone chan struct{}
	stderr   *tailBuffer

	// wmu serializes frame writes. The dispatcher answers reverse RPCs from
	// its own goroutine while a test writes requests from its own, and two
	// concurrent Writes to a pipe can interleave mid-frame.
	wmu sync.Mutex

	mu     sync.Mutex
	nextID int
	notes  []string // notification methods observed (diagnostics)
	// pending maps a request's raw JSON id to whoever is awaiting it.
	pending map[string]chan *rpcMsg
	// dispErr is a fault the dispatcher could not report itself (t.Fatalf is
	// illegal off the test goroutine); the next await surfaces it.
	dispErr error
}

// rpcMsg is the union of everything the gateway may send us.
type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// codeRetryBusy is the gateway's transient "downstreams still connecting"
// code, taken from the facade rather than mirrored as a literal.
//
// A copied number is only ever read when the race it guards actually
// happens, so a stale one does not fail — it turns a retry loop into a hard
// failure on whichever run is slow enough to hit it. This constant was
// -32000 and went stale the moment the gateway moved off the legacy band.
const codeRetryBusy = mcp.CodeBusy

// startGateway spawns the gateway child and starts the frame reader.
func startGateway(t *testing.T, dataDir, clientID string) *gatewayClient {
	t.Helper()
	return startGatewayEnv(t, testEnv(dataDir), clientID)
}

// startGatewayEnv is startGateway with an explicit child environment.
func startGatewayEnv(t *testing.T, env []string, clientID string) *gatewayClient {
	t.Helper()
	cmd := exec.Command(agenthubBin, "connect", "--client", clientID)
	cmd.Env = env
	tail := newTailBuffer(16 << 10)
	cmd.Stderr = tail
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gateway: %v", err)
	}

	c := &gatewayClient{
		t:        t,
		cmd:      cmd,
		stdin:    stdin,
		msgs:     make(chan []byte, 64),
		rdDone:   make(chan struct{}),
		dispDone: make(chan struct{}),
		stderr:   tail,
		pending:  make(map[string]chan *rpcMsg),
	}
	go func() {
		defer close(c.msgs)
		defer close(c.rdDone)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 32<<20)
		for sc.Scan() {
			c.msgs <- append([]byte(nil), sc.Bytes()...)
		}
	}()
	go c.dispatch()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			<-c.rdDone
			_ = cmd.Wait()
		}
	})
	return c
}

// dispatch is the single reader of the gateway's frames: it routes each
// response to whoever is waiting on that id, answers reverse RPCs, and
// records notifications.
//
// It exists so MORE THAN ONE REQUEST CAN BE IN FLIGHT. call() used to read
// the stream itself and discard every response whose id it was not waiting
// for, which made the client structurally serial — a second request could
// not be sent until the first was answered, and any test of concurrency was
// unwritable rather than merely unwritten.
//
// It runs on its own goroutine and therefore must never call t.Fatalf: the
// testing package only allows that from the test's own goroutine, and doing
// it here would turn a diagnosable failure into a hung or panicking run.
// Anything it cannot handle is recorded in dispErr and reported by the next
// await() — on the test goroutine, where it can fail properly.
func (c *gatewayClient) dispatch() {
	defer close(c.dispDone)
	for raw := range c.msgs {
		var m rpcMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			c.setDispErr(fmt.Errorf("gateway sent a non-JSON frame: %v\n%s", err, raw))
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null":
			c.answerReverse(&m)
		case m.Method != "":
			c.mu.Lock()
			c.notes = append(c.notes, m.Method)
			c.mu.Unlock()
		default:
			c.deliver(&m)
		}
	}
}

// deliver hands a response to its waiter. A response nobody is waiting for
// is DROPPED rather than reported: a cancelled request's late reply and a
// stale reverse-RPC answer both land here legitimately.
func (c *gatewayClient) deliver(m *rpcMsg) {
	c.mu.Lock()
	ch := c.pending[string(m.ID)]
	delete(c.pending, string(m.ID))
	c.mu.Unlock()
	if ch != nil {
		ch <- m
	}
}

func (c *gatewayClient) setDispErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dispErr == nil {
		c.dispErr = err
	}
}

func (c *gatewayClient) takeDispErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.dispErr
	c.dispErr = nil
	return err
}

// stderrTail returns the retained gateway stderr for failure diagnostics.
func (c *gatewayClient) stderrTail() string { return c.stderr.String() }

// noteSnapshot copies the notification methods observed so far. The
// dispatcher records them as they arrive, so this advances on its own
// rather than only across an intervening RPC.
func (c *gatewayClient) noteSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.notes...)
}

// dumpStacks SIGQUITs the gateway so the Go runtime prints every
// goroutine's stack to the stderr this client already captures. Used when
// a call never returns: "timeout" alone cannot say where it was parked.
// The gateway dies in the process, so only call this on a failing path.
func (c *gatewayClient) dumpStacks() {
	if c.cmd.Process == nil {
		return
	}
	_ = c.cmd.Process.Signal(syscall.SIGQUIT)
	time.Sleep(500 * time.Millisecond) // let the runtime finish writing
}

func (c *gatewayClient) fatalf(format string, args ...any) {
	c.t.Helper()
	c.t.Fatalf(format+"\n--- gateway stderr tail ---\n%s", append(args, c.stderrTail())...)
}

// writeFrame emits one frame. The lock is what keeps a reverse-RPC answer
// written by the dispatcher from interleaving with a request written by a
// test goroutine — two Writes to the same pipe are not atomic, and a torn
// frame would surface as the gateway "sending garbage".
//
// It never calls t.Fatalf, because the dispatcher calls it too. A write
// failure means the gateway is gone, which the awaiting call reports.
func (c *gatewayClient) writeFrame(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		c.setDispErr(fmt.Errorf("encode frame: %w", err))
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		c.setDispErr(fmt.Errorf("write frame %s: %w", b, err))
	}
}

// notify sends a notification.
func (c *gatewayClient) notify(method string, params any) {
	c.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	c.writeFrame(msg)
}

// inflight is one sent request whose answer has not been collected yet. It
// is what lets a test hold several requests open at once: begin returns
// immediately, and each handle is awaited in whatever order the test likes.
type inflight struct {
	c      *gatewayClient
	id     int
	method string
	ch     chan *rpcMsg
}

// begin sends a request and returns without waiting. The dispatcher parks
// the response on the handle's channel whether or not anyone is awaiting it
// yet, so a fast answer to an early request cannot be lost while the test is
// still sending later ones.
func (c *gatewayClient) begin(method string, params any) *inflight {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan *rpcMsg, 1)
	c.pending[fmt.Sprintf("%d", id)] = ch
	c.mu.Unlock()

	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	c.writeFrame(msg)
	return &inflight{c: c, id: id, method: method, ch: ch}
}

// await blocks for this request's response.
func (ic *inflight) await(timeout time.Duration) (json.RawMessage, *rpcError) {
	c := ic.c
	c.t.Helper()
	if err := c.takeDispErr(); err != nil {
		c.fatalf("%v", err)
	}
	select {
	case m := <-ic.ch:
		if err := c.takeDispErr(); err != nil {
			c.fatalf("%v", err)
		}
		return m.Result, m.Error
	case <-c.dispDone:
		// The frame stream ended: the gateway closed stdout or died.
		if err := c.takeDispErr(); err != nil {
			c.fatalf("%v", err)
		}
		c.fatalf("gateway closed stdout while waiting for %s response", ic.method)
	case <-time.After(timeout):
		// "timeout" alone cannot say where the gateway was parked, so take
		// the goroutine dump with us on the way out.
		c.dumpStacks()
		c.fatalf("timeout (%s) waiting for %s response", timeout, ic.method)
	}
	return nil, nil // unreachable: every branch above fails the test
}

// settled reports whether the response has already arrived, without waiting.
// It is how a case asserts that one request is STILL running while another
// finished — the distinction a serial client could not express at all, and
// the one that separates "the fast call overtook the slow one" from "the
// slow one had quietly already finished".
func (ic *inflight) settled() bool { return len(ic.ch) > 0 }

// abandon stops awaiting a request and releases its slot. A cancelled
// request is never answered by contract, so its waiter would otherwise stay
// registered for the client's lifetime.
func (ic *inflight) abandon() {
	c := ic.c
	c.mu.Lock()
	delete(c.pending, fmt.Sprintf("%d", ic.id))
	c.mu.Unlock()
}

// call performs one request and waits for its response — begin plus await,
// which is what almost every case wants.
func (c *gatewayClient) call(method string, params any, timeout time.Duration) (json.RawMessage, *rpcError) {
	c.t.Helper()
	return c.begin(method, params).await(timeout)
}

// answerReverse replies to a gateway->client request. The e2e client
// declares no roots capability, so roots/list should not arrive — but
// answering defensively keeps the read loop deadlock-free either way.
func (c *gatewayClient) answerReverse(m *rpcMsg) {
	c.t.Helper()
	// DEPRECATED-UPSTREAM(roots, earliest-removal: 2027-07-28): the e2e
	// client answers the reverse RPC a ≤ 2025-11-25 server may still send.
	if m.Method == "roots/list" {
		c.writeFrame(map[string]any{
			"jsonrpc": "2.0", "id": json.RawMessage(m.ID),
			"result": map[string]any{"roots": []any{}},
		})
		return
	}
	c.writeFrame(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(m.ID),
		"error": map[string]any{"code": -32601, "message": "e2e client does not serve " + m.Method},
	})
}

// initialize performs the MCP handshake and asserts the gateway identity.
func (c *gatewayClient) initialize() {
	c.t.Helper()
	res, rpcErr := c.call("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "agenthub-e2e", "version": "0"},
	}, 30*time.Second)
	if rpcErr != nil {
		c.fatalf("initialize failed: %v", rpcErr)
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(res, &init); err != nil {
		c.fatalf("initialize result: %v\n%s", err, res)
	}
	if init.ServerInfo.Name != "agenthub" || init.ProtocolVersion != "2025-06-18" {
		c.fatalf("unexpected initialize result: %s", res)
	}
	c.notify("notifications/initialized", nil)
}

// listTools returns the current exposed tool names.
func (c *gatewayClient) listTools(timeout time.Duration) []string {
	c.t.Helper()
	res, rpcErr := c.call("tools/list", map[string]any{}, timeout)
	if rpcErr != nil {
		c.fatalf("tools/list failed: %v", rpcErr)
	}
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		c.fatalf("tools/list result: %v\n%s", err, res)
	}
	names := make([]string, 0, len(list.Tools))
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// waitForTool polls tools/list until the exposed name appears (the gateway
// answers from cache first and pushes list_changed when the live catalog is
// ready; polling covers both signals).
func (c *gatewayClient) waitForTool(name string, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var last []string
	for time.Now().Before(deadline) {
		last = c.listTools(30 * time.Second)
		for _, n := range last {
			if n == name {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	c.fatalf("tool %q never appeared within %s; last tools/list = %v", name, timeout, last)
}

// callTool invokes tools/call, retrying the gateway's transient
// "still connecting" busy error until the deadline.
func (c *gatewayClient) callTool(name string, args any, timeout time.Duration) json.RawMessage {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		res, rpcErr := c.call("tools/call", map[string]any{"name": name, "arguments": args}, timeout)
		if rpcErr == nil {
			return res
		}
		if rpcErr.Code != codeRetryBusy || !time.Now().Before(deadline) {
			c.fatalf("tools/call %s failed: %v", name, rpcErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// callToolRefused invokes tools/call expecting the gateway to REFUSE it,
// and returns the JSON-RPC error it answered with.
//
// It retries the same transient busy error callTool does, and for a reason
// that decides whether this helper proves anything: a gate rejection and a
// downstream that has not finished connecting are both errors on the wire,
// so a caller that asserted on the first error to arrive would pass on
// whichever run lost that race — and would keep passing with every gate
// removed. Only a non-busy error is a decision.
//
// A call that SUCCEEDS fails immediately rather than being retried: success
// is terminal, and waiting out the deadline would report the refusal that
// did not happen as a timeout.
func (c *gatewayClient) callToolRefused(name string, args any, timeout time.Duration) *rpcError {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		res, rpcErr := c.call("tools/call", map[string]any{"name": name, "arguments": args}, timeout)
		if rpcErr == nil {
			c.fatalf("tools/call %s was ANSWERED, not refused: %s", name, res)
		}
		if rpcErr.Code != codeRetryBusy {
			return rpcErr
		}
		if !time.Now().Before(deadline) {
			c.fatalf("tools/call %s stayed busy for %s and never reached a decision", name, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// textContent flattens the text items of a tools/call result and asserts
// the tool did not report an error.
func (c *gatewayClient) textContent(res json.RawMessage) string {
	c.t.Helper()
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		c.fatalf("tools/call result: %v\n%s", err, res)
	}
	if out.IsError {
		c.fatalf("tool reported isError: %s", res)
	}
	var sb strings.Builder
	for _, item := range out.Content {
		if item.Type == "text" {
			sb.WriteString(item.Text)
		}
	}
	return sb.String()
}

// errorText is textContent's mirror: it flattens the text of a tools/call
// result that MUST report isError, and fails when the call succeeded.
//
// The two are separate helpers rather than one with a flag because they
// guard opposite mistakes, and the dangerous one is silent. A meta-tool
// refusal is a RESULT, not a JSON-RPC error (internal/discovery.ErrorResult),
// so a test that read the text without checking isError would report a
// refusal and a successful call identically — and pass on the call that
// should never have run.
func (c *gatewayClient) errorText(res json.RawMessage) string {
	c.t.Helper()
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		c.fatalf("tools/call result: %v\n%s", err, res)
	}
	if !out.IsError {
		c.fatalf("tools/call SUCCEEDED where a refusal was required: %s", res)
	}
	var sb strings.Builder
	for _, item := range out.Content {
		if item.Type == "text" {
			sb.WriteString(item.Text)
		}
	}
	return sb.String()
}

// close performs a clean client-side disconnect (stdin EOF) and asserts the
// gateway exits 0.
func (c *gatewayClient) close() {
	c.t.Helper()
	if err := c.stdin.Close(); err != nil {
		c.fatalf("close stdin: %v", err)
	}
	select {
	case <-c.rdDone:
	case <-time.After(15 * time.Second):
		_ = c.cmd.Process.Kill()
		c.fatalf("gateway did not exit within 15s of stdin EOF")
	}
	if err := c.cmd.Wait(); err != nil {
		c.fatalf("gateway exit: %v (want exit 0 on clean disconnect)", err)
	}
}

// tailBuffer is a concurrency-safe writer retaining the last max bytes —
// enough gateway stderr to diagnose a failure without unbounded growth.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
