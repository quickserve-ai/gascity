package scripts_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gastownhall/gascity/internal/testpolicy/shardbudget"
)

const shardBalanceStep = "Shard balance"

// TestBazelShardBalanceStepSkipsMissingLanes runs the bep-summary job's
// "Shard balance" arguments, as the step writes them, through shardbudget.Run.
// Some lanes never upload a BEP file in a given run (unit-gc exists only on
// the fork, integration only on dispatch and schedule), so the step passes
// --allow-missing. Go's flag package stops at the first positional argument,
// so the flag must come before every BEP path: placed after them it is read
// as one more path, the first missing file ends the run with exit 1, and no
// lane after it is reported (ga-ltp626, found by the cross-family read of
// gascity #364). The step is continue-on-error, so nothing else would show it.
func TestBazelShardBalanceStepSkipsMissingLanes(t *testing.T) {
	var wf struct {
		Jobs map[string]bepWorkflowJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), bazelMultiLaneWorkflow)), &wf); err != nil {
		t.Fatalf("parse %s: %v", bazelMultiLaneWorkflow, err)
	}
	job := wf.Jobs[bepSummaryJob]
	i := bepStepIndex(job.Steps, func(s bepWorkflowStep) bool { return s.Name == shardBalanceStep })
	if i < 0 {
		t.Fatalf("%s job has no %q step", bepSummaryJob, shardBalanceStep)
	}
	run := strings.ReplaceAll(job.Steps[i].Run, "\\\n", " ")
	const tool = "go run ./scripts/shard-budget.go"
	_, rest, ok := strings.Cut(run, tool)
	if !ok {
		t.Fatalf("%q does not run %s:\n%s", shardBalanceStep, tool, job.Steps[i].Run)
	}
	rest, _, _ = strings.Cut(rest, ">>")
	rest, _, _ = strings.Cut(rest, "\n")

	dir := t.TempDir()
	// The attempt expression holds spaces: substitute before splitting.
	rest = strings.ReplaceAll(rest, "${{ github.run_attempt }}", "1")
	var args []string
	for _, f := range strings.Fields(rest) {
		f = strings.Trim(f, `"`)
		f = strings.ReplaceAll(f, "$dir", dir)
		args = append(args, f)
	}

	// Every lane any run starts is read.
	for _, lane := range multiLaneLaneNames() {
		want := filepath.Join(dir, "bazel-bep-"+lane+"-1", "bazel-bep.json")
		found := false
		for _, a := range args {
			found = found || a == want
		}
		if !found {
			t.Errorf("%q does not read lane %s's BEP file (%s); args %v", shardBalanceStep, lane, want, args)
		}
	}

	// Only acceptance uploaded a BEP file: every lane before it is missing.
	// Its one target has a straggler shard, so a report must name it.
	const label = "//test/acceptance:shard_balance_probe_test"
	var bep bytes.Buffer
	for shard, ms := range []int{60000, 60000, 60000, 900000} {
		fmt.Fprintf(&bep, `{"id":{"testResult":{"label":%q,"run":1,"shard":%d,"attempt":1}},"testResult":{"status":"PASSED","testAttemptDurationMillis":"%d"}}`+"\n", label, shard+1, ms)
	}
	acc := filepath.Join(dir, "bazel-bep-acceptance-1", "bazel-bep.json")
	if err := os.MkdirAll(filepath.Dir(acc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(acc, bep.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if rc := shardbudget.Run(args, &stdout, &stderr); rc != 0 {
		t.Fatalf("%q exits %d with only the acceptance BEP present (args %v); a missing lane must be skipped:\n%s%s", shardBalanceStep, rc, args, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), label) {
		t.Errorf("%q reported nothing for %s after skipping the missing lanes:\nstdout:\n%s\nstderr:\n%s", shardBalanceStep, label, stdout.String(), stderr.String())
	}
}
