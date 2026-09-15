package sessionlog

import "testing"

func TestModelContextWindow(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"claude-opus-4-5-20251101", 200_000},
		{"claude-sonnet-4-5-20251101", 200_000},
		{"claude-haiku-4-5-20251001", 200_000},
		// 1M-window Claude variants carry a "[1m]" suffix on the model ID.
		{"claude-opus-4-8[1m]", 1_000_000},
		{"sonnet[1m]", 1_000_000},
		{"claude-haiku-4-5-20251001[1m]", 1_000_000},
		// The fable enum's three live forms (ga-a306b1): the latest-tracking
		// alias, the explicit 5.1 pin, and the 5.0 pin the fleet runs today.
		// All three returned 0 before "fable" was a known family, which
		// suppressed the context reading for every fable seat rather than
		// reporting a wrong one.
		{"fable[1m]", 1_000_000},
		{"claude-fable-5-1[1m]", 1_000_000},
		{"claude-fable-5[1m]", 1_000_000},
		// The suffix-less forms must ALSO read 1M: the "[1m]" suffix never
		// survives into the transcript this package reads (sampled across the
		// live fleet 2026-09-14), so gating on it would report every 1M fable
		// seat as 200k — a meter 5x too full.
		{"claude-fable-5", 1_000_000},
		{"claude-fable-5-1", 1_000_000},
		{"gemini-2.5-pro", 1_000_000},
		{"gpt-5-20260101", 258_000},
		{"codex-mini-latest", 258_000},
		{"gpt-4o-2024-08-06", 128_000},
		{"gpt-4-turbo", 128_000},
		{"unknown-model-xyz", 0},
		{"", 0},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := ModelContextWindow(tt.model)
			if got != tt.want {
				t.Errorf("ModelContextWindow(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}
