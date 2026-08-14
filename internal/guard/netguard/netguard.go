// Package netguard provides the SSRF host/IP predicates and the dial-time
// control hook (docs/subsystems/guard.md, internal/guard/netguard).
//
// The core insight is that "is this host private?" is TWO questions with
// opposite failure directions, so there are two exported predicates:
//
//   - HostIsPrivate — used to REFUSE (e.g. an OAuth redirect target, a
//     remote server URL). FAIL-CLOSED: resolution failure or any
//     uncertainty answers true, so the refusal happens.
//   - HostIsDefinitelyPrivate — used to GRANT trust (e.g. relaxing a
//     localhost-only rule). FAIL-TO-FALSE: only a literal private address
//     or a localhost name answers true; DNS is never believed for this
//     direction, because DNS answers change (rebinding).
//
// Refusing with the fail-to-false one is a third and much narrower use, and
// every caller it has today is of that kind: confops screens a URL as the
// operator types it, where the fail-closed predicate would reject a laptop
// with no network and a name that only resolves inside a VPN. That is sound
// only IN FRONT OF a fail-closed screen that runs later — internal/downstream
// screens the name before connecting and the address at dial time. On its own
// it refuses the certain cases and waves everything else through, which is the
// shape of no protection at all, so a caller reaching for it this way has to
// be able to name the check that runs after it.
//
// A hostname check alone is TOCTOU-vulnerable to DNS rebinding: the name
// resolves public at check time and private at dial time. DialControl closes
// that hole — install it as net.Dialer.Control and every connection is
// re-screened against the ACTUAL resolved address the socket is about to
// connect to:
//
//	d := &net.Dialer{Control: netguard.DialControl}
//
// "Private" throughout means "not publicly routable": RFC1918, loopback,
// link-local, ULA, deprecated IPv6 site-local, CGNAT, unspecified, multicast,
// documentation/benchmark ranges, and v4-mapped v6 forms of all of these.
//
// Dependency constraint (canonical.md §2 rule 4, depguard-enforced): only
// the standard library plus internal/guard.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"github.com/dinstein/agent-hub/internal/guard"
)

// BlockedError is the typed rejection DialControl returns. It unwraps to
// guard.ErrBlocked so callers can classify with errors.Is.
type BlockedError struct {
	// Addr is the dial address that was rejected.
	Addr string
	// Reason explains the rejection.
	Reason string
}

// Error implements error.
func (e *BlockedError) Error() string {
	return fmt.Sprintf("netguard: blocked dial to %s: %s", e.Addr, e.Reason)
}

// Unwrap ties BlockedError to the guard.ErrBlocked sentinel.
func (e *BlockedError) Unwrap() error { return guard.ErrBlocked }

// resolveTimeout bounds the DNS lookup inside HostIsPrivate. On expiry the
// lookup errors and the predicate answers true (fail-closed).
const resolveTimeout = 5 * time.Second

// lookupNetIP is swappable in tests; production uses the default resolver.
var lookupNetIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// Non-public ranges beyond what netip.Addr's own classifiers cover.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT (RFC 6598)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking (RFC 2544)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved (includes broadcast)
	netip.MustParsePrefix("100::/64"),        // discard-only (RFC 6666)
	netip.MustParsePrefix("2001:db8::/32"),   // v6 documentation

	// Deprecated IPv6 site-local. netip.Addr has no classifier for it —
	// IsPrivate covers the ULA replacement fc00::/7 and stops there — so
	// without this line fec0::1 was not private to AddrIsPrivate, and both
	// the hostname-time screen and the dial-time DialControl consult that
	// one function, which is what made a single gap close both doors at
	// once. Deprecated by RFC 3879 and unroutable on the public internet,
	// but hosts that still hold a route to it are exactly the hosts where
	// reaching it means something.
	netip.MustParsePrefix("fec0::/10"), // site-local (RFC 3879, deprecated)

	// The v4-embedding ranges. All four carry an IPv4 address inside an IPv6
	// one, so classifying the v6 form on its own merits answers the wrong
	// question: ::127.0.0.1, 2002:7f00:1::, 64:ff9b::7f00:1 and
	// 64:ff9b:1::7f00:1 all name 127.0.0.1, and none of them looks private to
	// IsLoopback.
	//
	// Blocked wholesale rather than by decoding the embedded v4 and
	// classifying that. Both of the deprecated forms exist only as a way to
	// spell an IPv4 address, and neither has a use worth preserving: IPv4-
	// compatible was deprecated by RFC 4291 and 6to4's relays were shut down
	// (RFC 7526). The two NAT64 prefixes are not deprecated, but a wholesale
	// block is still the right call for them: the well-known prefix already
	// takes this shape below, and a NAT64 translator's whole job is to carry
	// an arbitrary v4 destination across the v6 side, so decoding and
	// re-judging the embedded address would only reintroduce the address
	// exactly the SSRF screen exists to catch. Refusing the whole range costs
	// nothing real and needs no decoder to be correct.
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 well-known prefix (RFC 6052)
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use prefix (RFC 8215)
	netip.MustParsePrefix("::/96"),          // IPv4-compatible (RFC 4291 §2.5.5.1, deprecated)
	netip.MustParsePrefix("2002::/16"),      // 6to4 (RFC 3056, deprecated by RFC 7526)
}

