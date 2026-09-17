package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/pflag"
)

// bdCreateChildLabelsOracle answers "what labels does bd give the child this
// argv creates" WITHOUT using the code under test. It parses the argv with
// pflag — the library bd's cobra command parses it with — registering the
// create flags these tests exercise exactly as bd registers them
// (StringSliceP labels/-l, the hidden StringSlice label alias, the Bool
// no-inherit-labels), then applies bd's own rule from cmd/bd/create.go:
// NormalizeLabels(labels ++ label), merged with the parent's labels unless
// --no-inherit-labels. An independent oracle is the point: a parser bug shared
// by the implementation and its test would otherwise pass.
func bdCreateChildLabelsOracle(t *testing.T, bdArgs []string, parentLabels []string) []string {
	t.Helper()
	verbAt := -1
	for i, a := range bdArgs {
		if a == "create" || a == "new" {
			verbAt = i
			break
		}
	}
	if verbAt < 0 {
		t.Fatalf("oracle: no create verb in %q", bdArgs)
	}
	fs := pflag.NewFlagSet("bd create", pflag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	labelsFlag := fs.StringSliceP("labels", "l", []string{}, "")
	labelAlias := fs.StringSlice("label", []string{}, "")
	noInherit := fs.Bool("no-inherit-labels", false, "")
	parent := fs.String("parent", "", "")
	fs.StringP("description", "d", "", "")
	fs.StringP("assignee", "a", "", "")
	fs.StringP("type", "t", "", "")
	fs.StringP("priority", "p", "", "")
	fs.StringP("file", "f", "", "")
	// bd's hidden description aliases (cmd/bd/flags.go registerCommonIssueFlags).
	fs.String("body", "", "")
	fs.StringP("message", "m", "", "")
	fs.String("description-file", "", "")
	fs.BoolP("quiet", "q", false, "")
	fs.Bool("json", false, "")
	fs.Bool("dry-run", false, "")
	if err := fs.Parse(bdArgs[verbAt+1:]); err != nil {
		t.Fatalf("oracle: bd would reject argv %q: %v", bdArgs, err)
	}
	var explicit []string
	seen := map[string]bool{}
	for _, l := range append(append([]string(nil), *labelsFlag...), *labelAlias...) {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		explicit = append(explicit, l)
	}
	merged := explicit
	if *parent != "" && !*noInherit {
		for _, l := range parentLabels {
			if !seen[l] {
				seen[l] = true
				merged = append(merged, l)
			}
		}
	}
	out := append([]string(nil), merged...)
	sort.Strings(out)
	return out
}

func sortedLabels(labels ...string) []string {
	out := append([]string(nil), labels...)
	sort.Strings(out)
	return out
}

// specimenParentLabels is the qc-88kjhl / qc-ntokpb.4 shape: dimensional
// labels that cascade by design alongside state labels that must not.
var specimenParentLabels = []string{"town:x", "ready:y", "hold:cert-wait", "cert:action", "needs-summon"}

type bdCreateParentLabelsCall struct {
	calls []string
}

func (c *bdCreateParentLabelsCall) reader(labels []string, err error) func(string) ([]string, error) {
	return func(parentID string) ([]string, error) {
		c.calls = append(c.calls, parentID)
		return labels, err
	}
}

func runStripInheritedStateLabels(t *testing.T, args []string, parentLabels []string, readErr error) (out []string, stderr string, calls []string) {
	t.Helper()
	var buf strings.Builder
	rec := &bdCreateParentLabelsCall{}
	out = stripInheritedStateLabelsFromBdCreateArgs(args, rec.reader(parentLabels, readErr), &buf)
	return out, buf.String(), rec.calls
}

func TestStripInheritedStateLabelsChildGetsOnlyDimensionalLabels(t *testing.T) {
	args := []string{"create", "Follow-up from the gate", "--parent", "qc-88kjhl", "-t", "task"}
	out, stderr, calls := runStripInheritedStateLabels(t, args, specimenParentLabels, nil)

	if !slices.Equal(calls, []string{"qc-88kjhl"}) {
		t.Fatalf("parent label reads = %q, want exactly [qc-88kjhl]", calls)
	}
	got := bdCreateChildLabelsOracle(t, out, specimenParentLabels)
	if want := sortedLabels("town:x", "ready:y"); !slices.Equal(got, want) {
		t.Fatalf("child labels = %q, want %q (argv %q)", got, want, out)
	}
	for _, stripped := range []string{"hold:cert-wait", "cert:action", "needs-summon"} {
		if !strings.Contains(stderr, stripped) {
			t.Errorf("stderr does not name stripped label %q: %q", stripped, stderr)
		}
	}
	for _, kept := range []string{"town:x", "ready:y"} {
		if strings.Contains(stderr, kept) {
			t.Errorf("stderr names kept dimensional label %q as stripped: %q", kept, stderr)
		}
	}
	if !strings.Contains(stderr, "qc-p9m8oa9") || strings.Count(stderr, "\n") != 1 {
		t.Errorf("want exactly one log line citing qc-p9m8oa9, got %q", stderr)
	}
	// The caller's own tokens survive verbatim, in order: the rewrite only adds.
	if !isSubsequence(args, out) {
		t.Errorf("rewritten argv %q dropped or reordered caller tokens %q", out, args)
	}
}

func TestStripInheritedStateLabelsWithoutTheInterimTheChildInheritsState(t *testing.T) {
	// The defect itself, pinned against the oracle: the untouched argv gives
	// the child every parent label. If this stops holding, bd changed and the
	// interim's removal condition may have been met.
	args := []string{"create", "Follow-up", "--parent", "qc-88kjhl"}
	got := bdCreateChildLabelsOracle(t, args, specimenParentLabels)
	if want := sortedLabels(specimenParentLabels...); !slices.Equal(got, want) {
		t.Fatalf("oracle child labels for untouched argv = %q, want all parent labels %q", got, want)
	}
}

func TestStripInheritedStateLabelsKeepsExplicitlyPassedStateLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"-l value", []string{"create", "t", "--parent", "qc-1", "-l", "hold:cert-wait"}},
		{"-l attached", []string{"create", "t", "--parent", "qc-1", "-lhold:cert-wait"}},
		{"-l=value", []string{"create", "t", "--parent", "qc-1", "-l=hold:cert-wait"}},
		{"shorthand cluster", []string{"create", "t", "--parent", "qc-1", "-ql", "hold:cert-wait"}},
		{"--labels csv", []string{"create", "t", "--labels", "extra,hold:cert-wait", "--parent", "qc-1"}},
		{"--labels=csv", []string{"create", "t", "--labels=extra, hold:cert-wait", "--parent=qc-1"}},
		{"--label alias", []string{"create", "t", "--parent", "qc-1", "--label", "hold:cert-wait"}},
		{"repeated flags", []string{"create", "t", "-l", "extra", "--parent", "qc-1", "--label=hold:cert-wait"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, stderr, _ := runStripInheritedStateLabels(t, tc.args, specimenParentLabels, nil)
			got := bdCreateChildLabelsOracle(t, out, specimenParentLabels)
			if !slices.Contains(got, "hold:cert-wait") {
				t.Fatalf("child labels = %q lost the explicitly passed hold:cert-wait (argv %q)", got, out)
			}
			for _, l := range []string{"town:x", "ready:y"} {
				if !slices.Contains(got, l) {
					t.Fatalf("child labels = %q lost dimensional %q (argv %q)", got, l, out)
				}
			}
			for _, l := range []string{"cert:action", "needs-summon"} {
				if slices.Contains(got, l) {
					t.Fatalf("child labels = %q still inherit %q (argv %q)", got, l, out)
				}
			}
			if strings.Contains(stderr, "hold:cert-wait") {
				t.Errorf("stderr reports an explicitly passed label as stripped: %q", stderr)
			}
			if !isSubsequence(tc.args, out) {
				t.Errorf("rewritten argv %q dropped or reordered caller tokens %q", out, tc.args)
			}
		})
	}
}

