package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/supervisor"
	"gopkg.in/yaml.v3"
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
// enumeration `gc dolt cleanup` reaps from). Each server gets an identity
// path, anchored at the server's working directory when relative: its
// --data-dir (the store it actually serves), else its --config, else its
// working directory (a `bd dolt start` server carries neither flag and runs
// from its data dir; the ps-based discovery used on hosts without /proc keeps
// only --config, so a --data-dir is re-read from the full command line). A
// --config beside a --data-dir is kept only for the gc-launched marker. It is
// then sorted into:
//
//   - managed: serves this city's managed runtime layout. More than one is a
//     split-brain on the city's own store -> Error. One, on a city that is not
//     configured to run a local server, is unaccounted for -> Warning.
//   - rig-local: under a non-HQ rig root. Two on the same store is a
//     split-brain on that rig's store -> Error. On a rig configured to run NO
//     local server (see localServerPolicy) even ONE is unaccounted for: on the
//     rig's own store -> Error, elsewhere under the rig -> Warning. dolt-drift
//     finds such a server only when it wrote .dolt/sql-server.info.
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
	// args reads a process's FULL command line (/proc, else `ps -o args`).
	// Discovery's macOS ps fallback keeps only --config, so a --data-dir-only
	// server is re-read here rather than identified by its cwd.
	args func(pid int) (string, error)
	// recordedPID reads the managed runtime's pid file without side effects,
	// with the time the file was written (zero when unknown).
	recordedPID func(layout managedDoltRuntimeLayout) (int, time.Time)
	// readFile reads a server's --config YAML to find the store it names.
	readFile func(path string) ([]byte, error)
	// exactArgv reports whether a process's argv came from an exact source
	// (/proc cmdline) rather than a flattened `ps` line whose values can
	// swallow the flags after them.
	exactArgv func(pid int) bool
	// localPolicy resolves, per scope, whether the city's configuration
	// expects a local dolt server there at all.
	localPolicy func() (doltLocalPolicy, error)
	// activeTestRoots lists test roots whose owning test process is alive.
	activeTestRoots func() []string
	// startIdentity is the ps lstart fallback when discovery left it empty.
	startIdentity func(pid int) string
	homeDir       string
	tempDir       string
	now           func() time.Time

	// ids memoizes identify per Run: on hosts without /proc it can shell out.
	ids map[int]doltServerIdentity
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
		args:            processArgs,
		recordedPID:     readManagedDoltPIDFile,
		exactArgv:       argvIsExact,
		readFile:        os.ReadFile,
		localPolicy:     func() (doltLocalPolicy, error) { return localServerPolicy(cityPath, cfg) },
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
	// config is the anchored --config of a server identified by its
	// --data-dir; it decides only whether the server is gc-launched.
	config string
	// relative is set when the path was a relative --config/--data-dir whose
	// anchor (the server's cwd) could not be read: unidentifiable, not "not gc".
	relative bool
	// configUnread is set when a config-only server's YAML could not be read,
	// so the store it serves is unknown. Such a server is never counted as
	// "not gc": its config may name this city's store.
	configUnread bool
}

func (c *doltServersCheck) identify(p DoltProcInfo) doltServerIdentity {
	if id, ok := c.ids[p.PID]; ok {
		return id
	}
	id := c.identifyUncached(p)
	if c.ids != nil {
		c.ids[p.PID] = id
	}
	return id
}

func (c *doltServersCheck) identifyUncached(p DoltProcInfo) doltServerIdentity {
	dd, hasDD := argvFlagValue(p.Argv)
	if !hasDD && c.args != nil {
		// The ps fallback kept only --config (parseDoltPSCommandLine), so a
		// --data-dir would otherwise be lost. Re-read the full command line
		// here; the shared parser stays as is because `gc dolt cleanup` reaps
		// by what it returns.
		if full, err := c.args(p.PID); err == nil {
			if v := flattenedFlagValue(full, "--data-dir"); v != "" {
				dd, hasDD = v, true
			}
		}
	}
	trim := func(v string) string { return v }
	if c.exactArgv == nil || !c.exactArgv(p.PID) {
		trim = trimFlattenedDoltArgs
	}
	cfg := trim(extractConfigPath(p.Argv))
	if hasDD && dd != "" {
		// The data dir is the store served; a --config beside it does not
		// override that. An unanchorable relative data dir stays
		// unidentifiable rather than falling back to the config.
		id := c.anchored(p.PID, trim(dd), "data-dir")
		if cfg != "" {
			id.config = c.anchored(p.PID, cfg, "config").path
		}
		return id
	}
	if cfg != "" {
		id := c.anchored(p.PID, cfg, "config")
		// The config names the store: gc's own writer makes its data_dir
		// authoritative (cmd_dolt_config.go). A copied config pointing at this
		// city's store is that store's server, whatever the file is called.
		dd, resolved := c.configDataDir(id.path)
		if dd != "" {
			// A relative data_dir with an unreadable cwd stays unanchored
			// (a Warning), never the config filename.
			ddID := c.anchored(p.PID, dd, "data-dir")
			ddID.config = id.path
			return ddID
		}
		id.configUnread = !resolved
		return id
	}
	if cwd, ok := c.cwd(p.PID); ok && cwd != "" {
		return doltServerIdentity{path: cwd, source: "cwd"}
	}
	return doltServerIdentity{}
}

