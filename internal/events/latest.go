package events

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// TypeSubject keys one grain of the LatestPerSubjectInActiveLog fold: the
// newest event of a given Type for a given Subject. An event type that carries
// no subject (controller.started, for example) folds under the empty Subject.
type TypeSubject struct {
	Type    string
	Subject string
}

// LatestPerSubjectInActiveLog scans the ACTIVE event log at path exactly once
// and returns the newest event for each (Type, Subject) pair whose Type appears
// in types. Sibling archives are deliberately NOT read. A missing active log is
// not an error — the result is empty.
//
// This is the AGGREGATE counterpart to ReadFiltered, for callers that need only
// "the most recent event per subject" and not the history behind it.
// ReadFiltered materializes EVERY matching event across EVERY sibling archive,
// so on a long-lived city it gunzips and JSON-decodes the entire retained
// corpus on every call. Measured on this host (ga-22tvtm): gc doctor's
// order-firing-current check made two such reads — 35.2s for order.fired
// (410,282 events materialized) plus 26.8s for controller.started — to derive
// 43 timestamps, against a 48s budget it could not possibly meet. The fold here
// is one pass and holds one Event per pair.
//
// Reading only the active log is what makes the cost bounded by ROTATION POLICY
// rather than by how much history the city has retained. That bound is the
// point: a check whose cost grows with retained history abandons precisely when
// the city is oldest and busiest. Archived events are strictly older than every
// event in the active log, so an archive can never win the fold for a subject
// the active log already covers. A caller that needs a subject the active log
// does NOT cover must resolve it from an authoritative store — event history is
// not that store. (Events briefly stranded in an in-flight
// events.jsonl.rotating-* file, in the seconds between rename and gzip, are
// likewise not read.)
func LatestPerSubjectInActiveLog(path string, types ...string) (map[TypeSubject]Event, error) {
	latest := make(map[TypeSubject]Event, len(types))
	if len(types) == 0 {
		return latest, nil
	}
	wanted := make(map[string]struct{}, len(types))
	for _, t := range types {
		wanted[t] = struct{}{}
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return latest, nil
		}
		return latest, fmt.Errorf("reading events: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // handle lines up to 1MB
	for scanner.Scan() {
		// Decode the type before the whole event: the log is dominated by types
		// no caller of this fold wants, and skipping them on a small header
		// decode is what keeps the pass cheap.
		var header struct {
			Type string `json:"type"`
		}
		line := scanner.Bytes()
		if json.Unmarshal(line, &header) != nil {
			continue // skip malformed lines, like ReadFiltered
		}
		if _, ok := wanted[header.Type]; !ok {
			continue
		}
		var e Event
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		key := TypeSubject{Type: e.Type, Subject: e.Subject}
		if prev, ok := latest[key]; ok && !e.Ts.After(prev.Ts) {
			continue
		}
		latest[key] = e
	}
	if err := scanner.Err(); err != nil {
		return latest, fmt.Errorf("scanning events: %w", err)
	}
	return latest, nil
}

// LatestTsForType returns the newest Ts across every subject of the given type
// in a LatestPerSubjectInActiveLog result, or the zero time when the type is
// absent. It is the fold for types whose subject is irrelevant to the caller.
func LatestTsForType(latest map[TypeSubject]Event, eventType string) time.Time {
	var newest time.Time
	for key, e := range latest {
		if key.Type != eventType {
			continue
		}
		if e.Ts.After(newest) {
			newest = e.Ts
		}
	}
	return newest
}
