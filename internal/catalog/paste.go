package catalog

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/dinstein/agent-hub/internal/registry"
)

// The paste path (docs/modules/controlplane.md): a user has a README fragment or another
// client's configuration in the clipboard, and the alternative to
// understanding it is retyping it as flags.
//
// ONE RULE GOVERNS THIS FILE: it parses, it never writes. Nothing here opens
// the registry, resolves a secret or touches a client's file on disk. The
// result is a PREVIEW the caller renders and the user confirms, after which
// the normal add path (internal/confops) validates it like any other entry.
//
// That is also why an unknown field is a WARNING here and a hard error on
// the CLI's `server add --stdin` path: a preview shows the user exactly what
// would be stored, so "these keys were ignored" is information they can act
// on; a write with no preview has to refuse instead, because the user would
// never learn that the "oauth" block they pasted vanished.
//
// The reading itself — which wrapper keys name a servers section, which
// document shapes count, and what each entry key means — is not here: it is
// in entrymap.go, shared verbatim with `agenthub server add --stdin`, which
// takes the same snippet without a preview in front of it. This file is what
// the PREVIEW route adds on top: proposals, skips, warnings, and the refusal
// of a format we recognize but do not parse.

// Shape names the wrapper a pasted document was recognized as.
type Shape string

// The recognized paste shapes. The first four are wrapper key paths from
// the client table; the last two are the naked forms people paste out of a
// README.
const (
	// ShapeWrapped is any of the client wrapper keys ("mcpServers",
	// "servers", "mcp.servers", "context_servers"); ParseResult.Section
	// says which.
	ShapeWrapped Shape = "wrapped"
	// ShapeEntryMap is a bare name -> entry object with no wrapper.
	ShapeEntryMap Shape = "entry-map"
	// ShapeSingleEntry is one unnamed entry ({"command": …}), which is what
	// `claude mcp add-json <name> '<json>'` takes as its argument.
	ShapeSingleEntry Shape = "single-entry"
)

// SourcePasted marks an entry that came from pasted client configuration.
// It records only that: the document was pasted, and unlike a file read
// from a known client's configuration it carries no provenance of its own.
const SourcePasted = "pasted"

// Proposal is one server a pasted document proposes. It is a candidate, not
// a stored entry: Name may be empty (a single entry names nothing) and the
// definition has not been through confops validation yet.
type Proposal struct {
	Name string `json:"name"`
	// Entry is the definition as it would be stored.
	Entry registry.ServerEntry `json:"entry"`
	// Warnings are the things the user must see before confirming:
	// dropped fields, a literal-looking credential, a missing name.
	Warnings []string `json:"warnings,omitempty"`
}

// Skip is one recognized entry that is deliberately not proposed.
type Skip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// ParseResult is the preview.
type ParseResult struct {
	Shape Shape `json:"shape"`
	// Section is the JSON key path the servers were found under (empty for
	// the naked shapes), e.g. ["mcp","servers"].
	Section []string `json:"section,omitempty"`
	// Servers are the proposals, sorted by name (an unnamed single entry
	// sorts first, being the only one).
	Servers []Proposal `json:"servers"`
	Skipped []Skip     `json:"skipped,omitempty"`
}

// UnsupportedError reports a configuration format agenthub RECOGNIZES but
// does not parse: today, TOML (Codex) and YAML (Continue).
//
// It is deliberately a distinct type from ParseError. "This is TOML and we
// do not read TOML" has an answer — the manual steps in Hint — while "this
// is not a configuration at all" does not, and folding the two together
// would hide the answer behind a generic failure.
//
// Failure direction: no dependency is added to make it parseable. A parser
// for a format agenthub only ever reads once, in one dialog, is not worth a
// permanent supply-chain edge (docs/modules/controlplane.md).
type UnsupportedError struct {
	Format string
	Hint   string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("%s configuration is not parsed by agenthub", e.Format)
}

// ParseError reports text that could not be read as a client configuration.
type ParseError struct {
	Reason string
	Hint   string
}

func (e *ParseError) Error() string { return e.Reason }

// maxPasteBytes bounds one pasted document. A client configuration is a
// handful of kilobytes; anything past this is a mistake or an attempt to
// make the parser the expensive part of a request.
const maxPasteBytes = 1 << 20

