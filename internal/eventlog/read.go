package eventlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Reading side. The rule that matters here is the one the retired savings
// projection got wrong: a reader that opens only the ACTIVE file silently
// under-reports everything rotation moved aside, and the symptom is a report
// that looks like "nothing happened" rather than an error.
//
// jsonl.Writer rotates by renaming <base>.jsonl to
// <base>-<RFC3339-ish stamp>.p<pid>.jsonl, so segments sort chronologically
// by name and the active file is always newest.

// maxLineBytes bounds one line while reading. The writer bounds what it
// appends; a longer line means a foreign or corrupt file.
const maxLineBytes = 1 << 20

// Query narrows a read. The zero value reads everything.
type Query struct {
	// Since drops records older than this instant.
	Since time.Time
	// Scope, Server, Client and Kinds are exact-match narrowings. Empty
	// means "no rule" — never "match nothing" — which is the same nil-vs-
	// empty distinction the tool selectors make.
	Scope  Scope
	Server string
	Client string
	Kinds  []Kind
}

func (q Query) admit(r Record) bool {
	if !q.Since.IsZero() && r.TS.Before(q.Since) {
		return false
	}
	if q.Scope != "" && r.Scope != q.Scope {
		return false
	}
	if q.Server != "" && r.Server != q.Server {
		return false
	}
	if q.Client != "" && r.Client != q.Client {
		return false
	}
	if len(q.Kinds) > 0 && !slices.Contains(q.Kinds, r.Kind) {
		return false
	}
	return true
}

// Result is what a read returns.
type Result struct {
	Records []Record
	// Skipped counts undecodable lines: a torn tail from a killed writer, a
	// foreign file. Counted, never silently dropped — a reader that says
	// nothing about them cannot distinguish an empty stream from a corrupt
	// one.
	Skipped int
	// Files is every path that was read, oldest segment first. It is
	// returned so a caller can say WHICH files a report covers; "no records"
	// over three segments is a different fact from "no records" over none.
	Files []string
}

// Read returns every admitted record from every segment plus the active
// file, oldest first.
//
// A missing file is not an error: nothing has been recorded yet is a normal
// state, and it is the state a fresh installation is in.
func Read(path string, q Query) (Result, error) {
	var out Result
	for _, f := range Segments(path) {
		records, skipped, err := readFile(f, q)
		if err != nil {
			return Result{}, err
		}
		if records == nil && skipped == 0 && !fileExists(f) {
			continue
		}
		out.Files = append(out.Files, f)
		out.Records = append(out.Records, records...)
		out.Skipped += skipped
	}
	// Segments are chronological by name, but two processes rotating in the
	// same instant, or a clock step, can put a record out of order across
	// the boundary. Sorting is cheap and makes the result mean one thing.
	slices.SortStableFunc(out.Records, func(a, b Record) int { return a.TS.Compare(b.TS) })
	return out, nil
}

// Segments lists the stream's files oldest first, active file last.
//
// It is exported because it is the answer to "which files does this stream
// occupy", which both the reader and any future prune need, and because a
// caller that composed the glob itself would be the second place the naming
// scheme lives.
func Segments(path string) []string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	matches, err := filepath.Glob(base + "-*" + ext)
	if err != nil {
		matches = nil
	}
	// The stamp is fixed-width and zero-padded, so lexical order is
	// chronological order.
	slices.Sort(matches)
	return append(matches, path)
}

// pruneSegments deletes all but the newest keep rotated segments. The active
// file is never touched.
//
// Failure to remove one is ignored on purpose: another process may have
// pruned it already, and a retention sweep that could fail an Open would
// make "the disk is briefly busy" into "this gateway does not start".
func pruneSegments(path string, keep int) {
	all := Segments(path)
	segments := all[:len(all)-1] // drop the active file
	if len(segments) <= keep {
		return
	}
	for _, old := range segments[:len(segments)-keep] {
		_ = os.Remove(old)
	}
}

func readFile(path string, q Query) ([]Record, int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	records, skipped, err := decodeRecords(f, q)
	if err != nil {
		return nil, skipped, fmt.Errorf("read %s: %w", path, err)
	}
	return records, skipped, nil
}

func decodeRecords(r io.Reader, q Query) ([]Record, int, error) {
	var (
		out     []Record
		skipped int
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec Record
		if json.Unmarshal([]byte(line), &rec) != nil {
			skipped++
			continue
		}
		// An oversize marker shares the "ts" field with a record, so it
		// unmarshals cleanly into a Record with no kind — a blank row
		// claiming nothing happened. Count it as skipped instead. Detail is
		// bounded on write specifically so this should never fire.
		if rec.Kind == "" {
			skipped++
			continue
		}
		if !q.admit(rec) {
			continue
		}
		out = append(out, rec)
	}
	return out, skipped, sc.Err()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
