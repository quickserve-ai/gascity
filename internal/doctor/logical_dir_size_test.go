package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// du prints each directory's total when it finishes that directory, after
// everything under it. The running lower bound sums only the outermost
// finished directories, so no subtree is counted twice.
func TestDuSubtreeTally_CountsOnlyOutermostFinishedDirectories(t *testing.T) {
	tally := duSubtreeTally{root: "/r"}
	out := &lineSplitter{onLine: tally.add}
	steps := []struct {
		line   string
		wantKB int64
	}{
		{"4\t/r/a/x\n", 4},
		{"8\t/r/a\n", 8}, // a contains a/x: its total replaces a/x
		{"2\t/r/b/y\n", 10},
		{"20\t/r/b\n", 28},
		{"1\t/r/bc\n", 29}, // shares a name prefix with /r/b but is not inside it
		{"30\t/r\n", 30},
	}
	for _, s := range steps {
		if _, err := out.Write([]byte(s.line)); err != nil {
			t.Fatalf("Write(%q): %v", s.line, err)
		}
		if got, want := tally.bytes(), s.wantKB*1024; got != want {
			t.Errorf("after %q: bytes() = %d, want %d", s.line, got, want)
		}
	}
}

// A line du had not finished writing when it was stopped may carry a path cut
// short, which could make it swallow finished subtrees it does not contain.
// Only newline-terminated lines count, however the output was chunked.
func TestDuSubtreeTally_IgnoresUnterminatedLineAcrossSplitWrites(t *testing.T) {
	tally := duSubtreeTally{root: "/r"}
	out := &lineSplitter{onLine: tally.add}
	for _, chunk := range []string{"4\t/r/a", "/x\n8\t/r/", "a\n99\t/r"} {
		if _, err := out.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
	}
	if got, want := tally.bytes(), int64(8*1024); got != want {
		t.Errorf("bytes() = %d, want %d (the unterminated \"99\\t/r\" must not count)", got, want)
	}
}

// installFakeDu puts a du on PATH that runs script under /bin/sh. The real du
// is invoked as `du -k <root>`, so the script sees the root as $2.
func installFakeDu(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "du"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

const (
	// fakeDuStartLimit is how long a fake du may take to signal readiness
	// before the test gives up on it. The first run of a newly written file
	// is slow on macOS: ~0.1s median, seconds in the tail under load.
	fakeDuStartLimit = time.Minute

	// fakeDuStopLimit bounds how long a measurer may take to return once its
	// deadline passes: duWaitDelay, the most du's exec may spend releasing its
	// pipes after the kill, plus time for a loaded machine to schedule the
	// kill and reap. The fake du sleeps 30s, so returning inside this limit
	// means du was stopped, not waited out.
	fakeDuStopLimit = duWaitDelay + 3*time.Second
)

// readyDeadline is a context that expires the way a deadline does, with Err
// reporting context.DeadlineExceeded. deadlineOnceFakeDuReady closes done
// once the fake du has signaled that it printed its output, or once the test
// has given up waiting for that signal.
type readyDeadline struct {
	context.Context
	done    chan struct{}
	expired time.Time // written before done is closed
}

func (c *readyDeadline) Done() <-chan struct{} { return c.done }

func (c *readyDeadline) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// deadlineOnceFakeDuReady stands in for a short timeout on a walk by a fake
// du. A timeout races the script's own start: under load the deadline can
// pass before du prints anything, and the walk counts nothing. The fake du
// signals readiness by running `: > "$FAKE_DU_READY"` after its output, and
// the returned context expires only after that, so the walk is stopped
// midway with its output already written. If the signal never comes, the
// test fails after fakeDuStartLimit and the context expires then.
func deadlineOnceFakeDuReady(t *testing.T) *readyDeadline {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv("FAKE_DU_READY", ready)

	ctx := &readyDeadline{Context: context.Background(), done: make(chan struct{})}
	stop := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		giveUp := time.NewTimer(fakeDuStartLimit)
		defer giveUp.Stop()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
	wait:
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			select {
			case <-tick.C:
			case <-giveUp.C:
				t.Errorf("fake du did not signal readiness within %s", fakeDuStartLimit)
				break wait
			case <-stop:
				return
			}
		}
		ctx.expired = time.Now()
		close(ctx.done)
	}()
	t.Cleanup(func() {
		close(stop)
		<-exited
	})
	return ctx
}

