package calllog

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

var packMagic = [4]byte{'A', 'H', 'P', '1'}

const packPrefixBytes = 4 + 4 + 8

type packHeader struct {
	Version int         `json:"v"`
	CallID  string      `json:"callId"`
	Kind    PayloadKind `json:"kind"`
	Nonce   []byte      `json:"nonce"`
	Raw     int         `json:"rawBytes"`
	Codec   string      `json:"codec"`
}

type packWriter struct {
	dir      string
	bootID   string
	keyID    string
	maxBytes int64
	aead     cipherAEAD
	seq      int
	f        *os.File
	size     int64
	guard    func(int64, func() error) error
}

// cipherAEAD is the subset used here, kept narrow for tests and to avoid
// exposing the concrete XChaCha implementation in the store type.
type cipherAEAD interface {
	NonceSize() int
	Overhead() int
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}

func newPackWriter(dir, bootID string, key []byte, keyID string, maxBytes int64, guard func(int64, func() error) error) (*packWriter, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("calllog: payload cipher: %w", err)
	}
	return &packWriter{dir: dir, bootID: bootID, keyID: keyID, maxBytes: maxBytes, aead: aead, guard: guard}, nil
}

func (w *packWriter) openNext() error {
	if w.f != nil {
		if err := w.f.Sync(); err != nil {
			return err
		}
		if err := w.f.Close(); err != nil {
			return err
		}
	}
	name := fmt.Sprintf("payload-%s-p%d-%04d.pack", w.bootID, os.Getpid(), w.seq)
	w.seq++
	f, err := os.OpenFile(filepath.Join(w.dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("calllog: open payload pack: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.f, w.size = f, fi.Size()
	return nil
}

func gzipBytes(raw []byte) ([]byte, error) {
	var b bytes.Buffer
	zw, err := gzip.NewWriterLevel(&b, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func (w *packWriter) append(callID string, kind PayloadKind, raw []byte, sync bool, day string) (PayloadRef, error) {
	compressed, err := gzipBytes(raw)
	if err != nil {
		return PayloadRef{}, fmt.Errorf("calllog: compress payload: %w", err)
	}
	nonce := make([]byte, w.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return PayloadRef{}, fmt.Errorf("calllog: payload nonce: %w", err)
	}
	h := packHeader{Version: Version, CallID: callID, Kind: kind, Nonce: nonce, Raw: len(raw), Codec: "gzip"}
	header, err := json.Marshal(h)
	if err != nil {
		return PayloadRef{}, err
	}
	ciphertext := w.aead.Seal(nil, nonce, compressed, header)
	entryLen := int64(packPrefixBytes + len(header) + len(ciphertext))
	buf := make([]byte, packPrefixBytes, int(entryLen))
	copy(buf[:4], packMagic[:])
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(header)))
	binary.BigEndian.PutUint64(buf[8:16], uint64(len(ciphertext)))
	buf = append(buf, header...)
	buf = append(buf, ciphertext...)
	var offset int64
	err = w.guard(entryLen, func() error {
		if w.f == nil || w.size > 0 && w.size+entryLen > w.maxBytes {
			if err := w.openNext(); err != nil {
				return err
			}
		}
		offset = w.size
		if _, err := w.f.Write(buf); err != nil {
			return fmt.Errorf("calllog: append payload: %w", err)
		}
		w.size += entryLen
		if sync {
			if err := w.f.Sync(); err != nil {
				return fmt.Errorf("calllog: sync payload: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return PayloadRef{}, err
	}
	return PayloadRef{
		Day: day, File: filepath.Base(w.f.Name()), Offset: offset, Length: entryLen,
		RawBytes: len(raw), StoredBytes: len(ciphertext), KeyID: w.keyID,
	}, nil
}

func (w *packWriter) close() error {
	if w == nil || w.f == nil {
		return nil
	}
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		return err
	}
	err := w.f.Close()
	w.f = nil
	return err
}

func readPayload(root string, ref PayloadRef, key []byte) ([]byte, packHeader, error) {
	if _, err := time.Parse("2006-01-02", ref.Day); err != nil || ref.File == "" || filepath.Base(ref.File) != ref.File || ref.Offset < 0 || ref.Length <= 0 {
		return nil, packHeader{}, ErrBadReference
	}
	f, err := os.Open(filepath.Join(root, ref.Day, ref.File))
	if err != nil {
		return nil, packHeader{}, err
	}
	defer func() { _ = f.Close() }()
	r := io.NewSectionReader(f, ref.Offset, ref.Length)
	prefix := make([]byte, packPrefixBytes)
	if _, err := io.ReadFull(r, prefix); err != nil {
		return nil, packHeader{}, fmt.Errorf("calllog: read payload prefix: %w", err)
	}
	if !bytes.Equal(prefix[:4], packMagic[:]) {
		return nil, packHeader{}, fmt.Errorf("calllog: bad payload magic")
	}
	headerLen := binary.BigEndian.Uint32(prefix[4:8])
	cipherLen := binary.BigEndian.Uint64(prefix[8:16])
	if int64(packPrefixBytes)+int64(headerLen)+int64(cipherLen) != ref.Length || headerLen > 1<<20 || cipherLen > 32<<20 {
		return nil, packHeader{}, ErrBadReference
	}
	header := make([]byte, headerLen)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, packHeader{}, err
	}
	var h packHeader
	if err := json.Unmarshal(header, &h); err != nil || h.Version != Version || h.Codec != "gzip" {
		return nil, packHeader{}, fmt.Errorf("calllog: invalid payload header")
	}
	if h.Raw < 0 || h.Raw > MaxPayloadBytes || len(h.Nonce) != chacha20poly1305.NonceSizeX {
		return nil, packHeader{}, fmt.Errorf("calllog: invalid payload bounds")
	}
	if h.Raw != ref.RawBytes || int(cipherLen) != ref.StoredBytes {
		return nil, packHeader{}, fmt.Errorf("calllog: payload reference size mismatch")
	}
	ciphertext := make([]byte, cipherLen)
	if _, err := io.ReadFull(r, ciphertext); err != nil {
		return nil, packHeader{}, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, packHeader{}, ErrBadKey
	}
	keyID, err := KeyID(key)
	if err != nil || keyID != ref.KeyID {
		return nil, packHeader{}, fmt.Errorf("calllog: payload key id mismatch")
	}
	compressed, err := aead.Open(nil, h.Nonce, ciphertext, header)
	if err != nil {
		return nil, packHeader{}, fmt.Errorf("calllog: decrypt payload: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, packHeader{}, err
	}
	limited := io.LimitReader(zr, int64(h.Raw)+1)
	raw, err := io.ReadAll(limited)
	closeErr := zr.Close()
	if err != nil {
		return nil, packHeader{}, err
	}
	if closeErr != nil {
		return nil, packHeader{}, closeErr
	}
	if len(raw) != h.Raw {
		return nil, packHeader{}, fmt.Errorf("calllog: payload length %d, want %d", len(raw), h.Raw)
	}
	return raw, h, nil
}
