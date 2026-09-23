package events

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// WalkFilteredTypesNewestFirst reads the log at path one source at a time,
// newest first: the active log, then each archive in descending seq order.
// For each source it calls fn with that source's events matching filter whose
// Type is one of types, in seq order, and with through: every matching event
// with Ts at or after through has now been handed to fn. through is zero once
// the whole retained log has been. fn returning false ends the walk before the
// next, older, source is opened, which is what bounds a walk that only needs
// the most recent matches before some point (ga-4mu4k5): ReadFilteredTypes
// walks oldest-first and cannot stop early.
//
// filter.Type must be empty. Sources wholly at or after filter.BeforeSeq are
// skipped unread. In-flight rotation files are not read, as in
// ReadFilteredTypes.
func WalkFilteredTypesNewestFirst(path string, filter Filter, types []string, fn func(evts []Event, through time.Time) bool) error {
	if filter.Type != "" {
		return fmt.Errorf("WalkFilteredTypesNewestFirst: filter.Type %q must be empty; pass the types as arguments", filter.Type)
	}
	if len(types) == 0 {
		return fmt.Errorf("WalkFilteredTypesNewestFirst: no types given")
	}
	needles := typeNeedles(filter, types)
	matches := func(e Event) bool {
		return matchesFilter(e, filter) && slices.Contains(types, e.Type)
	}

	// Open the active log before listing archives. A rotation between the two
	// then lists the rotated file's archive too, and the activeFirst bound
	// below drops it as a duplicate; listing first could miss that archive.
	var active []Event
	var activeFirst uint64
	haveActive := false
	f, err := os.Open(path)
	switch {
	case err == nil:
		defer f.Close() //nolint:errcheck // read-only file
		if first, _, ok := seqLineAt(f, 0); ok {
			activeFirst, haveActive = first, true
			if filter.BeforeSeq == 0 || first < filter.BeforeSeq {
				if active, err = scanActiveMatches(f, filter, needles, matches); err != nil {
					return err
				}
			}
		}
	case !os.IsNotExist(err):
		return fmt.Errorf("reading events: %w", err)
	}

	dir := filepath.Dir(path)
	archives, err := archiveFilesIn(dir)
	if err != nil {
		archives = nil // as in readFilteredTrackedTypes: a missing dir is an empty log
	}
	var older []archiveInfo
	for _, info := range archives {
		if haveActive && info.FirstSeq >= activeFirst {
			continue
		}
		if !archiveOverlapsFilter(info, filter) {
			continue
		}
		older = append(older, info)
	}

	// through for a source is one second past the rotation stamp of the next
	// older archive: every event that archive holds was appended before its
	// rotation, and the stamp is truncated to the second (archiveOverlapsFilter).
	throughBelow := func(i int) time.Time {
		if i < 0 {
			return time.Time{}
		}
		return older[i].Timestamp.Add(time.Second)
	}
	if haveActive && !fn(active, throughBelow(len(older)-1)) {
		return nil
	}
	for i := len(older) - 1; i >= 0; i-- {
		var evts []Event
		err := streamArchive(filepath.Join(dir, older[i].Basename), filter, needles, func(e Event) bool {
			if matches(e) {
				evts = append(evts, e)
			}
			return true
		})
		if err != nil {
			return fmt.Errorf("reading archive %q: %w", older[i].Basename, err)
		}
		if !fn(evts, throughBelow(i-1)) {
			return nil
		}
	}
	return nil
}

// scanActiveMatches scans the active log from its start, skipping before
// decoding the lines needles or filter.BeforeSeq rule out, and returns the
// events matches accepts.
func scanActiveMatches(f *os.File, filter Filter, needles [][]byte, matches func(Event) bool) ([]Event, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking events: %w", err)
	}
	var result []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !lineMayMatch(line, needles) {
			continue
		}
		if seq, ok := archiveSeq(line); ok && filter.BeforeSeq > 0 && seq >= filter.BeforeSeq {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if matches(e) {
			result = append(result, e)
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("scanning events: %w", err)
	}
	return result, nil
}
