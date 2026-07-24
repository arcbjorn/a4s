package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

// backupFixture builds a volume manager with an off-host backup store.
func backupFixture(t *testing.T) (*Volumes, string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "volumes")
	state := filepath.Join(dir, "volume-state.jsonl")
	// The store lives outside the volume tree, standing in for another host.
	offHost := filepath.Join(t.TempDir(), "offsite")

	volumes, err := OpenVolumes(root, state)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewDirectoryBackupStore(offHost, root)
	if err != nil {
		t.Fatal(err)
	}
	volumes.WithBackupStore(store)
	return volumes, root, offHost
}

func backupAction(name, id, checksum string) control.Action {
	ref := control.VolumeRef{Name: name, MountPath: "/var/lib/app", Checksum: checksum}
	return control.Action{
		Kind: control.ActionBackupSnapshot, Target: name, Workload: "app",
		Node: "base", Volume: &ref, Snapshot: id,
	}
}

// The failure backups exist for: the node's own snapshots are gone, and the
// data comes back anyway.
func TestVolumeRecoversFromOffHostBackupAfterHostLoss(t *testing.T) {
	volumes, root, _ := backupFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{
		"important.db":    "irreplaceable",
		"nested/other.db": "also irreplaceable",
	})

	taken, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	checksum := taken.Observed["checksum"]
	shipped, err := volumes.Execute(ctx, backupAction("app-data", "backup-1", checksum))
	if err != nil {
		t.Fatal(err)
	}
	if shipped.Observed["location"] == "" {
		t.Fatalf("backup recorded no location: %+v", shipped.Observed)
	}

	// The host loses its disk: local snapshots and volume contents are gone.
	if err := os.RemoveAll(filepath.Join(filepath.Dir(root), "snapshots")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "app-data")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "app-data"), 0o750); err != nil {
		t.Fatal(err)
	}

	restored, err := volumes.Execute(ctx, restoreAction("app-data", "backup-1", checksum))
	if err != nil {
		t.Fatalf("recovery from off-host backup failed: %v", err)
	}
	if restored.Observed["source"] != "backup-store" {
		t.Fatalf("recovery did not come from the backup store: %+v", restored.Observed)
	}
	recovered, err := os.ReadFile(filepath.Join(root, "app-data", "important.db"))
	if err != nil || string(recovered) != "irreplaceable" {
		t.Fatalf("data was not recovered: %q %v", recovered, err)
	}
	nested, err := os.ReadFile(filepath.Join(root, "app-data", "nested/other.db"))
	if err != nil || string(nested) != "also irreplaceable" {
		t.Fatalf("nested data was not recovered: %q %v", nested, err)
	}
}

// A backup store on the same disk as the data does not survive the loss of that
// disk, which defeats its purpose. Refusing the configuration is better than
// discovering it during an incident.
func TestBackupStoreRefusesPathInsideVolumeRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "volumes")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "backups")

	if _, err := NewDirectoryBackupStore(inside, root); err == nil ||
		!strings.Contains(err.Error(), "would not survive host loss") {
		t.Fatalf("a backup store inside the volume root was accepted: %v", err)
	}
	// The volume root itself is equally unsafe.
	if _, err := NewDirectoryBackupStore(root, root); err == nil {
		t.Fatal("a backup store at the volume root was accepted")
	}
}

