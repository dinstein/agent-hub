//go:build !unix

package cli

// detachProcessGroup is a no-op on non-unix platforms (Windows lands in
// M2; process-group semantics differ there anyway).
func detachProcessGroup() error { return nil }
