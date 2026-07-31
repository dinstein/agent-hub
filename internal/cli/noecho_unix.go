//go:build darwin || linux

package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// readNoEcho reads one line from a terminal with local echo disabled, then
// restores the previous terminal state.
//
// Implemented directly on termios (golang.org/x/sys is already a direct
// dependency) rather than pulling in golang.org/x/term for one call.
//
// Failure directions, both deliberate:
//   - a non-terminal fd returns an error instead of reading anyway, so a
//     redirected stdin can never silently echo a credential into a log;
//   - the restore runs on every path — the deferred call for a normal
//     return or a read error, and restoreOnSignal for the one that used to
//     escape it — so an interrupted read never leaves the user's shell with
//     echo off.
//
// That second promise needed the signal handler to be true. This function
// deliberately leaves ISIG enabled so Ctrl-C at a hidden prompt still works,
// and Go's default disposition for SIGINT terminates the process — which
// runs no deferred function. The operator was left typing blind into a
// shell with echo off until they thought to run `stty sane`, and the
// sequence that produced it (`agenthub secret set …`, then change your
// mind) is an ordinary one, not an edge case.
func readNoEcho(f *os.File) (string, error) {
	fd := int(f.Fd())
	prev, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return "", fmt.Errorf("not a terminal: %w", err)
	}
	noEcho := *prev
	noEcho.Lflag &^= unix.ECHO
	noEcho.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, &noEcho); err != nil {
		return "", fmt.Errorf("disable echo: %w", err)
	}

	var once sync.Once
	restore := func() {
		once.Do(func() { _ = unix.IoctlSetTermios(fd, ioctlWriteTermios, prev) })
	}
	defer restore()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	done := make(chan struct{})
	defer func() {
		signal.Stop(sigs)
		close(done)
	}()
	go restoreOnSignal(sigs, done, restore, reraise)

	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// restoreOnSignal puts the terminal back before a signal that would
// otherwise kill this process gets to do it, then lets the signal proceed
// with its default meaning.
//
// The order is the whole point: restore first, re-raise second. Anything
// that ran the other way round would still exit with echo disabled, and
// swallowing the signal instead of re-raising would be worse than the bug —
// Ctrl-C at a password prompt must stop the program.
//
// Split out of readNoEcho so the sequencing can be tested without a signal
// that kills the test binary.
func restoreOnSignal(sigs <-chan os.Signal, done <-chan struct{}, restore func(), raise func(os.Signal)) {
	select {
	case sig, ok := <-sigs:
		if !ok {
			return
		}
		restore()
		raise(sig)
	case <-done:
	}
}

// reraise restores a signal's default disposition and delivers it again, so
// the process dies the way it would have without a handler installed. A
// signal Go does not model as syscall.Signal is ignored rather than
// guessed at; the terminal has already been restored by then either way.
func reraise(sig os.Signal) {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return
	}
	signal.Reset(s)
	_ = syscall.Kill(syscall.Getpid(), s)
}
