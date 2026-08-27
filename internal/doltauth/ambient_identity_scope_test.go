package doltauth

import "testing"

// The regression these guard is ga-3qvmjj: after the 2026-08-27 qcore hub flip,
// an agent session projected for the hub could not open the LOCAL city store,
// because the endpoint resolved per store while the credential resolved per
// process. hq is the coordination plane, so the agent lost mail, ga- beads, its
// hook queue and its work queue at once.

func TestResolveUser_HubIdentityDoesNotLeakOntoLocalStore(t *testing.T) {
	// Exactly the live shape read off agent qcore/archer's pane.
	t.Setenv("GC_DOLT_HOST", "100.71.23.94")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "cherub")

	got := resolveUser("", "127.0.0.1", 51361)
	if got == "cherub" {
		t.Fatal("hub identity was applied to the local managed store — this is the 1045 that took hq down for flip-primed sessions")
	}
	if got != "" {
		t.Fatalf("resolveUser = %q, want the fallback (empty), not the mismatched ambient override", got)
	}
}

func TestResolveUser_AppliesToItsOwnEndpoint(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "100.71.23.94")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "cherub")

	if got := resolveUser("", "100.71.23.94", 3307); got != "cherub" {
		t.Fatalf("resolveUser = %q, want cherub — the override must still apply to the endpoint it was set for", got)
	}
}

// The operator override is documented behaviour and must survive: doltauth
// reads GC_DOLT_USER via os.Getenv precisely so an operator can force it.
func TestResolveUser_BareOverrideStillAppliesEverywhere(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "operator")

	for _, tc := range []struct {
		host string
		port int
	}{{"127.0.0.1", 51361}, {"100.71.23.94", 3307}, {"", 0}} {
		if got := resolveUser("fallback", tc.host, tc.port); got != "operator" {
			t.Fatalf("resolveUser(%q,%d) = %q, want operator — a bare override names no endpoint, so nothing contradicts it", tc.host, tc.port, got)
		}
	}
}

// Narrow only where the mismatch is provable: an unknown target endpoint is not
// evidence of a mismatch.
func TestResolveUser_UnknownTargetKeepsOverride(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "100.71.23.94")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "cherub")

	if got := resolveUser("fallback", "", 0); got != "cherub" {
		t.Fatalf("resolveUser = %q, want cherub — an unknown target cannot contradict the override", got)
	}
}

func TestResolveUser_NoOverrideUsesFallback(t *testing.T) {
	t.Setenv("GC_DOLT_USER", "")
	if got := resolveUser("target-user", "127.0.0.1", 51361); got != "target-user" {
		t.Fatalf("resolveUser = %q, want target-user", got)
	}
}

// localhost and 127.0.0.1 are the same server and both spellings appear in the
// config surface; treating them as different would decline a valid override.
func TestResolveUser_LocalhostAndLoopbackAreTheSameEndpoint(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "localhost")
	t.Setenv("GC_DOLT_PORT", "51361")
	t.Setenv("GC_DOLT_USER", "operator")

	if got := resolveUser("", "127.0.0.1", 51361); got != "operator" {
		t.Fatalf("resolveUser = %q, want operator — localhost and 127.0.0.1 are one endpoint", got)
	}
}

// A port-only mismatch is enough: same host, different server.
func TestResolveUser_PortOnlyMismatchDeclines(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "127.0.0.1")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "cherub")

	if got := resolveUser("fallback", "127.0.0.1", 51361); got != "fallback" {
		t.Fatalf("resolveUser = %q, want fallback — same host but a different server is still a different endpoint", got)
	}
}

// --- the password half (reopened ga-3qvmjj) ---------------------------------
//
// The first fix guarded the ambient USER only. Lana reproduced against it with
// a real crew env: the ambient PASSWORD still rode through to the local server,
// and because a password present forces the credentialed path, the connection
// fell back to root and failed as "Access denied for user 'root'" — the same
// leak one variable further along, wearing a different error string. These pin
// the FULL crew shape (host+port+user+password), which is what agents actually
// carry; testing host/port/user alone is what let it through the first time.

// crewEnv is lana's live pane shape: hub endpoint, hub identity, both password
// spellings.
func crewEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GC_DOLT_HOST", "100.71.23.94")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "cherub")
	t.Setenv("GC_DOLT_PASSWORD", "hub-secret")
	t.Setenv("BEADS_DOLT_PASSWORD", "hub-secret")
}

func TestResolve_CrewEnvLeaksNeitherUserNorPasswordOntoLocalStore(t *testing.T) {
	crewEnv(t)
	got := Resolve(t.TempDir(), "", "127.0.0.1", 51361)
	if got.User == "cherub" {
		t.Error("ambient hub USER leaked onto the local store")
	}
	if got.Password != "" {
		t.Errorf("ambient hub PASSWORD leaked onto the local store (%d bytes) — root has no password, so supplying one is what breaks the connection", len(got.Password))
	}
}

func TestResolve_CrewEnvStillAuthenticatesToItsOwnEndpoint(t *testing.T) {
	crewEnv(t)
	got := Resolve(t.TempDir(), "", "100.71.23.94", 3307)
	if got.User != "cherub" {
		t.Errorf("User = %q, want cherub — the hub identity must still reach the hub", got.User)
	}
	if got.Password != "hub-secret" {
		t.Error("Password was declined for the endpoint it belongs to — this would break qcore")
	}
}

// A bare password override with no ambient endpoint is an operator override and
// must survive, exactly as the bare user override does.
func TestResolve_BarePasswordOverrideStillApplies(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "operator-secret")

	if got := Resolve(t.TempDir(), "", "127.0.0.1", 51361); got.Password != "operator-secret" {
		t.Errorf("Password = %q, want operator-secret — a bare override names no endpoint, so nothing contradicts it", got.Password)
	}
}

// ResolveScopedFromEnv is the path gc's own store opens use.
func TestResolveScopedFromEnv_CrewEnvDeclinesBothOnMismatch(t *testing.T) {
	crewEnv(t)
	got := ResolveScopedFromEnv(t.TempDir(), "", map[string]string{
		"GC_DOLT_HOST": "127.0.0.1",
		"GC_DOLT_PORT": "51361",
	})
	if got.User == "cherub" {
		t.Error("scoped resolution leaked the ambient hub user onto the local store")
	}
	if got.Password != "" {
		t.Error("scoped resolution leaked the ambient hub password onto the local store")
	}
}
