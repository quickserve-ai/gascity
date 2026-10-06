package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func openManagedDoltLifecycleLock(cityPath string) (*os.File, managedDoltRuntimeLayout, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return nil, managedDoltRuntimeLayout{}, err
	}
	if err := os.MkdirAll(filepath.Dir(layout.LockFile), 0o755); err != nil {
		return nil, managedDoltRuntimeLayout{}, fmt.Errorf("create managed dolt lock dir: %w", err)
	}
	f, err := os.OpenFile(layout.LockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, managedDoltRuntimeLayout{}, fmt.Errorf("open managed dolt lock: %w", err)
	}
	return f, layout, nil
}

func tryManagedDoltLifecycleLock(f *os.File) (bool, error) {
	if f == nil {
		return false, fmt.Errorf("nil managed dolt lock file")
	}
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, fmt.Errorf("lock managed dolt lifecycle: %w", err)
}

func releaseManagedDoltLifecycleLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// acquireStartManagedLifecycleLock admits `gc dolt-state start-managed` under
// the managed dolt lifecycle lock and returns the matching release.
//
// heldFD < 0: start-managed opens the lock file and takes the lock itself,
// non-blocking, and is refused as busy while anyone else holds it.
//
// heldFD >= 0: the caller (gc-beads-bd.sh op_start) already holds the lock on
// that inherited fd. A fresh open of the lock file is a separate open file
// description, which the caller's own flock always refuses (ga-7yjvin), so the
// lock is taken through the inherited fd instead. That flock succeeds only
// because the fd shares the caller's open file description; it does not admit a
// caller that does not hold the lock while someone else does. The fd must name
// the layout's lock file, it is marked close-on-exec so the dolt server and
// watchdog spawned next do not inherit the lock, and release only closes it:
// unlocking would drop the caller's lock.
func acquireStartManagedLifecycleLock(cityPath string, heldFD int) (func(), error) {
	if heldFD < 0 {
		lock, _, err := openManagedDoltLifecycleLock(cityPath)
		if err != nil {
			return nil, err
		}
		locked, err := tryManagedDoltLifecycleLock(lock)
		if err != nil || !locked {
			releaseManagedDoltLifecycleLock(lock)
			if err == nil {
				err = fmt.Errorf("managed dolt lifecycle is busy")
			}
			return nil, err
		}
		return func() { releaseManagedDoltLifecycleLock(lock) }, nil
	}
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return nil, err
	}
	held := os.NewFile(uintptr(heldFD), "managed-dolt-lifecycle-lock")
	heldInfo, err := held.Stat()
	if err != nil {
		_ = held.Close()
		return nil, fmt.Errorf("lifecycle lock fd %d: %w", heldFD, err)
	}
	syscall.CloseOnExec(heldFD)
	wantInfo, err := os.Stat(layout.LockFile)
	if err != nil || !os.SameFile(heldInfo, wantInfo) {
		_ = held.Close()
		return nil, fmt.Errorf("lifecycle lock fd %d is not the managed dolt lifecycle lock %s", heldFD, layout.LockFile)
	}
	locked, err := tryManagedDoltLifecycleLock(held)
	if err != nil || !locked {
		_ = held.Close()
		if err == nil {
			err = fmt.Errorf("managed dolt lifecycle is busy")
		}
		return nil, err
	}
	return func() { _ = held.Close() }, nil
}

// managedDoltStartRefusedError marks a start-managed failure where starting a
// server anyway could put a second server on the data dir or override an
// ownership decision. gc-beads-bd.sh must not fall back to a bare start on it.
type managedDoltStartRefusedError struct{ err error }

func (e managedDoltStartRefusedError) Error() string { return e.err.Error() }
func (e managedDoltStartRefusedError) Unwrap() error { return e.err }

func isManagedDoltStartRefusal(err error) bool {
	var refusal managedDoltStartRefusedError
	return errors.As(err, &refusal)
}

// writeStartManagedFailure reports a start-managed failure to its caller.
// gc-beads-bd.sh discards stderr and parses stdout as key<TAB>value lines, so
// the reason goes to stdout as one `error` line, plus `refused	true` when the
// script must not fall back to an unwatched bare start (ga-7yjvin). stderr
// keeps the message it always had.
func writeStartManagedFailure(stdout, stderr io.Writer, err error, refused bool) {
	fmt.Fprintf(stderr, "gc dolt-state start-managed: %v\n", err) //nolint:errcheck
	if refused {
		fmt.Fprintln(stdout, "refused\ttrue") //nolint:errcheck
	}
	fmt.Fprintln(stdout, "error\t"+strings.Join(strings.Fields(err.Error()), " ")) //nolint:errcheck
}
