package dashboardbff

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeAttentionFile writes one file into a city's attention registry directory,
// creating the directory on first use.
func writeAttentionFile(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, attentionRegistryRelDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func getAttention(t *testing.T, root string) attentionRegistry {
	t.Helper()
	p := New(Deps{Resolver: mapResolver{"alpha": root}})
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/alpha/attention", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got attentionRegistry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func TestAttentionReadsValidEntries(t *testing.T) {
	root := t.TempDir()
	writeAttentionFile(t, root, "qcore_archer.json", `{"seat":"qcore/archer","runtime":"claude","event_id":"a22fb8d9","reason":"question","state":"waiting_user","since":"2026-09-08T12:50:49-0700","summary":"Claude is waiting for your input","session_id":"173c7130-ab43"}`)
	// Unknown extra keys are tolerated: the two hook writers evolve
	// independently of this reader.
	writeAttentionFile(t, root, "katya.json", `{"seat":"katya","runtime":"omp","reason":"permission","state":"waiting_user","since":"2026-09-08T19:50:49Z","summary":"approve write","session_id":"01a08070","future_field":{"nested":true}}`)

	got := getAttention(t, root)
	if got.SkippedMalformed != 0 {
		t.Errorf("skippedMalformed = %d, want 0", got.SkippedMalformed)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries = %d, want 2: %#v", len(got.Entries), got.Entries)
	}
	// os.ReadDir orders lexically, so katya precedes qcore_archer.
	if got.Entries[0].Seat != "katya" || got.Entries[0].Runtime != "omp" {
		t.Errorf("entry[0] = %+v, want the omp katya entry", got.Entries[0])
	}
	if got.Entries[0].Reason != "permission" {
		t.Errorf("entry[0].Reason = %q, want permission", got.Entries[0].Reason)
	}
	archer := got.Entries[1]
	if archer.Seat != "qcore/archer" || archer.Runtime != "claude" {
		t.Errorf("entry[1] = %+v, want the claude qcore/archer entry", archer)
	}
	if archer.SessionID != "173c7130-ab43" {
		t.Errorf("entry[1].SessionID = %q, want 173c7130-ab43", archer.SessionID)
	}
	if archer.Since != "2026-09-08T12:50:49-0700" {
		t.Errorf("entry[1].Since = %q, want the offset-style timestamp verbatim", archer.Since)
	}
	if archer.Summary != "Claude is waiting for your input" {
		t.Errorf("entry[1].Summary = %q", archer.Summary)
	}
	if _, err := time.Parse(time.RFC3339Nano, got.ReadAt); err != nil {
		t.Errorf("readAt = %q, want an RFC3339 UTC stamp: %v", got.ReadAt, err)
	}
	if !strings.HasSuffix(got.ReadAt, "Z") {
		t.Errorf("readAt = %q, want UTC (Z-suffixed)", got.ReadAt)
	}
}

func TestAttentionSkipsMalformedAndCountsIt(t *testing.T) {
	root := t.TempDir()
	writeAttentionFile(t, root, "good.json", `{"seat":"cheryl","runtime":"claude","reason":"question","since":"2026-09-08T12:49:37-0700","session_id":"2b721294"}`)
	writeAttentionFile(t, root, "truncated.json", `{"seat":"broken","runtime":`)
	// Valid JSON that is not an entry object is malformed for this reader too.
	writeAttentionFile(t, root, "array.json", `["not","an","entry"]`)

	got := getAttention(t, root)
	if got.SkippedMalformed != 2 {
		t.Errorf("skippedMalformed = %d, want 2", got.SkippedMalformed)
	}
	if len(got.Entries) != 1 || got.Entries[0].Seat != "cheryl" {
		t.Fatalf("entries = %#v, want only the cheryl entry", got.Entries)
	}
}

func TestAttentionIgnoresDotfilesAndNonJSON(t *testing.T) {
	root := t.TempDir()
	writeAttentionFile(t, root, "qcore_pam.json", `{"seat":"qcore/pam","runtime":"claude","reason":"question","since":"2026-09-08T12:50:49-0700","session_id":"a3a6354e"}`)
	// The hooks write entries as a dot-prefixed tmp then rename; a half-written
	// tmp must never be read, and neither must a lock file.
	writeAttentionFile(t, root, ".qcore_pam.json.tmp", `{"seat":"qcore/pam"`)
	writeAttentionFile(t, root, ".ztestpatrol-live.lock", "")
	// The reaper's own log lives in the same directory.
	writeAttentionFile(t, root, "reaper.log", "2026-09-08T13:02:28-0700 tick kept=12 reaped=0 entries=12\n")
	// A directory named like an entry is not a regular file.
	if err := os.MkdirAll(filepath.Join(root, attentionRegistryRelDir, "subdir.json"), 0o755); err != nil {
		t.Fatalf("mkdir subdir.json: %v", err)
	}

	got := getAttention(t, root)
	if got.SkippedMalformed != 0 {
		t.Errorf("skippedMalformed = %d, want 0 (ignored files are not malformed entries)", got.SkippedMalformed)
	}
	if len(got.Entries) != 1 || got.Entries[0].Seat != "qcore/pam" {
		t.Fatalf("entries = %#v, want only the qcore/pam entry", got.Entries)
	}
}

func TestAttentionMissingDirectoryIsEmptyNotError(t *testing.T) {
	root := t.TempDir() // no .gc/runtime/attention at all
	p := New(Deps{Resolver: mapResolver{"alpha": root}})
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/alpha/attention", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a missing registry is an empty registry)", rec.Code)
	}
	// entries must serialize as [] and never null — the SPA decoder requires an
	// array, and `.map` on null is the classic wire-shape crash.
	if body := rec.Body.String(); !strings.Contains(body, `"entries":[]`) {
		t.Errorf("body = %s, want entries serialized as []", body)
	}
	var got attentionRegistry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Entries) != 0 || got.SkippedMalformed != 0 {
		t.Errorf("got %#v, want an empty registry", got)
	}
}

func TestAttentionUnknownCityIs404(t *testing.T) {
	p := New(Deps{Resolver: mapResolver{}})
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/ghost/attention", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown city", rec.Code)
	}
}

// The SPA's decodeAttentionRegistry requires entries:array,
// skippedMalformed:number, readAt:string. Assert the wire shape here so a Go
// struct that drifts from the TS decoder fails in this package, not in a
// browser.
func TestAttentionWireContract(t *testing.T) {
	root := t.TempDir()
	writeAttentionFile(t, root, "seat.json", `{"seat":"cheryl","runtime":"claude","reason":"question","since":"2026-09-08T12:49:37-0700","session_id":"2b721294"}`)
	p := New(Deps{Resolver: mapResolver{"alpha": root}})
	m := wireGet(t, p, "/api/city/alpha/attention")
	mustArray(t, m, "entries")
	if _, ok := m["skippedMalformed"].(float64); !ok {
		t.Errorf("field %q must be a number, got %T", "skippedMalformed", m["skippedMalformed"])
	}
	mustString(t, m, "readAt")

	first, ok := m["entries"].([]any)[0].(map[string]any)
	if !ok {
		t.Fatalf("entries[0] must be an object, got %T", m["entries"].([]any)[0])
	}
	for _, field := range []string{"seat", "runtime", "event_id", "reason", "state", "since", "summary", "session_id"} {
		mustString(t, first, field)
	}
}
