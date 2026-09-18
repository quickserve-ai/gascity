package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worktree"
)

// The rescue proves a skill symlink is gc's own by reading the materializer's
// ownership manifest; if the two names drift, every owned symlink is secured
// as work and nothing fails loudly.
func TestWorktreeRescueReadsTheMaterializerManifest(t *testing.T) {
	if worktree.SkillOwnershipManifest != materialize.OwnershipManifestFile {
		t.Fatalf("worktree reads %q, materialize writes %q", worktree.SkillOwnershipManifest, materialize.OwnershipManifestFile)
	}
	sinks := rescueSkillSinks()
	for _, want := range []string{".claude/skills", ".agents/skills", ompSkillSink} {
		found := false
		for _, s := range sinks {
			found = found || s == want
		}
		if !found {
			t.Errorf("rescue sinks %v lack %s", sinks, want)
		}
	}
}

func TestCmdWorktreeRescueAndTeardownStrictJSONContract(t *testing.T) {
	t.Setenv("GC_JSON_CONTRACT_STRICT", "1")
	t.Setenv("GC_CITY_PATH", "")
	repo, _ := testutil.InitGitRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	testutil.RunGit(t, repo, "worktree", "add", "--detach", wt, "HEAD")
	if err := os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	args := []string{"worktree", "rescue", "--json", "--path", wt, "--bead", "gc-test"}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("run(%v): code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
	}
	validateJSONAgainstResultSchema(t, []string{"worktree", "rescue"}, stdout.Bytes())
	var rescued struct {
		RescueRef string `json:"rescue_ref"`
		RescueSHA string `json:"rescue_sha"`
		WIP       bool   `json:"wip"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &rescued); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout.String(), err)
	}
	if rescued.RescueRef != "refs/rescue/gc-test" || !rescued.WIP {
		t.Fatalf("rescue result = %+v", rescued)
	}

	stdout.Reset()
	stderr.Reset()
	args = []string{"worktree", "teardown", "--json", "--path", wt, "--bead", "gc-test", "--rescue-sha", rescued.RescueSHA}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("run(%v): code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
	}
	validateJSONAgainstResultSchema(t, []string{"worktree", "teardown"}, stdout.Bytes())
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still present after teardown: %v", err)
	}
	if got := testutil.RunGit(t, repo, "show", rescued.RescueSHA+":wip.txt"); got != "wip" {
		t.Fatalf("rescued wip.txt = %q", got)
	}
}

func TestCmdWorktreeTeardownRequiresTheRecordedRescue(t *testing.T) {
	repo, _ := testutil.InitGitRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	testutil.RunGit(t, repo, "worktree", "add", "--detach", wt, "HEAD")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"worktree", "teardown", "--path", wt, "--bead", "gc-test"}, &stdout, &stderr); code == 0 {
		t.Fatal("teardown without --rescue-sha succeeded")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree was removed: %v", err)
	}
}
