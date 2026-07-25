package control

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// failedRolloutWorld builds a world where the goal's version crashed and an
// earlier version is recorded as known-good.
func failedRolloutWorld(t *testing.T) (Goal, World) {
	t.Helper()
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
	return goal, world
}

// approveRollback records a live operator grant for the rollback scope.
func approveRollback(world *World, goalID, failed, target string) {
	now := world.Now()
	if world.Approvals == nil {
		world.Approvals = map[string]*Approval{}
	}
	world.Approvals["rollback-"+goalID] = &Approval{
		ID: "rollback-" + goalID, GoalID: goalID, Scope: "rollback",
		Granted: true, IssuedBy: "arc", IssuedAt: now.Add(-time.Minute),
		ExpiresAt: now.Add(time.Hour),
		Subject:   failed, Rollback: target,
	}
}

// Without an approval the rollback stays a reported obstacle. This is the
// existing behaviour and must not regress: an agent may not redefine a goal.
func TestCompensationRequiresApproval(t *testing.T) {
	goal, world := failedRolloutWorld(t)

	if _, _, compensating := CompensatedGoal(goal, world); compensating {
		t.Fatal("compensation applied without an operator approval")
	}
	if _, err := (RolloutAgent{}).Propose(goal, world); err == nil {
		t.Fatal("expected an unapproved rollback to be refused")
	}
}

// An approved rollback resolves the goal to the known-good image, which is what
// makes every agent in the round agree on which version to run.
func TestApprovedRollbackResolvesToKnownGood(t *testing.T) {
	goal, world := failedRolloutWorld(t)
	approveRollback(&world, goal.ID, nextImage, testImage)

	effective, failedImage, compensating := CompensatedGoal(goal, world)
	if !compensating {
		t.Fatal("an approved rollback did not compensate")
	}
	if effective.Workload.Image != testImage {
		t.Fatalf("effective image = %q, want the known-good %q",
			effective.Workload.Image, testImage)
	}
	if failedImage != nextImage {
		t.Fatalf("failed image = %q, want %q", failedImage, nextImage)
	}
	// The caller's goal must not be mutated; compensation returns a copy.
	if goal.Workload.Image != nextImage {
		t.Fatal("compensation mutated the operator's goal")
	}
}

// The approval is scoped to one goal, so approving a rollback for one workload
// must not silently authorize another.
func TestRollbackApprovalIsScopedToItsGoal(t *testing.T) {
	goal, world := failedRolloutWorld(t)
	approveRollback(&world, "some-other-goal", nextImage, testImage)

	if _, _, compensating := CompensatedGoal(goal, world); compensating {
		t.Fatal("an approval for another goal authorized this rollback")
	}
}

// An expired approval must not keep a workload pinned to an old version
// forever, which is the whole reason approvals carry an expiry.
func TestExpiredRollbackApprovalDoesNotCompensate(t *testing.T) {
	goal, world := failedRolloutWorld(t)
	now := world.Now()
	world.Approvals = map[string]*Approval{
		"rollback-" + goal.ID: {
			ID: "rollback-" + goal.ID, GoalID: goal.ID, Scope: "rollback",
			Granted: true, IssuedBy: "arc", IssuedAt: now.Add(-2 * time.Hour),
			ExpiresAt: now.Add(-time.Hour),
		},
	}
	if _, _, compensating := CompensatedGoal(goal, world); compensating {
		t.Fatal("an expired approval authorized a rollback")
	}
}

// The end-to-end property: with an approval the engine actually executes the
// rollback instead of blocking, retiring the failed version and rebuilding on
// the known-good one.
func TestApprovedRollbackExecutesAndConverges(t *testing.T) {
	goal, world := failedRolloutWorld(t)
	approveRollback(&world, goal.ID, nextImage, testImage)

	engine := NewEngine(NewMemoryExecutor(world), RolloutAgent{}, PlacementAgent{})
	err := engine.Run(goal, 12)

	var rollback *RollbackRequired
	if errors.As(err, &rollback) {
		t.Fatalf("an approved rollback still blocked the goal: %v", err)
	}
	if err != nil {
		t.Fatalf("approved rollback did not converge: %v", err)
	}

	// History must show the compensation, so an operator reading the log can
	// see the workload is deliberately not running what its goal names.
	compensated := false
	for _, event := range engine.Events {
		if event.Type == EventGoalCompensating {
			compensated = true
			if !strings.Contains(event.Message, testImage) {
				t.Fatalf("compensation event does not name the known-good image: %q", event.Message)
			}
		}
	}
	if !compensated {
		t.Fatal("no compensation event was recorded")
	}

	// Every surviving allocation must run the known-good image.
	final := engine.World.World()
	for _, allocation := range final.Allocations {
		if allocation.Workload != "app" || allocation.Phase == AllocationStopped {
			continue
		}
		if allocation.Image != testImage {
			t.Fatalf("allocation %q runs %q, want the known-good %q",
				allocation.ID, allocation.Image, testImage)
		}
	}
}

