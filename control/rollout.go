package control

import (
	"fmt"
	"sort"
)

// RolloutAgent replaces allocations whose image no longer matches the goal.
//
// It replaces one allocation at a time and refuses to disrupt an allocation
// while doing so would drop available replicas below the goal's availability
// floor. That budget is what separates a rollout from an outage: without it, a
// bad image would be applied to every replica before anyone observed the first
// one failing.
//
// The agent proposes stop and delete for the drifted allocation only. Placement
// then creates the replacement against fresh observations, so a rollout is a
// sequence of small authorized steps rather than one large plan built on stale
// state.
type RolloutAgent struct {
	// MinAvailable is how many matching ready replicas must remain during a
	// rollout. Zero means the goal's replica count minus one, which keeps at
	// least one replica serving for a multi-replica workload.
	MinAvailable int
}

func (RolloutAgent) Descriptor() AgentDescriptor {
	return AgentDescriptor{
		ID: "rollout-agent", Role: "replace drifted allocations within an availability budget",
		Capabilities: []ActionKind{ActionStopAllocation, ActionDeleteAllocation},
	}
}

// RollbackRequired reports that a deployed version failed and names the version
// last observed serving.
//
// It is an error rather than a proposal because reverting changes what the
// operator asked for. The kernel refuses to let an agent rewrite a goal, so the
// system surfaces the decision with the evidence needed to make it instead of
// quietly running something other than what was requested.
type RollbackRequired struct {
	Workload  string
	Failed    string
	KnownGood string
}

func (e *RollbackRequired) Error() string {
	return fmt.Sprintf("workload %q failed on %s; last known-good image is %s",
		e.Workload, e.Failed, e.KnownGood)
}

// RollbackTarget reports the image a failed rollout should return to, and
// whether a rollback is warranted at all.
//
// A rollout is judged failed only when a replacement on the goal's image has
// actually been observed failing, not merely when it is not yet ready. The
// difference matters: treating "still starting" as failure would roll back
// every deployment before it had a chance to come up.
func RollbackTarget(goal Goal, world World) (string, bool) {
	knownGood, recorded := world.KnownGood[goal.Workload.Name]
	if !recorded || knownGood == goal.Workload.Image {
		// Nothing better to return to, or the goal already names the version
		// that was last observed serving.
		return "", false
	}
	for _, allocation := range world.Allocations {
		if allocation.Workload != goal.Workload.Name || allocation.Image != goal.Workload.Image {
			continue
		}
		// A replacement that crashed or exhausted its restart budget is
		// evidence the new version does not work here.
		if allocation.Phase == AllocationStopped && (allocation.ExitCode != 0 || allocation.Restarts > 0) {
			return knownGood, true
		}
	}
	return "", false
}

func (r RolloutAgent) Propose(goal Goal, world World) (Proposal, error) {
	descriptor := r.Descriptor()
	proposal := Proposal{
		ID: fmt.Sprintf("%s-r%d", descriptor.ID, world.Revision), AgentID: descriptor.ID,
		GoalID: goal.ID, BasedOnRevision: world.Revision,
		Reasoning: "retire one allocation that no longer matches the goal, within the availability budget",
	}

	// A failed rollout is reported rather than silently reversed. Rolling back
	// means running a different version than the operator asked for, and an
	// agent may not redefine the goal. The engine raises this as a blocked goal
	// naming the known-good digest so a human or a Git source can decide.
	if knownGood, failed := RollbackTarget(goal, world); failed {
		return proposal, &RollbackRequired{
			Workload:  goal.Workload.Name,
			Failed:    goal.Workload.Image,
			KnownGood: knownGood,
		}
	}

	drifted := driftedAllocations(goal, world)
	if len(drifted) == 0 {
		return proposal, nil
	}

	// Retiring a stopped allocation costs no availability, so clear those first
	// and let placement rebuild before disrupting anything still serving.
	for _, allocation := range drifted {
		if allocation.Phase == AllocationStopped {
			proposal.Actions = []Action{{
				ID: "delete-" + allocation.ID, Kind: ActionDeleteAllocation,
				Target: allocation.ID, Workload: allocation.Workload,
				Node: allocation.Node,
			}}
			return proposal, nil
		}
	}

	floor := r.floor(goal)
	available := servingAllocations(goal, world)
	// Only a ready allocation's removal actually costs availability.
	candidate := drifted[0]
	cost := 0
	if candidate.ReadyAt(world.Now()) {
		cost = 1
	}
	if available-cost < floor {
		return proposal, fmt.Errorf(
			"stopping %q would leave %d ready replicas, below the availability floor of %d",
			candidate.ID, available-cost, floor)
	}

	stopID := "stop-" + candidate.ID
	proposal.Actions = []Action{
		{
			ID: stopID, Kind: ActionStopAllocation,
			Target: candidate.ID, Workload: candidate.Workload,
			Node: candidate.Node,
		},
		{
			ID: "delete-" + candidate.ID, Kind: ActionDeleteAllocation,
			Target: candidate.ID, Workload: candidate.Workload,
			Node: candidate.Node, DependsOn: []string{stopID},
		},
	}
	return proposal, nil
}

// floor is the number of ready replicas a rollout must preserve.
func (r RolloutAgent) floor(goal Goal) int {
	if r.MinAvailable > 0 {
		return r.MinAvailable
	}
	// A single-replica workload cannot be updated without a gap, so its floor
	// is zero. Anything larger keeps at least one replica serving.
	if goal.Workload.Replicas <= 1 {
		return 0
	}
	return goal.Workload.Replicas - 1
}

// driftedAllocations returns allocations that no longer match the goal, in
// stable order so proposals are deterministic.
func driftedAllocations(goal Goal, world World) []*Allocation {
	var drifted []*Allocation
	for _, allocation := range world.Allocations {
		if allocation.Workload != goal.Workload.Name {
			continue
		}
		node := world.Nodes[allocation.Node]
		matches := node != nil &&
			allocation.Image == goal.Workload.Image &&
			allocation.Resources == goal.Workload.Resources &&
			nodeAllowed(goal.Constraints, *node)
		if !matches {
			drifted = append(drifted, allocation)
		}
	}
	sort.Slice(drifted, func(i, j int) bool { return drifted[i].ID < drifted[j].ID })
	return drifted
}
