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
	dir := laurelsSeatHome(t)
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
	dir := laurelsSeatHome(t)
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

// Codex review of #102: a cap-sized paragraph with its customary trailing newline
// made the old cut index one past the trimmed text and panic in SessionStart.
func TestPrimeLaurelsInjectionCapSizedParagraphWithNewline(t *testing.T) {
	dir := laurelsSeatHome(t)
	body := strings.Repeat("a", laurelsMaxBytes)
	if err := os.WriteFile(filepath.Join(dir, laurelsFileName), []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := primeLaurelsInjection(dir)
	if !strings.Contains(got, body) {
		t.Fatal("a cap-sized paragraph must survive whole")
	}
	if strings.Contains(got, "[truncated]") {
		t.Fatal("only a trailing newline was dropped; nothing was truncated")
	}
}

func TestPrimeLaurelsInjectionReadsTheSeatHomeFirst(t *testing.T) {
	dir := laurelsSeatHome(t)
	if err := os.MkdirAll(filepath.Join(dir, "seat"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seat", laurelsFileName), []byte("from the seat home"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, laurelsFileName), []byte("from the flat home"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := primeLaurelsInjection(dir); !strings.Contains(got, "from the seat home") || strings.Contains(got, "from the flat home") {
		t.Fatalf("want seat/laurels.md to win: %q", got)
	}
}

func TestPrimeLaurelsInjectionIgnoresANonRegularFile(t *testing.T) {
	dir := laurelsSeatHome(t)
	// A directory stands in for any non-regular path (a FIFO would block a read).
	if err := os.MkdirAll(filepath.Join(dir, laurelsFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := primeLaurelsInjection(dir); got != "" {
		t.Fatalf("non-regular laurels path: got %q, want nothing", got)
	}
}

// The laurels ride SessionStart only: a UserPromptSubmit hook must not repeat
// them on every turn.
func TestPrimeHookContextSuffixCarriesLaurelsAtSessionStartOnly(t *testing.T) {
	// seat/laurels.md is read in any GC_DIR. A .gc/agents home here would make
	// the temp dir look like a city and start a dolt server for it.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "seat"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seat", laurelsFileName), []byte("A customer thanked this seat."), 0o644); err != nil {
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

// laurelsSeatHome makes a city seat's .gc/agents/<name> home, the only GC_DIR
// where a flat laurels.md is read.
func laurelsSeatHome(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".gc", "agents", "seat-under-test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Codex review of #102, round 3: opening the path followed a symlinked
// laurels.md, so a rig could point it at a credential and have it sent to the
// provider at SessionStart. Both homes must refuse a symlink.
func TestPrimeLaurelsInjectionRefusesASymlink(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(secret, []byte("SECRET-TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{laurelsFileName, filepath.Join("seat", laurelsFileName)} {
		dir := laurelsSeatHome(t)
		link := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, link); err != nil {
			t.Fatal(err)
		}
		if got := primeLaurelsInjection(dir); got != "" {
			t.Fatalf("%s is a symlink: got %q, want nothing", rel, got)
		}
	}
}

// Codex review of #102, round 3: a rig seat's GC_DIR is its project checkout,
// whose root laurels.md is repository content. Only seat/laurels.md counts there.
func TestPrimeLaurelsInjectionReadsTheFlatFileOnlyInACitySeatHome(t *testing.T) {
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, laurelsFileName), []byte("a repo file named laurels.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := primeLaurelsInjection(checkout); got != "" {
		t.Fatalf("a checkout root laurels.md was injected: %q", got)
	}
	if err := os.MkdirAll(filepath.Join(checkout, "seat"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "seat", laurelsFileName), []byte("from the seat home"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := primeLaurelsInjection(checkout); !strings.Contains(got, "from the seat home") {
		t.Fatalf("seat/laurels.md in a checkout was not injected: %q", got)
	}
}
