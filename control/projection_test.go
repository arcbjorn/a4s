package control

import (
	"strings"
	"testing"
)

func projectionWorld() World {
	world := World{Nodes: map[string]*Node{
		"base": {
			ID: "base", Healthy: true, Labels: map[string]string{"pool": "base"},
			Capacity: Resources{CPUMillis: 4000, MemoryMB: 8192},
		},
	}}
	world.normalize()
	return world
}

func createdEvidence() Evidence {
	return Evidence{
		Kind: EvidenceAllocationCreated, Target: "app-0",
		Observed: map[string]string{
			"node": "base", "workload": "app", "image": testImage,
			"replica": "0", "cpu_millis": "100", "memory_mb": "128",
		},
	}
}

// Replaying an action after a crash between host mutation and result recording
// must not double-count capacity. The projection is therefore idempotent.
func TestProjectionIsIdempotentForRepeatedCreation(t *testing.T) {
	world := projectionWorld()
	first, err := Project(world, createdEvidence())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Project(first, createdEvidence())
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Nodes["base"].Used; got != (Resources{CPUMillis: 100, MemoryMB: 128}) {
		t.Fatalf("replayed creation double-counted capacity: %+v", got)
	}
	if len(second.Allocations) != 1 {
		t.Fatalf("replayed creation duplicated allocations: %+v", second.Allocations)
	}
}

// Project must never mutate its input, so a failed or rejected projection
// cannot leave the authoritative world half-updated.
func TestProjectDoesNotMutateInputWorld(t *testing.T) {
	world := projectionWorld()
	if _, err := Project(world, createdEvidence()); err != nil {
		t.Fatal(err)
	}
	if len(world.Allocations) != 0 || world.Nodes["base"].Used != (Resources{}) {
		t.Fatalf("Project mutated its input world: %+v", world)
	}
}

// A running allocation is not a ready one. Readiness requires separate evidence
// so an executor cannot declare its own work successful.
func TestRunningEvidenceDoesNotImplyReadiness(t *testing.T) {
	world := projectionWorld()
	world, err := Project(world, createdEvidence())
	if err != nil {
		t.Fatal(err)
	}
	world, err = Project(world, Evidence{
		Kind: EvidenceAllocationRunning, Target: "app-0",
		Observed: map[string]string{"node": "base"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if world.Allocations["app-0"].Ready {
		t.Fatal("running evidence marked the allocation ready")
	}
	world, err = Project(world, Evidence{
		Kind: EvidenceAllocationReady, Target: "app-0",
		Observed: map[string]string{"ready": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !world.Allocations["app-0"].Ready {
		t.Fatal("probe evidence did not mark the allocation ready")
	}
}

// Readiness for an allocation that was never observed running is incoherent and
// must be refused rather than silently accepted.
func TestProjectionRejectsReadinessWithoutRunning(t *testing.T) {
	world := projectionWorld()
	world, err := Project(world, createdEvidence())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Project(world, Evidence{
		Kind: EvidenceAllocationReady, Target: "app-0",
		Observed: map[string]string{"ready": "true"},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be ready in phase") {
		t.Fatalf("expected phase rejection, got %v", err)
	}
}

func TestProjectionRejectsUnknownEvidenceKind(t *testing.T) {
	if _, err := Project(projectionWorld(), Evidence{Kind: "invented.kind", Target: "app-0"}); err == nil ||
		!strings.Contains(err.Error(), "unknown evidence kind") {
		t.Fatalf("expected unknown evidence rejection, got %v", err)
	}
}

// The executor reports what it did; it must not assert readiness. This is the
// structural guarantee behind independent verification.
func TestMemoryExecutorNeverAssertsReadiness(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	executor := NewMemoryExecutor(scenario.World)
	for _, action := range []Action{
		{Kind: ActionPullImage, Target: testImage, Node: "base", Image: testImage},
		{
			Kind: ActionCreateAllocation, Target: "app-0", Workload: "app", Node: "base",
			Image: testImage, Resources: scenario.Goal.Workload.Resources,
		},
		{Kind: ActionStartAllocation, Target: "app-0", Workload: "app"},
	} {
		evidence, err := executor.Execute(action)
		if err != nil {
			t.Fatal(err)
		}
		if evidence.Kind == EvidenceAllocationReady || evidence.Observed["ready"] == "true" {
			t.Fatalf("executor asserted readiness: %+v", evidence)
		}
		if err := executor.Project(evidence); err != nil {
			t.Fatal(err)
		}
	}
	if executor.World().Allocations["app-0"].Ready {
		t.Fatal("executor evidence alone marked the allocation ready")
	}
}
