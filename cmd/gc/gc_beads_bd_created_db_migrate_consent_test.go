package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGcBeadsBdInitConsentsToMigrateOnlyADatabaseItCreated pins who may tell bd
// to finish migrating a server-mode database (BD_ALLOW_REMOTE_MIGRATE=1).
//
// bd init --reinit-local runs a preflight (countExistingIssues) that opens the
// store WRITABLE under a 5-second cap. On a database that holds no schema yet,
// bd's shared-server gate lets that open start all of bd's migrations; when the
// cap expires it leaves the database part-way (v41..v46 of v66 in fork CI,
// ga-zyvj2k). bd's real open then finds pending migrations on a server it did
// not start and refuses them as a co-resident client's (#5920): init dies on a
// half-migrated store that nobody else has ever seen. How often depends only on
// how loaded the host is.
//
// Creating the database is consent to its schema, so the script gives bd that
// consent for a database THIS invocation created (backing store absent before
// its own CREATE DATABASE), and never for one that already existed: a
// pre-existing database may have co-resident clients on an older bd, which is
// exactly what the refusal protects.
//
// bd reads the consent once for the whole process, so every database that
// process opens must be the created one: the preflight opens the database
// metadata.json (or BEADS_DOLT_SERVER_DATABASE) names, not --database, and
// shared-server mode also opens beads_global. So a mismatch there, or an
// environment that asks for shared-server mode, gets no consent, and the
// consented call runs with BD_DOLT_SHARED_SERVER=false so a config.yaml
// setting cannot turn the mode back on. The checkpoint retry after a dirty
// partial schema gets the same consent as the first attempt.
//
// The fake bd below behaves like the real one on the half-migrated database:
// it refuses without consent and succeeds with it. It records
// "<BD_ALLOW_REMOTE_MIGRATE>/<BD_DOLT_SHARED_SERVER>" for every init call.
func TestGcBeadsBdInitConsentsToMigrateOnlyADatabaseItCreated(t *testing.T) {
	// The #4566 wording gc-beads-bd.sh matches to checkpoint a partial schema.
	const dirtyRefusal = "Error: failed to initialize schema: schema migration: pending schema migrations alter pre-existing dirty tables: issues; run 'bd dolt commit' to commit the working set at the current schema, then re-run the migration (gastownhall/beads#4566)"
	const refusal = "refusing to auto-apply 20 pending schema migrations to a shared server database (v46 -> v66): migrating would lock out every co-resident bd client still on the old schema (#5920)"

	tests := []struct {
		name                string
		databasePreexisting bool
		backingStoreOnly    bool
		metadataDatabase    string
		firstInitDirty      bool
		env                 []string
		wantSuccess         bool
		wantConsent         string
	}{
		{
			name:        "database this invocation created gets consent and init completes",
			wantSuccess: true,
			wantConsent: "1/false",
		},
		{
			name:                "preexisting empty database keeps the refusal",
			databasePreexisting: true,
			wantConsent:         "unset/unset",
		},
		{
			name:        "shared-server mode from BEADS_DOLT_SHARED_SERVER gets no consent",
			env:         []string{"BEADS_DOLT_SHARED_SERVER=TRUE"},
			wantConsent: "unset/unset",
		},
		{
			name:        "shared-server mode from BD_DOLT_SHARED_SERVER gets no consent",
			env:         []string{"BD_DOLT_SHARED_SERVER=t"},
			wantConsent: "unset/t",
		},
		{
			name:             "metadata naming another database gets no consent",
			metadataDatabase: "old",
			wantConsent:      "unset/unset",
		},
		{
			name:        "BEADS_DOLT_SERVER_DATABASE naming another database gets no consent",
			env:         []string{"BEADS_DOLT_SERVER_DATABASE=old"},
			wantConsent: "unset/unset",
		},
		{
			name:             "adopted backing store is not a created database",
			backingStoreOnly: true,
			wantConsent:      "unset/unset",
		},
		{
			name:           "checkpoint retry after a dirty partial schema keeps the consent",
			firstInitDirty: true,
			wantSuccess:    true,
			wantConsent:    "1/false",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
				t.Fatal(err)
			}
			// gc pre-seeds metadata.json before it calls the script, so even a
			// brand-new database takes the existing-metadata branch.
			metadataDatabase := tt.metadataDatabase
			if metadataDatabase == "" {
				metadataDatabase = "hq"
			}
			if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
				[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"`+metadataDatabase+`"}`), 0o644); err != nil {
				t.Fatal(err)
			}

			materializeBuiltinPacksForTest(t, cityPath)
			script := gcBeadsBdScriptPath(cityPath)
			binDir := filepath.Join(t.TempDir(), "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}

			stateDir := t.TempDir()
			consentFile := filepath.Join(stateDir, "bd-init-consent")
			dirtyOnceFile := filepath.Join(stateDir, "first-init-dirty")
			schemaReadyFile := filepath.Join(stateDir, "schema-ready")
			databaseFile := filepath.Join(stateDir, "database-exists")
			dataDir := filepath.Join(stateDir, "dolt-data")
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if tt.databasePreexisting {
				if err := os.WriteFile(databaseFile, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.databasePreexisting || tt.backingStoreOnly {
				if err := os.MkdirAll(filepath.Join(dataDir, "hq"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tt.firstInitDirty {
				if err := os.WriteFile(dirtyOnceFile, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			fakeBd := fmt.Sprintf(`#!/bin/sh
set -eu
case "${1:-}" in
  --version|version)
    echo "bd version 1.3.0 (test)"
    exit 0
    ;;
  init)
    printf '%%s/%%s\n' "${BD_ALLOW_REMOTE_MIGRATE:-unset}" "${BD_DOLT_SHARED_SERVER:-unset}" >> %q
    if [ -f %q ]; then
      rm -f %q
      printf '%%s\n' %q >&2
      exit 1
    fi
    if [ "${BD_ALLOW_REMOTE_MIGRATE:-}" != 1 ]; then
      printf '%%s\n' %q >&2
      exit 1
    fi
    : > %q
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, consentFile, dirtyOnceFile, dirtyOnceFile, dirtyRefusal, refusal, schemaReadyFile)
			if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(fakeBd), 0o755); err != nil {
				t.Fatal(err)
			}

			fakeDolt := fmt.Sprintf(`#!/bin/sh
set -eu
query=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-q" ]; then
    query="$arg"
    break
  fi
  prev="$arg"
done
case "$query" in
  'SELECT 1')
    exit 0
    ;;
  'USE `+"`hq`"+`')
    [ -f %q ]
    ;;
  'CREATE DATABASE IF NOT EXISTS `+"`hq`"+`')
    : > %q
    ;;
  *information_schema.tables*)
    printf 'cnt\n0\n'
    ;;
  *'FROM dolt_status'*)
    printf 'table_name\nissues\n'
    ;;
  *'FROM config'*)
    # A bd-less database answers the way dolt does, so the script's
    # schema probe (upstream #7265) reads it as "no schema", not a failure.
    if [ ! -f %q ]; then
      echo "table not found: config" >&2
      exit 1
    fi
    ;;
  *)
    exit 0
    ;;
esac
`, databaseFile, databaseFile, schemaReadyFile)
			if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(fakeDolt), 0o755); err != nil {
				t.Fatal(err)
			}
			// sleep_ms shells out to sleep; stubbing it spends retry budgets at no
			// wall-clock cost.
			if err := os.WriteFile(filepath.Join(binDir, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(script, "init", cityPath, "gc", "hq")
			// sanitizedBaseEnv drops GC_* and BEADS_* only; clear the BD_* inputs
			// explicitly so an inherited consent cannot pass the refusal cases.
			env := append(gcBeadsBdTestHomeEnv(t),
				"GC_CITY_PATH="+cityPath,
				"GC_DOLT_DATA_DIR="+dataDir,
				"PATH="+strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)),
				"BD_ALLOW_REMOTE_MIGRATE=",
				"BD_DOLT_SHARED_SERVER=",
			)
			cmd.Env = sanitizedBaseEnv(append(env, tt.env...)...)
			out, err := cmd.CombinedOutput()
			if tt.wantSuccess && err != nil {
				t.Fatalf("gc-beads-bd init failed on a database it created: %v\n%s", err, out)
			}
			if !tt.wantSuccess {
				if err == nil {
					t.Fatalf("gc-beads-bd init migrated a pre-existing database without consent:\n%s", out)
				}
				if !strings.Contains(string(out), "#5920") {
					t.Fatalf("gc-beads-bd init lost bd's refusal:\n%s", out)
				}
			}

			consent, err := os.ReadFile(consentFile)
			if err != nil {
				t.Fatalf("bd init never ran: %v\n%s", err, out)
			}
			for _, got := range strings.Fields(string(consent)) {
				if got != tt.wantConsent {
					t.Fatalf("bd init saw BD_ALLOW_REMOTE_MIGRATE=%s, want %s (every call: %q)\n%s",
						got, tt.wantConsent, strings.TrimSpace(string(consent)), out)
				}
			}
		})
	}
}
