//go:build !windows

package tmux

import (
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
	dir := t.TempDir()
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
	defer f.Close()
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

// A delivery that starts while another process holds the lock proceeds as
// soon as that process releases it, inside its timeout.
func TestNudgeLockProceedsWhenTheOtherProcessReleases(t *testing.T) {
	withScratchNudgeXLockDir(t)
	const session = "krieger"
	release := holdAsOtherProcess(t, session)
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
	}()
	start := time.Now()
	if !acquireNudgeLock(session, 2*time.Second) {
		t.Fatal("timed out although the other process released after 100ms")
	}
	defer releaseNudgeLock(session)
	if waited := time.Since(start); waited < 90*time.Millisecond {
		t.Fatalf("acquired after %v, before the other process released", waited)
	}
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
