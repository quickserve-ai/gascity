package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// THE SYNTHESIZED-ENDPOINT GUARD (ga-bb7rzy).
//
// A Dolt endpoint is TWO independent variables — host and port — and each is
// resolved by its own ladder that is allowed to bottom out in a DIFFERENT
// source. gc projects them as separate env keys; beads resolves them separately
// again (internal/storage/dolt/store.go host at BEADS_DOLT_SERVER_HOST-or-
// 127.0.0.1, port at BEADS_DOLT_SERVER_PORT-or-the-.beads-port-file). Nothing in
// either ladder requires the two halves to have come from the same scope, so any
// partial projection can splice them into a pair no scope declares.
//
// 2026-09-02 22:28Z: a qcore-scoped `gc hook --claim` federated onto the CITY
// store (cmd_hook.go appendCityHookStore). The city projection deleted the host
// key (managed-local loopback needs no host) and set the port to the city's
// managed 51361; the bd child-env overlay onto os.Environ() then refilled the
// deleted host from the witness session's rig projection — 100.71.23.94. bd
// dialed 100.71.23.94:51361 and spent 48.27s being refused, blocking two agents.
// The endpoint appeared in no file and in no session env: it existed only inside
// the process, after the merge.
//
// The primary fix is upstream in cmd/gc/bd_env.go, which now records a scope's
// "no host here" as PRESENT AND EMPTY so the overlay cannot resurrect a parent
// value. This guard is the second line, and it is placed at the LAST point
// before exec where the FULLY MERGED child environment exists — the only place
// the splice is observable, since neither half is wrong on its own.
//
// PLACEMENT: execCommandRunnerWithEnv's closure (bdstore.go), which builds
// cmd.Env for every child launched through a BdStore CommandRunner. That is
// exactly the incident path: gc hook --claim -> hookClaimWithBdStore ->
// hookClaimBdStoreContext -> beads.NewBdStore(dir,
// hookClaimCommandRunnerWithEnvContext(ctx, hookClaimEnvMap(env, dir, actor)))
// -> ExecCommandRunnerWithEnvContext -> this closure. It also covers the store
// bridge, libstore, provider lifecycle and t3bridge, which reach bd through the
// same runner.
//
// The predicate is deliberately narrow, so a legitimate endpoint can never be
// refused: a non-loopback host, on the port the city's OWN managed-local Dolt is
// serving, that NO scope in play declares. A rig that genuinely points at a
// remote host is declared in its .beads/config.yaml and is allowed even when its
// port number happens to collide with the local managed port.

// managedDoltRuntimeState mirrors the fields
// contract.readManagedRuntimeState reads from the published runtime state. It is
// duplicated rather than exported so the guard stays a leaf with no new
// dependency on contract's unexported resolution.
type managedDoltRuntimeState struct {
	Running bool   `json:"running"`
	Port    int    `json:"port"`
	DataDir string `json:"data_dir"`
}

// lastEnvValue returns the value of key in an environ slice, taking the LAST
// occurrence so it agrees with the overlay builders (mergeEnv appends the
// override after removing prior entries) and with exec's last-wins behavior.
func lastEnvValue(environ []string, key string) string {
	prefix := key + "="
	out := ""
	for _, entry := range environ {
		if strings.HasPrefix(entry, prefix) {
			out = strings.TrimPrefix(entry, prefix)
		}
	}
	return strings.TrimSpace(out)
}

// firstNonEmptyEnv returns the first key in keys that has a non-empty value,
// with the key name, so an error can name where a half came from.
func firstNonEmptyEnv(environ []string, keys ...string) (value, key string) {
	for _, k := range keys {
		if v := lastEnvValue(environ, k); v != "" {
			return v, k
		}
	}
	return "", ""
}

// managedCityDoltPort returns the port the city's OWN managed-local Dolt server
// is serving, and the file that said so. It returns "" when the city runs no
// managed server, when the published state is not this city's (the data dir must
// live under the city's .beads), or when nothing can be read — the guard then
// stays silent rather than guessing.
func managedCityDoltPort(cityPath string) (port, source string) {
	cityPath = strings.TrimSpace(cityPath)
	if cityPath == "" {
		return "", ""
	}
	statePath := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	if data, err := os.ReadFile(statePath); err == nil {
		var state managedDoltRuntimeState
		if err := json.Unmarshal(data, &state); err == nil && state.Running && state.Port > 0 {
			wantDir := filepath.Clean(filepath.Join(cityPath, ".beads", "dolt"))
			if filepath.Clean(strings.TrimSpace(state.DataDir)) == wantDir {
				return fmt.Sprintf("%d", state.Port), statePath
			}
		}
	}
	portPath := filepath.Join(cityPath, ".beads", "dolt-server.port")
	if data, err := os.ReadFile(portPath); err == nil {
		if p := strings.TrimSpace(string(data)); p != "" {
			return p, portPath
		}
	}
	return "", ""
}

