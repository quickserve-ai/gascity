package orders

import (
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestScan(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/layer1/orders/digest.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "24h"
pool = "dog"
`)
	fs.Files["/layer1/orders/cleanup.toml"] = []byte(`
[order]
formula = "mol-cleanup"
trigger = "cron"
schedule = "0 3 * * *"
`)

	orders, err := Scan(fs, []string{"/layer1/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("got %d orders, want 2", len(orders))
	}
	// Names should be set from directory names.
	names := map[string]bool{}
	for _, a := range orders {
		names[a.Name] = true
	}
	if !names["digest"] || !names["cleanup"] {
		t.Errorf("expected digest and cleanup, got %v", names)
	}
}

func TestScanEmpty(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/layer1/formulas"] = true

	orders, err := Scan(fs, []string{"/layer1/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 0 {
		t.Fatalf("got %d orders, want 0", len(orders))
	}
}

func TestScanLayerOverride(t *testing.T) {
	fs := fsys.NewFake()
	// Layer 1 (lower priority): digest with 24h.
	fs.Files["/layer1/orders/digest.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "24h"
pool = "dog"
`)
	// Layer 2 (higher priority): digest with 8h.
	fs.Files["/layer2/orders/digest.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "8h"
pool = "dog"
`)

	orders, err := Scan(fs, []string{"/layer1/formulas", "/layer2/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Interval != "8h" {
		t.Errorf("Interval = %q, want %q (layer 2 overrides)", orders[0].Interval, "8h")
	}
}

func TestScanSkip(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/layer1/orders/digest.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "24h"
`)
	fs.Files["/layer1/orders/cleanup.toml"] = []byte(`
[order]
formula = "mol-cleanup"
trigger = "manual"
`)

	orders, err := Scan(fs, []string{"/layer1/formulas"}, []string{"digest"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Name != "cleanup" {
		t.Errorf("Name = %q, want %q", orders[0].Name, "cleanup")
	}
}

func TestScanSkipAliases(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/layer1/orders/maintenance-export.toml"] = []byte(`
[order]
exec = "scripts/export.sh"
trigger = "cooldown"
interval = "15m"
skip_aliases = ["old-export"]
`)
	fs.Files["/layer1/orders/cleanup.toml"] = []byte(`
[order]
formula = "mol-cleanup"
trigger = "manual"
`)

	orders, err := Scan(fs, []string{"/layer1/formulas"}, []string{"old-export"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Name != "cleanup" {
		t.Errorf("Name = %q, want %q", orders[0].Name, "cleanup")
	}
}

func TestScanDisabled(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/layer1/orders/digest.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "24h"
enabled = false
`)

	orders, err := Scan(fs, []string{"/layer1/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 0 {
		t.Fatalf("got %d orders, want 0 (disabled)", len(orders))
	}
}

func TestScanRootsRetainingKeepsOnlyNamedPackDisabledOrders(t *testing.T) {
	fs := fsys.NewFake()
	for _, name := range []string{"digest", "sweep"} {
		fs.Files["/layer1/orders/"+name+".toml"] = []byte(`
[order]
formula = "mol-` + name + `"
trigger = "cooldown"
interval = "24h"
enabled = false
`)
	}
	roots := []ScanRoot{{Dir: "/layer1/orders", FormulaLayer: "/layer1/formulas", FromPack: true}}

	plain, err := ScanRoots(fs, roots, nil)
	if err != nil {
		t.Fatalf("ScanRoots: %v", err)
	}
	if len(plain) != 0 {
		t.Fatalf("ScanRoots returned %d orders, want 0 (both disabled)", len(plain))
	}

	retained, err := ScanRootsRetaining(fs, roots, nil, []string{"digest"})
	if err != nil {
		t.Fatalf("ScanRootsRetaining: %v", err)
	}
	if len(retained) != 1 || retained[0].Name != "digest" {
		t.Fatalf("ScanRootsRetaining = %+v, want only digest", retained)
	}
	if retained[0].IsEnabled() {
		t.Error("retained order came back enabled; retention must not change Enabled")
	}

	local := []ScanRoot{{Dir: "/layer1/orders", FormulaLayer: "/layer1/formulas"}}
	notPack, err := ScanRootsRetaining(fs, local, nil, []string{"digest"})
	if err != nil {
		t.Fatalf("ScanRootsRetaining on a non-pack root: %v", err)
	}
	if len(notPack) != 0 {
		t.Fatalf("got %+v, want nothing: a disabled order outside a pack is never retained", notPack)
	}

	skipped, err := ScanRootsRetaining(fs, roots, []string{"digest"}, []string{"digest"})
	if err != nil {
		t.Fatalf("ScanRootsRetaining with skip: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("got %d orders, want 0: skip must win over retention", len(skipped))
	}
}

func TestScanFormulaLayer(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/pack/orders/health.toml"] = []byte(`
[order]
exec = "$PACK_DIR/scripts/health.sh"
trigger = "cooldown"
interval = "1m"
`)

	orders, err := Scan(fs, []string{"/pack/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].FormulaLayer != "/pack/formulas" {
		t.Errorf("FormulaLayer = %q, want %q", orders[0].FormulaLayer, "/pack/formulas")
	}
}

func TestScanFormulaLayerOverride(t *testing.T) {
	fs := fsys.NewFake()
	// Layer 1: lower priority.
	fs.Files["/base/orders/health.toml"] = []byte(`
[order]
exec = "$PACK_DIR/scripts/health.sh"
trigger = "cooldown"
interval = "1h"
`)
	// Layer 2: higher priority overrides.
	fs.Files["/pack/orders/health.toml"] = []byte(`
[order]
exec = "$PACK_DIR/scripts/health.sh"
trigger = "cooldown"
interval = "5m"
`)

	orders, err := Scan(fs, []string{"/base/formulas", "/pack/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	// FormulaLayer should come from the winning (higher-priority) layer.
	if orders[0].FormulaLayer != "/pack/formulas" {
		t.Errorf("FormulaLayer = %q, want %q", orders[0].FormulaLayer, "/pack/formulas")
	}
}

func TestScanSourcePath(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/layer1/orders/digest.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "manual"
`)

	orders, err := Scan(fs, []string{"/layer1/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Source != "/layer1/orders/digest.toml" {
		t.Errorf("Source = %q, want %q", orders[0].Source, "/layer1/orders/digest.toml")
	}
}
