package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// takeSnapshots writes a series of snapshots with increasing modification
// times, so the node's recency ordering is deterministic.
func takeSnapshots(t *testing.T, volumes *Volumes, root, name string, ids ...string) {
	t.Helper()
	ctx := context.Background()
	seedVolume(t, volumes, root, name, map[string]string{"important.db": "data"})
	base := time.Now().Add(-time.Hour)
	snapshotDir := filepath.Join(filepath.Dir(root), "snapshots", name)
	for i, id := range ids {
		if _, err := volumes.Execute(ctx, snapshotAction(name, id)); err != nil {
			t.Fatal(err)
		}
		// Space the modification times so recency is unambiguous.
		when := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(snapshotDir, id), when, when); err != nil {
			t.Fatal(err)
		}
		// Change the volume so each snapshot has distinct content.
		if err := os.WriteFile(filepath.Join(root, name, "important.db"),
			[]byte(id), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func pruneAction(name string, retain int, dryRun bool) control.Action {
	return control.Action{
		Kind: control.ActionPruneSnapshots, Target: name, Workload: "app", Node: "base",
		Volume: &control.VolumeRef{Name: name, MountPath: "/var/lib/app"},
		Retain: retain, DryRun: dryRun,
	}
}

func snapshotPresent(t *testing.T, root, volume, id string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(filepath.Dir(root), "snapshots", volume, id))
	return err == nil
}

// Pruning removes the oldest snapshots and keeps the most recent, on disk.
func TestNodePruneRemovesOldSnapshots(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	takeSnapshots(t, volumes, root, "app-data", "s1", "s2", "s3", "s4")

	evidence, err := volumes.Execute(context.Background(), pruneAction("app-data", 2, false))
	if err != nil {
		t.Fatal(err)
	}
	removed := evidence.Observed["removed"]
	if !strings.Contains(removed, "s1") || !strings.Contains(removed, "s2") {
		t.Fatalf("prune did not remove the oldest snapshots: %q", removed)
	}
	if snapshotPresent(t, root, "app-data", "s1") {
		t.Fatal("an old snapshot survived pruning")
	}
	if !snapshotPresent(t, root, "app-data", "s4") {
		t.Fatal("a recent snapshot was pruned")
	}
}

// A dry run reports what it would remove without touching the disk.
func TestNodePruneDryRunRemovesNothing(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	takeSnapshots(t, volumes, root, "app-data", "s1", "s2", "s3")

	evidence, err := volumes.Execute(context.Background(), pruneAction("app-data", 1, true))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Observed["dry_run"] != "true" {
		t.Fatalf("dry run was not reported as such: %+v", evidence.Observed)
	}
	if evidence.Observed["removed"] == "" {
		t.Fatal("dry run reported nothing to remove")
	}
	// Every snapshot must still be on disk.
	for _, id := range []string{"s1", "s2", "s3"} {
		if !snapshotPresent(t, root, "app-data", id) {
			t.Fatalf("dry run removed snapshot %q", id)
		}
	}
}

// The node never removes the last snapshot standing, even when asked to retain
// fewer than exist. A volume with no recovery point is the state pruning must
// never produce.
func TestNodePruneNeverRemovesLastSnapshot(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	takeSnapshots(t, volumes, root, "app-data", "s1")

	// Retain zero would, taken literally, delete everything.
	if _, err := volumes.Execute(context.Background(), pruneAction("app-data", 0, false)); err != nil {
		t.Fatal(err)
	}
	if !snapshotPresent(t, root, "app-data", "s1") {
		t.Fatal("the last snapshot was pruned, leaving no recovery point")
	}
}

// A dry run followed by a real prune removes exactly what the dry run reported,
// so an operator's preview matches the result.
func TestNodePrunePreviewMatchesResult(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	takeSnapshots(t, volumes, root, "app-data", "s1", "s2", "s3", "s4", "s5")
	ctx := context.Background()

	preview, err := volumes.Execute(ctx, pruneAction("app-data", 2, true))
	if err != nil {
		t.Fatal(err)
	}
	real, err := volumes.Execute(ctx, pruneAction("app-data", 2, false))
	if err != nil {
		t.Fatal(err)
	}
	if preview.Observed["removed"] != real.Observed["removed"] {
		t.Fatalf("preview did not match the real prune:\npreview: %q\nreal:    %q",
			preview.Observed["removed"], real.Observed["removed"])
	}
}

// Pruning is idempotent: a second prune with the same retention removes nothing
// more.
func TestNodePruneIsIdempotent(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	takeSnapshots(t, volumes, root, "app-data", "s1", "s2", "s3")
	ctx := context.Background()

	if _, err := volumes.Execute(ctx, pruneAction("app-data", 1, false)); err != nil {
		t.Fatal(err)
	}
	second, err := volumes.Execute(ctx, pruneAction("app-data", 1, false))
	if err != nil {
		t.Fatal(err)
	}
	if second.Observed["removed"] != "" {
		t.Fatalf("a second prune removed more: %q", second.Observed["removed"])
	}
}

// A restore after pruning still works for a retained snapshot, so pruning does
// not compromise recovery.
func TestRestoreWorksAfterPrune(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	takeSnapshots(t, volumes, root, "app-data", "s1", "s2", "s3")
	ctx := context.Background()

	// s3 holds "s2" as content (written before the s3 snapshot was taken).
	kept, err := checksumTreeForTest(t, filepath.Join(filepath.Dir(root), "snapshots", "app-data", "s3"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.Execute(ctx, pruneAction("app-data", 1, false)); err != nil {
		t.Fatal(err)
	}
	if !snapshotPresent(t, root, "app-data", "s3") {
		t.Fatal("the retained snapshot was pruned")
	}
	if _, err := volumes.Execute(ctx, restoreAction("app-data", "s3", kept)); err != nil {
		t.Fatalf("restore of a retained snapshot failed after prune: %v", err)
	}
}

func checksumTreeForTest(t *testing.T, dir string) (string, error) {
	t.Helper()
	return checksumTree(dir)
}
