// Package proclog reads the hub's PROCESS logs — daemon.log and every
// gateway-<client>.log — as one time-ordered stream.
//
// It exists because two callers need the same answer. `agenthub logs` reads
// these files directly and offline; the control plane serves them to the GUI,
// which cannot read a file at all. Before this package the CLI held the only
// implementation, so the GUI had no view of the half of the record that
// matters most: the daemon never dials a downstream, and every connection
// failure, circuit transition, health flip and respawn is therefore observed
// and written by a gateway.
//
// It is READ-ONLY by construction. Nothing here opens a file for writing, so
// serving a page can never disturb the multi-writer discipline the gateways
// and the daemon depend on (internal/jsonl).
//
// Dependency budget: the standard library plus internal/jsonl (the segment
// naming), internal/logx (the field names) and internal/platform.
package proclog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dinstein/agent-hub/internal/jsonl"
	"github.com/dinstein/agent-hub/internal/logx"
)

const (
	// MaxLine bounds one line while reading. The writer bounds what it
	// appends (jsonl.DefaultMaxLineBytes); a longer line means a foreign or
	// corrupt file.
	MaxLine = 1 << 20
	// DaemonFileName is the daemon's own log.
	DaemonFileName = "daemon.log"
	// GatewayPrefix and GatewayExt bracket a gateway's log name. The client
	// id sits between them, sanitized by the writer.
	GatewayPrefix = "gateway-"
	GatewayExt    = ".log"
)

// Origin names which kind of process produced a record. It is the one piece
// of provenance the FILE carries and the record does not.
type Origin string

const (
	OriginDaemon  Origin = "daemon"
	OriginGateway Origin = "gateway"
)

// Origins lists the sources a caller may narrow to, in presentation order.
func Origins() []string { return []string{string(OriginDaemon), string(OriginGateway)} }