// A snapshot that rotted on local disk must not be propagated to the store as
// though it were good.
func TestBackupVerifiesBeforeShipping(t *testing.T) {
	volumes, root, offHost := backupFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "good"})

	taken, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	recorded := taken.Observed["checksum"]

	// The local snapshot rots before it is shipped.
	snapshotFile := filepath.Join(filepath.Dir(root), "snapshots", "app-data", "backup-1", "important.db")
	if err := os.WriteFile(snapshotFile, []byte("rotted"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = volumes.Execute(ctx, backupAction("app-data", "backup-1", recorded))
	if err == nil || !strings.Contains(err.Error(), "failed verification before backup") {
		t.Fatalf("a rotted snapshot was shipped off-host: %v", err)
	}
	if _, err := os.Stat(filepath.Join(offHost, "app-data", "backup-1")); err == nil {
		t.Fatal("the corrupt snapshot reached the backup store")
	}
}

// A backup that rots in the store must be refused at restore, before it
// overwrites the data it is supposed to recover.
func TestCorruptBackupIsRefusedAtRestore(t *testing.T) {
	volumes, root, offHost := backupFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "live data"})

	taken, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	checksum := taken.Observed["checksum"]
	if _, err := volumes.Execute(ctx, backupAction("app-data", "backup-1", checksum)); err != nil {
		t.Fatal(err)
	}

	// Local snapshots are gone and the remote copy has rotted.
	if err := os.RemoveAll(filepath.Join(filepath.Dir(root), "snapshots", "app-data")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(offHost, "app-data", "backup-1", "important.db"),
		[]byte("rot in the archive"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = volumes.Execute(ctx, restoreAction("app-data", "backup-1", checksum))
	if err == nil || !strings.Contains(err.Error(), "failed verification") {
		t.Fatalf("a corrupt backup was restored: %v", err)
	}
	// The live data must be untouched by the refused restore.
	live, err := os.ReadFile(filepath.Join(root, "app-data", "important.db"))
	if err != nil || string(live) != "live data" {
		t.Fatalf("a refused restore damaged live data: %q %v", live, err)
	}
}

// A local snapshot is preferred when present, so an ordinary restore does not
// pull data across the network unnecessarily.
func TestRestorePrefersLocalSnapshot(t *testing.T) {
	volumes, root, _ := backupFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "original"})

	taken, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	checksum := taken.Observed["checksum"]
	if _, err := volumes.Execute(ctx, backupAction("app-data", "backup-1", checksum)); err != nil {
		t.Fatal(err)
	}

	restored, err := volumes.Execute(ctx, restoreAction("app-data", "backup-1", checksum))
	if err != nil {
		t.Fatal(err)
	}
	if restored.Observed["source"] != "local-snapshot" {
		t.Fatalf("restore went off-host despite a local snapshot: %+v", restored.Observed)
	}
}

// A backup is as immutable as the snapshot it holds. Re-shipping must not
// change what a verified location refers to.
func TestBackupIsImmutable(t *testing.T) {
	volumes, root, offHost := backupFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "first"})

	taken, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	checksum := taken.Observed["checksum"]
	if _, err := volumes.Execute(ctx, backupAction("app-data", "backup-1", checksum)); err != nil {
		t.Fatal(err)
	}
	// Something writes into the store between backups.
	stored := filepath.Join(offHost, "app-data", "backup-1", "important.db")
	if err := os.WriteFile(stored, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.Execute(ctx, backupAction("app-data", "backup-1", checksum)); err != nil {
		t.Fatal(err)
	}
	// Re-shipping must not have silently replaced the stored copy, so the
	// tampering remains detectable at restore rather than being papered over.
	content, err := os.ReadFile(stored)
	if err != nil || string(content) != "tampered" {
		t.Fatalf("re-shipping overwrote the stored backup: %q %v", content, err)
	}
}

// Backing up without a store configured must fail loudly rather than silently
// recording a backup that does not exist.
func TestBackupRequiresConfiguredStore(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "data"})
	if _, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1")); err != nil {
		t.Fatal(err)
	}

	_, err := volumes.Execute(ctx, backupAction("app-data", "backup-1", ""))
	if err == nil || !strings.Contains(err.Error(), "no backup store") {
		t.Fatalf("backup succeeded without a store: %v", err)
	}
}

// A snapshot that was never taken cannot be backed up.
func TestBackupRequiresExistingSnapshot(t *testing.T) {
	volumes, root, _ := backupFixture(t)
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "data"})

	_, err := volumes.Execute(context.Background(), backupAction("app-data", "never-taken", ""))
	if err == nil || !strings.Contains(err.Error(), "not present on this node") {
		t.Fatalf("backed up a snapshot that does not exist: %v", err)
	}
}
