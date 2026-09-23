package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/pathutil"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestWorktreeIsLive_ProcessCWDEqualsWorktree(t *testing.T) {
	wt := t.TempDir()
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(wt)}}
	got, reason := worktreeIsLive(wt, live, nil)
	if !got {
		t.Fatalf("worktreeIsLive = false, want true when a process cwd equals the worktree (reason %q)", reason)
	}
}

func TestWorktreeIsLive_ProcessCWDUnderWorktree(t *testing.T) {
	wt := t.TempDir()
	nested := filepath.Join(wt, "test", "integration")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(nested)}}
	got, reason := worktreeIsLive(wt, live, nil)
	if !got {
		t.Fatalf("worktreeIsLive = false, want true when a process cwd is a nested subdir (reason %q)", reason)
	}
}

func TestWorktreeIsLive_ProcessCWDAboveWorktreeIsNotLive(t *testing.T) {
	parent := t.TempDir()
	wt := filepath.Join(parent, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}
	// A process sitting in the PARENT of the worktree is not "working in" it.
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(parent)}}
	if got, _ := worktreeIsLive(wt, live, nil); got {
		t.Fatal("worktreeIsLive = true, want false when the only live cwd is an ancestor of the worktree")
	}
}

func TestWorktreeIsLive_SiblingCWDIsNotLive(t *testing.T) {
	base := t.TempDir()
	wt := filepath.Join(base, "wt")
	sibling := filepath.Join(base, "other")
	for _, d := range []string{wt, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(sibling)}}
	if got, _ := worktreeIsLive(wt, live, nil); got {
		t.Fatal("worktreeIsLive = true, want false for a sibling directory cwd")
	}
}

func TestWorktreeIsLive_SessionDirProtects(t *testing.T) {
	wt := t.TempDir()
	live := liveWorktreeState{scanned: true} // no live process cwds
	got, reason := worktreeIsLive(wt, live, []string{wt})
	if !got {
		t.Fatalf("worktreeIsLive = false, want true when an active session dir equals the worktree (reason %q)", reason)
	}
}

func TestWorktreeIsLive_NothingMatches(t *testing.T) {
	wt := t.TempDir()
	other := t.TempDir()
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(other)}}
	if got, _ := worktreeIsLive(wt, live, []string{other}); got {
		t.Fatal("worktreeIsLive = true, want false when no live signal is at or under the worktree")
	}
}

// TestCollectLiveWorktreeState_IncludesOwnCWD runs on EVERY platform, which is
// the whole point of ga-bq84cj. It used to skip unless GOOS=linux — so the one
// property that mattered on the fleet host, that the scan can succeed at all,
// was asserted nowhere the fleet host would run it. The reaper then failed
// closed 157,136 times and reaped nothing for the life of the host with a green
// suite.
func TestCollectLiveWorktreeState_IncludesOwnCWD(t *testing.T) {
	live := collectLiveWorktreeState()
	if !live.scanned {
		t.Fatalf("collectLiveWorktreeState scanned = false on GOOS=%s, want true — the reaper fails closed and reaps nothing when this happens", runtime.GOOS)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	want := pathutil.NormalizePathForCompare(cwd)
	for _, c := range live.cwds {
		if c == want {
			return // found this process's own cwd in the live set
		}
	}
	t.Fatalf("collectLiveWorktreeState did not include this process's cwd %q in %d entries", want, len(live.cwds))
}

func TestLiveSessionWorktreeDirs_CollectsAndDedups(t *testing.T) {
	abs1 := t.TempDir()
	abs2 := t.TempDir()
	snapshot := newSessionBeadSnapshotFromInfos([]sessionpkg.Info{
		{ID: "s1", WorkerDir: abs1},
		{ID: "s2", WorkDir: abs2},
		{ID: "s3", WorkerDir: abs1},          // duplicate of s1 → deduped
		{ID: "s4", WorkDir: "relative/path"}, // non-absolute → dropped
		{ID: "s5"},                           // empty → dropped
	})
	got := liveSessionWorktreeDirs(snapshot)

	want := map[string]bool{
		pathutil.NormalizePathForCompare(abs1): false,
		pathutil.NormalizePathForCompare(abs2): false,
	}
	for _, d := range got {
		nd := pathutil.NormalizePathForCompare(d)
		if _, ok := want[nd]; !ok {
			t.Errorf("unexpected dir %q (normalized %q)", d, nd)
			continue
		}
		want[nd] = true
	}
	for d, seen := range want {
		if !seen {
			t.Errorf("expected dir %q missing from result %v", d, got)
		}
	}
}

func TestLiveSessionWorktreeDirs_NilSnapshot(t *testing.T) {
	if got := liveSessionWorktreeDirs(nil); got != nil {
		t.Fatalf("liveSessionWorktreeDirs(nil) = %v, want nil", got)
	}
}

// --- lsof probe (ga-bq84cj) ------------------------------------------------

const lsofSample = "p1\nn/\np412\nn/Users/cherub/gascity\np880\nn/Users/cherub/gascity/.gc/worktrees/qc-abc\np881\nn/Users/cherub/gascity\n"

func TestParseLsofCwds_ExtractsDedupsAndDropsNonPaths(t *testing.T) {
	got := parseLsofCwds([]byte(lsofSample + "nrelative/path\nfcwd\n\n"))
	want := map[string]bool{
		pathutil.NormalizePathForCompare("/"):                                          true,
		pathutil.NormalizePathForCompare("/Users/cherub/gascity"):                      true,
		pathutil.NormalizePathForCompare("/Users/cherub/gascity/.gc/worktrees/qc-abc"): true,
	}
	if len(got) != len(want) {
		t.Fatalf("parseLsofCwds returned %d entries %v, want %d (deduped, absolute only)", len(got), got, len(want))
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("unexpected cwd %q in %v", g, got)
		}
	}
}

