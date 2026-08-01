package accesslog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ReadEvents reads every decodable event from every daily partition. It is
// intentionally offline and never opens a file for writing.
func ReadEvents(root string) ([]Event, int, error) {
	days, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	var events []Event
	skipped := 0
	for _, day := range days {
		if !day.IsDir() || len(day.Name()) != len("2006-01-02") {
			continue
		}
		f, err := os.Open(filepath.Join(root, day.Name(), EventFileName))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, skipped, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), MaxEventLineBytes)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var e Event
			if err := json.Unmarshal([]byte(line), &e); err != nil || e.Version != Version || e.CallID == "" {
				skipped++
				continue
			}
			events = append(events, e)
		}
		if err := sc.Err(); err != nil {
			_ = f.Close()
			return nil, skipped, fmt.Errorf("accesslog: read %s: %w", f.Name(), err)
		}
		if err := f.Close(); err != nil {
			return nil, skipped, err
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].TS.Before(events[j].TS) })
	return events, skipped, nil
}

// ReadPayload decrypts and returns exactly the bytes named by ref.
func ReadPayload(root string, ref PayloadRef, key []byte) ([]byte, error) {
	raw, _, err := readPayload(root, ref, key)
	return raw, err
}