// ParseClientConfig turns pasted client-configuration text into proposals.
//
// Recognized, in order: the wrapper key paths of every client in the
// internal/clients table (mcpServers, servers, mcp.servers,
// context_servers), a bare name -> entry map, and a single unnamed entry.
// TOML and YAML are recognized and REFUSED with instructions
// (*UnsupportedError); anything else is *ParseError.
func ParseClientConfig(text string) (ParseResult, error) {
	trimmed := strings.TrimSpace(text)
	switch {
	case trimmed == "":
		return ParseResult{}, &ParseError{
			Reason: "nothing to parse",
			Hint:   "paste the JSON object your client stores its MCP servers in",
		}
	case len(text) > maxPasteBytes:
		return ParseResult{}, &ParseError{
			Reason: fmt.Sprintf("pasted text exceeds %d bytes", maxPasteBytes),
			Hint:   "paste only the MCP server section, not the whole settings file",
		}
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &top); err != nil {
		if format, ok := nonJSONFormat(trimmed); ok {
			return ParseResult{}, unsupported(format)
		}
		return ParseResult{}, &ParseError{
			Reason: "the pasted text is not a JSON object: " + err.Error(),
			Hint:   `expected something like {"mcpServers":{"name":{"command":"npx","args":["-y","pkg"]}}}`,
		}
	}

	shape, section, raws, ok := Recognize(top)
	if !ok {
		return ParseResult{}, &ParseError{
			Reason: "the pasted JSON does not look like an MCP server configuration",
			Hint: "expected a wrapper key (" + strings.Join(SectionNames(), ", ") +
				"), a name -> entry object, or a single entry with a command or url",
		}
	}
	res := ParseResult{Shape: shape, Section: section}
	if len(raws) == 0 {
		return ParseResult{}, &ParseError{
			Reason: "the pasted configuration declares no servers",
			Hint:   "check that the section you copied is not empty",
		}
	}

	names := slices.Sorted(maps.Keys(raws))
	for _, name := range names {
		entry, warnings, err := entryFrom(raws[name])
		if err != nil {
			// One bad entry does not sink the batch: it is reported and the
			// rest stay usable, exactly as internal/clients does on import.
			res.Skipped = append(res.Skipped, Skip{Name: name, Reason: err.Error()})
			continue
		}
		if reason, self := isGatewayEntry(entry); self {
			res.Skipped = append(res.Skipped, Skip{Name: name, Reason: reason})
			continue
		}
		if name == "" {
			warnings = append(warnings,
				"this snippet does not name the server; choose a name before adding it")
		}
		res.Servers = append(res.Servers, Proposal{Name: name, Entry: entry, Warnings: warnings})
	}
	if len(res.Servers) == 0 && len(res.Skipped) > 0 {
		return res, &ParseError{
			Reason: "no usable server entries: " + res.Skipped[0].Reason,
			Hint:   "the reported entries were recognized but could not be converted",
		}
	}
	return res, nil
}

// entryFrom converts one pasted entry into a registry definition, as the
// PREVIEW route wants it: unmodeled keys are reported and the entry survives.
//
// The conversion itself is MapEntry (entrymap.go), shared with `server add
// --stdin`; what belongs to this route and not to that one is added here.
func entryFrom(raw json.RawMessage) (registry.ServerEntry, []string, error) {
	entry, warnings, err := MapEntry(raw, WarnOnUnknownFields)
	if err != nil {
		return registry.ServerEntry{}, nil, err
	}
	entry.Source = SourcePasted
	if entry.Transport != registry.TransportStdio {
		// Provenance is a trust declaration, and a pasted endpoint has made
		// none: default to the screened value so SSRF checks stay on. Only
		// an explicit operator action may relax it.
		entry.Provenance = registry.ProvenanceRemote
	}
	if leaked := literalCredentials(entry); len(leaked) > 0 {
		warnings = append(warnings,
			"looks like a literal credential in "+strings.Join(leaked, ", ")+
				"; store it with 'agenthub secret set' and reference it as ${SECRET_KEY} instead")
	}
	return entry, warnings, nil
}

