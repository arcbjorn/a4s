package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

// fakeEngine records how it was used and produces a distinctive backup, so a
// test can tell an engine backup from a raw file copy.
type fakeEngine struct {
	name        string
	backups     int
	ready       bool
	readyErr    error
	backupFiles map[string]string
}

func (e *fakeEngine) Name() string { return e.name }

func (e *fakeEngine) Backup(_ context.Context, _, address string, port int, destination string) error {
	e.backups++
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return err
	}
	// A real engine writes its own consistent copy. The marker proves the
	// backup came from the engine and not from copying the volume directory.
	files := e.backupFiles
	if files == nil {
		files = map[string]string{"base.tar": "consistent engine backup"}
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(destination, name), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (e *fakeEngine) Ready(context.Context, string, string, int) (bool, error) {
	return e.ready, e.readyErr
}

func databaseFixture(t *testing.T, engine *fakeEngine) (*DatabaseManager, *Volumes, string) {
	t.Helper()
	volumes, root, _ := volumeFixture(t)
	// The database is attached, so its data lives under an owned volume.
	seedVolume(t, volumes, root, "db-data", map[string]string{"torn.db": "half-written live data"})
	if _, err := volumes.Execute(context.Background(),
		volumeAction(control.ActionAttachVolume, "db-0", "db-data")); err != nil {
		t.Fatal(err)
	}
	endpoints := func(allocation string) (string, int, bool) {
		if allocation == "db-0" {
			return "10.42.0.5", 5432, true
		}
		return "", 0, false
	}
	return NewDatabaseManager(volumes, endpoints, engine), volumes, root
}

func dbBackupAction(volume, label string) control.Action {
	return control.Action{
		Kind: control.ActionDatabaseBackup, Target: volume, Workload: "db",
		Node: "base", Volume: &control.VolumeRef{Name: volume}, Snapshot: label,
		Engine: "postgres",
	}
}

// The defining property: a database backup comes from the engine's own tool,
// not a copy of the volume's live files, which would be torn.
func TestDatabaseBackupUsesEngineNotFileCopy(t *testing.T) {
	engine := &fakeEngine{name: "postgres"}
	manager, _, root := databaseFixture(t, engine)

	evidence, err := manager.Execute(context.Background(), dbBackupAction("db-data", "base-1"))
	if err != nil {
		t.Fatal(err)
	}
	if engine.backups != 1 {
		t.Fatalf("the engine backup tool was not invoked: %d calls", engine.backups)
	}

	// The backup holds the engine's consistent copy, not the volume's torn
	// live file.
	backupDir := filepath.Join(filepath.Dir(root), "snapshots", "db-data", "base-1")
	if _, err := os.Stat(filepath.Join(backupDir, "base.tar")); err != nil {
		t.Fatalf("the backup is missing the engine's output: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "torn.db")); err == nil {
		t.Fatal("the backup copied the volume's live files instead of using the engine")
	}
	if evidence.Observed["checksum"] == "" {
		t.Fatalf("database backup recorded no checksum: %+v", evidence.Observed)
	}
}

// A database backup is a first-class recovery point: it lands in the snapshot
// area and can be verified and restored like any other.
func TestDatabaseBackupIsVerifiable(t *testing.T) {
	engine := &fakeEngine{name: "postgres"}
	manager, volumes, _ := databaseFixture(t, engine)
	ctx := context.Background()

	taken, err := manager.Execute(ctx, dbBackupAction("db-data", "base-1"))
	if err != nil {
		t.Fatal(err)
	}
	checksum := taken.Observed["checksum"]

	verified, err := volumes.Execute(ctx, verifyAction("db-data", "base-1", checksum))
	if err != nil {
		t.Fatal(err)
	}
	if verified.Observed["verified"] != "true" {
		t.Fatalf("a database backup did not verify: %+v", verified.Observed)
	}
}

// A backup label is immutable, so re-backing-up under an existing label returns
// the original rather than overwriting it.
func TestDatabaseBackupLabelIsImmutable(t *testing.T) {
	engine := &fakeEngine{name: "postgres"}
	manager, _, _ := databaseFixture(t, engine)
	ctx := context.Background()

	first, err := manager.Execute(ctx, dbBackupAction("db-data", "base-1"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Execute(ctx, dbBackupAction("db-data", "base-1"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Observed["repeated"] != "true" {
		t.Fatalf("a repeated backup was not reported as such: %+v", second.Observed)
	}
	if first.Observed["checksum"] != second.Observed["checksum"] {
		t.Fatal("a repeated backup produced a different checksum")
	}
	if engine.backups != 1 {
		t.Fatalf("the engine ran a second backup for an existing label: %d", engine.backups)
	}
}

// Database readiness comes from the engine accepting a connection, not from the
// port being open.
func TestDatabaseReadinessUsesConnection(t *testing.T) {
	engine := &fakeEngine{name: "postgres", ready: false}
	manager, _, _ := databaseFixture(t, engine)

	// Not accepting connections yet -> not ready, even though the port is up.
	ready, observed, err := manager.ObserveReadiness(control.ProbeTarget{
		Allocation: "db-0", Kind: control.ProbeDatabase, Engine: "postgres", Port: 5432,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatalf("a database still starting up reported ready: %+v", observed)
	}

	// Now it accepts connections.
	engine.ready = true
	ready, observed, err = manager.ObserveReadiness(control.ProbeTarget{
		Allocation: "db-0", Kind: control.ProbeDatabase, Engine: "postgres", Port: 5432,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ready || observed["engine"] != "postgres" {
		t.Fatalf("a serving database did not report ready: %+v", observed)
	}
}

// A backup for an engine the node does not have is refused rather than silently
// falling back to a file copy.
func TestBackupRefusesUnknownEngine(t *testing.T) {
	engine := &fakeEngine{name: "postgres"}
	manager, _, _ := databaseFixture(t, engine)

	action := dbBackupAction("db-data", "base-1")
	action.Engine = "mysql"
	_, err := manager.Execute(context.Background(), action)
	if err == nil || !strings.Contains(err.Error(), "no backend for database engine") {
		t.Fatalf("a backup for an unknown engine was accepted: %v", err)
	}
}

// A backup with no reachable database is refused, since there is nothing to
// take a consistent copy from.
func TestBackupRefusesUnreachableDatabase(t *testing.T) {
	engine := &fakeEngine{name: "postgres"}
	volumes, root, _ := volumeFixture(t)
	seedVolume(t, volumes, root, "db-data", map[string]string{"data": "x"})
	// The volume exists but is not attached, so no allocation owns it.
	manager := NewDatabaseManager(volumes, func(string) (string, int, bool) {
		return "", 0, false
	}, engine)

	_, err := manager.Execute(context.Background(), dbBackupAction("db-data", "base-1"))
	if err == nil || !strings.Contains(err.Error(), "must be attached and running") {
		t.Fatalf("a backup ran against an unreachable database: %v", err)
	}
}

// A Postgres readiness probe treats a refused connection as not-ready rather
// than an error, so a starting database does not fail the reconciliation.
func TestPostgresReadyReportsNotReadyOnRefusedConnection(t *testing.T) {
	engine := NewPostgresEngine("a4s", "postgres")
	engine.probe = func(context.Context, string) error {
		return context.DeadlineExceeded
	}
	ready, err := engine.Ready(context.Background(), "db-0", "10.42.0.5", 5432)
	if err != nil {
		t.Fatalf("a refused connection was reported as an error: %v", err)
	}
	if ready {
		t.Fatal("a database that refused the connection reported ready")
	}
}
