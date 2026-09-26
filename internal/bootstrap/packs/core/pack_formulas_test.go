package core

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// formulaFile is the subset of a formula TOML these tests inspect. Steps carry
// the agent-facing instructions, so asserting on a step description is how the
// pack pins behavior that lives in prompt text rather than in Go.
type formulaFile struct {
	Formula string `toml:"formula"`
	Steps   []struct {
		ID          string `toml:"id"`
		Title       string `toml:"title"`
		Description string `toml:"description"`
	} `toml:"steps"`
}

// readFormula decodes a formula TOML from the embedded core pack.
//
// file stays parameterized even though every current caller passes
// mol-polecat-base.toml: the pack ships sibling formulas (mol-polecat-commit,
// mol-polecat-report) that inherit these steps, and the next test to pin one of
// them reads it through this same helper.
//
//nolint:unparam // see above
func readFormula(t *testing.T, file string) formulaFile {
	t.Helper()
	data, err := fs.ReadFile(PackFS, "formulas/"+file)
	if err != nil {
		t.Fatalf("reading formulas/%s: %v", file, err)
	}
	var parsed formulaFile
	if _, err := toml.Decode(string(data), &parsed); err != nil {
		t.Fatalf("decoding formulas/%s: %v", file, err)
	}
	return parsed
}

// formulaStep returns the description of the named step, failing the test when
// the step is absent.
func formulaStep(t *testing.T, f formulaFile, id string) string {
	t.Helper()
	for _, step := range f.Steps {
		if step.ID == id {
			return step.Description
		}
	}
	t.Fatalf("formula %s has no step %q", f.Formula, id)
	return ""
}

// TestMolDoWorkDrainClaimsCurrentContinuation pins the distinction between a
// process's immutable startup bead and the continuation that became ready
// while that process was running.
func TestMolDoWorkDrainClaimsCurrentContinuation(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-do-work.toml"), "drain")

	if !strings.Contains(step, "gc ready --json --limit=2") {
		t.Fatal("drain must resolve the current continuation from ready work")
	}
	if !strings.Contains(step, "--include-ephemeral") {
		t.Fatal("drain must include ephemeral rows; a wisp-tier continuation is invisible to bd ready without it")
	}
	if !strings.Contains(step, `--metadata-field "gc.root_bead_id=$ROOT_BEAD_ID"`) || !strings.Contains(step, `--metadata-field "gc.step_ref=mol-do-work.drain"`) {
		t.Fatal("drain must select the ready drain step from the current workflow root")
	}
	if strings.Contains(step, `DRAIN_BEAD_ID="${GC_BEAD_ID`) {
		t.Fatal("drain must not treat the immutable startup bead as the continuation bead")
	}
	if !strings.Contains(step, "if [ -z \"$ROOT_BEAD_ID\" ]") {
		t.Fatal("drain must fail closed when the startup bead has no workflow root")
	}
	if !strings.Contains(step, "if length == 1") || !strings.Contains(step, "could not resolve one ready continuation bead") {
		t.Fatal("drain must fail closed unless exactly one matching continuation is ready")
	}
	// The current-claim branch must stay gated on gc.step_ref. An unguarded
	// `gc hook current` re-introduces gh-5141 on the deferred path, where the
	// claim stamp still names the do-work step rather than the drain step.
	currentAt := strings.Index(step, "gc hook current --id-only")
	if currentAt < 0 {
		t.Fatal("drain must consult the current claim before falling back to a ready query")
	}
	readyAt := strings.Index(step, "gc ready --json --limit=2")
	if readyAt < currentAt {
		t.Fatal("drain must consult the current claim before the ready query, not after it")
	}
	if !strings.Contains(step[currentAt:readyAt], `.metadata["gc.step_ref"] == "mol-do-work.drain"`) {
		t.Fatal("drain must gate the current-claim branch on gc.step_ref; an unguarded claim restores the stale-identity bug")
	}
	// The startup-bead id is only needed by the ready-query fallback. Requiring
	// it up front aborts drain on any seat that is not demand-spawned:
	// GC_BEAD_ID exists only in the dispatch condition environment, never in a
	// session shell, and GC_TRIGGER_BEAD_ID is pool-seat-only (see
	// cmd/gc/cmd_hook_current.go). Same shape as ga-2q2r0.
	startupAt := strings.Index(step, `STARTUP_BEAD_ID="${GC_BEAD_ID`)
	if startupAt < 0 || startupAt < currentAt {
		t.Fatal("drain must not require a startup bead id before consulting the current claim")
	}
	// The current claim carries gc.root_bead_id, and on a warm seat it is the
	// ONLY source of it: GC_BEAD_ID never reaches a session shell and
	// GC_TRIGGER_BEAD_ID is demand-spawn-only (cmd/gc/cmd_hook_current.go).
	// Reading the root off the startup bead first strands every warm seat on
	// the deferred path — the ga-2q2r0 failure, one layer in.
	rootFromCurrent := strings.Index(step, `ROOT_BEAD_ID=$(printf '%s' "$CURRENT"`)
	if rootFromCurrent < 0 {
		t.Fatal("drain must derive the workflow root from the current claim it already fetched")
	}
	if rootFromCurrent > startupAt {
		t.Fatal("drain must try the current claim's root before falling back to startup env vars")
	}

	updateAt := strings.Index(step, "gc bd update")
	drainAckAt := strings.Index(step, "gc runtime drain-ack")
	if drainAckAt < 0 {
		t.Fatal("drain must still acknowledge runtime drain after closing its continuation bead")
	}
	if updateAt < 0 || !strings.Contains(step[updateAt:drainAckAt], "|| exit 1") {
		t.Fatal("drain must not acknowledge runtime drain after a failed bead close")
	}
}

