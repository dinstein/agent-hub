package accesslog

import (
	"errors"
	"time"
)

const (
	// Version is the event and pack format version.
	Version = 1
	// DirectoryName is the access-ledger directory below the data directory.
	DirectoryName = "audit"
	// EventFileName is the shared bounded metadata stream inside one UTC day.
	EventFileName = "access.jsonl"
	// MaxEventLineBytes preserves the multi-process one-write line contract.
	MaxEventLineBytes = 4096
	// DefaultMaxPackBytes rotates a process-owned payload pack at 64 MiB.
	DefaultMaxPackBytes = 64 << 20
	// MaxPayloadBytes matches the MCP frame bound. Complete means every byte
	// of an accepted frame, not an unbounded allocation outside the protocol.
	MaxPayloadBytes = 16 << 20
)

var (
	ErrClosed        = errors.New("accesslog: store is closed")
	ErrEventTooLarge = errors.New("accesslog: event exceeds line bound")
	ErrBadKey        = errors.New("accesslog: encryption key must be 32 bytes")
	ErrBadReference  = errors.New("accesslog: invalid payload reference")
	ErrPayloadTooBig = errors.New("accesslog: payload exceeds accepted MCP frame bound")
	ErrCapacity      = errors.New("accesslog: storage limit reached")
	ErrFreeReserve   = errors.New("accesslog: free-space reserve reached")
	ErrExpired       = errors.New("accesslog: event is outside the retention window")
)

// Durability controls when a write is acknowledged.
type Durability string

const (
	// DurabilityWrite waits for the kernel write but not an fsync.
	DurabilityWrite Durability = "write"
	// DurabilitySync fsyncs payload and event files before returning.
	DurabilitySync Durability = "sync"
)

// EventKind names one immutable point in a call's lifecycle.
type EventKind string

const (
	EventReceived EventKind = "received"
	EventRouted   EventKind = "routed"
	EventFinished EventKind = "finished"
)

// PayloadKind identifies bytes stored in a payload pack.
type PayloadKind string

const (
	PayloadRequest       PayloadKind = "request"
	PayloadEffectiveArgs PayloadKind = "effective_arguments"
	PayloadResult        PayloadKind = "result"
)

// PayloadRef points to one encrypted entry in one process-owned pack.
type PayloadRef struct {
	Day         string `json:"day"`
	File        string `json:"file"`
	Offset      int64  `json:"offset"`
	Length      int64  `json:"length"`
	RawBytes    int    `json:"rawBytes"`
	StoredBytes int    `json:"storedBytes"`
	KeyID       string `json:"keyId"`
}

// Event is one bounded line in access.jsonl. Fields that do not apply to a
// lifecycle point are omitted; readers join events by CallID.
type Event struct {
	Version int       `json:"v"`
	TS      time.Time `json:"ts"`
	Kind    EventKind `json:"event"`
	CallID  string    `json:"callId"`
	KeyID   string    `json:"keyId"`
	MAC     string    `json:"mac"`

	Client     string `json:"client,omitempty"`
	Session    string `json:"session,omitempty"`
	PID        int    `json:"pid"`
	BootID     string `json:"bootId"`
	RequestID  string `json:"requestId,omitempty"`
	Face       string `json:"face,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	PolicyRev  uint64 `json:"policyRev,omitempty"`
	Exposed    string `json:"exposedTool,omitempty"`
	Server     string `json:"server,omitempty"`
	Tool       string `json:"tool,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Instance   string `json:"inst,omitempty"`
	CallerTier string `json:"callerTier,omitempty"`

	Outcome    string `json:"outcome,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Gate       string `json:"gate,omitempty"`
	Rule       string `json:"rule,omitempty"`
	Code       string `json:"code,omitempty"`
	Error      string `json:"error,omitempty"`
	ToolError  bool   `json:"toolError,omitempty"`

	Request       *PayloadRef `json:"request,omitempty"`
	EffectiveArgs *PayloadRef `json:"effectiveArguments,omitempty"`
	Result        *PayloadRef `json:"result,omitempty"`
	ResultMode    string      `json:"resultMode,omitempty"`
	ResultCapture string      `json:"resultCapture,omitempty"`
	ResultBytes   int         `json:"resultBytes,omitempty"`
	ResultCut     bool        `json:"resultTruncated,omitempty"`
}

// Options configures an access store. Root is normally <data>/audit.
type Options struct {
	Root          string
	Key           []byte
	KeyID         string
	Durability    Durability
	MaxPackBytes  int64
	RetentionDays int
	MaxBytes      int64
	MinFreeBytes  int64
	Clock         func() time.Time
}
