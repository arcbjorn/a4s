package control

import (
	"errors"
	"strings"
	"testing"
)

const nextImage = "registry.example/app@sha256:1111111111111111111111111111111111111111111111111111111111111111"

// rolloutWorld builds a world already running `replicas` allocations of the
// goal's workload on the old image.
func rolloutWorld(goal Goal, replicas int, image string) World {
	world := World{Nodes: map[string]*Node{
		"base": {
			ID: "base", Healthy: true, Labels: map[string]string{"pool": "base"},
			Capacity: Resources{CPUMillis: 8000, MemoryMB: 16384},
		},
	}}
	world.normalize()
	for replica := 0; replica < replicas; replica++ {
		id := allocationID(goal.Workload.Name, replica)
		world.Allocations[id] = &Allocation{
			ID: id, Workload: goal.Workload.Name, Replica: replica, Node: "base",
			Image: image, Resources: goal.Workload.Resources,
			Phase: AllocationRunning, Ready: true,
		}
		world.Nodes["base"].Used = world.Nodes["base"].Used.Add(goal.Workload.Resources)
	}
	return world
}

func allocationID(workload string, replica int) string {
	return workload + "-" + string(rune('0'+replica))
}

// A workload whose image changed must be retired by the rollout agent rather
// than left in place or duplicated.
func TestRolloutRetiresDriftedAllocation(t *testing.T) {
	goal := validScenario().Goal
	goal.Workload.Replicas = 2
	goal.Workload.Image = nextImage
	world := rolloutWorld(goal, 2, testImage)

	proposal, err := (RolloutAgent{}).Propose(goal, world)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Actions) != 2 {
		t.Fatalf("expected a stop and a delete, got %+v", proposal.Actions)
	}
	if proposal.Actions[0].Kind != ActionStopAllocation || proposal.Actions[1].Kind != ActionDeleteAllocation {
		t.Fatalf("unexpected rollout plan: %+v", proposal.Actions)
	}
	// Delete must depend on stop, or the plan could destroy a running workload.
	if len(proposal.Actions[1].DependsOn) != 1 || proposal.Actions[1].DependsOn[0] != proposal.Actions[0].ID {
		t.Fatalf("delete does not depend on stop: %+v", proposal.Actions[1])
	}
	// Only one allocation is disrupted per proposal.
	if proposal.Actions[0].Target != "app-0" {
		t.Fatalf("expected the first allocation to be retired, got %q", proposal.Actions[0].Target)
	}
}

// The rollout must stop before it takes the workload below its availability
// floor. This is what separates a rolling update from an outage.
func TestRolloutRespectsAvailabilityFloor(t *testing.T) {
	goal := validScenario().Goal
	goal.Workload.Replicas = 2
	goal.Workload.Image = nextImage
	world := rolloutWorld(goal, 2, testImage)
	// One replica has already been retired and not yet replaced.
	delete(world.Allocations, "app-1")

	_, err := (RolloutAgent{}).Propose(goal, world)
	if err == nil || !strings.Contains(err.Error(), "availability floor") {
		t.Fatalf("expected the availability budget to block disruption, got %v", err)
	}
}

// The kernel must enforce the floor itself, so a buggy or hostile agent cannot
// exceed it by simply proposing the action anyway.
func TestKernelRejectsDisruptionBelowFloor(t *testing.T) {
	goal := validScenario().Goal
	goal.Workload.Replicas = 2
	world := rolloutWorld(goal, 1, testImage)

	proposal := Proposal{
		ID: "hostile-r0", AgentID: "rollout-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "stop-app-0", Kind: ActionStopAllocation,
			Target: "app-0", Workload: goal.Workload.Name,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(RolloutAgent{}).Descriptor(), goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "availability floor") {
		t.Fatalf("kernel allowed disruption below the floor: %v", err)
	}
}