// TestPolecatPreflightSearchesLedgerBeforeFiling pins the search-before-file
// contract in the polecat preflight step.
//
// Concurrent polecats all run a baseline against the same base branch, so they
// all observe the same pre-existing failure within seconds of each other. With
// no ledger search in front of the create, each one files its own bug: five
// duplicates inside three minutes from three polecats, and one duplicate that
// sat ready in the pool after its original had already merged, one sling away
// from dispatching a polecat to redo merged work.
func TestPolecatPreflightSearchesLedgerBeforeFiling(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-base.toml"), "preflight-tests")

	searchAt := strings.Index(step, "gc bd list --title-contains")
	if searchAt < 0 {
		t.Fatal("preflight-tests must search the ledger with `gc bd list --title-contains` before filing a pre-existing-failure bead")
	}
	createAt := strings.Index(step, "gc bd create")
	if createAt < 0 {
		t.Fatal("preflight-tests must still describe how to file a pre-existing-failure bead")
	}
	if searchAt > createAt {
		t.Error("preflight-tests searches the ledger after `gc bd create`; the search must gate the create, not follow it")
	}

	// The original report is usually already claimed by the polecat or refinery
	// fixing it, so an open-only filter misses the very bead it should match.
	searchCmd := step[searchAt:createAt]
	if !strings.Contains(searchCmd, "in_progress") {
		t.Error("the dedupe lookup must include --status in_progress; the existing bead is frequently already claimed")
	}

	// A lookup that errors is not an all-clear. The refinery's earlier attempt at
	// this check invoked a flag that does not exist (`gc bd list --search`), so it
	// errored every run and the "no duplicate found" branch filed anyway.
	if !strings.Contains(step, "LOOKUP_RC") || !strings.Contains(step, `"$LOOKUP_RC" -ne 0`) {
		t.Error("the dedupe lookup must capture its exit status in LOOKUP_RC and fail closed on a non-zero result")
	}

	// Round-trip matchability: agents write different prose for one defect, so the
	// filed title has to carry the same stable key the next agent searches for.
	if !strings.Contains(searchCmd, `--title-contains "$SYMPTOM_KEY"`) {
		t.Error("the dedupe lookup must search by the stable $SYMPTOM_KEY, not by free-text description")
	}
	if !strings.Contains(step[createAt:], "$SYMPTOM_KEY") {
		t.Error("the filed title must embed $SYMPTOM_KEY verbatim so the next polecat's lookup matches it")
	}
}

