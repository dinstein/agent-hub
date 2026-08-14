package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dinstein/agent-hub/internal/clients"
	"github.com/dinstein/agent-hub/internal/skills"
)

// The `skill` group manages agent assets (SKILL.md packages, slash commands,
// subagent definitions). Its shape is deliberately isomorphic to the server
// group — add / ls / inspect / rm / enable / disable plus an integrity
// fingerprint — so "manage an agent asset" is one learnable pattern rather
// than an MCP special case (docs/modules/controlplane.md point 7).
//
// Two layers, always distinguished in the output: the LIBRARY (agenthub's
// canonical copy, content-addressed and pinned) and the INSTALLS (the bytes
// materialized into a client's directory). Granularity is always "client":
// file materialization cannot reach per-session precision, and every result
// says so instead of implying otherwise.

// SkillRow is one library entry with its install points.
type SkillRow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Version     string `json:"version"`
	Enabled     bool   `json:"enabled"`
	Source      string `json:"source"`
	SourcePath  string `json:"source_path,omitempty"`
	Fingerprint string `json:"fingerprint"`
	// Library is ok / tampered / unpinned / missing.
	Library     string          `json:"library"`
	Description string          `json:"description,omitempty"`
	Installs    []SkillInstall  `json:"installs"`
	Files       []SkillFileInfo `json:"files,omitempty"`
	Granularity string          `json:"granularity"`
}

