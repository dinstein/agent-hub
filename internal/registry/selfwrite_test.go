package registry

import (
	"testing"
	"time"
)

func TestSelfWriteConsumeRemovesOneSlot(t *testing.T) {
	var s selfWriteSet
	s.register("aa")
	s.register("aa") // multi-slot: two in-flight writes of identical payload

	if !s.consume("aa") {
		t.Fatal("first consume: want hit")
	}
	if !s.consume("aa") {
		t.Fatal("second consume: want hit (second slot)")
	}
	if s.consume("aa") {
		t.Fatal("third consume: want miss, both slots spent")
	}
}

func TestSelfWriteTTLExpiry(t *testing.T) {
	now := time.Now()
	s := selfWriteSet{now: func() time.Time { return now }}

	s.register("aa")
	now = now.Add(selfWriteTTL - time.Millisecond)
	if !s.consume("aa") {
		t.Fatal("entry inside TTL: want hit")
	}

	s.register("bb")
	now = now.Add(selfWriteTTL) // exactly TTL later: expired
	if s.consume("bb") {
		t.Fatal("entry past TTL: want miss")
	}
}

func TestSelfWriteBoundedEvictsOldest(t *testing.T) {
	var s selfWriteSet
	s.register("first")
	for range selfWriteSlots {
		s.register("filler")
	}
	if s.consume("first") {
		t.Fatal("oldest entry should have been evicted at capacity")
	}
	if !s.consume("filler") {
		t.Fatal("newer entries must survive eviction")
	}
}

func TestSelfWriteWithdrawRemovesMostRecent(t *testing.T) {
	var s selfWriteSet
	s.register("aa")
	s.withdraw("aa")
	if s.consume("aa") {
		t.Fatal("withdrawn entry must not suppress")
	}
	s.withdraw("missing") // no-op, must not panic
}

func TestSelfWriteClear(t *testing.T) {
	var s selfWriteSet
	s.register("aa")
	s.register("bb")
	s.clear()
	if s.consume("aa") || s.consume("bb") {
		t.Fatal("clear must drop every entry")
	}
}

func TestFingerprintIsFormattingInsensitive(t *testing.T) {
	a := fingerprint([]byte("{\n  \"b\": 1,\n  \"a\": [1, 2]\n}\n"))
	b := fingerprint([]byte(`{"a":[1,2],"b":1}`))
	if a != b {
		t.Errorf("fingerprints differ across formatting: %s vs %s", a, b)
	}
	c := fingerprint([]byte(`{"a":[1,2],"b":2}`))
	if a == c {
		t.Error("fingerprints collide across distinct values")
	}
	// Invalid JSON falls back to raw-byte hashing and must not panic.
	if fingerprint([]byte("{oops")) == fingerprint([]byte("{oops!")) {
		t.Error("raw fallback fingerprints collide")
	}
}
