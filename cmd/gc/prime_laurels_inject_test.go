package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrimeLaurelsInjection(t *testing.T) {
	dir := t.TempDir()
	if got := primeLaurelsInjection(dir); got != "" {
		t.Fatalf("absent file: got %q, want nothing", got)
	}
	if got := primeLaurelsInjection(""); got != "" {
		t.Fatalf("no seat dir: got %q, want nothing", got)
	}
	path := filepath.Join(dir, laurelsFileName)
	if err := os.WriteFile(path, []byte("  \n\t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := primeLaurelsInjection(dir); got != "" {
		t.Fatalf("blank file: got %q, want nothing", got)
	}
	if err := os.WriteFile(path, []byte("Cherub, 2026-09-18: the reaper fix saved the operator checklist.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := primeLaurelsInjection(dir)
	for _, want := range []string{"<laurels>", "saved the operator checklist.", "no task, no bead and no priority", "</laurels>"} {
		if !strings.Contains(got, want) {
			t.Errorf("injection %q lacks %q", got, want)
		}
	}
}

func TestPrimeLaurelsInjectionCapsAtARuneBoundary(t *testing.T) {
	dir := t.TempDir()
	// 3-byte runes straddle the cap, so a byte cut would split one.
	if err := os.WriteFile(filepath.Join(dir, laurelsFileName), []byte(strings.Repeat("✓", laurelsMaxBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	got := primeLaurelsInjection(dir)
	if !utf8.ValidString(got) {
		t.Fatal("truncated laurels are not valid UTF-8")
	}
	if !strings.Contains(got, "[truncated]") {
		t.Fatal("an oversized file must say it was truncated")
	}
	if len(got) > laurelsMaxBytes+300 {
		t.Fatalf("injection is %d bytes; the cap is %d plus the wrapper", len(got), laurelsMaxBytes)
	}
}

// The laurels ride SessionStart only: a UserPromptSubmit hook must not repeat
// them on every turn.
func TestPrimeHookContextSuffixCarriesLaurelsAtSessionStartOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, laurelsFileName), []byte("A customer thanked this seat."), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DIR", dir)
	city := t.TempDir()
	start := primeHookContextSuffix(city, true, primeHookContext{HookEventName: "SessionStart"}, io.Discard, false)
	if !strings.Contains(start.text, "A customer thanked this seat.") {
		t.Fatalf("SessionStart context lacks the laurels: %q", start.text)
	}
	turn := primeHookContextSuffix(city, true, primeHookContext{HookEventName: "UserPromptSubmit"}, io.Discard, false)
	if strings.Contains(turn.text, "<laurels>") {
		t.Fatalf("a non-SessionStart hook carried the laurels: %q", turn.text)
	}
}
