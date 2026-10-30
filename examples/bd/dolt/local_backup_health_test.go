package dolt_test

// ga-xyuer S3a (forward-ported from graft e2546f8cb0, ga-g3p5rm, and 6d496fb15e):
// the backup block of `gc dolt health` answers two questions the doctor already
// answers, and must answer them the same way. WHEN was this database last backed
// up: the newer of its manifest and its sync stamp. WHICH databases are judged:
// the live ones whenever the server can say so, because an artifact directory
// outlives the database it backed up.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeBackupArtifact creates <artifactDir>/<db> with a manifest aged by age.
func writeBackupArtifact(t *testing.T, artifactDir, db string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(artifactDir, db)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", db, err)
	}
	manifest := filepath.Join(dir, "manifest")
	if err := os.WriteFile(manifest, []byte("x"), 0o644); err != nil {
		t.Fatalf("write manifest %s: %v", db, err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(manifest, at, at); err != nil {
		t.Fatalf("chtimes %s: %v", db, err)
	}
	return dir
}

// writeBackupSyncStamp writes the stamp mol-dog-backup leaves after a sync exits 0.
func writeBackupSyncStamp(t *testing.T, cityPath, db string, at time.Time) {
	t.Helper()
	physical, err := filepath.EvalSymlinks(cityPath)
	if err != nil {
		t.Fatalf("resolve city path: %v", err)
	}
	writeRawBackupStamp(t, cityPath, db, fmt.Sprintf("%d", at.Unix()), filepath.Join(physical, ".dolt-backup"))
}

func writeRawBackupStamp(t *testing.T, cityPath, db, epoch, artifactDir string) {
	t.Helper()
	dir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "local-backup-freshness")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir stamp dir: %v", err)
	}
	body := fmt.Sprintf("synced_at_epoch=%s\nartifact_dir=%s\n", epoch, artifactDir)
	if err := os.WriteFile(filepath.Join(dir, db), []byte(body), 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
}

func backupByName(t *testing.T, backups backupsReport, out []byte, name string) (int, bool) {
	t.Helper()
	for _, db := range backups.Databases {
		if db.Name == name {
			return db.AgeSec, db.Stale
		}
	}
	t.Fatalf("dolt_databases has no %s: %+v\n%s", name, backups.Databases, out)
	return 0, false
}

// A sync with nothing new to copy leaves the manifest alone, so a quiet database
// that backed up an hour ago must not read as old as its last change.
func TestHealthDatesABackupFromItsSyncStamp(t *testing.T) {
	cityPath := t.TempDir()
	writeBackupArtifact(t, filepath.Join(cityPath, ".dolt-backup"), "hq", 72*time.Hour)
	writeBackupSyncStamp(t, cityPath, "hq", time.Now().Add(-time.Hour))

	backups, out := runHealthBackupsJSON(t, cityPath)

	age, stale := backupByName(t, backups, out, "hq")
	if stale {
		t.Fatalf("hq read stale an hour after a successful sync\n%s", out)
	}
	if age < 3000 || age > 2*3600 {
		t.Fatalf("hq age = %ds, want about an hour from the stamp\n%s", age, out)
	}
}

// A stamp is believed only for the destination it names, and only as a real past
// time. Each of these would otherwise date a three-day-old manifest as fresh.
func TestHealthRejectsAStampThatCannotDateThisBackup(t *testing.T) {
	past := fmt.Sprintf("%d", time.Now().Add(-time.Hour).Unix())
	future := fmt.Sprintf("%d", time.Now().Add(720*time.Hour).Unix())
	for name, stamp := range map[string][2]string{
		"another destination": {past, "/somewhere/else/.dolt-backup"},
		"no destination":      {past, ""},
		"future epoch":        {future, "here"},
		"leading zero":        {"0" + past, "here"},
		"overflowing epoch":   {"18446744075509551616", "here"},
	} {
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			artifactDir := filepath.Join(cityPath, ".dolt-backup")
			writeBackupArtifact(t, artifactDir, "hq", 72*time.Hour)
			dest := stamp[1]
			if dest == "here" {
				physical, err := filepath.EvalSymlinks(cityPath)
				if err != nil {
					t.Fatalf("resolve city path: %v", err)
				}
				dest = filepath.Join(physical, ".dolt-backup")
			}
			writeRawBackupStamp(t, cityPath, "hq", stamp[0], dest)

			backups, out := runHealthBackupsJSON(t, cityPath)

			if _, stale := backupByName(t, backups, out, "hq"); !stale {
				t.Fatalf("a stamp that cannot date this backup made it read fresh\n%s", out)
			}
		})
	}
}

