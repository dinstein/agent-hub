package buildrules

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// mandatoryLogKeys are the keys internal/logx declares mandatory: every record
// that concerns one of these must use the constant, so that a log stream stays
// joinable across the gateway, daemon and CLI. The value is the helper that
// spells it.
var mandatoryLogKeys = map[string]string{
	"server":  "logx.Server(...)",
	"tool":    "logx.Tool(...)",
	"client":  "logx.Client(...)",
	"session": "logx.Session(...)",
	"rev":     "logx.Rev(...)",
	"inst":    "logx.Instance(...)",
	"pid":     "logx.PID()",
}

// logMethods are the slog call shapes whose variadic tail is a key/value list.
// `With` is included because a key bound once on a logger reaches every line
// that logger writes, which is the case the convention exists for.
var logMethods = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
	"Log": true, "With": true,
}

// attrConstructors are slog.<T>("key", v) — the other way a key reaches a
// record.
var attrConstructors = map[string]bool{
	"String": true, "Int": true, "Int64": true, "Uint64": true, "Bool": true,
	"Float64": true, "Duration": true, "Time": true, "Any": true, "Group": true,
}

// TestMandatoryLogFieldsUseTheirConstants fails when a log call spells one of
// logx's mandatory keys as a string literal instead of calling the helper.
//
// The convention is written down twice — internal/logx/fields.go ("Do not
// invent synonyms") and docs/subsystems/platform.md — and until this test
// existed nothing enforced it. A rule that quietly became a suggestion reads
// exactly like a rule, and the drift it permits is invisible: a hand-spelled
// key still produces a plausible-looking line, and the reader only finds out
// when a join across two streams silently returns nothing.
//
// FieldSession's own comment names the two assemblies that have a session id
// and says "Both go through this constant — spelling it by hand is how a
// stream stops joining." One of the two did not, at the moment that sentence
// was written.
//
// WHAT THIS CHECKS, AND WHAT IT DOES NOT. It is an AST walk, not a grep, so a
// cobra flag named "client" or a table column named "server" is not a hit: only
// a string literal in the argument list of a slog logging method or a slog attr
// constructor counts. It cannot see a key assembled at runtime, or one passed
// through a helper of the caller's own — those stay a review question. What it
// makes impossible is the cheap version of the mistake, which is the one that
// happens.
func TestMandatoryLogFieldsUseTheirConstants(t *testing.T) {
	root := repoRoot(t)
	files := productionGoFiles(t, root)
	if len(files) == 0 {
		t.Fatal("found no non-test .go files; the walk or the root is wrong")
	}

	fset := token.NewFileSet()
	calls := 0
	for _, rel := range files {
		// logx declares the constants, so it is the one package that spells
		// them.
		if strings.HasPrefix(filepath.ToSlash(rel), "internal/logx/") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			// A file listed and then deleted underneath the walk, or one this
			// tree does not own. See isTransientProbe.
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case logMethods[sel.Sel.Name]:
			case attrConstructors[sel.Sel.Name] && isIdent(sel.X, "slog"):
			default:
				return true
			}
			calls++
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				key, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				helper, mandatory := mandatoryLogKeys[key]
				if !mandatory {
					continue
				}
				pos := fset.Position(lit.Pos())
				t.Errorf("%s:%d: log call spells the mandatory key %q by hand; use %s.\n"+
					"internal/logx/fields.go declares these keys so a stream stays joinable across "+
					"the gateway, daemon and CLI. A synonym or a hand-spelled duplicate is how a join "+
					"stops returning rows, and nothing in the line looks wrong when it happens.",
					rel, pos.Line, key, helper)
			}
			return true
		})
	}
	// A refactor that routed every log call through a wrapper of its own would
	// make this test vacuously green; the count is what notices.
	if calls < 200 {
		t.Errorf("only %d slog call sites found, expected at least 200 — "+
			"the detection stopped matching and this test is no longer checking anything", calls)
	}
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
