// Package catalog answers one question a configuration table cannot:
// "which server could I add next, and what would that cost me?"
//
// It holds two ways of arriving at a proposed server definition, and it
// WRITES NEITHER of them:
//
//   - The curated directory (catalog.go + seed.json): a small, embedded set
//     of well-known MCP servers, each carrying the way its publisher
//     documents reaching it, so adding one is a choice from a list rather
//     than a remembered endpoint or command line. MOST OF THE SET IS NOW
//     HOSTED — an HTTP endpoint the publisher runs — with local commands the
//     minority. This paragraph described the command line alone, from when
//     that was the whole directory; count seed.json rather than trusting a
//     number written here, which is how it went stale the first time.
//   - The pasted-configuration parser (paste.go): the README fragment or
//     client configuration a user already has in the clipboard, turned into
//     the same proposal shape for preview.
//
// Both produce PROPOSALS. internal/confops remains the single implementation
// of every registry write, and the single place an entry is validated — this
// package never opens the registry, so a catalog entry gets exactly the same
// scrutiny a hand-typed one does.
//
// # What provenance means, and what it does not
//
// Entry.Provenance grades WHERE a definition came from — curated (this
// file, reviewed by the agenthub maintainers when it was written), registry
// (a remote index, not implemented) or user (typed or pasted by the person
// at the keyboard). Together with Publisher and Homepage it is a SOURCE
// SIGNAL, not a cryptographic proof: nothing here is signed, nothing is
// verified at add time, and `npx -y <package>` still fetches whatever the
// registry serves at that moment. A curated entry means the maintainers
// believed the command line was the publisher's documented one; it does not
// mean the code that eventually runs is the code they looked at.
//
// The defences that DO make claims about running code live elsewhere:
// internal/guard/spawnguard screens what gets spawned, internal/guard/netguard
// screens where a connection may go. This package feeds them a definition; it
// does not vouch for it.
//
// # needsConfig: the one-click split
//
// Entry.NeedsConfig reports whether adding the entry can be a single click.
// An entry needs configuration when it declares a credential, declares a
// parameter, or still carries an unsubstituted placeholder anywhere in its
// command line, URL, environment or headers. Everything else is addable as
// it stands.
//
// It answers "is there anything to TYPE", not "is there anything to DO", and
// on a hosted directory those have come apart. Most hosted entries declare
// `auth: oauth`, which NeedsConfig deliberately does not count: there is no
// form, and putting one in front of an OAuth server would ask for something
// the user cannot supply by typing. Such an entry is still a one-click ADD
// followed by `agenthub auth login <id>` before the server serves anything.
// Worth saying in a package whose first sentence is "what would that cost
// me?".
//
// Failure direction: an entry whose placeholders are not all resolved is
// REFUSED by Render, never added with the literal "{{directory}}" left in
// its arguments. A server that fails at connect time with a path nobody
// typed is much harder to explain than a refusal at add time.
package catalog