func TestStripInheritedStateLabelsAllStateLabelsExplicitIsUntouched(t *testing.T) {
	args := []string{"create", "t", "--parent", "qc-1", "-l", "hold:cert-wait,cert:action,needs-summon"}
	out, stderr, _ := runStripInheritedStateLabels(t, args, specimenParentLabels, nil)
	if !slices.Equal(out, args) || stderr != "" {
		t.Fatalf("argv = %q stderr = %q, want untouched and silent", out, stderr)
	}
}

func TestStripInheritedStateLabelsParentWithoutStateLabelsIsUntouchedAndSilent(t *testing.T) {
	parent := []string{"town:x", "ready:y", "init:qc-1"}
	args := []string{"create", "t", "--parent", "qc-1"}
	out, stderr, calls := runStripInheritedStateLabels(t, args, parent, nil)
	if !slices.Equal(out, args) {
		t.Fatalf("argv = %q, want untouched %q", out, args)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want silent", stderr)
	}
	if len(calls) != 1 {
		t.Fatalf("parent label reads = %q, want one", calls)
	}
	got := bdCreateChildLabelsOracle(t, out, parent)
	if want := sortedLabels(parent...); !slices.Equal(got, want) {
		t.Fatalf("child labels = %q, want every dimensional parent label %q", got, want)
	}
}

