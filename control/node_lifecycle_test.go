package control

import (
	"strings"
	"testing"
)

func lifecycleWorld() World {
	world := spreadWorld(map[string]string{"node-a": "rack-1", "node-b": "rack-2"})
	world.Allocations["web-0"] = &Allocation{
		ID: "web-0", Workload: "web", Node: "node-a", Image: spreadImage,
		Phase: AllocationRunning, Ready: true,
		Resources: Resources{CPUMillis: 100, MemoryMB: 128},
	}
	world.Allocations["db-0"] = &Allocation{
		ID: "db-0", Workload: "db", Node: "node-a", Image: spreadImage,
		Phase: AllocationRunning, Ready: true, Stateful: true,
		Resources: Resources{CPUMillis: 100, MemoryMB: 128},
	}
	return world
}

// Health and cordon mean different things. A node that healed mid-evacuation
// must stay out of service until something decides otherwise.
func TestCordonIsIndependentOfHealth(t *testing.T) {
	node := &Node{ID: "node-a", Healthy: true}
	if !node.Schedulable() {
		t.Fatal("a healthy uncordoned node is not schedulable")
	}
	node.Cordoned = true
	if node.Schedulable() {
		t.Fatal("a cordoned node is still schedulable")
	}
	node.Healthy = false
	node.Cordoned = false
	if node.Schedulable() {
		t.Fatal("an unhealthy node is schedulable")
	}
}

// The kernel refuses to place on a cordoned node, whoever proposed it. A cordon
// only the proposing agent respected would be advisory.
func TestKernelRefusesPlacementOnACordonedNode(t *testing.T) {
	world := lifecycleWorld()
	world.Nodes["node-a"].Cordoned = true
	world.Nodes["node-a"].CordonReason = "disk failing"
	goal := spreadGoal(1, 0)

	err := validateAction(goal, world, Action{
		ID: "create", Kind: ActionCreateAllocation, Target: "web-1",
		Workload: "web", Node: "node-a", Image: spreadImage, Replica: 0,
		Resources: goal.Workload.Resources,
	})
	if err == nil || !strings.Contains(err.Error(), "cordoned") {
		t.Fatalf("expected a cordon denial, got %v", err)
	}
	if !strings.Contains(err.Error(), "disk failing") {
		t.Fatalf("the denial did not carry the reason: %v", err)
	}
}

// New durable state must not land on a node being emptied, or the drain never
// finishes.
func TestKernelRefusesNewVolumeOnACordonedNode(t *testing.T) {
	world := lifecycleWorld()
	world.Nodes["node-b"].Cordoned = true
	goal := spreadGoal(1, 0)
	goal.Workload.Volumes = []VolumeRef{{Name: "data", MountPath: "/data"}}

	err := validateAction(goal, world, Action{
		ID: "create-volume", Kind: ActionCreateVolume, Target: "data",
		Workload: "web", Node: "node-b", Volume: &VolumeRef{Name: "data", MountPath: "/data"},
	})
	if err == nil || !strings.Contains(err.Error(), "cordoned") {
		t.Fatalf("expected a cordon denial for new storage, got %v", err)
	}
}

// The placement agent must not select a cordoned node, or every round would
// propose work the kernel refuses.
func TestPlacementAgentSkipsCordonedNodes(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1", "node-b": "rack-2"})
	world.Nodes["node-a"].Cordoned = true

	proposal, err := (PlacementAgent{}).Propose(spreadGoal(1, 0), world)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range proposal.Actions {
		if action.Node == "node-a" {
			t.Fatalf("action %q was placed on a cordoned node", action.ID)
		}
	}
}

// Cordoning is idempotent, because a retry after an ambiguous failure must not
// be refused.
func TestCordonIsIdempotent(t *testing.T) {
	world := lifecycleWorld()
	action := Action{ID: "cordon", Kind: ActionCordonNode, Target: "node-a", Reason: "maintenance"}
	if err := validateAction(spreadGoal(1, 0), world, action); err != nil {
		t.Fatalf("first cordon refused: %v", err)
	}
	world.Nodes["node-a"].Cordoned = true
	if err := validateAction(spreadGoal(1, 0), world, action); err != nil {
		t.Fatalf("repeating a cordon was refused: %v", err)
	}
	if err := validateAction(spreadGoal(1, 0), world, Action{
		ID: "cordon", Kind: ActionCordonNode, Target: "ghost",
	}); err == nil {
		t.Fatal("cordoning an unknown node was accepted")
	}
}

