package scripts_test

import (
	"os"
	"os/exec"
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
	if got := f.read(t, record); !strings.Contains(got, "--wait-s") ||
		!strings.Contains(got, "-- env GOMAXPROCS=2 GOFLAGS=-p=2 make test-fast-parallel") {
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

// pre-commit: every heavy Go step goes through the gate, capped, when the clone
// opts in; without the setting the same steps run directly (the control).
func newPreCommitFixture(t *testing.T) (repo string, env []string, goRuns, makeRuns string) {
	t.Helper()
	root := repoRoot(t)
	repo = t.TempDir()
	binDir := t.TempDir()
	rec := t.TempDir()
	goRuns, makeRuns = filepath.Join(rec, "go-runs"), filepath.Join(rec, "make-runs")
	env = append(os.Environ(),
		"PATH="+binDir+":/usr/bin:/bin",
		"GIT_CONFIG_GLOBAL="+filepath.Join(rec, "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(rec, "gitconfig-system"),
		"GO_RECORD="+goRuns, "MAKE_RECORD="+makeRuns, "BD_STDIN_RECORD="+filepath.Join(rec, "bd"),
	)
	writeExecutable(t, filepath.Join(binDir, "bd"), "#!/usr/bin/env sh\ncat > \"$BD_STDIN_RECORD\"\n")
	writeExecutable(t, filepath.Join(binDir, "go"), "#!/usr/bin/env sh\nprintf '%s|%s\\n' \"$*\" \"$GOMAXPROCS $GOFLAGS\" >> \"$GO_RECORD\"\n")
	writeExecutable(t, filepath.Join(binDir, "make"), "#!/usr/bin/env sh\nprintf '%s|%s\\n' \"$*\" \"$GOMAXPROCS $GOFLAGS\" >> \"$MAKE_RECORD\"\n")
	for _, rel := range []string{".githooks/pre-commit", ".githooks/lib/beads-chain.sh", ".githooks/lib/build-admit.sh"} {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(repo, rel), string(body))
	}
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(repo, "scripts", "precommit-format-staged-go"), "#!/usr/bin/env sh\ncat >/dev/null\n")
	// The files the hook re-stages after regenerating them must exist.
	for _, rel := range []string{"internal/api/openapi.json", "docs/reference/schema/openapi.json",
		"docs/reference/schema/openapi.txt", "internal/api/genclient/client_gen.go",
		"docs/reference/schema/city-schema.json", "docs/reference/schema/city-schema.txt",
		"docs/reference/config.md", "docs/reference/cli.md"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, rel), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		cmd := testCommand("git", args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	git("add", "-A")
	git("commit", "-q", "--no-verify", "-m", "base")
	if err := os.WriteFile(filepath.Join(repo, "touched.go"), []byte("package fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "touched.go")
	return repo, env, goRuns, makeRuns
}

func runPreCommit(t *testing.T, repo string, env []string) (int, string) {
	t.Helper()
	cmd := testCommand(filepath.Join(repo, ".githooks", "pre-commit"))
	cmd.Dir, cmd.Env = repo, env
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("run pre-commit: %v\n%s", err, out)
	}
	return exitErr.ExitCode(), string(out)
}

func TestPreCommitRunsItsGoStepsThroughTheBuildGateWhenConfigured(t *testing.T) {
	repo, env, goRuns, makeRuns := newPreCommitFixture(t)
	gateRecord := filepath.Join(t.TempDir(), "gate-args")
	gate := filepath.Join(t.TempDir(), "go-build-admit.py")
	writeExecutable(t, gate, "#!/usr/bin/env sh\nprintf '%s\\n' \"$*\" >> \""+gateRecord+"\"\nwhile [ \"$#\" -gt 0 ] && [ \"$1\" != \"--\" ]; do shift; done\nshift\nexec \"$@\"\n")
	env = append(env, "GO_BUILD_ADMIT="+gate)

	code, out := runPreCommit(t, repo, env)
	if code != 0 {
		t.Fatalf("pre-commit exit = %d, want 0\n%s", code, out)
	}
	gated, _ := os.ReadFile(gateRecord)
	for _, step := range []string{"make lint-changed", "go run ./cmd/genspec", "go generate ./internal/api/genclient",
		"go run ./cmd/genschema", "make vet"} {
		if !strings.Contains(string(gated), "env GOMAXPROCS=2 GOFLAGS=-p=2 "+step) {
			t.Fatalf("%q did not go through the gate capped; gate args:\n%s", step, gated)
		}
	}
	for _, rec := range []string{goRuns, makeRuns} {
		body, _ := os.ReadFile(rec)
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			if !strings.HasSuffix(line, "|2 -p=2") {
				t.Fatalf("a Go step ran without the caps: %q", line)
			}
		}
	}
}

func TestPreCommitRunsItsGoStepsDirectlyWithoutAGate(t *testing.T) {
	repo, env, goRuns, makeRuns := newPreCommitFixture(t)

	code, out := runPreCommit(t, repo, env)
	if code != 0 {
		t.Fatalf("pre-commit exit = %d, want 0\n%s", code, out)
	}
	gor, _ := os.ReadFile(goRuns)
	mk, _ := os.ReadFile(makeRuns)
	if !strings.Contains(string(gor), "run ./cmd/genspec") || !strings.Contains(string(mk), "vet") {
		t.Fatalf("control: the Go steps did not run directly: go=%q make=%q", gor, mk)
	}
}
