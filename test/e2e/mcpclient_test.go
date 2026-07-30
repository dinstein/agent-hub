package e2e_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// gatewayClient is a hand-written MCP stdio client: it spawns the real
// `agenthub connect --client <id>` gateway process and speaks
// newline-delimited JSON-RPC to it, exactly like Claude Code would. It is
// deliberately built on encoding/json alone (no internal/mcp import) so the
// suite exercises the wire format from the outside.
type gatewayClient struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	msgs   chan []byte // frames read from the gateway's stdout
	rdDone chan struct{}
	stderr *tailBuffer
	nextID int

	mu    sync.Mutex
	notes []string // notification methods observed (diagnostics)
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

// codeRetryBusy mirrors the gateway's transient "downstreams still
// connecting" error code.
const codeRetryBusy = -32000

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
		t:      t,
		cmd:    cmd,
		stdin:  stdin,
		msgs:   make(chan []byte, 64),
		rdDone: make(chan struct{}),
		stderr: tail,
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
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			<-c.rdDone
			_ = cmd.Wait()
		}
	})
	return c
}

// stderrTail returns the retained gateway stderr for failure diagnostics.
func (c *gatewayClient) stderrTail() string { return c.stderr.String() }

// noteSnapshot copies the notification methods observed so far. Frames are
// drained inside call(), so this only advances across an intervening RPC.
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

func (c *gatewayClient) writeFrame(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("encode frame: %v", err)
	}
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		c.fatalf("write frame %s: %v", b, err)
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

// call performs one request and waits for its response, transparently
// answering gateway-initiated reverse RPCs (roots/list) and recording
// notifications along the way.
func (c *gatewayClient) call(method string, params any, timeout time.Duration) (json.RawMessage, *rpcError) {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	c.writeFrame(msg)

	wantID := []byte(fmt.Sprintf("%d", id))
	deadline := time.After(timeout)
	for {
		select {
		case raw, ok := <-c.msgs:
			if !ok {
				c.fatalf("gateway closed stdout while waiting for %s response", method)
			}
			var m rpcMsg
			if err := json.Unmarshal(raw, &m); err != nil {
				c.fatalf("gateway sent a non-JSON frame: %v\n%s", err, raw)
			}
			switch {
			case m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null":
				c.answerReverse(&m)
			case m.Method != "":
				c.mu.Lock()
				c.notes = append(c.notes, m.Method)
				c.mu.Unlock()
			case bytes.Equal(m.ID, wantID):
				return m.Result, m.Error
			default:
				// Responses to other ids (stale reverse-RPC replies) drop.
			}
		case <-deadline:
			// "timeout" alone cannot say where the gateway was parked, so
			// take the goroutine dump with us on the way out.
			c.dumpStacks()
			c.fatalf("timeout (%s) waiting for %s response", timeout, method)
		}
	}
}

// answerReverse replies to a gateway->client request. The e2e client
// declares no roots capability, so roots/list should not arrive — but
// answering defensively keeps the read loop deadlock-free either way.
func (c *gatewayClient) answerReverse(m *rpcMsg) {
	c.t.Helper()
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
