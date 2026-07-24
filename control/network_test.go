package control

import (
	"strings"
	"testing"
)

// The correctness ceiling this removes: before per-allocation addressing, two
// replicas on one node shared the host network and contended for the same port.
func TestReplicasOnOneNodeGetDistinctAddresses(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Workload.Replicas = 3
	scenario.Goal.Route = nil
	// Only one node satisfies the constraints, so every replica lands together.
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}

	executor := NewMemoryExecutor(scenario.World)
	engine := NewEngine(executor, PlacementAgent{})
	if err := engine.Run(scenario.Goal, 20); err != nil {
		t.Fatalf("multi-replica placement did not converge: %v", err)
	}

	world := executor.World()
	if len(world.Allocations) != 3 {
		t.Fatalf("expected three replicas, got %d: %+v", len(world.Allocations), world.Allocations)
	}

	addresses := make(map[string]string)
	for id, allocation := range world.Allocations {
		if allocation.Node != "base" {
			t.Fatalf("replica %q was placed off the constrained node: %+v", id, allocation)
		}
		if allocation.Address == "" {
			t.Fatalf("replica %q has no address: %+v", id, allocation)
		}
		if existing, clash := addresses[allocation.Address]; clash {
			t.Fatalf("replicas %q and %q share address %s", existing, id, allocation.Address)
		}
		addresses[allocation.Address] = id
	}
}

// A workload that serves a port must not start before it has an address.
// Starting first would leave it either unreachable or contending for a host
// port with its own replicas.
func TestKernelRefusesStartWithoutAddress(t *testing.T) {
	goal := validScenario().Goal
	world := World{Nodes: map[string]*Node{
		"base": {
			ID: "base", Healthy: true, Labels: map[string]string{"pool": "base"},
			Capacity: Resources{CPUMillis: 4000, MemoryMB: 8192},
		},
	}}
	world.normalize()
	world.Allocations["app-0"] = &Allocation{
		ID: "app-0", Workload: "app", Node: "base", Image: testImage,
		Resources: goal.Workload.Resources, Phase: AllocationCreated,
	}

	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "start-app-0", Kind: ActionStartAllocation,
			Target: "app-0", Workload: "app", Node: "base",
		}},
		ExpectedEvidence: []Check{{Kind: CheckAllocationReady, Target: "app-0", Want: "true"}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(PlacementAgent{}).Descriptor(), goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "no network address") {
		t.Fatalf("kernel started a networked workload with no address: %v", err)
	}
}

// A workload with no port needs no address, so it must not be blocked by the
// network requirement.
func TestPortlessWorkloadNeedsNoAddress(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Route = nil
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	// Validation requires a port, so drop it after validation to model a
	// batch workload that serves nothing.
	goal := scenario.Goal
	goal.Workload.Port = 0

	proposal, err := (PlacementAgent{}).Propose(goal, scenario.World)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range proposal.Actions {
		if action.Kind == ActionAttachNetwork {
			t.Fatalf("a portless workload was given a network attachment: %+v", action)
		}
	}
	if err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(PlacementAgent{}).Descriptor(), goal, scenario.World, proposal); err != nil {
		t.Fatalf("kernel refused a portless workload: %v", err)
	}
}

// Attachment evidence must record the address, since the world is what the
// probe path consults to find where a workload listens.
func TestNetworkEvidenceRecordsAddress(t *testing.T) {
	world := projectionWorld()
	world, err := Project(world, createdEvidence())
	if err != nil {
		t.Fatal(err)
	}
	world, err = Project(world, Evidence{
		Kind: EvidenceNetworkAttached, Target: "app-0",
		Observed: map[string]string{"address": "10.42.0.7", "namespace": "/var/run/a4s/netns/app-0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if world.Allocations["app-0"].Address != "10.42.0.7" {
		t.Fatalf("attachment evidence did not record the address: %+v", world.Allocations["app-0"])
	}
}

// Attachment evidence without an address is incoherent and must be refused
// rather than silently recording an empty one.
func TestNetworkEvidenceRequiresAddress(t *testing.T) {
	world := projectionWorld()
	world, err := Project(world, createdEvidence())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Project(world, Evidence{
		Kind: EvidenceNetworkAttached, Target: "app-0",
		Observed: map[string]string{"namespace": "/var/run/a4s/netns/app-0"},
	})
	if err == nil || !strings.Contains(err.Error(), "must observe an address") {
		t.Fatalf("expected an address requirement, got %v", err)
	}
}

// Deleting an allocation must clear its address, so a released IP is not left
// looking assigned.
func TestDeletionClearsAddress(t *testing.T) {
	world := projectionWorld()
	world, err := Project(world, createdEvidence())
	if err != nil {
		t.Fatal(err)
	}
	world, err = Project(world, Evidence{
		Kind: EvidenceNetworkAttached, Target: "app-0",
		Observed: map[string]string{"address": "10.42.0.7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	world, err = Project(world, Evidence{
		Kind: EvidenceNetworkDetached, Target: "app-0",
		Observed: map[string]string{"released": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if world.Allocations["app-0"].Address != "" {
		t.Fatalf("detachment left a stale address: %+v", world.Allocations["app-0"])
	}
}

// A replayed detach must succeed rather than failing on an allocation that is
// already gone.
func TestDetachIsIdempotent(t *testing.T) {
	world := projectionWorld()
	detached := Evidence{
		Kind: EvidenceNetworkDetached, Target: "never-existed",
		Observed: map[string]string{"released": "false"},
	}
	if _, err := Project(world, detached); err != nil {
		t.Fatalf("replayed detach failed: %v", err)
	}
}

// Placement must stay within the kernel's action budget as replica count grows,
// or a large workload would be permanently unschedulable.
func TestPlacementStaysWithinActionBudget(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Workload.Replicas = 6
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	proposal, err := (PlacementAgent{}).Propose(scenario.Goal, scenario.World)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Actions) > DefaultPolicy().MaxActionsPerProposal {
		t.Fatalf("placement proposed %d actions, over the %d limit",
			len(proposal.Actions), DefaultPolicy().MaxActionsPerProposal)
	}
	if err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(PlacementAgent{}).Descriptor(), scenario.Goal, scenario.World, proposal); err != nil {
		t.Fatalf("kernel refused a batched placement: %v", err)
	}
}