// TestPolecatPreflightKeysOnTestFunctionNotSubtest pins the two widenings that
// an exact-title match alone does not deliver.
//
// Go subtests make the reported name unstable across agents: two concurrent
// polecats often fail different subtests of the same function
// (`TestX/clean_config_with_residual_files_is_a_conflict` versus
// `TestX/peer_successor_cross-device_tree`), search different strings, and both
// file. Stripping at the first `/` keys them together.
//
// Sibling functions in one package are the second gap: three different
// `TestDisableAndPurge*` functions can share a single root cause (for example
// an environment leak), and no exact-name key groups them. Eight
// productmetrics beads landed in ~38h that way.
func TestPolecatPreflightKeysOnTestFunctionNotSubtest(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-base.toml"), "preflight-tests")

	if !strings.Contains(step, "%%/*") {
		t.Error("the symptom key must strip the Go subtest suffix (${NAME%%/*}) so sibling subtests of one function share a key")
	}

	familyAt := strings.Index(step, `--title-contains "$SYMPTOM_FAMILY"`)
	if familyAt < 0 {
		t.Error("preflight-tests must widen to a $SYMPTOM_FAMILY lookup; sibling tests in one package usually share one root cause")
	}
	createAt := strings.Index(step, "gc bd create")
	if createAt >= 0 && familyAt > createAt {
		t.Error("the family lookup must run before `gc bd create`, not after it")
	}

	// The family branch is a judgement call, so nothing auto-assigns the match —
	// but the step must still tell the agent how to hand a family hit to 3c, or
	// 3c's `gc bd comment "$EXISTING"` runs with an empty id.
	if !strings.Contains(step, `EXISTING="<the bead id you judged to be the same defect>"`) {
		t.Error("3b2 must show how to set $EXISTING for a family match; 3c cannot comment without it")
	}
	if !strings.Contains(step, "FAMILY_RC") {
		t.Error("the family lookup must capture its exit status; a failed lookup is not an all-clear")
	}
}

// TestPolecatPreflightChecksRecentlyClosedBeforeFiling pins the staleness gate.
//
// Re-filing a defect that already merged puts a ready bead in the pool, and the
// next sling dispatches a polecat to redo merged work.
func TestPolecatPreflightChecksRecentlyClosedBeforeFiling(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-base.toml"), "preflight-tests")

	closedAt := strings.Index(step, "--closed-after")
	if closedAt < 0 {
		t.Fatal("preflight-tests must check recently-closed beads before filing; a stale baseline otherwise re-files merged work")
	}
	createAt := strings.Index(step, "gc bd create")
	if createAt >= 0 && closedAt > createAt {
		t.Error("the recently-closed check must run before `gc bd create`, not after it")
	}
	if !strings.Contains(step, "CLOSED_RC") {
		t.Error("the recently-closed lookup must capture its exit status; a failed lookup is not proof the fix has not landed")
	}
}

// TestPolecatSelfReviewDefersToPreflightDedupeProtocol keeps the second
// pre-existing-failure filing path pointed at the one protocol.
//
// self-review also tells the polecat to file a bead when a failure turns out to
// be pre-existing. Restating the protocol there would let the two copies drift;
// referring to preflight-tests keeps one definition.
func TestPolecatSelfReviewDefersToPreflightDedupeProtocol(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-base.toml"), "self-review")

	if !strings.Contains(step, "preflight-tests") {
		t.Error("self-review's pre-existing-failure path must point at the preflight-tests search-first protocol instead of filing directly")
	}
	if strings.Contains(step, "gc bd create") {
		t.Error("self-review must not carry its own `gc bd create` for pre-existing failures; that bypasses the dedupe protocol")
	}
}

