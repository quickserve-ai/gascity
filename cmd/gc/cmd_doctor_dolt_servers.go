package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/supervisor"
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
// enumeration `gc dolt cleanup` reaps from). Each server gets ONE identity
// path: its --config, else its --data-dir, else its working directory (a
// `bd dolt start` server carries neither flag and runs from its data dir; and
// the ps-based discovery used on hosts without /proc keeps only --config). It
// is then sorted into:
//
//   - managed: serves this city's managed runtime layout. More than one is a
//     split-brain on the city's own store -> Error.
//   - rig-local: under a non-HQ rig root. Two on the same identity is a
//     split-brain on that rig's store -> Error; one is dolt-drift's call.
//   - city-stray: under the city root, not the managed layout -> Warning.
//   - other-city: under a city or rig registered with the supervisor on this
//     host (~/.gc/cities.toml). Counted: a multi-city host runs one managed
//     server per city by design.
//   - test: on the test-path allowlist `gc dolt cleanup` reaps from. Counted
//     while its test root is active; an orphan -> Warning, cleanup's job.
//   - foreign: gc-launched (a .gc/runtime/packs/dolt/ config), no registered
//     scope -> Warning. Nothing reaps these; the reaper protects them.
//   - not gc-launched: counted in Details only.
//   - unidentifiable: no path at all and no readable cwd -> Warning, because
//     it could be serving this city's store and the check cannot rule it out.
//
// It never reports OK without having looked: an enumeration failure is
// StatusSkipped, and every partial-assessment path is a Warning. Ports come
// from best-effort lsof on hosts without /proc; they are displayed, never used
// to decide ownership. The check is read-only — it removes no pid file.
type doltServersCheck struct {
	cityPath string
	cfg      *config.City

	discover func() ([]DoltProcInfo, error)
	layout   func(cityPath string) (managedDoltRuntimeLayout, error)
	// otherScopes returns the roots of every city and rig registered with
	// the supervisor on this host, excluding this city.
	otherScopes func(cityPath string) ([]string, error)
	// cwd resolves a process's working directory; ok=false when unreadable.
	cwd func(pid int) (string, bool)
	// recordedPID reads the managed runtime's pid file without side effects.
	recordedPID func(layout managedDoltRuntimeLayout) int
	// activeTestRoots lists test roots whose owning test process is alive.
	activeTestRoots func() []string
	// startIdentity is the ps lstart fallback when discovery left it empty.
	startIdentity func(pid int) string
	homeDir       string
	tempDir       string
	now           func() time.Time
}

var _ doctor.Check = (*doltServersCheck)(nil)

func newDoltServersCheck(cityPath string, cfg *config.City) *doltServersCheck {
	home, _ := os.UserHomeDir()
	temp := os.TempDir()
	return &doltServersCheck{
		cityPath: cityPath,
		cfg:      cfg,
		discover: discoverDoltProcesses,
		// Strict: resolve the layout from cityPath alone. A seat's ambient
		// GC_DOLT_* env can describe another city, and honoring it would let
		// this check call that city's server "managed" here.
		layout:          resolveManagedDoltRuntimeLayoutStrict,
		otherScopes:     registeredScopeRootsExcept,
		cwd:             processCWD,
		recordedPID:     readManagedDoltPIDFile,
		activeTestRoots: func() []string { return discoverActiveTestRoots(home, temp) },
		startIdentity:   readProcStartIdentity,
		homeDir:         home,
		tempDir:         temp,
		now:             time.Now,
	}
}

func (c *doltServersCheck) Name() string { return "dolt-servers" }

// CanFix is false: every server this check flags is one gc cannot prove it owns
// well enough to kill. `gc dolt cleanup` is the reaper.
func (c *doltServersCheck) CanFix() bool { return false }

func (c *doltServersCheck) Fix(*doctor.CheckContext) error { return nil }

func (c *doltServersCheck) WarmupEligible() bool { return false }

