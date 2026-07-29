package cli

import "golang.org/x/sys/unix"

// termios ioctl numbers on linux.
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)
