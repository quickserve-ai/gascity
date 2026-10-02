package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

// reply --from accepts what send's --from accepts: a local-city qualifier
// strips to the bare form, and an identity this city cannot resolve is
// refused before anything is written.
func TestCmdMailReply_ExplicitFromResolvesLikeSend(t *testing.T) {
	cityPath := writeCrossCityTestCity(t)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	orig, err := beadmail.New(store).Send("x", "human", "status", "local thread")
	if err != nil {
		t.Fatalf("seed Send: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdMailReplyFromJSON([]string{orig.ID}, "qlandia/nobody", "", "refused", false, false, &stdout, &stderr); code == 0 {
		t.Fatalf("reply --from qlandia/nobody = 0, want invalid sender; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), `gc mail reply: invalid sender "nobody"`) {
		t.Fatalf("stderr = %q, want the invalid-sender refusal", stderr.String())
	}
	if n := countMessageBeads(t, cityPath); n != 1 {
		t.Fatalf("message beads after refused reply = %d, want 1 (the original only)", n)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdMailReplyFromJSON([]string{orig.ID}, "qlandia/human", "", "operator reply", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("reply --from qlandia/human = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if n := countMessageBeads(t, cityPath); n != 2 {
		t.Fatalf("message beads after reply = %d, want 2", n)
	}
	after, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt after reply: %v", err)
	}
	all, err := after.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List messages: %v", err)
	}
	for _, b := range all {
		if b.ID != orig.ID && b.From != "human" {
			t.Fatalf("reply From = %q, want the local qualifier stripped to human", b.From)
		}
	}
}

// gc handoff --target --from human hands off as the operator even from a
// seat; a self-handoff mails as the session, so --from is refused there.
func TestCmdHandoff_FromNamesTargetSender(t *testing.T) {
	clearMailIdentityEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_MAIL", "")
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	for _, alias := range []string{"sender", "recipient"} {
		if _, err := store.Create(beads.Bead{
			Type:     session.BeadType,
			Labels:   []string{session.LabelSession},
			Metadata: map[string]string{"alias": alias, "session_name": alias + "-gc-42"},
		}); err != nil {
			t.Fatalf("Create %s: %v", alias, err)
		}
	}
	t.Setenv("GC_ALIAS", "sender")

	var stdout, stderr bytes.Buffer
	if code := cmdHandoffRemoteFrom([]string{"context cycle"}, "recipient", "human", false, &stdout, &stderr); code != 0 {
		t.Fatalf("handoff --target --from human = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if stored := mailSendTestFindMessage(t, cityPath); stored.From != "human" {
		t.Fatalf("From = %q, want human", stored.From)
	}

	stderr.Reset()
	if code := cmdHandoffWithFrom([]string{"context cycle"}, "", "human", false, "", false, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "--from needs --target") {
		t.Fatalf("self-handoff --from = %d, stderr=%q; want the --target refusal", code, stderr.String())
	}
}