// A single-replica workload has no floor to preserve, so its update is allowed
// to have a gap rather than being permanently blocked.
func TestSingleReplicaRolloutIsAllowed(t *testing.T) {
	goal := validScenario().Goal
	goal.Workload.Image = nextImage
	world := rolloutWorld(goal, 1, testImage)

	proposal, err := (RolloutAgent{}).Propose(goal, world)
	if err != nil {
		t.Fatalf("single-replica rollout was blocked: %v", err)
	}
	if len(proposal.Actions) == 0 {
		t.Fatal("single-replica rollout produced no plan")
	}
}

// A stopped allocation costs no availability, so it is cleared first without
// consuming the disruption budget.
func TestRolloutClearsStoppedAllocationFirst(t *testing.T) {
	goal := validScenario().Goal
	goal.Workload.Replicas = 2
	goal.Workload.Image = nextImage
	world := rolloutWorld(goal, 2, testImage)
	world.Allocations["app-1"].Phase = AllocationStopped
	world.Allocations["app-1"].Ready = false

	proposal, err := (RolloutAgent{}).Propose(goal, world)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Actions) != 1 || proposal.Actions[0].Kind != ActionDeleteAllocation {
		t.Fatalf("expected a lone delete for the stopped allocation: %+v", proposal.Actions)
	}
	if proposal.Actions[0].Target != "app-1" {
		t.Fatalf("expected the stopped allocation to be cleared: %+v", proposal.Actions[0])
	}
}

// Placement must not consider a drifted allocation as filling its replica slot,
// or the replacement would never be created after the rollout retires it.
func TestPlacementReplacesRetiredAllocation(t *testing.T) {
	goal := validScenario().Goal
	goal.Workload.Image = nextImage
	world := rolloutWorld(goal, 1, testImage)

	proposal, err := (PlacementAgent{}).Propose(goal, world)
	if err != nil {
		t.Fatalf("placement refused to plan a replacement: %v", err)
	}
	if len(proposal.Actions) == 0 {
		t.Fatal("placement produced no replacement plan for a drifted allocation")
	}
	for _, action := range proposal.Actions {
		if action.Kind == ActionCreateAllocation && action.Image != nextImage {
			t.Fatalf("replacement used the old image: %+v", action)
		}
	}
}

// The end-to-end update: a goal whose image changed converges to a running,
// ready allocation on the new image with the old one gone.
func TestRolloutConvergesToNewImage(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	goal := scenario.Goal
	goal.Route = nil

	world := rolloutWorld(goal, 1, testImage)
	world.Approvals = scenario.World.Approvals
	goal.Workload.Image = nextImage

	executor := NewMemoryExecutor(world)
	engine := NewEngine(executor, RolloutAgent{}, PlacementAgent{})
	if err := engine.Run(goal, 12); err != nil {
		t.Fatalf("rollout did not converge: %v", err)
	}

	final := executor.World()
	if len(final.Allocations) != 1 {
		t.Fatalf("expected exactly one allocation after rollout: %+v", final.Allocations)
	}
	for _, allocation := range final.Allocations {
		if allocation.Image != nextImage {
			t.Fatalf("allocation still runs the old image: %+v", allocation)
		}
		if !allocation.ReadyAt(final.Now()) {
			t.Fatalf("replacement is not ready: %+v", allocation)
		}
	}
	// Capacity must reflect one allocation, not the retired one as well.
	if final.Nodes["base"].Used != goal.Workload.Resources {
		t.Fatalf("rollout leaked capacity: %+v", final.Nodes["base"].Used)
	}
}

