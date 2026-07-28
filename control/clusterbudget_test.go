package control

import (
	"strings"
	"testing"
)

func committedWorld() World {
	world := spreadWorld(map[string]string{"node-a": "rack-1"})
	world.Allocations["web-0"] = &Allocation{
		ID: "web-0", Workload: "web", Node: "node-a", Image: spreadImage,
		Phase: AllocationRunning, Resources: Resources{CPUMillis: 500, MemoryMB: 512},
		Budget: Budget{Tokens: 1000, CostMillis: 100},
	}
	return world
}

func createProposal(resources Resources, budget Budget) Proposal {
	return Proposal{ID: "p", AgentID: "placement-agent", Actions: []Action{{
		ID: "create-web-1", Kind: ActionCreateAllocation, Target: "web-1",
		Resources: resources, Budget: budget,
	}}}
}

// Node capacity bounds one machine. Nothing bounded the total until now.
func TestClusterCeilingBoundsTotalCompute(t *testing.T) {
	world := committedWorld()
	kernel := Kernel{Policy: Policy{ClusterCeiling: Resources{CPUMillis: 800, MemoryMB: 4096}}}

	err := kernel.checkClusterBudget(world, createProposal(Resources{CPUMillis: 400, MemoryMB: 128}, Budget{}))
	if err == nil || !strings.Contains(err.Error(), "cluster cpu ceiling") {
		t.Fatalf("expected a cpu ceiling denial, got %v", err)
	}
	// Just inside the ceiling is allowed.
	if err := kernel.checkClusterBudget(world,
		createProposal(Resources{CPUMillis: 300, MemoryMB: 128}, Budget{})); err != nil {
		t.Fatalf("a proposal inside the ceiling was refused: %v", err)
	}
}

func TestClusterCeilingBoundsMemoryAndCount(t *testing.T) {
	world := committedWorld()

	memory := Kernel{Policy: Policy{ClusterCeiling: Resources{MemoryMB: 600}}}
	err := memory.checkClusterBudget(world, createProposal(Resources{MemoryMB: 200}, Budget{}))
	if err == nil || !strings.Contains(err.Error(), "cluster memory ceiling") {
		t.Fatalf("expected a memory ceiling denial, got %v", err)
	}

	count := Kernel{Policy: Policy{MaxAllocations: 1}}
	err = count.checkClusterBudget(world, createProposal(Resources{MemoryMB: 1}, Budget{}))
	if err == nil || !strings.Contains(err.Error(), "allocation ceiling") {
		t.Fatalf("expected an allocation count denial, got %v", err)
	}
}

// The claim worth making: a runaway agent control loop has a maximum cost.
func TestClusterBudgetBoundsAgentSpend(t *testing.T) {
	world := committedWorld()
	kernel := Kernel{Policy: Policy{ClusterBudget: Budget{Tokens: 1500, CostMillis: 1000}}}

	err := kernel.checkClusterBudget(world,
		createProposal(Resources{CPUMillis: 1}, Budget{Tokens: 1000}))
	if err == nil || !strings.Contains(err.Error(), "cluster agent tokens ceiling") {
		t.Fatalf("expected a token ceiling denial, got %v", err)
	}
	if err := kernel.checkClusterBudget(world,
		createProposal(Resources{CPUMillis: 1}, Budget{Tokens: 400})); err != nil {
		t.Fatalf("a proposal inside the spend ceiling was refused: %v", err)
	}
}

// A zero ceiling means no ceiling, or every Policy written before this existed
// would refuse everything.
func TestZeroCeilingMeansUnlimited(t *testing.T) {
	world := committedWorld()
	kernel := Kernel{Policy: DefaultPolicy()}
	huge := createProposal(Resources{CPUMillis: 1 << 20, MemoryMB: 1 << 20},
		Budget{Tokens: 1 << 20, CostMillis: 1 << 20})
	if err := kernel.checkClusterBudget(world, huge); err != nil {
		t.Fatalf("the default policy imposed a ceiling: %v", err)
	}
}

// Stopped allocations release their commitment, matching how node capacity is
// released, or a churning cluster would shrink its own ceiling permanently.
func TestStoppedAllocationsReleaseCommitment(t *testing.T) {
	world := committedWorld()
	world.Allocations["web-0"].Phase = AllocationStopped

	held := Commitment(world)
	if held.Allocations != 0 || held.Resources.CPUMillis != 0 || held.Budget.Tokens != 0 {
		t.Fatalf("a stopped allocation still held commitment: %+v", held)
	}
	kernel := Kernel{Policy: Policy{ClusterCeiling: Resources{CPUMillis: 600, MemoryMB: 600}}}
	if err := kernel.checkClusterBudget(world,
		createProposal(Resources{CPUMillis: 500, MemoryMB: 500}, Budget{})); err != nil {
		t.Fatalf("released capacity was not reusable: %v", err)
	}
}

// A proposal that creates nothing commits nothing, so removal is never blocked
// by a ceiling. A cluster at its limit must still be able to shrink.
func TestCeilingDoesNotBlockRemoval(t *testing.T) {
	world := committedWorld()
	kernel := Kernel{Policy: Policy{
		MaxAllocations: 1, ClusterCeiling: Resources{CPUMillis: 1, MemoryMB: 1},
	}}
	if err := kernel.checkClusterBudget(world, stopProposal("web-0")); err != nil {
		t.Fatalf("a full cluster could not shrink: %v", err)
	}
}
