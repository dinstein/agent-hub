// Command agenthub is the single required binary: CLI management commands,
// the stdio gateway (connect) and the daemon are all subcommands of it.
//
// This file is deliberately thin: everything testable lives in internal/cli.
package main

import (
	"os"

	"github.com/dinstein/agent-hub/internal/cli"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

// channel is "release" only when a build says so, via
// -ldflags "-X main.channel=release". Every other build — `go build`,
// `go run`, an IDE, a forgotten flag — is development.
//
// What that decides, and why the default is the safe direction rather than
// the convenient one, lives with the decision itself in
// cli.Options.ForChannel; this variable only carries the value there.
var channel = "dev"

func main() {
	opts := cli.Options{
		Version: version,
		Args:    os.Args[1:],
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	}
	opts.ForChannel(channel)
	os.Exit(cli.Main(opts))
}
