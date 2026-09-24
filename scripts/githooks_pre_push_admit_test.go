package scripts_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// The push-time suite runs through the host's go-build-admit gate when a clone
// opts in (ga-wirl8l.2). A stub gate records its arguments and then runs the
// command after "--", the way the real gate does once it admits.
func stubAdmitGate(t *testing.T, f *prePushFixture, exitCode string) (gate, record string) {
	t.Helper()
	gate = filepath.Join(f.binDir, "go-build-admit.py")
	record = filepath.Join(t.TempDir(), "gate-args")
	writeExecutable(t, gate, `#!/usr/bin/env sh
printf '%s\n' "$*" >> "`+record+`"
if [ "`+exitCode+`" != "0" ]; then exit `+exitCode+`; fi
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift
exec "$@"
`)
	return gate, record
}

func TestPrePushRunsTheSuiteThroughTheBuildGateWhenConfigured(t *testing.T) {
	f := newPrePushFixture(t)
	gate, record := stubAdmitGate(t, f, "0")
	f.git(t, "config", "gascity.buildAdmit", gate)
	refLine := "refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"

	code, out := f.run(t, refLine)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, record); !strings.Contains(got, "--wait-s") || !strings.Contains(got, "-- make test-fast-parallel") {
		t.Fatalf("the suite did not go through the gate: gate args = %q", got)
	}
	got := f.read(t, f.makeRuns)
	for _, want := range []string{"test-fast-parallel", "LOCAL_TEST_JOBS=1", "EXTRA_TEST_ENV=GOMAXPROCS=2 GC_TEST_INNER_P=2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("make invocation %q lacks %q: the suite must run serialized and capped", got, want)
		}
	}
}

func TestPrePushRefusesThePushWhenTheGateDoesNotAdmit(t *testing.T) {
	f := newPrePushFixture(t)
	gate, _ := stubAdmitGate(t, f, "75")
	f.env = append(f.env, "GO_BUILD_ADMIT="+gate)
	refLine := "refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"

	code, out := f.run(t, refLine)
	if code != 75 {
		t.Fatalf("pre-push exit = %d, want 75 (not admitted must refuse the push, never skip the suite)\n%s", code, out)
	}
	if !strings.Contains(out, "did not admit") {
		t.Fatalf("no refusal message on a not-admitted suite:\n%s", out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("the suite ran although the gate refused: %q", got)
	}
}

func TestPrePushRefusesWhenTheConfiguredGateIsMissing(t *testing.T) {
	f := newPrePushFixture(t)
	f.git(t, "config", "gascity.buildAdmit", filepath.Join(f.binDir, "no-such-gate.py"))
	refLine := "refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"

	code, out := f.run(t, refLine)
	if code != 1 {
		t.Fatalf("pre-push exit = %d, want 1 for a configured gate that does not exist\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("the ungated suite ran although a gate was configured: %q", got)
	}
}