// SkillInstall is one materialization point plus its live state.
type SkillInstall struct {
	Client string `json:"client"`
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	// State is recomputed against disk, so it can differ from what the
	// last write recorded.
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// SkillFileInfo is one file of a package (inspect only).
type SkillFileInfo struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// SkillList is the `skill ls` result.
type SkillList struct {
	Skills      []SkillRow `json:"skills"`
	Granularity string     `json:"granularity"`
}

// Human renders the library table.
func (l SkillList) Human(w io.Writer) error {
	if len(l.Skills) == 0 {
		_, err := fmt.Fprintln(w, "no skills in the library")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tKIND\tVERSION\tENABLED\tLIBRARY\tINSTALLS")
	for _, s := range l.Skills {
		installs := make([]string, 0, len(s.Installs))
		for _, in := range s.Installs {
			installs = append(installs, in.Client+":"+in.State)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.ID, s.Kind, s.Version, boolText(s.Enabled), s.Library, dash(strings.Join(installs, ",")))
	}
	return tw.Flush()
}

// Human renders one library entry in detail.
func (s SkillRow) Human(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "%s (%s) v%s  enabled=%s  library=%s\n",
		s.ID, s.Kind, s.Version, boolText(s.Enabled), s.Library); err != nil {
		return err
	}
	if s.Description != "" {
		if _, err := fmt.Fprintf(w, "%s\n", oneLine(s.Description, descriptionColumnBytes)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "source: %s %s\nfingerprint: %s\ngranularity: %s\n",
		s.Source, dash(s.SourcePath), s.Fingerprint, s.Granularity); err != nil {
		return err
	}
	for _, in := range s.Installs {
		if _, err := fmt.Fprintf(w, "install: %s (%s) %s -> %s %s\n",
			in.Client, in.Scope, in.State, in.Path, in.Detail); err != nil {
			return err
		}
	}
	for _, f := range s.Files {
		if _, err := fmt.Fprintf(w, "file: %s (%d bytes) %s\n", f.Path, f.Size, f.SHA256[:min(12, len(f.SHA256))]); err != nil {
			return err
		}
	}
	return nil
}

// SkillAction is the result of the small mutating subcommands.
type SkillAction struct {
	Action      string   `json:"action"`
	SkillID     string   `json:"skill_id"`
	Removed     []string `json:"removed_installs,omitempty"`
	Conflicts   []string `json:"conflicts,omitempty"`
	Granularity string   `json:"granularity"`
	Note        string   `json:"note,omitempty"`
}

// Human renders the action.
func (r SkillAction) Human(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "%s: %s\n", r.Action, r.SkillID); err != nil {
		return err
	}
	for _, p := range r.Removed {
		if _, err := fmt.Fprintf(w, "unmaterialized: %s\n", p); err != nil {
			return err
		}
	}
	for _, c := range r.Conflicts {
		if _, err := fmt.Fprintf(w, "conflict: %s\n", c); err != nil {
			return err
		}
	}
	if r.Note != "" {
		_, err := fmt.Fprintf(w, "note: %s\n", r.Note)
		return err
	}
	return nil
}

// SkillSyncResult is the `skill sync` / `skill install-to` result.
type SkillSyncResult struct {
	Client      string          `json:"client"`
	Scope       string          `json:"scope"`
	Items       []SkillSyncItem `json:"items"`
	Changed     bool            `json:"changed"`
	Granularity string          `json:"granularity"`
}

// SkillSyncItem is one skill's outcome.
type SkillSyncItem struct {
	SkillID string `json:"skill_id"`
	Action  string `json:"action"`
	State   string `json:"state"`
	Path    string `json:"path,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Human renders the sync table.
func (r SkillSyncResult) Human(w io.Writer) error {
	if len(r.Items) == 0 {
		_, err := fmt.Fprintf(w, "nothing to materialize for %s (%s scope)\n", r.Client, r.Scope)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SKILL\tACTION\tSTATE\tPATH\tDETAIL")
	for _, it := range r.Items {
		detail := it.Detail
		if it.Error != "" {
			detail = it.Error
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", it.SkillID, it.Action, it.State, dash(it.Path), detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "changed=%s granularity=%s (every session of this client shares these bytes)\n",
		boolText(r.Changed), r.Granularity)
	return err
}

// SkillVerifyReport is the `skill verify` result.
type SkillVerifyReport struct {
	OK          bool               `json:"ok"`
	Skills      []SkillVerifyEntry `json:"skills"`
	Granularity string             `json:"granularity"`
}

// SkillVerifyEntry is one entry's three-way comparison.
type SkillVerifyEntry struct {
	SkillID           string         `json:"skill_id"`
	Library           string         `json:"library"`
	Detail            string         `json:"detail,omitempty"`
	Fingerprint       string         `json:"fingerprint,omitempty"`
	PinnedFingerprint string         `json:"pinned_fingerprint,omitempty"`
	Installs          []SkillInstall `json:"installs"`
}

// Human renders the verification report.
func (r SkillVerifyReport) Human(w io.Writer) error {
	for _, s := range r.Skills {
		if _, err := fmt.Fprintf(w, "%s: library=%s %s\n", s.SkillID, s.Library, s.Detail); err != nil {
			return err
		}
		for _, in := range s.Installs {
			if _, err := fmt.Fprintf(w, "  %s (%s): %s %s\n",
				in.Client, in.Scope, in.State, in.Detail); err != nil {
				return err
			}
		}
	}
	verdict := "FAIL"
	if r.OK {
		verdict = "ok"
	}
	_, err := fmt.Fprintf(w, "verify: %s\n", verdict)
	return err
}

// SkillUpdateResult is the `skill update` result.
type SkillUpdateResult struct {
	SkillID         string          `json:"skill_id"`
	Changed         bool            `json:"changed"`
	Check           bool            `json:"check"`
	FromVersion     string          `json:"from_version"`
	ToVersion       string          `json:"to_version"`
	FromFingerprint string          `json:"from_fingerprint"`
	ToFingerprint   string          `json:"to_fingerprint"`
	Reapplied       []SkillSyncItem `json:"reapplied,omitempty"`
	Detail          string          `json:"detail,omitempty"`
	Granularity     string          `json:"granularity"`
}

// Human renders the update outcome.
func (r SkillUpdateResult) Human(w io.Writer) error {
	verb := "updated"
	switch {
	case r.Check && r.Changed:
		verb = "would update"
	case r.Check:
		verb = "up to date"
	case !r.Changed:
		verb = "unchanged"
	}
	if _, err := fmt.Fprintf(w, "%s: %s %s -> %s\n", verb, r.SkillID, r.FromVersion, r.ToVersion); err != nil {
		return err
	}
	for _, it := range r.Reapplied {
		if _, err := fmt.Fprintf(w, "reapplied: %s %s %s\n", it.SkillID, it.Action, it.Path); err != nil {
			return err
		}
	}
	if r.Detail != "" {
		_, err := fmt.Fprintf(w, "note: %s\n", r.Detail)
		return err
	}
	return nil
}

func (a *App) newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "skill",
		Aliases: []string{"skills"}, // singular canonical, plural alias (canonical.md §3)
		Short:   "Manage agent skill packages (library + per-client materialization)",
		Args:    cobra.ArbitraryArgs,
		RunE:    groupRunE,
	}
	cmd.AddCommand(
		a.newSkillLsCmd(),
		a.newSkillInspectCmd(),
		a.newSkillAddCmd(),
		a.newSkillRmCmd(),
		a.newSkillToggleCmd(true),
		a.newSkillToggleCmd(false),
		a.newSkillInstallToCmd(),
		a.newSkillSyncCmd(),
		a.newSkillUpdateCmd(),
		a.newSkillVerifyCmd(),
	)
	return cmd
}

// skillManager opens the skills library under <data>/skills.
func (a *App) skillManager() (*skills.Manager, error) {
	data, err := a.resolver.DataDir()
	if err != nil {
		return nil, err
	}
	return skills.Open(filepath.Join(data, "skills"), skills.Options{
		LockTimeout:  a.lockTimeout,
		AgentVersion: a.version,
		BackupDir:    filepath.Join(data, "backups", "skills"),
	})
}

func skillRowOf(v skills.SkillView, withFiles bool) SkillRow {
	row := SkillRow{
		ID: v.Skill.ID, Name: v.Skill.Name, Kind: string(v.Skill.Kind),
		Version: v.Skill.Version, Enabled: v.Skill.Enabled,
		Source: string(v.Skill.Source.Kind), SourcePath: v.Skill.Source.Path,
		Fingerprint: v.Skill.Fingerprint, Library: string(v.Library),
		Description: v.Skill.Description, Installs: []SkillInstall{},
		Granularity: v.Granularity,
	}
	for _, in := range v.Installs {
		row.Installs = append(row.Installs, SkillInstall{
			Client: in.Install.ClientID, Scope: in.Install.Scope, Path: in.Install.Path,
			State: string(in.State), Detail: in.Detail,
		})
	}
	if withFiles {
		for _, f := range v.Skill.Files {
			row.Files = append(row.Files, SkillFileInfo{Path: f.Path, SHA256: f.SHA256, Size: f.Size})
		}
	}
	return row
}

func (a *App) newSkillAddCmd() *cobra.Command {
	var (
		name     string
		id       string
		kind     string
		pin      string
		gitURL   string
		disabled bool
	)
	cmd := &cobra.Command{
		Use:   "add <path> [--pin <rev>]",
		Short: "Import a skill package directory into the library (fingerprint pinned)",
		Long: "Import a local directory into the skill library.\n\n" +
			"agenthub performs no git operations: --git-url and --pin record\n" +
			"provenance for a checkout you already have, they do not fetch it. The\n" +
			"imported bytes are content-addressed and fingerprinted, so a later\n" +
			"'skill verify' can prove the library copy has not been edited.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := a.skillManager()
			if err != nil {
				return err
			}
			req := skills.AddRequest{
				Path: args[0], Name: name, ID: id, Pin: pin,
				GitURL: gitURL, Disabled: disabled,
			}
			if kind != "" {
				req.Kind = skills.SkillKind(kind)
			}
			if gitURL != "" {
				req.SourceKind = skills.SourceGit
			}
			sk, err := mgr.Add(cmd.Context(), req)
			if err != nil {
				return classifySkillsError(err, id)
			}
			view, err := mgr.Inspect(cmd.Context(), sk.ID)
			if err != nil {
				return classifySkillsError(err, sk.ID)
			}
			return a.printer().Emit(skillRowOf(*view, false))
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "override the SKILL.md frontmatter name")
	cmd.Flags().StringVar(&id, "id", "", "override the derived library id")
	cmd.Flags().StringVar(&kind, "kind", "", "asset kind: skill, command or agent")
	cmd.Flags().StringVar(&pin, "pin", "", "record this revision as the source pin")
	cmd.Flags().StringVar(&gitURL, "git-url", "", "record git provenance for the imported checkout")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "add the entry disabled")
	return cmd
}

func (a *App) newSkillLsCmd() *cobra.Command {
	var client string
	cmd := &cobra.Command{
		Use:   "ls [--client x]",
		Short: "List library entries with their materialization state",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := a.skillManager()
			if err != nil {
				return err
			}
			views, err := mgr.List(cmd.Context(), skills.ListOptions{ClientID: client})
			if err != nil {
				return classifySkillsError(err, "")
			}
			list := SkillList{Skills: []SkillRow{}, Granularity: skills.GranularityClient}
			for _, v := range views {
				list.Skills = append(list.Skills, skillRowOf(v, false))
			}
			return a.printer().Emit(list)
		},
	}
	cmd.Flags().StringVar(&client, "client", "", "only this client's install points")
	return cmd
}

func (a *App) newSkillInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <id>",
		Short: "Show one library entry, its files and its install points",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := a.skillManager()
			if err != nil {
				return err
			}
			view, err := mgr.Inspect(cmd.Context(), args[0])
			if err != nil {
				return classifySkillsError(err, args[0])
			}
			return a.printer().Emit(skillRowOf(*view, true))
		},
	}
}

func (a *App) newSkillRmCmd() *cobra.Command {
	var (
		force         bool
		keepInstalled bool
	)
	cmd := &cobra.Command{
		Use:     "rm <id>",
		Aliases: []string{"remove"},
		Short:   "Remove a library entry and unmaterialize everything it installed",
		Args:    exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := a.skillManager()
			if err != nil {
				return err
			}
			res, err := mgr.Remove(cmd.Context(), skills.RemoveRequest{
				ID: args[0], Force: force, KeepInstalled: keepInstalled,
			})
			if err != nil {
				// res is non-nil on a partial removal and carries what DID
				// get unmaterialized; the error still wins, so the partial
				// result is left for the follow-up `skill ls` rather than
				// reported as a success.
				return classifySkillsError(err, args[0])
			}
			return a.printer().Emit(SkillAction{
				Action: "removed", SkillID: res.SkillID, Removed: res.RemovedInstalls,
				Conflicts: res.Conflicts, Granularity: res.Granularity,
				Note: "the integrity pin is kept on purpose, so a later re-add can be compared against it",
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop tracking install points that could not be removed")
	cmd.Flags().BoolVar(&keepInstalled, "keep-installed", false, "leave materialized copies in place")
	return cmd
}

// newSkillToggleCmd builds `skill enable` / `skill disable`.
func (a *App) newSkillToggleCmd(enable bool) *cobra.Command {
	verb, short := "disable", "Disable a library entry (Sync stops materializing it)"
	note := "already-materialized bytes stay until a sync (or 'skill rm') converges the target"
	if enable {
		verb, short, note = "enable", "Enable a library entry", ""
	}
	return &cobra.Command{
		Use:   verb + " <id>",
		Short: short,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := a.skillManager()
			if err != nil {
				return err
			}
			if enable {
				_, err = mgr.Enable(cmd.Context(), args[0])
			} else {
				_, err = mgr.Disable(cmd.Context(), args[0])
			}
			if err != nil {
				return classifySkillsError(err, args[0])
			}
			return a.printer().Emit(SkillAction{
				Action: verb + "d", SkillID: args[0],
				Granularity: skills.GranularityClient, Note: note,
			})
		},
	}
}

// skillInstallFlags groups the target-resolution flags shared by install-to
// and sync.
type skillInstallFlags struct {
	client      string
	scopeName   string
	projectRoot string
	dir         string
	allowDrift  bool
}

func (f *skillInstallFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.client, "client", "", "target client id")
	cmd.Flags().StringVar(&f.scopeName, "scope", skills.ScopeUser, "materialization scope: user or project")
	cmd.Flags().StringVar(&f.projectRoot, "root", "", "project root (required at project scope)")
	cmd.Flags().StringVar(&f.dir, "dir", "", "override the target directory convention")
	cmd.Flags().BoolVar(&f.allowDrift, "allow-drift", false, "overwrite copies modified outside agenthub")
}

func (a *App) newSkillInstallToCmd() *cobra.Command {
	var (
		f      skillInstallFlags
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "install-to <id> --client <client>",
		Short: "Materialize ONE skill into one client (sync does a whole scope)",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.client == "" {
				e := Usagef("--client is required")
				e.Hint = helpHint(cmd)
				return e
			}
			mgr, err := a.skillManager()
			if err != nil {
				return err
			}
			req := skills.InstallRequest{
				SkillID: args[0], ClientID: f.client, Scope: f.scopeName,
				ProjectRoot: f.projectRoot, Dir: f.dir, AllowDrift: f.allowDrift,
			}
			if dryRun {
				plan, err := mgr.Plan(cmd.Context(), req)
				if err != nil {
					return classifySkillsError(err, args[0])
				}
				return a.printer().Emit(SkillSyncResult{
					Client: plan.ClientID, Scope: plan.Scope, Changed: plan.Changed,
					Granularity: plan.Granularity,
					Items: []SkillSyncItem{{
						SkillID: plan.SkillID, Action: "plan", State: string(plan.State),
						Path: plan.Path, Detail: plan.Detail,
					}},
				})
			}
			rec, err := mgr.InstallTo(cmd.Context(), req)
			if err != nil {
				return classifySkillsError(err, args[0])
			}
			return a.printer().Emit(SkillSyncResult{
				Client: rec.ClientID, Scope: rec.Scope, Changed: true,
				Granularity: rec.Granularity,
				Items: []SkillSyncItem{{
					SkillID: rec.SkillID, Action: "installed", State: string(rec.State), Path: rec.Path,
				}},
			})
		},
	}
	f.bind(cmd)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be written without writing")
	return cmd
}

func (a *App) newSkillSyncCmd() *cobra.Command {
	var (
		f       skillInstallFlags
		noPrune bool
	)
	cmd := &cobra.Command{
		Use:   "sync <client> [--profile p]",
		Short: "Converge one client's skill directory on the enabled, in-scope set",
		Long: "Materialize every enabled skill selected by the client's scope.\n\n" +
			"Sync CONVERGES: a skill that is no longer selected is unmaterialized,\n" +
			"because leaving it would make the target disagree with the scope that\n" +
			"governs it. Pass --no-prune to keep such copies.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := a.skillManager()
			if err != nil {
				return err
			}
			res, err := mgr.Sync(cmd.Context(), skills.SyncRequest{
				ClientID: args[0], Scope: f.scopeName, ProjectRoot: f.projectRoot,
				Dir: f.dir, AllowDrift: f.allowDrift, NoPrune: noPrune,
			})
			if err != nil {
				return classifySkillsError(err, args[0])
			}
			return a.printer().Emit(syncResultOf(res))
		},
	}
	f.bind(cmd)
	// --client is redundant here (the client is the positional argument);
	// hide it so the two commands still share one flag struct.
	_ = cmd.Flags().MarkHidden("client")
	cmd.Flags().StringVar(new(string), "profile", "", "profile whose skill selector applies (reserved; scope selectors land with the daemon wiring)")
	cmd.Flags().BoolVar(&noPrune, "no-prune", false, "keep copies of skills that are no longer selected")
	return cmd
}

func syncResultOf(res *skills.SyncResult) SkillSyncResult {
	out := SkillSyncResult{
		Client: res.ClientID, Scope: res.Scope, Changed: res.Changed,
		Granularity: res.Granularity, Items: []SkillSyncItem{},
	}
	for _, it := range res.Items {
		out.Items = append(out.Items, SkillSyncItem{
			SkillID: it.SkillID, Action: string(it.Action), State: string(it.State),
			Path: it.Path, Detail: it.Detail, Error: it.Error,
		})
	}
	return out
}

func (a *App) newSkillUpdateCmd() *cobra.Command {
	var (
		path       string
		pin        string
		commit     string
		check      bool
		reapply    bool
		allowDrift bool
	)
	cmd := &cobra.Command{
		Use:   "update <id> [--path dir] [--pin rev] [--check]",
		Short: "Re-import a library entry from its source and optionally reapply it",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := a.skillManager()
			if err != nil {
				return err
			}
			res, err := mgr.Update(cmd.Context(), skills.UpdateRequest{
				ID: args[0], Path: path, Pin: pin, Commit: commit,
				Check: check, Reapply: reapply, AllowDrift: allowDrift,
			})
			if err != nil {
				return classifySkillsError(err, args[0])
			}
			out := SkillUpdateResult{
				SkillID: res.SkillID, Changed: res.Changed, Check: res.Check,
				FromVersion: res.FromVersion, ToVersion: res.ToVersion,
				FromFingerprint: res.FromFingerprint, ToFingerprint: res.ToFingerprint,
				Detail: res.Detail, Granularity: res.Granularity,
			}
			for _, it := range res.Reapplied {
				out.Reapplied = append(out.Reapplied, SkillSyncItem{
					SkillID: it.SkillID, Action: string(it.Action), State: string(it.State),
					Path: it.Path, Detail: it.Detail, Error: it.Error,
				})
			}
			return a.printer().Emit(out)
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "directory to re-import from (defaults to the recorded source)")
	cmd.Flags().StringVar(&pin, "pin", "", "record a new source revision")
	cmd.Flags().StringVar(&commit, "commit", "", "record the resolved commit for --pin")
	cmd.Flags().BoolVar(&check, "check", false, "report what would change and write nothing")
	cmd.Flags().BoolVar(&reapply, "reapply", false, "re-materialize every install point afterwards")
	cmd.Flags().BoolVar(&allowDrift, "allow-drift", false, "let --reapply overwrite locally modified copies")
	return cmd
}

func (a *App) newSkillVerifyCmd() *cobra.Command {
	var (
		id     string
		client string
	)
	cmd := &cobra.Command{
		Use:   "verify [--id x] [--client c]",
		Short: "Recompute library fingerprints from disk and re-classify every install point",
		Long: "Verify the three-way comparison: recorded provenance, library copy\n" +
			"and materialized copies.\n\n" +
			"Fingerprints are recomputed FROM THE BYTES, never read from the index —\n" +
			"an index a tamperer edited must not be able to vouch for itself. Exit 1\n" +
			"when anything is tampered, drifted, missing or conflicted.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := a.skillManager()
			if err != nil {
				return err
			}
			rep, err := mgr.Verify(cmd.Context(), skills.VerifyRequest{ID: id, ClientID: client})
			if err != nil {
				return classifySkillsError(err, id)
			}
			out := SkillVerifyReport{OK: rep.OK, Granularity: rep.Granularity, Skills: []SkillVerifyEntry{}}
			for _, s := range rep.Skills {
				entry := SkillVerifyEntry{
					SkillID: s.SkillID, Library: string(s.Library), Detail: s.Detail,
					Fingerprint: s.Fingerprint, PinnedFingerprint: s.PinnedFingerprint,
					Installs: []SkillInstall{},
				}
				for _, in := range s.Installs {
					entry.Installs = append(entry.Installs, SkillInstall{
						Client: in.Install.ClientID, Scope: in.Install.Scope, Path: in.Install.Path,
						State: string(in.State), Detail: in.Detail,
					})
				}
				out.Skills = append(out.Skills, entry)
			}
			if err := a.printer().Emit(out); err != nil {
				return err
			}
			if !rep.OK {
				// The report itself succeeded (envelope stays ok:true); the
				// non-zero exit is the machine-readable verdict.
				return &silentExitError{code: ExitGeneral}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "verify only this library entry")
	cmd.Flags().StringVar(&client, "client", "", "verify only this client's install points")
	return cmd
}

// classifySkillsError maps the skills package sentinels onto the frozen
// exit-code table.
//
// Note the direction of ErrTampered/ErrDrifted: they are exit 6 (rejected by
// governance), not a generic failure — the operation was REFUSED by an
// integrity rule, and a script should be able to tell that apart from an
// I/O error.
//
// OWED, and the reason this switch is easy to leave incomplete: the default is
// to return the error unclassified, which exits 1 as E_GENERAL and looks like
// a deliberate answer. Two sentinels are in that state — ErrInvalidID (a
// rejected --id is an argument, so exit 2) and ErrUnverifiable (the other arm
// of the same fail-closed check as ErrTampered, so exit 6). Both are written
// up with their reproduction in docs/subsystems/skills.md, under the skills
// integrity section; the exit table is frozen, so moving them is a decision
// rather than a fix.
func classifySkillsError(err error, id string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, skills.ErrNotFound):
		e := NotFoundf(CodeSkillNotFound, "%s", err.Error())
		e.Hint = "run 'agenthub skill ls' to see the library"
		return e
	case errors.Is(err, skills.ErrExists):
		return &Error{
			Code: CodeSkillExists, ExitCode: ExitGeneral,
			Message: err.Error(),
			Hint:    fmt.Sprintf("pick another --id, or update the existing entry with 'agenthub skill update %s'", id),
		}
	case errors.Is(err, skills.ErrTampered):
		return &Error{
			Code: CodeDenied, ExitCode: ExitDenied,
			Message: err.Error(),
			Hint:    "the library copy no longer matches its pin; re-add or 'skill update' it after reviewing",
		}
	case errors.Is(err, skills.ErrDrifted):
		return &Error{
			Code: CodeDenied, ExitCode: ExitDenied,
			Message: err.Error(),
			Hint:    "pass --allow-drift to overwrite the locally modified copy",
		}
	case errors.Is(err, skills.ErrConflict):
		return &Error{
			Code: CodeDenied, ExitCode: ExitDenied,
			Message: err.Error(),
			Hint:    "agenthub only writes directories carrying its own marker",
		}
	case errors.Is(err, skills.ErrStoreCorrupt):
		return &Error{
			Code: CodeStateCorrupt, ExitCode: ExitLocked,
			Message: err.Error(),
			Hint:    "the skills state file was NOT rewritten; inspect it before retrying",
		}
	case errors.Is(err, skills.ErrLockTimeout):
		return &Error{
			Code: CodeLockTimeout, ExitCode: ExitLocked,
			Message: err.Error(),
			Hint:    "another agenthub process holds the skills lock; retry in a moment",
		}
	case errors.Is(err, skills.ErrUnsupportedKind):
		return &Error{Code: CodeClientUnsupported, ExitCode: ExitGeneral, Message: err.Error()}
	}
	var unknown *clients.UnknownClientError
	if errors.As(err, &unknown) {
		e := NotFoundf(CodeClientUnsupported, "%s", err.Error())
		e.Hint = "known clients: " + strings.Join(clients.IDs(), ", ")
		return e
	}
	return err
}
