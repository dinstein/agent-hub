package services

import (
	"context"

	"github.com/dinstein/agent-hub/api"
)

// The two ways a server definition reaches the registry without anyone
// typing `npx -y @modelcontextprotocol/server-…` from memory (docs/modules/controlplane.md,
// §3.4): the curated catalog, and the parser that reads another client's
// configuration out of the clipboard.
//
// They sit in their own file rather than in registry.go or hub.go because
// they straddle the split the other two files are named for: browsing and
// parsing are reads that touch no document at all, while the add is an
// ordinary registry write with the usual expectedGeneration. Only
// AddFromCatalog can lose a compare-and-swap; the other two cannot.
//
// NOTHING here is a privileged path. Being curated buys an entry no shortcut
// through validation — `catalog add` goes through the same confops checks as
// `server add`, so a curated entry with a loopback URL is refused exactly
// like a hand-typed one. And the parser writes nothing at all: it answers
// with a PREVIEW, and the entries the user keeps are stored afterwards by
// CreateServer, which is where validation happens. A frontend must therefore
// expect a proposal the registry will later refuse, and must not present the
// preview as an accepted definition.

// SearchCatalog returns the catalog entries matching query, best match
// first; an empty query returns the whole directory.
//
// The answer echoes the query that produced it (api.CatalogList.Query) so a
// frontend typing into a search box can drop an answer that arrived after
// the user moved on, instead of painting stale results over fresh ones.
func (h *Hub) SearchCatalog(ctx context.Context, query string) (api.CatalogList, error) {
	return call(ctx, h, func(c *api.Client) (api.CatalogList, error) {
		return c.Catalog.Search(ctx, query)
	})
}

// AddFromCatalog stores one catalog entry as a server definition.
//
// It is an ordinary registry write: the same validation, the same 409 on an
// id already taken (E_SERVER_EXISTS — a name conflict, NOT a stale
// precondition, so retrying it unchanged can never succeed), and the same
// expectedGeneration guard as CreateServer.
//
// The answer carries NextSteps: the commands that finish the job (storing a
// credential, logging in). Adding the definition is not the same as making
// the server work, and a frontend that drops that list makes a half-done
// setup look finished.
func (h *Hub) AddFromCatalog(
	ctx context.Context, id string, req api.CatalogAddRequest, expectedGeneration uint64,
) (api.CatalogAdded, error) {
	return call(ctx, h, func(c *api.Client) (api.CatalogAdded, error) {
		return c.Catalog.Add(ctx, id, req, expectedGeneration)
	})
}

// ParseClientConfig reads pasted client-configuration text into a preview.
//
// NOTHING IS WRITTEN, so there is no precondition to carry: the caller
// renders the proposals, the user unticks the ones they do not want, and
// CreateServer stores the rest one by one.
//
// A TOML or YAML configuration is refused with E_UNSUPPORTED_FORMAT and a
// hint carrying the manual route. That refusal is deliberate and permanent
// (docs/modules/controlplane.md): agenthub does not take a parser dependency for a format it
// would read in exactly one dialog.
func (h *Hub) ParseClientConfig(ctx context.Context, text string) (api.ParsedClientConfig, error) {
	return call(ctx, h, func(c *api.Client) (api.ParsedClientConfig, error) {
		return c.Parse.ClientConfig(ctx, text)
	})
}
