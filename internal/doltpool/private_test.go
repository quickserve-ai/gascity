package doltpool

import (
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

// TestOpenPrivateHandleStaysOutOfTheRegistry pins what makes a private handle
// safe for its owner to Close: every call returns a distinct *sql.DB, none of
// them is the pooled handle for the same endpoint, and the registry never
// learns about them.
func TestOpenPrivateHandleStaysOutOfTheRegistry(t *testing.T) {
	resetForTest(t)
	pooled, err := Open("127.0.0.1", "3307", "root", "pw", "hq")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first, err := OpenPrivate("127.0.0.1", "3307", "root", "pw", "hq", 5*time.Second)
	if err != nil {
		t.Fatalf("OpenPrivate: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := OpenPrivate("127.0.0.1", "3307", "root", "pw", "hq", 5*time.Second)
	if err != nil {
		t.Fatalf("OpenPrivate: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	if first == pooled || second == pooled {
		t.Fatal("OpenPrivate returned the pooled handle; closing it would break every pooled caller")
	}
	if first == second {
		t.Fatal("OpenPrivate returned one handle for two calls; each owner must hold its own")
	}
	if got := Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1 — private handles must never enter the registry", got)
	}
}

// TestPrivateDSNCarriesTheCallerTimeout pins the private handle's connection
// settings: the caller's timeout bounds the dial, reads and writes, ParseTime
// stays off, and an IPv6 host is bracketed as in the pooled DSN.
func TestPrivateDSNCarriesTheCallerTimeout(t *testing.T) {
	dsn := privateDSN("::1", "3307", "root", "pw", "hq", 5*time.Second)
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", dsn, err)
	}
	if parsed.Addr != "[::1]:3307" {
		t.Errorf("Addr = %q, want [::1]:3307", parsed.Addr)
	}
	if parsed.User != "root" || parsed.Passwd != "pw" || parsed.DBName != "hq" {
		t.Errorf("user/password/database = %q/%q/%q, want root/pw/hq", parsed.User, parsed.Passwd, parsed.DBName)
	}
	for name, got := range map[string]time.Duration{
		"Timeout":      parsed.Timeout,
		"ReadTimeout":  parsed.ReadTimeout,
		"WriteTimeout": parsed.WriteTimeout,
	} {
		if got != 5*time.Second {
			t.Errorf("%s = %v, want 5s", name, got)
		}
	}
	if parsed.ParseTime {
		t.Error("ParseTime = true, want false")
	}
	if !parsed.AllowNativePasswords {
		t.Error("AllowNativePasswords = false, want true")
	}
}
