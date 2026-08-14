//go:build darwin || linux

package cli

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// TestRestoreOnSignalPutsTheTerminalBackBeforeTheSignalLands is the
// regression for the finding the 2026-07-31 sweep confirmed.
//
// readNoEcho's own comment and docs/subsystems/cli.md both promised
// that "an interrupted read never leaves the user's shell with echo off",
// and the restore was a defer. readNoEcho deliberately leaves ISIG enabled
// so Ctrl-C works at a hidden prompt, and Go's default disposition for
// SIGINT terminates the process without running any deferred function — so
// the one interruption the promise was about was the one it did not cover.
//
// The sequencing is asserted here rather than in a real prompt because the
// signal under test kills the test binary once its default disposition is
// restored.
func TestRestoreOnSignalPutsTheTerminalBackBeforeTheSignalLands(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	order := make(chan string, 4)

	go restoreOnSignal(sigs, done,
		func() { order <- "restore" },
		func(os.Signal) { order <- "raise" },
	)

	sigs <- syscall.SIGINT

	want := []string{"restore", "raise"}
	for _, w := range want {
		select {
		case got := <-order:
			if got != w {
				t.Fatalf("step order = %q, want %q: the terminal must be restored before the signal is allowed to kill us", got, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q; a signal at a hidden prompt would exit with echo still off", w)
		}
	}
	close(done)
}

// TestRestoreOnSignalDoesNothingOnANormalReturn keeps the handler from
// touching the terminal a second time, or re-raising a signal nobody sent,
// when the read simply finished.
func TestRestoreOnSignalDoesNothingOnANormalReturn(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	acted := make(chan string, 2)
	finished := make(chan struct{})

	go func() {
		restoreOnSignal(sigs, done,
			func() { acted <- "restore" },
			func(os.Signal) { acted <- "raise" },
		)
		close(finished)
	}()

	close(done)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the signal watcher outlived the read it was guarding")
	}
	select {
	case got := <-acted:
		t.Fatalf("the watcher did %q with no signal delivered", got)
	default:
	}
}

// TestReadNoEchoRefusesANonTerminal pins the other documented failure
// direction: a redirected stdin errors rather than reading a credential it
// would echo into a log.
func TestReadNoEchoRefusesANonTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatalf("creating the stand-in for a redirected stdin: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := readNoEcho(f); err == nil {
		t.Fatal("a non-terminal was read with echo state unknown")
	}
}
