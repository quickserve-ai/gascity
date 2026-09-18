package orders

import (
	"errors"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
)

// orderDir is the subdirectory name within formula layers that contains orders.
const orderDir = "orders"

// orderFileName is the expected filename inside each order subdirectory.
const orderFileName = "order.toml"

// ScanRoot describes one order discovery root and, optionally, the
// formula layer it belongs to for PACK_DIR semantics.
type ScanRoot struct {
	Dir          string
	FormulaLayer string
	// FromPack marks a pack's orders directory. Only a disabled order whose
	// winning definition comes from such a root can be retained by
	// ScanRootsRetaining: a pack's enabled = false is a default a city may
	// override, while an operator's own enabled = false stays final.
	FromPack bool
}

// Scan discovers orders across formula layers. Wave 2 requires top-level flat
// order files; older PackV1 directory layouts now hard-error. Higher-priority
// layers (later in the slice) override lower ones by order name. Disabled
// orders and those in the skip list are excluded.
func Scan(fs fsys.FS, formulaLayers []string, skip []string) ([]Order, error) {
	roots := make([]ScanRoot, 0, len(formulaLayers))
	for _, layer := range formulaLayers {
		roots = append(roots, ScanRoot{
			Dir:          filepath.Join(filepath.Dir(layer), orderDir),
			FormulaLayer: layer,
		})
	}
	return ScanRoots(fs, roots, skip)
}

// ScanRoots discovers orders across explicit order roots. Higher-priority
// roots (later in the slice) override lower ones by order name. Disabled
// orders and those in the skip list are excluded.
func ScanRoots(fs fsys.FS, roots []ScanRoot, skip []string) ([]Order, error) {
	return ScanRootsRetaining(fs, roots, skip, nil)
}

// ScanRootsRetaining is ScanRoots, except that a disabled order whose name is
// in retainDisabled, and whose winning definition comes from a FromPack root,
// is returned, still disabled, instead of being dropped. Order discovery
// passes the names its [[orders.overrides]] target, so an override can match,
// and re-enable, an order its pack ships disabled. The caller owns dropping
// every retained order that no override enabled. Skipped orders are excluded
// either way.
func ScanRootsRetaining(fs fsys.FS, roots []ScanRoot, skip []string, retainDisabled []string) ([]Order, error) {
	skipSet := make(map[string]bool, len(skip))
	for _, s := range skip {
		skipSet[s] = true
	}
	retainSet := make(map[string]bool, len(retainDisabled))
	for _, name := range retainDisabled {
		retainSet[name] = true
	}

	// Scan layers lowest → highest priority. Later entries override earlier ones.
	found := make(map[string]Order) // name → order
	fromPack := make(map[string]bool)
	var order []string // preserve discovery order
	var legacyFindings []legacyOrderLayoutFinding

	for _, root := range roots {
		discovered, err := discoverRoot(fs, root)
		if err != nil {
			var legacyErr legacyOrderLayoutError
			if errors.As(err, &legacyErr) {
				legacyFindings = append(legacyFindings, legacyErr.findings...)
				continue
			}
			return nil, err
		}
		for _, a := range discovered {
			name := a.Name
			if _, exists := found[name]; !exists {
				order = append(order, name)
			}
			found[name] = a // higher-priority layer overwrites
			fromPack[name] = root.FromPack
		}
	}
	if len(legacyFindings) > 0 {
		return nil, legacyOrderLayoutError{findings: legacyFindings}
	}

	// Collect results, excluding skipped orders and disabled ones the caller
	// did not ask to retain.
	var result []Order
	for _, name := range order {
		a := found[name]
		if !a.IsEnabled() && (!retainSet[name] || !fromPack[name]) {
			continue
		}
		if skipSet[name] || hasSkippedAlias(a, skipSet) {
			continue
		}
		result = append(result, a)
	}
	return result, nil
}

func hasSkippedAlias(a Order, skipSet map[string]bool) bool {
	for _, alias := range a.skipAliases {
		if skipSet[alias] {
			return true
		}
	}
	return false
}
