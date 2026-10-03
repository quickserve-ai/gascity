package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// claudePromptMarkerPath is the per-session "prompt submitted" marker, keyed by
// the session bead id (GC_SESSION_ID in the seat, target.sessionID in the
// dispatcher). "" for an id that is not a safe file name.
func claudePromptMarkerPath(cityPath, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if strings.TrimSpace(cityPath) == "" || sessionID == "" || strings.HasPrefix(sessionID, ".") ||
		strings.Contains(sessionID, "..") || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	return citylayout.RuntimePath(cityPath, "nudges", "prompt-submitted", sessionID)
}

// recordClaudePromptSubmitted writes the marker atomically. It must never fail
// the prompt, so callers only log its error. When the write fails, the old
// marker is removed: left in place it would keep vouching for the previous
// prompt, while a missing marker shuts the transcript path until the next
// write. The error is returned only if that removal fails too.
func recordClaudePromptSubmitted(cityPath, sessionID string, at time.Time) error {
	path := claudePromptMarkerPath(cityPath, sessionID)
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		err = claudePromptMarkerWrite(path, []byte(at.UTC().Format(time.RFC3339Nano)+"\n"))
	}
	if err == nil {
		pruneClaudePromptMarkers(dir, time.Now())
		return nil
	}
	if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return fmt.Errorf("prompt marker %s: write: %w; removing the stale marker: %w", path, err, rmErr)
	}
	return nil
}

// claudePromptMarkerWrite writes one marker by temp file + rename (no
// directory sweep per write); a var so tests can fail it.
var claudePromptMarkerWrite = func(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// claudePromptMarkerTTL: a marker this old belongs to a seat that has not
// prompted for a week or is gone. Pruning it only shuts the transcript path
// for that seat until its next prompt.
const claudePromptMarkerTTL = 7 * 24 * time.Hour

// pruneClaudePromptMarkers removes markers and stray temp files older than
// claudePromptMarkerTTL, at most once a day: each call costs one stat of the
// ".pruned" stamp (a name no session id can take), and only a stale stamp
// pays for the directory read.
func pruneClaudePromptMarkers(dir string, now time.Time) {
	stamp := filepath.Join(dir, ".pruned")
	if info, err := os.Stat(stamp); err == nil && now.Sub(info.ModTime()) < 24*time.Hour {
		return
	}
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		return
	}
	_ = os.Chtimes(stamp, now, now)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Name() == ".pruned" || entry.IsDir() {
			continue
		}
		if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > claudePromptMarkerTTL {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

// readClaudePromptSubmitted returns the marker time, or false when there is no
// readable marker.
func readClaudePromptSubmitted(cityPath, sessionID string) (time.Time, bool) {
	path := claudePromptMarkerPath(cityPath, sessionID)
	if path == "" {
		return time.Time{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}
