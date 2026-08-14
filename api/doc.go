// Package api is the public control-plane surface of agenthub: the wire
// DTOs and the Go client used by the GUI (cmd/agenthub-gui), the CLI and
// third-party integrations. It talks REST + SSE over the daemon's Unix
// domain socket; the server side lives in
// internal/ctlapi.
//
// Constraints (docs/conventions.md#dependency-directions rule 1, enforced by depguard and proven by
// internal/depguardtest):
//
//   - This package must never import internal/*. "GUI is optional" is a
//     compile-time property, and it only holds if the public surface stays
//     free of internal dependencies.
//   - Standard library only: no third-party modules either, so embedding
//     the client never drags in extra dependency surface.
//
// The surface is split in two by CONCURRENCY, not by resource:
//
//   - Registry-backed configuration (servers, profiles, client scope
//     bindings, governance keys, tool governance) is a shared document with
//     five writers, so every write takes an expectedGeneration and a lost
//     compare-and-swap comes back as *ConflictError — see IsConflict and
//     write.go. Zero means "do not check".
//   - Everything else (secrets, skills, agent tokens, client adaptation,
//     OAuth state) writes state that is NOT the registry. Those methods take
//     no generation: there is no shared document to lose a race against.
//
// Two red lines are enforced by the TYPES rather than by convention: no
// exported type here has a field a credential value could be assigned to
// (TokenCreated.Value is the single, deliberate exception — a minted token
// has to leave the process exactly once), and the three-state selectors
// spell "block everything" explicitly, so it can never collapse into
// "allow everything" by way of a dropped empty list.
//
// Because internal/platform is off-limits, the default control-socket path
// is re-implemented here (see paths.go); a cross-package contract test on
// the internal/ctlapi side pins both implementations together.
//
// The Health string constants in this package are frozen wire values
// (docs/subsystems/docs/subsystems/controlplane.md): every frontend renders them verbatim and the TS
// constants for the GUI are generated from this package.
package api
