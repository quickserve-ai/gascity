package dashboardbff

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// attentionEntry is one seat's "waiting on the operator" record, as written by
// the runtime hooks into <cityRoot>/.gc/runtime/attention/<seat>.json (one file
// per seat, cleared by the hook when the operator acts and by the stale-entry
// reaper when a session cycles). It must match shared/src/attention-registry.ts
// AttentionRegistryEntry exactly (the frontend's decodeAttentionRegistry
// validates the envelope; whosWaiting.ts reads these fields).
//
// The writers are two independent runtimes (the claude hook pair and the omp
// gc-hook), so unknown extra keys are tolerated by construction — encoding/json
// drops them — and a field a writer omits decodes as "". Only the fields the
// pane actually reads are named here; nothing is required, because a partially
// written entry is still better signal than a dropped one.
type attentionEntry struct {
	Seat      string `json:"seat"`
	Runtime   string `json:"runtime"`
	EventID   string `json:"event_id"`
	Reason    string `json:"reason"`
	State     string `json:"state"`
	Since     string `json:"since"`
	Summary   string `json:"summary"`
	SessionID string `json:"session_id"`
}

// attentionRegistry is the GET /api/city/{cityName}/attention body, matching
// shared/src/attention-registry.ts AttentionRegistry. entries is always an
// explicit array (never null) — a missing registry directory is an empty
// registry, not an error, because the directory only exists once some seat has
// asked for the operator. skippedMalformed counts files that did not parse, so
// the pane can say so out loud instead of silently under-reporting.
type attentionRegistry struct {
	Entries          []attentionEntry `json:"entries"`
	SkippedMalformed int              `json:"skippedMalformed"`
	ReadAt           string           `json:"readAt"`
}

// attentionRegistryRelDir is the registry's path under a city root.
const attentionRegistryRelDir = ".gc/runtime/attention"

// maxAttentionEntryBytes caps how much of one registry file is read. Entries are
// a few hundred bytes; anything larger is not an entry this endpoint should
// stream into the operator's browser.
const maxAttentionEntryBytes = 64 << 10

// registerAttention wires GET /api/city/{cityName}/attention onto the plane mux.
func (p *Plane) registerAttention() {
	p.mux.HandleFunc("GET /api/city/{cityName}/attention", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("cityName")
		root, ok := p.resolveCityPath(name)
		if !ok {
			writeError(w, http.StatusNotFound, "unknown city")
			return
		}
		entries, skipped := readAttentionRegistry(filepath.Join(root, attentionRegistryRelDir))
		writeJSON(w, http.StatusOK, attentionRegistry{
			Entries:          entries,
			SkippedMalformed: skipped,
			ReadAt:           time.Now().UTC().Format(time.RFC3339Nano),
		})
	})
}

// readAttentionRegistry reads every entry file in dir, returning the parsed
// entries (in os.ReadDir's lexical filename order) and the count of files that
// were present but did not parse.
//
// Only regular files whose name ends in ".json" and does not start with "." are
// entries. The dot rule is load-bearing, not cosmetic: both hook writers create
// the entry atomically as a dot-prefixed ".<seat>.json.tmp" and rename it into
// place, so a dotfile is either a half-written entry or a lock, never a record.
// reaper.log is excluded by the same suffix rule.
//
// A missing directory yields (empty, 0) — the registry simply has not been
// written yet. Any other directory-level error yields the same, because this is
// an ambient panel: it degrades to "nobody is waiting", never to a 500 on the
// operator's home page.
func readAttentionRegistry(dir string) ([]attentionEntry, int) {
	entries := []attentionEntry{}
	skipped := 0

	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return entries, skipped
	}
	for _, de := range dirEntries {
		name := de.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if !de.Type().IsRegular() {
			continue
		}
		data, err := readCapped(filepath.Join(dir, name), maxAttentionEntryBytes)
		if err != nil {
			// Present but unreadable reads the same as unparseable to the
			// operator: an entry this pane could not account for.
			skipped++
			continue
		}
		var entry attentionEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			skipped++
			continue
		}
		entries = append(entries, entry)
	}
	return entries, skipped
}

// readCapped reads at most limit bytes from path. A file at or over the cap is
// rejected rather than truncated — a truncated entry would fail to parse anyway
// and would only be counted twice.
func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	return io.ReadAll(io.LimitReader(f, limit))
}
