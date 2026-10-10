package scripts_test

import (
	"strings"
	"testing"
)

// bazelUpstreamRepository is the one repository Blacksmith runners and the
// remote-execution secrets exist for.
const bazelUpstreamRepository = "github.repository == 'gastownhall/gascity'"

// TestBazelWorkflowForkRunnerRule pins the fork carry's rule for bazel.yml
// (pl-axh9, fork #198; re-expressed at re-sync #6 after upstream #7239 folded
// bazel-test.yml into it), on every job rather than a list of them, so a job
// upstream adds or moves trips it at the next rebase: the quickserve-ai fork
// has no Blacksmith runners, and a job that asks for one sits queued there
// forever. A job on a Blacksmith label either takes the repository-aware
// runs-on (upstream's label upstream, ubuntu-latest anywhere else) or runs
// upstream only by a repository term in its if.
func TestBazelWorkflowForkRunnerRule(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	if len(wf.Jobs) == 0 {
		t.Fatalf("%s: no jobs", bazelMultiLaneWorkflow)
	}
	for id, job := range wf.Jobs {
		if !strings.Contains(job.RunsOn, "blacksmith") {
			continue
		}
		repositoryAware := strings.HasPrefix(job.RunsOn, "${{ "+bazelUpstreamRepository+" && ") &&
			strings.HasSuffix(job.RunsOn, " || 'ubuntu-latest' }}")
		upstreamOnly := strings.Contains(job.If, bazelUpstreamRepository)
		if !repositoryAware && !upstreamOnly {
			t.Errorf("job %s runs-on %q, if %q: on the fork it queues forever; give it the repository-aware runs-on or, if it needs the remote-execution secret or a Blacksmith host, %q in its if",
				id, job.RunsOn, job.If, bazelUpstreamRepository)
		}
	}
	// The required checks' jobs and the lanes they wait for must run on the
	// fork, not be skipped there: a skipped required check reads as passed.
	for _, id := range []string{"rbe", "lane", "sync-check", "gate"} {
		job, ok := wf.Jobs[id]
		if !ok {
			t.Errorf("%s: no job %s", bazelMultiLaneWorkflow, id)
			continue
		}
		if strings.Contains(job.If, bazelUpstreamRepository) {
			t.Errorf("job %s is upstream only (if %q); the fork's required checks need it", id, job.If)
		}
	}
}

// TestBazelWorkflowForkUnitLaneSwap pins the fork's unit-lane swapfile
// (ga-l0b72a.3). Without it the 16 GB GitHub-hosted runner runs out of memory
// at nogo on //cmd/gc:gc_test and the VM shuts the runner down, so the unit
// lane never concludes on the fork. The step must run on the fork only, and
// before the lane's bazel step, and the bazel step caps the fork's unit lane
// at two parallel actions (the swap alone did not hold it).
func TestBazelWorkflowForkUnitLaneSwap(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	job, ok := wf.Jobs["lane"]
	if !ok {
		t.Fatalf("%s: no job lane", bazelMultiLaneWorkflow)
	}
	swap, test := -1, -1
	for i, step := range job.Steps {
		switch {
		case step.Name == "Fork runner swap for the unit lane":
			swap = i
			if !strings.Contains(step.If, "github.repository != 'gastownhall/gascity'") || !strings.Contains(step.If, "matrix.lane == 'unit'") {
				t.Errorf("swap step if %q: must be fork-only and unit-lane-only", step.If)
			}
			if !strings.Contains(step.Run, "swapon /swapfile-bazel") {
				t.Errorf("swap step does not enable /swapfile-bazel:\n%s", step.Run)
			}
		case step.ID == "test":
			test = i
			if step.Env["FORK_RUNNER"] != "${{ github.repository != 'gastownhall/gascity' }}" {
				t.Errorf("bazel test step env FORK_RUNNER = %q; want the fork-only repository test", step.Env["FORK_RUNNER"])
			}
			if !strings.Contains(step.Run, `if [ "$FORK_RUNNER" = true ] && [ "$LANE" = unit ]; then`) || !strings.Contains(step.Run, "args+=(--jobs=2)") {
				t.Errorf("bazel test step does not cap the fork's unit lane at --jobs=2:\n%s", step.Run)
			}
		}
	}
	if swap < 0 {
		t.Fatalf("job lane has no %q step", "Fork runner swap for the unit lane")
	}
	if test < 0 || swap > test {
		t.Errorf("swap step (index %d) must come before the bazel test step (index %d)", swap, test)
	}
}
