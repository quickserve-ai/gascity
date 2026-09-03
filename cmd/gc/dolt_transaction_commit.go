package main

import (
	"fmt"
	"io"
	"strings"
)

// This file used to SET GLOBAL dolt_transaction_commit = 1 on every managed
// Dolt server once it was query-ready. It now ASSERTS THE OPPOSITE, because
// under the v59 beads pin 1 is the hazardous value and 0 is correct.
//
// WHY IT WAS SET (2026-08-26 - 2026-09-02). Pre-v59, beads' post-run
// auto-commit hook returned immediately outside embedded mode -- "Skips SQL
// server modes; the server owns transaction commit lifecycle there" -- so on a
// server that did not own it, NEITHER side committed. 23 of 27 bd write-command
// files relied on that hook (memory, state, gate, promote, merge_slot, config,
// migrate, ...); only batch, delete and label routed through the transactional
// helper. Their rows landed in the working set and stayed there forever, and a
// dirty working set blocks every subsequent merge -- which wedged qcore
// hub-sync three times in three days (ga-7unsv0). Turning the global on made
// the server commit for them.
//
// It had to be applied after readiness rather than declared in config: dolt
// 2.2.4 IGNORES dolt_transaction_commit in the server config's system_variables
// block (measured on the live server 2026-08-26 -- dolt_stats_enabled,
// dolt_stats_paused, dolt_auto_gc_enabled and wait_timeout from the same block
// all took effect), and SET PERSIST writes a value that a server started with
// --config does not read back.
//
// WHY IT IS RETIRED. v59 beads commits EXPLICITLY: CALL DOLT_COMMIT in
// DoltStore.UpdateIssue / UpdateIssueChecked, StageAndCommit in
// runDoltTransaction. The stranded-writes hazard the global compensated for is
// fixed upstream, and re-applying the global now buys the doubling trap in the
// other direction -- every SQL transaction auto-commits a Dolt commit ON TOP of
// bd's explicit one. It would also make the gc-owned session_liveness writes
// (ga-lys454) auto-commit-eligible, and those exist precisely so that a
// high-frequency telemetry write mints NO commit; the table is in dolt_ignore
// so its rows never stage, but a server-side auto-commit is a different path
// and not worth being clever about. Measured 2026-09-03 on the running server
// (up since the 2026-09-02 unfork window): the global reads 0, hq and as
// working sets are clean, and commits flow.
//
// So the global staying 0 is now the correct state, and the only thing worth
// spending a managed start on is noticing if something turns it back on.

// managedDoltGlobalChecks are read-only assertions run against every managed
// Dolt server once it is query-ready. Each is a query, the value it must
// return, and what a mismatch costs.
var managedDoltGlobalChecks = []struct {
	name string
	stmt string
	want string
	harm string
}{
	{
		name: "dolt_transaction_commit",
		stmt: "SELECT @@GLOBAL.dolt_transaction_commit AS v",
		want: "0",
		harm: "under the v59 beads pin bd commits explicitly, so a server that ALSO auto-commits doubles every write's Dolt commits; see ga-09xcry (and if this pin is ever rolled back below v59, 1 is correct again and this check is the thing to change)",
	},
}

// managedDoltGlobalCheckExecFn is a seam so the fail-visible path can be
// tested. A warning nobody has ever seen fire is a warning that may not fire.
var managedDoltGlobalCheckExecFn = func(host, port, user, stmt string) (string, error) {
	out, err := runManagedDoltSQL(host, port, user, "-q", stmt)
	return string(out), err
}

// verifyManagedDoltGlobals checks the post-readiness global settings on a
// freshly started managed server.
//
// FAIL VISIBLE, NOT FAIL CLOSED — the same posture the SET it replaced had, for
// the same reason. A wrong global, or a check that cannot run, is reported on
// stderr and the server still starts: refusing to start would trade a write
// anomaly for "there is no data plane at all", which is strictly worse. But it
// is never silent, so a caller that sees no warning can rely on the globals.
func verifyManagedDoltGlobals(host, port, user string, stderr io.Writer) {
	for _, c := range managedDoltGlobalChecks {
		out, err := managedDoltGlobalCheckExecFn(host, port, user, c.stmt)
		if err != nil {
			fmt.Fprintf(stderr, "managed-dolt: could not read %s (%v) -- cannot confirm it is %s; %s\n", c.name, err, c.want, c.harm) //nolint:errcheck
			continue
		}
		if got := parseManagedDoltScalar(out); got != c.want {
			fmt.Fprintf(stderr, "managed-dolt: %s is %q, want %q -- %s\n", c.name, got, c.want, c.harm) //nolint:errcheck
		}
	}
}

// parseManagedDoltScalar pulls the single value out of the dolt CLI's tabular
// output: it strips box-drawing rules and the pipes and padding around a cell,
// and keeps the LAST remaining line. For a one-column, one-row result that is
// the value and not the header, and it reads the same whether the client
// renders a table or a bare scalar.
func parseManagedDoltScalar(out string) string {
	var last string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "+-") {
			continue
		}
		v := strings.Trim(line, "|")
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		last = v
	}
	return last
}
