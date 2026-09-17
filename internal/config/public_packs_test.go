package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
)

// TestSupersededBundledPinTargetCoversCarryCanonicalPins pins the canonical
// bundled pins that quickserve-ai carry/operational builds wrote before the
// 2026-09-15 re-sync. No gastownhall/gascity ref reaches those commits, so a
// city still pinned at one can neither load nor fetch it; the not-cached
// error must point at the offline doctor re-pin instead.
func TestSupersededBundledPinTargetCoversCarryCanonicalPins(t *testing.T) {
	carryPins := []string{
		"sha:f4128e404f6e8329fabb6343f8825f6db753e01b",
		"sha:997b8a563e6d460e6716ecf466b8905049f64139",
		"sha:e37a541e1dc4444e0792272d4029e0b330d82616",
		"sha:abcf2b639b85f849391ef0daf9ddcd27b0728f0a",
		"sha:abcf2b6393a1e52378656570028ac6deb3d2f10f",
	}
	for _, name := range []string{"core", "bd", "dolt"} {
		source, ok := builtinpacks.CanonicalImportSource(name)
		if !ok {
			t.Fatalf("CanonicalImportSource(%q) found no bundled pack", name)
		}
		for _, pin := range carryPins {
			current, ok := SupersededBundledPinTarget(source, pin)
			if !ok || current != BundledPackImportVersion {
				t.Errorf("SupersededBundledPinTarget(%s, %s) = %q, %v; want %q, true", source, pin, current, ok, BundledPackImportVersion)
			}
			if got := notCachedRemediation(source, pin); !strings.Contains(got, `"gc doctor --fix"`) {
				t.Errorf("notCachedRemediation(%s, %s) = %q; want the offline doctor re-pin", source, pin, got)
			}
		}
	}
}

// TestSupersededPublicPackVersionsAreUnique guards the append-only superseded
// pin histories against duplicate entries. The lists are documented as
// "oldest first" and appended singly when a canonical pin is bumped; a
// union/concat (e.g. a botched merge conflict resolution) that reintroduces a
// pin must fail here rather than silently double-pin a version.
func TestSupersededPublicPackVersionsAreUnique(t *testing.T) {
	lists := map[string][]string{
		"SupersededBundledPackImportVersions": SupersededBundledPackImportVersions,
		"SupersededPublicGastownPackVersions": SupersededPublicGastownPackVersions,
		"SupersededPublicGascityPackVersions": SupersededPublicGascityPackVersions,
	}
	for name, versions := range lists {
		seen := make(map[string]int, len(versions))
		for i, v := range versions {
			if prev, ok := seen[v]; ok {
				t.Errorf("%s has duplicate %q at indexes %d and %d", name, v, prev, i)
				continue
			}
			seen[v] = i
		}
	}
}
