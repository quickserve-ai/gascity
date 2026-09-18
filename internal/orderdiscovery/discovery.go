// Package orderdiscovery scans configured city and rig order roots.
package orderdiscovery

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
)

// RigScanErrorHandler handles a failed rig-exclusive order scan.
// Returning nil skips that rig and continues scanning remaining rigs.
type RigScanErrorHandler func(rigName string, err error) error

// OverrideErrorHandler handles a failed [orders.ApplyOverrides] call.
// Returning nil keeps the scanned orders with every valid override applied;
// only the overrides named in the error had no effect.
type OverrideErrorHandler func(err error) error

// ValidateErrorHandler handles an order validation failure after config
// layering. Returning nil drops that order and continues scanning.
type ValidateErrorHandler func(orderName string, err error) error

// OrderValidator performs caller-specific post-layering validation.
type OrderValidator func(order orders.Order) error

// ScanOptions controls shared order discovery behavior.
type ScanOptions struct {
	FS              fsys.FS
	OnRigScanError  RigScanErrorHandler
	OnOverrideError OverrideErrorHandler
	OnValidateError ValidateErrorHandler
	ValidateOrder   OrderValidator
}

// ScanAll scans city-level and rig-exclusive order roots, stamps rig orders,
// and applies configured order overrides. The returned slice includes orders
// disabled by overrides; callers choose whether to filter them.
//
// An order its pack ships disabled is scanned only when an override names it,
// so that the override can match it and re-enable it. It is returned only if
// an override enabled it (a later override may still disable it again, which
// leaves it listed as disabled, like any other override-disabled order);
// otherwise it is dropped exactly as the scan would have dropped it. An order
// disabled by the city's or a rig's own local order file is never retained:
// that enabled = false is the operator's, and no override reopens it.
func ScanAll(cityPath string, cfg *config.City, opts ScanOptions) ([]orders.Order, error) {
	if cfg == nil {
		cfg = &config.City{}
	}
	fsysImpl := opts.FS
	if fsysImpl == nil {
		fsysImpl = fsys.OSFS{}
	}

	cityLayers := cityFormulaLayers(cityPath, cfg)
	rigNames := make(map[string]struct{}, len(cfg.FormulaLayers.Rigs)+len(cfg.RigPackDirs))
	for rigName := range cfg.FormulaLayers.Rigs {
		rigNames[rigName] = struct{}{}
	}
	for rigName := range cfg.RigPackDirs {
		rigNames[rigName] = struct{}{}
	}

	retainDisabled := overrideTargetNames(cfg.Orders.Overrides)
	cityOrders, err := orders.ScanRootsRetaining(fsysImpl, withoutOperatorPackRoots(CityOrderRoots(cityPath, cfg), cityOperatorOrderDirs(cityPath)), cfg.Orders.Skip, retainDisabled)
	if err != nil {
		return nil, err
	}

	// City-scoped orders register exactly once regardless of how many rigs
	// import the pack, so dedup them across the rig loop by name. Seed the set
	// with city-level orders so a city-local order of the same name wins.
	// Retained disabled orders never claim a name here; dropShadowedDisabled
	// resolves them after the loop.
	cityScopedSeen := make(map[string]bool, len(cityOrders))
	for _, o := range cityOrders {
		if o.IsEnabled() {
			cityScopedSeen[o.Name] = true
		}
	}

	var promotedCityOrders, rigOrders []orders.Order
	for _, rigName := range sortedRigNames(rigNames) {
		exclusive := RigExclusiveLayers(cfg.FormulaLayers.Rigs[rigName], cityLayers)
		exclusivePackDirs := cfg.RigPackDirs[rigName]
		if len(exclusive) == 0 && len(exclusivePackDirs) == 0 {
			continue
		}
		roots := rigOrderRoots(exclusive, exclusivePackDirs, rigLocalFormulaLayer(exclusive, exclusivePackDirs))
		aa, err := orders.ScanRootsRetaining(fsysImpl, withoutOperatorPackRoots(roots, rigOperatorOrderDirs(cityPath, cfg, cityLayers, rigName)), cfg.Orders.Skip, retainDisabled)
		if err != nil {
			if opts.OnRigScanError != nil {
				if handlerErr := opts.OnRigScanError(rigName, err); handlerErr != nil {
					return nil, handlerErr
				}
				continue
			}
			return nil, fmt.Errorf("rig %s: %w", rigName, err)
		}
		for i := range aa {
			if aa[i].IsCityScoped() {
				if !aa[i].IsEnabled() {
					promotedCityOrders = append(promotedCityOrders, aa[i])
					continue
				}
				// Keep the first occurrence (rigs are scanned in deterministic
				// order) and leave Rig empty so it registers city-wide once.
				if cityScopedSeen[aa[i].Name] {
					continue
				}
				cityScopedSeen[aa[i].Name] = true
				promotedCityOrders = append(promotedCityOrders, aa[i])
				continue
			}
			aa[i].Rig = rigName
			rigOrders = append(rigOrders, aa[i])
		}
	}

	// An order that declares scope = "rig" in a pack imported at city scope
	// instantiates once per configured rig, the way an agent or named
	// session declaring the same in that pack does (config's
	// expandCityImportedAgentsForRigs): the declaration would otherwise be
	// validated and then silently dropped, registering the order once,
	// city-wide, against the city store. A rig that imports the pack itself
	// already holds its instance from the loop above and is skipped. An
	// order that declares nothing keeps its contract: once, city-wide.
	cityOrders, cityRigScoped := splitCityImportedRigScopedOrders(cityOrders, cfg.PackDirs)
	for _, rig := range cfg.Rigs {
		for _, o := range cityRigScoped {
			if orderUnderPackDirs(o.Source, cfg.RigPackDirs[rig.Name]) {
				continue
			}
			rigOrders = append(rigOrders, cloneOrderForRig(o, rig.Name))
		}
	}

	allOrders := make([]orders.Order, 0, len(cityOrders)+len(promotedCityOrders)+len(rigOrders))
	allOrders = append(allOrders, cityOrders...)
	allOrders = append(allOrders, promotedCityOrders...)
	allOrders = append(allOrders, rigOrders...)
	allOrders = dropShadowedDisabled(allOrders)
	// Stamp the city-default cron timezone onto orders that don't author
	// their own tz, so trigger evaluation sees one explicit location without
	// widening the CheckTrigger signature. A bad [workspace] timezone fails
	// the whole scan loudly — a silent fallback would move every inheriting
	// order's schedule onto a different wall clock.
	if tz := cfg.Workspace.Timezone; tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return nil, fmt.Errorf("[workspace] timezone %q: %w", tz, err)
		}
		for i := range allOrders {
			if allOrders[i].TZ == "" {
				allOrders[i].TZ = tz
			}
		}
	}
	if len(cfg.Orders.Overrides) > 0 {
		overrides := overridesFromConfig(cfg.Orders.Overrides)
		disabledAtScan := make([]bool, len(allOrders))
		for i := range allOrders {
			disabledAtScan[i] = !allOrders[i].IsEnabled()
		}
		if err := orders.ApplyOverrides(allOrders, overrides); err != nil {
			if opts.OnOverrideError == nil {
				return nil, err
			}
			if handlerErr := opts.OnOverrideError(err); handlerErr != nil {
				return nil, handlerErr
			}
		}
		// A retained order that no override enabled leaves the scan here,
		// before validation, as it did when the scan dropped it outright.
		kept := allOrders[:0]
		for i := range allOrders {
			if disabledAtScan[i] && !enabledByOverride(overrides, &allOrders[i]) {
				continue
			}
			kept = append(kept, allOrders[i])
		}
		allOrders = kept
	}
	allOrders, err = validateOrders(allOrders, opts.ValidateOrder, opts.OnValidateError)
	if err != nil {
		return nil, err
	}
	return allOrders, nil
}