// configDataDir reads the top-level data_dir from a dolt config YAML with the
// YAML parser, so anchors, aliases and block scalars resolve as dolt would read
// them. A config naming no data_dir returns ("", true) and the config path
// stays the identity. resolved is false when the file cannot be read (deleted
// or unreadable since startup) or cannot be decoded, or data_dir is not a
// string: the store the server was started on is then unknown.
func (c *doltServersCheck) configDataDir(configPath string) (dataDir string, resolved bool) {
	if configPath == "" || c.readFile == nil {
		return "", true
	}
	data, err := c.readFile(configPath)
	if err != nil {
		return "", false
	}
	var doc struct {
		DataDir *string `yaml:"data_dir"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", false
	}
	if doc.DataDir == nil {
		return "", true
	}
	return strings.TrimSpace(*doc.DataDir), true
}

// flattenedFlagValue recovers a flag's value from a flat command line
// (`ps -o args`), where a path may contain spaces. The value runs to the next
// " -" flag boundary, the same cut trimFlattenedDoltArgs applies to --config.
func flattenedFlagValue(args, flag string) string {
	for from := 0; from < len(args); {
		j := strings.Index(args[from:], flag)
		if j < 0 {
			return ""
		}
		j += from
		end := j + len(flag)
		if (j == 0 || args[j-1] == ' ') && end < len(args) && (args[end] == ' ' || args[end] == '=') {
			v := strings.TrimLeft(args[end+1:], " ")
			if strings.HasPrefix(v, "-") {
				return ""
			}
			return strings.TrimSpace(trimFlattenedDoltArgs(v))
		}
		from = end
	}
	return ""
}

// anchored resolves a --config/--data-dir value the way the SERVER did: a
// relative path is relative to the server's working directory, not doctor's.
// Unreadable cwd -> unidentifiable (a Warning), never classified as "not gc".
func (c *doltServersCheck) anchored(pid int, path, source string) doltServerIdentity {
	if filepath.IsAbs(path) {
		return doltServerIdentity{path: path, source: source}
	}
	cwd, ok := c.cwd(pid)
	if !ok || cwd == "" {
		return doltServerIdentity{source: source, relative: true}
	}
	return doltServerIdentity{path: filepath.Join(cwd, path), source: source}
}

func (c *doltServersCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	c.ids = map[int]doltServerIdentity{}

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

	var managed, strays, foreign, orphanTests, unidentified, unanchored, unreadConfig []DoltProcInfo
	rigLocal := map[string][]DoltProcInfo{}
	rigRootOf := map[string]string{}
	var rigLocalCount, otherCity, activeTests, notGC int
	for _, p := range procs {
		id := c.identify(p)
		switch {
		case id.path == "" && id.relative:
			unanchored = append(unanchored, p)
		case id.path == "":
			unidentified = append(unidentified, p)
		case layoutErr == nil && servesDoltLayout(id, layout):
			managed = append(managed, p)
		default:
			root, hq, ok := deepestDoltScopeOwner(id.path, cityScopes)
			switch {
			// An unresolved config may have named this city's store, so no
			// path-based ownership may account for it; only a match to a
			// rig's own layout (above: the managed one) identifies it.
			case id.configUnread && (!ok || hq || !strings.HasSuffix(c.rigStoreKey(root, id), rigStoreKeySuffix)):
				unreadConfig = append(unreadConfig, p)
			case ok && !hq:
				key := c.rigStoreKey(root, id)
				rigLocal[key] = append(rigLocal[key], p)
				rigRootOf[key] = root
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
			case strings.Contains(id.configPath(), gcDoltConfigMarker):
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
	var policy doltLocalPolicy
	var policyErr error
	if (len(rigKeys) > 0 || len(managed) > 0) && c.localPolicy != nil {
		policy, policyErr = c.localPolicy()
	}
	unchecked := 0
	if len(managed) == 1 && c.localPolicy != nil {
		switch {
		case policyErr != nil:
			unchecked++
		case !policy.city.expectsLocal:
			warns = append(warns, fmt.Sprintf(
				"dolt server on this city's managed store, but the city runs no local server (%s): %s",
				policy.city.reason, c.describe(managed[0])))
		}
	}
	for _, k := range rigKeys {
		group := rigLocal[k]
		if len(group) > 1 {
			errs = append(errs, fmt.Sprintf(
				"%d dolt servers serve one rig store — split-brain; at most one may: %s",
				len(group), c.describeAll(group)))
			continue
		}
		if c.localPolicy == nil {
			continue
		}
		if policyErr != nil {
			unchecked++
			continue
		}
		rp, ok := policy.rigs[rigRootOf[k]]
		if !ok {
			continue
		}
		if rp.expectsLocal {
			// A --self rig accounts for ONE server: the one on its own store.
			// A server elsewhere under the rig is not that server.
			if !strings.HasSuffix(k, rigStoreKeySuffix) {
				warns = append(warns, fmt.Sprintf(
					"dolt server under rig %q that does not serve the rig's configured store: %s",
					rp.name, c.describeAll(group)))
			}
			continue
		}
		if strings.HasSuffix(k, rigStoreKeySuffix) {
			errs = append(errs, fmt.Sprintf(
				"rig %q runs no local dolt server (%s), but one serves its store — split-brain with the store it is configured for: %s",
				rp.name, rp.reason, c.describeAll(group)))
		} else {
			warns = append(warns, fmt.Sprintf(
				"dolt server under rig %q, which runs no local server (%s): %s",
				rp.name, rp.reason, c.describeAll(group)))
		}
	}
	if unchecked > 0 {
		warns = append(warns, fmt.Sprintf(
			"endpoint configuration unresolvable (%v); %d single local dolt server(s) not checked against whether their scope should run one",
			policyErr, unchecked))
	}
	var pidNotes []string
	if layoutErr == nil {
		if pid, written := c.recordedPID(layout); pid > 0 && !doltPIDIn(pid, managed) {
			if p, ok := doltProcByPID(pid, procs); ok {
				// The pid file survives crashes, and pids are reused: only a
				// process that was already running when the file was written
				// can be the one that wrote it.
				started, known := c.startedAt(p)
				switch {
				case !known || written.IsZero():
					pidNotes = append(pidNotes, fmt.Sprintf("runtime-recorded managed dolt pid %d is a dolt server outside the managed layout, but its start time or the pid file's age is unknown; not judged", pid))
				case started.After(written.Add(time.Second)):
					pidNotes = append(pidNotes, fmt.Sprintf("runtime pid file is stale: pid %d now belongs to a dolt server started after the file was written", pid))
				default:
					warns = append(warns, fmt.Sprintf(
						"the runtime-recorded managed dolt pid %d does not match the managed layout; a duplicate of it would not be detected", pid))
				}
			}
		}
	}
	for _, p := range unidentified {
		warns = append(warns, "dolt server with no --config, no --data-dir and an unreadable working directory (cannot rule out this city's store): "+c.describe(p))
	}
	for _, p := range unanchored {
		warns = append(warns, "dolt server with a relative --config/--data-dir and an unreadable working directory to resolve it against (cannot rule out this city's store): "+c.describe(p))
	}
	for _, p := range unreadConfig {
		warns = append(warns, "dolt server whose --config can no longer be read or decoded, so the store it serves is unknown (cannot rule out this city's store): "+c.describe(p))
	}
	for _, p := range strays {
		warns = append(warns, "dolt server under the city root that is not the managed server: "+c.describe(p))
	}
	for _, p := range foreign {
		warns = append(warns, "gc-launched dolt server that belongs to no registered city or rig: "+c.describe(p))
	}
	if len(unidentified)+len(unanchored)+len(unreadConfig)+len(strays)+len(foreign) > 0 {
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
	details = append(details, pidNotes...)
	details = append(details, fmt.Sprintf(
		"%d dolt sql-server process(es) on host: %d managed, %d rig-local, %d city-stray, %d other registered city, %d active test, %d orphan test, %d foreign gc-launched, %d not gc-launched, %d unidentifiable",
		len(procs), len(managed), rigLocalCount, len(strays), otherCity, activeTests, len(orphanTests), len(foreign), notGC, len(unidentified)+len(unanchored)+len(unreadConfig)))

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

// servesDoltLayout reports whether a server serves the store a layout
// describes, judged ONLY by its anchored identity: a raw argv value would be
// resolved against doctor's working directory, not the server's. A data dir,
// when known, decides alone; a config is compared only when there is none.
func servesDoltLayout(id doltServerIdentity, layout managedDoltRuntimeLayout) bool {
	switch id.source {
	case "config":
		return strings.TrimSpace(layout.ConfigFile) != "" && samePath(id.path, layout.ConfigFile)
	case "data-dir", "cwd":
		return strings.TrimSpace(layout.DataDir) != "" && samePath(id.path, layout.DataDir)
	}
	return false
}

// configPath is the server's anchored --config, whichever identity it has.
func (id doltServerIdentity) configPath() string {
	if id.source == "config" {
		return id.path
	}
	return id.config
}

// rigStoreKey groups rig-local servers by the store they serve, so a gc-launched
// server (identified by its config) and a `bd dolt start` server (identified by
// its cwd) on the same rig store land in ONE group and count as a split-brain.
// A server that does not serve the rig's own layout keys on its identity path,
// which is its data dir whenever one is known.
func (c *doltServersCheck) rigStoreKey(root string, id doltServerIdentity) string {
	if layout, err := c.layout(root); err == nil && servesDoltLayout(id, layout) {
		return root + rigStoreKeySuffix
	}
	return root + "\x00" + normalizePathForCompare(id.path)
}

const rigStoreKeySuffix = "\x00store"

// doltLocalPolicy says, per scope, whether the city's configuration expects
// a local dolt server there. Rigs are keyed by the normalized root
// deepestDoltScopeOwner returns.
type doltLocalPolicy struct {
	city doltScopeLocal
	rigs map[string]doltScopeLocal
}

type doltScopeLocal struct {
	name         string
	expectsLocal bool
	reason       string // why no local server is expected; "" when one is
}

// localServerPolicy resolves each scope's store provider and endpoint origin
// the way dolt-drift does. A local server is expected only for a bd-store
// city whose origin is managed_city, and a bd-store rig with an explicit
// endpoint on a local host (`gc rig set-endpoint --self`). Everything else —
// file-backed, city_canonical, inherited_city, explicit external — runs none.
func localServerPolicy(cityPath string, cfg *config.City) (doltLocalPolicy, error) {
	pol := doltLocalPolicy{rigs: map[string]doltScopeLocal{}}
	if cfg == nil {
		// No clean city.toml: whether any scope should run a local server is
		// unknown, and assuming "yes" would read a stale server as expected.
		return doltLocalPolicy{}, fmt.Errorf("city config unavailable")
	}
	cityState, _, err := resolveDesiredCityEndpointState(cityPath, cfg.Dolt, config.EffectiveHQPrefix(cfg))
	if err != nil {
		return doltLocalPolicy{}, fmt.Errorf("resolve city endpoint state: %w", err)
	}
	pol.city = doltScopeLocal{name: "city", expectsLocal: true}
	switch {
	case !scopeUsesManagedBdStoreContract(cityPath, cityPath):
		pol.city = doltScopeLocal{name: "city", reason: "the city store is not bd/dolt"}
	case scopeBackendIsDoltlite(cityPath, cityPath):
		pol.city = doltScopeLocal{name: "city", reason: "the city store backend is doltlite"}
	case cityState.EndpointOrigin != contract.EndpointOriginManagedCity:
		pol.city = doltScopeLocal{name: "city", reason: "city endpoint origin " + string(cityState.EndpointOrigin)}
	}
	rigs := make([]config.Rig, len(cfg.Rigs))
	copy(rigs, cfg.Rigs)
	resolveRigPaths(cityPath, rigs)
	for _, rig := range rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		root := normalizePathForCompare(strings.TrimSpace(rig.Path))
		if !rigUsesManagedBdStoreContract(cityPath, rig) {
			pol.rigs[root] = doltScopeLocal{name: rig.Name, reason: "the rig store is not bd/dolt"}
			continue
		}
		if scopeBackendIsDoltlite(cityPath, rig.Path) {
			pol.rigs[root] = doltScopeLocal{name: rig.Name, reason: "the rig store backend is doltlite"}
			continue
		}
		st, err := resolveDesiredRigEndpointState(cityPath, rig, cityState)
		if err != nil {
			return doltLocalPolicy{}, fmt.Errorf("rig %q: %w", rig.Name, err)
		}
		switch {
		case st.EndpointOrigin == contract.EndpointOriginExplicit && contract.DoltHostIsLocal(st.DoltHost):
			pol.rigs[root] = doltScopeLocal{name: rig.Name, expectsLocal: true}
		case st.EndpointOrigin == contract.EndpointOriginExplicit:
			pol.rigs[root] = doltScopeLocal{name: rig.Name, reason: "explicit endpoint on " + st.DoltHost}
		default:
			pol.rigs[root] = doltScopeLocal{name: rig.Name, reason: "endpoint origin " + string(st.EndpointOrigin)}
		}
	}
	return pol, nil
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
	_, ok := doltProcByPID(pid, procs)
	return ok
}

func doltProcByPID(pid int, procs []DoltProcInfo) (DoltProcInfo, bool) {
	for _, p := range procs {
		if p.PID == pid {
			return p, true
		}
	}
	return DoltProcInfo{}, false
}

// argvIsExact reports whether pid's argv is readable from /proc, the source
// discovery uses when present; elsewhere argv came from a flattened ps line.
func argvIsExact(pid int) bool {
	_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	return err == nil
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

// readManagedDoltPIDFile reads the managed runtime's pid file and when it was
// written. Unlike managedPIDFromPIDFile it never removes a stale file: doctor
// is read-only.
func readManagedDoltPIDFile(layout managedDoltRuntimeLayout) (int, time.Time) {
	if strings.TrimSpace(layout.PIDFile) == "" {
		return 0, time.Time{}
	}
	data, err := os.ReadFile(layout.PIDFile)
	if err != nil {
		return 0, time.Time{}
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, time.Time{}
	}
	var written time.Time
	if st, err := os.Stat(layout.PIDFile); err == nil {
		written = st.ModTime()
	}
	return pid, written
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
		fmt.Fprintf(&b, ", %s %s", id.source, id.path)
	}
	return b.String()
}

// trimFlattenedDoltArgs cuts a value recovered from a flattened ps line at the
// first RECOGNIZED dolt sql-server flag after it (`--config x.yaml -u root -p
// <password>`). The ps parser already cuts at " --"; the short flags are the
// ones that survive it. A bare " -" is not a boundary: "/srv/Gas - City" is a
// path. Cut before classification, not only before display: the tail both
// defeats a path match against the managed layout and must never reach doctor
// output. Values from an exact /proc argv are never passed through here.
func trimFlattenedDoltArgs(path string) string {
	for i := 0; i+1 < len(path); i++ {
		if path[i] != ' ' || path[i+1] != '-' {
			continue
		}
		tok := path[i+1:]
		if j := strings.IndexAny(tok, " ="); j >= 0 {
			tok = tok[:j]
		}
		if doltSQLServerFlags[tok] || (strings.HasPrefix(tok, "--") && len(tok) > 2) {
			return strings.TrimRight(path[:i], " ")
		}
	}
	return path
}

// doltSQLServerFlags are the short flags of `dolt sql-server`; any "--word"
// is treated as a boundary as well.
var doltSQLServerFlags = map[string]bool{
	"-H": true, "-P": true, "-u": true, "-p": true, "-t": true, "-r": true, "-l": true,
}

// startedAt parses the ps lstart identity. Absent or unparseable -> unknown.
func (c *doltServersCheck) startedAt(p DoltProcInfo) (time.Time, bool) {
	s := p.StartIdentity
	if s == "" && c.startIdentity != nil {
		s = c.startIdentity(p.PID)
	}
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return time.Time{}, false
	}
	started, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", s, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return started, true
}

// age derives uptime from the ps lstart identity. Unparseable or absent -> no
// age, never a guessed one.
func (c *doltServersCheck) age(p DoltProcInfo) (string, bool) {
	started, ok := c.startedAt(p)
	if !ok {
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