// Record is one parsed line plus where it came from.
type Record struct {
	Origin Origin `json:"origin"`
	// Raw is the line verbatim. It is what a --json consumer emits: the file
	// is already machine-readable and re-encoding could only lose fields.
	Raw string `json:"raw"`
	// TS/Level are the parsed sort and filter keys; the *OK flags say whether
	// the parse succeeded, because a filter must not admit a line it cannot
	// classify.
	TS      time.Time      `json:"ts"`
	TSOK    bool           `json:"tsOk"`
	Level   slog.Level     `json:"-"`
	LevelOK bool           `json:"levelOk"`
	Text    string         `json:"level"`
	Msg     string         `json:"msg"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// Field returns one string field, or "".
func (r Record) Field(key string) string {
	s, _ := r.Fields[key].(string)
	return s
}

// Query narrows a read. The zero value reads everything.
//
// Every selector is exact-match and empty means "no rule", never "match
// nothing" — the same nil-vs-empty distinction every other reader here makes.
type Query struct {
	// Since and Until bound the window. Zero means unbounded on that side.
	Since, Until time.Time
	// MinLevel drops quieter records when Leveled is set. It is a separate
	// flag because slog.LevelDebug is 0 and therefore indistinguishable from
	// "no rule" on its own.
	MinLevel slog.Level
	Leveled  bool
	// Source narrows to one kind of process ("" = both).
	Source Origin
	// Client and Server narrow by the record's own fields.
	Client, Server string
}

// Admit reports whether one record passes.
//
// Failure direction: when a selector is active and the field is ABSENT, the
// record is dropped. A daemon record carries no client, so `client=x` showing
// daemon lines would be a filtered view smuggling in records it cannot
// classify.
func (q Query) Admit(r Record) bool {
	if !q.Since.IsZero() {
		if !r.TSOK || r.TS.Before(q.Since) {
			return false
		}
	}
	if !q.Until.IsZero() {
		if !r.TSOK || !r.TS.Before(q.Until) {
			return false
		}
	}
	if q.Leveled {
		if !r.LevelOK || r.Level < q.MinLevel {
			return false
		}
	}
	if q.Source != "" && r.Origin != q.Source {
		return false
	}
	if q.Client != "" && r.Field(logx.FieldClient) != q.Client {
		return false
	}
	if q.Server != "" && r.Field(logx.FieldServer) != q.Server {
		return false
	}
	return true
}

// File is one openable source.
type File struct {
	Path   string
	Origin Origin
}

// Files lists everything the query can possibly match, oldest segment first
// within each stream.
//
// A missing file is simply absent: a daemon that has never run, or a client
// that has never connected, is a normal state and not an error.
//
// Client narrows by FILE NAME here, which is a superset (the writer's name is
// many-to-one), and the exact match happens per record in Admit. Doing only
// one of the two would be wrong in a different direction each way: the name
// alone can over-select, and the field alone would read every file.
func Files(logsDir string, q Query) ([]File, error) {
	var out []File
	if q.Source != OriginGateway && q.Client == "" {
		out = appendStream(out, filepath.Join(logsDir, DaemonFileName), OriginDaemon)
	}
	if q.Source == OriginDaemon {
		return out, nil
	}
	if q.Client != "" {
		return appendStream(out, GatewayPath(logsDir, q.Client), OriginGateway), nil
	}
	matches, err := filepath.Glob(filepath.Join(logsDir, GatewayPrefix+"*"+GatewayExt))
	if err != nil {
		return nil, fmt.Errorf("proclog: list gateway logs in %s: %w", logsDir, err)
	}
	slices.Sort(matches)
	for _, path := range matches {
		// The glob matches rotated segments too. Each is read as part of the
		// stream it belongs to, so taking one for a stream of its own would
		// list its records twice.
		if jsonl.IsSegment(path) {
			continue
		}
		out = appendStream(out, path, OriginGateway)
	}
	return out, nil
}

// appendStream adds one stream's files — rotated segments oldest first, then
// the active file — skipping those that do not exist.
//
// Reading only the active file is the mistake this exists to prevent: the
// process logs rotate, so a `--since 24h` that opened just the newest file
// would answer "nothing happened" for everything rotation moved aside.
func appendStream(out []File, active string, origin Origin) []File {
	for _, path := range jsonl.Segments(active) {
		if _, err := os.Stat(path); err == nil {
			out = append(out, File{Path: path, Origin: origin})
		}
	}
	return out
}

// GatewayPath is the log file of one client's gateway. It mirrors the
// writer's own naming, which internal/gateway owns; the sanitizing rule is
// duplicated nowhere — this only joins the pieces.
func GatewayPath(logsDir, clientID string) string {
	return filepath.Join(logsDir, GatewayPrefix+sanitize(clientID)+GatewayExt)
}

// sanitize maps a client id to a safe file-name element. It matches
// gateway.LogPath; the two are checked against each other by a test there,
// because a reader that sanitized differently would look in the wrong place
// and report "no records" for a client that has been logging all day.
func sanitize(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		return "_"
	}
	return out
}

// Read returns every admitted record from every matching file, oldest first.
//
// It is the whole-window read the control plane serves and the CLI's first
// batch: a caller that wants a tail takes the last N, and one that wants a
// page takes a slice. Following is the CLI's own business (offsets per file),
// because a request/response API has nowhere to keep them.
func Read(logsDir string, q Query) ([]Record, error) {
	files, err := Files(logsDir, q)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, f := range files {
		records, err := ReadFrom(f, 0, q)
		if err != nil {
			return nil, err
		}
		out = append(out, records...)
	}
	// Stable, so records sharing a timestamp keep file order rather than
	// shuffling between two reads of the same data.
	slices.SortStableFunc(out, func(x, y Record) int { return x.TS.Compare(y.TS) })
	return out, nil
}

// ReadFrom decodes one file's admitted records from offset to EOF.
//
// An unparseable line is DROPPED rather than counted or shown. That differs
// from the frame reader, which counts them, and the reason is the merge: a
// line that does not parse has no timestamp, so there is no position in a
// merged stream where showing it would be truthful.
func ReadFrom(f File, offset int64, q Query) ([]Record, error) {
	file, err := os.Open(f.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // rotated away between the listing and the open
		}
		return nil, fmt.Errorf("proclog: open %s: %w", f.Path, err)
	}
	defer func() { _ = file.Close() }()
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	var out []Record
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		rec, ok := Parse(line, f.Origin)
		if !ok || !q.Admit(rec) {
			continue
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("proclog: read %s: %w", f.Path, err)
	}
	return out, nil
}

// Parse decodes one slog JSON line. It reports false for anything that is not
// a JSON object, which is the only shape logx writes.
func Parse(line string, origin Origin) (Record, bool) {
	var fields map[string]any
	if json.Unmarshal([]byte(line), &fields) != nil {
		return Record{}, false
	}
	rec := Record{Origin: origin, Raw: line, Fields: fields}
	if s, ok := fields["time"].(string); ok {
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			rec.TS, rec.TSOK = ts, true
		}
	}
	if s, ok := fields["level"].(string); ok {
		rec.Text = s
		rec.LevelOK = rec.Level.UnmarshalText([]byte(s)) == nil
	}
	rec.Msg, _ = fields["msg"].(string)
	return rec, true
}

// Size reports a file's size, treating a missing file as zero. Callers use it
// to detect rotation: a file that SHRANK was rotated away or truncated, so a
// follower restarts at the top of the new one.
func Size(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
