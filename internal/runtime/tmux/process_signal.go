package tmux

type processSignal string

const (
	processSignalTerm processSignal = "TERM"
	processSignalKill processSignal = "KILL"
	// processSignalUsr1 asks a tmux server to recreate its socket.
	processSignalUsr1 processSignal = "USR1"
)
