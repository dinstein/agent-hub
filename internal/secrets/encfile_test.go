package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEncFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), encFileName)
	e := &encFile{path: path, key: deriveKey("test-pass")}

	want := map[string]string{
		"agenthub/v1/srv/_global/token": "s3cret",
		"agenthub/v1/srv/work/token":    "other",
	}
	if err := e.save(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := e.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("got[%q] = %q, want %q", k, got[k], v)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secrets.enc permissions = %o, want 600", perm)
	}
}

func TestEncFileMissingIsEmpty(t *testing.T) {
	e := &encFile{path: filepath.Join(t.TempDir(), encFileName), key: deriveKey("k")}
	m, err := e.load()
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %v", m)
	}
}

func TestEncFileWrongKeyFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), encFileName)
	if err := (&encFile{path: path, key: deriveKey("right")}).save(map[string]string{"k": "v"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	_, err := (&encFile{path: path, key: deriveKey("wrong")}).load()
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("load with wrong key: got %v, want ErrDecrypt", err)
	}
}

// TestEncFileRandomNonce: sealing the same plaintext twice must produce
// different files (fresh random nonce per write).
func TestEncFileRandomNonce(t *testing.T) {
	dir := t.TempDir()
	m := map[string]string{"k": "v"}
	read := func(name string) []byte {
		t.Helper()
		e := &encFile{path: filepath.Join(dir, name), key: deriveKey("k")}
		if err := e.save(m); err != nil {
			t.Fatalf("save: %v", err)
		}
		b, err := os.ReadFile(e.path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return b
	}
	if bytes.Equal(read("a.enc"), read("b.enc")) {
		t.Fatal("two seals of the same plaintext produced identical files (nonce reuse?)")
	}
}

// TestDeriveKey: 64 hex chars are a raw key; anything else is hashed.
// Both must be 32 bytes and deterministic.
func TestDeriveKey(t *testing.T) {
	hexKey := "000102030405060708090a0b0c0d0e0f000102030405060708090a0b0c0d0e0f"
	k := deriveKey(hexKey)
	if len(k) != 32 || k[0] != 0x00 || k[1] != 0x01 {
		t.Fatalf("hex key not decoded raw: %x", k)
	}
	p1, p2 := deriveKey("passphrase"), deriveKey("passphrase")
	if len(p1) != 32 || !bytes.Equal(p1, p2) {
		t.Fatal("passphrase derivation not deterministic 32 bytes")
	}
	if bytes.Equal(p1, deriveKey("other")) {
		t.Fatal("distinct passphrases derived the same key")
	}
	// Surrounding whitespace is trimmed (values often arrive via env).
	if !bytes.Equal(deriveKey(" passphrase \n"), p1) {
		t.Fatal("whitespace not trimmed before derivation")
	}
}

func TestDevKeyLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), devKeyFileName)
	k1, err := loadOrCreateDevKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(k1) != 32 {
		t.Fatalf("dev key length %d, want 32", len(k1))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("dev key permissions = %o, want 600", perm)
	}
	k2, err := loadOrCreateDevKey(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("dev key not stable across loads")
	}
	// Corrupt file is an error, not a silent regeneration (regenerating
	// would orphan everything sealed under the old key).
	if err := os.WriteFile(path, []byte("not-hex"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateDevKey(path); err == nil {
		t.Fatal("corrupt dev key file: expected error")
	}
}