// gcDoltConfigMarker is the path segment every gc-launched dolt server's
// --config carries (<scope>/.gc/runtime/packs/dolt/dolt-config.yaml).
var gcDoltConfigMarker = filepath.Join(".gc", "runtime", "packs", "dolt") + string(filepath.Separator)

type doltServerIdentity struct {
	path   string // config, data-dir, or cwd; "" when none is known
	source string // "config", "data-dir", "cwd"
}

func (c *doltServersCheck) identify(p DoltProcInfo) doltServerIdentity {
	if cfg := extractConfigPath(p.Argv); cfg != "" {
		return doltServerIdentity{path: cfg, source: "config"}
	}
	if dd, ok := argvFlagValue(p.Argv); ok && dd != "" {
		return doltServerIdentity{path: dd, source: "data-dir"}
	}
	if cwd, ok := c.cwd(p.PID); ok && cwd != "" {
		return doltServerIdentity{path: cwd, source: "cwd"}
	}
	return doltServerIdentity{}
}

func (c *doltServersCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}

	procs, err := c.discover()
	if err != nil {
		r.Status = doctor.StatusSkipped
		r.Message = fmt.Sprintf("could not enumerate dolt sql-server processes: %v — orphan and duplicate analysis NOT performed", err)
		return r
	}

	layout, layoutErr := c.layout(c.cityPath)
	cityScopes := c.scopeRigs()
	others, othersErr := c.otherScopes(c.cityPath)
	otherScopes := make([]resolverRig, 0, len(others))
	for _, root := range others {
		otherScopes = append(otherScopes, resolverRig{Path: root})
	}
	var testRoots []string
	testRootsLoaded := false

	var managed, strays, foreign, orphanTests, unidentified []DoltProcInfo
	rigLocal := map[string][]DoltProcInfo{}
	var rigLocalCount, otherCity, activeTests, notGC int
	for _, p := range procs {
		id := c.identify(p)
		switch {
		case id.path == "":
			unidentified = append(unidentified, p)
		case layoutErr == nil && c.servesManagedLayout(p, id, layout):
			managed = append(managed, p)
		default:
			root, hq, ok := deepestDoltScopeOwner(id.path, cityScopes)
			switch {
			case ok && !hq:
				key := root + "\x00" + normalizePathForCompare(id.path)
				rigLocal[key] = append(rigLocal[key], p)
				rigLocalCount++
			case ok && hq:
				strays = append(strays, p)
			case doltPathUnderAny(id.path, otherScopes):
				otherCity++
			case isTestConfigPath(id.path, c.homeDir, c.tempDir):
				if !testRootsLoaded {
					testRoots, testRootsLoaded = c.activeTestRoots(), true
				}
				if configUnderActiveTestRoot(id.path, testRoots) {
					activeTests++
				} else {
					orphanTests = append(orphanTests, p)
				}
			case id.source == "config" && strings.Contains(id.path, gcDoltConfigMarker):
				foreign = append(foreign, p)
			default:
				notGC++
			}
		}
	}

	var errs, warns, hints []string
	if len(managed) > 1 {
		errs = append(errs, fmt.Sprintf(
			"%d dolt servers serve this city's managed store — split-brain; at most one may: %s",
			len(managed), c.describeAll(managed)))
	}
	rigKeys := make([]string, 0, len(rigLocal))
	for k := range rigLocal {
		rigKeys = append(rigKeys, k)
	}
	sort.Strings(rigKeys)
	for _, k := range rigKeys {
		if group := rigLocal[k]; len(group) > 1 {
			errs = append(errs, fmt.Sprintf(
				"%d dolt servers serve one rig store — split-brain; at most one may: %s",
				len(group), c.describeAll(group)))
		}
	}
	if layoutErr == nil {
		if pid := c.recordedPID(layout); pid > 0 && !doltPIDIn(pid, managed) && doltPIDIn(pid, procs) {
			warns = append(warns, fmt.Sprintf(
				"the runtime-recorded managed dolt pid %d does not match the managed layout; a duplicate of it would not be detected", pid))
		}
	}
	for _, p := range unidentified {
		warns = append(warns, "dolt server with no --config, no --data-dir and an unreadable working directory (cannot rule out this city's store): "+c.describe(p))
	}
	for _, p := range strays {
		warns = append(warns, "dolt server under the city root that is not the managed server: "+c.describe(p))
	}
	for _, p := range foreign {
		warns = append(warns, "gc-launched dolt server that belongs to no registered city or rig: "+c.describe(p))
	}
	if len(unidentified)+len(strays)+len(foreign) > 0 {
		hints = append(hints, "confirm the owning scope is no longer in use, then stop the server (`kill <pid>`); `gc dolt cleanup` deliberately protects these")
	}
	for _, p := range orphanTests {
		warns = append(warns, "test dolt server whose test is no longer running: "+c.describe(p))
	}
	if len(orphanTests) > 0 {
		hints = append(hints, "`gc dolt cleanup` reaps orphaned test servers")
	}

	var details []string
	if othersErr != nil {
		details = append(details, fmt.Sprintf("supervisor registry unreadable (%v); servers of other registered cities may be listed as foreign", othersErr))
	}
	if layoutErr != nil {
		details = append(details, fmt.Sprintf("managed dolt layout unresolvable (%v); duplicate-managed analysis skipped", layoutErr))
	}
	details = append(details, fmt.Sprintf(
		"%d dolt sql-server process(es) on host: %d managed, %d rig-local, %d city-stray, %d other registered city, %d active test, %d orphan test, %d foreign gc-launched, %d not gc-launched, %d unidentifiable",
		len(procs), len(managed), rigLocalCount, len(strays), otherCity, activeTests, len(orphanTests), len(foreign), notGC, len(unidentified)))

	switch {
	case len(errs) > 0:
		r.Status = doctor.StatusError
		r.Message = errs[0]
		r.Details = append(append(append([]string{}, errs[1:]...), warns...), details...)
		r.FixHint = strings.Join(append([]string{"`gc dolt status` names the server the city runtime recorded; stop the others only after confirming none is mid-write — doctor will not kill them"}, hints...), "; ")
	case len(warns) > 0:
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("%d dolt server finding(s) this city does not account for", len(warns))
		r.Details = append(append([]string{}, warns...), details...)
		r.FixHint = strings.Join(hints, "; ")
	case layoutErr != nil:
		// Nothing extra seen, but the one comparison that detects a
		// split-brain on the city's own store never ran. Not OK.
		r.Status = doctor.StatusWarning
		r.Message = "no unaccounted dolt servers found, but the managed layout was unresolvable so duplicate-managed analysis did not run"
		r.Details = details
	default:
		r.Status = doctor.StatusOK
		r.Message = fmt.Sprintf("every dolt server on this host is accounted for (%d managed, %d rig-local)", len(managed), rigLocalCount)
		r.Details = details
	}
	return r
}

