package calllog

import (
	"errors"
	"time"
)

const (
	// Version is the event and pack format version.
	Version = 1
	// DirectoryName is the call ledger's directory below the data directory.
	DirectoryName = "calls"
	// LegacyDirectoryName is what it was called while the ledger recorded
	// nothing but tools/call evidence. DefaultDir renames one to the other,
	// once — see migrateLegacyDir.
	LegacyDirectoryName = "audit"
	// EventFileName is the shared bounded metadata stream inside one UTC day.
	EventFileName = "calls.jsonl"
	// FramePrefix names the per-process frame stream inside one UTC day:
	// frames-<bootid>-p<pid>.jsonl.
	//
	// Frames do NOT share calls.jsonl, and the reason is the reason that file
	// has a 4096-byte line bound at all: PIPE_BUF is what makes one line from
	// one of N gateway processes atomic, and it exists because that file is
	// SHARED. Frames outnumber lifecycle records by one to two orders of
	// magnitude and are process-local by nature, so putting them there would
	// make a debugging switch the hot path of the file that decides whether a
	// call may run. One schema, one reader, one retention policy; only the
	// contention is split.
	FramePrefix = "frames-"
	// FrameExt completes that name.
	FrameExt = ".jsonl"
	// LegacyEventFileName is its previous name. A day directory holding one
	// is read, never rewritten: history is not ours to restate.
	LegacyEventFileName = "access.jsonl"
	// MaxEventLineBytes preserves the multi-process one-write line contract.
	MaxEventLineBytes = 4096
	// DefaultMaxPackBytes rotates a process-owned payload pack at 64 MiB.
	DefaultMaxPackBytes = 64 << 20
	// MaxPayloadBytes matches the MCP frame bound. Complete means every byte
	// of an accepted frame, not an unbounded allocation outside the protocol.
	MaxPayloadBytes = 16 << 20
)

var (
	ErrClosed        = errors.New("calllog: store is closed")
	ErrEventTooLarge = errors.New("calllog: event exceeds line bound")
	ErrBadKey        = errors.New("calllog: encryption key must be 32 bytes")
	ErrNoKey         = errors.New("calllog: this store records metadata only; no encryption key is configured")
	ErrBadReference  = errors.New("calllog: invalid payload reference")
	ErrPayloadTooBig = errors.New("calllog: payload exceeds accepted MCP frame bound")
	ErrCapacity      = errors.New("calllog: storage limit reached")
	ErrFreeReserve   = errors.New("calllog: free-space reserve reached")
	ErrExpired       = errors.New("calllog: event is outside the retention window")
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
	// The three lifecycle points of one upstream request, at the boundary
	// with the CLIENT.
	EventReceived EventKind = "received"
	EventRouted   EventKind = "routed"
	EventFinished EventKind = "finished"
	// The two directions of one frame at the boundary with a DOWNSTREAM.
	//
	// They are in the same vocabulary as the three above because they are the
	// same story sampled more finely: `routed` says which server was chosen,
	// `sent` says what actually went on the wire, and one `routed` can be
	// followed by three `sent`/`recv` pairs when the connection died twice on
	// the way. Two separate streams could not express that at all — which is
	// what the per-server trace log could not, having no call id in it.
	EventSent EventKind = "sent"
	EventRecv EventKind = "recv"
)

// Cause says why a frame crossed the boundary. It is the answer for the
// frames that belong to no client call, which are not an exception: a health
// probe, a tools refresh and a token replay are all traffic somebody has to
// account for when they are reading a server's conversation.
type Cause string

const (
	// CauseCall is a frame carrying an upstream client's call. CallID is set.
	CauseCall Cause = "call"
	// CauseList is a tools/list refresh, from RefreshTools or a
	// list_changed notification.
	CauseList Cause = "list"
	// CauseProbe is the health ping or a breaker's half-open probe.
	CauseProbe Cause = "probe"
	// CauseRefresh is a call replayed after a credential was renewed.
	CauseRefresh Cause = "refresh"
)

// PayloadKind identifies bytes stored in a payload pack.
type PayloadKind string

const (
	PayloadRequest       PayloadKind = "request"
	PayloadEffectiveArgs PayloadKind = "effective_arguments"
	PayloadResult        PayloadKind = "result"
	// PayloadFrame is one downstream frame's body. Its direction is the
	// event's own kind, so it needs no second spelling here.
	PayloadFrame PayloadKind = "frame"
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

// Event is one bounded line in calls.jsonl. Fields that do not apply to a
// lifecycle point are omitted; readers join events by CallID.
type Event struct {
	Version int       `json:"v"`
	TS      time.Time `json:"ts"`
	Kind    EventKind `json:"event"`
	CallID  string    `json:"callId"`
	KeyID   string    `json:"keyId"`
	MAC     string    `json:"mac"`

	Client    string `json:"client,omitempty"`
	Session   string `json:"session,omitempty"`
	PID       int    `json:"pid"`
	BootID    string `json:"bootId"`
	RequestID string `json:"requestId,omitempty"`
	Face      string `json:"face,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	PolicyRev uint64 `json:"policyRev,omitempty"`
	Exposed   string `json:"exposedTool,omitempty"`
	// Surface says WHICH of agenthub's own surfaces the client reached, in
	// internal/discovery's vocabulary: `meta` for one of the hub's own tools
	// (search_tools, call_tool, fetch_result …), `group` for a grouped
	// listing, `tool` for a name that routes straight through to a server.
	//
	// It is not derivable from Exposed after the fact — the same name means
	// different things under different discovery modes — and it is the
	// difference between "the client called the server" and "the client
	// asked the hub, which called the server". A `meta` record and a routed
	// one under the SAME call id is the second case, told in full.
	Surface    string `json:"surface,omitempty"`
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

	// Cause, Method and Seq describe a frame. Cause is why it crossed the
	// boundary, Method is the JSON-RPC method on the wire, and Seq numbers the
	// attempts within one call — a retry ladder produces sent/recv pairs 1, 2,
	// 3, and without the number they read as one exchange reported three
	// times.
	Cause  Cause  `json:"cause,omitempty"`
	Method string `json:"method,omitempty"`
	Seq    int    `json:"seq,omitempty"`

	Request       *PayloadRef `json:"request,omitempty"`
	EffectiveArgs *PayloadRef `json:"effectiveArguments,omitempty"`
	Result        *PayloadRef `json:"result,omitempty"`
	ResultMode    string      `json:"resultMode,omitempty"`
	ResultCapture string      `json:"resultCapture,omitempty"`
	ResultBytes   int         `json:"resultBytes,omitempty"`
	ResultCut     bool        `json:"resultTruncated,omitempty"`
	// Frame points at one downstream frame's body, when payload capture is on.
	// The metadata line stands on its own without it: bytes, duration and
	// outcome are on the event, so a frame stream costs nothing but a line
	// per frame until somebody asks for the contents.
	Frame *PayloadRef `json:"frame,omitempty"`
	// Bytes is the frame body's size BEFORE any capture decision, so a line
	// with no payload still tells the truth about how big the frame was.
	Bytes int `json:"bytes,omitempty"`
}

// Options configures a store. Root is normally <data>/calls.
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
