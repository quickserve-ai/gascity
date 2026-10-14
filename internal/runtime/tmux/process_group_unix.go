//go:build !windows

package tmux

import (
	"errors"
	"fmt"
	"syscall"
)

func signalPID(pid int, signal processSignal) error {
	var sig syscall.Signal
	switch signal {
	case processSignalTerm:
		sig = syscall.SIGTERM
	case processSignalKill:
		sig = syscall.SIGKILL
	case processSignalUsr1:
		sig = syscall.SIGUSR1
	default:
		return fmt.Errorf("unsupported process signal %q", signal)
	}
	return syscall.Kill(pid, sig)
}

func isProcessSignalGoneError(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}
