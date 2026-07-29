package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dinstein/agent-hub/internal/registry"
)

// canonical.md §6 lists three golden-test families that must run in CI from
// day one: the signature grammar, the search ranking, and the ERROR COPY.
// The first two live in internal/discovery (testdata/ranking.txt and the
// exposure corpora); this file is the third.
//
// What is frozen here is the whole failure contract of the CLI: for every
// classifiable error, the stable machine code, the process exit code, the
// message and the hint. Agents and scripts key off all four — a silent
// wording change is a contract break, which is exactly what a golden file
// makes impossible to do by accident.
//
// Regenerate with:
//
//	go test ./internal/cli -update
//
// and REVIEW the diff: every line is a contract, not an artefact.
var updateErrorGolden = flag.Bool("update", false, "rewrite testdata golden files")

// errorCase is one frozen rendering.
type errorCase struct {
	Name     string `json:"name"`
	Code     string `json:"code"`
	ExitCode int    `json:"exitCode"`
	Message  string `json:"message"`
	Hint     string `json:"hint,omitempty"`
}

// errorCorpus is the fixed set of failures the CLI can classify. It covers
// every constructor plus the two registry sentinels that are translated
// rather than constructed.
func errorCorpus() []error {
	quarantined := &registry.UnreadableError{
		Kind:           registry.DocServers,
		Path:           "/data/registry/servers.json",
		QuarantinePath: "/data/registry/servers.json.unreadable-20260726T120000.000000000Z",
		Err:            errors.New("invalid character '}' looking for beginning of object key string"),
	}
	return []error{
		Usagef("nothing to change: pass --enable-server/--disable-server/--tools/--discovery/--reset"),
		NotFoundf(CodeServerNotFound, "no server %q", "github"),
		NotFoundf(CodeProfileNotFound, "no profile %q", "work"),
		NotFoundf(CodeSecretNotFound, "no secret %q for server %q", "TOKEN", "github"),
		NotFoundf(CodeSkillNotFound, "no skill %q", "review"),
		NotFoundf(CodeToolNotFound, "no tool %q", "fs__read_file"),
		NotFoundf(CodeSessionNotFound, "no live session %q", "cursor:1"),
		DaemonDownf("this command needs a running daemon"),
		AuthFailedf("authentication failed for server %q", "github"),
		Deniedf("the call was denied by governance policy"),
		&Error{Code: CodeServerExists, ExitCode: ExitGeneral, Message: `server "github" already exists`,
			Hint: "pass a different name, or remove the existing entry first"},
		&Error{Code: CodeDaemonRunning, ExitCode: ExitGeneral, Message: "the daemon is already running"},
		&Error{Code: CodeInvalidJSON, ExitCode: ExitUsage, Message: "stdin is not valid JSON",
			Err: errors.New("unexpected end of JSON input")},
		&Error{Code: CodeUnsupportedTransport, ExitCode: ExitUsage, Message: `unknown transport "grpc"`},
		&Error{Code: CodeClientUnsupported, ExitCode: ExitGeneral, Message: `client "emacs" is not supported`},
		&Error{Code: CodeClientNotConnected, ExitCode: ExitGeneral, Message: `client "cursor" is not connected`},
		&Error{Code: CodeNotImplemented, ExitCode: ExitGeneral, Message: "not implemented in this milestone"},
		&Error{Code: CodeStateCorrupt, ExitCode: ExitLocked, Message: "the integrity state file is corrupt"},
		&Error{Code: CodeTightenOnly, ExitCode: ExitDenied,
			Message: "this operation may only tighten scope; widening needs an approved grant"},
		&Error{Code: CodeConfigKeyUnknown, ExitCode: ExitUsage, Message: `unknown config key "colour"`},
		registry.ErrLockTimeout,
		quarantined,
		errors.New("an unclassified failure"),
	}
}

// caseName labels a corpus entry stably: the code it renders to, so the
// golden file is readable and reorderings are visible in the diff.
func caseName(i int, d errorCase) string {
	return fmt.Sprintf("%02d-%s", i, d.Code)
}

func TestErrorCopyGolden(t *testing.T) {
	corpus := errorCorpus()
	got := make([]errorCase, 0, len(corpus))
	for i, err := range corpus {
		d := errorDetailFor(err)
		c := errorCase{Code: d.Code, ExitCode: ExitCodeFor(err), Message: d.Message, Hint: d.Hint}
		c.Name = caseName(i, c)
		got = append(got, c)
	}

	path := filepath.Join("testdata", "errors.golden.json")
	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')

	if *updateErrorGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run: go test ./internal/cli -update)", err)
	}
	if string(want) != string(encoded) {
		t.Errorf("error copy drifted from the frozen contract.\n--- want ---\n%s\n--- got ---\n%s",
			want, encoded)
	}
}

// Every stable code constant must appear in the corpus: a new code that
// nobody froze is a contract nobody reviewed.
func TestErrorGoldenCoversEveryCode(t *testing.T) {
	all := []string{
		CodeGeneral, CodeUsage, CodeNotFound, CodeServerNotFound, CodeServerExists,
		CodeDaemonDown, CodeDaemonRunning, CodeAuthFailed, CodeDenied, CodeLockTimeout,
		CodeRegistryCorrupt, CodeInvalidJSON, CodeUnsupportedTransport, CodeClientUnsupported,
		CodeClientNotConnected, CodeNotImplemented, CodeProfileNotFound, CodeProfileExists,
		CodeSessionNotFound, CodeToolNotFound, CodeSkillNotFound, CodeSkillExists,
		CodeSecretNotFound, CodeConfigKeyUnknown, CodeStateCorrupt, CodeTightenOnly,
	}
	covered := map[string]bool{}
	for _, err := range errorCorpus() {
		covered[errorDetailFor(err).Code] = true
	}
	// CodeNotFound / CodeProfileExists / CodeSkillExists have no dedicated
	// corpus entry only if they are genuinely unreachable; list the gap
	// explicitly instead of letting it pass silently.
	var missing []string
	for _, code := range all {
		if !covered[code] {
			missing = append(missing, code)
		}
	}
	want := []string{CodeNotFound, CodeProfileExists, CodeSkillExists}
	if len(missing) != len(want) {
		t.Fatalf("uncovered codes = %v, want exactly %v (add the new code to errorCorpus)", missing, want)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Errorf("uncovered codes = %v, want %v", missing, want)
			break
		}
	}
}
