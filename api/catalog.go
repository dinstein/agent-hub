package api

import (
	"context"
	"net/http"
	"net/url"
)

// The curated catalog and the paste parser: the two ways a server gets into
// the registry without anyone typing an `npx -y @…` command line from memory
// (docs/subsystems/controlplane.md).
//
// CONTRACT: these types mirror internal/catalog (api cannot import
// internal/*). Both surfaces are READ-ONLY except CatalogService.Add, which
// is an ordinary registry write and therefore takes an expectedGeneration
// like every other one.

// Catalog provenance values. Provenance grades WHERE a definition came from
// and is a SOURCE SIGNAL, not a cryptographic proof: nothing in the catalog
// is signed and nothing is verified at add time. A frontend may render it as
// origin; it must not render it as "verified" or "safe".
const (
	// ProvenanceCurated is agenthub's embedded directory, reviewed by the
	// maintainers when it was written.
	ProvenanceCurated = "curated"
	// ProvenanceRegistry is a remote index (not implemented).
	ProvenanceRegistry = "registry"
	// ProvenanceUser is a definition the user typed or pasted.
	ProvenanceUser = "user"
)

// CatalogAuthOAuth marks an entry whose server needs an OAuth login AFTER it
// is added. It does not make the entry harder to add — the login is a later,
// separate step — so such an entry is still one-click addable.
const CatalogAuthOAuth = "oauth"

// CatalogCredential is one secret a catalog entry needs.
//
// Key is the VAULT key (`agenthub secret set <server> <KEY>`), which the
// entry references as ${SECRET_<KEY>}. No value travels here: the catalog
// says which credential is needed, it never carries one.
type CatalogCredential struct {
	Key         string `json:"key"`
	Description string `json:"description,omitempty"`
	// Optional marks a credential the server works without. An optional
	// credential does not make an entry need configuration.
	Optional bool `json:"optional,omitempty"`
}

// CatalogParam is one plain (non-secret) value the user must supply: a
// directory, a database path, a workspace id. The entry references it as
// {{name}}, and Add substitutes it.
type CatalogParam struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Example     string `json:"example,omitempty"`
}

