// Command fakemcp is a standalone scripted fake MCP downstream server
// speaking newline-delimited JSON-RPC over stdio (test infrastructure,
// canonical.md §6). It exists for spawn tests that want a dedicated
// binary instead of the TestMain re-exec pattern (fakemcp.MaybeServe).
//
// The behavior script is taken from, in order of precedence:
//
//  1. the FAKEMCP_SCRIPT environment variable (JSON),
//  2. a JSON script file given as the first argument,
//  3. the fakemcp.Minimal() default (one echo tool).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/dinstein/agent-hub/internal/testutil/fakemcp"
)

func main() {
	fakemcp.MaybeServe() // never returns when FAKEMCP_SCRIPT is set

	script := fakemcp.Minimal()
	if len(os.Args) > 1 {
		data, err := os.ReadFile(os.Args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakemcp: %v\n", err)
			os.Exit(2)
		}
		if script, err = fakemcp.ParseScript(data); err != nil {
			fmt.Fprintf(os.Stderr, "fakemcp: %v\n", err)
			os.Exit(2)
		}
	}
	if err := fakemcp.Serve(context.Background(), os.Stdin, os.Stdout, os.Stderr, script); err != nil {
		fmt.Fprintf(os.Stderr, "fakemcp: %v\n", err)
		os.Exit(1)
	}
}