// servesManagedLayout matches by argv (doltProcMatchesManagedLayout) or, for a
// server identified only by its working directory, by that directory being
// the managed data dir — the shape `bd dolt start` produces.
func (c *doltServersCheck) servesManagedLayout(p DoltProcInfo, id doltServerIdentity, layout managedDoltRuntimeLayout) bool {
	if doltProcMatchesManagedLayout(p, layout) {
		return true
	}
	return id.source == "cwd" && strings.TrimSpace(layout.DataDir) != "" && samePath(id.path, layout.DataDir)
}

// scopeRigs returns the city (HQ) and its rigs with resolved paths. A nil
// config still yields the HQ scope so a stray under the city root is caught.
func (c *doltServersCheck) scopeRigs() []resolverRig {
	if c.cfg == nil {
		return []resolverRig{{Name: "city", Path: c.cityPath, HQ: true}}
	}
	return loadResolverRigs(c.cityPath, c.cfg)
}

// deepestDoltScopeOwner returns the normalized root of the deepest scope that
// contains path. Rigs commonly live UNDER the city root (<city>/rigs/<name>),
// so the first-match rule doltProcRigOwner uses (safe for the reaper, where
// any match protects) would attribute every rig-local server to HQ here and
// report it as a stray.
func deepestDoltScopeOwner(path string, scopes []resolverRig) (root string, hq, ok bool) {
	for _, s := range scopes {
		r := normalizePathForCompare(strings.TrimSpace(s.Path))
		if r == "" || r == "." || r == string(filepath.Separator) {
			continue
		}
		if pathUnderRoot(path, r) && len(r) > len(root) {
			root, hq, ok = r, s.HQ, true
		}
	}
	return root, hq, ok
}