func validateOrders(allOrders []orders.Order, extraValidate OrderValidator, onError ValidateErrorHandler) ([]orders.Order, error) {
	valid := allOrders[:0]
	for _, order := range allOrders {
		if err := validateOrder(order, extraValidate); err != nil {
			if onError == nil {
				return nil, err
			}
			if handlerErr := onError(order.ScopedName(), err); handlerErr != nil {
				return nil, handlerErr
			}
			continue
		}
		valid = append(valid, order)
	}
	return valid, nil
}

func validateOrder(order orders.Order, extraValidate OrderValidator) error {
	if err := orders.Validate(order); err != nil {
		return err
	}
	if extraValidate != nil {
		if err := extraValidate(order); err != nil {
			return err
		}
	}
	return nil
}

// cityFormulaLayers returns the formula directory layers for city-level order
// scanning.
func cityFormulaLayers(cityPath string, cfg *config.City) []string {
	if len(cfg.FormulaLayers.City) > 0 {
		return cfg.FormulaLayers.City
	}
	return []string{citylayout.ResolveFormulasDir(cityPath, cfg.FormulasDir())}
}

// CityOrderRoots returns the order roots used for city-level discovery.
func CityOrderRoots(cityPath string, cfg *config.City) []orders.ScanRoot {
	formulaLayers := cityFormulaLayers(cityPath, cfg)
	localFormulas := citylayout.ResolveFormulasDir(cityPath, cfg.FormulasDir())

	// Formula layers include system packs (via LoadWithIncludes extraIncludes)
	// and user packs (via workspace.includes). City-local formulas are highest
	// priority and override pack formulas when order names collide.
	return orderRoots(formulaLayers, cfg.PackDirs, localFormulas, orders.ScanRoot{
		Dir:          citylayout.OrdersPath(cityPath),
		FormulaLayer: localFormulas,
	})
}

