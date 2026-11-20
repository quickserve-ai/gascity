//go:build !windows

package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
)

// The cross-process half of the per-session nudge lock (ga-bz7eo5).
//
// sessionNudgeLocks serializes deliveries to one session inside ONE process.
// A pane is typed into by several processes: the supervisor's nudge
// dispatcher, the live wait-idle delivery a `gc mail send` / `gc session
// nudge` makes in its own short-lived process, and per-session pollers in
// legacy mode. On 2026-11-20 two of them typed into the q-core project
// lead's composer 23 ms apart; each one's before-submit guard then saw the
// other's paste placeholder beside its own, read the box as a person's
// draft and withheld Enter, and the seat heard nothing until a person
// pressed a key. An advisory flock per session key makes the second
// delivery wait for the first's submit instead of typing beside it.
//
// The lock files live in one per-user directory every gc process agrees on.
// It is NOT os.TempDir(): seats and gated jobs run with private TMPDIRs. The
// kernel drops a flock when its holder exits, so a killed delivery never
// leaves the session locked.
//
// Failure to open the lock file fails OPEN (the in-process lock still
// holds, which is the behaviour before this change): a nudge plane that
// stops delivering because /tmp is unwritable is worse than the race.

// nudgeXLockDir is a variable so tests can point it at a scratch directory.
var nudgeXLockDir = func() string {
	return filepath.Join("/tmp", fmt.Sprintf("gc-nudge-locks-%d", os.Getuid()))
}

// nudgeXLockPoll is how often a waiting delivery retries the flock.
var nudgeXLockPoll = 25 * time.Millisecond

// nudgeXLockFiles holds the open lock file per key while this process holds
// the lock. The in-process semaphore guarantees one holder per key here, so
// a plain Store / LoadAndDelete pairing is enough.
var nudgeXLockFiles sync.Map // map[string]*os.File

var nudgeXLockUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// nudgeXLockPath maps a lock key (a session name or a pane target such as
// "%12" or "sess:0.1") to a file name: the key made filename-safe, plus a
// short hash so two keys that sanitize alike never share a file.
func nudgeXLockPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	safe := nudgeXLockUnsafe.ReplaceAllString(key, "_")
	if len(safe) > 64 {
		safe = safe[:64]
	}
	return filepath.Join(nudgeXLockDir(), safe+"-"+hex.EncodeToString(sum[:6])+".lock")
}

// acquireNudgeXLock takes the cross-process lock for key, retrying until
// deadline. It returns false only when another holder kept the lock past the
// deadline; any other failure fails open (true, nothing held).
func acquireNudgeXLock(key string, deadline time.Time) bool {
	dir := nudgeXLockDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return true
	}
	f, err := os.OpenFile(nudgeXLockPath(key), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return true
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			nudgeXLockFiles.Store(key, f)
			return true
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return true
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return false
		}
		time.Sleep(nudgeXLockPoll)
	}
}

// releaseNudgeXLock drops the cross-process lock for key, if this process
// holds it.
func releaseNudgeXLock(key string) {
	v, ok := nudgeXLockFiles.LoadAndDelete(key)
	if !ok {
		return
	}
	f := v.(*os.File)
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
