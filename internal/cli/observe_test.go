package cli

import (
	"strings"
	"testing"
)

// The four record readers must accept the same --since vocabulary. Before
// this, `calls tail --since all` worked and `logs --since all` was a parse
// error, which is the kind of difference somebody discovers mid-incident.
func TestObserveReadersShareTheSinceVocabulary(t *testing.T) {
	setDataDir(t)
	for _, cmd := range [][]string{
		{"calls", "tail"}, {"events"}, {"logs"}, {"server", "logs", "any"},
	} {
		for _, since := range []string{"24h", "all", "2020-01-02T10:00:00Z"} {
			args := append(append([]string{}, cmd...), "--since", since)
			code, _, errOut := runCLI(t, "", args...)
			// Exit 0 or a NOT-FOUND (nothing recorded yet) are both fine;
			// what must not happen is a usage error about the flag itself.
			if strings.Contains(errOut, "--since") {
				t.Errorf("%v --since %s was refused: %s", cmd, since, errOut)
			}
			if code == 2 {
				t.Errorf("%v --since %s exited with a usage error: %s", cmd, since, errOut)
			}
		}
	}
}

// And the same --limit meaning. 0 was "all of them" in three readers and a
// usage error in the fourth.
func TestObserveReadersShareTheLimitMeaning(t *testing.T) {
	setDataDir(t)
	for _, cmd := range [][]string{
		{"calls", "tail"}, {"events"}, {"logs"}, {"server", "logs", "any"},
	} {
		args := append(append([]string{}, cmd...), "--limit", "0")
		code, _, errOut := runCLI(t, "", args...)
		if code == 2 || strings.Contains(errOut, "--limit") {
			t.Errorf("%v --limit 0 was refused: %s", cmd, errOut)
		}
	}
}

func TestObserveSinceRejectsWhatIsNeitherDurationNorTime(t *testing.T) {
	if _, err := observeSince("yesterday"); err == nil {
		t.Fatal("a word that is neither was accepted")
	}
	// A negative age is the one shape that parses as a duration and cannot
	// mean anything: --since is an age, not an offset into the future.
	if _, err := observeSince("-1h"); err == nil {
		t.Fatal("a negative age was accepted")
	}
	for _, ok := range []string{"", "all", "24h", "2020-01-02T10:00:00Z"} {
		if _, err := observeSince(ok); err != nil {
			t.Errorf("observeSince(%q) = %v", ok, err)
		}
	}
}
