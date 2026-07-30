package ctlapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/dinstein/agent-hub/api"
	"github.com/dinstein/agent-hub/internal/skills"
)

func nrSkillView(id string, enabled bool, state skills.ApplyState) skills.SkillView {
	return skills.SkillView{
		Skill: skills.Skill{
			ID: id, Name: "Skill " + id, Description: "d", Kind: skills.KindSkillPack,
			Enabled: enabled, Fingerprint: "v1:abc", UpdatedAt: time.Unix(1700000000, 0).UTC(),
		},
		Library: skills.LibraryOK,
		Installs: []skills.InstallView{{
			Install: skills.InstallState{
				SkillID: id, ClientID: "claude-code", Scope: skills.ScopeUser,
				Path: "/home/u/.claude/skills/" + id, State: skills.StateApplied,
			},
			// The LIVE state differs from the receipt's on purpose.
			State: state, Detail: "library moved on",
		}},
		Granularity: skills.GranularityClient,
	}
}

func TestSkillsList(t *testing.T) {
	lib := &nrSkills{views: []skills.SkillView{nrSkillView("writer", true, skills.StateStale)}}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Skills = lib })

	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/skills", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out []api.Skill
	nrData(t, body, &out)
	if len(out) != 1 {
		t.Fatalf("got %d skills: %+v", len(out), out)
	}
	sk := out[0]
	if sk.ID != "writer" || sk.Name != "Skill writer" || !sk.Enabled || sk.Fingerprint != "v1:abc" {
		t.Errorf("skill = %+v", sk)
	}
	if sk.UpdatedAt == nil || sk.UpdatedAt.Unix() != 1700000000 {
		t.Errorf("updatedAt = %v", sk.UpdatedAt)
	}
	if len(sk.Installs) != 1 {
		t.Fatalf("installs = %+v", sk.Installs)
	}
	in := sk.Installs[0]
	// The live state wins over the receipt's, and "stale" is spelled
	// "outdated" on the wire.
	if in.State != api.ApplyStateOutdated {
		t.Errorf("state = %q, want %q", in.State, api.ApplyStateOutdated)
	}
	if in.ClientID != "claude-code" || in.Scope != skills.ScopeUser || in.Detail != "library moved on" {
		t.Errorf("install = %+v", in)
	}
}

// TestSkillApplyStateMapping pins the vocabulary bridge, including the
// fail direction: an unknown state must never read as "applied".
func TestSkillApplyStateMapping(t *testing.T) {
	for in, want := range map[skills.ApplyState]string{
		skills.StateApplied:  api.ApplyStateApplied,
		skills.StateStale:    api.ApplyStateOutdated,
		skills.StateDrifted:  api.ApplyStateDrifted,
		skills.StateMissing:  api.ApplyStateMissing,
		skills.StateConflict: api.ApplyStateConflict,
		"some-future-state":  api.ApplyStateConflict,
		"":                   api.ApplyStateConflict,
	} {
		if got := apiApplyState(in); got != want {
			t.Errorf("apiApplyState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSkillsListError(t *testing.T) {
	lib := &nrSkills{listErr: skills.ErrStoreCorrupt}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Skills = lib })

	status, body := nrDo(t, env.sock, http.MethodGet, "/v1/skills", nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", status, body)
	}
}

func TestSkillPatchEnableDisable(t *testing.T) {
	lib := &nrSkills{}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Skills = lib })

	on := true
	status, body := nrDo(t, env.sock, http.MethodPatch, "/v1/skills/writer",
		SkillPatchRequest{Enabled: &on})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var sk api.Skill
	nrData(t, body, &sk)
	if sk.ID != "writer" || !sk.Enabled {
		t.Errorf("skill = %+v", sk)
	}
	if !lib.enabled["writer"] {
		t.Errorf("Enable was not called")
	}

	off := false
	status, body = nrDo(t, env.sock, http.MethodPatch, "/v1/skills/writer",
		SkillPatchRequest{Enabled: &off})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	if lib.enabled["writer"] {
		t.Errorf("Disable was not called")
	}
}

// TestSkillPatchRequiresEnabled: an omitted field must not read as "disable".
func TestSkillPatchRequiresEnabled(t *testing.T) {
	lib := &nrSkills{}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Skills = lib })

	status, body := nrDo(t, env.sock, http.MethodPatch, "/v1/skills/writer", map[string]any{})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if code := nrErrCode(t, body); code != CodeBadRequest {
		t.Errorf("code = %s", code)
	}
	if len(lib.enabled) != 0 {
		t.Errorf("an empty patch flipped something: %+v", lib.enabled)
	}
}

func TestSkillPatchUnknownIs404(t *testing.T) {
	lib := &nrSkills{opErr: skills.ErrNotFound}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Skills = lib })

	on := true
	status, body := nrDo(t, env.sock, http.MethodPatch, "/v1/skills/nope",
		SkillPatchRequest{Enabled: &on})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", status, body)
	}
	if code := nrErrCode(t, body); code != CodeNotFound {
		t.Errorf("code = %s", code)
	}
}

func TestSkillInstall(t *testing.T) {
	lib := &nrSkills{}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Skills = lib })

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/skills/writer/install",
		SkillInstallRequest{ClientID: "claude-code", Scope: skills.ScopeProject, ProjectRoot: "/tmp/p"})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var in api.SkillInstall
	nrData(t, body, &in)
	if in.ClientID != "claude-code" || in.Scope != skills.ScopeProject || in.State != api.ApplyStateApplied {
		t.Errorf("install = %+v", in)
	}
	if lib.lastReq.SkillID != "writer" || lib.lastReq.ProjectRoot != "/tmp/p" {
		t.Errorf("request = %+v", lib.lastReq)
	}
}

func TestSkillInstallRequiresClient(t *testing.T) {
	lib := &nrSkills{}
	env := nrStart(t, func(d *NonRegistryDeps) { d.Skills = lib })

	status, body := nrDo(t, env.sock, http.MethodPost, "/v1/skills/writer/install",
		SkillInstallRequest{})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", status, body)
	}
	if lib.installed != nil {
		t.Errorf("a clientless request still installed something")
	}
}

// TestSkillInstallRefusalIs409: a refusal is the daemon working correctly
// and the operator having a decision to make, not a server fault.
func TestSkillInstallRefusalIs409(t *testing.T) {
	for _, err := range []error{skills.ErrDrifted, skills.ErrConflict, skills.ErrTampered} {
		lib := &nrSkills{opErr: err}
		env := nrStart(t, func(d *NonRegistryDeps) { d.Skills = lib })

		status, body := nrDo(t, env.sock, http.MethodPost, "/v1/skills/writer/install",
			SkillInstallRequest{ClientID: "claude-code"})
		if status != http.StatusConflict {
			t.Fatalf("%v: status = %d, want 409: %s", err, status, body)
		}
		if code := nrErrCode(t, body); code != CodeConflict {
			t.Errorf("%v: code = %s", err, code)
		}
	}
}
