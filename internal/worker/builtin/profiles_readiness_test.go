package builtin

import "testing"

// TestEveryBuiltinProfileDeclaresAReadinessSignal is the guard half of
// ga-8ouxd. The omp profile shipped with neither ReadyPromptPrefix nor
// ReadyDelayMs, so gc could never observe an omp session become ready: the
// session bead stayed in state=creating forever, the reconciler retried the
// start in a loop, and four named agents churned off the provider before the
// cause was found. Nothing structural prevented the next profile from
// shipping the same hole — this test is that structure. A provider that
// genuinely needs no readiness signal (a bounded one-shot command) is not a
// TUI profile and does not belong in this table without a comment and an
// explicit exemption here.
func TestEveryBuiltinProfileDeclaresAReadinessSignal(t *testing.T) {
	for name, p := range builtinProviderSpecs {
		if p.ReadyPromptPrefix == "" && p.ReadyDelayMs <= 0 {
			t.Errorf("profile %q declares neither ReadyPromptPrefix nor ReadyDelayMs — sessions on it will never leave state=creating (ga-8ouxd); set one of them", name)
		}
	}
}
