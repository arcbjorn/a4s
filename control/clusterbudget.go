package control

import "fmt"

// Cluster ceilings: the orchestrator that cannot exceed its budget.
//
// Node capacity already stops one machine being oversubscribed, and per-node
// agent budget capacity already stops one node's agents consuming a whole
// cluster's spend. Neither bounds the total. A control plane placing work
// without a human in the loop will happily fill every node it can see and, for
// agent workloads, commit every token those nodes will underwrite, because
// nothing in the system was ever told how large this cluster is allowed to get.
//
// This is the same shape as the agent budget one level up: the kernel authorizes
// against a ceiling, and refusing is the enforcement. It is what lets a4s say
// something an orchestrator normally cannot, which is that a runaway control
// loop has a maximum cost.

// ClusterCommitment is what the world currently holds against its ceilings.
type ClusterCommitment struct {
	// Allocations counts live allocations.
	Allocations int `json:"allocations"`
	// Resources is the compute committed across every live allocation.
	Resources Resources `json:"resources"`
	// Budget is the agent spend ceiling committed across every live agent
	// allocation. It is the sum of what was authorized, not of what was spent:
	// the commitment is what the cluster is on the hook for.
	Budget Budget `json:"budget,omitzero"`
}

// Commitment totals what the world currently holds.
//
// Stopped allocations are excluded, matching how node capacity is released when
// an allocation stops. A ceiling that counted them would shrink permanently as a
// cluster churned.
func Commitment(world World) ClusterCommitment {
	var total ClusterCommitment
	for _, allocation := range world.Allocations {
		if allocation.Phase == AllocationStopped {
			continue
		}
		total.Allocations++
		total.Resources = total.Resources.Add(allocation.Resources)
		total.Budget = total.Budget.Add(allocation.Budget)
	}
	return total
}

// exceeds reports whether a total has passed a ceiling, treating a zero ceiling
// as no ceiling.
//
// Zero means unlimited rather than zero-allowed, because a Policy written before
// these fields existed leaves them zero and must keep working. Resources.Fits
// cannot express that on its own: an unset ceiling would refuse everything.
func exceeds(total, ceiling int) bool { return ceiling > 0 && total > ceiling }

// checkClusterBudget refuses a proposal that would commit the cluster beyond its
// declared ceilings.
//
// It is checked against the observed world plus what the proposal would add,
// rather than inside per-action simulation, because a ceiling is a property of
// the whole cluster and the question "would this take us over" only has an
// answer for a complete proposal.
func (k Kernel) checkClusterBudget(world World, proposal Proposal) error {
	var adding ClusterCommitment
	for _, action := range proposal.Actions {
		if action.Kind != ActionCreateAllocation {
			continue
		}
		adding.Allocations++
		adding.Resources = adding.Resources.Add(action.Resources)
		adding.Budget = adding.Budget.Add(action.Budget)
	}
	if adding.Allocations == 0 {
		return nil
	}

	held := Commitment(world)
	if exceeds(held.Allocations+adding.Allocations, k.Policy.MaxAllocations) {
		return fmt.Errorf(
			"cluster allocation ceiling reached: %d live plus %d proposed exceeds %d",
			held.Allocations, adding.Allocations, k.Policy.MaxAllocations)
	}

	ceiling := k.Policy.ClusterCeiling
	total := held.Resources.Add(adding.Resources)
	if exceeds(total.CPUMillis, ceiling.CPUMillis) {
		return fmt.Errorf(
			"cluster cpu ceiling reached: %d millis committed plus %d proposed exceeds %d",
			held.Resources.CPUMillis, adding.Resources.CPUMillis, ceiling.CPUMillis)
	}
	if exceeds(total.MemoryMB, ceiling.MemoryMB) {
		return fmt.Errorf(
			"cluster memory ceiling reached: %d MB committed plus %d proposed exceeds %d",
			held.Resources.MemoryMB, adding.Resources.MemoryMB, ceiling.MemoryMB)
	}

	spend := k.Policy.ClusterBudget
	committed := held.Budget.Add(adding.Budget)
	for _, dimension := range []struct {
		name          string
		total, ceilng int
	}{
		{"tokens", committed.Tokens, spend.Tokens},
		{"cost", committed.CostMillis, spend.CostMillis},
		{"wall seconds", committed.WallSeconds, spend.WallSeconds},
		{"tool calls", committed.ToolCalls, spend.ToolCalls},
	} {
		if exceeds(dimension.total, dimension.ceilng) {
			return fmt.Errorf(
				"cluster agent %s ceiling reached: %d committed exceeds %d",
				dimension.name, dimension.total, dimension.ceilng)
		}
	}
	return nil
}
