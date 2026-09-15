package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bdDirtyTablesErrText is the refusal beads emits (verbatim, modulo the table
// list) when pending schema migrations would alter tables that already carry
// uncommitted working-set changes. Reproduced here so the classifier is tested
// against the real upstream text rather than a paraphrase of it.
const bdDirtyTablesErrText = "bd init: pending schema migrations alter pre-existing dirty tables: " +
	"config, dependencies; run 'bd dolt commit' to commit the working set at the current schema, " +
	"then re-run the migration (gastownhall/beads#4566)"

func TestIsBdInitDirtyTablesError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "verbatim beads refusal", err: errors.New(bdDirtyTablesErrText), want: true},
		{
			name: "ignored-migration variant",
			err: errors.New("pending ignored schema migrations alter pre-existing dirty tables: " +
				"issue_snapshots"),
			want: true,
		},
		{
			name: "wrapped by an exec provider",
			err:  fmt.Errorf("gc-beads-bd init: %w", errors.New(bdDirtyTablesErrText)),
			want: true,
		},
		{
			name: "unrelated migration failure",
			err:  errors.New("bd init: Unknown column 'agent_state' in 'issues'"),
			want: false,
		},
		{name: "already initialized", err: errors.New("bd init: already initialized"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBdInitDirtyTablesError(tt.err); got != tt.want {
				t.Fatalf("isBdInitDirtyTablesError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// stubCommitDirtyScopeTables swaps the managed-Dolt commit seam for the test's
// own and restores it afterwards.
func stubCommitDirtyScopeTables(t *testing.T, fn func(cityPath, scopeRoot, database string) (bool, error)) {
	t.Helper()
	prev := commitDirtyScopeTables
	commitDirtyScopeTables = fn
	t.Cleanup(func() { commitDirtyScopeTables = prev })
}

// The dirty set observed in the field shrank 9 -> 1 -> 0: bd init makes partial
// progress each pass and re-dirties tables, so a single commit does not clear
// it. Recovery must keep committing until init stops refusing.
func TestRecoverBdInitFromDirtyTablesLoopsUntilClean(t *testing.T) {
	var commits int
	stubCommitDirtyScopeTables(t, func(_, _, database string) (bool, error) {
		commits++
		if database != "gsp" {
			t.Fatalf("commit database = %q, want %q", database, "gsp")
		}
		return true, nil
	})

	var reinits int
	err := recoverBdInitFromDirtyTables("/city", "/city/rigs/gsp", "gsp", errors.New(bdDirtyTablesErrText), func() error {
		reinits++
		if reinits < 2 {
			return errors.New(bdDirtyTablesErrText)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("recoverBdInitFromDirtyTables() = %v, want nil", err)
	}
	if commits != 2 {
		t.Fatalf("commit rounds = %d, want 2", commits)
	}
	if reinits != 2 {
		t.Fatalf("re-init attempts = %d, want 2", reinits)
	}
}

// A commit that reports nothing left to commit cannot make further progress:
// looping again would spin against an unchanged database.
func TestRecoverBdInitFromDirtyTablesStopsWhenNothingLeftToCommit(t *testing.T) {
	initErr := errors.New(bdDirtyTablesErrText)
	var commits, reinits int
	stubCommitDirtyScopeTables(t, func(string, string, string) (bool, error) {
		commits++
		return false, nil
	})

	err := recoverBdInitFromDirtyTables("/city", "/city/rigs/gsp", "gsp", initErr, func() error {
		reinits++
		return nil
	})
	if !errors.Is(err, initErr) {
		t.Fatalf("error = %v, want the original init error", err)
	}
	if commits != 1 {
		t.Fatalf("commit rounds = %d, want 1", commits)
	}
	if reinits != 0 {
		t.Fatalf("re-init attempts = %d, want 0", reinits)
	}
}

// A non-dirty-table failure on the retry is the real outcome and must surface
// unchanged rather than being retried as if it were still the deadlock.
func TestRecoverBdInitFromDirtyTablesSurfacesUnrelatedRetryFailure(t *testing.T) {
	stubCommitDirtyScopeTables(t, func(string, string, string) (bool, error) { return true, nil })

	other := errors.New("bd init: connection refused")
	err := recoverBdInitFromDirtyTables("/city", "/city/rigs/gsp", "gsp", errors.New(bdDirtyTablesErrText), func() error {
		return other
	})
	if !errors.Is(err, other) {
		t.Fatalf("error = %v, want %v", err, other)
	}
}

// When committing itself fails, the operator needs both the refusal and the
// reason the documented remedy could not be applied.
func TestRecoverBdInitFromDirtyTablesSurfacesCommitFailure(t *testing.T) {
	initErr := errors.New(bdDirtyTablesErrText)
	commitErr := errors.New("connect to managed Dolt: dial tcp: connection refused")
	stubCommitDirtyScopeTables(t, func(string, string, string) (bool, error) { return false, commitErr })

	err := recoverBdInitFromDirtyTables("/city", "/city/rigs/gsp", "gsp", initErr, func() error {
		t.Fatal("re-init must not run after a failed commit")
		return nil
	})
	if !errors.Is(err, initErr) {
		t.Fatalf("error = %v, want it to wrap the init refusal", err)
	}
	if !errors.Is(err, commitErr) {
		t.Fatalf("error = %v, want it to wrap the commit failure", err)
	}
}

// A database that never stops refusing must not spin forever.
func TestRecoverBdInitFromDirtyTablesBoundsRounds(t *testing.T) {
	var commits int
	stubCommitDirtyScopeTables(t, func(string, string, string) (bool, error) {
		commits++
		return true, nil
	})

	err := recoverBdInitFromDirtyTables("/city", "/city/rigs/gsp", "gsp", errors.New(bdDirtyTablesErrText), func() error {
		return errors.New(bdDirtyTablesErrText)
	})
	if err == nil {
		t.Fatal("recoverBdInitFromDirtyTables() = nil, want an error")
	}
	if commits != maxBdInitDirtyTableRounds {
		t.Fatalf("commit rounds = %d, want %d", commits, maxBdInitDirtyTableRounds)
	}
}

// An empty database name leaves nothing to commit against, so recovery must
// decline rather than guess at a target.
func TestRecoverBdInitFromDirtyTablesWithoutDatabase(t *testing.T) {
	initErr := errors.New(bdDirtyTablesErrText)
	stubCommitDirtyScopeTables(t, func(string, string, string) (bool, error) {
		t.Fatal("commit must not run without a database name")
		return false, nil
	})

	if err := recoverBdInitFromDirtyTables("/city", "/city/rigs/gsp", "  ", initErr, func() error {
		t.Fatal("re-init must not run without a database name")
		return nil
	}); !errors.Is(err, initErr) {
		t.Fatalf("error = %v, want the original init error", err)
	}
}

// gc-beads-bd.sh prints bd's refusal and then declines its own checkpoint for a
// database that invocation did not create, for a dirty table outside bd's
// partial-init schema, or when the checkpoint cannot complete. The provider
// error carries both texts, so it matches the dirty-table classifier; the
// recovery must honor the script's refusal instead of committing the whole
// working set over it.
func TestRecoverBdInitFromDirtyTablesHonorsGcBeadsBdRefusal(t *testing.T) {
	tests := []struct {
		name       string
		scriptTail string
	}{
		{
			name:       "pre-existing database",
			scriptTail: "bd init failed for /city/rigs/gsp; refusing to checkpoint a pre-existing database",
		},
		{
			name:       "unexpected dirty table",
			scriptTail: "error: refusing bd init schema checkpoint with unexpected dirty table: operator_notes",
		},
		{
			name:       "checkpoint could not complete",
			scriptTail: "failed to checkpoint partial bd init schema for gsp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubCommitDirtyScopeTables(t, func(string, string, string) (bool, error) {
				t.Fatal("commit must not run after gc-beads-bd refused the checkpoint")
				return false, nil
			})
			initErr := fmt.Errorf("exec beads init: %s\n%s", bdDirtyTablesErrText, tt.scriptTail)
			err := recoverBdInitFromDirtyTables("/city", "/city/rigs/gsp", "gsp", initErr, func() error {
				t.Fatal("re-init must not run after gc-beads-bd refused the checkpoint")
				return nil
			})
			if !errors.Is(err, initErr) {
				t.Fatalf("error = %v, want the script's refusal unchanged", err)
			}
		})
	}
}

// Once a commit has run, the re-init meets a database that already exists, so
// gc-beads-bd treats it as one it did not create and refuses. That refusal is
// final: another commit of the whole working set would override it.
func TestRecoverBdInitFromDirtyTablesStopsAtGcBeadsBdRefusalOnRetry(t *testing.T) {
	var commits, reinits int
	stubCommitDirtyScopeTables(t, func(string, string, string) (bool, error) {
		commits++
		return true, nil
	})

	initErr := fmt.Errorf("exec beads init: %s\nbd init retry failed for /city/rigs/gsp", bdDirtyTablesErrText)
	refusal := fmt.Errorf("exec beads init: %s\nbd init failed for /city/rigs/gsp; refusing to checkpoint a pre-existing database", bdDirtyTablesErrText)
	err := recoverBdInitFromDirtyTables("/city", "/city/rigs/gsp", "gsp", initErr, func() error {
		reinits++
		return refusal
	})
	if !errors.Is(err, refusal) {
		t.Fatalf("error = %v, want the script's refusal on the retry", err)
	}
	if commits != 1 {
		t.Fatalf("commit rounds = %d, want 1", commits)
	}
	if reinits != 1 {
		t.Fatalf("re-init attempts = %d, want 1", reinits)
	}
}

// The refusals are matched against gc-beads-bd.sh's own output, so a rewording
// in the script must fail here rather than silently let the recovery commit the
// whole working set over a refusal it no longer recognizes.
func TestGcBeadsBdCheckpointRefusalsMatchScript(t *testing.T) {
	if len(gcBeadsBdCheckpointRefusals) == 0 {
		t.Fatal("gcBeadsBdCheckpointRefusals is empty")
	}
	script, err := os.ReadFile(filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read gc-beads-bd.sh: %v", err)
	}
	for _, refusal := range gcBeadsBdCheckpointRefusals {
		if !strings.Contains(string(script), refusal) {
			t.Errorf("gc-beads-bd.sh does not print %q; keep gcBeadsBdCheckpointRefusals in step with the script", refusal)
		}
	}
}

// End-to-end through the entry point `gc rig add` actually calls: a rig whose
// fresh database comes back dirty must be recovered in-process instead of
// handing the operator beads' circular "run 'bd dolt commit'" advice.
func TestInitBeadsForDirWithExecutorRecoversFromDirtyTables(t *testing.T) {
	cityDir := t.TempDir()
	cityConfig := `[workspace]
name = "demo"

[beads]
provider = "bd"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(cityDir, "rigs", "gascity-packs")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var commits int
	var gotCityPath, gotScopeRoot, gotDatabase string
	stubCommitDirtyScopeTables(t, func(cityPath, scopeRoot, database string) (bool, error) {
		commits++
		gotCityPath, gotScopeRoot, gotDatabase = cityPath, scopeRoot, database
		return true, nil
	})

	// Fail the first init the way beads does, then stop with a sentinel so the
	// assertion stays on the recovery and never reaches the real store
	// finalization.
	stopAfterRecovery := errors.New("stop after recovery")
	var calls int
	execute := func(_ string, _ []string, _ ...string) error {
		calls++
		if calls == 1 {
			return errors.New(bdDirtyTablesErrText)
		}
		return stopAfterRecovery
	}

	err := initBeadsForDirWithExecutor(cityDir, rigDir, "gsp", "gsp", execute)
	if !errors.Is(err, stopAfterRecovery) {
		t.Fatalf("initBeadsForDirWithExecutor error = %v, want %v", err, stopAfterRecovery)
	}
	if calls != 2 {
		t.Fatalf("bd init attempts = %d, want 2 (initial + post-commit retry)", calls)
	}
	if commits != 1 {
		t.Fatalf("commit rounds = %d, want 1", commits)
	}
	if gotCityPath != cityDir {
		t.Fatalf("commit cityPath = %q, want %q", gotCityPath, cityDir)
	}
	// The commit must be judged against the RIG scope's endpoint, not the
	// city's (hq-mbe2s): the recovery has to be handed the scope it re-inits.
	if gotScopeRoot != rigDir {
		t.Fatalf("commit scopeRoot = %q, want the rig scope %q", gotScopeRoot, rigDir)
	}
	if gotDatabase != "gsp" {
		t.Fatalf("commit database = %q, want %q", gotDatabase, "gsp")
	}
}

// The commit must carry an explicit --author: Dolt aborts a commit with an
// empty committer identity, and a fresh host is not guaranteed to have a
// global one configured. `USE` has to land before the `CALL` so the procedure
// runs against the scope database rather than whatever the session defaults to.
func TestDirtyScopeTablesCommitSQL(t *testing.T) {
	got := dirtyScopeTablesCommitSQL("gsp")

	wantAuthor := "'--author', 'gascity-builder <builder@gascity.local>'"
	if !strings.Contains(got, wantAuthor) {
		t.Fatalf("commit SQL = %q, want it to contain %q", got, wantAuthor)
	}
	if !strings.Contains(got, "DOLT_COMMIT('-A'") {
		t.Fatalf("commit SQL = %q, want it to stage every table with -A", got)
	}
	if !strings.Contains(got, dirtyScopeTablesCommitMessage) {
		t.Fatalf("commit SQL = %q, want it to contain message %q", got, dirtyScopeTablesCommitMessage)
	}

	use := strings.Index(got, "USE "+managedDoltQuoteIdent("gsp")+";")
	call := strings.Index(got, "CALL DOLT_COMMIT")
	if use != 0 {
		t.Fatalf("commit SQL = %q, want it to start with the USE statement", got)
	}
	if call < use {
		t.Fatalf("commit SQL = %q, want USE before CALL", got)
	}
}

// The identifier is quoted, and neither fixed literal contains a quote
// character, so the statement cannot be broken by the database name.
func TestDirtyScopeTablesCommitSQLQuotesDatabase(t *testing.T) {
	got := dirtyScopeTablesCommitSQL("we`ird")
	if !strings.HasPrefix(got, "USE `we``ird`;") {
		t.Fatalf("commit SQL = %q, want the database identifier escaped", got)
	}
}

func TestParseSmokeCount(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    int
		wantErr bool
	}{
		{name: "header and value", out: "cnt\n9\n", want: 9},
		{name: "zero rows dirty", out: "cnt\n0\n", want: 0},
		{name: "trailing blank lines", out: "cnt\n3\n\n\n", want: 3},
		{name: "empty output", out: "", wantErr: true},
		{name: "whitespace only", out: "   \n\t\n", wantErr: true},
		{name: "trailing non-numeric warning", out: "cnt\n3\nWarning: connection reset\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSmokeCount(tt.out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSmokeCount(%q) = %d, want an error", tt.out, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSmokeCount(%q) = %v, want %d", tt.out, err, tt.want)
			}
			if got != tt.want {
				t.Fatalf("parseSmokeCount(%q) = %d, want %d", tt.out, got, tt.want)
			}
		})
	}
}
