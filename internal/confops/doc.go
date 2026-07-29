// Package confops is the single implementation of every SEMANTIC write
// against the configuration registry: adding a server, renaming a profile,
// binding a client, flipping a governance switch, disabling one tool.
//
// Why it exists (docs/modules/controlplane.md): the CLI and the
// control plane are two front ends over the same configuration. If each one
// spelled out "what it means to rename a profile", they would drift — and
// the two would then disagree about the same operation, which is the class
// of bug SpecFromEntry already demonstrated once (a second translation point
// silently dropped container isolation). Front ends here own flag parsing,
// rendering and transport; they own no rules.
//
// Shape of the API: OPERATIONS, never field setters. RenameProfile also
// repoints every client and project reference, because leaving them behind
// would fail-close those clients to an empty scope — that consequence is
// part of the operation, not of the caller.
//
// Every operation:
//
//  1. validates its arguments (rejecting before anything is opened),
//  2. mutates inside registry.Store.Update — i.e. under the cross-process
//     lock, against documents freshly re-read from disk,
//  3. returns a Result carrying the post-commit generation, the healed
//     quarantine reports as warnings, and whether anything actually changed.
//
// Concurrency (docs/flows.md §4): the registry has five
// writers (N gateways, the daemon, the CLI, the GUI, third parties). Last
// writer wins is not acceptable for a long-lived GUI window whose view may
// be minutes old, so every operation takes a Precondition. A non-zero
// Precondition.Generation is compared INSIDE the lock, before the mutation,
// against the generation the transaction loaded; a mismatch returns a typed
// *StaleError carrying the current generation and writes nothing.
// Precondition{} (generation 0) means "do not check" and is what the CLI
// passes, so its behaviour is unchanged.
//
// Failure direction: validation refuses rather than normalizes. An unknown
// transport, an unknown runtime, an unparseable boolean — each leaves the
// registry untouched instead of landing on a default the operator did not
// ask for.
//
// Errors are *Error values carrying a stable machine code (the same E_*
// vocabulary the CLI's JSON envelope and the control plane's error body
// use) plus a Kind the front end maps to its own exit code or HTTP status.
package confops
