package orderdiscovery

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

const disabledPatrolOrder = `[order]
exec = "scripts/patrol.sh"
trigger = "cooldown"
interval = "15m"
enabled = false
`

// packDisabledRigCity is a city whose rigs alpha and beta both import a pack
// that ships the rig-scoped order "patrol" disabled.
func packDisabledRigCity(t *testing.T, overrides ...config.OrderOverride) (string, *config.City) {
	t.Helper()
	cityPath, cityLayer := orderDiscoveryCity(t)
	packDir := filepath.Join(t.TempDir(), "oversight-pack")
	writeOrderDiscoveryFile(t, filepath.Join(packDir, "orders"), "patrol", disabledPatrolOrder)
	return cityPath, &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{
				"alpha": {cityLayer},
				"beta":  {cityLayer},
			},
		},
		RigPackDirs: map[string][]string{
			"alpha": {packDir},
			"beta":  {packDir},
		},
		Orders: config.OrdersConfig{Overrides: overrides},
	}
}

// cityPackWithOrder returns a city-imported pack dir holding one order file.
func cityPackWithOrder(t *testing.T, name, content string) string {
	t.Helper()
	packDir := filepath.Join(t.TempDir(), "city-pack")
	writeOrderDiscoveryFile(t, filepath.Join(packDir, "orders"), name, content)
	return packDir
}

func enabledRigs(aa []orders.Order, name string) []string {
	var rigs []string
	for _, a := range aa {
		if a.Name == name && a.IsEnabled() {
			rigs = append(rigs, a.Rig)
		}
	}
	return rigs
}

func TestScanAllOverrideReEnablesPackDisabledOrder(t *testing.T) {
	on := true
	tests := []struct {
		name     string
		override []config.OrderOverride
		want     []string
	}{
		{name: "no override stays disabled", want: nil},
		{name: "wildcard enables every rig", override: []config.OrderOverride{{Name: "patrol", Rig: orders.RigWildcard, Enabled: &on}}, want: []string{"alpha", "beta"}},
		{name: "rig override enables only that rig", override: []config.OrderOverride{{Name: "patrol", Rig: "beta", Enabled: &on}}, want: []string{"beta"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath, cfg := packDisabledRigCity(t, tt.override...)
			aa, err := ScanAll(cityPath, cfg, ScanOptions{})
			if err != nil {
				t.Fatalf("ScanAll returned error: %v", err)
			}
			if got := enabledRigs(aa, "patrol"); strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("enabled patrol rigs = %v, want %v", got, tt.want)
			}
			if len(aa) != len(tt.want) {
				t.Fatalf("got %d orders, want %d: a disabled instance nobody re-enabled leaked out: %+v", len(aa), len(tt.want), aa)
			}
		})
	}
}

func TestScanAllCityLevelOverrideReEnablesPackDisabledOrder(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	on := true
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{City: []string{cityLayer}},
		PackDirs:      []string{cityPackWithOrder(t, "patrol", disabledPatrolOrder)},
		Orders:        config.OrdersConfig{Overrides: []config.OrderOverride{{Name: "patrol", Enabled: &on}}},
	}

	aa, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	if len(aa) != 1 || aa[0].Name != "patrol" || aa[0].Rig != "" || !aa[0].IsEnabled() {
		t.Fatalf("ScanAll = %+v, want one enabled city-level patrol", aa)
	}
}

// An override that targets a pack-disabled order without enabling it matched
// nothing before, so a pack that started shipping an order disabled turned a
// city's own enabled = false for it into an unmatched-override error.
func TestScanAllOverrideOnPackDisabledOrderIsNotAnError(t *testing.T) {
	off := false
	interval := "5m"
	for _, ov := range []config.OrderOverride{
		{Name: "patrol", Rig: orders.RigWildcard, Enabled: &off},
		{Name: "patrol", Rig: "alpha", Interval: &interval},
	} {
		cityPath, cfg := packDisabledRigCity(t, ov)
		aa, err := ScanAll(cityPath, cfg, ScanOptions{})
		if err != nil {
			t.Fatalf("override %+v: ScanAll returned error: %v", ov, err)
		}
		if len(aa) != 0 {
			t.Fatalf("override %+v: got %+v, want no orders (patrol stays disabled)", ov, aa)
		}
	}
}

