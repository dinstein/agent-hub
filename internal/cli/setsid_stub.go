//go:build !unix

package cli

// detachProcessGroup is a no-op on non-unix platforms: process-group
// semantics do not carry over, and the Windows equivalent (a Job Object)
// is unimplemented. Silently absent rather than reported — docs/status/windows.md
// lists it under the POSIX helpers that stub out.
func detachProcessGroup() error { return nil }
