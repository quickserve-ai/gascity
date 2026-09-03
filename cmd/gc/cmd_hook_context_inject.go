package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// gc hook context-inject — the store-free UserPromptSubmit carrier for the
// clock + context-usage advisory.
//
// WHY THIS EXISTS. The advisory lines are cheap to compute: clockInjectLine is
// pure, and contextInjectLine reads only the tail of the local transcript file
// named in the hook input. But they used to ride inside `gc nudge drain
// --inject`, which resolves a session target and drains the nudge queue from
// the shared single-store dolt server. After the single-store cutover that
// store path costs 33–35s per prompt, while the UserPromptSubmit hook runs
// under `gc hook run --timeout 15s`, which kills the child and DISCARDS its
// buffered stdout on timeout (see cmd_hook.go). The advisory — written by a
// deferred flush that fires AFTER the store work — was therefore dropped on
// every prompt for every laptop crew session (qc-s3i236.4). Splitting it into
// its own hook entry makes delivery independent of the nudge queue: sub-second,
// touches no store, always flushed within the timeout.
//
// It reads the provider hook input (UserPromptSubmit JSON on stdin, pipe-only —
// see readHookStdin) and writes ONE provider-formatted payload carrying the
// clock line plus, above the context threshold, the context-usage guidance.
// Fail-open: any write error still exits 0 so a prompt is never blocked. The
// clock and context knobs (GC_INJECT_CLOCK, GC_INJECT_CONTEXT,
// GC_CONTEXT_ADVISORY_PCT, GC_CONTEXT_URGENT_PCT, GC_CONTEXT_WINDOW_TOKENS,
// GC_OPERATOR_TZ) are unchanged — this is a new transport for the same two
// line-builders, not a new policy.

func newHookContextInjectCmd(stdout, stderr io.Writer) *cobra.Command {
	var hookFormat string
	cmd := &cobra.Command{
		Use:   "context-inject",
		Short: "Emit the clock + context-usage advisory (store-free UserPromptSubmit hook)",
		Long: `Emits the live clock and, above the context-usage threshold, the
context-pressure advisory as a single UserPromptSubmit provider payload.

Store-free and sub-second by construction: it reads only the hook input JSON on
stdin (transcript_path) and the local transcript file, never the nudge queue or
any other store, so it always flushes within the hook timeout. Wire it as its
own UserPromptSubmit entry alongside (not replacing) nudge drain / mail check.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return exitForCode(cmdHookContextInject(hookFormat, readHookStdin(), stdout, stderr))
		},
	}
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	return cmd
}

// cmdHookContextInject writes the clock + context-usage advisory for the
// session whose UserPromptSubmit hook JSON is in hookInput. It touches no store
// and always returns 0 (fail-open — never block a prompt submit).
func cmdHookContextInject(hookFormat string, hookInput []byte, stdout, stderr io.Writer) int {
	line := clockInjectLine() + contextInjectLine(hookInput)
	if err := writeProviderHookContextForEvent(stdout, hookFormat, "UserPromptSubmit", line); err != nil {
		fmt.Fprintf(stderr, "gc hook context-inject: %v\n", err) //nolint:errcheck
	}
	return 0
}
