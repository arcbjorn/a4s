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

// The tool count is node-supplied and sizes an allocation. A negative value
// must be refused, not panic the projection on every replay of the log.
func TestToolCountIsBounded(t *testing.T) {
	for _, count := range []string{"-1", "33", "9223372036854775807"} {
		t.Run(count, func(t *testing.T) {
			world := evidenceWorld()
			world.Allocations["a-0"] = &Allocation{ID: "a-0", Workload: "a", Node: "base",
				Phase: AllocationCreated}
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("a tool count of %s panicked the projection: %v", count, recovered)
				}
			}()
			if _, err := Project(world, Evidence{Kind: EvidenceToolsGranted, Target: "a-0",
				Observed: map[string]string{"count": count}}); err == nil {
				t.Fatalf("a tool count of %s was accepted", count)
			}
		})
	}
	world := evidenceWorld()
	world.Allocations["a-0"] = &Allocation{ID: "a-0", Workload: "a", Node: "base", Phase: AllocationCreated}
	next, err := Project(world, Evidence{Kind: EvidenceToolsGranted, Target: "a-0",
		Observed: map[string]string{"count": "3"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(next.Allocations["a-0"].Tools); got != 3 {
		t.Fatalf("recorded %d tools, want 3", got)
	}
}

// Spend only increases, in every dimension. A stale tool-call count alone must
// not un-exhaust an agent whose token spend still reads current.
func TestStaleSpendCannotUnexhaustAnAgent(t *testing.T) {
	world := evidenceWorld()
	world.Allocations["a-0"] = &Allocation{ID: "a-0", Workload: "a", Node: "base", Phase: AllocationRunning,
		Budget: Budget{Tokens: 1000, CostMillis: 1000, WallSeconds: 900, ToolCalls: 50},
		Spent:  Budget{Tokens: 10, CostMillis: 10, WallSeconds: 100, ToolCalls: 50}}
	next, err := Project(world, Evidence{Kind: EvidenceAgentSpent, Target: "a-0", Observed: map[string]string{
		"tokens": "20", "cost_millis": "10", "wall_seconds": "90", "tool_calls": "3"}})
	if err != nil {
		t.Fatal(err)
	}
	spent := next.Allocations["a-0"].Spent
	if !next.Allocations["a-0"].Exhausted() {
		t.Fatalf("a stale reading un-exhausted the agent: spent %+v", spent)
	}
	want := Budget{Tokens: 20, CostMillis: 10, WallSeconds: 100, ToolCalls: 50}
	if spent != want {
		t.Fatalf("spent = %+v, want the larger reading in each dimension %+v", spent, want)
	}
}

// Deleting an agent allocation releases the node budget it reserved, exactly
// as simulation does, or a node's agent capacity drains with every deletion.
func TestDeleteReleasesAgentBudget(t *testing.T) {
	world := evidenceWorld()
	for _, evidence := range []Evidence{
		{Kind: EvidenceAllocationCreated, Target: "triage-0", Observed: map[string]string{
			"node": "base", "workload": "triage", "image": testImage, "replica": "0",
			"cpu_millis": "100", "memory_mb": "128",
			"tokens": "10", "cost_millis": "10", "wall_seconds": "10", "tool_calls": "10"}},
		{Kind: EvidenceAllocationDeleted, Target: "triage-0", Observed: map[string]string{"deleted": "true"}},
	} {
		next, err := Project(world, evidence)
		if err != nil {
			t.Fatal(err)
		}
		world = next
	}
	node := world.Nodes["base"]
	if !node.BudgetUsed.IsZero() {
		t.Fatalf("deletion leaked agent budget %+v", node.BudgetUsed)
	}
	if node.Used != (Resources{}) {
		t.Fatalf("deletion leaked resources %+v", node.Used)
	}
}
