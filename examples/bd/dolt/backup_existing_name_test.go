package dolt_test

// ga-eh8lt5: the backup dog synced each database by the fixed name
// <db>-backup. `bd backup init` registers its backup as 'default', and Dolt
// refuses a second name for a URL that already has one ("Address conflict"),
// so for a database whose artifact URL was already taken under another name
// the dog's `dolt backup add <db>-backup` failed, the database was filed
// failed, and its local backup stopped advancing (hq, 2026-10-09 21:01Z run).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeNamedBackupFakeDolt fakes a server with prod and archive. Each
// database's backups live in .dolt/fake-backups as "<name> <url>" lines (the
// snapshot clone carries the file). prod starts with 'default' at its artifact
// URL; archive starts with none. `backup add` refuses a URL that already has a
// name, as Dolt does; `backup sync <name>` fails for an unknown name and
// otherwise lands fakeSyncRoot on the source and on the named backup's URL.
// With FAKE_SERVER_DATA_DIR set, a `backup -v` run under that dir prints each
// line as "name url {}", the form Dolt prints when a sql-server holds the data
// dir (the live city's case); the snapshot clone outside it has no server
// above it and prints the plain form.
func writeNamedBackupFakeDolt(t *testing.T, binDir, dataDir, artifactDir string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dataDir, "prod", ".dolt", "fake-backups"),
		[]byte("default file://"+filepath.Join(artifactDir, "prod")+" \n"), 0o644); err != nil {
		t.Fatalf("seed prod backups: %v", err)
	}
	logPath := filepath.Join(binDir, "dolt.log")
	writeExecutable(t, filepath.Join(binDir, "dolt"), fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
printf 'dolt %%s\n' "$*" >> %s
if [ "${1:-}" = "version" ]; then
  printf 'dolt version 2.1.0\n'
  exit 0
fi
case "$*" in
  *"SHOW DATABASES"*)
    printf 'Database\nprod\narchive\n'
    exit 0
    ;;
esac
touch .dolt/fake-backups
if [ "${1:-}" = "backup" ] && [ "$#" -eq 1 ]; then
  awk '{print $1}' .dolt/fake-backups
  exit 0
fi
if [ "${1:-} ${2:-}" = "backup -v" ]; then
  case "$PWD/" in
    "${FAKE_SERVER_DATA_DIR:-/nonexistent}"/*) awk '{ print $1 " " $2 " {}" }' .dolt/fake-backups ;;
    *) cat .dolt/fake-backups ;;
  esac
  exit 0
fi
if [ "${1:-} ${2:-}" = "backup add" ]; then
  if awk -v u="$4" '$2 == u { found = 1 } END { exit !found }' .dolt/fake-backups; then
    printf "Address conflict with a remote: '%%s' -> %%s\n" "$(awk -v u="$4" '$2 == u { print $1 }' .dolt/fake-backups)" "$4" >&2
    exit 1
  fi
  printf '%%s %%s \n' "$3" "$4" >> .dolt/fake-backups
  exit 0
fi
if [ "${1:-} ${2:-}" = "backup sync" ]; then
  url="$(awk -v n="$3" '$1 == n { print $2 }' .dolt/fake-backups)"
  if [ -z "$url" ]; then
    printf 'unknown backup %%s\n' "$3" >&2
    exit 1
  fi
  bak="${url#file://}"
  mkdir -p .dolt/noms "$bak"
  printf '5:__DOLT__:fakelock:%%s:00000000000000000000000000000000' %s > .dolt/noms/manifest
  printf '5:__DOLT__:fakelock:%%s:00000000000000000000000000000000' %s > "$bak/manifest"
  exit 0
fi
exit 0
`, shellQuote(logPath), fakeSyncRoot, fakeSyncRoot))
	return logPath
}

// TestBackupScriptSyncsExistingBackupAtArtifactURLUnderAnyName: prod already
// has a backup named 'default' at its artifact URL, so the dog syncs
// 'default' (and says so) rather than adding a conflicting prod-backup; archive
// has none, so it still gets archive-backup auto-added and synced. Both write
// the local-backup freshness stamp. The server case is the live city's: `dolt
// backup -v` there appends a params field, which the lookup must ignore.
func TestBackupScriptSyncsExistingBackupAtArtifactURLUnderAnyName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server bool
	}{
		{name: "no-server", server: false},
		{name: "sql-server-params-field", server: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runExistingBackupNameCase(t, tc.server)
		})
	}
}

func runExistingBackupNameCase(t *testing.T, server bool) {
	t.Helper()
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	for _, db := range []string{"prod", "archive"} {
		if err := os.MkdirAll(filepath.Join(dataDir, db, ".dolt"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", db, err)
		}
	}
	binDir := t.TempDir()
	_ = writeDogFakeGC(t, binDir)
	doltLogPath := writeNamedBackupFakeDolt(t, binDir, dataDir, artifactDir)

	var extraEnv []string
	if server {
		extraEnv = append(extraEnv, "FAKE_SERVER_DATA_DIR="+dataDir)
	}
	out := runDogScript(t, "mol-dog-backup.sh", binDir, cityPath, dataDir, extraEnv...)
	if !strings.Contains(out, "synced: 2/2") {
		t.Fatalf("both databases must sync:\n%s", out)
	}
	if !strings.Contains(out, "syncing backup 'default'") {
		t.Fatalf("a backup name other than <db>-backup must be logged:\n%s", out)
	}
	// On the snapshot path the ga-rirak7 root check reads the synced backup's
	// url by name, so a pass there proves it read 'default'.
	if cowCloneAvailable(t) {
		for _, db := range []string{"prod", "archive"} {
			if !strings.Contains(out, "backup: "+db+" — synced (snapshot)") {
				t.Fatalf("%s must sync from the snapshot so the root check runs:\n%s", db, out)
			}
		}
	}
	doltLog, err := os.ReadFile(doltLogPath)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	for _, want := range []string{"backup sync default", "backup add archive-backup", "backup sync archive-backup"} {
		if !strings.Contains(string(doltLog), want) {
			t.Fatalf("dolt log missing %q:\n%s", want, doltLog)
		}
	}
	for _, unwanted := range []string{"backup add prod-backup", "backup sync prod-backup"} {
		if strings.Contains(string(doltLog), unwanted) {
			t.Fatalf("prod's artifact URL already has a backup; dolt log must not contain %q:\n%s", unwanted, doltLog)
		}
	}
	for _, db := range []string{"prod", "archive"} {
		stamp := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "local-backup-freshness", db)
		if body, err := os.ReadFile(stamp); err != nil || !strings.Contains(string(body), "synced_at_epoch=") {
			t.Fatalf("%s: a good sync must write the freshness stamp at %s: err=%v body=%q", db, stamp, err, body)
		}
	}
}