func TestStripInheritedStateLabelsLeavesNonParentCreatesAndOtherVerbsAlone(t *testing.T) {
	for _, args := range [][]string{
		{"create", "no parent here", "-l", "hold:cert-wait"},
		{"create", "t", "--description", "--parent"},               // a VALUE that looks like the flag
		{"create", "t", "-d", "--parent qc-1"},                     // same, shorthand
		{"create", "t", "--parent", "qc-1", "--no-inherit-labels"}, // caller opted out already
		{"create", "t", "--parent", "qc-1", "--no-inherit-labels=true"},
		{"create", "--file", "plan.md", "--parent", "qc-1"}, // batch create: bd ignores --parent
		{"create", "t", "--", "--parent", "qc-1"},           // after the terminator: positional
		{"update", "qc-1", "--parent", "qc-2"},
		{"q", "quick", "--parent", "qc-1"},
		{"list", "--parent", "qc-1"},
		{"show", "qc-1"},
		{},
	} {
		out, stderr, calls := runStripInheritedStateLabels(t, args, specimenParentLabels, nil)
		if !slices.Equal(out, args) {
			t.Errorf("argv %q rewritten to %q, want untouched", args, out)
		}
		if stderr != "" {
			t.Errorf("argv %q: stderr = %q, want silent", args, stderr)
		}
		if len(calls) != 0 {
			t.Errorf("argv %q: read parent labels %q, want no store round-trip", args, calls)
		}
	}
}

func TestStripInheritedStateLabelsHonorsExplicitInheritFalse(t *testing.T) {
	// bd takes the last occurrence of a flag, so the guard's
	// --no-inherit-labels must land after a caller's =false, not before it.
	for _, args := range [][]string{
		{"create", "t", "--parent", "qc-1", "--no-inherit-labels=false"},
		{"create", "--no-inherit-labels=true", "t", "--no-inherit-labels=false", "--parent", "qc-1", "-d", "x"},
	} {
		out, _, calls := runStripInheritedStateLabels(t, args, specimenParentLabels, nil)
		if len(calls) != 1 {
			t.Fatalf("argv %q still inherits, so the parent must be read; reads = %q", args, calls)
		}
		got := bdCreateChildLabelsOracle(t, out, specimenParentLabels)
		if want := sortedLabels("town:x", "ready:y"); !slices.Equal(got, want) {
			t.Fatalf("child labels = %q, want %q (argv %q)", got, want, out)
		}
		if !isSubsequence(args, out) {
			t.Fatalf("rewritten argv %q dropped or reordered caller tokens %q", out, args)
		}
	}
}

func TestStripInheritedStateLabelsGlobalFlagsAndNewAlias(t *testing.T) {
	for _, args := range [][]string{
		{"--actor", "woodhouse", "create", "t", "--parent", "qc-1"},
		{"--json", "new", "t", "--parent=qc-1"},
	} {
		out, stderr, _ := runStripInheritedStateLabels(t, args, specimenParentLabels, nil)
		got := bdCreateChildLabelsOracle(t, out, specimenParentLabels)
		if want := sortedLabels("town:x", "ready:y"); !slices.Equal(got, want) {
			t.Errorf("argv %q: child labels = %q, want %q (rewritten %q)", args, got, want, out)
		}
		if !strings.Contains(stderr, "needs-summon") {
			t.Errorf("argv %q: stderr = %q, want strip log", args, stderr)
		}
	}
}

