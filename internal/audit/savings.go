package audit

import "time"

// SavingsRecord is one savings.jsonl line: a token-savings estimate for
// one shaped/discovery-assisted interaction, aggregated later by the
// `agenthub activity` command.
//
// Field order is frozen (golden-tested).
type SavingsRecord struct {
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

// SavingsStream appends SavingsRecords to savings.jsonl.
type SavingsStream struct {
	w     *Writer
	clock func() time.Time
}

// NewSavingsStream opens (creating if needed) the savings stream at path.
func NewSavingsStream(path string, opts WriterOptions) (*SavingsStream, error) {
	w, err := NewWriter(path, opts)
	if err != nil {
		return nil, err
	}
	return &SavingsStream{w: w, clock: w.clock}, nil
}

// Append enqueues one record. A zero TS is filled with the stream clock;
// TS is normalized to UTC. Never blocks (drops on backpressure).
func (s *SavingsStream) Append(r SavingsRecord) {
	if r.TS.IsZero() {
		r.TS = s.clock()
	}
	r.TS = r.TS.UTC()
	s.w.AppendLine(marshalLine(r))
}

// Dropped reports records discarded by writer backpressure.
func (s *SavingsStream) Dropped() uint64 { return s.w.Dropped() }

// Close flushes and closes the underlying writer.
func (s *SavingsStream) Close() error { return s.w.Close() }
