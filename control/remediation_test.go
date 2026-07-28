package control

import (
	"strings"
	"testing"
	"time"
)

func remediationWorld() World {
	world := spreadWorld(map[string]string{"node-a": "rack-1", "node-b": "rack-2"})
	world.ObservedAt = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	world.Allocations["web-0"] = &Allocation{
		ID: "web-0", Workload: "web", Replica: 0, Node: "node-a", Image: spreadImage,
		Phase: AllocationRunning, Ready: true,
		Resources: Resources{CPUMillis: 100, MemoryMB: 128},
	}
	return world
}

func remediate(t *testing.T, world World) Proposal {
	t.Helper()
	proposal, err := (RemediationAgent{}).Propose(spreadGoal(2, 0), world)
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}

// A healthy cluster needs no repair, and an empty proposal is how an agent says
// it has no opinion.
func TestRemediationProposesNothingWhenHealthy(t *testing.T) {
	if actions := remediate(t, remediationWorld()).Actions; len(actions) != 0 {
		t.Fatalf("a healthy cluster was remediated: %v", actions)
	}
}

// Rung one is the cheapest and fully reversible step.
func TestRemediationCordonsAnUnhealthyNodeFirst(t *testing.T) {
	world := remediationWorld()
	world.Nodes["node-a"].Healthy = false
	// A stopped allocation is also present, so the ladder's ordering is what
	// decides which rung runs rather than which condition happens to be found.
	world.Allocations["web-1"] = &Allocation{
		ID: "web-1", Workload: "web", Replica: 1, Node: "node-b",
		Image: spreadImage, Phase: AllocationStopped,
	}

	proposal := remediate(t, world)
	if len(proposal.Actions) != 1 || proposal.Actions[0].Kind != ActionCordonNode {
		t.Fatalf("expected a single cordon, got %v", proposal.Actions)
	}
	if proposal.Actions[0].Target != "node-a" {
		t.Fatalf("cordoned %q, want node-a", proposal.Actions[0].Target)
	}
	if proposal.Actions[0].Reason == "" {
		t.Fatal("the cordon carried no reason into history")
	}
}

// A stopped allocation holds its replica slot: placement is refused because the
// id is taken, so the goal stays stuck until something clears it.
func TestRemediationRetiresAStoppedAllocation(t *testing.T) {
	world := remediationWorld()
	world.Allocations["web-0"].Phase = AllocationStopped
	world.Allocations["web-0"].ExitCode = 1

	proposal := remediate(t, world)
	if len(proposal.Actions) != 1 || proposal.Actions[0].Kind != ActionDeleteAllocation {
		t.Fatalf("expected a single delete, got %v", proposal.Actions)
	}
	if proposal.Actions[0].Target != "web-0" {
		t.Fatalf("retired %q, want web-0", proposal.Actions[0].Target)
	}
}

// The escalation. Repairing forever looks like activity and is indistinguishable
// from progress; stopping is what gets a human involved.
func TestRemediationGivesUpAfterRepeatedFailures(t *testing.T) {
	world := remediationWorld()
	world.Allocations["web-0"].Phase = AllocationStopped
	world.Backoff = map[string]*Backoff{
		"web-0": {Failures: MaxRemediationAttempts, Until: world.ObservedAt},
	}

	if actions := remediate(t, world).Actions; len(actions) != 0 {
		t.Fatalf("expected the agent to stop repairing, got %v", actions)
	}

	// One failure below the ceiling it still tries.
	world.Backoff["web-0"].Failures = MaxRemediationAttempts - 1
	if actions := remediate(t, world).Actions; len(actions) != 1 {
		t.Fatalf("expected a repair below the ceiling, got %v", actions)
	}
}

// Evacuation is what turns a cordon into a drain, and it is last because it is
// the only rung that removes something that is working.
func TestRemediationEvacuatesACordonedNode(t *testing.T) {
	world := remediationWorld()
	world.Nodes["node-a"].Cordoned = true

	proposal := remediate(t, world)
	if len(proposal.Actions) != 2 {
		t.Fatalf("expected stop then delete, got %v", proposal.Actions)
	}
	if proposal.Actions[0].Kind != ActionStopAllocation ||
		proposal.Actions[1].Kind != ActionDeleteAllocation {
		t.Fatalf("evacuation was not stop-then-delete: %v", proposal.Actions)
	}
	// Deletion is refused while an allocation runs, so the order has to be
	// declared rather than assumed.
	if len(proposal.Actions[1].DependsOn) != 1 ||
		proposal.Actions[1].DependsOn[0] != proposal.Actions[0].ID {
		t.Fatal("delete does not depend on the stop")
	}
}