func TestStripInheritedStateLabelsFailsOpenLoudlyWhenParentUnreadable(t *testing.T) {
	args := []string{"create", "t", "--parent", "qc-1"}
	out, stderr, _ := runStripInheritedStateLabels(t, args, nil, errors.New("store unavailable"))
	if !slices.Equal(out, args) {
		t.Fatalf("argv = %q, want untouched on read failure", out)
	}
	if !strings.Contains(stderr, "store unavailable") || !strings.Contains(stderr, "qc-p9m8oa9") {
		t.Fatalf("stderr = %q, want a warning naming the read failure and qc-p9m8oa9", stderr)
	}
}

func TestStripInheritedStateLabelsRoundTripsKeptLabelsThroughBdCSVParsing(t *testing.T) {
	// A kept label bd's comma-splitting would otherwise break apart.
	parent := []string{`note:a,b`, `quote:"x"`, "hold:cert-wait"}
	args := []string{"create", "t", "--parent", "qc-1"}
	out, _, _ := runStripInheritedStateLabels(t, args, parent, nil)
	got := bdCreateChildLabelsOracle(t, out, parent)
	if want := sortedLabels(`note:a,b`, `quote:"x"`); !slices.Equal(got, want) {
		t.Fatalf("child labels = %q, want %q (argv %q)", got, want, out)
	}
}

func TestStripInheritedStateLabelsRefusesToRewriteAroundAnUnrepresentableLabel(t *testing.T) {
	// bd trims explicit labels; a stored label with edge whitespace cannot be
	// re-passed faithfully, so the rewrite must not silently change it.
	parent := []string{" padded ", "hold:cert-wait"}
	args := []string{"create", "t", "--parent", "qc-1"}
	out, stderr, _ := runStripInheritedStateLabels(t, args, parent, nil)
	if !slices.Equal(out, args) {
		t.Fatalf("argv = %q, want untouched", out)
	}
	if !strings.Contains(stderr, "qc-p9m8oa9") {
		t.Fatalf("stderr = %q, want a warning that the guard was skipped", stderr)
	}
}

// isSubsequence reports whether every element of want appears in got in order.
func isSubsequence(want, got []string) bool {
	j := 0
	for _, g := range got {
		if j < len(want) && g == want[j] {
			j++
		}
	}
	return j == len(want)
}

