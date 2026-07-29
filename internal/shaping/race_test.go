package shaping

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// Both stores are shared by every request of a session (and, for FileStore,
// by every session of the daemon), so concurrent Fetch/Put/Sweep must be
// safe and must never serve one owner's payload to another. Run under
// -race; the assertions catch the isolation break, the race detector
// catches the data race.
func TestConcurrentFetchIsolation(t *testing.T) {
	owners := []Owner{"claude-code:1", "claude-code:2", "cursor:7"}
	payload := func(o Owner) string { return strings.Repeat(string(o)+" payload. ", 40) }

	for _, tc := range []struct {
		name  string
		build func(t *testing.T) Store
	}{
		{"MemStore", func(t *testing.T) Store { return NewMemStore(256) }},
		{"FileStore", func(t *testing.T) Store {
			s, err := NewFileStoreAt(t.TempDir(), goldenNow)
			if err != nil {
				t.Fatal(err)
			}
			return s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.build(t)
			ids := make(map[Owner]string, len(owners))
			for _, o := range owners {
				id := store.NextID()
				ids[o] = id
				if err := store.Put(t.Context(), Entry{
					ID: id, Owner: o, CreatedAt: time.Now(), TTL: time.Hour,
					Budget: Budget{Bytes: 256}, Full: payload(o),
				}); err != nil {
					t.Fatal(err)
				}
			}

			var wg sync.WaitGroup
			for _, o := range owners {
				for _, target := range owners {
					wg.Add(1)
					go func(reader, holder Owner) {
						defer wg.Done()
						for i := 0; i < 40; i++ {
							res, ok := Fetch(t.Context(), store, reader, ids[holder], i*7)
							switch {
							case reader != holder:
								// Another session's cursor: always the
								// frozen miss, never a byte of payload.
								if ok {
									t.Errorf("%s read %s's cursor", reader, holder)
									return
								}
								if string(res.Content) != goldenNotFound {
									t.Errorf("cross-owner miss text drifted: %s", res.Content)
									return
								}
							case !ok:
								t.Errorf("%s could not read its own cursor", reader)
								return
							}
						}
					}(o, target)
				}
			}
			// Concurrent writers and sweepers on the same store.
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func(n int) {
					defer wg.Done()
					for j := 0; j < 20; j++ {
						id := store.NextID()
						o := owners[(n+j)%len(owners)]
						if err := store.Put(t.Context(), Entry{
							ID: id, Owner: o, CreatedAt: time.Now(), TTL: time.Hour,
							Budget: Budget{Bytes: 128}, Full: payload(o),
						}); err != nil {
							t.Errorf("put: %v", err)
							return
						}
						if _, err := store.Sweep(t.Context(), time.Now()); err != nil {
							t.Errorf("sweep: %v", err)
							return
						}
					}
				}(i)
			}
			wg.Wait()
		})
	}
}

// NextID must hand out distinct ids under concurrency: two calls sharing an
// id would let one session's Put clobber another's cursor.
func TestNextIDUnique(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) Store
	}{
		{"MemStore", func(t *testing.T) Store { return NewMemStore(0) }},
		{"FileStore", func(t *testing.T) Store {
			s, err := NewFileStoreAt(t.TempDir(), goldenNow)
			if err != nil {
				t.Fatal(err)
			}
			return s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.build(t)
			const n = 200
			out := make(chan string, n)
			var wg sync.WaitGroup
			for range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					out <- store.NextID()
				}()
			}
			wg.Wait()
			close(out)
			seen := make(map[string]bool, n)
			for id := range out {
				if !validID(id) {
					t.Fatalf("minted an invalid id %q", id)
				}
				if seen[id] {
					t.Fatalf("duplicate id %q", id)
				}
				seen[id] = true
			}
		})
	}
}
