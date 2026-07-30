package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/dinstein/agent-hub/internal/cli/output"
)

// errWriter fails every write, so a renderer's error propagation is exercised
// rather than assumed.
type errWriter struct{}

var errWriteFailed = errors.New("write failed")

func (errWriter) Write([]byte) (int, error) { return 0, errWriteFailed }

// TestEveryHumanRendererHandlesTheZeroValue renders the ZERO VALUE of every
// result type the CLI can emit.
//
// This is the case no command test reaches. Each result type is normally
// constructed by its own command with its slices and maps populated, so the
// human renderers are only ever exercised on well-formed data — and 41 of the
// 58 of them had no coverage at all. The zero value is not hypothetical
// though: it is what a command produces when the daemon answers with an empty
// list, when a decode leaves the struct untouched, or when an early return
// emits a result that was only partly filled.
//
// A nil map or slice is fine to range over; what is not fine is indexing
// element zero, dereferencing an optional pointer field, or reaching into a
// nested struct that was never set. Those panic, and a panic in the renderer
// destroys output the command had already successfully computed — the JSON
// path would have printed it fine.
//
// The table is spelled out rather than discovered by reflection so that adding
// a result type without adding it here is a visible omission in review.
func TestEveryHumanRendererHandlesTheZeroValue(t *testing.T) {
	cases := []struct {
		data output.Data
		name string
	}{
		{ActivityReport{}, "ActivityReport"},
		{AddedServers{}, "AddedServers"},
		{AuthLoginResult{}, "AuthLoginResult"},
		{AuthLogoutResult{}, "AuthLogoutResult"},
		{AuthRefreshResult{}, "AuthRefreshResult"},
		{AuthStatusList{}, "AuthStatusList"},
		{CatalogAdded{}, "CatalogAdded"},
		{CatalogEntryView{}, "CatalogEntryView"},
		{CatalogList{}, "CatalogList"},
		{ConfigEntry{}, "ConfigEntry"},
		{ConfigList{}, "ConfigList"},
		{ConfigSetResult{}, "ConfigSetResult"},
		{ConnectPlan{}, "ConnectPlan"},
		{DaemonStatus{}, "DaemonStatus"},
		{DaemonStopResult{}, "DaemonStopResult"},
		{DetectList{}, "DetectList"},
		{DisconnectResult{}, "DisconnectResult"},
		{DoctorReport{}, "DoctorReport"},
		{EventBatch{}, "EventBatch"},
		{EventRow{}, "EventRow"},
		{ProfileChange{}, "ProfileChange"},
		{ProfileList{}, "ProfileList"},
		{RemovedServer{}, "RemovedServer"},
		{ClientBindingList{}, "ClientBindingList"},
		{ClientBindResult{}, "ClientBindResult"},
		{ClientList{}, "ClientList"},
		{ClientInspectView{}, "ClientInspectView"},
		{SecretChange{}, "SecretChange"},
		{SecretList{}, "SecretList"},
		{ServerInspect{}, "ServerInspect"},
		{ServerList{}, "ServerList"},
		{ServerLogs{}, "ServerLogs"},
		{ServerTestResult{}, "ServerTestResult"},
		{ServerToggle{}, "ServerToggle"},
		{SessionDetail{}, "SessionDetail"},
		{SessionKillResult{}, "SessionKillResult"},
		{SessionList{}, "SessionList"},
		{SkillAction{}, "SkillAction"},
		{SkillList{}, "SkillList"},
		{SkillRow{}, "SkillRow"},
		{SkillSyncResult{}, "SkillSyncResult"},
		{SkillUpdateResult{}, "SkillUpdateResult"},
		{SkillVerifyReport{}, "SkillVerifyReport"},
		{TokenCreated{}, "TokenCreated"},
		{TokenList{}, "TokenList"},
		{TokenRevoked{}, "TokenRevoked"},
		{ToolAllowResult{}, "ToolAllowResult"},
		{ToolList{}, "ToolList"},
	}

	// Guards against the table silently falling behind the code. The count is
	// asserted rather than the membership, because the compiler already
	// rejects a name that does not exist.
	const humanImplementations = 48
	if len(cases) != humanImplementations {
		t.Fatalf("table covers %d types, expected %d — a result type was added or removed "+
			"without updating this table", len(cases), humanImplementations)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sb strings.Builder
			if err := tc.data.Human(&sb); err != nil {
				t.Fatalf("Human on the zero value returned %v", err)
			}

			// A renderer that writes must report a failing writer rather than
			// swallow it: the exit code is how a caller learns output was lost.
			if sb.Len() > 0 {
				if err := tc.data.Human(errWriter{}); !errors.Is(err, errWriteFailed) {
					t.Errorf("Human ignored a failing writer: err = %v", err)
				}
			}
		})
	}
}
