package scripts_test

import "testing"

// TestGa82cffdRedNegativeCanary fails BY DESIGN. It exists only on the
// ga-82cffd stage (ii-d) negative-control branch, to make that PR's
// "CI / required" check genuinely red so the active carry-operational
// merge gate (ruleset 22632861) can be shown refusing a non-bypass merge
// server-side. This file must never reach carry/operational; if you are
// reading it there, the merge gate failed — revert the merge and reopen
// ga-82cffd stage (ii-d).
func TestGa82cffdRedNegativeCanary(t *testing.T) {
	t.Fatal("deliberate red: ga-82cffd stage (ii-d) negative-control canary")
}
