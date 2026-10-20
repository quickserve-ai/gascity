package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// primeHookContextLimit is the largest hook context, in bytes, a capped
// provider is trusted to inject whole. Claude Code caps a hook's injected
// context at 10,000 characters and shows anything larger as a short preview
// plus a path, which reads like the whole prompt and is not. Bytes never
// undercount characters, so a payload within this many bytes is within the cap.
const primeHookContextLimit = 10000

// primeHookBootstrapMargin is held back from primeHookContextLimit when the
// bootstrap is sized, so a beacon whose timestamp renders a few bytes longer
// at delivery cannot carry it over the limit.
const primeHookBootstrapMargin = 500

// primeHookBootstrapExcerptMax bounds the head of the prompt the bootstrap
// carries: enough for the seat to see whose prompt it is, not a substitute
// for reading the file.
const primeHookBootstrapExcerptMax = 4000

// primeHookPromptFileName holds the seat's full prompt, byte for byte as
// `gc prime` renders it.
const primeHookPromptFileName = "prime.md"

// primeHookSeat names a seat whose SessionStart hook context is being
// delivered: where its oversized prompt is written, and whether its
// provider caps hook context at all.
type primeHookSeat struct {
	cityPath, cityName, agentName, sessionName string
	capped                                     bool
}

// primeHookContextCapped reports whether the provider truncates hook context
// at primeHookContextLimit. Only the Claude family does; every other
// provider keeps receiving its prompt whole.
func primeHookContextCapped(resolved *config.ResolvedProvider) bool {
	return resolved != nil && resolved.AccountFamily() == "claude"
}

// primeHookSeatDir returns the seat's directory under the city's session
// runtime root, the session name reduced to one path element.
func primeHookSeatDir(cityPath, sessionName string) (string, error) {
	name := strings.ReplaceAll(strings.TrimSpace(sessionName), "/", "--")
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("no usable session name for the prompt file (got %q)", sessionName)
	}
	return filepath.Join(citylayout.SessionDiagnosticsDir(cityPath), name), nil
}

// primeHookFitContext returns the prompt and hook suffix a SessionStart hook
// delivers. On a capped provider, when the beacon, prompt and suffix together
// are over primeHookContextLimit, the prompt is written byte for byte to the
// seat's prime.md and the returned prompt is empty; the suffix is then a
// bootstrap naming that file, followed by the original suffix. When the file
// cannot be written, the failure goes to stderr and the bootstrap tells the
// seat to capture `gc prime` itself. A suppressed prompt is not delivered,
// so it is returned unchanged.
func primeHookFitContext(seat primeHookSeat, prompt string, suppressPrompt bool, suffix string, stderr io.Writer) (string, string) {
	if !seat.capped || suppressPrompt {
		return prompt, suffix
	}
	overhead := len(prependHookBeacon(seat.cityName, seat.agentName, "x")) - len("x")
	if overhead+len(prompt)+len(suffix) <= primeHookContextLimit {
		return prompt, suffix
	}
	budget := primeHookContextLimit - primeHookBootstrapMargin - overhead - len(suffix)
	dir, err := primeHookSeatDir(seat.cityPath, seat.sessionName)
	path := filepath.Join(dir, primeHookPromptFileName)
	if err == nil {
		err = runtime.EnsurePrivateDir(dir)
	}
	if err == nil {
		err = runtime.WritePrivateFile(path, []byte(prompt))
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: hook context is %d bytes, over the %d-byte hook limit, and could not be written to a file: %v\n", overhead+len(prompt)+len(suffix), primeHookContextLimit, err) //nolint:errcheck // diagnostics are best effort.
		return "", primeHookWriteFailureNotice(seat.agentName, len(prompt), err) + suffix
	}
	header := primeHookBootstrapHeader(seat.agentName, len(prompt), path)
	return "", header + primeHookExcerpt(prompt, path, budget-len(header)) + suffix
}

// primeHookBootstrapHeader tells the seat which file to read before acting.
func primeHookBootstrapHeader(agentName string, promptBytes int, promptPath string) string {
	seat := agentName
	if seat == "" {
		seat = "this seat"
	}
	return fmt.Sprintf("\n\n# Your session context is in a file: read it before acting\n\nYour full role prompt (%d bytes, as `gc prime %s` renders it) is too large for this hook to inject, so it was written to:\n\n    %s\n\nBefore you do anything else, read each file named above to its end with your file-read tool, in parts (offset/limit of about 1,200 lines per call, or allow_large where your tool offers it). They hold this seat's identity, startup behaviour and rules; act on them once you have read all of them.\n", promptBytes, seat, promptPath)
}

// primeHookExcerpt returns the head of the prompt, cut at a line boundary to
// fit room, so the seat can see whose prompt the file holds. It is empty when
// no prompt was written or nothing fits.
func primeHookExcerpt(prompt, promptPath string, room int) string {
	if prompt == "" {
		return ""
	}
	const excerptOpen = "\nIts opening lines follow so you know whose prompt it is. They are not the whole prompt.\n\n---\n"
	excerptClose := "\n---\n(The prompt continues in " + promptPath + ".)\n"
	room -= len(excerptOpen) + len(excerptClose)
	if room > primeHookBootstrapExcerptMax {
		room = primeHookBootstrapExcerptMax
	}
	if room <= 0 {
		return ""
	}
	excerpt := prompt
	if len(excerpt) > room {
		cut := strings.LastIndexByte(excerpt[:room], '\n')
		if cut <= 0 {
			return ""
		}
		excerpt = excerpt[:cut]
	}
	return excerptOpen + strings.TrimRight(excerpt, "\n") + excerptClose
}

// primeHookWriteFailureNotice replaces an oversized context whose file could
// not be written: the seat captures its prompt itself and reads it in parts.
func primeHookWriteFailureNotice(agentName string, promptBytes int, err error) string {
	seat := agentName
	if seat == "" {
		seat = "<your agent>"
	}
	return fmt.Sprintf("\n\n# Your role prompt did not reach this conversation\n\nThis hook's context is over the %d-byte hook limit (role prompt %d bytes), and writing it to a file failed: %v\n\nBefore you do anything else, capture your prompt and read it to its end in parts (offset/limit of about 1,200 lines per call):\n\n    f=$(mktemp \"${TMPDIR:-/tmp}/prime.XXXXXX\") && gc prime %s > \"$f\" && echo \"$f\"\n", primeHookContextLimit, promptBytes, err, seat)
}