// TestGcBdCreateDoesNotInheritParentStateLabels drives the real passthrough:
// doBd resolves the scope, reads the parent through the stubbed store seam, and
// hands the argv to a fake bd that records it. The recorded argv is then judged
// by the pflag oracle, so the assertion is about what bd would create.
func TestGcBdCreateDoesNotInheritParentStateLabels(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	silentFallbackTestSetup(t, `#!/bin/sh
: > "$GC_TEST_BD_ARGV"
for a in "$@"; do printf '%s\n' "$a" >> "$GC_TEST_BD_ARGV"; done
echo '{"id":"demo-parent.1"}'
`)
	t.Setenv("GC_TEST_BD_ARGV", argvFile)

	prev := bdCreateParentLabels
	var reads []string
	bdCreateParentLabels = func(_ string, _ *config.City, _ execStoreTarget, parentID string) ([]string, error) {
		reads = append(reads, parentID)
		return specimenParentLabels, nil
	}
	t.Cleanup(func() { bdCreateParentLabels = prev })

	recorded := func(t *testing.T) []string {
		t.Helper()
		data, err := os.ReadFile(argvFile)
		if err != nil {
			t.Fatalf("fake bd never ran: %v", err)
		}
		return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}

	t.Run("parent create strips state labels and logs", func(t *testing.T) {
		reads = nil
		var stdout, stderr bytes.Buffer
		if code := doBd([]string{"create", "Follow-up", "--parent", "demo-parent"}, &stdout, &stderr); code != 0 {
			t.Fatalf("doBd = %d, stderr=%q", code, stderr.String())
		}
		got := bdCreateChildLabelsOracle(t, recorded(t), specimenParentLabels)
		if want := sortedLabels("town:x", "ready:y"); !slices.Equal(got, want) {
			t.Fatalf("child labels = %q, want %q (bd argv %q)", got, want, recorded(t))
		}
		if !strings.Contains(stderr.String(), "hold:cert-wait") || !strings.Contains(stderr.String(), "qc-p9m8oa9") {
			t.Fatalf("stderr = %q, want the strip log line", stderr.String())
		}
		if !slices.Equal(reads, []string{"demo-parent"}) {
			t.Fatalf("parent reads = %q, want [demo-parent]", reads)
		}
	})

	t.Run("explicit state label survives end to end", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := doBd([]string{"create", "Follow-up", "--parent", "demo-parent", "-l", "hold:cert-wait"}, &stdout, &stderr); code != 0 {
			t.Fatalf("doBd = %d, stderr=%q", code, stderr.String())
		}
		got := bdCreateChildLabelsOracle(t, recorded(t), specimenParentLabels)
		if want := sortedLabels("town:x", "ready:y", "hold:cert-wait"); !slices.Equal(got, want) {
			t.Fatalf("child labels = %q, want %q (bd argv %q)", got, want, recorded(t))
		}
	})

	t.Run("non-parent create is forwarded verbatim with no store read", func(t *testing.T) {
		reads = nil
		args := []string{"create", "Standalone", "-l", "hold:cert-wait"}
		var stdout, stderr bytes.Buffer
		if code := doBd(args, &stdout, &stderr); code != 0 {
			t.Fatalf("doBd = %d, stderr=%q", code, stderr.String())
		}
		if got := recorded(t); !slices.Equal(got, args) {
			t.Fatalf("bd argv = %q, want verbatim %q", got, args)
		}
		if len(reads) != 0 || strings.Contains(stderr.String(), "qc-p9m8oa9") {
			t.Fatalf("reads = %q stderr = %q, want no read and no log", reads, stderr.String())
		}
	})

	t.Run("a parent read past the deadline forwards the argv unchanged with a warning", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		bdCreateParentLabels = func(_ string, _ *config.City, _ execStoreTarget, _ string) ([]string, error) {
			<-release
			return specimenParentLabels, nil
		}
		prevTimeout := bdCreateParentLabelsTimeout
		bdCreateParentLabelsTimeout = 50 * time.Millisecond
		t.Cleanup(func() { bdCreateParentLabelsTimeout = prevTimeout })

		args := []string{"create", "Follow-up", "--parent", "demo-parent"}
		var stdout, stderr bytes.Buffer
		start := time.Now()
		if code := doBd(args, &stdout, &stderr); code != 0 {
			t.Fatalf("doBd = %d, stderr=%q", code, stderr.String())
		}
		if got := recorded(t); !slices.Equal(got, args) {
			t.Fatalf("bd argv = %q, want verbatim %q after the read timed out", got, args)
		}
		if !strings.Contains(stderr.String(), "timed out") || !strings.Contains(stderr.String(), "qc-p9m8oa9") {
			t.Fatalf("stderr = %q, want a timeout warning citing qc-p9m8oa9", stderr.String())
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("doBd took %s; the parent read must not hold the create past its deadline", elapsed)
		}
	})
}

