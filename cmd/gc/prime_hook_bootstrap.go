package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/runtime"
)

// primeHookContextLimit is the largest hook context, in bytes, a provider is
// trusted to inject whole. Claude Code caps a hook's injected context at
// 10,000 characters and shows anything larger as a short preview plus a
// path, which reads like the whole prompt and is not. Bytes never undercount
// characters, so a payload within this many bytes is within the cap.
const primeHookContextLimit = 10000

// primeHookBootstrapMargin is held back from primeHookContextLimit when the
// bootstrap sizes its prompt excerpt, so a beacon or suffix that differs
// slightly between sizing and delivery cannot carry it over the limit.
const primeHookBootstrapMargin = 500

// primeHookBootstrapExcerptMax bounds the head of the prompt the bootstrap
// carries: enough for the seat to see whose prompt it is, not a substitute
// for reading the file.
const primeHookBootstrapExcerptMax = 4000

// primeHookPromptFileName is the per-seat file an oversized hook prompt is
// written to, under the seat's session directory.
const primeHookPromptFileName = "prime.md"

// primeHookPromptPath returns the stable per-seat path for the full prompt:
// the seat's own directory under the city's session runtime root, the same
// directory its session diagnostics use. sessionName names the seat; it is
// reduced to one path element so it cannot point outside that root.
func primeHookPromptPath(cityPath, sessionName string) (string, error) {
	name := strings.ReplaceAll(strings.TrimSpace(sessionName), "/", "--")
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, os.PathSeparator) {
		return "", fmt.Errorf("no usable session name for the prompt file (got %q)", sessionName)
	}
	return filepath.Join(citylayout.SessionDiagnosticsDir(cityPath), name, primeHookPromptFileName), nil
}

// primeHookPromptForDelivery returns the prompt a hook should inject. When
// the beacon, prompt and hook suffix together fit primeHookContextLimit, or
// the prompt is not being delivered at all, the prompt is returned as is.
// Otherwise the full prompt is written, byte for byte as `gc prime` renders
// it, to the seat's prompt file, and a bootstrap under the limit is returned
// in its place: who the seat is, the instruction to read the file to its end
// before acting, and the head of the prompt. If the file cannot be written
// the failure is reported on stderr and the full prompt is returned, so the
// seat still gets what the provider shows of it.
func primeHookPromptForDelivery(cityPath, cityName, agentName, sessionName, prompt string, hookMode, suppressPrompt bool, hookSuffix string, stderr io.Writer) string {
	if !hookMode || suppressPrompt || prompt == "" {
		return prompt
	}
	overhead := len(prependHookBeacon(cityName, agentName, "x")) - len("x") + len(hookSuffix)
	if overhead+len(prompt) <= primeHookContextLimit {
		return prompt
	}
	path, err := primeHookPromptPath(cityPath, sessionName)
	if err == nil {
		if err = runtime.EnsurePrivateDir(filepath.Dir(path)); err == nil {
			err = runtime.WritePrivateFile(path, []byte(prompt))
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: prompt is %d bytes, over the %d-byte hook limit, and could not be written to a file; injecting it whole: %v\n", len(prompt), primeHookContextLimit, err) //nolint:errcheck // diagnostics are best effort.
		return prompt
	}
	return primeHookBootstrap(agentName, path, prompt, primeHookContextLimit-primeHookBootstrapMargin-overhead)
}

// primeHookBootstrap renders the context injected in place of an oversized
// prompt. budget is the room left for the whole bootstrap; the prompt
// excerpt shrinks to fit it, cut at a line boundary, and is left out when
// nothing fits.
func primeHookBootstrap(agentName, path, prompt string, budget int) string {
	seat := agentName
	if seat == "" {
		seat = "this seat"
	}
	header := fmt.Sprintf(`# Your role prompt is in a file: read it before acting

Your full role prompt (%d bytes, as `+"`gc prime %s`"+` renders it) is too large for this hook to inject, so it was written to:

    %s

Before you do anything else, read that file to its end with your file-read tool, in parts (offset/limit of about 1,200 lines per call, or allow_large where your tool offers it). It holds this seat's identity, startup behaviour and rules; act on it once you have read all of it.
`, len(prompt), seat, path)
	const excerptOpen = "\nIts opening lines follow so you know whose prompt it is. They are not the whole prompt.\n\n---\n"
	excerptClose := "\n---\n(The prompt continues in " + path + ".)\n"
	room := budget - len(header) - len(excerptOpen) - len(excerptClose)
	if room > primeHookBootstrapExcerptMax {
		room = primeHookBootstrapExcerptMax
	}
	if room <= 0 {
		return header
	}
	excerpt := prompt
	if len(excerpt) > room {
		excerpt = excerpt[:room]
		cut := strings.LastIndexByte(excerpt, '\n')
		if cut <= 0 {
			return header
		}
		excerpt = excerpt[:cut]
	}
	return header + excerptOpen + strings.TrimRight(excerpt, "\n") + excerptClose
}
