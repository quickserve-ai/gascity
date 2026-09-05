package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestVerifyManagedDoltGlobalsIsSilentWhenTheGlobalIsCorrect(t *testing.T) {
	orig := managedDoltGlobalCheckExecFn
	t.Cleanup(func() { managedDoltGlobalCheckExecFn = orig })

	managedDoltGlobalCheckExecFn = func(_, _, _, _ string) (string, error) {
		// The dolt CLI's tabular rendering, header row and all.
		return "+---+\n| v |\n+---+\n| 0 |\n+---+\n", nil
	}

	var buf bytes.Buffer
	verifyManagedDoltGlobals("127.0.0.1", "51361", "root", &buf)

	if buf.Len() != 0 {
		t.Fatalf("stderr = %q, want silence when the global is already correct", buf.String())
	}
}

// A re-enabled global must be visible to the operator.
func TestVerifyManagedDoltGlobalsWarnsLoudlyWhenTheGlobalIsOn(t *testing.T) {
	orig := managedDoltGlobalCheckExecFn
	t.Cleanup(func() { managedDoltGlobalCheckExecFn = orig })
	managedDoltGlobalCheckExecFn = func(_, _, _, stmt string) (string, error) {
		// This boundary may only read the server-wide policy. A SET would
		// change the policy being checked; a session read could conceal it.
		query := strings.ToUpper(strings.TrimSpace(stmt))
		if !strings.HasPrefix(query, "SELECT @@GLOBAL.DOLT_TRANSACTION_COMMIT ") || strings.Contains(query, ";") {
			t.Fatalf("global-policy check must issue a read-only GLOBAL query, got %q", stmt)
		}
		return "+---+\n| v |\n+---+\n| 1 |\n+---+\n", nil
	}

	var buf bytes.Buffer
	verifyManagedDoltGlobals("127.0.0.1", "51361", "root", &buf)

	out := buf.String()
	if out == "" {
		t.Fatal("stderr is empty; a re-enabled dolt_transaction_commit must never be silent")
	}
	for _, want := range []string{"dolt_transaction_commit", `"1"`, `"0"`} {
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
