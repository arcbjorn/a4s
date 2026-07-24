package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

// seedVolume creates a volume holding known content.
func seedVolume(t *testing.T, volumes *Volumes, root, name string, files map[string]string) {
	t.Helper()
	if _, err := volumes.Execute(context.Background(), createAction(name)); err != nil {
		t.Fatal(err)
	}
	for relative, content := range files {
		path := filepath.Join(root, name, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func snapshotAction(name, id string) control.Action {
	ref := control.VolumeRef{Name: name, MountPath: "/var/lib/app"}
	return control.Action{
		Kind: control.ActionSnapshotVolume, Target: name, Workload: "app",
		Node: "base", Volume: &ref, Snapshot: id,
	}
}

func restoreAction(name, id, checksum string) control.Action {
	ref := control.VolumeRef{Name: name, MountPath: "/var/lib/app", Checksum: checksum}
	return control.Action{
		Kind: control.ActionRestoreSnapshot, Target: name, Workload: "app",
		Node: "base", Volume: &ref, Snapshot: id,
	}
}

// The property that makes a backup a backup: data written, snapshotted,
// destroyed, and restored comes back byte for byte.
func TestSnapshotRestoreRoundTrips(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{
		"important.db":    "original contents",
		"nested/other.db": "more data",
	})

	taken, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	checksum := taken.Observed["checksum"]
	if checksum == "" {
		t.Fatalf("snapshot recorded no checksum: %+v", taken.Observed)
	}

	// Simulate the disaster the backup exists for.
	if err := os.WriteFile(filepath.Join(root, "app-data", "important.db"),
		[]byte("corrupted by the incident"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "app-data", "nested")); err != nil {
		t.Fatal(err)
	}

	if _, err := volumes.Execute(ctx, restoreAction("app-data", "backup-1", checksum)); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	recovered, err := os.ReadFile(filepath.Join(root, "app-data", "important.db"))
	if err != nil || string(recovered) != "original contents" {
		t.Fatalf("restore did not recover the data: %q %v", recovered, err)
	}
	nested, err := os.ReadFile(filepath.Join(root, "app-data", "nested/other.db"))
	if err != nil || string(nested) != "more data" {
		t.Fatalf("restore did not recover nested data: %q %v", nested, err)
	}
}

// A corrupt snapshot must be refused before anything is overwritten. Restoring
// it would destroy the only remaining copy and replace it with something
// unusable, which is worse than the failure being restored from.
func TestCorruptSnapshotIsRefusedBeforeOverwriting(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "live data"})

	taken, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	recorded := taken.Observed["checksum"]

	// The snapshot rots on disk.
	snapshotFile := filepath.Join(filepath.Dir(root), "snapshots", "app-data", "backup-1", "important.db")
	if err := os.WriteFile(snapshotFile, []byte("bit rot"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = volumes.Execute(ctx, restoreAction("app-data", "backup-1", recorded))
	if err == nil || !strings.Contains(err.Error(), "failed verification") {
		t.Fatalf("a corrupt snapshot was restored: %v", err)
	}
	// The live data must be untouched.
	live, err := os.ReadFile(filepath.Join(root, "app-data", "important.db"))
	if err != nil || string(live) != "live data" {
		t.Fatalf("a refused restore damaged live data: %q %v", live, err)
	}
}

// A snapshot is immutable. Re-taking one under an existing id must return the
// original rather than silently replacing what a verified id refers to.
func TestSnapshotIsImmutable(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "first"})

	first, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app-data", "important.db"),
		[]byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Observed["checksum"] != second.Observed["checksum"] {
		t.Fatalf("an existing snapshot was overwritten: %s -> %s",
			first.Observed["checksum"], second.Observed["checksum"])
	}
	if second.Observed["repeated"] != "true" {
		t.Fatalf("a repeated snapshot was reported as fresh: %+v", second.Observed)
	}
}

// Snapshotting a live writer would produce a copy that may be internally
// inconsistent.
func TestSnapshotRefusesAttachedVolumeAtNode(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "live"})
	if _, err := volumes.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-0", "app-data")); err != nil {
		t.Fatal(err)
	}

	_, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err == nil || !strings.Contains(err.Error(), "quiesce") {
		t.Fatalf("snapshot of a live writer was allowed: %v", err)
	}
}

// Restoring over a live writer would replace the filesystem underneath a
// running process.
func TestRestoreRefusesAttachedVolume(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "live"})

	taken, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-0", "app-data")); err != nil {
		t.Fatal(err)
	}

	_, err = volumes.Execute(ctx, restoreAction("app-data", "backup-1", taken.Observed["checksum"]))
	if err == nil || !strings.Contains(err.Error(), "detach it before restoring") {
		t.Fatalf("restore over a live writer was allowed: %v", err)
	}
}

// The checksum must cover structure, not just content, so a file moved between
// names is detected.
func TestChecksumCoversPathsNotJustContent(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"a.db": "same bytes"})

	first, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	// Same content under a different name.
	if err := os.Rename(filepath.Join(root, "app-data", "a.db"),
		filepath.Join(root, "app-data", "b.db")); err != nil {
		t.Fatal(err)
	}
	second, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-2"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Observed["checksum"] == second.Observed["checksum"] {
		t.Fatal("checksum ignored the file's path")
	}
}

// A snapshot that was never taken cannot be restored, rather than producing an
// empty volume.
func TestRestoreRequiresExistingSnapshot(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "live"})

	_, err := volumes.Execute(context.Background(), restoreAction("app-data", "never-taken", ""))
	if err == nil || !strings.Contains(err.Error(), "not present on this node") {
		t.Fatalf("restored a snapshot that does not exist: %v", err)
	}
}

// A snapshot id must be a safe path component, so it cannot escape the snapshot
// directory.
func TestSnapshotIDIsConstrained(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "live"})

	for _, id := range []string{"../escape", "with/slash", "UPPER", ""} {
		if _, err := volumes.Execute(context.Background(), snapshotAction("app-data", id)); err == nil {
			t.Fatalf("snapshot id %q was accepted", id)
		}
	}
}

// An interrupted snapshot must not leave a partial tree under a name that looks
// complete, so a later restore cannot silently recover half the data.
func TestPartialSnapshotIsNotVisible(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "live"})

	if _, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1")); err != nil {
		t.Fatal(err)
	}
	// A staging directory left by an interrupted run must not be mistaken for a
	// snapshot.
	staging := filepath.Join(filepath.Dir(root), "snapshots", "app-data", "backup-2.partial")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.Execute(ctx, restoreAction("app-data", "backup-2", "")); err == nil {
		t.Fatal("a partial snapshot was restored")
	}
}
