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

func (r RolloutAgent) Propose(goal Goal, world World) (Proposal, error) {
	descriptor := r.Descriptor()
	proposal := Proposal{
		ID: fmt.Sprintf("%s-r%d", descriptor.ID, world.Revision), AgentID: descriptor.ID,
		GoalID: goal.ID, BasedOnRevision: world.Revision,
		Reasoning: "retire one allocation that no longer matches the goal, within the availability budget",
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
		},
		{
			ID: "delete-" + candidate.ID, Kind: ActionDeleteAllocation,
			Target: candidate.ID, Workload: candidate.Workload,
			DependsOn: []string{stopID},
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