// A version that has been observed serving becomes the rollback target. It is
// recorded from readiness evidence, so the target is always one this cluster
// actually saw working.
func TestKnownGoodIsRecordedFromReadiness(t *testing.T) {
	world := projectionWorld()
	world, err := Project(world, createdEvidence())
	if err != nil {
		t.Fatal(err)
	}
	world, err = Project(world, Evidence{Kind: EvidenceAllocationRunning, Target: "app-0",
		Observed: map[string]string{"node": "base"}})
	if err != nil {
		t.Fatal(err)
	}
	if world.KnownGood["app"] != "" {
		t.Fatal("a merely running version was recorded as known good")
	}
	world, err = Project(world, Evidence{Kind: EvidenceAllocationReady, Target: "app-0",
		Observed: map[string]string{"ready": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if world.KnownGood["app"] != testImage {
		t.Fatalf("readiness did not record a known-good image: %+v", world.KnownGood)
	}
}

// A replacement that is still starting must not trigger a rollback, or every
// deployment would revert before it had a chance to come up.
func TestStartingReplacementDoesNotTriggerRollback(t *testing.T) {
	goal := validScenario().Goal
	goal.Workload.Image = nextImage
	world := rolloutWorld(goal, 1, nextImage)
	world.KnownGood = map[string]string{"app": testImage}
	// The replacement exists and is running but has not reported readiness yet.
	world.Allocations["app-0"].Ready = false
	world.Allocations["app-0"].Phase = AllocationRunning

	if _, failed := RollbackTarget(goal, world); failed {
		t.Fatal("a starting replacement was treated as a failed rollout")
	}
}

// A replacement observed crashing is a failed rollout, and the known-good image
// must be surfaced rather than silently applied.
func TestFailedReplacementRequestsRollback(t *testing.T) {
	goal := validScenario().Goal
	goal.Workload.Image = nextImage
	world := rolloutWorld(goal, 1, nextImage)
	world.KnownGood = map[string]string{"app": testImage}
	world.Allocations["app-0"].Phase = AllocationStopped
	world.Allocations["app-0"].Ready = false
	world.Allocations["app-0"].ExitCode = 1

	target, failed := RollbackTarget(goal, world)
	if !failed || target != testImage {
		t.Fatalf("expected a rollback to the known-good image: target=%q failed=%t", target, failed)
	}

	_, err := (RolloutAgent{}).Propose(goal, world)
	var rollback *RollbackRequired
	if !errors.As(err, &rollback) {
		t.Fatalf("expected a RollbackRequired error, got %v", err)
	}
	if rollback.KnownGood != testImage || rollback.Failed != nextImage {
		t.Fatalf("rollback did not name the right versions: %+v", rollback)
	}
}

// An agent must never rewrite the goal. A required rollback blocks the goal with
// the evidence needed to decide, rather than quietly running another version.
func TestRollbackBlocksGoalRatherThanRewritingIt(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	goal := scenario.Goal
	goal.Route = nil
	goal.Workload.Image = nextImage

	world := rolloutWorld(goal, 1, nextImage)
	world.KnownGood = map[string]string{"app": testImage}
	world.Allocations["app-0"].Phase = AllocationStopped
	world.Allocations["app-0"].Ready = false
	world.Allocations["app-0"].ExitCode = 137

	engine := NewEngine(NewMemoryExecutor(world), RolloutAgent{}, PlacementAgent{})
	err := engine.Run(goal, 5)
	var rollback *RollbackRequired
	if !errors.As(err, &rollback) {
		t.Fatalf("expected the goal to block on a rollback decision, got %v", err)
	}
	blocked := false
	for _, event := range engine.Events {
		if event.Type == EventGoalBlocked && strings.Contains(event.Message, "last known-good image") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("no blocked-goal event named the known-good image: %+v", engine.Events)
	}
}

// Once the operator adopts the known-good image, reconciliation proceeds
// normally instead of continuing to report a rollback.
func TestAdoptingKnownGoodClearsRollback(t *testing.T) {
	goal := validScenario().Goal
	goal.Workload.Image = testImage
	world := rolloutWorld(goal, 1, nextImage)
	world.KnownGood = map[string]string{"app": testImage}
	world.Allocations["app-0"].Phase = AllocationStopped
	world.Allocations["app-0"].ExitCode = 1

	if _, failed := RollbackTarget(goal, world); failed {
		t.Fatal("a goal already naming the known-good image still requested rollback")
	}
}