// CatalogEntry is one server the catalog offers.
type CatalogEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Publisher   string `json:"publisher,omitempty"`
	Homepage    string `json:"homepage,omitempty"`
	// Provenance is one of the Provenance* constants above.
	Provenance string `json:"provenance"`

	// Transport is one of the Transport* constants.
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`

	Keys   []CatalogCredential `json:"keys,omitempty"`
	Params []CatalogParam      `json:"params,omitempty"`
	// Auth is CatalogAuthOAuth for a server that needs a login after being
	// added, empty otherwise.
	Auth string   `json:"auth,omitempty"`
	Tags []string `json:"tags,omitempty"`

	// NeedsConfig is false when the entry can be added with a single click:
	// no required credential, no parameter, no leftover placeholder. The
	// daemon computes it so every frontend splits the list identically.
	NeedsConfig bool `json:"needs_config"`
	// RequiredKeys are the credentials to store afterwards.
	RequiredKeys []string `json:"required_keys,omitempty"`
}

// CatalogList is the answer to List and Search. Query echoes what produced
// the entries, so a frontend can tell a stale response from a current one.
type CatalogList struct {
	Query   string         `json:"query,omitempty"`
	Entries []CatalogEntry `json:"entries"`
}

// CatalogAddRequest configures one add.
type CatalogAddRequest struct {
	// Name overrides the registry id ("" = the catalog id).
	Name string `json:"name,omitempty"`
	// Params supplies the entry's declared parameters. A missing or unknown
	// one is refused, never guessed: an entry added with an unresolved
	// placeholder would fail much later with a path nobody typed.
	Params map[string]string `json:"params,omitempty"`
}

// CatalogAdded is the answer to a successful add.
type CatalogAdded struct {
	WriteResult
	// ID is the server id as stored; CatalogID is where it came from.
	ID        string       `json:"id"`
	CatalogID string       `json:"catalog_id"`
	Entry     *ServerEntry `json:"entry,omitempty"`
	// NextSteps are the commands that finish the job — storing a credential,
	// logging in. Adding the definition is not the same as making the server
	// work, and a frontend that hides this makes a half-done setup look
	// finished.
	NextSteps []string `json:"next_steps,omitempty"`
}

// CatalogService browses the curated server directory and adds from it.
type CatalogService struct{ c *Client }

// List returns the whole directory, sorted by id.
func (s *CatalogService) List(ctx context.Context) (CatalogList, error) {
	return s.Search(ctx, "")
}

// Search returns the entries matching query, best match first. Every
// whitespace-separated term must match (terms narrow, they do not widen);
// an empty query returns everything.
func (s *CatalogService) Search(ctx context.Context, query string) (CatalogList, error) {
	var q url.Values
	if query != "" {
		q = url.Values{"q": []string{query}}
	}
	var out CatalogList
	err := s.c.do(ctx, http.MethodGet, "/catalog", q, nil, &out)
	return out, err
}

// Get returns one catalog entry. An unknown id answers the uniform 404.
func (s *CatalogService) Get(ctx context.Context, id string) (CatalogEntry, error) {
	var out CatalogEntry
	err := s.c.do(ctx, http.MethodGet, "/catalog/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

// Add stores a catalog entry as a server definition.
//
// It is an ordinary registry write: the same validation, the same conflict
// on an id already taken (E_SERVER_EXISTS at 409, not a stale precondition),
// and the same expectedGeneration guard. Being curated buys the entry no
// shortcut through the rules — only the typing.
func (s *CatalogService) Add(
	ctx context.Context, id string, req CatalogAddRequest, expectedGeneration uint64,
) (CatalogAdded, error) {
	var out CatalogAdded
	err := s.c.doWrite(ctx, http.MethodPost, "/catalog/"+url.PathEscape(id)+"/add", nil,
		expectedGeneration, req, &out)
	return out, err
}

// ParsedServer is one server a pasted configuration proposes. It is a
// candidate, not a stored entry: Name may be empty (a single pasted entry
// names nothing), and nothing has been written.
type ParsedServer struct {
	Name string `json:"name"`
	// Entry is the definition as it would be stored.
	Entry ServerEntry `json:"entry"`
	// Warnings are what the user must see before confirming: fields
	// agenthub does not model and therefore dropped, a value that looks
	// like a pasted credential, a missing name.
	Warnings []string `json:"warnings,omitempty"`
}

// ParsedSkip is one recognized entry that is deliberately not proposed —
// agenthub's own gateway entry, or an entry naming neither command nor url.
type ParsedSkip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// ParsedClientConfig is the preview of a pasted configuration.
type ParsedClientConfig struct {
	// Shape names the form recognized: "wrapped", "entry-map" or
	// "single-entry".
	Shape string `json:"shape"`
	// Section is the JSON key path the servers were found under
	// (["mcpServers"], ["mcp","servers"], …); empty for the naked shapes.
	Section []string `json:"section,omitempty"`
	// Servers are the proposals, sorted by name.
	Servers []ParsedServer `json:"servers"`
	Skipped []ParsedSkip   `json:"skipped,omitempty"`
}

// ParseService reads configuration text without applying it.
type ParseService struct{ c *Client }

// ClientConfig parses pasted client-configuration text into a preview.
//
// NOTHING IS WRITTEN. The caller renders the proposals, the user edits and
// confirms them, and Servers.Create stores the ones they kept — which is
// also where validation happens, so a preview may contain an entry the
// registry will later refuse (a loopback URL, say).
//
// TOML and YAML configurations answer 400 with E_UNSUPPORTED_FORMAT and a
// hint carrying the manual route: agenthub deliberately ships no parser for
// a format it would read in exactly one dialog.
func (s *ParseService) ClientConfig(ctx context.Context, text string) (ParsedClientConfig, error) {
	var out ParsedClientConfig
	err := s.c.do(ctx, http.MethodPost, "/parse/client-config", nil,
		struct {
			Text string `json:"text"`
		}{Text: text}, &out)
	return out, err
}