func rigOrderRoots(formulaLayers []string, packDirs []string, localFormulas string) []orders.ScanRoot {
	localRoot := orders.ScanRoot{}
	if localFormulas != "" {
		localRoot = formulaLayerRoot(localFormulas)
	}
	return orderRoots(formulaLayers, packDirs, localFormulas, localRoot)
}

func orderRoots(formulaLayers []string, packDirs []string, localFormulas string, localRoot orders.ScanRoot) []orders.ScanRoot {
	roots := make([]orders.ScanRoot, 0, len(formulaLayers)+len(packDirs)+1)
	seen := make(map[string]bool, len(formulaLayers)+len(packDirs)+1)
	appendRoot := func(root orders.ScanRoot) {
		key := scanRootKey(root)
		if seen[key] {
			return
		}
		seen[key] = true
		roots = append(roots, root)
	}

	for _, packDir := range packDirs {
		appendRoot(packRoot(packDir))
	}

	localFound := false
	for _, layer := range formulaLayers {
		if samePath(layer, localFormulas) {
			if !localFound {
				if localRoot.Dir == "" {
					localRoot = formulaLayerRoot(layer)
				}
				localFound = true
			}
			continue
		}
		appendRoot(formulaLayerRoot(layer))
	}

	if localFound {
		appendRoot(localRoot)
	}
	return roots
}

func formulaLayerRoot(layer string) orders.ScanRoot {
	return orders.ScanRoot{
		Dir:          filepath.Join(filepath.Dir(layer), "orders"),
		FormulaLayer: layer,
	}
}

// splitCityImportedRigScopedOrders partitions the city-level scan into the
// orders that stay city-wide and those a city-imported pack declared with
// scope = "rig", recognized by a Source under one of the pack dirs' orders/.
func splitCityImportedRigScopedOrders(cityOrders []orders.Order, packDirs []string) (city, rigScoped []orders.Order) {
	for _, o := range cityOrders {
		if o.IsRigScoped() && orderUnderPackDirs(o.Source, packDirs) {
			rigScoped = append(rigScoped, o)
			continue
		}
		city = append(city, o)
	}
	return city, rigScoped
}

// orderUnderPackDirs reports whether an order file was scanned from the
// orders/ directory of one of the given pack dirs.
func orderUnderPackDirs(source string, packDirs []string) bool {
	for _, dir := range packDirs {
		if pathWithin(source, packRoot(dir).Dir) {
			return true
		}
	}
	return false
}

