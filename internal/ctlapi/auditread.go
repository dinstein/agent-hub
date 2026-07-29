package ctlapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/audit"
)

// GET /v1/audit and GET /v1/security backfill the two governance streams for
// a frontend (docs/modules/controlplane.md). They are the two routes the
// api client has been calling into a 404 since M1-G: `agenthub audit tail`
// reads the JSONL files directly, which a GUI must not do — docs/modules/controlplane.md
// forbids it from touching the data directory at all.
//
// Tail is the BACKFILL, not the feed: live records arrive on the `activity`
// SSE topic. So this reads the ACTIVE segment only, exactly like the CLI's
// default `audit tail`; rotated segments are the CLI's --all-segments
// territory and a frontend that wants history has the export path.
//
// Args red line (docs/modules/foundation.md): a record carries argsHash only. There are
// no argument bytes to leak here because audit.Record has no field for them
// — a type-level guarantee, not a filter this handler applies.
//
// Read discipline: the file is opened READ-ONLY and never truncated or
// renamed, so this cannot disturb the multi-writer append discipline that N
// gateways plus the daemon rely on.

// auditMaxLine bounds one JSONL line. The writer bounds what it appends; a
// longer line means a foreign or corrupt file, not a record.
const auditMaxLine = 1 << 20

// handleAuditTail implements GET /v1/audit and GET /v1/security.
//
// stream selects the file: "" and "audit" the call ledger, "security" the
// guard events. /v1/security passes the stream itself, so the two routes are
// one implementation and cannot answer differently.
func (s *Server) handleAuditTail(w http.ResponseWriter, r *http.Request, stream string) {
	reqID := requestIDFrom(r.Context())
	if s.opts.LogsDir == "" {
		// Not wired: the same "this daemon does not serve it" shape a
		// frontend already handles, never an empty list that would read as
		// "nothing has been logged".
		writeNotFound(w, r)
		return
	}
	limit, err := tailLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(), "", reqID)
		return
	}
	switch stream {
	case "", api.AuditStreamAudit:
		out := []audit.Record{}
		if err := s.tailStream(audit.AuditFileName, limit, &out); err != nil {
			s.writeOpsError(w, r, err)
			return
		}
		writeOK(w, http.StatusOK, out)
	case api.AuditStreamSecurity:
		out := []audit.SecurityEvent{}
		if err := s.tailStream(audit.SecurityFileName, limit, &out); err != nil {
			s.writeOpsError(w, r, err)
			return
		}
		writeOK(w, http.StatusOK, out)
	default:
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "unknown stream "+stream,
			"valid streams: audit, security", reqID)
	}
}

// tailLimit parses and clamps ?limit. Absent or 0 selects the daemon
// default. A client-side clamp is not a trusted bound, so the ceiling is
// re-applied here.
func tailLimit(raw string) (int, error) {
	if raw == "" {
		return defaultAuditTail, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("limit must be a non-negative number, got %q", raw)
	}
	if n == 0 {
		return defaultAuditTail, nil
	}
	return min(n, maxAuditTail), nil
}

// tailStream decodes the last `limit` records of one JSONL stream into out
// (a pointer to a slice of the record type).
//
// Failure direction, in two halves that must not be confused:
//
//   - a MISSING file is an empty tail — nothing has been logged yet;
//   - an undecodable LINE is counted and skipped, because a crashed writer's
//     torn last line must not make the whole log unreadable;
//   - an I/O error is an error, never a short read rendered as "quiet".
func (s *Server) tailStream(name string, limit int, out any) error {
	path := filepath.Join(s.opts.LogsDir, name)
	lines, skipped, err := tailLines(path, limit)
	if err != nil {
		return err
	}
	if skipped > 0 {
		s.log.Warn("ctlapi: skipped undecodable audit lines", "path", path, "skipped", skipped)
	}
	if len(lines) == 0 {
		return nil
	}
	// One array decode over the kept lines: the caller's slice type decides
	// how each record is shaped, so this function stays stream-agnostic.
	buf := make([]byte, 0, 2+len(lines))
	buf = append(buf, '[')
	for i, l := range lines {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, l...)
	}
	buf = append(buf, ']')
	return json.Unmarshal(buf, out)
}

// tailLines returns the last `limit` well-formed JSON lines of path, oldest
// first, plus how many lines were undecodable.
//
// Well-formedness is checked with json.Valid rather than a full decode:
// keeping the raw bytes lets the caller decode once, into its own type,
// without this function knowing either.
func tailLines(path string, limit int) ([][]byte, int, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer func() { _ = file.Close() }()

	// Circular buffer of the last `limit` lines: the stream is append-only
	// and unbounded, so the tail is taken in one pass without ever holding
	// the whole file — and without the O(kept) shift a naive slice would
	// pay on every line of a long log.
	ring := make([][]byte, limit)
	kept := 0
	skipped := 0
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64<<10), auditMaxLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			skipped++
			continue
		}
		ring[kept%limit] = []byte(line)
		kept++
	}
	if err := sc.Err(); err != nil {
		return nil, skipped, err
	}
	if kept > limit {
		// Unwrap the ring oldest-first: the tail is chronological, like the
		// file it came from.
		out := make([][]byte, 0, limit)
		for i := kept - limit; i < kept; i++ {
			out = append(out, ring[i%limit])
		}
		return out, skipped, nil
	}
	return ring[:kept], skipped, nil
}
