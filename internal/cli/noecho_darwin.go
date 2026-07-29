package cli

import "golang.org/x/sys/unix"

// termios ioctl numbers on darwin/BSD.
const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
)
