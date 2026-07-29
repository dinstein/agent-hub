package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// ArgsHash returns the SHA-256 (lowercase hex) of the canonical-JSON
// encoding of raw. Empty or whitespace-only input is treated as the JSON
// literal null, so "call with no arguments" hashes to one deterministic
// constant.
//
// The hash binds an audit record (and an approval) to the exact arguments
// without ever retaining them ("what was approved is what runs").
func ArgsHash(raw []byte) (string, error) {
	c, err := CanonicalJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}

// CanonicalJSON re-encodes one JSON document into its canonical form:
//
//   - object keys sorted lexicographically (byte order), duplicate keys
//     collapse to the last occurrence (encoding/json semantics);
//   - no insignificant whitespace;
//   - numbers preserved lexically via json.Number (1, 1.0 and 1e0 stay
//     distinct — canonicalization normalizes layout, not numeric value);
//   - strings re-escaped by encoding/json, so equivalent escape spellings
//     (é vs é) converge.
//
// Empty input canonicalizes as null; trailing non-whitespace after the
// document is an error.
func CanonicalJSON(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		trimmed = []byte("null")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("audit: canonical json: %w", err)
	}
	if dec.More() {
		return nil, errors.New("audit: canonical json: trailing data after document")
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		// The decoder guarantees the literal is a valid JSON number;
		// keeping it verbatim avoids float round-trip drift.
		buf.WriteString(t.String())
	case string:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Errorf("audit: canonical json: %w", err)
		}
		buf.Write(b)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := slices.Sorted(maps.Keys(t))
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return fmt.Errorf("audit: canonical json: %w", err)
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("audit: canonical json: unexpected type %T", v)
	}
	return nil
}