func doltPathUnderAny(path string, scopes []resolverRig) bool {
	_, _, ok := deepestDoltScopeOwner(path, scopes)
	return ok
}

func doltPIDIn(pid int, procs []DoltProcInfo) bool {
	for _, p := range procs {
		if p.PID == pid {
			return true
		}
	}
	return false
}

// registeredScopeRootsExcept lists the supervisor registry's city and rig
// roots, minus cityPath itself.
func registeredScopeRootsExcept(cityPath string) ([]string, error) {
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	cities, err := reg.List()
	if err != nil {
		return nil, fmt.Errorf("reading city registry: %w", err)
	}
	rigs, err := reg.ListRigs()
	if err != nil {
		return nil, fmt.Errorf("reading rig registry: %w", err)
	}
	self := normalizePathForCompare(cityPath)
	var roots []string
	for _, e := range cities {
		if normalizePathForCompare(e.Path) != self {
			roots = append(roots, e.Path)
		}
	}
	for _, e := range rigs {
		roots = append(roots, e.Path)
	}
	return roots, nil
}

// processCWD reads a process's working directory from /proc, else lsof.
func processCWD(pid int) (string, bool) {
	if cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd")); err == nil {
		return cwd, true
	}
	return processCWDFromLsof(pid)
}

// readManagedDoltPIDFile reads the managed runtime's pid file. Unlike
// managedPIDFromPIDFile it never removes a stale file: doctor is read-only.
func readManagedDoltPIDFile(layout managedDoltRuntimeLayout) int {
	if strings.TrimSpace(layout.PIDFile) == "" {
		return 0
	}
	data, err := os.ReadFile(layout.PIDFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func (c *doltServersCheck) describeAll(ps []DoltProcInfo) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		parts = append(parts, c.describe(p))
	}
	return strings.Join(parts, "; ")
}

// describe renders one server as pid, age, ports and the path it is
// identified by — never the full argv, which can carry credentials.
func (c *doltServersCheck) describe(p DoltProcInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pid %d", p.PID)
	if age, ok := c.age(p); ok {
		fmt.Fprintf(&b, ", up %s", age)
	}
	if len(p.Ports) > 0 {
		ports := append([]int(nil), p.Ports...)
		sort.Ints(ports)
		strs := make([]string, len(ports))
		for i, port := range ports {
			strs[i] = strconv.Itoa(port)
		}
		fmt.Fprintf(&b, ", port %s", strings.Join(strs, ","))
	}
	if id := c.identify(p); id.path != "" {
		fmt.Fprintf(&b, ", %s %s", id.source, displayDoltPath(id.path))
	}
	return b.String()
}

// displayDoltPath cuts a path at the first " -". On hosts without /proc the
// --config value is recovered from a flat ps command line and can swallow the
// flags after it (`--config x.yaml -u root -p <password>`); those must never
// reach doctor output.
func displayDoltPath(path string) string {
	if i := strings.Index(path, " -"); i >= 0 {
		return path[:i]
	}
	return path
}

// age derives uptime from the ps lstart identity. Unparseable or absent -> no
// age, never a guessed one.
func (c *doltServersCheck) age(p DoltProcInfo) (string, bool) {
	s := p.StartIdentity
	if s == "" && c.startIdentity != nil {
		s = c.startIdentity(p.PID)
	}
	s = strings.Join(strings.Fields(s), " ")
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