// The backup order stamps the city's physical path plus /.dolt-backup, so health
// must spell it the same way when both the city and the artifact dir are links.
func TestHealthMatchesTheStampSpellingOfTheBackupOrder(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	physical := filepath.Join(base, "city")
	link := filepath.Join(base, "link")
	elsewhere := filepath.Join(base, "backups")
	if err := os.MkdirAll(physical, 0o755); err != nil {
		t.Fatalf("mkdir city: %v", err)
	}
	if err := os.Symlink(physical, link); err != nil {
		t.Fatalf("link city: %v", err)
	}
	writeBackupArtifact(t, elsewhere, "hq", 72*time.Hour)
	if err := os.Symlink(elsewhere, filepath.Join(physical, ".dolt-backup")); err != nil {
		t.Fatalf("link artifact dir: %v", err)
	}
	writeBackupSyncStamp(t, link, "hq", time.Now().Add(-time.Hour))

	backups, out := runHealthBackupsJSON(t, link)

	if _, stale := backupByName(t, backups, out, "hq"); stale {
		t.Fatalf("a stamp the backup order wrote was not recognised\n%s", out)
	}
}

// The failure this plane exists to catch: a killed sync writes chunks, so
// everything on the artifact side looks recent except the manifest.
func TestHealthFailedSyncDoesNotFreshenABackup(t *testing.T) {
	cityPath := t.TempDir()
	dir := writeBackupArtifact(t, filepath.Join(cityPath, ".dolt-backup"), "hq", 72*time.Hour)
	if err := os.WriteFile(filepath.Join(dir, "chunk-from-a-killed-sync"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	writeBackupSyncStamp(t, cityPath, "hq", time.Now().Add(-72*time.Hour))

	backups, out := runHealthBackupsJSON(t, cityPath)

	if _, stale := backupByName(t, backups, out, "hq"); !stale {
		t.Fatalf("a failed sync read as a fresh backup\n%s", out)
	}
}

// The stamp dates a manifest; it does not stand in for one. Nothing restorable
// exists here, however recent the stamp.
func TestHealthStampWithoutManifestIsNeverBackedUp(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".dolt-backup", "hq"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeBackupSyncStamp(t, cityPath, "hq", time.Now())

	backups, out := runHealthBackupsJSON(t, cityPath)

	age, stale := backupByName(t, backups, out, "hq")
	if !stale || age != -1 {
		t.Fatalf("hq = age %d stale %v, want never backed up (-1, true)\n%s", age, stale, out)
	}
}

// runHealthBackupsLive runs health with the server reachable, onDisk databases
// in the data dir, and catalog as the server's SHOW DATABASES answer.
func runHealthBackupsLive(t *testing.T, cityPath string, onDisk, catalog []string) (backupsReport, []byte) {
	t.Helper()
	return runHealthBackupsCatalog(t, cityPath, onDisk, catalog, false)
}

func runHealthBackupsCatalog(t *testing.T, cityPath string, onDisk, catalog []string, catalogFails bool) (backupsReport, []byte) {
	t.Helper()
	dataDir := filepath.Join(cityPath, "data")
	for _, db := range onDisk {
		if err := os.MkdirAll(filepath.Join(dataDir, db, ".dolt"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", db, err)
		}
	}
	fakeBin := t.TempDir()
	show := "printf 'Database\\n" + strings.Join(catalog, "\\n") + "\\n'"
	if catalogFails {
		show += "; exit 1"
	}
	writeExecutable(t, filepath.Join(fakeBin, "dolt"), "#!/bin/sh\ncase \"$*\" in\n  *\"SHOW DATABASES\"*) "+show+" ;;\nesac\nexit 0\n")
	root := repoRoot(t)
	env := reachableServerEnv(t, root, cityPath)
	for _, entry := range env {
		if rest, ok := strings.CutPrefix(entry, "PATH="); ok {
			env = append(env, "PATH="+fakeBin+string(os.PathListSeparator)+rest)
			break
		}
	}
	env = append(env, "GC_DOLT_DATA_DIR="+dataDir)
	out, err := newHealthScriptCmd(root, env, "--json").Output()
	if err != nil {
		t.Fatalf("health --json failed: %v\n%s", err, out)
	}
	var report struct {
		Backups backupsReport `json:"backups"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("parse health JSON: %v\n%s", err, out)
	}
	return report.Backups, out
}

// An artifact directory outlives its database. A city that has run its tests
// against the managed server keeps dozens of them, and judging those would pin
// the aggregate at stale for good, hiding the one reading that matters.
func TestHealthJudgesLiveDatabasesNotLeftoverArtifacts(t *testing.T) {
	cityPath := t.TempDir()
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	writeBackupArtifact(t, artifactDir, "hq", time.Minute)
	writeBackupArtifact(t, artifactDir, "testdb_0616a0a02c32", 500*time.Hour)

	backups, out := runHealthBackupsLive(t, cityPath, []string{"hq"}, []string{"hq"})

	if backups.DoltStale == nil || *backups.DoltStale {
		t.Fatalf("dolt_stale = %v, want false: the only live database is fresh\n%s", backups.DoltStale, out)
	}
	if len(backups.Databases) != 1 || backups.Databases[0].Name != "hq" {
		t.Fatalf("dolt_databases = %+v, want hq alone\n%s", backups.Databases, out)
	}
}

// The other half of judging live databases: one the backup order never reached
// has no artifact directory at all, which a scan of that directory cannot see.
func TestHealthReportsALiveDatabaseWithNoBackup(t *testing.T) {
	cityPath := t.TempDir()
	writeBackupArtifact(t, filepath.Join(cityPath, ".dolt-backup"), "hq", time.Minute)

	backups, out := runHealthBackupsLive(t, cityPath, []string{"hq", "reviews"}, []string{"hq", "reviews"})

	if backups.DoltStale == nil || !*backups.DoltStale {
		t.Fatalf("dolt_stale = %v, want true: reviews has no backup\n%s", backups.DoltStale, out)
	}
	age, stale := backupByName(t, backups, out, "reviews")
	if !stale || age != -1 {
		t.Fatalf("reviews = age %d stale %v, want never backed up (-1, true)\n%s", age, stale, out)
	}
}

// The set comes from the server, not the data dir: a data dir that is not the one
// the server serves must not drop a stale database from the report.
func TestHealthServerCatalogBeatsAPartialDataDir(t *testing.T) {
	cityPath := t.TempDir()
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	writeBackupArtifact(t, artifactDir, "hq", time.Minute)
	writeBackupArtifact(t, artifactDir, "reviews", 72*time.Hour)

	backups, out := runHealthBackupsLive(t, cityPath, []string{"hq"}, []string{"hq", "reviews"})

	if _, stale := backupByName(t, backups, out, "reviews"); !stale {
		t.Fatalf("reviews is 3 days old and live on the server\n%s", out)
	}
	if backups.DoltStale == nil || !*backups.DoltStale {
		t.Fatalf("dolt_stale = %v, want true\n%s", backups.DoltStale, out)
	}
}

// No answer from the server must fail toward coverage: the scan can over-report
// stale, but it cannot lose a database.
func TestHealthFailedCatalogReadKeepsEveryArtifact(t *testing.T) {
	cityPath := t.TempDir()
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	writeBackupArtifact(t, artifactDir, "hq", time.Minute)
	writeBackupArtifact(t, artifactDir, "reviews", 72*time.Hour)

	// Both shapes of failure: nothing printed, and a read that dies partway
	// through the list after naming only the fresh database.
	for name, catalog := range map[string][]string{"no output": nil, "partial output": {"hq"}} {
		t.Run(name, func(t *testing.T) {
			backups, out := runHealthBackupsCatalog(t, cityPath, []string{"hq"}, catalog, true)

			if _, stale := backupByName(t, backups, out, "reviews"); !stale {
				t.Fatalf("a failed catalog read lost a stale database\n%s", out)
			}
		})
	}
}

// The backup order covers bead scopes, so a test database left live on the server
// has no backup by design and must not pin the reading at stale.
func TestHealthIgnoresALiveTestDatabase(t *testing.T) {
	cityPath := t.TempDir()
	writeBackupArtifact(t, filepath.Join(cityPath, ".dolt-backup"), "hq", time.Minute)

	backups, out := runHealthBackupsLive(t, cityPath, []string{"hq", "testdb_0616a0a02c32"}, []string{"hq", "testdb_0616a0a02c32"})

	if backups.DoltStale == nil || *backups.DoltStale {
		t.Fatalf("dolt_stale = %v, want false: a live test database is not a gap\n%s", backups.DoltStale, out)
	}
	if len(backups.Databases) != 1 || backups.Databases[0].Name != "hq" {
		t.Fatalf("dolt_databases = %+v, want hq alone\n%s", backups.Databases, out)
	}
}

// A city that takes no backups has nothing to measure: live databases must not
// turn an unmeasured report into a stale one.
func TestHealthLiveDatabasesDoNotInventABackupPlane(t *testing.T) {
	cityPath := t.TempDir()

	backups, out := runHealthBackupsLive(t, cityPath, []string{"hq"}, []string{"hq"})

	if backups.DoltMeasured || backups.DoltStale != nil {
		t.Fatalf("measured=%v stale=%v, want unmeasured with no backup directory\n%s", backups.DoltMeasured, backups.DoltStale, out)
	}
}