// guardScopeRoots returns the scope roots whose declarations could legitimately
// name host, most specific first: the store the child was pointed at, then the
// city. BEADS_DIR names a .beads directory, so its parent is the scope root.
func guardScopeRoots(environ []string, dir, cityPath string) []string {
	var roots []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		for _, existing := range roots {
			if existing == p {
				return
			}
		}
		roots = append(roots, p)
	}
	if beadsDir := lastEnvValue(environ, "BEADS_DIR"); beadsDir != "" {
		add(filepath.Dir(beadsDir))
	}
	add(lastEnvValue(environ, "GC_STORE_ROOT"))
	add(lastEnvValue(environ, "GC_RIG_ROOT"))
	add(dir)
	add(cityPath)
	return roots
}

// scopeDeclaresDoltHost reports whether any scope in play states this host in
// its .beads/config.yaml. A declared endpoint is legitimate by construction —
// that is the whole difference between a rig pointing at the hub and a spliced
// pair — so the guard must never refuse one.
func scopeDeclaresDoltHost(roots []string, host string) bool {
	for _, root := range roots {
		cfg, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(root, ".beads", "config.yaml"))
		if err != nil || !ok {
			continue
		}
		if sameGuardHost(cfg.DoltHost, host) {
			return true
		}
	}
	return false
}

func sameGuardHost(a, b string) bool {
	norm := func(s string) string {
		return strings.Trim(strings.ToLower(strings.TrimSpace(s)), "[]")
	}
	a, b = norm(a), norm(b)
	return a != "" && a == b
}

// guardSynthesizedDoltEndpoint refuses a child whose fully merged environment
// carries a Dolt endpoint that no scope declares: a non-loopback host paired
// with the port the city's own managed-local Dolt is serving. Returning an error
// here fails the command in milliseconds with both halves and their sources
// named, instead of the 48s of refused dials the field incident spent.
//
// It fails OPEN on every uncertainty — no endpoint in the env, a loopback host,
// no resolvable city, no managed server, or a scope that declares the host — so
// it can only ever reject a pair that is structurally impossible.
func guardSynthesizedDoltEndpoint(environ []string, dir string) error {
	host, hostKey := firstNonEmptyEnv(environ, "BEADS_DOLT_SERVER_HOST", "GC_DOLT_HOST")
	if host == "" || contract.DoltHostIsLocal(host) {
		return nil
	}
	port, portKey := firstNonEmptyEnv(environ, "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT", "GC_DOLT_PORT")
	if port == "" {
		return nil
	}
	cityPath, _ := firstNonEmptyEnv(environ, "GC_CITY_PATH", "GC_CITY")
	managedPort, managedSource := managedCityDoltPort(cityPath)
	if managedPort == "" || managedPort != port {
		return nil
	}
	if scopeDeclaresDoltHost(guardScopeRoots(environ, dir, cityPath), host) {
		return nil
	}
	return fmt.Errorf(
		"refusing synthesized Dolt endpoint %s:%s: non-loopback host with the city's managed-local port — no scope declares this pair (ga-bb7rzy); host %q came from %s (the ambient/inherited environment), port %q from %s and matches the city's managed Dolt runtime state in %s; if this endpoint is real, declare it in the scope's .beads/config.yaml, or set GC_DOLT_ENDPOINT_GUARD=0 to disable this check",
		host, port, host, hostKey, port, portKey, managedSource,
	)
}

// guardSynthesizedDoltEndpointEnabled lets an operator disable the guard if it
// ever misjudges a topology in the field. It is deliberately opt-OUT: the
// failure it prevents is silent and expensive, while a wrong refusal is loud and
// names its own escape hatch.
func guardSynthesizedDoltEndpointEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GC_DOLT_ENDPOINT_GUARD"))) {
	case "0", "off", "false":
		return false
	default:
		return true
	}
}