// AddrIsPrivate reports whether a resolved address is NOT publicly routable
// (see package doc for the covered ranges). v4-mapped v6 addresses are
// unmapped first, so ::ffff:10.0.0.1 classifies as 10.0.0.1.
//
// Failure direction: fail-closed — the invalid (zero) Addr answers true.
func AddrIsPrivate(a netip.Addr) bool {
	if !a.IsValid() {
		return true // fail-closed: an unclassifiable address is never trusted as public
	}
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsUnspecified() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() {
		return true
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// HostIsPrivate reports whether host (a bare hostname, an IP literal, or
// either with a :port) refers to a private/non-public destination. Use it to
// REFUSE access.
//
// Failure direction: FAIL-CLOSED — an empty host, a failed or timed-out DNS
// resolution, or an empty answer all return true (the caller refuses). A
// hostname resolving to ANY private address returns true: one attacker-
// controlled A record must be enough to trip the refusal.
//
// Note: a true answer here is necessary but not sufficient protection —
// rebinding can still flip the name after this check. Pair it with
// DialControl, which screens the actual dialed IP.
func HostIsPrivate(host string) bool {
	h := canonicalHost(host)
	if h == "" {
		return true // fail-closed
	}
	if isLocalhostName(h) {
		return true
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return AddrIsPrivate(a)
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	addrs, err := lookupNetIP(ctx, h)
	if err != nil || len(addrs) == 0 {
		return true // fail-closed: unresolvable is refused, not trusted
	}
	for _, a := range addrs {
		if AddrIsPrivate(a) {
			return true
		}
	}
	return false
}

// HostIsDefinitelyPrivate reports whether host CERTAINLY refers to a private
// destination. Use it to GRANT trust (e.g. treating a target as local).
//
// Failure direction: FAIL-TO-FALSE — only a literal IP that classifies as
// private-for-trust (loopback, RFC1918, ULA, link-local, unspecified) or a
// localhost name returns true. Hostnames are NEVER resolved here: a DNS
// answer is a claim the zone owner can change at will (rebinding), so it can
// deny trust but never confer it. Anything uncertain returns false.
func HostIsDefinitelyPrivate(host string) bool {
	h := canonicalHost(host)
	if h == "" {
		return false // fail-to-false: uncertainty never grants trust
	}
	if isLocalhostName(h) {
		return true
	}
	a, err := netip.ParseAddr(h)
	if err != nil {
		return false // not a literal: cannot be *definitely* private
	}
	a = a.Unmap()
	// Narrower than AddrIsPrivate on purpose: documentation/benchmark/CGNAT
	// ranges are non-routable but not *locally private*, so they must not
	// unlock local-only trust.
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsUnspecified()
}

// AddrIsLoopback reports whether a host[:port] authority names a loopback
// host, and is the repository's one answer to that question:
// internal/httpbridge binds the MCP face by it and screens a browser's
// Origin with it, internal/daemon decides from it whether an operator must
// confirm exposure to the network, and internal/diag refuses to publish
// profiles anywhere else. Two implementations would eventually disagree, and
// the direction they disagree in is the one that publishes tool execution,
// or a heap dump holding credentials, to a LAN.
//
// It lives beside the outbound predicates rather than in the package that
// binds, because the packages that need it sit on different layers and the
// lower ones must not import the higher to ask. Note it is not
// interchangeable with HostIsDefinitelyPrivate: this reads an authority on
// THIS side ("where am I accepting from, and who is calling"), that one
// reads a destination ("may I connect there"), and only that one has a
// rebinding problem to defend against.
//
// Failure direction: FAIL-TO-FALSE, the same as HostIsDefinitelyPrivate and
// for the same reason — the answer GRANTS a weaker authorization, so
// everything it cannot prove is loopback must read as not loopback: an empty
// host (which binds every interface), a hostname other than localhost, an
// unparsable address.
func AddrIsLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false // ":8080" listens on every interface
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// DialControl is a net.Dialer.Control hook that rejects connections to
// private/non-public addresses. The address it sees is the one the socket is
// about to connect to — already resolved — which closes the DNS-rebind
// TOCTOU window left open by any hostname-level precheck.
//
// Failure direction: FAIL-CLOSED — an address that cannot be parsed is
// rejected, and every rejection satisfies errors.Is(err, guard.ErrBlocked).
// It strips the port and brackets itself rather than calling canonicalHost,
// and the two must not be merged on the strength of looking alike.
// canonicalHost trims surrounding whitespace, so it parses inputs this one
// refuses: " 8.8.8.8:53 " is unparsable here and a public address there, which
// turns a rejection into a dial. The dialer only ever hands Control the
// resolved host:port it built itself, so no such input can arrive — but a
// fail-closed predicate is the wrong place to widen what is accepted on the
// strength of an argument about what cannot happen.
func DialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	if err != nil {
		// fail-closed: Control must only ever see resolved IP literals; an
		// unparsable address is refused rather than guessed at.
		return &BlockedError{Addr: address, Reason: "unparsable dial address"}
	}
	if AddrIsPrivate(a) {
		return &BlockedError{Addr: address, Reason: "private or non-public address"}
	}
	return nil
}

// canonicalHost strips an optional :port and IPv6 brackets, returning the
// bare host. It never fails; unparsable inputs pass through unchanged and
// fall to each predicate's failure direction.
func canonicalHost(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		host = h
	}
	return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
}

// isLocalhostName reports whether host is "localhost" or ends in
// ".localhost" (RFC 6761 reserves the whole tree for loopback).
func isLocalhostName(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}