// A rollback must respect the availability floor like any other rollout, so
// compensating cannot take down more replicas than a normal replacement would.
func TestRollbackRespectsAvailabilityFloor(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	goal := scenario.Goal
	goal.Route = nil
	goal.Workload.Image = nextImage
	goal.Workload.Replicas = 2

	world := rolloutWorld(goal, 2, nextImage)
	world.KnownGood = map[string]string{"app": testImage}
	// One replica crashed; the other is still serving the failed version.
	world.Allocations["app-0"].Phase = AllocationStopped
	world.Allocations["app-0"].Ready = false
	world.Allocations["app-0"].ExitCode = 1
	approveRollback(&world, goal.ID, nextImage, testImage)

	effective, _, compensating := CompensatedGoal(goal, world)
	if !compensating {
		t.Fatal("expected compensation")
	}
	// The rollout agent retires the stopped replica first, which costs no
	// availability at all.
	proposal, err := (RolloutAgent{MinAvailable: 1}).Propose(effective, world)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if len(proposal.Actions) == 0 {
		t.Fatal("compensation proposed nothing")
	}
	if proposal.Actions[0].Target != "app-0" {
		t.Fatalf("expected the stopped replica to be retired first, got %q",
			proposal.Actions[0].Target)
	}
}

// Once the goal itself names the known-good image there is nothing to
// compensate, so an operator who fixes the goal is not left in rollback mode.
func TestNoCompensationWhenGoalAlreadyNamesKnownGood(t *testing.T) {
	goal, world := failedRolloutWorld(t)
	approveRollback(&world, goal.ID, nextImage, testImage)
	goal.Workload.Image = testImage

	if _, _, compensating := CompensatedGoal(goal, world); compensating {
		t.Fatal("compensated a goal that already names the known-good image")
	}
}

// The scope must be one the approval vocabulary actually defines, or an
// operator could never issue it.
func TestRollbackScopeIsApprovable(t *testing.T) {
	if _, ok := ApprovalScopes["rollback"]; !ok {
		t.Fatal("rollback is not an approvable scope")
	}
}

// A rollback must not switch itself off partway through.
//
// KnownGood moves as the restored version is observed serving, so a
// compensation that recomputed its target each round would stop compensating
// and rebuild the version that failed. The approval records both versions for
// exactly this reason.
func TestCompensationSurvivesKnownGoodMoving(t *testing.T) {
	goal, world := failedRolloutWorld(t)
	approveRollback(&world, goal.ID, nextImage, testImage)

	// Simulate the restored version having been observed serving, which is what
	// updates KnownGood to the image the rollback returned to.
	world.KnownGood = map[string]string{"app": nextImage}

	effective, _, compensating := CompensatedGoal(goal, world)
	if !compensating {
		t.Fatal("compensation stopped once KnownGood moved")
	}
	if effective.Workload.Image != testImage {
		t.Fatalf("effective image = %q, want the approved rollback target %q",
			effective.Workload.Image, testImage)
	}
}

// The approved rollback must hold across a full convergence, leaving the
// workload on the known-good image rather than oscillating back.
func TestRollbackDoesNotOscillateBackToFailedImage(t *testing.T) {
	goal, world := failedRolloutWorld(t)
	approveRollback(&world, goal.ID, nextImage, testImage)

	engine := NewEngine(NewMemoryExecutor(world), RolloutAgent{}, PlacementAgent{})
	if err := engine.Run(goal, 20); err != nil {
		t.Fatalf("run: %v", err)
	}
	final := engine.World.World()
	for _, allocation := range final.Allocations {
		if allocation.Workload != "app" || allocation.Phase == AllocationStopped {
			continue
		}
		if allocation.Image == nextImage {
			t.Fatalf("allocation %q oscillated back to the failed image", allocation.ID)
		}
	}
}
