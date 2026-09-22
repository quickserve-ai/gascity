package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// doltServersCheck enumerates every live `dolt sql-server` on the host from the
// OS side and accounts for each one against this city (ga-7qkj).
//
// Every other liveness signal doctor has is SELF-REPORTED: dolt-drift reads a
// rig's .dolt/sql-server.info, the managed-dolt probes read the runtime handle
// and pid file, and processAliveByPID checks a pid a heartbeat recorded. A
// process that never wrote a marker file is invisible to all of them by
// construction. That is the field case: two gc-launched dolt servers held
// stores for 4 and 12 days with no lock, .port or .pid file, found only by
// lsof (ga-rd381q), while doctor read green. A guard built on marker files
// passes while the thing it guards is open.
//
// So this check starts from the process table (discoverDoltProcesses — the same
// enumeration `gc dolt cleanup` reaps from) and sorts each server into:
//
//   - managed: serves this city's managed runtime layout (--config/--data-dir).
//     More than one is a split-brain on the city's own store -> Error.
//   - rig-local: its --config/--data-dir sits under a non-HQ rig root. dolt-drift
//     judges whether that rig is allowed its own server; this check does not.
//   - city-stray: under the city root, but not the managed layout -> Warning.
//   - foreign: gc-launched (a .gc/runtime/packs/dolt/ config) but belonging to
//     no scope of this city -> Warning. Nothing in the fleet reaps these; a
//     scratch city left running holds RAM and a port until someone looks.
//
// A dolt server that gc did not launch (no gc runtime config) is not this
// tool's business and is only counted in Details.
//
// It never reports OK without having looked: an enumeration failure is
// StatusSkipped with the reason. On hosts without /proc, ports come from a
// best-effort lsof; they are shown for the operator and never used to decide
// ownership, so an lsof miss cannot turn a stray into an accounted server.
type doltServersCheck struct {
	cityPath string
	cfg      *config.City

	discover func() ([]DoltProcInfo, error)
	layout   func(cityPath string) (managedDoltRuntimeLayout, error)
	now      func() time.Time
}

var _ doctor.Check = (*doltServersCheck)(nil)

func newDoltServersCheck(cityPath string, cfg *config.City) *doltServersCheck {
	return &doltServersCheck{
		cityPath: cityPath,
		cfg:      cfg,
		discover: discoverDoltProcesses,
		// Strict: resolve the layout from cityPath alone. A seat's ambient
		// GC_DOLT_* env can describe another city, and honoring it would let
		// this check call that city's server "managed" here.
		layout: resolveManagedDoltRuntimeLayoutStrict,
		now:    time.Now,
	}
}

func (c *doltServersCheck) Name() string { return "dolt-servers" }

// CanFix is false: every server this check flags is one gc cannot prove it owns
// well enough to kill. `gc dolt cleanup` is the reaper, and it protects exactly
// these shapes on purpose.
func (c *doltServersCheck) CanFix() bool { return false }

func (c *doltServersCheck) Fix(*doctor.CheckContext) error { return nil }

func (c *doltServersCheck) WarmupEligible() bool { return false }

// gcDoltConfigMarker is the path suffix every gc-launched dolt server's
// --config carries (<scope>/.gc/runtime/packs/dolt/dolt-config.yaml).
var gcDoltConfigMarker = filepath.Join(".gc", "runtime", "packs", "dolt") + string(filepath.Separator)