// The agent may subtract but never add. This is the argument for running it
// unattended: a remediation loop that went wrong cannot conjure capacity.
func TestRemediationCannotCreateOrStart(t *testing.T) {
	grants := DefaultPolicy().Grants["remediation-agent"]
	if len(grants) == 0 {
		t.Fatal("the remediation agent holds no grants")
	}
	for _, forbidden := range []ActionKind{
		ActionCreateAllocation, ActionStartAllocation, ActionPullImage,
		ActionPublishRoute, ActionGrantTools, ActionRestoreSnapshot,
	} {
		if grants[forbidden] {
			t.Fatalf("the remediation agent is granted %s", forbidden)
		}
	}
	for _, expected := range []ActionKind{
		ActionCordonNode, ActionStopAllocation, ActionDeleteAllocation,
	} {
		if !grants[expected] {
			t.Fatalf("the remediation agent is missing %s", expected)
		}
	}
}

// Every action the agent proposes must survive the kernel it will be judged by.
func TestRemediationProposalsAreAuthorizable(t *testing.T) {
	world := remediationWorld()
	world.Nodes["node-a"].Healthy = false
	goal := spreadGoal(2, 0)
	kernel := Kernel{Policy: DefaultPolicy()}

	proposal := remediate(t, world)
	proposal.BasedOnRevision = world.Revision
	if err := kernel.Authorize((RemediationAgent{}).Descriptor(),
		goal, world, proposal); err != nil {
		t.Fatalf("the kernel refused the agent's own cordon: %v", err)
	}
}

// Destroying durable data stays an operator decision. The agent proposes it and
// is refused, which puts the reason in the event log rather than silently
// skipping the workload that most needs attention.
func TestRemediationCannotDestroyStatefulDataUnapproved(t *testing.T) {
	world := remediationWorld()
	world.Nodes["node-a"].Cordoned = true
	world.Allocations["web-0"].Stateful = true
	// A second ready replica, so the availability floor permits moving the
	// first and the stateful gate is what the proposal actually meets.
	world.Allocations["web-1"] = &Allocation{
		ID: "web-1", Workload: "web", Replica: 1, Node: "node-b", Image: spreadImage,
		Phase: AllocationRunning, Ready: true,
		Resources: Resources{CPUMillis: 100, MemoryMB: 128},
	}

	proposal := remediate(t, world)
	if len(proposal.Actions) == 0 {
		t.Fatal("the agent skipped a stateful workload instead of proposing")
	}
	proposal.BasedOnRevision = world.Revision
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(RemediationAgent{}).Descriptor(), spreadGoal(2, 0), world, proposal)
	if err == nil || !strings.Contains(err.Error(), "destroy-stateful") {
		t.Fatalf("expected a destroy-stateful denial, got %v", err)
	}
}

// Evacuation must not take the service down to empty a node. The availability
// floor is enforced by the kernel independently, so an agent that proposed it
// anyway is refused rather than obeyed.
func TestRemediationCannotEvacuateTheLastReadyReplica(t *testing.T) {
	world := remediationWorld()
	world.Nodes["node-a"].Cordoned = true

	proposal := remediate(t, world)
	proposal.BasedOnRevision = world.Revision
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(RemediationAgent{}).Descriptor(), spreadGoal(2, 0), world, proposal)
	if err == nil || !strings.Contains(err.Error(), "availability floor") {
		t.Fatalf("expected an availability-floor denial, got %v", err)
	}
}

// Clearing an allocation that already stopped takes no capacity away, so it must
// not spend the budget that paces disruption of live work.
func TestRetiringADeadAllocationCostsNoBudget(t *testing.T) {
	world := remediationWorld()
	world.Allocations["web-0"].Phase = AllocationStopped

	next, err := Project(world, Evidence{
		Kind: EvidenceAllocationDeleted, Target: "web-0", ObservedAt: world.ObservedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Disruptions) != 0 {
		t.Fatalf("retiring a dead allocation was charged: %v", next.Disruptions)
	}

	// Stopping a live one still is.
	live := remediationWorld()
	stopped, err := Project(live, Evidence{
		Kind: EvidenceAllocationStopped, Target: "web-0", ObservedAt: live.ObservedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped.Disruptions) != 1 {
		t.Fatalf("stopping live work was not charged: %v", stopped.Disruptions)
	}
}
