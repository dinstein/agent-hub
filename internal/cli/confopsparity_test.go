package cli

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/platform"
	"github.com/dinstein/agent-hub/internal/registry"
)

// TestCLIAndConfopsProduceIdenticalDocuments is the guarantee that phase A
// exists for (docs/modules/controlplane.md): the CLI and any other
// front end must not be able to drift, because they are the same code.
//
// The same script of operations is run twice — once through the real cobra
// command tree, once by calling internal/confops directly the way the
// control plane will — into two separate data directories, and every
// resulting document is compared BYTE FOR BYTE. That catches more than "the
// same fields were set": key order, the nil-vs-empty distinction of the
// three-state selectors, and the generation counter all have to match, so a
// second implementation that merely looks equivalent still fails here.
func TestCLIAndConfopsProduceIdenticalDocuments(t *testing.T) {
	cliDir := setDataDir(t)
	runScriptThroughCLI(t)

	opsDir := t.TempDir()
	runScriptThroughConfops(t, opsDir)

	compareTrees(t, cliDir, opsDir)
}

// runScriptThroughCLI executes the script as an operator would.
func runScriptThroughCLI(t *testing.T) {
	t.Helper()
	mustRun(t, "", "server", "add", "github", "--cmd", "gh-mcp", "--args", "-y,pkg",
		"--env", "TOKEN=${SECRET_GH}")
	mustRun(t, "", "server", "add", "linear", "--cmd", "linear-mcp")
	mustRun(t, "", "server", "add", "remote", "--url", "https://example.com/mcp")
	mustRun(t, "", "server", "disable", "linear")

	mustRun(t, "", "profile", "create", "work", "--servers", "github")
	mustRun(t, "", "profile", "server", "add", "work", "linear")
	mustRun(t, "", "profile", "tool", "allow", "work", "github", "--only", "list_prs,create_pr")
	mustRun(t, "", "profile", "tool", "allow", "work", "linear", "--none")

	mustRun(t, "", "profile", "discovery", "work", "grouped")

	mustRun(t, "", "client", "bind", "cursor", "work")
	mustRun(t, "", "client", "bind", "throwaway", "work")
	mustRun(t, "", "client", "unbind", "throwaway")

	mustRun(t, "", "profile", "rename", "work", "work2")
	mustRun(t, "", "profile", "use", "work2")

	mustRun(t, "", "config", "set", "resultBudget.*", "65536")
	mustRun(t, "", "config", "set", "resultBudget.github", "1024!")

	mustRun(t, "", "server", "rm", "remote")
	mustRun(t, "", "profile", "create", "spare")
	mustRun(t, "", "profile", "rm", "spare")
	mustRun(t, "", "profile", "server", "rm", "work2", "linear")
}

