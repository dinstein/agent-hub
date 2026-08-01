package accesslog

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

const (
	accessHelperMode = "AGENTHUB_ACCESSLOG_TEST_HELPER"
	accessHelperRoot = "AGENTHUB_ACCESSLOG_TEST_ROOT"
	accessHelperID   = "AGENTHUB_ACCESSLOG_TEST_ID"
	accessHelperN    = "AGENTHUB_ACCESSLOG_TEST_N"
	accessHelperKey  = "AGENTHUB_ACCESSLOG_TEST_KEY"
)

func TestMain(m *testing.M) {
	if os.Getenv(accessHelperMode) == "append" {
		helperAppendEvents()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helperAppendEvents() {
	root, id := os.Getenv(accessHelperRoot), os.Getenv(accessHelperID)
	n, err := strconv.Atoi(os.Getenv(accessHelperN))
	if err != nil || root == "" || id == "" {
		fmt.Fprintln(os.Stderr, "bad helper environment")
		os.Exit(2)
	}
	key, err := hex.DecodeString(os.Getenv(accessHelperKey))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	keyID, err := KeyID(key)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	s, err := Open(Options{Root: root, Key: key, KeyID: keyID, Durability: DurabilitySync})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ts := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		if err := s.Append(Event{TS: ts, Kind: EventReceived, CallID: fmt.Sprintf("%s-%04d", id, i), Client: id}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if err := s.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func TestMetadataAppendIsWholeAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	const processes, records = 4, 75
	commands := make([]*exec.Cmd, processes)
	outputs := make([]*bytes.Buffer, processes)
	for i := range commands {
		outputs[i] = &bytes.Buffer{}
		id := fmt.Sprintf("p%d", i)
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(),
			accessHelperMode+"=append",
			accessHelperRoot+"="+root,
			accessHelperID+"="+id,
			accessHelperN+"="+strconv.Itoa(records),
			accessHelperKey+"="+hex.EncodeToString(testKey()),
		)
		cmd.Stdout = outputs[i]
		cmd.Stderr = outputs[i]
		commands[i] = cmd
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper failed: %v\n%s", err, outputs[i].Bytes())
		}
	}
	events, skipped, err := ReadEvents(root)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Fatalf("%d torn or undecodable lines", skipped)
	}
	if len(events) != processes*records {
		t.Fatalf("events = %d, want %d", len(events), processes*records)
	}
}
