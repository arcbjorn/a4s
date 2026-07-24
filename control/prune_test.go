package control

import (
	"strings"
	"testing"
)

// volumeWithSnapshots builds a volume holding an ordered series of snapshots.
func volumeWithSnapshots(ids ...string) *Volume {
	volume := &Volume{Name: "app-data", Node: "base", Snapshots: map[string]string{}}
	for _, id := range ids {
		volume.Snapshots[id] = id + "-checksum"
		volume.SnapshotOrder = append(volume.SnapshotOrder, id)
	}
	if len(ids) > 0 {
		volume.LastSnapshot = ids[len(ids)-1]
	}
	return volume
}

// Pruning keeps the retention count and removes the oldest beyond it.
func TestPruneKeepsRetentionCount(t *testing.T) {
	volume := volumeWithSnapshots("s1", "s2", "s3", "s4", "s5")
	removable := prunableSnapshots(volume, 2)

	// The two most recent plus the last-known (s5, already recent) are kept.
	kept := map[string]bool{"s4": true, "s5": true}
	for _, id := range removable {
		if kept[id] {
			t.Fatalf("prune would remove a retained snapshot %q", id)
		}
	}
	if len(removable) != 3 {
		t.Fatalf("expected three prunable snapshots, got %v", removable)
	}
}

// The last known-good snapshot is never removed, even when it is not among the
// most recent. It is the recovery point an operator would reach for.
func TestPruneProtectsLastKnownGood(t *testing.T) {
	volume := volumeWithSnapshots("s1", "s2", "s3", "s4")
	// A restore pinned an older snapshot as the reference point.
	volume.LastSnapshot = "s1"

	removable := prunableSnapshots(volume, 1)
	for _, id := range removable {
		if id == "s1" {
			t.Fatalf("prune would remove the last known-good snapshot")
		}
	}
}

// A backed-up snapshot is never pruned. Its off-host backup is the copy that
// survives host loss, and the record must not dangle.
func TestPruneProtectsBackedUpSnapshots(t *testing.T) {
	volume := volumeWithSnapshots("s1", "s2", "s3", "s4")
	volume.Backups = map[string]string{"s1": "offsite://s1"}

	removable := prunableSnapshots(volume, 1)
	for _, id := range removable {
		if id == "s1" {
			t.Fatalf("prune would remove a backed-up snapshot, orphaning its backup")
		}
	}
}

// Retention below one is refused: a volume with no snapshot has no recovery
// point, which defeats why snapshots exist.
func TestKernelRefusesRetentionBelowOne(t *testing.T) {
	world := World{Nodes: map[string]*Node{"base": {ID: "base", Healthy: true}}}
	world.normalize()
	world.Volumes["app-data"] = volumeWithSnapshots("s1", "s2")
	goal := Goal{APIVersion: APIVersion, ID: "app-public",
		Workload: WorkloadSpec{Name: "app"}}

	ref := VolumeRef{Name: "app-data", MountPath: "/var/lib/app"}
	proposal := Proposal{
		ID: "p1", AgentID: "storage-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "prune", Kind: ActionPruneSnapshots, Target: "app-data",
			Node: "base", Volume: &ref, Retain: 0,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "storage-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "at least one snapshot") {
		t.Fatalf("kernel allowed retaining zero snapshots: %v", err)
	}
}

// A volume mid-move cannot be pruned; the moving snapshot could be the one
// removed, stranding the handoff.
func TestPruneRefusedDuringHandoff(t *testing.T) {
	world := World{Nodes: map[string]*Node{"base": {ID: "base", Healthy: true}}}
	world.normalize()
	volume := volumeWithSnapshots("s1", "s2")
	volume.Handoff = &VolumeHandoff{From: "base", To: "other", Phase: HandoffSnapshotted}
	world.Volumes["app-data"] = volume
	goal := Goal{APIVersion: APIVersion, ID: "app-public", Workload: WorkloadSpec{Name: "app"}}

	ref := VolumeRef{Name: "app-data", MountPath: "/var/lib/app"}
	proposal := Proposal{
		ID: "p1", AgentID: "storage-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "prune", Kind: ActionPruneSnapshots, Target: "app-data",
			Node: "base", Volume: &ref, Retain: 1,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "storage-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "being moved and cannot be pruned") {
		t.Fatalf("prune proceeded during a move: %v", err)
	}
}

