package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

// Sentinel errors of the keyring layer.
var (
	// ErrKeyringNotFound reports a missing entry. Both the real backend
	// and fakes must return it (or wrap it) so the chain can distinguish
	// "miss" from "broken".
	ErrKeyringNotFound = errors.New("secrets: keyring entry not found")
	// ErrKeyringTimeout reports an operation that exceeded the hard
	// timeout — typically a keychain unlock prompt nobody is answering.
	ErrKeyringTimeout = errors.New("secrets: keyring operation timed out")
)

// Backend abstracts the OS keyring so tests never touch the real
// keychain. The zalando-backed implementation is systemBackend; tests
// inject fakes via ChainConfig.Keyring.
type Backend interface {
	Get(service, user string) (string, error)
	Set(service, user, secret string) error
	Delete(service, user string) error
}

// systemBackend adapts zalando/go-keyring, translating its not-found
// sentinel to ours.
type systemBackend struct{}

func (systemBackend) Get(service, user string) (string, error) {
	v, err := keyring.Get(service, user)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrKeyringNotFound
	}
	return v, err
}

func (systemBackend) Set(service, user, secret string) error {
	return keyring.Set(service, user, secret)
}

func (systemBackend) Delete(service, user string) error {
	err := keyring.Delete(service, user)
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrKeyringNotFound
	}
	return err
}

// probeUser is the account name used by the availability probe. It is
// only ever read — never written — because a Set probe would trigger the
// destructive macOS confirmation prompt.
const probeUser = "agenthub/probe"

// hardKeyring wraps a Backend with the 7.11 hardening: read-based
// availability probe with a process-lifetime cache, and a hard timeout on
// every operation.
type hardKeyring struct {
	b       Backend
	service string
	timeout time.Duration

	mu     sync.Mutex
	probed bool
	avail  bool
}

func newHardKeyring(b Backend, service string, timeout time.Duration) *hardKeyring {
	return &hardKeyring{b: b, service: service, timeout: timeout}
}

// available reports whether the keyring backend answers at all. The probe
// is a Get of a well-known absent account: both success and
// ErrKeyringNotFound prove the backend is alive; a timeout or any other
// error marks it unavailable. The verdict is cached for the process
// lifetime — an unavailable keyring flips the chain into the enc-file dev
// fallback (ruling A.6 #5) and must not re-prompt on every call.
func (h *hardKeyring) available(ctx context.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.probed {
		return h.avail
	}
	_, err := h.get(ctx, probeUser)
	h.probed = true
	h.avail = err == nil || errors.Is(err, ErrKeyringNotFound)
	return h.avail
}

// get runs Backend.Get under the hard timeout. The worker goroutine is
// intentionally abandoned on timeout: a blocked keychain prompt cannot be
// cancelled, and abandoning it is the only way to unblock the caller.
// Results travel through a buffered channel so an abandoned worker never
// races with the caller's return values.
func (h *hardKeyring) get(ctx context.Context, user string) (string, error) {
	type result struct {
		v   string
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := h.b.Get(h.service, user)
		done <- result{v, err}
	}()
	timer := time.NewTimer(h.timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("secrets: keyring get: %w", ctx.Err())
	case <-timer.C:
		return "", fmt.Errorf("secrets: keyring get: %w", ErrKeyringTimeout)
	}
}

// set / del: same timeout discipline as get.
func (h *hardKeyring) set(ctx context.Context, user, secret string) error {
	return h.run(ctx, "set", func() error { return h.b.Set(h.service, user, secret) })
}

func (h *hardKeyring) del(ctx context.Context, user string) error {
	return h.run(ctx, "delete", func() error { return h.b.Delete(h.service, user) })
}

func (h *hardKeyring) run(ctx context.Context, name string, op func() error) error {
	done := make(chan error, 1)
	go func() { done <- op() }()
	timer := time.NewTimer(h.timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("secrets: keyring %s: %w", name, ctx.Err())
	case <-timer.C:
		return fmt.Errorf("secrets: keyring %s: %w", name, ErrKeyringTimeout)
	}
}

// --- self-managed key registry -------------------------------------------
//
// OS keyrings cannot enumerate entries, so the chain mirrors every storage
// key it writes to the keyring into a plain JSON registry file (key names
// only, never values). Invariant: the registry mutates only alongside a
// successful keyring mutation, so it never claims keys the keyring lost
// nor loses keys the keyring still holds.

// keyRegistryFileName is the registry file inside the secrets directory.
const keyRegistryFileName = "keyring-keys.json"

type keyRegistryDoc struct {
	Version int      `json:"version"`
	Keys    []string `json:"keys"`
}

// loadKeyRegistry reads the registry; a missing file is an empty registry.
func loadKeyRegistry(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: read key registry: %w", err)
	}
	var doc keyRegistryDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("secrets: parse key registry: %w", err)
	}
	return doc.Keys, nil
}

// saveKeyRegistry writes the registry atomically (sorted, deduplicated,
// 0600 — key names embed server IDs and should not be world-readable).
func saveKeyRegistry(path string, keys []string) error {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	slices.Sort(out)
	data, err := json.MarshalIndent(keyRegistryDoc{Version: 1, Keys: out}, "", "  ")
	if err != nil {
		return fmt.Errorf("secrets: encode key registry: %w", err)
	}
	return atomicWrite0600(path, append(data, '\n'))
}
