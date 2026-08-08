package e2e_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A truncated result is retained behind a cursor, and cursor ids are a
// guessable sequence BY DESIGN — internal/shaping says so, and builds the
// isolation somewhere else entirely: every failure returns one frozen
// message. Unknown, expired, another session's, malformed, unreadable store —
// all the same bytes, "no variants, no error codes that differ", because
// telling "expired" apart from "not yours" would turn fetch_result into an
// oracle for enumerating another session's cursor space.
//
// lazy_test.go drives this door forward — truncate, fetch, page again — and
// never once drives it at something it should not open. So the suite covered
// the half that cannot leak and none of the half that can.
//
// These cases are about INDISTINGUISHABILITY, which is a property of two
// answers rather than of one, and cannot be asserted by any test that makes a
// single request. Each compares a miss against another miss, byte for byte.

// bigCallCursor puts a client in lazy mode, calls a tool with a payload past
// the session's result budget, and returns the cursor from the truncation
// trailer along with the live client.
func bigCallCursor(t *testing.T, dataDir, clientID string) (*gatewayClient, string, int) {
	t.Helper()
	c := startGateway(t, dataDir, clientID)
	c.initialize()
	c.waitForSearchHit("echo", "fake__echo", 30*time.Second)

	res := c.callTool("call_tool", map[string]any{
		"tool":      "fake__echo",
		"arguments": map[string]any{"payload": strings.Repeat("z", 4000)},
	}, 30*time.Second)
	cursor, offset := c.parseTrailer(c.textContent(res))
	return c, cursor, offset
}

// fetchText returns the flattened text of a fetch_result answer, whether it
// succeeded or missed. It deliberately does not assert on isError: the whole
// point here is to compare a miss with a miss, so the helper must be able to
// carry one.
func fetchText(t *testing.T, c *gatewayClient, cursor string, offset int) string {
	t.Helper()
	res := c.callTool("fetch_result", map[string]any{
		"cursor": cursor, "offset": offset,
	}, 30*time.Second)
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		c.fatalf("fetch_result result: %v\n%s", err, res)
	}
	var sb strings.Builder
	for _, item := range out.Content {
		if item.Type == "text" {
			sb.WriteString(item.Text)
		}
	}
	return sb.String()
}

// lazyBudgetFixture registers one downstream and pins lazy mode with a result
// budget small enough that an ordinary echo overflows it.
func lazyBudgetFixture(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	runAgenthub(t, dataDir, "", "server", "add", "fake", "--cmd", fakemcpBin, "--json")
	enableServer(t, dataDir, "fake")
	writeGovernance(t, dataDir, map[string]any{
		"discovery":    "lazy",
		"resultBudget": map[string]any{"*": map[string]any{"bytes": 512}},
	})
	return dataDir
}

// TestAnotherSessionsCursorIsIndistinguishableFromOneThatNeverExisted is the
// case the design's own reasoning asks for.
//
// Two gateway processes, one registry. The second is handed a cursor the
// first minted — the exact value, not a guess, which is a strictly stronger
// position than an attacker enumerating ids would ever be in. It must be
// answered with the same bytes as a cursor nobody ever minted, so that
// knowing an id tells a session nothing about whether it exists.
func TestAnotherSessionsCursorIsIndistinguishableFromOneThatNeverExisted(t *testing.T) {
	dataDir := lazyBudgetFixture(t)

	owner, cursor, offset := bigCallCursor(t, dataDir, "cursor-owner")
	// The owner can read its own cursor — otherwise the comparison below
	// would be between two answers that are the same because nothing works.
	if page := fetchText(t, owner, cursor, offset); !strings.Contains(page, "z") {
		owner.fatalf("the minting session cannot read its own cursor: %q", page)
	}

	other := startGateway(t, dataDir, "cursor-stranger")
	other.initialize()
	other.waitForSearchHit("echo", "fake__echo", 30*time.Second)

	foreign := fetchText(t, other, cursor, offset)
	invented := fetchText(t, other, "agenthub-cursor-that-was-never-minted", 0)
	if foreign != invented {
		other.fatalf("a foreign cursor and an invented one are distinguishable:\n"+
			" foreign:  %q\n invented: %q", foreign, invented)
	}
	// And it is the frozen miss, not some third thing both happen to share.
	if !strings.Contains(foreign, "unknown or expired cursor") {
		other.fatalf("the miss is not the frozen not-found message: %q", foreign)
	}
	// The payload cannot have leaked into a refusal.
	if strings.Contains(foreign, "zzz") {
		other.fatalf("the refusal carries the other session's data: %q", foreign)
	}

	other.close()
	owner.close()
}

// TestEveryCursorMissLooksTheSame widens the comparison to the other shapes a
// miss arrives in.
//
// The rule is "one message, no variants" across unknown, expired, foreign,
// malformed and unreadable-store, and the reason it is worth a test rather
// than a reading is that each of those is a different branch. A refusal that
// grew a distinguishing detail on one of them — a code, a suffix, a different
// capitalisation — would restore exactly the oracle the single message exists
// to remove.
func TestEveryCursorMissLooksTheSame(t *testing.T) {
	dataDir := lazyBudgetFixture(t)
	c, cursor, _ := bigCallCursor(t, dataDir, "cursor-shapes")

	misses := map[string]string{
		"never minted":     fetchText(t, c, "no-such-cursor", 0),
		"empty-ish":        fetchText(t, c, "-", 0),
		"path shaped":      fetchText(t, c, "../../etc/passwd", 0),
		"real id, wrong":   fetchText(t, c, cursor+"x", 0),
		"absurdly long":    fetchText(t, c, strings.Repeat("a", 4096), 0),
		"looks structured": fetchText(t, c, "stdio:cursor-shapes/1", 0),
	}
	var first, firstName string
	for name, text := range misses {
		if firstName == "" {
			first, firstName = text, name
			continue
		}
		if text != first {
			c.fatalf("cursor misses are distinguishable:\n %s: %q\n %s: %q",
				firstName, first, name, text)
		}
	}
	if !strings.Contains(first, "unknown or expired cursor") {
		c.fatalf("the shared miss is not the frozen message: %q", first)
	}
	c.close()
}

// TestAnOffsetPastTheEndIsAnEmptyPageAndNotAMiss is the control, and the case
// that keeps the two above from passing on a gateway that answers everything
// with the not-found text.
//
// internal/shaping draws this line explicitly: an offset at or past the end
// of the retained payload serves an empty page, "which is a success, not a
// miss". The distinction is what lets a client tell "I have read it all" from
// "your cursor is gone and you must re-run the call" — collapsing them would
// send an agent back to repeat an expensive tool call it had already finished
// reading.
func TestAnOffsetPastTheEndIsAnEmptyPageAndNotAMiss(t *testing.T) {
	dataDir := lazyBudgetFixture(t)
	c, cursor, _ := bigCallCursor(t, dataDir, "cursor-end")

	past := fetchText(t, c, cursor, 1<<20)
	if strings.Contains(past, "unknown or expired cursor") {
		c.fatalf("reading past the end was reported as a lost cursor: %q", past)
	}
	if strings.Contains(past, "z") {
		c.fatalf("an offset past the end returned payload: %q", past)
	}
	c.close()
}
