package buildrules

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestGUIBindingNamesResolve checks the one seam in this repository where two
// languages agree on a string and NOTHING checks the string.
//
// The frontend reaches the daemon by naming a Go method:
//
//	call<CallPage>("CallList", …)
//
// TypeScript checks the return type and the argument count against its own
// declaration, and Go compiles the method — so both halves are green while
// the NAME between them is wrong. The runtime failure is a red box in the
// page that needed the data, with `unknown bound method name` in it, and it
// reaches a user because nothing before them looks at both sides.
//
// It has already happened once: a rename moved services.Hub.CallPage to
// CallPage and the frontend to "CallList", and the Calls page shipped broken
// in v0.24.0.
//
// One direction only. A Go method with no frontend caller is not a fault:
// the tray and the Wails lifecycle call some of them from Go, and a binding
// that exists ahead of the page using it is ordinary. The reverse — a
// frontend naming a method that does not exist — is always a bug.
func TestGUIBindingNamesResolve(t *testing.T) {
	root := repoRoot(t)
	called := frontendBindingCalls(t, filepath.Join(root, "cmd", "agenthub-gui", "frontend", "src", "bridge.ts"))
	bound := boundServiceMethods(t, filepath.Join(root, "cmd", "agenthub-gui", "services"))

	if len(called) == 0 {
		t.Fatal("no binding calls found in bridge.ts; this test asserted nothing")
	}
	if len(bound) == 0 {
		t.Fatal("no service methods found; this test asserted nothing")
	}
	for _, name := range sortedKeys(called) {
		if !bound[name] {
			t.Errorf("bridge.ts calls the bound method %q, which cmd/agenthub-gui/services does not define.\n"+
				"TypeScript cannot see this and Go cannot either; the user sees "+
				"\"unknown bound method name\" in the page that needed it.", name)
		}
	}
}

// bindingCall matches call<T>("Name" — the one shape bridge.ts uses to reach
// a bound method. The type parameter is optional because a void call has none.
var bindingCall = regexp.MustCompile(`\bcall(?:<[^>]*>)?\(\s*"([A-Za-z][A-Za-z0-9_]*)"`)

func frontendBindingCalls(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]bool{}
	for _, m := range bindingCall.FindAllStringSubmatch(string(data), -1) {
		out[m[1]] = true
	}
	return out
}

// boundServiceMethods collects every exported method the bound service
// exposes, across every file in the package.
//
// Build tags are deliberately ignored: the file holding the window methods is
// `//go:build wails`, and a test that honoured the tag would report those as
// missing on every ordinary run — which is the opposite of useful, since a
// tagged-out method is still bound in the build the user runs.
//
// Methods on Hub and on HubService both count. HubService embeds *Hub, so a
// method on either is reachable under one name from the frontend, and the
// split between the two files is about build tags rather than about API.
func boundServiceMethods(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	out := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || !fn.Name.IsExported() {
				continue
			}
			if recv := receiverTypeName(fn.Recv.List[0].Type); recv == "Hub" || recv == "HubService" {
				out[fn.Name.Name] = true
			}
		}
	}
	return out
}

// receiverTypeName unwraps a pointer receiver to its type name.
func receiverTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}
	return ident.Name
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
