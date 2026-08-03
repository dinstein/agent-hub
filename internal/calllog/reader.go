package calllog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ReadEvents reads every decodable event from every daily partition. It is
// intentionally offline and never opens a file for writing.
func ReadEvents(root string) ([]Event, int, error) {
	var events []Event
	skipped, err := ScanEvents(root, func(e Event) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		return nil, skipped, err
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].TS.Before(events[j].TS) })
	return events, skipped, nil
}

// ScanEvents visits decodable events in daily/file order without retaining
// the whole ledger in memory. A malformed line is counted and skipped; an I/O
// error or callback error stops the scan.
func ScanEvents(root string, visit func(Event) error) (int, error) {
	return ScanEventsSince(root, time.Time{}, visit)
}

// ScanEventsSince skips complete UTC day partitions older than since and
// filters boundary-day events by timestamp.
func ScanEventsSince(root string, since time.Time, visit func(Event) error) (int, error) {
	days, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	skipped := 0
	cutoffDay := ""
	if !since.IsZero() {
		cutoffDay = since.UTC().Format("2006-01-02")
	}
	for _, day := range days {
		if !day.IsDir() || len(day.Name()) != len("2006-01-02") {
			continue
		}
		if cutoffDay != "" && day.Name() < cutoffDay {
			continue
		}
		f, err := openDayEvents(root, day.Name())
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return skipped, err
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
			if !since.IsZero() && e.TS.Before(since) {
				continue
			}
			if visit != nil {
				if err := visit(e); err != nil {
					_ = f.Close()
					return skipped, err
				}
			}
		}
		if err := sc.Err(); err != nil {
			_ = f.Close()
			return skipped, fmt.Errorf("calllog: read %s: %w", f.Name(), err)
		}
		if err := f.Close(); err != nil {
			return skipped, err
		}
	}
	return skipped, nil
}

// ReadPayload decrypts and returns exactly the bytes named by ref.
func ReadPayload(root string, ref PayloadRef, key []byte) ([]byte, error) {
	raw, _, err := readPayload(root, ref, key)
	return raw, err
}

// VerifyPayload authenticates a referenced entry and its call/kind binding.
func VerifyPayload(root string, ref PayloadRef, key []byte, callID string, kind PayloadKind) error {
	_, header, err := readPayload(root, ref, key)
	if err != nil {
		return err
	}
	if header.CallID != callID || header.Kind != kind {
		return fmt.Errorf("calllog: payload binding is %s/%s, want %s/%s", header.CallID, header.Kind, callID, kind)
	}
	return nil
}

// openDayEvents opens one day's metadata stream, falling back to the name it
// had before the ledger was renamed.
//
// The fallback is a READ path only. A day written by an older build keeps the
// file name it was written with — rewriting history to make it look current
// is exactly what an authenticated ledger must not do — so both names are
// readable forever and only new days get the current one.
func openDayEvents(root, day string) (*os.File, error) {
	f, err := os.Open(filepath.Join(root, day, EventFileName))
	if err == nil || !os.IsNotExist(err) {
		return f, err
	}
	return os.Open(filepath.Join(root, day, LegacyEventFileName))
}
