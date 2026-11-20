//go:build !windows

package tmux

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// withScratchNudgeXLockDir points the cross-process nudge lock at a scratch
// directory for the test.
func withScratchNudgeXLockDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir() // 0755 from the testing package; the lock tightens its own dir to 0700
	old := nudgeXLockDir
	nudgeXLockDir = func() string { return dir }
	t.Cleanup(func() { nudgeXLockDir = old })
	return dir
}

// holdAsOtherProcess takes the flock on key's lock file through a separate
// open file description, which flock(2) treats exactly like another
// process's: it conflicts with this process's own descriptor for the file.
func holdAsOtherProcess(t *testing.T, key string) (release func()) {
	t.Helper()
	if err := os.MkdirAll(nudgeXLockDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(nudgeXLockPath(key), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("other holder could not take the lock: %v", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

// otherProcessCanLock reports whether a separate holder could take key's
// lock right now (and releases it at once).
func otherProcessCanLock(t *testing.T, key string) bool {
	t.Helper()
	f, err := os.OpenFile(nudgeXLockPath(key), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return true
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("unexpected flock error: %v", err)
	}
	return false
}

// The 2026-11-20 project-lead case (ga-bz7eo5): while another gc process is
// delivering to a session, this process's delivery must not start typing.
func TestNudgeLockWaitsForAnotherProcessDelivery(t *testing.T) {
	withScratchNudgeXLockDir(t)
	const session = "qcore--oversight__project-lead"
	release := holdAsOtherProcess(t, session)

	if acquireNudgeLock(session, 150*time.Millisecond) {
		releaseNudgeLock(session)
		t.Fatal("acquired the nudge lock while another process held the session's lock")
	}

	// The failed attempt must leave the in-process half free too, or every
	// later delivery in this process would wait out its timeout.
	release()
	if !acquireNudgeLock(session, time.Second) {
		t.Fatal("could not acquire the nudge lock after the other process released it")
	}
	releaseNudgeLock(session)
}

// A delivery waiting on another process's hold proceeds once that process
// releases, inside its timeout. (That it WAITS while the hold stands is
// TestNudgeLockWaitsForAnotherProcessDelivery; this one needs no timing.)
func TestNudgeLockProceedsWhenTheOtherProcessReleases(t *testing.T) {
	withScratchNudgeXLockDir(t)
	const session = "krieger"
	release := holdAsOtherProcess(t, session)
	got := make(chan bool, 1)
	go func() { got <- acquireNudgeLock(session, 10*time.Second) }()
	release()
	if !<-got {
		t.Fatal("timed out although the other process released")
	}
	releaseNudgeLock(session)
}

// While this process holds the lock, another process cannot take it; after
// release it can.
func TestNudgeLockExcludesOtherProcessesUntilReleased(t *testing.T) {
	withScratchNudgeXLockDir(t)
	const session = "qcore--figgis"
	if !acquireNudgeLock(session, time.Second) {
		t.Fatal("could not acquire an uncontended nudge lock")
	}
	if otherProcessCanLock(t, session) {
		releaseNudgeLock(session)
		t.Fatal("another process could take the lock this process holds")
	}
	releaseNudgeLock(session)
	if !otherProcessCanLock(t, session) {
		t.Fatal("the lock stayed held after release")
	}
}

// Distinct sessions never share a lock, including keys that sanitize alike.
func TestNudgeLockKeysAreDistinct(t *testing.T) {
	withScratchNudgeXLockDir(t)
	if nudgeXLockPath("a:0.1") == nudgeXLockPath("a_0_1") {
		t.Fatal("keys that sanitize alike share a lock file")
	}
	if !acquireNudgeLock("alpha", time.Second) {
		t.Fatal("could not acquire alpha")
	}
	defer releaseNudgeLock("alpha")
	if !acquireNudgeLock("beta", 200*time.Millisecond) {
		t.Fatal("beta waited on alpha's lock")
	}
	releaseNudgeLock("beta")
}

// An unusable lock directory fails open: delivery keeps the in-process lock
// rather than stopping.
func TestNudgeLockFailsOpenWhenTheLockDirIsUnusable(t *testing.T) {
	file := t.TempDir() + "/not-a-dir"
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := nudgeXLockDir
	nudgeXLockDir = func() string { return file + "/locks" }
	t.Cleanup(func() { nudgeXLockDir = old })
	if !acquireNudgeLock("gamma", 200*time.Millisecond) {
		t.Fatal("an unusable lock dir blocked delivery")
	}
	releaseNudgeLock("gamma")
}

// A lock directory this user does not own, or that group/others can write,
// is not trusted: delivery fails open instead of using locks another user
// could hold or swap.
func TestNudgeLockFailsOpenOnAnUntrustedLockDir(t *testing.T) {
	// A symlink in place of the directory is never trusted: it could point at
	// a directory another user controls.
	target := t.TempDir()
	link := t.TempDir() + "/locks"
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	old := nudgeXLockDir
	nudgeXLockDir = func() string { return link }
	t.Cleanup(func() { nudgeXLockDir = old })
	if nudgeXLockDirTrusted(link) {
		t.Fatal("a symlinked lock dir was trusted")
	}
	if !acquireNudgeLock("delta", 200*time.Millisecond) {
		t.Fatal("an untrusted lock dir blocked delivery")
	}
	defer releaseNudgeLock("delta")
	if _, held := nudgeXLockFiles.Load("delta"); held {
		t.Fatal("took a flock in an untrusted dir")
	}
}

// This user's own lock dir with loose permissions is tightened, not
// abandoned.
func TestNudgeLockTightensItsOwnLooseDir(t *testing.T) {
	dir := withScratchNudgeXLockDir(t)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if !nudgeXLockDirTrusted(dir) {
		t.Fatal("this user's own dir was not trusted after tightening")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir left at %o, want 700", perm)
	}
}

// A holder in another process can delay a delivery by at most
// nudgeXLockMaxWait, even under a longer NudgeLockTimeout.
func TestNudgeLockCrossProcessWaitIsCapped(t *testing.T) {
	withScratchNudgeXLockDir(t)
	old := nudgeXLockMaxWait
	nudgeXLockMaxWait = 150 * time.Millisecond
	t.Cleanup(func() { nudgeXLockMaxWait = old })
	release := holdAsOtherProcess(t, "epsilon")
	defer release()
	start := time.Now()
	if acquireNudgeLock("epsilon", 10*time.Second) {
		releaseNudgeLock("epsilon")
		t.Fatal("acquired while another process held the lock")
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("waited %v, past the cross-process cap", waited)
	}
}

// A lock timeout wraps ErrNudgeLockBusy, so queue callers can back off on it
// without spending a delivery attempt (ga-bz7eo5).
func TestNudgeLockTimeoutWrapsErrNudgeLockBusy(t *testing.T) {
	withScratchNudgeXLockDir(t)
	release := holdAsOtherProcess(t, "zeta")
	defer release()
	tm := &Tmux{cfg: Config{NudgeLockTimeout: 100 * time.Millisecond}}
	err := tm.ClearInput(context.Background(), "zeta", 0)
	if !errors.Is(err, ErrNudgeLockBusy) {
		t.Fatalf("ClearInput under another process's lock: err = %v, want ErrNudgeLockBusy", err)
	}
}
