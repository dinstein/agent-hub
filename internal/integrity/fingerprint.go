package integrity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// HashSchemaVersion is the current fingerprint formula version. It prefixes
// every fingerprint ("v1:<hex>") and is stored alongside each pin/approval
// record so a future formula change can be recognized and bridged by content
// comparison instead of presenting as a fleet-wide fake rug-pull (7.5 —
// roughly half of mcpproxy's quarantine code existed to clean up after
// exactly that mistake).
const HashSchemaVersion = "v1"

// ToolSnapshot is the fingerprint input and the diff-review payload: the
// identity-bearing parts of a tool definition as served by a downstream.
//
// Annotations and title are deliberately absent: they are unstable across
// reconnects and including them caused false-change storms. Annotation
// downgrades are tracked separately by the drift severity layer, not by the
// fingerprint.
type ToolSnapshot struct {
	// Name is the RAW downstream tool name (rename-proof key; exposed names
	// are a router concern).
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// Fingerprint computes the versioned fingerprint of a tool definition.
//
// Formula (v1): "v1:" + hex(sha256(canonicalJSON({name, description,
// inputSchema}))). The input schema is canonicalized — object keys sorted at
// every level, no insignificant whitespace, numbers preserved verbatim — so
// key-order and formatting jitter from downstream re-serialization never
// reads as drift, while any semantic change does.
//
// Fail direction: an unparsable inputSchema returns an error (fail-closed).
// Callers must treat the tool as un-fingerprintable and keep it blocked —
// never as "matches its pin".
func Fingerprint(s ToolSnapshot) (string, error) {
	schema, err := canonicalSchemaValue(s.InputSchema)
	if err != nil {
		return "", fmt.Errorf("integrity: fingerprint %q: %w", s.Name, err)
	}
	payload, err := json.Marshal(map[string]any{
		"name":        s.Name,
		"description": s.Description,
		"inputSchema": schema,
	})
	if err != nil {
		return "", fmt.Errorf("integrity: fingerprint %q: %w", s.Name, err)
	}
	sum := sha256.Sum256(payload)
	return HashSchemaVersion + ":" + hex.EncodeToString(sum[:]), nil
}

// canonicalSchemaValue decodes raw JSON into a value whose re-marshaling is
// canonical: maps marshal with sorted keys, json.Number preserves the exact
// source form of numbers ("1" vs "1.0" stay distinct). A missing/empty
// schema canonicalizes to nil (JSON null).
func canonicalSchemaValue(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid inputSchema JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid inputSchema JSON: trailing data")
	}
	return v, nil
}

// schemasEqual reports whether two raw schemas encode the same JSON value.
// Fail direction: malformed input compares unequal (a schema we cannot parse
// must surface as a change, never be silently equated — fail-closed).
func schemasEqual(a, b json.RawMessage) bool {
	va, err := canonicalSchemaValue(a)
	if err != nil {
		return false
	}
	vb, err := canonicalSchemaValue(b)
	if err != nil {
		return false
	}
	ca, err := json.Marshal(va)
	if err != nil {
		return false
	}
	cb, err := json.Marshal(vb)
	if err != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}
