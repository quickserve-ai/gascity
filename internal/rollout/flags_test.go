package rollout

import "testing"

// TestNoticesReturnsDefensiveCopy proves a caller cannot mutate a Flags' retained
// notices through the slice Notices() returns.
func TestNoticesReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()
	f, err := Resolve(cityWith("require", nil),
		ResolveOptions{LookupEnv: envMap(map[string]string{envBeadsConditionalWrites: "auto"})})
	if err != nil {
		t.Fatal(err)
	}
	n1 := f.Notices()
	if len(n1) == 0 {
		t.Fatal("expected at least one notice (env overrides config)")
	}
	n1[0].Message = "MUTATED"
	if f.Notices()[0].Message == "MUTATED" {
		t.Error("Notices() must return a defensive copy; a caller's mutation leaked into the Flags")
	}
}

// TestZeroFlagsIsLegacy pins the documented degraded-safe zero value: an unwired
// Flags{} runs legacy paths (not the builtin defaults) and reports no origin.
func TestZeroFlagsIsLegacy(t *testing.T) {
	t.Parallel()
	var z Flags
	if z.BeadsConditionalWrites() != ModeUnset {
		t.Errorf("zero beads = %q, want ModeUnset", z.BeadsConditionalWrites())
	}
	if z.FormulaV2() {
		t.Errorf("zero formula_v2 = true, want false (legacy path, not the builtin default true)")
	}
	if z.OriginOf(keyBeadsConditionalWrites) != "" {
		t.Errorf("zero OriginOf = %q, want empty (unwired)", z.OriginOf(keyBeadsConditionalWrites))
	}
}