// isGatewayEntry recognizes agenthub's own gateway entry, which every
// adapted client configuration contains. Adding it would point agenthub at
// itself — an infinite regress that presents as a hang, not an error.
func isGatewayEntry(e registry.ServerEntry) (string, bool) {
	if len(e.Args) == 0 || e.Args[0] != "connect" {
		return "", false
	}
	for _, a := range e.Args {
		if a == "--client" {
			return "agenthub gateway entry (adding it would point agenthub at itself)", true
		}
	}
	return "", false
}

// secretishKeys are the substrings that make a configuration value likely to
// be a credential. Mirrors internal/clients' import warning, which draws the
// same line for the same reason: a registry document must never hold a
// credential.
var secretishKeys = []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "PASSWD", "AUTH", "CREDENTIAL"}

// literalCredentials names the env/header keys whose value looks like a
// pasted credential rather than a placeholder.
func literalCredentials(e registry.ServerEntry) []string {
	var out []string
	for _, m := range []map[string]string{e.Env, e.Headers} {
		for k, v := range m {
			if v == "" || strings.Contains(v, "${") {
				continue
			}
			up := strings.ToUpper(k)
			for _, s := range secretishKeys {
				if strings.Contains(up, s) {
					out = append(out, k)
					break
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// nonJSONFormat classifies text that is not JSON but IS a configuration we
// recognize, so the user gets instructions instead of a parse error.
//
// The detection is intentionally shallow: a TOML table header, a bare
// `key = value` line, or a YAML mapping key. It runs only after JSON
// decoding already failed, so a false positive costs a wrong hint, never a
// wrong parse — and the hint it costs still tells the user how to add the
// server by hand.
func nonJSONFormat(text string) (string, bool) {
	weak := ""
	scanned := 0
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if scanned++; scanned > maxFormatProbeLines {
			break
		}
		switch {
		case strings.HasPrefix(line, "[mcp_servers"), strings.HasPrefix(line, "[[mcp_servers"):
			return "TOML", true // the Codex spelling: unambiguous
		case isTOMLTableHeader(line):
			weak = "TOML"
		case strings.HasPrefix(line, "mcpServers:"), strings.HasPrefix(line, "mcp:"),
			strings.HasPrefix(line, "servers:"), strings.HasPrefix(line, "- name:"),
			strings.HasPrefix(line, "context_servers:"):
			return "YAML", true
		case weak == "" && isTOMLAssignment(line):
			weak = "TOML"
		case weak == "" && isYAMLMappingKey(line):
			weak = "YAML"
		}
	}
	return weak, weak != ""
}

// maxFormatProbeLines bounds the classification scan. The answer is in the
// first few lines of any real configuration; reading further would only make
// a large paste expensive.
const maxFormatProbeLines = 50

// isTOMLTableHeader recognizes `[table]` and `[[array]]`. The content must
// be a bare key path — otherwise a JSON array (`[{"command":…}]`) would be
// classified as TOML and the user would be told to edit a file they do not
// have.
func isTOMLTableHeader(line string) bool {
	if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
		return false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
	inner = strings.TrimSuffix(strings.TrimPrefix(inner, "["), "]")
	return isBareKey(inner)
}

// isTOMLAssignment recognizes `key = value` (TOML), which JSON never
// produces.
func isTOMLAssignment(line string) bool {
	key, _, ok := strings.Cut(line, "=")
	return ok && isBareKey(strings.TrimSpace(key))
}

// isYAMLMappingKey recognizes `key:` and `key: value` (YAML).
func isYAMLMappingKey(line string) bool {
	key, _, ok := strings.Cut(line, ":")
	return ok && isBareKey(strings.TrimSpace(key))
}

// isBareKey reports whether s is an unquoted identifier-ish key. A quoted
// key means JSON, which was already tried and failed for other reasons.
func isBareKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// unsupported builds the refusal, with the manual route in the hint. The
// user is redirected, not stranded: `agenthub server add` takes the same
// definition as flags.
func unsupported(format string) *UnsupportedError {
	return &UnsupportedError{
		Format: format,
		Hint: "agenthub does not parse " + format + " (adding a parser for one dialog is not worth " +
			"a permanent dependency). Add the server directly instead:\n" +
			"  agenthub server add <name> --cmd <command> --args <arg1>,<arg2>\n" +
			"or paste the equivalent JSON: {\"mcpServers\":{\"<name>\":{\"command\":\"…\",\"args\":[…]}}}",
	}
}