func pathWithin(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// cloneOrderForRig stamps a copy of a city-scanned order for one rig. The
// maps are copied: overrides and env projection mutate them per instance.
func cloneOrderForRig(o orders.Order, rig string) orders.Order {
	o.Rig = rig
	if o.Env != nil {
		env := make(map[string]string, len(o.Env))
		for k, v := range o.Env {
			env[k] = v
		}
		o.Env = env
	}
	if o.Params != nil {
		params := make(map[string]orders.OrderParam, len(o.Params))
		for k, v := range o.Params {
			params[k] = v
		}
		o.Params = params
	}
	return o
}

func packRoot(packDir string) orders.ScanRoot {
	return orders.ScanRoot{
		Dir:          filepath.Join(packDir, "orders"),
		FormulaLayer: filepath.Join(packDir, "formulas"),
		FromPack:     true,
	}
}

func scanRootKey(root orders.ScanRoot) string {
	return filepath.Clean(root.Dir) + "\n" + filepath.Clean(root.FormulaLayer)
}

func samePath(a, b string) bool {
	return a != "" && b != "" && filepath.Clean(a) == filepath.Clean(b)
}

func rigLocalFormulaLayer(formulaLayers []string, packDirs []string) string {
	packFormulaLayers := make(map[string]bool, len(packDirs))
	for _, packDir := range packDirs {
		packFormulaLayers[filepath.Clean(filepath.Join(packDir, "formulas"))] = true
	}
	for i := len(formulaLayers) - 1; i >= 0; i-- {
		layer := formulaLayers[i]
		if !packFormulaLayers[filepath.Clean(layer)] {
			return layer
		}
	}
	return ""
}

// RigExclusiveLayers returns the suffix of rig layers that is not inherited
// from the city formula layers.
func RigExclusiveLayers(rigLayers, cityLayers []string) []string {
	if len(rigLayers) <= len(cityLayers) {
		return nil
	}
	return rigLayers[len(cityLayers):]
}

func sortedRigNames(rigs map[string]struct{}) []string {
	names := make([]string, 0, len(rigs))
	for name := range rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// overrideTargetNames returns the order names the overrides target, which are
// the disabled orders the scan must retain for them to match.
func overrideTargetNames(cfgOverrides []config.OrderOverride) []string {
	names := make([]string, 0, len(cfgOverrides))
	for _, override := range cfgOverrides {
		if override.Name != "" {
			names = append(names, override.Name)
		}
	}
	return names
}

// cityOperatorOrderDirs lists the order directories the operator owns in the
// CITY scan: the city's own orders directory. An order disabled in an
// operator-owned directory is the operator's decision, so even when a pack dir
// resolves to the same directory it must never be retained for an override
// to reopen.
func cityOperatorOrderDirs(cityPath string) []string {
	return []string{citylayout.OrdersPath(cityPath)}
}

// rigOperatorOrderDirs lists the order directories the operator owns in ONE
// rig's scan: the city's own, plus that rig's configured formulas_dir and its
// rig-local formula layer. Ownership is per scope: when rig A's formulas_dir
// is the same directory as a pack that rig B (or the city) imports, the
// orders there are A's own but still B's pack orders, so marking them
// operator-owned in every scope would stop B's enable override from
// re-enabling them.
//
// A rig's formulas_dir is the operator's even when it is the same directory
// as one of the rig's pack formula layers, where rigLocalFormulaLayer cannot
// see it. The path resolves exactly as the rig's formula layer does, so a
// "//" prefix means the city root.
func rigOperatorOrderDirs(cityPath string, cfg *config.City, cityLayers []string, rigName string) []string {
	dirs := cityOperatorOrderDirs(cityPath)
	for _, rig := range cfg.Rigs {
		if rig.Name != rigName || rig.FormulasDir == "" {
			continue
		}
		dir := config.ResolveRigFormulasDir(rig.FormulasDir, cityPath)
		dirs = append(dirs, formulaLayerRoot(dir).Dir)
	}
	exclusive := RigExclusiveLayers(cfg.FormulaLayers.Rigs[rigName], cityLayers)
	if local := rigLocalFormulaLayer(exclusive, cfg.RigPackDirs[rigName]); local != "" {
		dirs = append(dirs, formulaLayerRoot(local).Dir)
	}
	return dirs
}

// withoutOperatorPackRoots clears FromPack on every root in an
// operator-owned directory.
func withoutOperatorPackRoots(roots []orders.ScanRoot, operatorDirs []string) []orders.ScanRoot {
	for i := range roots {
		for _, dir := range operatorDirs {
			if samePath(roots[i].Dir, dir) {
				roots[i].FromPack = false
			}
		}
	}
	return roots
}

// enabledByOverride reports whether any override sets enabled = true on a.
func enabledByOverride(overrides []orders.Override, a *orders.Order) bool {
	for i := range overrides {
		if overrides[i].Enabled != nil && *overrides[i].Enabled && overrides[i].Matches(a) {
			return true
		}
	}
	return false
}

// dropShadowedDisabled resolves the disabled orders retained for overrides.
// One that shares its scoped name with an enabled order is dropped, so the
// enabled order registers exactly as it would without the retention. Among
// disabled orders that share a scoped name only the first is kept, so an
// override that re-enables the name registers it once. Enabled orders are
// never dropped or reordered.
func dropShadowedDisabled(aa []orders.Order) []orders.Order {
	enabled := make(map[string]bool, len(aa))
	for i := range aa {
		if aa[i].IsEnabled() {
			enabled[aa[i].ScopedName()] = true
		}
	}
	disabledKept := make(map[string]bool)
	kept := aa[:0]
	for i := range aa {
		if !aa[i].IsEnabled() {
			key := aa[i].ScopedName()
			if enabled[key] || disabledKept[key] {
				continue
			}
			disabledKept[key] = true
		}
		kept = append(kept, aa[i])
	}
	return kept
}

func overridesFromConfig(cfgOverrides []config.OrderOverride) []orders.Override {
	out := make([]orders.Override, len(cfgOverrides))
	for i, override := range cfgOverrides {
		out[i] = orders.Override{
			Name:         override.Name,
			Rig:          override.Rig,
			Enabled:      override.Enabled,
			Trigger:      override.Trigger,
			Interval:     override.Interval,
			Schedule:     override.Schedule,
			Check:        override.Check,
			On:           override.On,
			Pool:         override.Pool,
			Timeout:      override.Timeout,
			CheckTimeout: override.CheckTimeout,
			Idempotent:   override.Idempotent,
			Env:          override.Env,
		}
	}
	return out
}