// TestCoreShippedAssetsAvoidNonexistentBDListSearchFlag guards the failure mode
// that made the sibling fix a no-op: `gc bd list` has no `--search` flag, so a
// dedupe lookup written against it exits 1 and returns nothing, which reads as
// "no duplicate exists" to the branch that follows. Search by --title-contains.
func TestCoreShippedAssetsAvoidNonexistentBDListSearchFlag(t *testing.T) {
	err := fs.WalkDir(PackFS, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(PackFS, path)
		if err != nil {
			return err
		}
		body := string(data)
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, "bd list") && strings.Contains(line, "--search") {
				t.Errorf("%s: `bd list --search` is not a real flag and exits 1; use --title-contains: %s", path, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded core pack: %v", err)
	}
}

// assertSecureBeforeDestroy pins the teardown order for a formula-managed
// worktree (ga-w805wc): the work is secured LOCALLY in a rescue ref, the bead
// records the rescue, gc worktree teardown removes the tree only if it still
// matches that rescue, and work_dir is cleared last. The teardown this
// replaced tested remote containment with `git branch -r --contains` (a stale
// cache that skipped the push and deleted anyway), pushed whatever
// `git add -A` swept up to a branch that could be a live PR head, and fell
// through to erasing work_dir after refusing to remove. The removal guards
// themselves (main checkout, submodule, unregistered directory) are pinned by
// internal/worktree's Teardown tests; this pins that the formula uses them.
func assertSecureBeforeDestroy(t *testing.T, name, block, worktreeVar string) {
	t.Helper()
	order := []string{
		`gc worktree rescue --path "` + worktreeVar + `" --bead "$WORK_BEAD_ID"`,
		`--set-metadata rescue_sha="$RESCUE_SHA"`,
		`gc worktree teardown --path "` + worktreeVar + `" --bead "$WORK_BEAD_ID" --rescue-sha "$RESCUE_SHA" || exit 1`,
		`--unset-metadata work_dir || exit 1`,
	}
	last := -1
	for _, want := range order {
		at := strings.Index(block, want)
		if at < 0 {
			t.Fatalf("%s: teardown is missing %q", name, want)
		}
		if at < last {
			t.Fatalf("%s: %q runs out of order; want rescue -> record on the bead -> teardown -> clear work_dir", name, want)
		}
		last = at
	}
	for _, forbidden := range []string{"rm -rf", "git push", "git add -A", "git branch -r", "git worktree remove"} {
		if strings.Contains(block, forbidden) {
			t.Errorf("%s: teardown must not run %q; securing and removal belong to gc worktree rescue/teardown", name, forbidden)
		}
	}
	if !strings.Contains(block, `--set-metadata rescue_taint="$RESCUE_TAINT" || exit 1`) {
		t.Errorf("%s: a failed rescue record must stop the step before removal", name)
	}
}

func TestMolPolecatCommitSecuresBeforeRemovingWorktree(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-polecat-commit.toml"), "commit-and-push")
	start := strings.Index(step, "**3. Clean up worktree")
	end := strings.Index(step, "**4. Close the bead:**")
	if start < 0 || end < start {
		t.Fatal("commit-and-push lost its numbered cleanup section")
	}
	block := step[start:end]
	assertSecureBeforeDestroy(t, "commit-and-push", block, "$WORKTREE_PATH")
	// Tearing down $(pwd) removes whatever worktree the session stands in,
	// possibly another bead's, and orphans this bead's real one.
	if strings.Contains(block, "WORKTREE_PATH=$(pwd)") {
		t.Error("commit-and-push must tear down the bead's recorded work_dir, not $(pwd)")
	}
	if !strings.Contains(block, `.[0].metadata.work_dir`) {
		t.Error("commit-and-push must read the worktree path from the bead's work_dir")
	}
}

func TestMolScopedWorkSecuresBeforeRemovingWorktree(t *testing.T) {
	step := formulaStep(t, readFormula(t, "mol-scoped-work.toml"), "cleanup-worktree")
	assertSecureBeforeDestroy(t, "cleanup-worktree", step, "$WORKTREE")

	// A failed bead read yields an empty work_dir, which would read as
	// "nothing to tear down" and erase the pointer to a live worktree.
	if !strings.Contains(step, `BEAD_JSON=$(gc bd show "$WORK_BEAD_ID" --json) || exit 1`) {
		t.Error("cleanup-worktree must stop on a failed bead read, not treat it as an empty work_dir")
	}
	// A shell existence test reads an unreadable path as absent and would
	// clear the pointer to a live worktree; gc decides absence (ENOENT only).
	if !strings.Contains(step, "--absent-ok --json") {
		t.Error("cleanup-worktree must let gc worktree rescue --absent-ok decide whether the worktree is gone")
	}
	for _, shellTest := range []string{`[ -e "$WORKTREE" ]`, `[ -d "$WORKTREE" ]`} {
		if strings.Contains(step, shellTest) {
			t.Errorf("cleanup-worktree decides absence with %s, which also reads an unreadable path as absent", shellTest)
		}
	}
	// An open work bead with a live convoy is the normal state after a
	// hand-off submit, and its tree can be a running session's cwd
	// (ga-6xehc5). The decline must stop the step before anything is secured
	// or removed. scripts/test-mol-scoped-work-teardown.sh runs the rendered
	// block for the behavior; this pins the order.
	decline := strings.Index(step, `DECLINED: work bead $WORK_BEAD_ID open with live convoy {{convoy_id}}; worktree preserved`)
	rescue := strings.Index(step, `gc worktree rescue --path "$WORKTREE"`)
	if decline < 0 || decline > rescue {
		t.Error("cleanup-worktree must decline an open work bead with a live convoy before gc worktree rescue")
	}
}
