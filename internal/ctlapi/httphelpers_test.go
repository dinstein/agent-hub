package ctlapi

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

// postJSON is the unix-socket request helper shared by the
// control-plane tests. They lived in the grants test file until grants and
// human approval were removed; nothing about them was grant-specific.

func postJSON(t *testing.T, sock, path string, body any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rawClient(sock).Post("http://d"+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}
