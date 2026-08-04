package buildrules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSSETopicListsAgree keeps the two copies of the SSE topic vocabulary
// from drifting apart.
//
// `internal/ctlapi/sse.go` owns the closed set the server accepts; `api`
// declares the same names for the clients that subscribe, and its own header
// says the daemon MATCHES these rather than treating them as free text. The
// duplication is not laziness and cannot be refactored away: `api` must not
// import `internal/*` (AGENTS.md constraint 1), so the second copy is
// structurally required and therefore permanently unguarded.
//
// This project has already paid for exactly this shape once, inside ctlapi:
// a hand-written copy of the topic list went on offering `activity` for the
// whole life of the removal that deleted the topic, and the hint naming it
// sent every reader back to the same 400. That one was fixed by DERIVING the
// list (`sseTopicNames`), which is the better answer wherever it is available
// — and across this boundary it is not.
//
// The two directions fail differently, which is why both are checked. A topic
// in `api` that the server does not accept is a client subscribing to a 400
// it has no way to anticipate. A topic the server accepts that `api` does not
// name is a stream no first-party client can reach, and the omission looks
// exactly like the feature not existing.
func TestSSETopicListsAgree(t *testing.T) {
	root := repoRoot(t)
	server := topicConstants(t, filepath.Join(root, "internal", "ctlapi", "sse.go"))
	client := topicConstants(t, filepath.Join(root, "api", "events.go"))

	if len(server) < 3 {
		t.Fatalf("parsed %d topics out of internal/ctlapi/sse.go; the declaration shape must have "+
			"changed, and a scan that finds nothing agrees with everything", len(server))
	}
	if len(client) == 0 {
		t.Fatal("parsed no topics out of api/events.go; see above")
	}

	for name, want := range server {
		got, ok := client[name]
		if !ok {
			t.Errorf("internal/ctlapi declares the SSE topic %s (%q) and api does not.\n"+
				"A stream no first-party client can name reads exactly like a feature that "+
				"does not exist.", name, want)
			continue
		}
		if got != want {
			t.Errorf("SSE topic %s is %q in internal/ctlapi and %q in api.\n"+
				"The wire name is what the server matches on; the two spellings cannot both "+
				"be right.", name, want, got)
		}
	}
	for name, got := range client {
		if _, ok := server[name]; !ok {
			t.Errorf("api declares the SSE topic %s (%q), which internal/ctlapi does not accept.\n"+
				"A client subscribing to it gets a 400 it had no way to anticipate — the same "+
				"failure the `activity` hint produced before sseTopicNames was derived.", name, got)
		}
	}
}

// topicConst matches `TopicSomething = "wire"`, the one form both files use.
var topicConst = regexp.MustCompile(`^\s*(Topic\w+)\s*=\s*"([a-z_]+)"`)

// topicConstants reads the Topic* constants of one file. Both declare them in
// a plain const block, so a line scan is enough and keeps this check free of
// an import that would raise its own dependency question.
func topicConstants(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if m := topicConst.FindStringSubmatch(line); m != nil {
			out[m[1]] = m[2]
		}
	}
	return out
}
