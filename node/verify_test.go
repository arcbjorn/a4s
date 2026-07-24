package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

func verifyAction(name, id, checksum string) control.Action {
	ref := control.VolumeRef{Name: name, MountPath: "/var/lib/app", Checksum: checksum}
	return control.Action{
		Kind: control.ActionVerifyBackup, Target: name, Workload: "app",
		Node: "base", Volume: &ref, Snapshot: id,
	}
}

// The defining property: verification proves recoverability without touching the
// live volume, so it is safe to run on a schedule against data in active use.
func TestVerifyDoesNotTouchLiveVolume(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "snapshot content"})

	taken, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	checksum := taken.Observed["checksum"]

	// The live volume then diverges from what the snapshot holds. A
	// verification that restored into the live path instead of scratch would
	// overwrite this with the snapshot content, which the assertion catches.
	if err := os.WriteFile(filepath.Join(root, "app-data", "important.db"),
		[]byte("live data written after the snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The volume is even attached to a running writer while we verify.
	if _, err := volumes.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-0", "app-data")); err != nil {
		t.Fatal(err)
	}

	evidence, err := volumes.Execute(ctx, verifyAction("app-data", "backup-1", checksum))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Observed["verified"] != "true" {
		t.Fatalf("a good backup was not verified: %+v", evidence.Observed)
	}
	// The live volume is byte-for-byte unchanged.
	live, err := os.ReadFile(filepath.Join(root, "app-data", "important.db"))
	if err != nil || string(live) != "live data written after the snapshot" {
		t.Fatalf("verification altered the live volume: %q %v", live, err)
	}
	// No scratch directory is left behind.
	entries, _ := os.ReadDir(filepath.Join(filepath.Dir(root), "snapshots", "app-data"))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".verify") {
			t.Fatalf("verification left scratch behind: %s", entry.Name())
		}
	}
}

// A corrupt backup must verify as failed, which is exactly what an operator
// needs to see before depending on it.
func TestVerifyDetectsCorruptBackup(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "data"})

	taken, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	recorded := taken.Observed["checksum"]

	// The snapshot rots.
	snapshotFile := filepath.Join(filepath.Dir(root), "snapshots", "app-data", "backup-1", "important.db")
	if err := os.WriteFile(snapshotFile, []byte("rot"), 0o600); err != nil {
		t.Fatal(err)
	}

	evidence, err := volumes.Execute(ctx, verifyAction("app-data", "backup-1", recorded))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Observed["verified"] != "false" {
		t.Fatalf("a corrupt backup verified as good: %+v", evidence.Observed)
	}
	if evidence.Observed["reason"] == "" {
		t.Fatal("a failed verification gave no reason")
	}
}

// Verification covers the off-host copy when the local snapshot is gone, since
// that is the copy a real host-loss recovery would use.
func TestVerifyChecksOffHostBackup(t *testing.T) {
	volumes, root, _ := backupFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "payload"})

	taken, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	checksum := taken.Observed["checksum"]
	if _, err := volumes.Execute(ctx, backupAction("app-data", "backup-1", checksum)); err != nil {
		t.Fatal(err)
	}

	// The local snapshot is gone; only the off-host backup remains.
	if err := os.RemoveAll(filepath.Join(filepath.Dir(root), "snapshots", "app-data", "backup-1")); err != nil {
		t.Fatal(err)
	}

	evidence, err := volumes.Execute(ctx, verifyAction("app-data", "backup-1", checksum))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Observed["verified"] != "true" {
		t.Fatalf("the off-host backup did not verify: %+v", evidence.Observed)
	}
}

// A backup that has rotted in the off-host store must verify as failed.
func TestVerifyDetectsCorruptOffHostBackup(t *testing.T) {
	volumes, root, offHost := backupFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "payload"})

	taken, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1"))
	if err != nil {
		t.Fatal(err)
	}
	checksum := taken.Observed["checksum"]
	if _, err := volumes.Execute(ctx, backupAction("app-data", "backup-1", checksum)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(filepath.Dir(root), "snapshots", "app-data", "backup-1")); err != nil {
		t.Fatal(err)
	}
	// The archived copy rots.
	if err := os.WriteFile(filepath.Join(offHost, "app-data", "backup-1", "important.db"),
		[]byte("archive rot"), 0o600); err != nil {
		t.Fatal(err)
	}

	evidence, err := volumes.Execute(ctx, verifyAction("app-data", "backup-1", checksum))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Observed["verified"] != "false" {
		t.Fatalf("a rotted off-host backup verified as good: %+v", evidence.Observed)
	}
}

// A backup missing everywhere verifies as failed rather than erroring, so a
// scheduled check reports the gap rather than halting.
func TestVerifyReportsMissingBackup(t *testing.T) {
	volumes, root, _ := backupFixture(t)
	ctx := context.Background()
	seedVolume(t, volumes, root, "app-data", map[string]string{"important.db": "payload"})
	if _, err := volumes.Execute(ctx, snapshotAction("app-data", "backup-1")); err != nil {
		t.Fatal(err)
	}
	// Remove both the local snapshot and never back it up.
	if err := os.RemoveAll(filepath.Join(filepath.Dir(root), "snapshots", "app-data", "backup-1")); err != nil {
		t.Fatal(err)
	}

	evidence, err := volumes.Execute(ctx, verifyAction("app-data", "backup-1", ""))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Observed["verified"] != "false" {
		t.Fatalf("a missing backup did not verify as failed: %+v", evidence.Observed)
	}
}
