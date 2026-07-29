//go:build tools

// Package tools pins module dependencies that M1 packages are being
// implemented against concurrently. Parallel implementation agents must not
// edit go.mod; this file keeps `go mod tidy` from dropping the pins until
// real imports land, after which entries here can be removed.
package tools

import (
	_ "github.com/fsnotify/fsnotify"
	_ "github.com/zalando/go-keyring"
	_ "golang.org/x/crypto/chacha20poly1305"
)