// TestCollectLiveWorktreeStateLsof_NonZeroExitWithOutputStillCounts pins the
// distinction that decides whether this probe is usable at all. lsof exits
// non-zero whenever any part of its search failed — routine on a shared box —
// while still printing every entry it resolved. Treating that exit code as
// failure would throw away a complete process table and fail closed forever,
// which is the ga-bq84cj defect in a new costume.
func TestCollectLiveWorktreeStateLsof_NonZeroExitWithOutputStillCounts(t *testing.T) {
	orig := lsofCwdOutputFn
	t.Cleanup(func() { lsofCwdOutputFn = orig })
	lsofCwdOutputFn = func(ctx context.Context) ([]byte, error) {
		return []byte(lsofSample), errors.New("exit status 1")
	}
	live := collectLiveWorktreeStateLsof()
	if !live.scanned {
		t.Fatal("scanned = false for a non-zero exit that still produced a process table; the reaper would protect everything forever")
	}
	if len(live.cwds) != 3 {
		t.Fatalf("cwds = %v, want the 3 deduped paths from the sample", live.cwds)
	}
}

// TestCollectLiveWorktreeStateLsof_EmptyOutputFailsClosed is the direction that
// must never be permissive: no parsed cwd means the probe did not work (no lsof
// on PATH, an unrecognized format), NOT that no process is live. This process
// itself always has a cwd, so an empty parse is impossible from a working
// probe.
func TestCollectLiveWorktreeStateLsof_EmptyOutputFailsClosed(t *testing.T) {
	orig := lsofCwdOutputFn
	t.Cleanup(func() { lsofCwdOutputFn = orig })
	for name, fn := range map[string]func(context.Context) ([]byte, error){
		"no output, error": func(ctx context.Context) ([]byte, error) {
			return nil, errors.New("exec: \"lsof\": executable file not found in $PATH")
		},
		"no output, no error": func(ctx context.Context) ([]byte, error) { return nil, nil },
		"unparseable output":  func(ctx context.Context) ([]byte, error) { return []byte("p123\np456\n"), nil },
	} {
		t.Run(name, func(t *testing.T) {
			lsofCwdOutputFn = fn
			if live := collectLiveWorktreeStateLsof(); live.scanned {
				t.Fatalf("scanned = true with no usable cwd (%s); the reaper would be authorized to delete live work", name)
			}
		})
	}
}

// TestCollectLiveWorktreeStateLsof_TimeoutFailsClosed: a partial view of the
// process table can omit exactly the process that would have protected a tree,
// so a cancelled probe is indeterminate even when it produced rows.
func TestCollectLiveWorktreeStateLsof_TimeoutFailsClosed(t *testing.T) {
	orig := lsofCwdOutputFn
	t.Cleanup(func() { lsofCwdOutputFn = orig })
	lsofCwdOutputFn = func(ctx context.Context) ([]byte, error) {
		inner, cancel := context.WithCancel(ctx)
		cancel()
		<-inner.Done()
		return []byte(lsofSample), inner.Err()
	}
	if live := collectLiveWorktreeStateLsof(); live.scanned {
		t.Fatal("scanned = true for a probe that did not finish; a partial process table must fail closed")
	}
}
