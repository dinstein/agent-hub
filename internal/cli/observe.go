package cli

import (
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// What the four record readers share.
//
// `calls tail`, `events`, `logs` and `server logs` answer four questions about
// one installation, and somebody diagnosing something runs three of them
// inside a minute. Before this they disagreed on the two flags every one of
// them has: --since took a duration in three of them and a
// duration-or-timestamp-or-"all" in the fourth, and --limit meant "0 is all
// of them" in three and "0 is a usage error" in the fourth.
//
// Neither difference was a decision. They are one vocabulary now, and the
// help text is one sentence, so a flag that works on one works on all four.
//
// What is NOT unified is the shape above the flags. `calls` is a group
// because the ledger has verbs — enable, verify, prune, rotate-key — and
// `events` and `logs` are leaves because they have none; docs/conventions.md#command-naming
// rules on that, and inventing `events tail` to match would add a subcommand
// with nothing to distinguish it from its own parent.

const (
	// callsTailDefaultLimit and callsTailDefaultSince are `calls tail`'s own
	// defaults. The ledger holds every call ever made, unlike the three
	// bounded streams, so it is the one reader that starts with a window.
	callsTailDefaultLimit = 20
	callsTailDefaultSince = "24h"
	// sinceAll is the word that removes the lower bound. It is a word rather
	// than a magic empty value so a script can say what it means, and it is
	// the same word for every reader.
	sinceAll = "all"
)

// observeSince parses the shared --since vocabulary: a duration ("24h",
// "30m"), an RFC3339 instant, or "all" for no lower bound.
//
// The empty string is also no bound, which is what a caller that never set
// the flag passes.
func observeSince(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == sinceAll {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d < 0 {
			return time.Time{}, Usagef("--since %q is negative; it is an age, not an offset", raw)
		}
		return time.Now().Add(-d), nil
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts.UTC(), nil
	}
	e := Usagef("--since %q is neither a duration nor a timestamp", raw)
	e.Hint = "use a duration (24h, 30m), an RFC3339 time, or " + strconv.Quote(sinceAll)
	return time.Time{}, e
}

// observeSinceUsage is the one help string for --since.
func observeSinceUsage(noun string) string {
	return "only " + noun + " newer than this age (24h, 30m), an RFC3339 time, or " +
		strconv.Quote(sinceAll) + " for no bound"
}

// observeLimitUsage is the one help string for --limit.
func observeLimitUsage(noun string) string {
	return "how many " + noun + " to show (0 = all of them)"
}

// observeFollowUsage is the one help string for -f.
func observeFollowUsage(noun string) string {
	return "stay open and keep printing new " + noun + " as they arrive"
}

// bindObserveFlags registers the three flags every record reader has, so none
// of them can drift in name, shorthand or meaning.
func bindObserveFlags(cmd *cobra.Command, noun string, since *string, limit *int, follow *bool, defaultLimit int) {
	cmd.Flags().StringVar(since, "since", "", observeSinceUsage(noun))
	cmd.Flags().IntVar(limit, "limit", defaultLimit, observeLimitUsage(noun))
	cmd.Flags().BoolVarP(follow, "follow", "f", false, observeFollowUsage(noun))
}
