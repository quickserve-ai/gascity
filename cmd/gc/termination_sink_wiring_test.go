package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryStopRecordedCallPassesASink is the structural guard for "MIGRATED IS
// NOT COLLECTING".
//
// Routing a call through runtime.StopRecorded makes the termination fence read
// clean and the census read complete. It does NOT make the ending recorded: the
// sink list is variadic, and an empty one means StopRecordedDetailed iterates
// nothing and the session stops having written nowhere. Five call sites in this
// package were in exactly that state while the fence reported zero and I
// reported the census complete (Codex #106).
//
// So the property has to be checked separately from the fence, and it is a
// SYNTACTIC property — "was an argument passed" — which is why a grep is the
// honest instrument rather than a weaker behavioral test that would pass for the
// four sites it happened to cover.
func TestEveryStopRecordedCallPassesASink(t *testing.T) {
	root := repoRootFromCmdGC(t)
	// `runtime.StopRecorded(sp, name, runtime.Termination{` opens a composite
	// literal that spans lines; the sinks appear after its closing brace. So the
	// check is on the CLOSING line of each call: `}, <something>...)` passes
	// sinks, `})` does not.
	callOpen := regexp.MustCompile(`runtime\.StopRecorded(Detailed)?\(`)
	sinkless := regexp.MustCompile(`^\s*\}\)(;| |$)`)
	withSink := regexp.MustCompile(`^\s*\},\s*\S+`)

	var offenders []string
	checked := 0
	err := filepath.WalkDir(filepath.Join(root, "cmd", "gc"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr // an unreadable file is not this test's business
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil //nolint:nilerr
		}
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "//") || !callOpen.MatchString(line) {
				continue
			}
			checked++
			// Walk forward to the call's closing line.
			for j := i; j < len(lines) && j < i+24; j++ {
				if withSink.MatchString(lines[j]) {
					break
				}
				if sinkless.MatchString(lines[j]) {
					rel, _ := filepath.Rel(root, path)
					offenders = append(offenders, fmt.Sprintf("%s:%d", rel, j+1))
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking cmd/gc: %v", err)
	}
	// A scan that found no CALLS proves nothing about sinks. Assert it saw some.
	if checked == 0 {
		t.Fatal("found no runtime.StopRecorded call sites in cmd/gc — the scan is looking at nothing")
	}
	for _, o := range offenders {
		t.Errorf("%s calls StopRecorded with NO SINK: it stops the session and records nowhere, "+
			"while the fence and the census both read clean", o)
	}
}

func repoRootFromCmdGC(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find the repo root; a scan over nothing proves nothing")
	return ""
}
