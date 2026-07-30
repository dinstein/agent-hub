// Package savings is the token-savings ledger: one JSONL line per shaped or
// discovery-assisted interaction, aggregated by `agenthub activity`.
//
// It is accounting, not governance. Nothing here decides anything about a
// call — it records what a call cost against what it would have cost — which
// is why it outlived the governance streams it used to sit beside.
//
// Dependency budget: standard library plus internal/jsonl.
package savings

import (
	"time"

	"github.com/dinstein/agent-hub/internal/jsonl"
)

// FileName is the ledger's file name under <data>/logs.
const FileName = "savings.jsonl"

// Record is one savings.jsonl line: a token-savings estimate for one
// shaped/discovery-assisted interaction.
//
// Field order is frozen (golden-tested).
type Record struct {
	// TS is the record time (UTC).
	TS time.Time `json:"ts"`
	// Client and Session identify who saved (optional, for grouping).
	Client  string `json:"client,omitempty"`
	Session string `json:"session,omitempty"`
	// Server is the downstream server involved (optional).
	Server string `json:"server,omitempty"`
	// Mode names the mechanism that produced the saving
	// (e.g. "lazy-discovery", "grouped", "shaping", "toon").
	Mode string `json:"mode"`
	// BaselineTokens estimates the tokens a full/unshaped exchange would
	// have cost.
	BaselineTokens int64 `json:"baselineTokens"`
	// ActualTokens estimates the tokens actually spent.
	ActualTokens int64 `json:"actualTokens"`
	// SavedTokens = BaselineTokens - ActualTokens (recorded explicitly so
	// consumers never re-derive it inconsistently).
	SavedTokens int64 `json:"savedTokens"`
}

// Stream appends Records to savings.jsonl.
type Stream struct {
	w     *jsonl.Writer
	clock func() time.Time
}

// NewStream opens (creating if needed) the savings stream at path.
func NewStream(path string, opts jsonl.WriterOptions) (*Stream, error) {
	w, err := jsonl.NewWriter(path, opts)
	if err != nil {
		return nil, err
	}
	return &Stream{w: w, clock: w.Clock()}, nil
}

// Append enqueues one record. A zero TS is filled with the stream clock;
// TS is normalized to UTC. Never blocks (drops on backpressure).
func (s *Stream) Append(r Record) {
	if r.TS.IsZero() {
		r.TS = s.clock()
	}
	r.TS = r.TS.UTC()
	s.w.AppendLine(jsonl.MarshalLine(r))
}

// Dropped reports records discarded by writer backpressure.
func (s *Stream) Dropped() uint64 { return s.w.Dropped() }

// Close flushes and closes the underlying writer.
func (s *Stream) Close() error { return s.w.Close() }
