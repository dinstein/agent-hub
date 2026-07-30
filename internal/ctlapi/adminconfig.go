package ctlapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/dinstein/agent-hub/internal/confops"
	"github.com/dinstein/agent-hub/internal/registry"
)

// The governance switches — the GLOBAL layer of the scope chain
// (docs/modules/controlplane.md). The key table is confops.GovernanceKeys:
// listing and setting read the SAME table, because a key whose listing and
// whose setter disagree is a governance switch nobody can trust.
//
// These switches merge tighten-only downward, so no lower layer can undo
// one. This endpoint is therefore the ONLY place that can relax them, and
// relaxing is deliberately allowed here — refusing would leave an operator
// unable to turn off a gate they turned on. What is not optional is the
// evidence: every write is audited with the key and both values, so
// "blockOnInjection went off at 03:00" is answerable after the fact.

// configEntryWire is one governance key with its current value.
type configEntryWire struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Kind is "bool", "enum" or "bytes".
	Kind string `json:"kind"`
	Doc  string `json:"doc,omitempty"`
}

// configListWire is the GET /v1/config body.
type configListWire struct {
	Generation uint64            `json:"generation"`
	Entries    []configEntryWire `json:"entries"`
}

// configSetWire is the PUT /v1/config/{key} body.
type configSetWire struct {
	preconditionWire
	// Value is the new value. A JSON string is passed to confops verbatim;
	// a bool or a number is rendered to its literal spelling first, because
	// a checkbox sending `true` means the same thing as a form sending
	// `"true"`. The VALUE SEMANTICS still live in confops, which parses the
	// string and refuses what it does not recognize.
	Value json.RawMessage `json:"value"`
}

// configWriteWire is the response of a governance write.
type configWriteWire struct {
	writeResultWire
	Key      string `json:"key"`
	Value    string `json:"value"`
	Previous string `json:"previous"`
}

// handleConfigList implements GET /v1/config.
func (s *Server) handleConfigList(w http.ResponseWriter, _ *http.Request) {
	snap := s.opts.Registry.Snapshot()
	entries := confops.ListGovernance(snap.Governance.V)
	out := configListWire{Generation: snap.Generation, Entries: make([]configEntryWire, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, configEntryWire{Key: e.Key, Value: e.Value, Kind: e.Kind, Doc: e.Doc})
	}
	writeOK(w, http.StatusOK, out)
}

// handleConfigSet implements PUT /v1/config/{key}.
func (s *Server) handleConfigSet(w http.ResponseWriter, r *http.Request, key string) {
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	var req configSetWire
	if !decodeAdminBody(w, r, body, &req) {
		return
	}
	pre, ok := adminPrecondition(w, r, body)
	if !ok {
		return
	}
	reqID := requestIDFrom(r.Context())
	value, err := configValueOf(req.Value)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error(),
			"send value as a JSON string, boolean or number", reqID)
		return
	}
	res, serr := confops.SetGovernance(r.Context(), s.opts.Registry, key, value, pre)
	if serr != nil {
		s.writeOpsError(w, r, serr)
		return
	}
	s.publishRegistryChange(registry.DocGovernance, res.Generation)
	writeOK(w, http.StatusOK, configWriteWire{
		writeResultWire: writeResultWire{
			Generation: res.Generation,
			// Changed is the SEMANTIC answer (did the value move), which is
			// what confops computes for a governance write and what a
			// frontend reports; it can differ from the generation-derived
			// flag when a rewrite only normalizes spelling.
			Changed:  res.Changed,
			Warnings: res.Warnings,
		},
		Key: res.Key, Value: res.Value, Previous: res.Previous,
	})
}

// configValueOf renders a JSON value as the string confops parses.
//
// Only the three scalar shapes are accepted. An object or an array is
// REFUSED rather than stringified: no governance key takes one, and
// inventing an encoding would be inventing a semantic that confops does not
// share.
func configValueOf(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("value is required")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("decoding value: %w", err)
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case nil:
		// null clears a key, the same spelling confops accepts for "unset".
		return "", nil
	default:
		return "", errors.New("value must be a string, boolean or number")
	}
}
