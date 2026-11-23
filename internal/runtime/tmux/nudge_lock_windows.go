//go:build windows

package tmux

import "time"

// nudgeXLockDir exists here only so the package tests can redirect it on
// every platform (main_test.go); Windows takes no cross-process lock.
var nudgeXLockDir = func() string { return "" }

// tmux does not run natively on Windows, so there is no second process to
// race with; the cross-process half of the nudge lock (nudge_lock_unix.go,
// ga-bz7eo5) is a no-op here and the in-process semaphore is the whole lock.

func acquireNudgeXLock(string, time.Time) bool { return true }

func releaseNudgeXLock(string) {}