// The measurer half of pl-59k: a walk that cannot finish inside its budget
// returns what it had counted, marked as a lower bound, and returns promptly,
// instead of an error that throws the count away.
func TestDuDirSizeWithin_BudgetExpiryReturnsCountedSubtreesAsLowerBound(t *testing.T) {
	root := t.TempDir()
	installFakeDu(t, `printf '4\t%s/a/x\n8\t%s/a\n16\t%s/b\n' "$2" "$2" "$2"
: > "$FAKE_DU_READY"
exec sleep 30
`)
	ctx := deadlineOnceFakeDuReady(t)

	got, err := duDirSizeWithin(ctx, root)
	returned := time.Now()

	if err != nil {
		t.Fatalf("duDirSizeWithin: %v", err)
	}
	if want := (dirSize{bytes: 24 * 1024, exists: true, lowerBound: true}); got != want {
		t.Errorf("duDirSizeWithin = %+v, want %+v", got, want)
	}
	select {
	case <-ctx.Done():
		if late := returned.Sub(ctx.expired); late > fakeDuStopLimit {
			t.Errorf("returned %s after its deadline, want within %s", late, fakeDuStopLimit)
		}
	default:
		t.Error("duDirSizeWithin returned before its deadline; the fake du should still have been running")
	}
}