// compareTrees asserts that both data directories hold the same set of
// registry and state documents with byte-identical content.
func runScriptThroughConfops(t *testing.T, dataDir string) {
	t.Helper()
	ctx := context.Background()
	resolver := &platform.Resolver{LookupEnv: func(key string) (string, bool) {
		if key == platform.EnvDataDir {
			return dataDir, true
		}
		return "", false
	}}
	regDir, err := resolver.RegistryDir()
	if err != nil {
		t.Fatal(err)
	}
	st, err := registry.Open(regDir)
	if err != nil {
		t.Fatal(err)
	}
	no := confops.Precondition{}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	_, err = confops.AddServer(ctx, st, confops.ServerSpec{ID: "github", Entry: registry.ServerEntry{
		Transport: registry.TransportStdio,
		Command:   "gh-mcp", Args: []string{"-y", "pkg"},
		Env: map[string]string{"TOKEN": "${SECRET_GH}"}, Source: sourceManual,
	}}, no)
	must("add github", err)
	_, err = confops.AddServer(ctx, st, confops.ServerSpec{ID: "linear", Entry: registry.ServerEntry{
		Transport: registry.TransportStdio, Command: "linear-mcp", Source: sourceManual,
	}}, no)
	must("add linear", err)
	_, err = confops.AddServer(ctx, st, confops.ServerSpec{ID: "remote", Entry: registry.ServerEntry{
		Transport: registry.TransportHTTP, URL: "https://example.com/mcp",
		Source: sourceManual,
	}}, no)
	must("add remote", err)
	_, err = confops.SetServerEnabled(ctx, st, "linear", false, no)
	must("disable linear", err)

	_, err = confops.CreateProfile(ctx, st, "work", []string{"github"}, no)
	must("create work", err)
	_, err = confops.SetProfileServers(ctx, st, "work",
		confops.ServerSelection{Mode: confops.ServerSetAdd, Servers: []string{"linear"}}, no)
	must("profile server add", err)
	_, err = confops.SetProfileTools(ctx, st, "work", "github",
		confops.ToolSelection{Mode: confops.ToolSelectOnly, Tools: []string{"list_prs", "create_pr"}}, no)
	must("profile tools --only", err)
	_, err = confops.SetProfileTools(ctx, st, "work", "linear",
		confops.ToolSelection{Mode: confops.ToolSelectNone}, no)
	must("profile tools --none", err)

	_, err = confops.SetProfileDiscovery(ctx, st, "work", "grouped", no)
	must("profile discovery", err)

	work := &confops.ProfileBindingSpec{Kind: registry.BindingNamed, Name: "work"}
	_, err = confops.SetClientBinding(ctx, st, "cursor", confops.ClientBinding{Profile: work}, no)
	must("client bind cursor", err)
	_, err = confops.SetClientBinding(ctx, st, "throwaway", confops.ClientBinding{Profile: work}, no)
	must("client bind throwaway", err)
	_, err = confops.ClearClientBinding(ctx, st, "throwaway", no)
	must("client unbind throwaway", err)

	_, err = confops.RenameProfile(ctx, st, "work", "work2", no)
	must("rename", err)
	_, err = confops.SetActiveProfile(ctx, st, "work2", no)
	must("profile use", err)

	must("config set bool", err)
	_, err = confops.SetGovernance(ctx, st, confops.ResultBudgetPrefix+"*", "65536", no)
	must("config set budget", err)
	_, err = confops.SetGovernance(ctx, st, confops.ResultBudgetPrefix+"github", "1024!", no)
	must("config set forced budget", err)

	_, err = confops.RemoveServer(ctx, st, "remote", no, confops.RemoveOptions{})
	must("rm remote", err)
	_, err = confops.CreateProfile(ctx, st, "spare", nil, no)
	must("create spare", err)
	_, err = confops.RemoveProfile(ctx, st, "spare", no)
	must("rm spare", err)
	_, err = confops.SetProfileServers(ctx, st, "work2",
		confops.ServerSelection{Mode: confops.ServerSetRemove, Servers: []string{"linear"}}, no)
	must("profile server rm", err)
}

func compareTrees(t *testing.T, wantDir, gotDir string) {
	t.Helper()
	// registry is required; state is compared when either side wrote
	// anything. Nothing writes <state> any more — the tool-override file was
	// the last document there and it went with the governance stores — but
	// the comparison stays so a new state document is covered the day one
	// arrives, rather than being added and silently unchecked.
	for _, sub := range []string{"registry", "state"} {
		wantFiles := readDocs(t, filepath.Join(wantDir, sub))
		gotFiles := readDocs(t, filepath.Join(gotDir, sub))
		if sub == "registry" && len(wantFiles) == 0 {
			t.Fatalf("%s produced no documents in %s", wantDir, sub)
		}
		for _, name := range sortedNames(wantFiles) {
			got, ok := gotFiles[name]
			if !ok {
				t.Errorf("%s/%s: the confops path never wrote it", sub, name)
				continue
			}
			if !bytes.Equal(wantFiles[name], got) {
				t.Errorf("%s/%s differs between the two paths\n--- cli ---\n%s\n--- confops ---\n%s",
					sub, name, wantFiles[name], got)
			}
		}
		for _, name := range sortedNames(gotFiles) {
			if _, ok := wantFiles[name]; !ok {
				t.Errorf("%s/%s: the confops path wrote a document the CLI did not", sub, name)
			}
		}
	}
}

// readDocs reads every top-level *.json file of a directory. Subdirectories
// (registry backups) are skipped: they hold prior generations, which are a
// function of the write order and not part of the resulting configuration.
func readDocs(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]byte{}
		}
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	return out
}

func sortedNames(m map[string][]byte) []string {
	out := slices.Sorted(maps.Keys(m))
	return out
}