func TestStripInheritedStateLabelsPassesThroughWhenAnotherStoreIsNamed(t *testing.T) {
	// bd resolves --parent in the store these flags select; gc's scope store
	// may hold a stale copy of the same id, whose labels would be wrong.
	for _, tc := range []struct {
		flag string
		args []string
	}{
		{"--repo", []string{"create", "t", "--parent", "qc-1", "--repo", "/other"}},
		{"--repo", []string{"create", "t", "--parent", "qc-1", "--repo=/other"}},
		{"--database", []string{"--database", "otherdb", "create", "t", "--parent", "qc-1"}},
		{"--database", []string{"--database=otherdb", "create", "t", "--parent", "qc-1"}},
		{"--database", []string{"create", "t", "--parent", "qc-1", "--database", "otherdb"}},
		{"--database", []string{"create", "t", "--database=otherdb", "--parent", "qc-1"}},
		{"--db", []string{"--db", "/x/.beads/beads.db", "create", "t", "--parent", "qc-1"}},
		{"--db", []string{"--db=/x/.beads/beads.db", "create", "t", "--parent", "qc-1"}},
		{"--db", []string{"create", "t", "--parent", "qc-1", "--db", "/x/.beads/beads.db"}},
		{"--db", []string{"create", "--db=/x/.beads/beads.db", "t", "--parent", "qc-1"}},
		{"--global", []string{"--global", "create", "t", "--parent", "qc-1"}},
		{"--global", []string{"--global=true", "create", "t", "--parent", "qc-1"}},
		{"--global", []string{"create", "t", "--parent", "qc-1", "--global"}},
		{"--global", []string{"create", "t", "--global=1", "--parent", "qc-1"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			out, stderr, calls := runStripInheritedStateLabels(t, tc.args, specimenParentLabels, nil)
			if !slices.Equal(out, tc.args) {
				t.Fatalf("argv rewritten to %q, want untouched %q", out, tc.args)
			}
			if len(calls) != 0 {
				t.Fatalf("read parent labels %q from gc's scope store, which is not the store %s selects", calls, tc.flag)
			}
			if !strings.Contains(stderr, tc.flag) || !strings.Contains(stderr, "qc-p9m8oa9") {
				t.Fatalf("stderr = %q, want a note naming %s and qc-p9m8oa9", stderr, tc.flag)
			}
		})
	}
	// Control: a global flag that does NOT move the store leaves the guard on.
	for _, args := range [][]string{
		{"--global=false", "create", "t", "--parent", "qc-1"},
		{"--db=", "create", "t", "--parent", "qc-1"},
	} {
		out, _, calls := runStripInheritedStateLabels(t, args, specimenParentLabels, nil)
		if len(calls) != 1 {
			t.Fatalf("argv %q: reads = %q, want the guard to run", args, calls)
		}
		if got := bdCreateChildLabelsOracle(t, out, specimenParentLabels); !slices.Equal(got, sortedLabels("town:x", "ready:y")) {
			t.Fatalf("argv %q: child labels = %q, want the strip", args, got)
		}
	}
}

func TestStripInheritedStateLabelsParsesBdHiddenDescriptionAliases(t *testing.T) {
	for _, args := range [][]string{
		{"create", "t", "-m", "desc", "--parent", "qc-1"},
		{"create", "t", "-mdesc", "--parent", "qc-1"},
		{"create", "t", "--message", "desc", "--parent", "qc-1"},
		{"create", "t", "--body", "desc", "--parent", "qc-1"},
		{"create", "t", "--description-file", "d.md", "--parent", "qc-1"},
	} {
		out, stderr, calls := runStripInheritedStateLabels(t, args, specimenParentLabels, nil)
		if !slices.Equal(calls, []string{"qc-1"}) {
			t.Errorf("argv %q: reads = %q, want [qc-1] (stderr %q)", args, calls, stderr)
			continue
		}
		if got := bdCreateChildLabelsOracle(t, out, specimenParentLabels); !slices.Equal(got, sortedLabels("town:x", "ready:y")) {
			t.Errorf("argv %q: child labels = %q, want the strip (argv %q)", args, got, out)
		}
	}
	// A value that looks like --parent belongs to the alias, as it does in bd.
	for _, args := range [][]string{
		{"create", "t", "--body", "--parent=qc-9"},
		{"create", "t", "--message", "--parent", "qc-9"},
		{"create", "t", "--description-file", "--parent=qc-9"},
	} {
		out, stderr, calls := runStripInheritedStateLabels(t, args, specimenParentLabels, nil)
		if !slices.Equal(out, args) || len(calls) != 0 || stderr != "" {
			t.Errorf("argv %q: out=%q reads=%q stderr=%q, want untouched, no read, silent", args, out, calls, stderr)
		}
	}
}