func (c *doltServersCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}

	procs, err := c.discover()
	if err != nil {
		r.Status = doctor.StatusSkipped
		r.Message = fmt.Sprintf("could not enumerate dolt sql-server processes: %v — orphan and duplicate analysis NOT performed", err)
		return r
	}

	layout, layoutErr := c.layout(c.cityPath)
	rigs := c.scopeRigs()

	var managed, strays, foreign []DoltProcInfo
	var rigLocal, unmanaged int
	for _, p := range procs {
		switch {
		case layoutErr == nil && doltProcMatchesManagedLayout(p, layout):
			managed = append(managed, p)
		default:
			hq, ok := deepestDoltScopeOwner(p, rigs)
			switch {
			case ok && !hq:
				rigLocal++
			case ok && hq:
				strays = append(strays, p)
			case isGCLaunchedDolt(p):
				foreign = append(foreign, p)
			default:
				unmanaged++
			}
		}
	}

	var errs, warns []string
	if len(managed) > 1 {
		errs = append(errs, fmt.Sprintf(
			"%d dolt servers serve this city's managed store — split-brain; at most one may: %s",
			len(managed), c.describeAll(managed)))
	}
	for _, p := range strays {
		warns = append(warns, "dolt server under the city root that is not the managed server: "+c.describe(p))
	}
	for _, p := range foreign {
		warns = append(warns, "gc-launched dolt server that belongs to no scope of this city: "+c.describe(p))
	}

	var details []string
	if layoutErr != nil {
		details = append(details, fmt.Sprintf("managed dolt layout unresolvable (%v); duplicate-managed analysis skipped", layoutErr))
	}
	details = append(details, fmt.Sprintf(
		"%d dolt sql-server process(es) on host: %d managed, %d rig-local, %d city-stray, %d foreign gc-launched, %d not gc-launched",
		len(procs), len(managed), rigLocal, len(strays), len(foreign), unmanaged))

	switch {
	case len(errs) > 0:
		r.Status = doctor.StatusError
		r.Message = errs[0]
		r.Details = append(append(append([]string{}, errs[1:]...), warns...), details...)
		r.FixHint = "`gc dolt status` names the server the city runtime recorded; stop the others only after confirming none is mid-write — doctor will not kill them"
	case len(warns) > 0:
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("%d dolt server(s) running that this city does not account for", len(warns))
		r.Details = append(append([]string{}, warns...), details...)
		r.FixHint = "confirm the owning scope is no longer in use, then stop the server (`kill <pid>`); `gc dolt cleanup` reaps only test-scoped servers and deliberately protects these"
	case layoutErr != nil:
		// Nothing extra seen, but the one comparison that detects a
		// split-brain on the city's own store never ran. Not OK.
		r.Status = doctor.StatusWarning
		r.Message = "no unaccounted dolt servers found, but the managed layout was unresolvable so duplicate-managed analysis did not run"
		r.Details = details
	default:
		r.Status = doctor.StatusOK
		r.Message = fmt.Sprintf("every gc-launched dolt server is accounted for (%d managed, %d rig-local)", len(managed), rigLocal)
		r.Details = details
	}
	return r
}

// scopeRigs returns the city (HQ) and its rigs with resolved paths. A nil
// config still yields the HQ scope so a stray under the city root is caught.
func (c *doltServersCheck) scopeRigs() []resolverRig {
	if c.cfg == nil {
		return []resolverRig{{Name: "city", Path: c.cityPath, HQ: true}}
	}
	return loadResolverRigs(c.cityPath, c.cfg)
}

// deepestDoltScopeOwner is doltProcRigOwner with longest-root-wins. Rigs
// commonly live UNDER the city root (<city>/rigs/<name>), and the first-match
// rule doltProcRigOwner uses (safe for the reaper, where any match protects)
// would attribute every rig-local server to HQ here and report it as a stray.
func deepestDoltScopeOwner(p DoltProcInfo, rigs []resolverRig) (hq, ok bool) {
	var candidates []string
	if cfg := extractConfigPath(p.Argv); cfg != "" {
		candidates = append(candidates, cfg)
	}
	if dd, found := argvFlagValue(p.Argv); found && dd != "" {
		candidates = append(candidates, dd)
	}
	best := -1
	for _, rig := range rigs {
		root := normalizePathForCompare(strings.TrimSpace(rig.Path))
		if root == "" || root == "." || root == string(filepath.Separator) {
			continue
		}
		for _, candidate := range candidates {
			if pathUnderRoot(candidate, root) && len(root) > best {
				best = len(root)
				hq, ok = rig.HQ, true
			}
		}
	}
	return hq, ok
}

func isGCLaunchedDolt(p DoltProcInfo) bool {
	return strings.Contains(extractConfigPath(p.Argv), gcDoltConfigMarker)
}

func (c *doltServersCheck) describeAll(ps []DoltProcInfo) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		parts = append(parts, c.describe(p))
	}
	return strings.Join(parts, "; ")
}

// describe renders one server as pid, age, ports and the scope it serves. It
// prints only the --config/--data-dir paths, never the full argv: a server's
// command line can carry credentials.
func (c *doltServersCheck) describe(p DoltProcInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pid %d", p.PID)
	if age, ok := c.age(p); ok {
		fmt.Fprintf(&b, ", up %s", age)
	}
	if len(p.Ports) > 0 {
		ports := append([]int(nil), p.Ports...)
		sort.Ints(ports)
		fmt.Fprintf(&b, ", port %s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(ports)), ","), "[]"))
	}
	if cfg := extractConfigPath(p.Argv); cfg != "" {
		fmt.Fprintf(&b, ", config %s", cfg)
	} else if dd, ok := argvFlagValue(p.Argv); ok && dd != "" {
		fmt.Fprintf(&b, ", data-dir %s", dd)
	}
	return b.String()
}

// age derives uptime from the ps lstart identity (populated on hosts without
// /proc). Unparseable or absent -> no age, never a guessed one.
func (c *doltServersCheck) age(p DoltProcInfo) (string, bool) {
	s := strings.Join(strings.Fields(p.StartIdentity), " ")
	if s == "" {
		return "", false
	}
	started, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", s, time.Local)
	if err != nil {
		return "", false
	}
	d := c.now().Sub(started)
	if d < 0 {
		return "", false
	}
	return formatDoltServerAge(d), true
}

func formatDoltServerAge(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
}