func TestScanAllRetainedDisabledOrderIsValidatedOnlyWhenReEnabled(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	// env on a formula order fails validation.
	packDir := cityPackWithOrder(t, "deploy", `[order]
formula = "mol-deploy"
trigger = "manual"
enabled = false

[order.env]
CUSTOM_ORDER_FLAG = "enabled"
`)
	interval := "5m"
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{City: []string{cityLayer}},
		PackDirs:      []string{packDir},
		Orders:        config.OrdersConfig{Overrides: []config.OrderOverride{{Name: "deploy", Interval: &interval}}},
	}

	aa, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error for an invalid order that stays disabled: %v", err)
	}
	if len(aa) != 0 {
		t.Fatalf("got %+v, want no orders", aa)
	}

	on := true
	cfg.Orders.Overrides = []config.OrderOverride{{Name: "deploy", Enabled: &on}}
	if _, err := ScanAll(cityPath, cfg, ScanOptions{}); err == nil || !strings.Contains(err.Error(), "env is supported only for exec orders") {
		t.Fatalf("ScanAll error = %v, want the re-enabled order's validation failure", err)
	}
}

// A disabled order retained for an override must never displace an enabled
// city-scoped order of the same name, which is what discovery registered
// before disabled orders were retained, and must never register beside it.
func TestScanAllRetainedDisabledOrderDoesNotShadowEnabledPromotedOrder(t *testing.T) {
	const enabledSweep = `[order]
scope = "city"
exec = "scripts/pack-sweep.sh"
trigger = "cooldown"
interval = "1h"
`
	const disabledSweep = `[order]
scope = "city"
exec = "scripts/disabled-sweep.sh"
trigger = "cooldown"
interval = "1h"
enabled = false
`
	interval := "5m"
	on := true
	for _, tc := range []struct {
		name string
		// cityLevel, when set, is shipped by a city-imported pack.
		cityLevel string
		// rigPacks are imported by rigs alpha, beta, ... in order.
		rigPacks []string
		override config.OrderOverride
	}{
		{name: "city-level disabled, override tunes", cityLevel: disabledSweep, rigPacks: []string{enabledSweep}, override: config.OrderOverride{Name: "sweep", Interval: &interval}},
		{name: "city-level disabled, override enables", cityLevel: disabledSweep, rigPacks: []string{enabledSweep}, override: config.OrderOverride{Name: "sweep", Enabled: &on, Interval: &interval}},
		{name: "first rig disabled, second enabled", rigPacks: []string{disabledSweep, enabledSweep}, override: config.OrderOverride{Name: "sweep", Interval: &interval}},
		{name: "first rig disabled, second enabled, override enables", rigPacks: []string{disabledSweep, enabledSweep}, override: config.OrderOverride{Name: "sweep", Enabled: &on, Interval: &interval}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cityLayer := orderDiscoveryCity(t)
			cfg := &config.City{
				FormulaLayers: config.FormulaLayers{City: []string{cityLayer}, Rigs: map[string][]string{}},
				RigPackDirs:   map[string][]string{},
				Orders:        config.OrdersConfig{Overrides: []config.OrderOverride{tc.override}},
			}
			if tc.cityLevel != "" {
				cfg.PackDirs = []string{cityPackWithOrder(t, "sweep", tc.cityLevel)}
			}
			for i, content := range tc.rigPacks {
				rig := []string{"alpha", "beta"}[i]
				packDir := filepath.Join(t.TempDir(), rig+"-pack")
				writeOrderDiscoveryFile(t, filepath.Join(packDir, "orders"), "sweep", content)
				cfg.FormulaLayers.Rigs[rig] = []string{cityLayer}
				cfg.RigPackDirs[rig] = []string{packDir}
			}

			aa, err := ScanAll(cityPath, cfg, ScanOptions{})
			if err != nil {
				t.Fatalf("ScanAll returned error: %v", err)
			}
			if len(aa) != 1 {
				t.Fatalf("got %d orders, want exactly one sweep: %+v", len(aa), aa)
			}
			if aa[0].Exec != "scripts/pack-sweep.sh" || !aa[0].IsEnabled() || aa[0].Interval != "5m" {
				t.Fatalf("sweep = %+v, want the enabled pack sweep with the override applied", aa[0])
			}
		})
	}
}