// An unhealthy node must still be uncordonable, or a machine cordoned while down
// could never be released without first coming back.
func TestUnhealthyNodeCanBeUncordoned(t *testing.T) {
	world := lifecycleWorld()
	world.Nodes["node-a"].Healthy = false
	world.Nodes["node-a"].Cordoned = true
	if err := validateAction(spreadGoal(1, 0), world, Action{
		ID: "uncordon", Kind: ActionUncordonNode, Target: "node-a",
	}); err != nil {
		t.Fatalf("uncordoning a down node was refused: %v", err)
	}
}

// Cordon must settle in the control plane. Taking an unresponsive node out of
// service cannot require that node's cooperation.
func TestCordonIsControlPlaneLocal(t *testing.T) {
	if !ActionCordonNode.ControlPlaneLocal() || !ActionUncordonNode.ControlPlaneLocal() {
		t.Fatal("cordon actions are not control-plane local")
	}
	for _, kind := range []ActionKind{
		ActionCreateAllocation, ActionStartAllocation, ActionStopAllocation, ActionPullImage,
	} {
		if kind.ControlPlaneLocal() {
			t.Fatalf("%s was treated as control-plane local", kind)
		}
	}

	evidence, err := CordonEvidence(Action{
		Kind: ActionCordonNode, Target: "node-a", Reason: "disk failing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Kind != EvidenceNodeCordoned || evidence.Target != "node-a" {
		t.Fatalf("unexpected evidence %+v", evidence)
	}
	if evidence.Observed["reason"] != "disk failing" {
		t.Fatal("the reason did not reach the evidence")
	}
	if _, err := CordonEvidence(Action{Kind: ActionCordonNode}); err == nil {
		t.Fatal("cordon evidence without a node was accepted")
	}
}

// The cordon must survive a rebuild, because it is what holds a drain in place
// across a control-plane restart.
func TestCordonProjectsAndPersists(t *testing.T) {
	world := lifecycleWorld()
	next, err := Project(world, Evidence{
		Kind: EvidenceNodeCordoned, Target: "node-a",
		Observed: map[string]string{"reason": "draining"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !next.Nodes["node-a"].Cordoned || next.Nodes["node-a"].CordonReason != "draining" {
		t.Fatalf("cordon did not project: %+v", next.Nodes["node-a"])
	}
	if world.Nodes["node-a"].Cordoned {
		t.Fatal("Project mutated the world it was given")
	}

	back, err := Project(next, Evidence{Kind: EvidenceNodeUncordoned, Target: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if back.Nodes["node-a"].Cordoned || back.Nodes["node-a"].CordonReason != "" {
		t.Fatal("uncordon did not clear the cordon")
	}
	if _, err := Project(world, Evidence{Kind: EvidenceNodeCordoned, Target: "ghost"}); err == nil {
		t.Fatal("cordon evidence for an unknown node was projected")
	}
}

// Evacuation reports what a drain would cost, and separates the allocations that
// cannot simply be recreated elsewhere.
func TestEvacuationSeparatesStatefulWork(t *testing.T) {
	world := lifecycleWorld()
	evacuation := PlanEvacuation(world, "node-a")
	if len(evacuation.Allocations) != 2 {
		t.Fatalf("expected two allocations to move, got %v", evacuation.Allocations)
	}
	if len(evacuation.Stateful) != 1 || evacuation.Stateful[0] != "db-0" {
		t.Fatalf("stateful set = %v, want [db-0]", evacuation.Stateful)
	}

	// A stopped allocation needs no evacuation.
	world.Allocations["web-0"].Phase = AllocationStopped
	if got := PlanEvacuation(world, "node-a"); len(got.Allocations) != 1 {
		t.Fatalf("a stopped allocation was counted: %v", got.Allocations)
	}
	if empty := PlanEvacuation(world, "node-b"); !empty.Empty() {
		t.Fatalf("an empty node reported work: %v", empty.Allocations)
	}
}

// Automatic cordoning must consider each unhealthy node once, or it would
// re-propose the same decision every round.
func TestUnhealthyNodesExcludesAlreadyCordoned(t *testing.T) {
	world := lifecycleWorld()
	world.Nodes["node-a"].Healthy = false
	if got := UnhealthyNodes(world); len(got) != 1 || got[0] != "node-a" {
		t.Fatalf("UnhealthyNodes = %v, want [node-a]", got)
	}
	world.Nodes["node-a"].Cordoned = true
	if got := UnhealthyNodes(world); len(got) != 0 {
		t.Fatalf("an already-cordoned node was reported again: %v", got)
	}
}
