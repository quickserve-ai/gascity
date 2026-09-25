package main

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/gastownhall/gascity/internal/testpolicy/resourcecensus"
)

// censusCannotTellMarker ends every skipped owner line: the check could not
// look the owner up, so it cannot say whether the owner is foreign or dangling.
const censusCannotTellMarker = "cannot tell foreign from dangling"

// censusSkipFixHint is the fix hint when the only problems are skips.
const censusSkipFixHint = "fix bead store access or prefix routing, then rerun gc doctor"

// censusOwnerLivenessCheck detects resource-census ledger rows
// (test/test-resources.toml) whose owner_bead no longer resolves in this
// city. Each owner_bead is looked up in the store its PREFIX routes to (the
// routes.jsonl mapping: the HQ prefix to the city, each bound rig's prefix to
// that rig), not in the store of the scope whose ledger names it. Detection
// only: it never repairs the ledger.
type censusOwnerLivenessCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

// newCensusOwnerLivenessCheck constructs a censusOwnerLivenessCheck.
func newCensusOwnerLivenessCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *censusOwnerLivenessCheck {
	return &censusOwnerLivenessCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

// Name returns the check's identifier.
func (c *censusOwnerLivenessCheck) Name() string { return "census-owner-liveness" }

// CanFix reports that this check is detection-only.
func (c *censusOwnerLivenessCheck) CanFix() bool { return false }

// Fix is a no-op; this check never auto-repairs findings.
func (c *censusOwnerLivenessCheck) Fix(_ *doctor.CheckContext) error { return nil }

// censusOwnerReport accumulates one run's classified owner lines.
type censusOwnerReport struct {
	// findings are dangling owners: not found where their prefix routes, on a
	// scope that declares no foreign owner namespace.
	findings []string
	// foreign are owners not found here on a rig that declares a foreign
	// owner namespace. Informational, never a finding.
	foreign []string
	// notes carry one caveat per declared rig that reported a foreign owner.
	notes []string
	// skipped are ledgers or owners the check could not classify.
	skipped []string
}

// censusRouteStore is one routed store, opened at most once per run.
type censusRouteStore struct {
	store beads.Store
	err   error
}

// censusOwnerScan holds the per-run routing table and opened stores.
type censusOwnerScan struct {
	check  *censusOwnerLivenessCheck
	routes map[string]string // lower-cased prefix -> store directory
	stores map[string]censusRouteStore
	report censusOwnerReport
}

// Run scans the city and each non-suspended, path-bearing rig's
// resource-census ledger and classifies every owner_bead it references as
// found, foreign (declared rig only), dangling, or skipped.
func (c *censusOwnerLivenessCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	scan := &censusOwnerScan{
		check:  c,
		routes: censusOwnerRoutes(c.cfg, c.cityPath),
		stores: map[string]censusRouteStore{},
	}

	// The city scope never declares a foreign owner namespace.
	scan.scanScope("city", c.cityPath, "")
	if c.cfg != nil {
		suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
		for _, rig := range c.cfg.Rigs {
			if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			scan.scanScope("rig "+rig.Name, rig.Path, strings.TrimSpace(rig.Doctor.CensusOwnerNamespace))
		}
	}
	report := scan.report

	details := append([]string{}, report.findings...)
	details = append(details, report.foreign...)
	details = append(details, report.notes...)
	details = append(details, report.skipped...)
	sort.Strings(details)

	if len(report.findings) == 0 && len(report.skipped) == 0 {
		if len(report.foreign) == 0 {
			return okCheck(c.Name(), "no dangling owner_bead references found in resource-census ledgers")
		}
		result := okCheck(c.Name(), fmt.Sprintf("no dangling owner_bead references found in resource-census ledgers (%d owner_bead(s) owned in a declared foreign namespace)", len(report.foreign)))
		result.Details = details
		return result
	}

	if len(report.findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("census-owner-liveness check skipped %d item(s)", len(report.skipped)),
			censusSkipFixHint,
			details)
	}

	message := fmt.Sprintf("found %d dangling owner_bead reference(s) in resource-census ledgers", len(report.findings))
	if len(report.skipped) > 0 {
		message = fmt.Sprintf("%s (and skipped %d item(s))", message, len(report.skipped))
	}
	fixHint := "re-point the ledger row's owner_bead through council review (see TESTING.md), or fix bead store access and rerun gc doctor"
	return warnCheck(c.Name(), message, fixHint, details)
}

// censusOwnerRoutes returns the prefix routing table gc bd and routes.jsonl
// use: the city's HQ prefix to the city directory and each path-bound rig's
// effective prefix to its directory. Prefixes are lower-cased; the first
// route for a prefix wins, so the HQ route leads.
func censusOwnerRoutes(cfg *config.City, cityPath string) map[string]string {
	routes := map[string]string{}
	if cfg == nil {
		return routes
	}
	for _, route := range collectRigRoutes(cityPath, cfg) {
		prefix := strings.ToLower(strings.TrimSpace(route.Prefix))
		if prefix == "" {
			continue
		}
		if _, exists := routes[prefix]; !exists {
			routes[prefix] = route.AbsDir
		}
	}
	return routes
}

