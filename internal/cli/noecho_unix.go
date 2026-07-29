//go:build darwin || linux

package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

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
//   - the restore runs on every path (defer), so an interrupted read never
//     leaves the user's shell with echo off.
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
	defer func() { _ = unix.IoctlSetTermios(fd, ioctlWriteTermios, prev) }()

	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
