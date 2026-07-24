package control

import (
	"strings"
	"testing"
)

func collectWorld() World {
	return World{
		Revision: 1,
		Nodes: map[string]*Node{
			"base": {ID: "base", Healthy: true, Capacity: Resources{CPUMillis: 4000, MemoryMB: 8192},
				Images: map[string]bool{
					"registry.example/web@sha256:" + strings.Repeat("a", 64): true,
					"registry.example/old@sha256:" + strings.Repeat("b", 64): true,
				}},
		},
		Allocations: map[string]*Allocation{
			"web-0": {
				ID: "web-0", Workload: "web", Node: "base",
				Image: "registry.example/web@sha256:" + strings.Repeat("a", 64),
			},
		},
	}
}

// The protected set must name every image a running allocation depends on.
func TestProtectedImagesCoversRunningAllocations(t *testing.T) {
	protected := collectWorld().ProtectedImages()
	if len(protected) != 1 {
		t.Fatalf("protected = %v, want one image", protected)
	}
	if !strings.HasPrefix(protected[0], "registry.example/web@") {
		t.Fatalf("protected the wrong image: %v", protected)
	}
}

// This is the check that keeps garbage collection from deleting an image a
// workload is running on.
func TestCollectImagesRefusesIncompleteProtectedSet(t *testing.T) {
	world := collectWorld()
	err := validateCollectImages(Goal{ID: "g"}, world, Action{
		ID: "gc", Kind: ActionCollectImages, Node: "base",
		Protected: nil,
	})
	if err == nil {
		t.Fatal("expected an empty protected set to be refused")
	}
	if !strings.Contains(err.Error(), "still references") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCollectImagesAcceptsCompleteProtectedSet(t *testing.T) {
	world := collectWorld()
	err := validateCollectImages(Goal{ID: "g"}, world, Action{
		ID: "gc", Kind: ActionCollectImages, Node: "base",
		Protected: world.ProtectedImages(),
	})
	if err != nil {
		t.Fatalf("complete protected set refused: %v", err)
	}
}

// Collecting on a node whose state is unknown risks acting on a partial view.
func TestCollectImagesRefusesUnhealthyNode(t *testing.T) {
	world := collectWorld()
	world.Nodes["base"].Healthy = false
	err := validateCollectImages(Goal{ID: "g"}, world, Action{
		ID: "gc", Kind: ActionCollectImages, Node: "base",
		Protected: world.ProtectedImages(),
	})
	if err == nil {
		t.Fatal("expected collection on an unhealthy node to be refused")
	}
}

func TestCollectImagesRequiresANode(t *testing.T) {
	if err := validateCollectImages(Goal{ID: "g"}, collectWorld(),
		Action{ID: "gc", Kind: ActionCollectImages}); err == nil {
		t.Fatal("expected a collection with no node to be refused")
	}
}

// Reclaimed images must leave the node's image set, or the world would believe
// an image is cached when its bytes are gone.
func TestProjectingCollectionForgetsReclaimedImages(t *testing.T) {
	world := collectWorld()
	old := "registry.example/old@sha256:" + strings.Repeat("b", 64)

	next, err := Project(world, Evidence{
		Kind: EvidenceImagesCollected, Target: "base",
		Observed: map[string]string{"reclaimed": old, "dry_run": "false"},
	})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if next.Nodes["base"].Images[old] {
		t.Fatal("a reclaimed image is still recorded as present")
	}
	// The referenced image must survive.
	kept := "registry.example/web@sha256:" + strings.Repeat("a", 64)
	if !next.Nodes["base"].Images[kept] {
		t.Fatal("collection removed an image an allocation references")
	}
}

// A dry run reports what it would reclaim without the world forgetting
// anything, so an operator can review before destroying storage.
func TestDryRunCollectionRemovesNothing(t *testing.T) {
	world := collectWorld()
	next, err := Project(world, Evidence{
		Kind: EvidenceImagesCollected, Target: "base",
		Observed: map[string]string{"reclaimed": "", "dry_run": "true"},
	})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if len(next.Nodes["base"].Images) != 2 {
		t.Fatalf("dry run changed the image set: %v", next.Nodes["base"].Images)
	}
}

func TestProjectingCollectionForUnknownNodeFails(t *testing.T) {
	_, err := Project(collectWorld(), Evidence{
		Kind: EvidenceImagesCollected, Target: "ghost",
		Observed: map[string]string{"reclaimed": ""},
	})
	if err == nil {
		t.Fatal("expected evidence naming an unknown node to be refused")
	}
}

// The storage agent holds this grant; placement must not be able to delete
// image storage as a side effect of scheduling.
func TestOnlyStorageAgentMayCollectImages(t *testing.T) {
	policy := DefaultPolicy()
	if !policy.Grants["storage-agent"][ActionCollectImages] {
		t.Fatal("storage agent cannot collect images")
	}
	for _, agent := range []string{"placement-agent", "network-agent", "rollout-agent"} {
		if policy.Grants[agent][ActionCollectImages] {
			t.Fatalf("%s must not be granted image collection", agent)
		}
	}
}
