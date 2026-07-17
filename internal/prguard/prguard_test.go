package prguard

import (
	"context"
	"errors"
	"testing"
	"time"
)

func fakeRunner(out string, err error) Runner {
	return func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte(out), err
	}
}

func TestMetadataPRURL(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want string
	}{
		{"nil metadata", nil, ""},
		{"no keys", map[string]string{"merge_strategy": "pr"}, ""},
		{"bare pr_url", map[string]string{"pr_url": "https://github.com/o/r/pull/1"}, "https://github.com/o/r/pull/1"},
		{"gc.pr_url fallback", map[string]string{"gc.pr_url": "https://github.com/o/r/pull/2"}, "https://github.com/o/r/pull/2"},
		{"pr_url precedence", map[string]string{"pr_url": "https://github.com/o/r/pull/1", "gc.pr_url": "https://github.com/o/r/pull/2"}, "https://github.com/o/r/pull/1"},
		{"whitespace only", map[string]string{"pr_url": "   "}, ""},
	}
	for _, tc := range cases {
		if got := MetadataPRURL(tc.meta); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestCheckBeadClosableNoPRAllows(t *testing.T) {
	// No runner needed: the guard must not touch gh when no PR URL is
	// recorded (dispatch closes wisps/control beads constantly).
	panicRunner := func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("runner invoked for bead without pr_url")
		return nil, nil
	}
	res := CheckBeadClosable(map[string]string{"outcome": "pass"}, panicRunner, time.Second)
	if res.Verdict != Allow {
		t.Fatalf("verdict = %v, want Allow", res.Verdict)
	}
}

func TestCheckPRURL(t *testing.T) {
	const url = "https://github.com/quickserve-ai/q-core/pull/2493"
	cases := []struct {
		name      string
		url       string
		runner    Runner
		want      Verdict
		wantState string
		wantNote  bool
	}{
		{"merged allows", url, fakeRunner(`{"state":"closed","merged":true}`, nil), Allow, "MERGED", false},
		{"open blocks", url, fakeRunner(`{"state":"open","merged":false}`, nil), Block, "OPEN", false},
		{"closed unmerged blocks", url, fakeRunner(`{"state":"closed","merged":false}`, nil), Block, "CLOSED", false},
		{"network error fails open", url, fakeRunner("", errors.New("dial tcp: timeout")), Allow, "", true},
		{"garbage output fails open", url, fakeRunner("<html>rate limited</html>", nil), Allow, "", true},
		{"empty state fails open", url, fakeRunner(`{"state":"","merged":false}`, nil), Allow, "", true},
		{"non-PR url fails open", "https://example.com/not-a-pr", fakeRunner(`{"state":"open"}`, nil), Allow, "", true},
		{"repo url fails open", "https://github.com/quickserve-ai/q-core", fakeRunner(`{"state":"open"}`, nil), Allow, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := CheckPRURL(tc.url, tc.runner, time.Second)
			if res.Verdict != tc.want {
				t.Fatalf("verdict = %v, want %v (note %q)", res.Verdict, tc.want, res.Note)
			}
			if res.State != tc.wantState {
				t.Errorf("state = %q, want %q", res.State, tc.wantState)
			}
			if tc.wantNote && res.Note == "" {
				t.Error("expected explanatory Note on fail-open result")
			}
			if !tc.wantNote && res.Note != "" {
				t.Errorf("unexpected Note %q", res.Note)
			}
		})
	}
}

func TestEnabledKillSwitch(t *testing.T) {
	t.Setenv("GC_PR_CLOSE_GUARD", "")
	if !Enabled() {
		t.Fatal("guard should default to enabled")
	}
	t.Setenv("GC_PR_CLOSE_GUARD", "off")
	if Enabled() {
		t.Fatal("GC_PR_CLOSE_GUARD=off should disable the guard")
	}
	t.Setenv("GC_PR_CLOSE_GUARD", "OFF")
	if Enabled() {
		t.Fatal("kill switch should be case-insensitive")
	}
}
