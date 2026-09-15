// Package sessionlog reads Claude Code JSONL session files for
// lightweight metadata extraction (model, context usage).
package sessionlog

import "strings"

// modelFamilyWindows maps model family keywords to their context window sizes.
var modelFamilyWindows = map[string]int{
	"opus":   200_000,
	"sonnet": 200_000,
	"haiku":  200_000,
	// Unlike the other Claude families, "fable" resolves to 1M WITHOUT needing
	// the "[1m]" suffix, because the suffix never survives into the transcript
	// this package reads: sampled 2026-09-14 across the live fleet's session
	// files, every fable usage line records a bare "claude-fable-5" or
	// "claude-fable-5-1" while the seats were launched with "...[1m]". Gating
	// fable on the suffix therefore reports a 1M seat as 200k — a meter 5x too
	// full: the meter reads 100% when a 1M seat has used 200k tokens, a FIFTH of
	// its real window, and the handoff bands trip earlier still.
	// context_inject.go's classifiedWindow already treats "fable" as 1M for
	// exactly this reason; this keeps the two window tables from disagreeing.
	//
	// The cost is the "fable-5-200k" pin, whose transcript is indistinguishable
	// from the 1M form's. That pin exists only for restart migration and no
	// provider selects it, so the ambiguity is resolved toward the 13 providers
	// that are really on the 1M tier (ga-a306b1).
	"fable":  1_000_000,
	"gemini": 1_000_000,
	"gpt-5":  258_000,
	"codex":  258_000,
	"gpt-4":  128_000,
	"gpt-4o": 128_000,
}

// millionTokenWindow is the context window for 1M-token model variants.
const millionTokenWindow = 1_000_000

// claudeFamilies are the Claude model families whose context window scales to
// 1M when the model ID carries the "[1m]" suffix (e.g. "claude-opus-4-8[1m]").
// Without the suffix they use the 200K default in modelFamilyWindows.
//
// "fable" is deliberately NOT in this map — it is a flat 1M in
// modelFamilyWindows above, for the transcript reason documented there.
//
// Before ga-a306b1 "fable" was in neither table, so every fable model ID fell
// through to the unknown-family 0 and suppressed the context reading entirely
// (tail.go only builds ContextUsage when the window is > 0). That is the quiet
// direction of this failure: fable seats reported no context usage rather than
// a wrong one, so nothing looked broken.
//
// NOTE: opus/sonnet have the same transcript-suffix-loss exposure — a seat
// launched "opus[1m]" writes "claude-opus-5" into its transcript and is read
// here as 200k (45 of 45 sampled opus usage lines, 2026-09-14). Fixing that
// means telling the 1M generations apart from the dated 200k ones that share
// the family substring, which is a wider change than this bead: ga-i1755o.
var claudeFamilies = map[string]bool{"opus": true, "sonnet": true, "haiku": true}

// ModelContextWindow returns the context window size for a model ID.
// It parses the model ID to extract the family name and looks it up.
// Claude families carrying the "[1m]" suffix resolve to the 1M window so
// context utilization does not saturate against the 200K default; "fable" is
// 1M with or without the suffix, for the transcript reason above.
// Returns 0 if the model family is unknown.
func ModelContextWindow(model string) int {
	lower := strings.ToLower(model)
	// Try longer matches first to avoid "gpt-4" matching before "gpt-4o".
	for _, family := range []string{"gpt-4o", "gpt-5", "gpt-4", "opus", "sonnet", "haiku", "fable", "gemini", "codex"} {
		if strings.Contains(lower, family) {
			if claudeFamilies[family] && strings.Contains(lower, "[1m]") {
				return millionTokenWindow
			}
			return modelFamilyWindows[family]
		}
	}
	return 0
}
