package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func writeCensusLedger(t *testing.T, scopePath, toml string) {
	t.Helper()
	dir := filepath.Join(scopePath, "test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir test dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test-resources.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
}

func TestCensusOwnerLivenessCheckSkipsScopeWithoutLedgerFile(t *testing.T) {
	cityDir := t.TempDir()
	result := newCensusOwnerLivenessCheck(nil, cityDir, func(string) (beads.Store, error) {
		t.Fatal("newStore should not be called when no ledger file exists")
		return nil, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	if len(result.Details) != 0 {
		t.Fatalf("details = %v, want empty", result.Details)
	}
}

func TestCensusOwnerLivenessCheckOKWhenAllOwnerBeadsAlive(t *testing.T) {
	cityDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-alive-1"

[[debt]]
scope = "untagged"
resource = "fixed_sleep"
owner_bead = "ga-alive-2"
`)
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "ga-alive-1", Title: "alive one"},
		{ID: "ga-alive-2", Title: "alive two"},
	}, nil)
	result := newCensusOwnerLivenessCheck(censusTestCity(), cityDir, func(string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK; message=%q details=%v", result.Status, result.Message, result.Details)
	}
}

func TestCensusOwnerLivenessCheckWarnsOnDanglingOwnerBead(t *testing.T) {
	cityDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-missing-1"
`)
	store := beads.NewMemStoreFrom(0, nil, nil)
	result := newCensusOwnerLivenessCheck(censusTestCity(), cityDir, func(string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "dangling owner_bead=ga-missing-1") {
		t.Fatalf("details missing dangling owner_bead marker:\n%s", details)
	}
	if !strings.Contains(result.FixHint, "council review") {
		t.Fatalf("fix hint = %q, want mention of council review", result.FixHint)
	}
}

func TestCensusOwnerLivenessCheckDedupesRepeatedOwnerBeadAcrossRows(t *testing.T) {
	cityDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-missing-shared"

[[debt]]
scope = "untagged"
resource = "subprocess"
owner_bead = "ga-missing-shared"

[[medium]]
package_dir = "cmd/gc"
package_name = "main"
owner = "TestFoo"
resources = ["subprocess"]
owner_bead = "ga-missing-shared"

[[small_debt]]
scope = "all"
resource = "fixed_sleep"
owner_bead = "ga-missing-shared"
`)
	inner := beads.NewMemStoreFrom(0, nil, nil)
	spy := &censusGetCountingStore{Store: inner, counts: map[string]int{}}
	result := newCensusOwnerLivenessCheck(censusTestCity(), cityDir, func(string) (beads.Store, error) {
		return spy, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	if got := spy.counts["ga-missing-shared"]; got != 1 {
		t.Fatalf("Get(ga-missing-shared) called %d times, want 1 (dedup across rows)", got)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{"audit_baseline:", "debt:", "medium:", "small_debt:"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing row category %q:\n%s", want, details)
		}
	}
	danglingCount := strings.Count(details, "dangling owner_bead=ga-missing-shared")
	if danglingCount != 1 {
		t.Fatalf("dangling owner_bead=ga-missing-shared appeared %d times, want exactly 1 finding line:\n%s", danglingCount, details)
	}
}

func TestCensusOwnerLivenessCheckSkipsOnStoreOpenFailure(t *testing.T) {
	cityDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-whatever"
`)
	result := newCensusOwnerLivenessCheck(censusTestCity(), cityDir, func(string) (beads.Store, error) {
		return nil, errors.New("city offline")
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "skipped: opening bead store: city offline") {
		t.Fatalf("details missing store-open skip marker:\n%s", details)
	}
	if !strings.Contains(details, censusCannotTellMarker) {
		t.Fatalf("details missing %q:\n%s", censusCannotTellMarker, details)
	}
	if strings.Contains(details, "dangling owner_bead") {
		t.Fatalf("store-open failure must not be reported as dangling:\n%s", details)
	}
	if result.FixHint != censusSkipFixHint {
		t.Fatalf("fix hint = %q, want %q", result.FixHint, censusSkipFixHint)
	}
}

func TestCensusOwnerLivenessCheckSkipsOnNonNotFoundGetError(t *testing.T) {
	cityDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-transient"
`)
	store := censusGetErrorStore{err: errors.New("connection reset")}
	result := newCensusOwnerLivenessCheck(censusTestCity(), cityDir, func(string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "skipped: checking owner_bead ga-transient: connection reset") {
		t.Fatalf("details missing get-error skip marker:\n%s", details)
	}
	if strings.Contains(details, "dangling owner_bead") {
		t.Fatalf("non-not-found Get error must not be reported as dangling:\n%s", details)
	}
}

func TestCensusOwnerLivenessCheckScansCityAndRigs(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-city-missing"
`)
	writeCensusLedger(t, rigDir, `
version = 1

[[debt]]
scope = "untagged"
resource = "fixed_sleep"
owner_bead = "ga-rig-missing"
`)
	cfg := censusTestCity(
		config.Rig{Name: "repo", Prefix: "re", Path: rigDir},
		config.Rig{Name: "ghost", Path: ""},
	)
	store := beads.NewMemStoreFrom(0, nil, nil)
	result := newCensusOwnerLivenessCheck(cfg, cityDir, func(string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"city: dangling owner_bead=ga-city-missing",
		"rig repo: dangling owner_bead=ga-rig-missing",
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
}

// censusTestCity returns a city config whose HQ prefix is "ga", so ga-
// owner_beads route to the city store, plus the given rigs.
func censusTestCity(rigs ...config.Rig) *config.City {
	return &config.City{Workspace: config.Workspace{Prefix: "ga"}, Rigs: rigs}
}

// censusStoresByPath returns a newStore func that serves each scope path its
// own store (or open error), counting opens per path. Any other path fails the
// test: the check must only open stores that a prefix routes to.
func censusStoresByPath(t *testing.T, stores map[string]beads.Store, openErrs map[string]error, opens map[string]int) func(string) (beads.Store, error) {
	t.Helper()
	return func(path string) (beads.Store, error) {
		if opens != nil {
			opens[path]++
		}
		if err, ok := openErrs[path]; ok {
			return nil, err
		}
		if store, ok := stores[path]; ok {
			return store, nil
		}
		t.Errorf("newStore(%q) opened a store no prefix routes to", path)
		return nil, errors.New("unexpected store path")
	}
}

// Test (a): a rig ledger's owner_bead is looked up in the store its PREFIX
// routes to, not the scanning rig's store.
func TestCensusOwnerLivenessCheckResolvesOwnerByPrefixRoute(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "pl-rig-alive"
`)
	writeCensusLedger(t, rigDir, `
version = 1

[[debt]]
scope = "untagged"
resource = "fixed_sleep"
owner_bead = "ga-hq-alive"

[[small_debt]]
scope = "all"
resource = "fixed_sleep"
owner_bead = "ga-hq-alive-2"
`)
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "ga-hq-alive", Title: "hq owner"},
		{ID: "ga-hq-alive-2", Title: "hq owner two"},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "pl-rig-alive", Title: "rig owner"}}, nil)
	opens := map[string]int{}
	cfg := censusTestCity(config.Rig{Name: "platform", Prefix: "pl", Path: rigDir})
	result := newCensusOwnerLivenessCheck(cfg, cityDir, censusStoresByPath(t,
		map[string]beads.Store{cityDir: cityStore, rigDir: rigStore}, nil, opens)).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	if len(result.Details) != 0 {
		t.Fatalf("details = %v, want empty when every owner resolves by prefix", result.Details)
	}
	for path, n := range opens {
		if n != 1 {
			t.Errorf("newStore(%q) called %d times, want 1 (one open per routed store per run)", path, n)
		}
	}
}

// Test (b): a nonexistent owner on a rig that declares no foreign namespace
// is dangling, with the re-point hint.
func TestCensusOwnerLivenessCheckUndeclaredRigMissingOwnerIsDangling(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	writeCensusLedger(t, rigDir, `
version = 1

[[debt]]
scope = "untagged"
resource = "fixed_sleep"
owner_bead = "ga-gone"
`)
	cfg := censusTestCity(config.Rig{Name: "platform", Prefix: "pl", Path: rigDir})
	result := newCensusOwnerLivenessCheck(cfg, cityDir, censusStoresByPath(t,
		map[string]beads.Store{cityDir: beads.NewMemStoreFrom(0, nil, nil), rigDir: beads.NewMemStoreFrom(0, nil, nil)}, nil, nil)).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "rig platform: dangling owner_bead=ga-gone") {
		t.Fatalf("details missing dangling finding:\n%s", details)
	}
	if strings.Contains(details, "not observable in this city") {
		t.Fatalf("an undeclared rig must never report an owner as foreign:\n%s", details)
	}
	if !strings.Contains(result.FixHint, "council review") {
		t.Fatalf("fix hint = %q, want the re-point hint", result.FixHint)
	}
}

// Test (c): a nonexistent owner on a rig that DECLARES a foreign owner
// namespace is reported as foreign in Details, is not a finding, and gets no
// re-point hint. A found owner on the same rig is still checked.
func TestCensusOwnerLivenessCheckDeclaredRigMissingOwnerIsForeign(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	writeCensusLedger(t, rigDir, `
version = 1

[[debt]]
scope = "untagged"
resource = "fixed_sleep"
owner_bead = "ga-cp3hwi"

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-ours"
`)
	cfg := censusTestCity(config.Rig{
		Name:   "platform",
		Prefix: "pl",
		Path:   rigDir,
		Doctor: config.RigDoctorConfig{CensusOwnerNamespace: "gastownhall/gascity"},
	})
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "ga-ours", Title: "ours"}}, nil)
	result := newCensusOwnerLivenessCheck(cfg, cityDir, censusStoresByPath(t,
		map[string]beads.Store{cityDir: cityStore, rigDir: beads.NewMemStoreFrom(0, nil, nil)}, nil, nil)).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK (a foreign owner is never a finding); message=%q details=%v", result.Status, result.Message, result.Details)
	}
	if strings.Contains(result.FixHint, "council review") {
		t.Fatalf("fix hint = %q, must not carry the re-point hint for a foreign owner", result.FixHint)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"rig platform: owner_bead=ga-cp3hwi owned in gastownhall/gascity, not observable in this city",
		"debt: scope=untagged resource=fixed_sleep",
		"not liveness-checked",
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	if strings.Contains(details, "dangling") || strings.Contains(details, "ga-ours") {
		t.Fatalf("details must report only the foreign owner:\n%s", details)
	}
}

// Test (d): an owner whose prefix routes to an UNREACHABLE store, or to no
// store at all, is a skipped line: never clean, never dangling, and never
// foreign even on a declared rig.
func TestCensusOwnerLivenessCheckUnreachableRouteIsSkipped(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	offlineDir := t.TempDir()
	writeCensusLedger(t, rigDir, `
version = 1

[[debt]]
scope = "untagged"
resource = "fixed_sleep"
owner_bead = "zz-offline"

[[debt]]
scope = "untagged"
resource = "subprocess"
owner_bead = "xx-unrouted"

[[debt]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-flaky"
`)
	for _, declared := range []string{"", "gastownhall/gascity"} {
		name := "undeclared"
		if declared != "" {
			name = "declared"
		}
		t.Run(name, func(t *testing.T) {
			cfg := censusTestCity(
				config.Rig{Name: "platform", Prefix: "pl", Path: rigDir, Doctor: config.RigDoctorConfig{CensusOwnerNamespace: declared}},
				config.Rig{Name: "offline", Prefix: "zz", Path: offlineDir},
			)
			result := newCensusOwnerLivenessCheck(cfg, cityDir, censusStoresByPath(t,
				map[string]beads.Store{cityDir: censusGetErrorStore{err: errors.New("connection reset")}},
				map[string]error{offlineDir: errors.New("dolt unreachable")}, nil)).Run(&doctor.CheckContext{})

			if result.Status != doctor.StatusWarning {
				t.Fatalf("status = %v, want StatusWarning; message=%q details=%v", result.Status, result.Message, result.Details)
			}
			details := strings.Join(result.Details, "\n")
			for _, want := range []string{
				"rig platform skipped: opening bead store: dolt unreachable (owner_bead zz-offline",
				`rig platform skipped: owner_bead xx-unrouted: prefix "xx" routes to no bead store in this city`,
				"rig platform skipped: checking owner_bead ga-flaky: connection reset",
			} {
				if !strings.Contains(details, want) {
					t.Fatalf("details missing %q:\n%s", want, details)
				}
			}
			if got := strings.Count(details, censusCannotTellMarker); got != 3 {
				t.Fatalf("%q appears %d times, want once per skipped owner:\n%s", censusCannotTellMarker, got, details)
			}
			if strings.Contains(details, "dangling owner_bead") || strings.Contains(details, "not observable in this city") {
				t.Fatalf("an unresolvable owner must be neither dangling nor foreign:\n%s", details)
			}
			if result.FixHint != censusSkipFixHint {
				t.Fatalf("fix hint = %q, want %q", result.FixHint, censusSkipFixHint)
			}
		})
	}
}

// Test (e): the city scope is never declared, so a missing owner in the city
// ledger stays dangling even when a rig declares a foreign namespace.
func TestCensusOwnerLivenessCheckCityScopeIgnoresRigDeclaration(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	writeCensusLedger(t, cityDir, `
version = 1

[[audit_baseline]]
scope = "all"
resource = "subprocess"
owner_bead = "ga-city-gone"

[[debt]]
scope = "untagged"
resource = "fixed_sleep"
owner_bead = "ga-city-alive"
`)
	cfg := censusTestCity(config.Rig{
		Name:   "platform",
		Prefix: "pl",
		Path:   rigDir,
		Doctor: config.RigDoctorConfig{CensusOwnerNamespace: "gastownhall/gascity"},
	})
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "ga-city-alive", Title: "alive"}}, nil)
	result := newCensusOwnerLivenessCheck(cfg, cityDir, censusStoresByPath(t,
		map[string]beads.Store{cityDir: cityStore}, nil, nil)).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "city: dangling owner_bead=ga-city-gone") {
		t.Fatalf("details missing city dangling finding:\n%s", details)
	}
	if strings.Contains(details, "not observable in this city") || strings.Contains(details, "ga-city-alive") {
		t.Fatalf("city scope must classify exactly as before:\n%s", details)
	}
	if !strings.Contains(result.FixHint, "council review") {
		t.Fatalf("fix hint = %q, want the re-point hint", result.FixHint)
	}
}

type censusGetCountingStore struct {
	beads.Store
	counts map[string]int
}

func (s *censusGetCountingStore) Get(id string) (beads.Bead, error) {
	s.counts[id]++
	return s.Store.Get(id)
}

type censusGetErrorStore struct {
	beads.Store
	err error
}

func (s censusGetErrorStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, s.err
}
