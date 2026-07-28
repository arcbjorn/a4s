package control

import (
	"fmt"
	"sort"
)

// The loop that closes: acting on what diagnosis already saw.
//
// Until now a4s could observe that a goal was stuck and explain why, and then
// wait. LogDiagnoser writes prose; nothing acts on it. On a cluster with an
// operator that is the correct division of labour, because the operator is the
// thing that acts. Without one, an allocation that fails leaves its record in
// the world, placement refuses to recreate a replica whose id is still taken,
// and the goal stays stuck on a failure the cluster diagnosed correctly and had
// the authority to repair.
//
// The agent below is the smallest thing that closes that loop, and it is
// deliberately conservative about how. Remediation that guesses is worse than
// remediation that stops, because a wrong repair is applied at machine speed to
// every instance of the same symptom.

// MaxRemediationAttempts bounds how many times a target may be repaired before
// the agent stops trying.
//
// Past this, repeating the repair is not going to work: the same placement has
// failed this many times, and continuing would spend disruption budget and node
// capacity to reach the same state. Stopping is what escalates to a human, since
// a goal that stays unconverged is what an operator is alerted on.
const MaxRemediationAttempts = 3

// RemediationAgent repairs what the cluster has observed to be broken.
//
// Its capability set is the argument for why it is safe to run unattended. It
// may take things out of service and it may remove allocations, but it cannot
// create or start one: replacement is placement's job. Repair and replacement
// stay in separate capability sets, so a remediation loop that went wrong can
// subtract capacity but can never conjure it, and every replacement it causes
// still passes through placement's constraints, the spread ceiling, and the
// backoff that paces retries.
type RemediationAgent struct{}

func (RemediationAgent) Descriptor() AgentDescriptor {
	return AgentDescriptor{
		ID:   "remediation-agent",
		Role: "take failed nodes out of service and retire allocations that cannot recover",
		Capabilities: []ActionKind{
			ActionCordonNode, ActionStopAllocation, ActionDeleteAllocation,
		},
	}
}

// Propose walks a fixed ladder, cheapest and most reversible first, and stops at
// the first rung that has work.
//
// One rung per proposal is the point rather than a limitation. Each step is
// authorized against the world it was proposed for, executed, and observed
// before the next is considered, so a repair that made things worse is measured
// before the following one compounds it.
func (RemediationAgent) Propose(goal Goal, world World) (Proposal, error) {
	descriptor := (RemediationAgent{}).Descriptor()
	proposal := Proposal{
		ID: fmt.Sprintf("%s-r%d", descriptor.ID, world.Revision), AgentID: descriptor.ID,
		GoalID: goal.ID, BasedOnRevision: world.Revision,
	}

	// Rung one: take an unhealthy node out of service. It is fully reversible,
	// destroys nothing, and stops the cluster placing more work somewhere that
	// has already failed once.
	if node := failedNodeHolding(world, goal.Workload.Name); node != "" {
		proposal.Reasoning = fmt.Sprintf(
			"node %q is unhealthy and still holds %q; stop placing work on it", node, goal.Workload.Name)
		proposal.Actions = append(proposal.Actions, Action{
			ID: "cordon-" + node, Kind: ActionCordonNode, Target: node,
			Reason: "unhealthy node holding " + goal.Workload.Name,
		})
		return proposal, nil
	}

	// Rung two: clear an allocation that has stopped and will not come back.
	// Nothing is running, so this takes no capacity away; it releases the
	// replica slot that placement is otherwise refused because the id is taken.
	if target := retirableAllocation(world, goal.Workload.Name); target != "" {
		proposal.Reasoning = fmt.Sprintf(
			"allocation %q stopped and is holding its replica slot; retire it so it can be replaced", target)
		proposal.Actions = append(proposal.Actions, Action{
			ID: "delete-" + target, Kind: ActionDeleteAllocation,
			Target: target, Workload: goal.Workload.Name,
		})
		return proposal, nil
	}

	// Rung three: evacuate a live allocation from a cordoned node. This is the
	// only rung that removes something that is working, which is why it is last
	// and why it moves one allocation at a time.
	if target := evacuableAllocation(world, goal.Workload.Name); target != "" {
		stopID := "stop-" + target
		proposal.Reasoning = fmt.Sprintf(
			"allocation %q runs on a cordoned node; move it off", target)
		proposal.Actions = append(proposal.Actions,
			Action{
				ID: stopID, Kind: ActionStopAllocation,
				Target: target, Workload: goal.Workload.Name,
			},
			Action{
				ID: "delete-" + target, Kind: ActionDeleteAllocation,
				Target: target, Workload: goal.Workload.Name,
				DependsOn: []string{stopID},
			})
		return proposal, nil
	}

	// Nothing to repair. An empty proposal is how an agent says it has no
	// opinion this round; the kernel refuses an empty one, so it is never
	// submitted.
	return proposal, nil
}

// failedNodeHolding returns an unhealthy, uncordoned node holding this workload.
func failedNodeHolding(world World, workload string) string {
	var candidates []string
	for _, id := range UnhealthyNodes(world) {
		for _, allocation := range world.Allocations {
			if allocation.Workload == workload && allocation.Node == id {
				candidates = append(candidates, id)
				break
			}
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

// retirableAllocation returns a stopped allocation of this workload that should
// be cleared, or empty when none should be.
//
// Giving up past MaxRemediationAttempts is the escalation. The alternative is a
// loop that deletes and recreates the same failing allocation forever, which
// looks like activity and is indistinguishable from progress in a dashboard.
func retirableAllocation(world World, workload string) string {
	var candidates []string
	for id, allocation := range world.Allocations {
		if allocation.Workload != workload || allocation.Phase != AllocationStopped {
			continue
		}
		if state := world.Backoff[id]; state != nil && state.Failures >= MaxRemediationAttempts {
			// Repeatedly repaired and repeatedly failed. Leave it in place so
			// the goal stays visibly unconverged and someone looks at it.
			continue
		}
		candidates = append(candidates, id)
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

// evacuableAllocation returns a live allocation of this workload sitting on a
// cordoned node.
//
// This is what turns a cordon into a drain. A stateful allocation is included:
// it is not skipped here, because the decision of whether its data may be
// destroyed belongs to the operator approval the kernel already requires on
// deletion, not to this agent's judgement. Proposing it and being refused is the
// correct outcome, and it puts the reason in the event log.
func evacuableAllocation(world World, workload string) string {
	var candidates []string
	for id, allocation := range world.Allocations {
		if allocation.Workload != workload || allocation.Phase == AllocationStopped {
			continue
		}
		if node := world.Nodes[allocation.Node]; node != nil && node.Cordoned {
			candidates = append(candidates, id)
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}
