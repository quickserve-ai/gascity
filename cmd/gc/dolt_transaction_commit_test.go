package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// The check is the whole point of the file. It replaced a SET that had been
// pinned here for the opposite reason: pre-v59, dropping the SET meant bd
// writes silently stopped committing and wedged the next merge (ga-7unsv0);
// under v59 bd commits explicitly, so the hazard reversed and 1 now DOUBLES
// every write's commits (ga-09xcry). Either way the failure is invisible at the
// call site, so pin the assertion here.
func TestManagedDoltGlobalChecksAssertTransactionCommitIsZero(t *testing.T) {
	var found bool
	for _, c := range managedDoltGlobalChecks {
		if c.name != "dolt_transaction_commit" {
			continue
		}
		found = true
		if !strings.Contains(c.stmt, "@@GLOBAL.dolt_transaction_commit") {
			t.Errorf("stmt = %q, want it to read the GLOBAL (a session value proves nothing about the server)", c.stmt)
		}
		if strings.Contains(strings.ToUpper(c.stmt), "SET ") {
			t.Errorf("stmt = %q, want a read — gc must not set this global under v59", c.stmt)
		}
		if c.want != "0" {
			t.Errorf("want = %q, want %q — v59 beads commits explicitly, so 1 doubles every write's Dolt commits", c.want, "0")
		}
	}
	if !found {
		t.Fatal("managedDoltGlobalChecks has no dolt_transaction_commit entry")
	}
}

func TestVerifyManagedDoltGlobalsIsSilentWhenTheGlobalIsCorrect(t *testing.T) {
	orig := managedDoltGlobalCheckExecFn
	t.Cleanup(func() { managedDoltGlobalCheckExecFn = orig })

	var gotHost, gotPort, gotUser, gotStmt string
	calls := 0
	managedDoltGlobalCheckExecFn = func(host, port, user, stmt string) (string, error) {
		calls++
		gotHost, gotPort, gotUser, gotStmt = host, port, user, stmt
		// The dolt CLI's tabular rendering, header row and all.
		return "+---+\n| v |\n+---+\n| 0 |\n+---+\n", nil
	}

	var buf bytes.Buffer
	verifyManagedDoltGlobals("127.0.0.1", "51361", "root", &buf)

	if calls != len(managedDoltGlobalChecks) {
		t.Fatalf("exec calls = %d, want %d", calls, len(managedDoltGlobalChecks))
	}
	if gotHost != "127.0.0.1" || gotPort != "51361" || gotUser != "root" {
		t.Fatalf("connection args = %q/%q/%q, want 127.0.0.1/51361/root", gotHost, gotPort, gotUser)
	}
	if !strings.Contains(gotStmt, "dolt_transaction_commit") {
		t.Fatalf("stmt = %q, want the transaction-commit check", gotStmt)
	}
	if buf.Len() != 0 {
		t.Fatalf("stderr = %q, want silence when the global is already correct", buf.String())
	}
}

// The value being WRONG is the case this file exists for: something turned the
// global back on, and every bd write from then on mints two Dolt commits.
func TestVerifyManagedDoltGlobalsWarnsLoudlyWhenTheGlobalIsOn(t *testing.T) {
	orig := managedDoltGlobalCheckExecFn
	t.Cleanup(func() { managedDoltGlobalCheckExecFn = orig })
	managedDoltGlobalCheckExecFn = func(_, _, _, _ string) (string, error) {
		return "+---+\n| v |\n+---+\n| 1 |\n+---+\n", nil
	}

	var buf bytes.Buffer
	verifyManagedDoltGlobals("127.0.0.1", "51361", "root", &buf)

	out := buf.String()
	if out == "" {
		t.Fatal("stderr is empty; a re-enabled dolt_transaction_commit must never be silent")
	}
	for _, want := range []string{"dolt_transaction_commit", `"1"`, "ga-09xcry"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr = %q, want it to mention %q", out, want)
		}
	}
}

// FAIL VISIBLE, NOT FAIL CLOSED: an unreadable global must still let the server
// come up, but the operator must be told, because "I could not check" and "it
// is correct" are otherwise indistinguishable.
func TestVerifyManagedDoltGlobalsWarnsLoudlyWhenTheCheckCannotRun(t *testing.T) {
	orig := managedDoltGlobalCheckExecFn
	t.Cleanup(func() { managedDoltGlobalCheckExecFn = orig })
	managedDoltGlobalCheckExecFn = func(_, _, _, _ string) (string, error) {
		return "", errors.New("connection refused")
	}

	var buf bytes.Buffer
	verifyManagedDoltGlobals("127.0.0.1", "51361", "root", &buf)

	out := buf.String()
	if out == "" {
		t.Fatal("stderr is empty; a check that could not run must never be silent")
	}
	for _, want := range []string{"dolt_transaction_commit", "connection refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr = %q, want it to mention %q", out, want)
		}
	}
}

func TestParseManagedDoltScalarReadsTheValueNotTheHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want string
	}{
		{"table with header", "+---+\n| v |\n+---+\n| 0 |\n+---+\n", "0"},
		{"bare scalar", "0\n", "0"},
		{"padded cell", "+-------+\n|   v   |\n+-------+\n|   1   |\n+-------+\n", "1"},
		{"empty output", "", ""},
	} {
		if got := parseManagedDoltScalar(tc.out); got != tc.want {
			t.Errorf("%s: parseManagedDoltScalar = %q, want %q", tc.name, got, tc.want)
		}
	}
}
