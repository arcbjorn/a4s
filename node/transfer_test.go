package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

// twoNodeFixture builds an origin and a target volume manager that share one
// off-host store, standing in for two hosts moving a volume between them.
func twoNodeFixture(t *testing.T) (origin, target *Volumes, originRoot string) {
	t.Helper()
	store := t.TempDir()

	makeNode := func() (*Volumes, string) {
		dir := t.TempDir()
		root := filepath.Join(dir, "volumes")
		volumes, err := OpenVolumes(root, filepath.Join(dir, "state.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		backup, err := NewDirectoryBackupStore(store, root)
		if err != nil {
			t.Fatal(err)
		}
		volumes.WithBackupStore(backup)
		return volumes, root
	}

	origin, originRoot = makeNode()
	target, _ = makeNode()
	return origin, target, originRoot
}

func quiesceAction(name, to string) control.Action {
	return control.Action{
		Kind: control.ActionQuiesceVolume, Target: name, Workload: "app",
		Node: to, Volume: &control.VolumeRef{Name: name, MountPath: "/var/lib/app"},
	}
}

func transferAction(name, snapshot, checksum, to string) control.Action {
	return control.Action{
		Kind: control.ActionTransferVolume, Target: name, Workload: "app", Node: to,
		Snapshot: snapshot,
		Volume:   &control.VolumeRef{Name: name, MountPath: "/var/lib/app", Checksum: checksum},
	}
}

func adoptAction(name, snapshot, checksum, to string) control.Action {
	return control.Action{
		Kind: control.ActionAdoptVolume, Target: name, Workload: "app", Node: to,
		Snapshot: snapshot,
		Volume:   &control.VolumeRef{Name: name, MountPath: "/var/lib/app", Checksum: checksum},
	}
}

// The data physically moves: written on the origin, and present on the target
// after the handoff, byte for byte.
func TestVolumePhysicallyMovesBetweenNodes(t *testing.T) {
	origin, target, originRoot := twoNodeFixture(t)
	ctx := context.Background()
	seedVolume(t, origin, originRoot, "app-data", map[string]string{
		"important.db":    "the payload",
		"nested/other.db": "more payload",
	})

	// Quiesce and snapshot on the origin.
	if _, err := origin.Execute(ctx, quiesceAction("app-data", "target")); err != nil {
		t.Fatal(err)
	}
	taken, err := origin.Execute(ctx, snapshotAction("app-data", "move-1"))
	if err != nil {
		t.Fatal(err)
	}
	checksum := taken.Observed["checksum"]

	// Ship it to the shared store, then transfer on the target.
	if _, err := origin.Execute(ctx, backupAction("app-data", "move-1", checksum)); err != nil {
		t.Fatal(err)
	}
	transferred, err := target.Execute(ctx, transferAction("app-data", "move-1", checksum, "target"))
	if err != nil {
		t.Fatalf("transfer to target failed: %v", err)
	}
	if transferred.Observed["checksum"] != checksum {
		t.Fatalf("target did not reproduce the checksum: %+v", transferred.Observed)
	}

	// Adopt on the target: the volume materializes there.
	if _, err := target.Execute(ctx, adoptAction("app-data", "move-1", checksum, "target")); err != nil {
		t.Fatalf("adoption on target failed: %v", err)
	}
	targetRoot := filepath.Dir(target.root)
	moved, err := os.ReadFile(filepath.Join(targetRoot, "volumes", "app-data", "important.db"))
	if err != nil || string(moved) != "the payload" {
		t.Fatalf("data did not arrive on the target: %q %v", moved, err)
	}
	nested, err := os.ReadFile(filepath.Join(targetRoot, "volumes", "app-data", "nested/other.db"))
	if err != nil || string(nested) != "more payload" {
		t.Fatalf("nested data did not arrive: %q %v", nested, err)
	}
}

// The origin keeps every byte until adoption. A move that stalls after transfer
// must leave the data reachable where it always was.
func TestOriginKeepsDataUntilAdoption(t *testing.T) {
	origin, target, originRoot := twoNodeFixture(t)
	ctx := context.Background()
	seedVolume(t, origin, originRoot, "app-data", map[string]string{"important.db": "still here"})

	if _, err := origin.Execute(ctx, quiesceAction("app-data", "target")); err != nil {
		t.Fatal(err)
	}
	taken, err := origin.Execute(ctx, snapshotAction("app-data", "move-1"))
	if err != nil {
		t.Fatal(err)
	}
	checksum := taken.Observed["checksum"]
	if _, err := origin.Execute(ctx, backupAction("app-data", "move-1", checksum)); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Execute(ctx, transferAction("app-data", "move-1", checksum, "target")); err != nil {
		t.Fatal(err)
	}

	// The move stalls here. The origin's data must be untouched and still owned
	// by the origin's record.
	live, err := os.ReadFile(filepath.Join(originRoot, "app-data", "important.db"))
	if err != nil || string(live) != "still here" {
		t.Fatalf("origin data changed during a move: %q %v", live, err)
	}
	if _, err := os.Stat(filepath.Join(originRoot, "app-data")); err != nil {
		t.Fatalf("origin volume was removed before adoption: %v", err)
	}
}

// A target that cannot reproduce the checksum has not received the data, so the
// transfer must be refused rather than reported complete.
func TestTransferRefusesChecksumMismatch(t *testing.T) {
	origin, target, originRoot := twoNodeFixture(t)
	ctx := context.Background()
	seedVolume(t, origin, originRoot, "app-data", map[string]string{"important.db": "payload"})

	if _, err := origin.Execute(ctx, quiesceAction("app-data", "target")); err != nil {
		t.Fatal(err)
	}
	taken, err := origin.Execute(ctx, snapshotAction("app-data", "move-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := origin.Execute(ctx, backupAction("app-data", "move-1", taken.Observed["checksum"])); err != nil {
		t.Fatal(err)
	}

	// The control plane hands the target a checksum that does not match.
	_, err = target.Execute(ctx, transferAction("app-data", "move-1", "wrong-checksum", "target"))
	if err == nil || !strings.Contains(err.Error(), "failed verification") {
		t.Fatalf("a mismatched transfer was accepted: %v", err)
	}
	// The failed transfer must not leave a usable snapshot the target could
	// adopt from.
	if _, err := os.Stat(filepath.Join(filepath.Dir(target.root), "snapshots", "app-data", "move-1")); err == nil {
		t.Fatal("a failed transfer left an adoptable snapshot on the target")
	}
}

// A target cannot adopt a snapshot that was never transferred to it, rather
// than materializing an empty volume.
func TestAdoptRequiresTransferredSnapshot(t *testing.T) {
	_, target, _ := twoNodeFixture(t)
	_, err := target.Execute(context.Background(),
		adoptAction("app-data", "never-arrived", "", "target"))
	if err == nil || !strings.Contains(err.Error(), "was not transferred to this node") {
		t.Fatalf("target adopted a snapshot it never received: %v", err)
	}
}

// A snapshot damaged between transfer and adoption must be caught before it is
// written as the live volume.
func TestAdoptRefusesDamagedSnapshot(t *testing.T) {
	origin, target, originRoot := twoNodeFixture(t)
	ctx := context.Background()
	seedVolume(t, origin, originRoot, "app-data", map[string]string{"important.db": "payload"})

	if _, err := origin.Execute(ctx, quiesceAction("app-data", "target")); err != nil {
		t.Fatal(err)
	}
	taken, err := origin.Execute(ctx, snapshotAction("app-data", "move-1"))
	if err != nil {
		t.Fatal(err)
	}
	checksum := taken.Observed["checksum"]
	if _, err := origin.Execute(ctx, backupAction("app-data", "move-1", checksum)); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Execute(ctx, transferAction("app-data", "move-1", checksum, "target")); err != nil {
		t.Fatal(err)
	}

	// The transferred snapshot rots on the target before adoption.
	damaged := filepath.Join(filepath.Dir(target.root), "snapshots", "app-data", "move-1", "important.db")
	if err := os.WriteFile(damaged, []byte("corrupted in transit"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = target.Execute(ctx, adoptAction("app-data", "move-1", checksum, "target"))
	if err == nil || !strings.Contains(err.Error(), "failed verification at adoption") {
		t.Fatalf("a damaged snapshot was adopted: %v", err)
	}
}

// Quiescing a volume a writer still holds would snapshot changing data.
func TestQuiesceRefusesHeldVolume(t *testing.T) {
	origin, _, originRoot := twoNodeFixture(t)
	ctx := context.Background()
	seedVolume(t, origin, originRoot, "app-data", map[string]string{"important.db": "payload"})
	if _, err := origin.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-0", "app-data")); err != nil {
		t.Fatal(err)
	}

	_, err := origin.Execute(ctx, quiesceAction("app-data", "target"))
	if err == nil || !strings.Contains(err.Error(), "cannot be quiesced") {
		t.Fatalf("a held volume was quiesced for a move: %v", err)
	}
}
