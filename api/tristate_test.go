package api

import (
	"encoding/json"
	"testing"
)

// The three-state selectors are the one place in this package where a wrong
// encoding is a SECURITY bug rather than a display bug: "expose no tool at
// all" and "expose every tool" are one dropped empty slice apart, and the
// collapse always goes in the fail-OPEN direction.
//
// These tests pin both halves of that contract:
//
//	encoding — block-all must reach the wire as a distinguishable value;
//	decoding — an absent list and an empty list must not read the same.

// TestToolSelectorEncodingGolden freezes the wire form of the three tool
// selector states.
func TestToolSelectorEncodingGolden(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"tools_all", AllTools(), `{"mode":"all"}`},
		{"tools_only", OnlyTools("search", "get_issue"), `{"mode":"only","tools":["search","get_issue"]}`},
		// The critical one: block-all carries its own mode, so there is no
		// empty list for a marshaller to drop.
		{"tools_none", NoTools(), `{"mode":"none"}`},
		// A caller that forgot the mode sends "" and gets refused daemon
		// side. It must NOT silently encode as "all".
		{"tools_unset_is_not_all", ProfileTools{}, `{"mode":""}`},
		{
			"tools_edit_scoped_to_server",
			ProfileToolsEdit{Server: "jira", ProfileTools: NoTools()},
			`{"server":"jira","mode":"none"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("frozen wire shape changed\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestServerSetEncodingGolden freezes the member-set three states. The
// pointer and the missing omitempty are what keep "block-all" and "no
// narrowing" apart on the wire.
func TestServerSetEncodingGolden(t *testing.T) {
	none := []string{}
	some := []string{"github"}
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"create_no_narrowing", profileCreateBody{Name: "dev"}, `{"name":"dev"}`},
		{"create_block_all", profileCreateBody{Name: "dev", Servers: &none}, `{"name":"dev","servers":[]}`},
		{"create_subset", profileCreateBody{Name: "dev", Servers: &some}, `{"name":"dev","servers":["github"]}`},

		{"edit_replace_clear", ServerSetEdit{Mode: ServerSetReplace}, `{"mode":"replace","servers":null}`},
		{"edit_replace_block_all", ServerSetEdit{Mode: ServerSetReplace, Servers: none}, `{"mode":"replace","servers":[]}`},
		{"edit_add", ServerSetEdit{Mode: ServerSetAdd, Servers: some}, `{"mode":"add","servers":["github"]}`},

		{"binding_untouched", ClientBinding{}, `{}`},
		{"binding_block_all", ClientBinding{Servers: &none}, `{"servers":[]}`},
		{"binding_subset", ClientBinding{Servers: &some}, `{"servers":["github"]}`},

		// A token allowlist has the same three states, and the same
		// fail-open collapse if the empty list is ever dropped.
		{"token_every_server", TokenSpec{Name: "ci", Tier: TierRead}, `{"name":"ci","tier":"read","servers":null}`},
		{"token_no_server", TokenSpec{Name: "ci", Tier: TierRead, Servers: none}, `{"name":"ci","tier":"read","servers":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("frozen wire shape changed\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestStoredSelectorDecodingKeepsBlockAll proves the READ side: a stored
// selector's absent / empty / populated allow list must decode to three
// distinguishable values, and Blocked() must answer for the middle one.
func TestStoredSelectorDecodingKeepsBlockAll(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantBlocked bool
		wantNil     bool
	}{
		{"no_rule", `{}`, false, true},
		{"block_all", `{"allow":[]}`, true, false},
		{"subset", `{"allow":["search"]}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sel ToolSelector
			if err := json.Unmarshal([]byte(tc.raw), &sel); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := sel.Blocked(); got != tc.wantBlocked {
				t.Errorf("Blocked() = %v, want %v (allow=%#v)", got, tc.wantBlocked, sel.Allow)
			}
			if got := sel.Allow == nil; got != tc.wantNil {
				t.Errorf("allow nil = %v, want %v", got, tc.wantNil)
			}
		})
	}
}

// TestProfileDecodingKeepsBlockAll is the same proof one level up: a profile
// whose member set is present and empty exposes NO server, and must not read
// like a profile that simply never narrowed.
func TestProfileDecodingKeepsBlockAll(t *testing.T) {
	var noRule, blockAll Profile
	if err := json.Unmarshal([]byte(`{"name":"open"}`), &noRule); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"name":"closed","servers":[]}`), &blockAll); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if noRule.Blocked() {
		t.Error("a profile without a servers key must not read as block-all")
	}
	if !blockAll.Blocked() {
		t.Error("a profile with an EMPTY servers list is block-all; collapsing it is fail-open")
	}
	// And the distinction survives a re-encode, so a read-modify-write does
	// not quietly widen the profile it round-trips.
	back, err := json.Marshal(blockAll)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(back) != `{"name":"closed","servers":[]}` {
		t.Errorf("re-encode dropped the empty list: %s", back)
	}
}

// TestServerEntryEncodesEveryKey pins the wholesale-replacement contract: the
// daemon merges a PATCH by key presence, so an entry that omitted its empty
// fields could never CLEAR one — a leaked env var or a stale mount would be
// unremovable through this API.
func TestServerEntryEncodesEveryKey(t *testing.T) {
	got, err := json.Marshal(ServerEntry{Command: "npx", Enabled: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"transport":"","command":"npx","args":null,"env":null,"cwd":"","url":"",` +
		`"headers":null,"oauth":null,"provenance":"","derive":"","runtime":"",` +
		`"docker":null,"enabled":true,"source":""}`
	if string(got) != want {
		t.Errorf("frozen wire shape changed\n got: %s\nwant: %s", got, want)
	}
}

// TestSecretTypesCarryNoValue is a structural guarantee, not a convention:
// no EXPORTED type in this package may have a field that could hold a
// credential value. The check is by name over the DTOs that touch secrets
// and tokens — a future field called Value/Secret/Password/Token on one of
// them fails here before it can reach a log or a screenshot.
func TestSecretTypesCarryNoValue(t *testing.T) {
	forbidden := map[string]bool{"value": true, "secret": true, "password": true, "plaintext": true}
	for _, tc := range []struct {
		name string
		v    any
	}{
		{"SecretRef", SecretRef{}},
		{"SecretChange", SecretChange{}},
		{"Token", Token{}},
		{"TokenRevoked", TokenRevoked{}},
		{"AuthStatus", AuthStatus{}},
		{"AuthRefreshed", AuthRefreshed{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for k := range fields {
				if forbidden[k] {
					t.Errorf("%s must not carry a %q field: a credential must never travel outward", tc.name, k)
				}
			}
		})
	}
	// TokenCreated is the ONE deliberate exception: a minted token has to
	// leave the process exactly once, or it could not be given to an agent.
	raw, err := json.Marshal(TokenCreated{Value: "agt_x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fields["value"]; !ok {
		t.Error("TokenCreated must carry the plaintext: it is the only place it ever appears")
	}
}
