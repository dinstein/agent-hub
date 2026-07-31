package clients

import (
	"encoding/json"
	"strings"
	"testing"
)

// dupDoc is a settings.json whose section key appears twice. JSON permits
// it, encoding/json resolves it to the LAST value, and so does every client
// application that reads these files — so the last one is the one in
// effect, and the only one an edit may touch.
const dupDoc = `// zed settings
{
  "context_servers": { "other": { "command": "other" } },
  "context_servers": { "agenthub": { "command": "agenthub" } }
}`

// TestSpliceRemovesFromTheDecidingDuplicate is the regression for the
// silent-no-op revocation the 2026-07-31 sweep confirmed.
//
// The locator resolved a duplicated key to the first occurrence while
// encoding/json — used by read() to find the entry, and by verifySplice to
// check the edit — resolved it to the last. So Disconnect found the entry,
// spliced against the shadowed object, changed nothing, verified clean, and
// reported the entry removed while the client kept spawning it.
func TestSpliceRemovesFromTheDecidingDuplicate(t *testing.T) {
	after, err := spliceRemove([]byte(dupDoc), []string{"context_servers"}, []string{"agenthub"})
	if err != nil {
		t.Fatalf("spliceRemove: %v", err)
	}
	if string(after) == dupDoc {
		t.Fatal("the document is unchanged: the removal was a silent no-op reported as success")
	}
	if strings.Contains(string(after), "agenthub") {
		t.Errorf("the entry survived the removal:\n%s", after)
	}

	// What the client actually reads afterwards must have no agenthub in it.
	var got map[string]map[string]any
	if err := json.Unmarshal([]byte(blankJSONC([]byte(after))), &got); err != nil {
		t.Fatalf("the result no longer decodes: %v\n%s", err, after)
	}
	if _, still := got["context_servers"]["agenthub"]; still {
		t.Errorf("the deciding section still holds agenthub:\n%s", after)
	}
}

// TestSpliceEntryTargetsTheDecidingDuplicate is the Connect mirror: an
// insert into a shadowed section is a connect the client never sees.
func TestSpliceEntryTargetsTheDecidingDuplicate(t *testing.T) {
	after, err := spliceEntry([]byte(dupDoc), []string{"context_servers"}, "hub2", map[string]any{"command": "hub2"})
	if err != nil {
		t.Fatalf("spliceEntry: %v", err)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal([]byte(blankJSONC([]byte(after))), &got); err != nil {
		t.Fatalf("the result no longer decodes: %v\n%s", err, after)
	}
	if _, ok := got["context_servers"]["hub2"]; !ok {
		t.Errorf("the entry went into a section the client does not read:\n%s", after)
	}
	if _, ok := got["context_servers"]["agenthub"]; !ok {
		t.Errorf("the deciding section lost an entry the edit did not name:\n%s", after)
	}
}

// TestSpliceVerifierCatchesADuplicatedEntryName covers the remaining
// ambiguity: the SECTION resolves to one object, but the edited entry
// appears twice inside it. spliceRemoveMembers deletes every occurrence,
// which is what "remove it" means, and the verifier must agree rather than
// flag it.
func TestSpliceVerifierCatchesADuplicatedEntryName(t *testing.T) {
	const doc = `// zed settings
{
  "context_servers": {
    "agenthub": { "command": "old" },
    "agenthub": { "command": "new" }
  }
}`
	after, err := spliceRemove([]byte(doc), []string{"context_servers"}, []string{"agenthub"})
	if err != nil {
		t.Fatalf("spliceRemove: %v", err)
	}
	if strings.Contains(string(after), "agenthub") {
		t.Errorf("a duplicated entry name left an occurrence behind:\n%s", after)
	}
	if err := verifySplice([]byte(doc), after, []string{"context_servers"}, []string{"agenthub"}); err != nil {
		t.Errorf("verifySplice rejected a correct removal: %v\n%s", err, after)
	}
}