// A dry run reports what it would remove and changes nothing. The roadmap
// requires this before a real prune.
func TestPruneDryRunChangesNothing(t *testing.T) {
	world := World{Nodes: map[string]*Node{"base": {ID: "base", Healthy: true}}}
	world.normalize()
	world.Volumes["app-data"] = volumeWithSnapshots("s1", "s2", "s3")
	executor := NewMemoryExecutor(world)

	ref := VolumeRef{Name: "app-data", MountPath: "/var/lib/app"}
	evidence, err := executor.Execute(Action{
		Kind: ActionPruneSnapshots, Target: "app-data", Node: "base",
		Volume: &ref, Retain: 1, DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Observed["dry_run"] != "true" {
		t.Fatalf("dry run was not reported as such: %+v", evidence.Observed)
	}
	if evidence.Observed["removed"] == "" {
		t.Fatal("dry run reported nothing to remove")
	}
	// The world is unchanged: the snapshots are all still present.
	if len(executor.World().Volumes["app-data"].Snapshots) != 3 {
		t.Fatal("dry run removed snapshots")
	}
}

// Projecting prune evidence removes the snapshots from the world, so no one can
// name a snapshot whose bytes are gone.
func TestPruneEvidenceRemovesSnapshots(t *testing.T) {
	world := World{Nodes: map[string]*Node{"base": {ID: "base", Healthy: true}}}
	world.normalize()
	world.Volumes["app-data"] = volumeWithSnapshots("s1", "s2", "s3")

	world, err := Project(world, Evidence{
		Kind: EvidenceSnapshotsPruned, Target: "app-data",
		Observed: map[string]string{"removed": "s1\ns2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	volume := world.Volumes["app-data"]
	if _, ok := volume.Snapshots["s1"]; ok {
		t.Fatal("pruned snapshot is still restorable")
	}
	if len(volume.SnapshotOrder) != 1 || volume.SnapshotOrder[0] != "s3" {
		t.Fatalf("snapshot order was not updated: %+v", volume.SnapshotOrder)
	}
	if volume.LastSnapshot != "s3" {
		t.Fatalf("last-known was not advanced to the newest survivor: %q", volume.LastSnapshot)
	}
}

// Pruning a snapshot also drops any dangling backup record, so the world does
// not point at a backup for a snapshot it no longer knows.
func TestPruneClearsBackupRecords(t *testing.T) {
	world := World{Nodes: map[string]*Node{"base": {ID: "base", Healthy: true}}}
	world.normalize()
	volume := volumeWithSnapshots("s1", "s2")
	volume.Backups = map[string]string{"s1": "offsite://s1"}
	world.Volumes["app-data"] = volume

	world, err := Project(world, Evidence{
		Kind: EvidenceSnapshotsPruned, Target: "app-data",
		Observed: map[string]string{"removed": "s1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := world.Volumes["app-data"].Backups["s1"]; ok {
		t.Fatal("prune left a dangling backup record")
	}
}

// The storage agent has the grant, so pruning is authorized under the same
// separation as snapshot and restore.
func TestStorageAgentMayPrune(t *testing.T) {
	if !DefaultPolicy().Grants["storage-agent"][ActionPruneSnapshots] {
		t.Fatal("storage agent cannot prune snapshots")
	}
	for _, other := range []string{"placement-agent", "network-agent", "rollout-agent"} {
		if DefaultPolicy().Grants[other][ActionPruneSnapshots] {
			t.Errorf("%s should not be able to prune", other)
		}
	}
}
