package ctlapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/dinstein/agent-hub/api"
)

// ctxKey namespaces the request-scoped values this package stores.
type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxActor
)

// requestIDFrom returns the request id stamped by the middleware ("" only
// if the middleware did not run, i.e. never in production).
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// validRequestID bounds what we echo back: header values are attacker-ish
// input even on a same-uid socket, and the id lands in response headers,
// error bodies and audit lines. Anything unverifiable is REPLACED with a
// generated id, never echoed raw.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// newRequestID returns 16 random bytes hex-encoded (crypto/rand.Read
// panics rather than failing since Go 1.24, so there is no error path).
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// actor validates the X-Agenthub-Actor header against the three allowed
// forms of docs/architecture.md §2: "cli", "gui", "gateway:<sid>". Anything else
// (including absence) is recorded as "cli" — the default caller class —
// rather than letting arbitrary strings into audit lines.
func actor(r *http.Request) string {
	v := r.Header.Get(HeaderActor)
	switch {
	case v == "cli" || v == "gui":
		return v
	case strings.HasPrefix(v, "gateway:") && len(v) > len("gateway:") && len(v) <= 200:
		return v
	default:
		return "cli"
	}
}

// statusWriter tracks whether a response has been started, so the panic
// recovery path knows whether a 500 envelope can still be written.
type statusWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *statusWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer
// (the SSE handler needs Flush).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// withMiddleware is the X-Request-Id + version-negotiation + panic-recovery
// wrapper (canonical.md §4):
//
//   - echo-or-generate the request id and write the response header BEFORE
//     the handler runs — a panicking handler cannot lose it;
//   - recover panics into a 500 envelope when the response has not started
//     (when it has, abort the connection instead of appending garbage);
//   - reject incompatible X-Agenthub-Api-Version with a structured error;
//   - stamp request id + validated actor into the request context for
//     handlers and audit records.
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(api.HeaderRequestID)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		// Header first, before any handler code: WriteHeader snapshots the
		// header map, so setting it here guarantees every response —
		// success, error, or recovered panic — carries the id.
		rw.Header().Set(api.HeaderRequestID, id)
		rw.Header().Set(api.HeaderAPIVersion, api.APIVersion)

		w := &statusWriter{ResponseWriter: rw}
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			s.log.Error("ctlapi: handler panic", "panic", p, "path", r.URL.Path, "requestId", id)
			if !w.wrote {
				writeErr(w, http.StatusInternalServerError, CodeInternal, "internal error", "", id)
				return
			}
			// Mid-response panic (e.g. inside an SSE stream): the body is
			// unrecoverable; kill the connection cleanly instead of leaving
			// a half response that parses as truncated success.
			panic(http.ErrAbortHandler)
		}()

		if v := r.Header.Get(api.HeaderAPIVersion); v != "" && v != api.APIVersion {
			writeErr(w, http.StatusBadRequest, CodeAPIVersion,
				"unsupported control API version "+v,
				"this daemon speaks version "+api.APIVersion, id)
			return
		}

		ctx := context.WithValue(r.Context(), ctxRequestID, id)
		ctx = context.WithValue(ctx, ctxActor, actor(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// wireError is the error object inside the envelope. It is
// api.ErrorBody plus the request id (the error body itself
// carries X-Request-Id; clients that only see the body can still correlate
// with `agenthub audit tail --request-id`). The extra field is ignored by
// api.ErrorBody decoding — additive, not a contract break.
type wireError struct {
	api.ErrorBody
	RequestID string `json:"requestId,omitempty"`
	// Generation is the registry generation the daemon is actually at. It
	// is set only by the lost-compare-and-swap 409 (CodeStalePrecondition),
	// where a client that re-read blindly would loop forever.
	Generation uint64 `json:"generation,omitempty"`
}

// wireEnvelope mirrors the api package's envelope shape ({ok,data,error}).
type wireEnvelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error *wireError      `json:"error,omitempty"`
}

// writeOK writes a success envelope. data must marshal (a failure here is a
// programming error and yields a 500).
func writeOK(w http.ResponseWriter, status int, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeInternal, "encoding response", "", "")
		return
	}
	writeJSON(w, status, wireEnvelope{OK: true, Data: raw})
}

// writeErr writes a failure envelope {code,message,hint} (+requestId).
func writeErr(w http.ResponseWriter, status int, code, message, hint, requestID string) {
	writeJSON(w, status, wireEnvelope{OK: false, Error: &wireError{
		ErrorBody: api.ErrorBody{Code: code, Message: message, Hint: hint},
		RequestID: requestID,
	}})
}

// writeErrGen is writeErr plus the current registry generation, used by the
// stale-precondition 409 so the client can retry against a known version.
func writeErrGen(w http.ResponseWriter, status int, code, message, hint, requestID string, generation uint64) {
	writeJSON(w, status, wireEnvelope{OK: false, Error: &wireError{
		ErrorBody:  api.ErrorBody{Code: code, Message: message, Hint: hint},
		RequestID:  requestID,
		Generation: generation,
	}})
}

// writeNotFound writes the single uniform 404 body. Anti-probing invariant:
// the (code, message, hint) triple is identical for every miss — unknown
// route, wrong method, unknown resource id — so a prober learns nothing
// from the shape of the response. Only the per-request id varies.
func writeNotFound(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotFound, CodeNotFound, notFoundMessage, "", requestIDFrom(r.Context()))
}

func writeJSON(w http.ResponseWriter, status int, env wireEnvelope) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}
