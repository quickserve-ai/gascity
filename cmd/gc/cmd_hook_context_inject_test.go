package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The context+clock advisory (context_inject.go / clock_inject.go) is delivered
// on UserPromptSubmit. Before this command it rode inside `gc nudge drain
// --inject`, whose deferred flush lands AFTER the nudge-queue store round-trip;
// on the single-store topology that round-trip is 33–35s, so `gc hook run
// --timeout 15s` killed the child and DISCARDED its buffered stdout — dropping
// the advisory on every prompt for every laptop crew session (qc-s3i236.4).
//
// cmdHookContextInject is the store-free carrier: it computes clock + context
// from the hook input alone and flushes immediately, touching no store. These
// tests pin (1) that it emits the advisory for a high-context transcript and
// (2) that the nudge-store target env vars — the exact inputs that make nudge
// drain round-trip the remote store — have no effect on it, i.e. it is
// store-independent by construction.

// highContextHookInput returns UserPromptSubmit hook JSON pointing at a
// transcript whose last usage entry sits at 90% of a 1M window.
func highContextHookInput(t *testing.T) []byte {
	t.Helper()
	// 40k input + 830k cache read + 30k cache create = 900k of 1,000,000 = 90%.
	p := writeTranscript(t, usageLine("claude-opus-4-8[1m]", 40_000, 830_000, 30_000))
	return hookInputFor(p)
}

func TestHookContextInjectEmitsAdvisoryForHighTranscript(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_INJECT_CLOCK", "")

	var stdout, stderr bytes.Buffer
	// Plain-text (claude) provider: no --hook-format.
	code := cmdHookContextInject("", highContextHookInput(t), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHookContextInject = %d, want 0 (fail-open); stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Current time:", "Context usage:", "900k/1000k", "~90%", "HIGH", "gc session reset"} {
		if !strings.Contains(out, want) {
			t.Errorf("advisory output missing %q; got %q", want, out)
		}
	}
}

// TestHookContextInjectIsStoreFreeUnderTargetEnv proves the fast path never
// touches the nudge store: the env vars nudge drain uses to resolve a target
// and round-trip the remote store (GC_SESSION_ID, GC_ALIAS, GC_CITY) have zero
// effect on the output. Clock is disabled here so the compared bytes depend
// only on the transcript, not on wall-clock time.
func TestHookContextInjectIsStoreFreeUnderTargetEnv(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_INJECT_CLOCK", "off")
	input := highContextHookInput(t)

	render := func() string {
		var stdout, stderr bytes.Buffer
		if code := cmdHookContextInject("", input, &stdout, &stderr); code != 0 {
			t.Fatalf("cmdHookContextInject = %d, want 0; stderr=%s", code, stderr.String())
		}
		return stdout.String()
	}

	base := render()
	if !strings.Contains(base, "Context usage:") {
		t.Fatalf("expected a context advisory with no store configured, got %q", base)
	}

	// Set the exact inputs that make `gc nudge drain --inject` resolve a target
	// and round-trip the remote single-store dolt server. A store-free carrier
	// must ignore them entirely.
	t.Setenv("GC_SESSION_ID", "sess-store-target")
	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_CITY", t.TempDir())
	withTarget := render()

	if base != withTarget {
		t.Errorf("nudge-store target env changed context-inject output (path is not store-free):\n base=%q\n with=%q", base, withTarget)
	}
}

// Below the advisory threshold the context line is silent, but the clock still
// rides — the fast path is the sole clock carrier once it is split from nudge
// drain, so it must deliver the clock on every prompt regardless of context.
func TestHookContextInjectClockRidesBelowAdvisory(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_INJECT_CLOCK", "")
	// 10% of a 1M window — well below advisory; context line is empty.
	p := writeTranscript(t, usageLine("claude-fable-5", 1_000, 98_000, 1_000))

	var stdout, stderr bytes.Buffer
	if code := cmdHookContextInject("", hookInputFor(p), &stdout, &stderr); code != 0 {
		t.Fatalf("cmdHookContextInject = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Current time:") {
		t.Errorf("clock must ride even below the context advisory, got %q", out)
	}
	if strings.Contains(out, "Context usage:") {
		t.Errorf("context line must be silent below advisory, got %q", out)
	}
}

// The JSON providers (codex/gemini) must receive exactly one valid JSON
// document from the command — clock and context folded into one additional
// context payload, never two concatenated objects.
func TestHookContextInjectCodexSingleJSONDocument(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_INJECT_CONTEXT", "")
	t.Setenv("GC_INJECT_CLOCK", "")

	var stdout, stderr bytes.Buffer
	if code := cmdHookContextInject("codex", highContextHookInput(t), &stdout, &stderr); code != 0 {
		t.Fatalf("cmdHookContextInject = %d, want 0; stderr=%s", code, stderr.String())
	}

	dec := json.NewDecoder(&stdout)
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode first JSON document: %v", err)
	}
	if dec.More() {
		t.Fatalf("stdout has more than one JSON document for codex format")
	}
	hook, ok := doc["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("missing hookSpecificOutput object, got %#v", doc)
	}
	ctx, ok := hook["additionalContext"].(string)
	if !ok {
		t.Fatalf("missing additionalContext string, got %#v", hook)
	}
	for _, want := range []string{"Current time:", "Context usage:", "HIGH"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("additionalContext missing %q, got %q", want, ctx)
		}
	}
}