func TestScanAllOverrideHandlerAppliesOverridesBelowAMiss(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	writeOrderDiscoveryFile(t, filepath.Join(cityPath, "orders"), "backup", `[order]
exec = "scripts/backup.sh"
trigger = "cooldown"
interval = "1h"
`)
	off := false
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{City: []string{cityLayer}},
		Orders: config.OrdersConfig{
			Overrides: []config.OrderOverride{
				{Name: "missing"},
				{Name: "backup", Enabled: &off},
			},
		},
	}

	var handled string
	aa, err := ScanAll(cityPath, cfg, ScanOptions{
		OnOverrideError: func(err error) error {
			handled = err.Error()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	if !strings.Contains(handled, `order "missing" not found`) {
		t.Fatalf("handled override error = %q, want missing-order error", handled)
	}
	if len(orders.FilterEnabled(aa)) != 0 {
		t.Fatalf("backup is still enabled: the enabled = false below the miss was skipped: %+v", aa)
	}
}

// The operator's own enabled = false, in a city- or rig-local order file,
// is final. An override that could not reach it before this change must not
// reach it now: a wildcard enable that was inert for the local copy would
// otherwise silently start an order the operator turned off.
func TestScanAllOverrideNeverReopensALocallyDisabledOrder(t *testing.T) {
	on := true

	t.Run("rig-local copy shadows an enabled pack order", func(t *testing.T) {
		cityPath, cityLayer := orderDiscoveryCity(t)
		packDir := filepath.Join(t.TempDir(), "patrol-pack")
		writeOrderDiscoveryFile(t, filepath.Join(packDir, "orders"), "patrol", `[order]
exec = "scripts/pack-patrol.sh"
trigger = "cooldown"
interval = "15m"
`)
		betaLayer := orderDiscoveryRigLayer(t, "beta")
		writeOrderDiscoveryFile(t, filepath.Join(filepath.Dir(betaLayer), "orders"), "patrol", `[order]
exec = "scripts/local-patrol.sh"
trigger = "cooldown"
interval = "15m"
enabled = false
`)
		cfg := &config.City{
			FormulaLayers: config.FormulaLayers{
				City: []string{cityLayer},
				Rigs: map[string][]string{
					"alpha": {cityLayer},
					"beta":  {cityLayer, betaLayer},
				},
			},
			Rigs:        []config.Rig{{Name: "alpha"}, {Name: "beta", FormulasDir: betaLayer}},
			RigPackDirs: map[string][]string{"alpha": {packDir}, "beta": {packDir}},
			Orders:      config.OrdersConfig{Overrides: []config.OrderOverride{{Name: "patrol", Rig: orders.RigWildcard, Enabled: &on}}},
		}
		aa, err := ScanAll(cityPath, cfg, ScanOptions{})
		if err != nil {
			t.Fatalf("ScanAll returned error: %v", err)
		}
		if got := enabledRigs(aa, "patrol"); strings.Join(got, ",") != "alpha" {
			t.Fatalf("enabled patrol rigs = %v, want [alpha]: beta's own enabled = false was reopened", got)
		}
		if len(aa) != 1 {
			t.Fatalf("got %+v, want only patrol:rig:alpha", aa)
		}
	})

	t.Run("city-local order file", func(t *testing.T) {
		cityPath, cityLayer := orderDiscoveryCity(t)
		writeOrderDiscoveryFile(t, filepath.Join(cityPath, "orders"), "patrol", disabledPatrolOrder)
		cfg := &config.City{
			FormulaLayers: config.FormulaLayers{City: []string{cityLayer}},
			Orders:        config.OrdersConfig{Overrides: []config.OrderOverride{{Name: "patrol", Enabled: &on}}},
		}
		aa, err := ScanAll(cityPath, cfg, ScanOptions{})
		if err == nil || !strings.Contains(err.Error(), `order "patrol" not found`) {
			t.Fatalf("ScanAll = %+v, %v; want the override to stay unmatched", aa, err)
		}
	})

	t.Run("city dir that is also a rig's pack dir", func(t *testing.T) {
		cityPath, cityLayer := orderDiscoveryCity(t)
		writeOrderDiscoveryFile(t, filepath.Join(cityPath, "orders"), "patrol", disabledPatrolOrder)
		cfg := &config.City{
			FormulaLayers: config.FormulaLayers{
				City: []string{cityLayer},
				Rigs: map[string][]string{"alpha": {cityLayer}},
			},
			RigPackDirs: map[string][]string{"alpha": {cityPath}},
			Orders:      config.OrdersConfig{Overrides: []config.OrderOverride{{Name: "patrol", Rig: "alpha", Enabled: &on}}},
		}
		aa, err := ScanAll(cityPath, cfg, ScanOptions{})
		if err == nil || !strings.Contains(err.Error(), `order "patrol" (rig "alpha") not found`) {
			t.Fatalf("ScanAll = %+v, %v; want the override to stay unmatched", aa, err)
		}
	})

	t.Run("city-local dir that is also a pack dir", func(t *testing.T) {
		cityPath, cityLayer := orderDiscoveryCity(t)
		writeOrderDiscoveryFile(t, filepath.Join(cityPath, "orders"), "patrol", disabledPatrolOrder)
		cfg := &config.City{
			FormulaLayers: config.FormulaLayers{City: []string{cityLayer}},
			PackDirs:      []string{cityPath},
			Orders:        config.OrdersConfig{Overrides: []config.OrderOverride{{Name: "patrol", Enabled: &on}}},
		}
		aa, err := ScanAll(cityPath, cfg, ScanOptions{})
		if err == nil || !strings.Contains(err.Error(), `order "patrol" not found`) {
			t.Fatalf("ScanAll = %+v, %v; want the override to stay unmatched", aa, err)
		}
	})
}

// An order an override enabled stays listed, as disabled, when a later
// override disables it again, like any pack-enabled order an override
// disables. Dropping it would hide it from gc order show and from the API's
// enable path, so an API disable could not be undone.
func TestScanAllReEnabledThenDisabledOrderStaysListed(t *testing.T) {
	on, off := true, false
	cityPath, cfg := packDisabledRigCity(t,
		config.OrderOverride{Name: "patrol", Rig: orders.RigWildcard, Enabled: &on},
		config.OrderOverride{Name: "patrol", Rig: "alpha", Enabled: &off},
	)
	aa, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	if got := enabledRigs(aa, "patrol"); strings.Join(got, ",") != "beta" {
		t.Fatalf("enabled patrol rigs = %v, want [beta]", got)
	}
	var alpha *orders.Order
	for i := range aa {
		if aa[i].Name == "patrol" && aa[i].Rig == "alpha" {
			alpha = &aa[i]
		}
	}
	if alpha == nil || alpha.IsEnabled() {
		t.Fatalf("patrol:rig:alpha = %+v, want it listed and disabled", alpha)
	}
	if len(orders.FilterEnabled(aa)) != 1 {
		t.Fatalf("FilterEnabled(%+v) should leave only beta", aa)
	}
}

// A pack imported at city scope that declares a disabled scope = "rig" order
// is cloned per rig; an override can re-enable one rig's clone.
func TestScanAllOverrideReEnablesOneCloneOfACityImportedRigScopedOrder(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	on := true
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{City: []string{cityLayer}},
		PackDirs: []string{cityPackWithOrder(t, "patrol", `[order]
scope = "rig"
exec = "scripts/patrol.sh"
trigger = "cooldown"
interval = "15m"
enabled = false
`)},
		Rigs:   []config.Rig{{Name: "alpha"}, {Name: "beta"}},
		Orders: config.OrdersConfig{Overrides: []config.OrderOverride{{Name: "patrol", Rig: "beta", Enabled: &on}}},
	}
	aa, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	if got := enabledRigs(aa, "patrol"); strings.Join(got, ",") != "beta" || len(aa) != 1 {
		t.Fatalf("ScanAll = %+v, want only patrol:rig:beta, enabled", aa)
	}
}
