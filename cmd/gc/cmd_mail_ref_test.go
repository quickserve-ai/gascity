package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func TestMailWakeRefSelection(t *testing.T) {
	cloud := nudgeTarget{agent: config.Agent{Name: "c", WakeTransport: config.WakeTransportClaudeCloud}}
	local := nudgeTarget{agent: config.Agent{Name: "l"}}
	httpsRef := "https://github.com/org/repo/pull/9"

	if got := mailWakeRef(cloud, httpsRef, "ga-m1"); got != httpsRef {
		t.Errorf("cloud seat with ref: got %q", got)
	}
	if got := mailWakeRef(cloud, "", "ga-m1"); got != "bead://ga-m1" {
		t.Errorf("cloud seat without ref falls back to bead:// (transport refuses loudly): got %q", got)
	}
	if got := mailWakeRef(local, httpsRef, "ga-m1"); got != "bead://ga-m1" {
		t.Errorf("session seat must keep bead:// even when a ref is supplied: got %q", got)
	}
}

func TestCmdMailSendRefusesMalformedRefUpFront(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdMailSendJSONRef([]string{"anyone", "body"}, true, false, "", "", "", "",
		"http://not-https.example.com/x", false, &stdout, &stderr)
	if code == 0 {
		t.Fatal("malformed --ref accepted")
	}
	if !strings.Contains(stderr.String(), "--ref") {
		t.Errorf("refusal must name the flag: %s", stderr.String())
	}
}

// TestCmdMailSendCloudSeatWithoutRefRefusedBeforeBead: a notified recipient
// on the claude-cloud wake transport with no --ref is refused BEFORE any
// mail bead is written (design §5.3 guard rail).
func TestCmdMailSendCloudSeatWithoutRefRefusedBeforeBead(t *testing.T) {
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	// A cloud-wake agent, in the agents/<name>/agent.toml surface.
	agentDir := filepath.Join(cityDir, "agents", "cloudy")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte("name = \"cloudy\"\nwake_transport = \"claude-cloud\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "cloudy",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "cloudy",
			"alias":        "cloudy",
			"agent_name":   "cloudy",
			"template":     "cloudy",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Preflight: the recipient must resolve as a cloud-wake seat, or the
	// guard has nothing to fire on and this test would pass vacuously.
	target, terr := resolveNudgeTarget("cloudy", os.Stderr)
	if terr != nil {
		t.Fatalf("resolveNudgeTarget(cloudy): %v", terr)
	}
	if got := strings.TrimSpace(target.agent.WakeTransport); got != config.WakeTransportClaudeCloud {
		t.Fatalf("test-city agent did not resolve as cloud-wake (WakeTransport=%q, agent=%q)", got, target.agent.Name)
	}

	var stdout, stderr bytes.Buffer
	code := cmdMailSendJSONRef([]string{"cloudy", "body"}, true, false, "human", "", "s", "m",
		"", false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cloud-wake recipient without --ref must refuse; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--ref") || !strings.Contains(stderr.String(), "cloud-wake") {
		t.Errorf("refusal must explain the --ref requirement: %s", stderr.String())
	}
	// The guard fires pre-bead: no message bead may exist.
	msgs, err := store.List(beads.ListQuery{Type: "message"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("mail bead %s was written despite the pre-bead refusal", msgs[0].ID)
	}
}
