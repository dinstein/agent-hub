package secrets

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// encFileName is the sealed vault file inside the secrets directory.
	encFileName = "secrets.enc"
	// devKeyFileName holds the auto-generated dev-fallback key (hex, 0600)
	// beside the enc file (ruling A.6 #5).
	devKeyFileName = "secrets.enc.key"
	// encAAD binds ciphertexts to this format version as AEAD associated
	// data — a v2 envelope can never be replayed as v1.
	encAAD = "agenthub/secrets/v1"
	// encVersion is the envelope format version.
	encVersion = 1
)

// ErrDecrypt is returned when secrets.enc cannot be opened with the
// provided key material. It is fail-closed: the chain surfaces it instead
// of silently falling through to the keyring, because a wrong
// AGENTHUB_SECRET_KEY must be visible, not swallowed.
var ErrDecrypt = errors.New("secrets: cannot decrypt secrets.enc (wrong key or corrupted file)")

// deriveKey turns AGENTHUB_SECRET_KEY into a 32-byte XChaCha20-Poly1305
// key: 64 hex characters decode to a raw key; anything else is treated as
// a passphrase and hashed with SHA-256 (no slow KDF in this milestone —
// operators are expected to supply high-entropy key material, documented
// on EnvEncKey).
func deriveKey(material string) []byte {
	material = strings.TrimSpace(material)
	if len(material) == 64 {
		if raw, err := hex.DecodeString(material); err == nil {
			return raw
		}
	}
	sum := sha256.Sum256([]byte(material))
	return sum[:]
}

// encEnvelope is the on-disk JSON wrapper. Nonce and Data are base64 via
// encoding/json's []byte handling.
type encEnvelope struct {
	Version int    `json:"version"`
	Nonce   []byte `json:"nonce"`
	Data    []byte `json:"data"`
}

// encFile seals a whole map[storageKey]value under one key. Load-modify-
// save cycles are serialized by the owning Chain's mutex.
type encFile struct {
	path string
	key  []byte
}

// load reads and opens the sealed map. A missing file is an empty map, not
// an error. Any decryption failure returns ErrDecrypt.
func (e *encFile) load() (map[string]string, error) {
	raw, err := os.ReadFile(e.path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: read %s: %w", e.path, err)
	}
	var env encEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("secrets: parse %s: %w", e.path, err)
	}
	if env.Version != encVersion {
		return nil, fmt.Errorf("secrets: %s: unsupported envelope version %d", e.path, env.Version)
	}
	aead, err := chacha20poly1305.NewX(e.key)
	if err != nil {
		return nil, fmt.Errorf("secrets: init cipher: %w", err)
	}
	if len(env.Nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("%w: bad nonce length %d", ErrDecrypt, len(env.Nonce))
	}
	plain, err := aead.Open(nil, env.Nonce, env.Data, []byte(encAAD))
	if err != nil {
		return nil, ErrDecrypt
	}
	m := map[string]string{}
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, fmt.Errorf("secrets: decode %s payload: %w", e.path, err)
	}
	return m, nil
}

// save seals the map under a fresh random nonce and writes it atomically
// with 0600 permissions.
func (e *encFile) save(m map[string]string) error {
	plain, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("secrets: encode payload: %w", err)
	}
	aead, err := chacha20poly1305.NewX(e.key)
	if err != nil {
		return fmt.Errorf("secrets: init cipher: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("secrets: nonce: %w", err)
	}
	env := encEnvelope{
		Version: encVersion,
		Nonce:   nonce,
		Data:    aead.Seal(nil, nonce, plain, []byte(encAAD)),
	}
	out, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("secrets: encode envelope: %w", err)
	}
	return atomicWrite0600(e.path, append(out, '\n'))
}

// loadOrCreateDevKey returns the dev-fallback key, generating and
// persisting a fresh random one (hex, 0600) on first use.
func loadOrCreateDevKey(path string) ([]byte, error) {
	key, err := readDevKey(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("secrets: generate dev key: %w", err)
	}
	if err := atomicWrite0600(path, []byte(hex.EncodeToString(key)+"\n")); err != nil {
		return nil, err
	}
	return key, nil
}

// readDevKey reads an existing dev key file; fs.ErrNotExist passes through
// so callers can distinguish "absent" from "corrupt".
func readDevKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: read dev key: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("secrets: dev key file %s is corrupt", path)
	}
	return key, nil
}

// atomicWrite0600 persists data: same-directory temp file, chmod 0600,
// write, fsync, rename over the target, fsync of the parent directory.
// Never leaves a partially written target (same ladder as
// internal/registry, re-implemented here to keep secrets self-contained).
func atomicWrite0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secrets: ensure dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory so a preceding rename is durable. Filesystems
// that do not support directory fsync (EINVAL/ENOTSUP) are tolerated.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
			return nil
		}
		return err
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