func TestDuDirSizeWithin_CompleteWalkMatchesDuTotal(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, n := range map[string]int{
		filepath.Join(root, "top"):          10 * 1024,
		filepath.Join(root, "a", "mid"):     100 * 1024,
		filepath.Join(root, "a", "b", "in"): 300 * 1024,
	} {
		if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := duDirSizeWithin(context.Background(), root)
	if err != nil {
		t.Fatalf("duDirSizeWithin: %v", err)
	}
	wantBytes, _, err := duDirBytes(root)
	if err != nil {
		t.Fatalf("duDirBytes: %v", err)
	}
	if want := (dirSize{bytes: wantBytes, exists: true}); got != want {
		t.Errorf("duDirSizeWithin = %+v, want %+v (du -sk's total, not a lower bound)", got, want)
	}
}

func TestDuDirSizeWithin_MissingRootDoesNotExist(t *testing.T) {
	got, err := duDirSizeWithin(context.Background(), filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("duDirSizeWithin: %v", err)
	}
	if got.exists {
		t.Errorf("duDirSizeWithin = %+v, want a missing root reported as not existing", got)
	}
}

// Running out of time and being unable to read are different failures with
// different fixes; the bounded measurer keeps #85's permission classification.
func TestDuDirSizeWithin_ReportsUnreadableTreeAsPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-000 directory")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	_, err := duDirSizeWithin(context.Background(), root)
	if err == nil {
		t.Fatal("duDirSizeWithin on a tree with an unreadable directory returned no error")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("error does not wrap fs.ErrPermission: %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a permission failure must not read as running out of time: %v", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error spans lines: %q", err.Error())
	}
}

func TestLogicalSizeLabel(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	if got, want := logicalSizeLabel(dirSize{bytes: 3 * gb / 2, exists: true}, 40*time.Second),
		"1.5 GB logical"; got != want {
		t.Errorf("complete: got %q, want %q", got, want)
	}
	if got, want := logicalSizeLabel(dirSize{bytes: 3 * gb / 2, exists: true, lowerBound: true}, 40*time.Second),
		"at least 1.5 GB logical (lower bound: the size walk did not finish within 40s)"; got != want {
		t.Errorf("lower bound: got %q, want %q", got, want)
	}
}

// du prints names raw, so a directory name containing a newline splits into a
// cut-off path and a stray fragment. Past that point the finished-subtree
// stack can double-count, so the tally marks itself untrustworthy instead of
// offering an inflated "lower bound".
func TestDuSubtreeTally_NewlineInNameMarksTallyGarbled(t *testing.T) {
	for name, out := range map[string]string{
		"fragment without a tab":        "10\t/r/p\n5/x\n100\t/r/p\n5\n",
		"fragment parsing outside root": "10\t/r/x\n99999999\tq\n",
	} {
		t.Run(name, func(t *testing.T) {
			tally := duSubtreeTally{root: "/r"}
			if _, err := (&lineSplitter{onLine: tally.add}).Write([]byte(out)); err != nil {
				t.Fatal(err)
			}
			if !tally.garbled {
				t.Errorf("tally over %q not marked garbled", out)
			}
		})
	}
}

// On a finished walk the answer is du's own total for the root, whatever the
// lines before it looked like: a newline-named directory must not inflate it.
func TestDuDirSizeWithin_NewlineInNameKeepsCompleteTotalExact(t *testing.T) {
	root := t.TempDir()
	odd := filepath.Join(root, "x\n99999999\tq")
	if err := os.MkdirAll(filepath.Join(odd, "p\n5"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(odd, "p\n5", "f"), make([]byte, 64*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := duDirSizeWithin(context.Background(), root)
	if err != nil {
		t.Fatalf("duDirSizeWithin: %v", err)
	}
	wantBytes, _, err := duDirBytes(root)
	if err != nil {
		t.Fatalf("duDirBytes: %v", err)
	}
	if want := (dirSize{bytes: wantBytes, exists: true}); got != want {
		t.Errorf("duDirSizeWithin = %+v, want %+v (du -sk's total)", got, want)
	}
}

// A walk stopped at its deadline whose output was garbled by a newline-named
// directory has no trustworthy count: it reports the size as unknown, as
// having run out of time, rather than a lower bound that could exceed the tree.
func TestDuDirSizeWithin_GarbledPartialOutputIsNotALowerBound(t *testing.T) {
	root := t.TempDir()
	installFakeDu(t, `printf '10\t%s/p\n5/x\n100\t%s/p\n5\n' "$2" "$2"
: > "$FAKE_DU_READY"
exec sleep 30
`)
	ctx := deadlineOnceFakeDuReady(t)

	got, err := duDirSizeWithin(ctx, root)
	if err == nil {
		t.Fatalf("duDirSizeWithin = %+v, want an error: garbled partial output is not a lower bound", got)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error should say the walk ran out of time: %v", err)
	}
}

func TestSizingBudget(t *testing.T) {
	if got := sizingBudget(&CheckContext{}, 0); got != worktreeSizeBudget {
		t.Errorf("no deadline, no override: got %s, want the %s default", got, worktreeSizeBudget)
	}
	if got := sizingBudget(&CheckContext{}, 100*time.Millisecond); got != 100*time.Millisecond {
		t.Errorf("no deadline, 100ms override: got %s, want 100ms", got)
	}
	// Doctor's default --check-timeout leaves the full budget.
	if got := sizingBudget(&CheckContext{Deadline: time.Now().Add(time.Minute)}, 0); got != worktreeSizeBudget {
		t.Errorf("1m deadline: got %s, want %s", got, worktreeSizeBudget)
	}
	// A lower --check-timeout shrinks it to two thirds of the time left, in
	// whole seconds so the lower-bound label stays readable.
	if got := sizingBudget(&CheckContext{Deadline: time.Now().Add(30 * time.Second)}, 0); got != 20*time.Second {
		t.Errorf("30s deadline: got %s, want 20s", got)
	}
	if got := sizingBudget(&CheckContext{Deadline: time.Now().Add(-time.Second)}, 0); got != 0 {
		t.Errorf("passed deadline: got %s, want no time at all", got)
	}
}