func TestStripInheritedStateLabelsWarnsWhenItCannotParseAParentCreate(t *testing.T) {
	for _, args := range [][]string{
		{"create", "t", "--parent", "qc-1", "-Z"},
		{"create", "t", "-Z", "--parent", "qc-1"},
		{"create", "t", "--parent", "qc-1", "-d"},
		{"create", "t", "--parent=qc-1", "-l", `"unterminated`},
	} {
		out, stderr, calls := runStripInheritedStateLabels(t, args, specimenParentLabels, nil)
		if !slices.Equal(out, args) || len(calls) != 0 {
			t.Errorf("argv %q: out=%q reads=%q, want untouched and no read", args, out, calls)
		}
		if !strings.Contains(stderr, "did not run") || !strings.Contains(stderr, "qc-p9m8oa9") {
			t.Errorf("argv %q: stderr = %q, want the skip warning", args, stderr)
		}
	}
	// No parent anywhere: nothing would be inherited, so nothing to say.
	out, stderr, _ := runStripInheritedStateLabels(t, []string{"create", "t", "-Z"}, specimenParentLabels, nil)
	if stderr != "" || len(out) != 3 {
		t.Errorf("parentless unparseable create: out=%q stderr=%q, want untouched and silent", out, stderr)
	}
}

func TestStripInheritedStateLabelsQuietSuppressesOnlyTheStripLine(t *testing.T) {
	for _, args := range [][]string{
		{"create", "t", "--parent", "qc-1", "-q"},
		{"create", "t", "--parent", "qc-1", "--quiet"},
		{"-q", "create", "t", "--parent", "qc-1"},
		{"--quiet=true", "create", "t", "--parent", "qc-1"},
		{"create", "t", "-ql", "extra", "--parent", "qc-1"},
	} {
		out, stderr, _ := runStripInheritedStateLabels(t, args, specimenParentLabels, nil)
		if got := bdCreateChildLabelsOracle(t, out, specimenParentLabels); slices.Contains(got, "hold:cert-wait") {
			t.Errorf("argv %q: quiet must not disable the strip; child labels = %q", args, got)
		}
		if stderr != "" {
			t.Errorf("argv %q: stderr = %q, want the strip line suppressed under quiet", args, stderr)
		}
	}
	_, stderr, _ := runStripInheritedStateLabels(t, []string{"create", "t", "--parent", "qc-1", "--quiet=false"}, specimenParentLabels, nil)
	if !strings.Contains(stderr, "needs-summon") {
		t.Errorf("--quiet=false: stderr = %q, want the strip line", stderr)
	}
	// Warnings are not chatter: they still print under -q.
	_, stderr, _ = runStripInheritedStateLabels(t, []string{"create", "t", "--parent", "qc-1", "-q"}, nil, errors.New("store unavailable"))
	if !strings.Contains(stderr, "store unavailable") {
		t.Errorf("-q with a failed read: stderr = %q, want the warning", stderr)
	}
}

func TestBdCreateParentLabelsWithDeadlineTimesOut(t *testing.T) {
	release := make(chan struct{})
	prev := bdCreateParentLabels
	bdCreateParentLabels = func(_ string, _ *config.City, _ execStoreTarget, _ string) ([]string, error) {
		<-release
		return specimenParentLabels, nil
	}
	prevTimeout := bdCreateParentLabelsTimeout
	bdCreateParentLabelsTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		close(release)
		bdCreateParentLabels = prev
		bdCreateParentLabelsTimeout = prevTimeout
	})

	labels, err := bdCreateParentLabelsWithDeadline("", nil, execStoreTarget{}, "qc-1")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("labels=%q err=%v, want a timeout error", labels, err)
	}

	// Control: a prompt read returns its labels.
	bdCreateParentLabels = func(_ string, _ *config.City, _ execStoreTarget, _ string) ([]string, error) {
		return specimenParentLabels, nil
	}
	labels, err = bdCreateParentLabelsWithDeadline("", nil, execStoreTarget{}, "qc-1")
	if err != nil || !slices.Equal(labels, specimenParentLabels) {
		t.Fatalf("labels=%q err=%v, want the parent labels", labels, err)
	}
}
