// Command healthts writes the frontend's generated copy of the Health
// display contract constants. It is invoked by the go:generate directive in
// cmd/agenthub-gui/main.go, so its default paths are relative to
// cmd/agenthub-gui.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/dinstein/agent-hub/cmd/agenthub-gui/internal/healthgen"
)

func main() {
	apiDir := flag.String("api", "../../api", "path to the api package source directory")
	out := flag.String("out", "frontend/src/generated/health.ts", "path of the generated TypeScript module")
	flag.Parse()

	if err := healthgen.WriteFile(*apiDir, *out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