// routedStore returns the store an owner_bead's prefix routes to. ok is false
// when the prefix routes to no store in this city; err is the open error of a
// routed store that could not be opened.
func (s *censusOwnerScan) routedStore(id string) (prefix, dir string, store beads.Store, ok bool, err error) {
	prefix = beadPrefix(s.check.cfg, id)
	dir, ok = s.routes[strings.ToLower(prefix)]
	if !ok {
		return prefix, "", nil, false, nil
	}
	opened, cached := s.stores[dir]
	if !cached {
		opened.store, opened.err = s.check.newStore(dir)
		s.stores[dir] = opened
	}
	return prefix, dir, opened.store, true, opened.err
}

// scanScope loads the resource-census ledger at path, if any, and classifies
// each unique owner_bead it references. namespace is the scope's declared
// foreign owner namespace, empty when undeclared.
//
// A missing ledger file is expected for almost every scope and is skipped
// silently. A ledger load error, an owner whose prefix routes to no store, a
// routed store that cannot be opened, and a non-not-found Get error are each
// recorded as a skip, never as dangling, clean, or foreign.
func (s *censusOwnerScan) scanScope(label, path, namespace string) {
	if s.check.newStore == nil || strings.TrimSpace(path) == "" {
		return
	}

	ledgerPath := filepath.Join(path, "test", "test-resources.toml")
	ledger, err := resourcecensus.LoadLedger(ledgerPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		s.report.skipped = append(s.report.skipped, fmt.Sprintf("%s skipped: loading resource-census ledger: %v", label, err))
		return
	}

	rows := collectCensusOwnerBeadRows(ledger)
	if len(rows) == 0 {
		return
	}

	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	foreign := 0
	for _, id := range ids {
		prefix, dir, store, routed, openErr := s.routedStore(id)
		if !routed {
			s.report.skipped = append(s.report.skipped, fmt.Sprintf("%s skipped: owner_bead %s: prefix %q routes to no bead store in this city (%s)", label, id, prefix, censusCannotTellMarker))
			continue
		}
		if openErr != nil {
			s.report.skipped = append(s.report.skipped, fmt.Sprintf("%s skipped: opening bead store: %v (owner_bead %s routes by prefix %q to %s; %s)", label, openErr, id, prefix, dir, censusCannotTellMarker))
			continue
		}
		_, err := store.Get(id)
		switch {
		case err == nil:
			continue
		case !errors.Is(err, beads.ErrNotFound):
			s.report.skipped = append(s.report.skipped, fmt.Sprintf("%s skipped: checking owner_bead %s: %v (%s)", label, id, err, censusCannotTellMarker))
		case namespace != "":
			foreign++
			s.report.foreign = append(s.report.foreign, fmt.Sprintf("%s: owner_bead=%s owned in %s, not observable in this city rows=[%s]", label, id, namespace, strings.Join(rows[id], "; ")))
		default:
			s.report.findings = append(s.report.findings, fmt.Sprintf("%s: dangling owner_bead=%s rows=[%s]", label, id, strings.Join(rows[id], "; ")))
		}
	}
	if foreign > 0 {
		s.report.notes = append(s.report.notes, fmt.Sprintf("%s: census_owner_namespace=%s: owners missing from this city are reported as owned there, so an owner on this rig that is genuinely this city's is not liveness-checked", label, namespace))
	}
}

// collectCensusOwnerBeadRows collects, per unique owner_bead, a
// human-readable descriptor of every ledger row that references it across
// all four row categories.
func collectCensusOwnerBeadRows(ledger resourcecensus.Ledger) map[string][]string {
	rows := map[string][]string{}

	addBaseline := func(category string, list []resourcecensus.Baseline) {
		for _, row := range list {
			id := strings.TrimSpace(row.OwnerBead)
			if id == "" {
				continue
			}
			desc := fmt.Sprintf("%s: scope=%s resource=%s", category, row.Scope, row.Resource)
			rows[id] = append(rows[id], desc)
		}
	}
	addBaseline("audit_baseline", ledger.AuditBaseline)
	addBaseline("debt", ledger.Debt)
	addBaseline("small_debt", ledger.SmallDebt)

	for _, row := range ledger.Medium {
		id := strings.TrimSpace(row.OwnerBead)
		if id == "" {
			continue
		}
		desc := fmt.Sprintf("medium: package_dir=%s package_name=%s owner=%s", row.PackageDir, row.PackageName, row.Owner)
		rows[id] = append(rows[id], desc)
	}

	return rows
}
