package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
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
// `gc prime` renders it; primeHookContextFileName holds the hook's session
// context (current step, mail) when that does not fit beside the bootstrap.
const (
	primeHookPromptFileName  = "prime.md"
	primeHookContextFileName = "prime-context.md"
)

// primeHookSeat names a seat whose SessionStart hook context is being
// delivered: where its oversized context is written, and whether its
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
// runtime root. The session name is percent-encoded into one path element,
// so distinct names ("a/b", "a--b") never share a directory and no name can
// point outside the root.
func primeHookSeatDir(cityPath, sessionName string) (string, error) {
	name := url.PathEscape(strings.TrimSpace(sessionName))
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("no usable session name for the prompt file (got %q)", sessionName)
	}
	return filepath.Join(citylayout.SessionDiagnosticsDir(cityPath), name), nil
}

// primeHookSeatDirReady walks dir from cityPath one element at a time and
// refuses any element that is a symlink or not a directory, so a link
// anywhere below the city root cannot redirect a write or a removal outside
// it. With create, missing elements are made and the seat directory is made
// private; without it, a missing element reports fs.ErrNotExist.
func primeHookSeatDirReady(cityPath, dir string, create bool) error {
	rel, err := filepath.Rel(cityPath, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("prompt directory %q is not below city %q", dir, cityPath)
	}
	cur := cityPath
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) && create {
			if err = os.Mkdir(cur, 0o700); err == nil || errors.Is(err, fs.ErrExist) {
				info, err = os.Lstat(cur)
			}
		}
		if err != nil {
			return fmt.Errorf("prompt directory %q: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("prompt directory %q is a symlink", cur)
		}
		if !info.IsDir() {
			return fmt.Errorf("prompt directory %q is not a directory", cur)
		}
	}
	if !create {
		return nil
	}
	return runtime.EnsurePrivateDir(dir)
}

// primeHookFitContext returns the prompt and hook suffix a SessionStart hook
// delivers, and whether the suffix is delivered (in the context or in a file
// the context names). On a capped provider the complete context (beacon,
// prompt unless suppressed, suffix) stays within primeHookContextLimit:
//
//   - When it fits, prompt and suffix are returned unchanged, and any prompt
//     file left by an earlier oversized hook for this seat is removed.
//   - Otherwise the prompt (when delivered) is written byte for byte to the
//     seat's prime.md, and the suffix to prime-context.md when it does not fit
//     beside the bootstrap. The returned prompt is empty and the suffix is a
//     bootstrap naming those files, followed by the suffix when it fits.
//     Exactly the files the bootstrap names remain in the seat's directory.
//     Each write replaces its file atomically; concurrent hooks for one seat
//     leave the last writer's complete file.
//   - When a file cannot be written, the failure goes to stderr and the
//     context tells the seat to capture `gc prime` itself. A suffix that does
//     not fit then is not delivered, so a handoff it carries stays unread.
func primeHookFitContext(seat primeHookSeat, prompt string, suppressPrompt bool, suffix string, stderr io.Writer) (string, string, bool) {
	if !seat.capped {
		return prompt, suffix, true
	}
	delivered := prompt
	if suppressPrompt {
		delivered = ""
	}
	overhead := len(prependHookBeacon(seat.cityName, seat.agentName, "x")) - len("x")
	budget := primeHookContextLimit - primeHookBootstrapMargin - overhead
	dir, dirErr := primeHookSeatDir(seat.cityPath, seat.sessionName)
	if overhead+len(delivered)+len(suffix) <= primeHookContextLimit {
		if dirErr == nil && primeHookSeatDirReady(seat.cityPath, dir, false) == nil {
			primeHookRemoveFiles(dir, nil)
		}
		return prompt, suffix, true
	}

	written := map[string]bool{}
	write := func(name, content string) (string, error) {
		path := filepath.Join(dir, name)
		err := dirErr
		if err == nil {
			err = primeHookSeatDirReady(seat.cityPath, dir, true)
		}
		if err == nil {
			err = runtime.WritePrivateFile(path, []byte(content))
		}
		written[name] = err == nil
		return path, err
	}
	var promptPath, contextPath string
	var err error
	if delivered != "" {
		promptPath, err = write(primeHookPromptFileName, delivered)
	}
	inline := suffix
	if err == nil && len(primeHookBootstrapHeader(seat.agentName, len(delivered), promptPath, filepath.Join(dir, primeHookContextFileName)))+len(suffix) > budget {
		contextPath, err = write(primeHookContextFileName, suffix)
		inline = ""
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: hook context is %d bytes, over the %d-byte hook limit, and could not be written to a file: %v\n", overhead+len(delivered)+len(suffix), primeHookContextLimit, err) //nolint:errcheck // diagnostics are best effort.
		notice := primeHookWriteFailureNotice(seat.agentName, len(delivered), err)
		if len(notice)+len(suffix) > budget {
			return "", notice, false
		}
		return "", notice + suffix, true
	}
	primeHookRemoveFiles(dir, written)
	header := primeHookBootstrapHeader(seat.agentName, len(delivered), promptPath, contextPath)
	return "", header + primeHookExcerpt(delivered, promptPath, budget-len(header)-len(inline)) + inline, true
}

// primeHookRemoveFiles removes the seat's prompt and context files except
// those in keep. A file already gone is not an error.
func primeHookRemoveFiles(dir string, keep map[string]bool) {
	for _, name := range []string{primeHookPromptFileName, primeHookContextFileName} {
		if !keep[name] {
			_ = os.Remove(filepath.Join(dir, name)) // best effort: a stale file is named nowhere.
		}
	}
}

// primeHookBootstrapHeader tells the seat which files to read before acting.
// promptPath or contextPath is empty when that part was not written.
func primeHookBootstrapHeader(agentName string, promptBytes int, promptPath, contextPath string) string {
	seat := agentName
	if seat == "" {
		seat = "this seat"
	}
	var b strings.Builder
	b.WriteString("\n\n# Your session context is in a file: read it before acting\n\n")
	if promptPath != "" {
		fmt.Fprintf(&b, "Your full role prompt (%d bytes, as `gc prime %s` renders it) is too large for this hook to inject, so it was written to:\n\n    %s\n\n", promptBytes, seat, promptPath)
	}
	if contextPath != "" {
		fmt.Fprintf(&b, "This hook's session context (current step, mail) did not fit either, so it was written to:\n\n    %s\n\n", contextPath)
	}
	b.WriteString("Before you do anything else, read each file named above to its end with your file-read tool, in parts (offset/limit of about 1,200 lines per call, or allow_large where your tool offers it). They hold this seat's identity, startup behaviour and rules; act on them once you have read all of them.\n")
	return b.String()
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
