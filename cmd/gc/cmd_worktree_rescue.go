package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/worktree"
	"github.com/spf13/cobra"
)

// ompSkillSink is not a materialize vendor sink and no gc code writes it
// today; the formula teardown this replaces recognized it, so rescue keeps
// doing so. A sink with no ownership manifest excludes only files that are
// byte-identical to the city's copy, so keeping it costs nothing.
const ompSkillSink = ".omp/skills"

type worktreeRescueOpts struct {
	Path      string
	BeadID    string
	CityPath  string
	RescueSHA string
	AbsentOK  bool
	JSON      bool
}

func (o worktreeRescueOpts) spec() (worktree.RescueSpec, error) {
	p, err := filepath.Abs(o.Path)
	if err != nil {
		return worktree.RescueSpec{}, fmt.Errorf("resolving --path %q: %w", o.Path, err)
	}
	city := o.CityPath
	if city == "" {
		city = os.Getenv("GC_CITY_PATH")
	}
	return worktree.RescueSpec{
		Path: p, BeadID: o.BeadID, CityPath: city, SkillSinks: rescueSkillSinks(), AbsentOK: o.AbsentOK,
	}, nil
}

// rescueSkillSinks lists every directory gc materializes skills into.
func rescueSkillSinks() []string {
	seen := map[string]bool{ompSkillSink: true}
	for _, vendor := range materialize.SupportedVendors() {
		if sink, ok := materialize.VendorSink(vendor); ok {
			seen[sink] = true
		}
	}
	sinks := make([]string, 0, len(seen))
	for sink := range seen {
		sinks = append(sinks, sink)
	}
	sort.Strings(sinks)
	return sinks
}

func worktreeRescueFlags(cmd *cobra.Command, opts *worktreeRescueOpts) {
	cmd.Flags().StringVar(&opts.Path, "path", "", "root of the linked worktree (required)")
	cmd.Flags().StringVar(&opts.BeadID, "bead", "", "work bead the rescue ref is named for (required)")
	cmd.Flags().StringVar(&opts.CityPath, "city-path", "",
		"city directory whose synced skill copies count as gc sediment (default: $GC_CITY_PATH)")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "emit the report as JSON")
	for _, name := range []string{"path", "bead"} {
		_ = cmd.MarkFlagRequired(name) //nolint:errcheck // flags exist
	}
}

func newWorktreeRescueCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts worktreeRescueOpts
	cmd := &cobra.Command{
		Use:   "rescue",
		Short: "Secure a linked worktree's work in a local rescue ref",
		Long: `Secure a linked worktree's work in a local rescue ref.

Rescue snapshots everything in the worktree that is not gitignored (commits,
staged and unstaged edits, untracked files) and makes it reachable from
refs/rescue/<bead> in the repository's shared ref store, where removing the
worktree cannot touch it. HEAD, the index, the working tree and every branch
are left unchanged, no hook runs, and no remote is contacted.

Files gc itself put in the tree are left out, but only when that is provable:
manifest-recorded skill symlinks, skill copies byte-identical to the city's,
AGENTS-gc.md and .worktree-stale. A failure to classify them fails the rescue.

With no changes beyond HEAD the rescue is HEAD. Rerunning on an unchanged tree
returns the same commit. An existing rescue ref is only ever advanced to a
descendant; a divergent rescue is written beside it as
refs/rescue/<bead>-<sha12>. Credential-shaped file names are reported as taint
and recorded in the rescue commit's Rescue-Taint trailer.

Removal also deletes everything private to the worktree, so the rescue keeps
it reachable too: staged content that differs from both HEAD and the working
tree (an index parent), and, through an anchor parent, every commit that only
the worktree's own state reaches: its HEAD reflog, an in-progress rebase,
merge, cherry-pick, revert or bisect, refs/worktree/*, and the HEADs of its
submodules (imported into the shared object store, since a linked worktree's
submodule repositories live in its admin dir). Remote-tracking refs are not
trusted to keep anything: fetch --prune drops them. Index bits that make git
skip real edits (assume-unchanged, skip-worktree on a present file) are
ignored.

Refused, because a rescue cannot carry them: a submodule with uncommitted or
untracked changes, an untracked directory holding its own git repository,
and another registered worktree nested inside this one.

--absent-ok reports a path that does not exist (ENOENT only) as absent=true
instead of failing, so a caller can tell "already gone" from "unreadable".`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if runWorktreeRescue(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	worktreeRescueFlags(cmd, &opts)
	cmd.Flags().BoolVar(&opts.AbsentOK, "absent-ok", false,
		"succeed with absent=true when --path does not exist (only ENOENT; an unreadable path still fails)")
	return cmd
}

func newWorktreeTeardownCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts worktreeRescueOpts
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Remove a linked worktree whose rescue was recorded",
		Long: `Remove a linked worktree whose rescue was recorded.

Teardown rescues the worktree again and removes it only if the result is
exactly --rescue-sha, the rescue the caller recorded (normally on the work
bead) after running gc worktree rescue. A worktree that changed in between is
secured anew and left in place, and the error names the new rescue to record.

It refuses anything that is not a registered linked worktree: a main checkout,
a submodule checkout, a subdirectory, a symlink, or an unregistered directory.
A locked worktree is removed. If git cannot remove the tree (one holding
submodules), the directory is deleted only after it is proven a registered
linked worktree again, and then only THIS worktree's admin directory is
removed. It never runs a repo-wide worktree prune, which would drop other
worktrees' stale registrations and the commits they still keep alive. A path
that no longer exists succeeds.

A write that lands after the final rescue is removed with the tree: the lock
excludes other gc worktree operations, not editors or background processes.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if runWorktreeTeardown(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	worktreeRescueFlags(cmd, &opts)
	cmd.Flags().StringVar(&opts.RescueSHA, "rescue-sha", "",
		"rescue commit recorded by the caller before removal (required)")
	_ = cmd.MarkFlagRequired("rescue-sha") //nolint:errcheck // flag exists
	return cmd
}

type worktreeRescueJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Action        string `json:"action"`
	worktree.RescueReport
}

func runWorktreeRescue(opts worktreeRescueOpts, stdout, stderr io.Writer) int {
	spec, err := opts.spec()
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree rescue: %v\n", err) //nolint:errcheck
		return 1
	}
	rep, err := worktree.Rescue(spec)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree rescue: %v\n", err) //nolint:errcheck
		return 1
	}
	if opts.JSON {
		result := worktreeRescueJSONResult{
			SchemaVersion: "1", OK: true, Command: "worktree rescue", Action: "rescue", RescueReport: rep,
		}
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			fmt.Fprintf(stderr, "gc worktree rescue: encoding report: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	if rep.Absent {
		fmt.Fprintf(stdout, "worktree %s does not exist; nothing to rescue\n", rep.Path) //nolint:errcheck
		return 0
	}
	kind := "HEAD, no changes beyond it"
	if rep.WIP {
		kind = "a snapshot of uncommitted work"
	}
	fmt.Fprintf(stdout, "secured %s as %s (%s) in %s\n", rep.Path, rep.RescueRef, kind, rep.Repo) //nolint:errcheck
	fmt.Fprintf(stdout, "rescue sha: %s\n", rep.RescueSHA)                                        //nolint:errcheck
	if len(rep.Taint) > 0 {
		fmt.Fprintf(stdout, "taint (credential-shaped paths): %v\n", rep.Taint) //nolint:errcheck
	}
	return 0
}

type worktreeTeardownJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Action        string `json:"action"`
	worktree.TeardownReport
}

func runWorktreeTeardown(opts worktreeRescueOpts, stdout, stderr io.Writer) int {
	spec, err := opts.spec()
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree teardown: %v\n", err) //nolint:errcheck
		return 1
	}
	rep, err := worktree.Teardown(worktree.TeardownSpec{RescueSpec: spec, RescueSHA: opts.RescueSHA})
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree teardown: %v\n", err) //nolint:errcheck
		return 1
	}
	if opts.JSON {
		result := worktreeTeardownJSONResult{
			SchemaVersion: "1", OK: true, Command: "worktree teardown", Action: "teardown", TeardownReport: rep,
		}
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			fmt.Fprintf(stderr, "gc worktree teardown: encoding report: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	if rep.AlreadyAbsent {
		fmt.Fprintf(stdout, "worktree %s is already absent\n", spec.Path) //nolint:errcheck
		return 0
	}
	fmt.Fprintf(stdout, "removed worktree %s; its work is at %s (%s)\n", spec.Path, rep.Rescue.RescueRef, rep.Rescue.RescueSHA) //nolint:errcheck
	return 0
}
