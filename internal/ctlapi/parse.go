package ctlapi

import (
	"errors"
	"net/http"

	"github.com/dinstein/agent-hub/internal/catalog"
)

// POST /v1/parse/client-config (docs/modules/controlplane.md): turn pasted client-configuration
// text into a preview of server entries.
//
// IT WRITES NOTHING. No registry, no client file, no secret — the answer is
// a proposal the user confirms, after which the normal POST /v1/servers path
// stores it.
//
// It exists because the alternative is retyping: internal/clients already
// recognizes these shapes on disk, but "parse this string" was the one entry
// point missing, and without it a GUI would have to grow its own parser —
// a second implementation of a format we already read, drifting from the
// first.

// CodeUnsupportedFormat rejects a configuration format agenthub RECOGNIZES
// but does not parse (TOML, YAML). It is deliberately distinct from
// CodeBadRequest: "this is TOML and we do not read TOML" has a next step,
// which travels in the error hint, and burying it under a generic parse
// failure would hide the only useful part of the answer.
const CodeUnsupportedFormat = "E_UNSUPPORTED_FORMAT"

// parseClientConfigRequest is the request body: the raw pasted text.
//
// Text is a plain JSON string rather than raw body bytes so the endpoint
// keeps the one content type the whole control plane speaks, and so a
// future field (a format hint, say) does not need a second shape.
type parseClientConfigRequest struct {
	Text string `json:"text"`
}

// handleParseClientConfig implements POST /v1/parse/client-config.
//
// The result is catalog.ParseResult verbatim — no projection. A preview
// exists to show the user exactly what would be stored, and a translation
// layer here would be a place for the preview and the stored entry to
// disagree.
func (s *Server) handleParseClientConfig(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFrom(r.Context())
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	var req parseClientConfigRequest
	if !decodeAdminBody(w, r, body, &req) {
		return
	}
	res, err := catalog.ParseClientConfig(req.Text)
	if err != nil {
		var unsupported *catalog.UnsupportedError
		var parseErr *catalog.ParseError
		switch {
		case errors.As(err, &unsupported):
			writeErr(w, http.StatusBadRequest, CodeUnsupportedFormat,
				unsupported.Error(), unsupported.Hint, reqID)
		case errors.As(err, &parseErr):
			writeErr(w, http.StatusBadRequest, CodeBadRequest, parseErr.Reason, parseErr.Hint, reqID)
		default:
			writeErr(w, http.StatusInternalServerError, CodeInternal, err.Error(), "", reqID)
		}
		return
	}
	writeOK(w, http.StatusOK, res)
}
