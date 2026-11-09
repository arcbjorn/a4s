package control

import (
	"testing"
	"time"
)

func evidenceWorld() World {
	world := World{Nodes: map[string]*Node{"base": {ID: "base", Healthy: true,
		Capacity: Resources{CPUMillis: 8000, MemoryMB: 16384}}}}
	world.normalize()
	world.ObservedAt = time.Unix(1_700_000_000, 0).UTC()
	return world
}

// A dry run lists what it would remove. Projecting that list as removal would
// make snapshots still on disk unrestorable.
func TestDryRunPruneLeavesSnapshotsRecorded(t *testing.T) {
	world := evidenceWorld()
	world.Volumes["app-data"] = volumeWithSnapshots("s1", "s2", "s3")
	ref := VolumeRef{Name: "app-data", MountPath: "/data"}
	evidence, err := NewMemoryExecutor(world).Execute(Action{Kind: ActionPruneSnapshots,
		Target: "app-data", Node: "base", Volume: &ref, Retain: 1, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	next, err := Project(world, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(next.Volumes["app-data"].Snapshots); got != 3 {
		t.Fatalf("a dry-run prune left %d of 3 snapshots recorded", got)
	}
	if next.Volumes["app-data"].LastSnapshot != "s3" {
		t.Fatal("a dry-run prune moved the last snapshot")
	}
}

func TestDryRunCollectLeavesImagesRecorded(t *testing.T) {
	world := evidenceWorld()
	world.Nodes["base"].Images[testImage] = true
	next, err := Project(world, Evidence{Kind: EvidenceImagesCollected, Target: "base",
		Observed: map[string]string{"reclaimed": testImage, "dry_run": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if !next.Nodes["base"].Images[testImage] {
		t.Fatal("a dry-run collection forgot an image still on disk")
	}
	real, err := Project(world, Evidence{Kind: EvidenceImagesCollected, Target: "base",
		Observed: map[string]string{"reclaimed": testImage}})
	if err != nil {
		t.Fatal(err)
	}
	if real.Nodes["base"].Images[testImage] {
		t.Fatal("a real collection must still forget the reclaimed image")
	}
}
