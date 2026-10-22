package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// An unapplied patch has to reach the operator on EVERY command, because the
// whole reason it is now safe to skip a dangling patch instead of aborting the
// load is that the skip is loud (ga-djvbvp). The always+fresh advisory is the
// deliberate contrast: a standing configuration choice stays off unrelated
// commands, a condition of this invocation does not.
func TestUnappliedPatchWarningIsAlwaysEmitted(t *testing.T) {
	warning := `patches.agent[12] was NOT APPLIED: agent "cherub-law.conway" not found in merged config`

	if !config.IsUnappliedPatchWarning(warning) {
		t.Fatal("IsUnappliedPatchWarning did not recognize the emitted warning shape — the marker and the emitter have drifted apart")
	}
	if !shouldEmitLoadCityConfigWarning(warning) {
		t.Fatal("an unapplied patch must print on every command; suppressing it turns a loud outage into a silent misconfiguration")
	}
	if !isNonFatalLoadConfigWarning(warning) {
		t.Fatal("an unapplied patch must not be strict-fatal; that is the city-wide halt this change exists to remove")
	}

	// CONTRAST, so this test fails if the filter degenerates into "emit
	// everything": the always+fresh advisory must still be suppressed.
	fresh := `named_session "x": mode "always" with wake_mode "fresh" on template "x" starts a fresh provider session after every drain; use only for a deliberate restart-per-cycle actor`
	if shouldEmitLoadCityConfigWarning(fresh) {
		t.Fatal("the always+fresh advisory must stay off unrelated commands (ga-3upjic); the filter is now emitting everything")
	}
	if config.IsUnappliedPatchWarning(fresh) {
		t.Fatal("the always+fresh advisory must not be classified as an unapplied patch")
	}
}

// The emitter must actually put the warning on the writer, not merely classify
// it. Without this, a correct classifier and a broken emit path look identical.
func TestUnappliedPatchWarningReachesTheWriter(t *testing.T) {
	warning := `patches.rigs[3] was NOT APPLIED: rig "ghost" not found in merged config`
	var buf bytes.Buffer
	emitLoadCityConfigWarnings(&buf, &config.Provenance{Warnings: []string{warning}})
	if !strings.Contains(buf.String(), "ghost") {
		t.Fatalf("unapplied-patch warning never reached the writer; got %q", buf.String())
	}
}

// `gc config --validate` must FAIL on an unapplied patch (ga-djvbvp). This is
// the half that keeps a typo from living forever now that it no longer aborts
// every command — and a mutation deleting the promotion survived the suite
// until this test existed.
func TestUnappliedPatchFailsConfigValidate(t *testing.T) {
	warnings := []string{
		`patches.agent[12] was NOT APPLIED: agent "cherub-law.conway" not found in merged config`,
		`named_session "x": mode "always" with wake_mode "fresh" starts a fresh provider session after every drain`,
		`patches.rigs[0] was NOT APPLIED: rig "ghostrig" not found in merged config`,
	}
	errs := unappliedPatchValidationErrors(warnings)
	if len(errs) != 2 {
		t.Fatalf("want both unapplied patches promoted to validation errors, got %d: %q", len(errs), errs)
	}
	// BOTH, not just the first: the old ApplyPatches returned on the first bad
	// patch and reported one defect per run.
	if !strings.Contains(errs[0], "cherub-law.conway") || !strings.Contains(errs[1], "ghostrig") {
		t.Fatalf("promoted the wrong warnings: %q", errs)
	}
	// NON-VACUITY: an unrelated advisory must NOT become a validation error,
	// or --validate fails on every city that carries a standing advisory.
	if len(unappliedPatchValidationErrors(warnings[1:2])) != 0 {
		t.Fatal("the always+fresh advisory was promoted to a validation error")
	}
	if len(unappliedPatchValidationErrors(nil)) != 0 {
		t.Fatal("a clean config produced validation errors")
	}
}
