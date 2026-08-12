package eventlog

import "slices"

// Class separates the hub running as intended from the hub reacting to
// something that went wrong.
//
// It exists because the question "what is unusual here" had no answer a
// consumer could ask for. The severity of the PROSE half is not that answer:
// slog levels are one-dimensional, so `health_down` is a warning and
// `health_up` is not — and a `logs --level warn` view therefore shows a
// server going down and never coming back, which is the exact reading a
// recovery line was added to prevent.
//
// The two questions are genuinely different. Level asks how loudly one record
// should speak in a stream of prose. Class asks which STORY a record belongs
// to, and a story includes how it ended.
type Class string

const (
	// ClassRoutine is what happens when everything works: a server connects,
	// a client attaches, a session opens and closes, config is adopted.
	ClassRoutine Class = "routine"
	// ClassDisruption is a failure, the hub's reaction to one, and the
	// recovery that ends it.
	//
	// The recovery kinds — circuit_closed, health_up — are deliberately in
	// here rather than in routine. They are the last act of the same episode,
	// and a filter that dropped them would show every outage beginning and
	// none of them ending: the worst possible answer, because it reads as an
	// outage still in progress.
	ClassDisruption Class = "disruption"
)

// classOrder is how the classes are offered in help text: the ordinary case
// first, so a reader meets the default before the exception.
var classOrder = []Class{ClassRoutine, ClassDisruption}

// disruptions is the closed list of kinds that are NOT routine.
//
// Written as the exception rather than as a full table on purpose: a new kind
// defaults to routine, and the failure that produces — a fault that does not
// show up under `--class disruption` — is caught by the test that walks every
// kind. The reverse default would be worse: a routine kind wrongly marked as
// a disruption makes the filter useless by filling it with noise, and nothing
// would ever flag that.
var disruptions = []Kind{
	// Something is wrong now.
	KindConnectFailed, KindRespawnFailed, KindCircuitOpen, KindHealthDown,
	KindOAuthRefreshFailed, KindOAuthGrantRevoked, KindSecretsMissing, KindOAuthLoginFailed,
	KindRegistryReloadFailed,
	// The hub reacting to it. A connection that ended and a child that was
	// restarted are not failures in themselves — a planned shutdown ends
	// connections too — but they are never part of a system simply running,
	// and an operator scanning for what moved wants them.
	KindDisconnected, KindRespawned, KindCircuitHalfOpen,
	// The end of the episode.
	KindCircuitClosed, KindHealthUp,
}

// ClassOf reports which class a kind belongs to. An unknown kind is routine:
// a record from a newer build must not be able to fill somebody's disruption
// filter with something this build cannot even name.
func ClassOf(kind Kind) Class {
	if slices.Contains(disruptions, kind) {
		return ClassDisruption
	}
	return ClassRoutine
}

// ClassNames lists every class in presentation order, as strings, for help
// text and error hints.
func ClassNames() []string {
	out := make([]string, 0, len(classOrder))
	for _, c := range classOrder {
		out = append(out, string(c))
	}
	return out
}

// KnownClass reports whether class is one this package defines.
func KnownClass(class Class) bool { return slices.Contains(classOrder, class) }
