package scope

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"io"
	"sort"
)

// hashScope computes the content address of an EffectiveScope: SHA-256 over
// a canonical, length-prefixed encoding of every field EXCEPT Generation and
// Hash itself. Map keys are visited in sorted order and every string is
// length-prefixed, so the encoding is injective and the hash is stable
// across processes and Go versions (golden-tested — determinism is
// contract, canonical.md §6).
func hashScope(es *EffectiveScope) [32]byte {
	h := sha256.New()

	ids := sortedKeys(es.Servers)
	writeStr(h, "servers")
	writeUint(h, uint64(len(ids)))
	for _, id := range ids {
		writeStr(h, id)
		tv := es.Servers[id]
		writeUint(h, uint64(len(tv.Tools)))
		for _, t := range tv.Tools {
			writeStr(h, t)
		}
	}

	writeStr(h, "discovery")
	writeStr(h, string(es.Discovery))

	keys := sortedKeys(es.Budgets)
	writeStr(h, "budgets")
	writeUint(h, uint64(len(keys)))
	for _, k := range keys {
		writeStr(h, k)
		writeUint(h, uint64(int64(es.Budgets[k])))
	}

	writeStr(h, "approval")
	writeBool(h, es.Approval.HumanApproval)
	writeBool(h, es.Approval.ConfirmDestructive)
	writeBool(h, es.Approval.DenyDestructive)

	writeStr(h, "diags")
	writeUint(h, uint64(len(es.Diags)))
	for _, d := range es.Diags {
		writeUint(h, uint64(d.Layer))
		writeStr(h, d.Origin)
		writeStr(h, d.Message)
	}

	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func writeStr(h hash.Hash, s string) {
	writeUint(h, uint64(len(s)))
	_, _ = io.WriteString(h, s)
}

func writeUint(h hash.Hash, v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	_, _ = h.Write(b[:])
}

func writeBool(h hash.Hash, v bool) {
	if v {
		_, _ = h.Write([]byte{1})
	} else {
		_, _ = h.Write([]byte{0})
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
