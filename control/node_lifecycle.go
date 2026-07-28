package control

import (
	"fmt"
	"sort"
)

// Node lifecycle: taking a machine out of service without a human.
//
// A node that has gone bad has to stop attracting work and then give up the work
// it holds. In a cluster with an operator watching, that is a person running two
// commands. Here it has to be a decision an agent can propose and the kernel can
// authorize, which means it has to be expressible as typed actions with
// deterministic policy behind them.
//
// The split matters: cordoning is reversible and cheap, so an agent may do it on
// measured evidence alone. Evacuation is neither, so it reuses the existing stop
// and delete actions and inherits every gate already on them, including the
// approval a stateful workload requires before its data moves.

// ControlPlaneLocal reports whether an action is settled by the control plane
// itself rather than by a node.
//
// Cordoning changes scheduling intent, which exists only in the control plane's
// world. Dispatching it would ask a node to attest to a fact it cannot observe,
// and worse, the usual reason to cordon a node is that it has stopped
// answering: an evacuation that required the failing node's cooperation would
// be unavailable exactly when it is needed.
func (k ActionKind) ControlPlaneLocal() bool {
	switch k {
	case ActionCordonNode, ActionUncordonNode:
		return true
	}
	return false
}

// CordonEvidence renders the observation a cordon or uncordon action produces.
//
// Both the in-memory and remote executors call this rather than each building
// the evidence themselves, so the simulated and real control planes cannot
// record a node leaving service in two different shapes.
func CordonEvidence(action Action) (Evidence, error) {
	if action.Target == "" {
		return Evidence{}, fmt.Errorf("%s requires a node", action.Kind)
	}
	kind := EvidenceNodeCordoned
	if action.Kind == ActionUncordonNode {
		kind = EvidenceNodeUncordoned
	}
	return Evidence{
		Kind: kind, Target: action.Target,
		Observed: map[string]string{"node": action.Target, "reason": action.Reason},
	}, nil
}

// Schedulable reports whether a node may receive new allocations.
//
// Health and cordon are both required, and they are checked together everywhere
// placement happens so the two cannot drift into meaning different things in
// different callers.
func (n *Node) Schedulable() bool {
	return n != nil && n.Healthy && !n.Cordoned
}

// validateCordonNode authorizes taking a node out of service.
//
// Cordoning is permitted even on a node that is already cordoned, because the
// action is a statement of intent rather than a transition: a retry after an
// ambiguous failure must not be refused, which is the same idempotency every
// other action here has.
func validateCordonNode(_ Goal, world World, action Action) error {
	if action.Target == "" {
		return fmt.Errorf("cordon requires a node")
	}
	if _, ok := world.Nodes[action.Target]; !ok {
		return fmt.Errorf("node %q does not exist", action.Target)
	}
	return nil
}

// validateUncordonNode authorizes returning a node to service.
//
// An unhealthy node may be uncordoned. Health is measured continuously and will
// keep it out of placement on its own, and refusing here would mean a node that
// was cordoned while down could never be released without first coming back,
// which inverts the order an operator actually recovers a machine in.
func validateUncordonNode(_ Goal, world World, action Action) error {
	if action.Target == "" {
		return fmt.Errorf("uncordon requires a node")
	}
	if _, ok := world.Nodes[action.Target]; !ok {
		return fmt.Errorf("node %q does not exist", action.Target)
	}
	return nil
}

// NodeEvacuation describes what draining one node would cost.
//
// It is computed rather than proposed so an operator, a planner, and an agent
// all see the same answer. Stateful allocations are listed separately because
// they are the ones that cannot simply be recreated elsewhere: their data lives
// on the node being emptied.
type NodeEvacuation struct {
	Node string `json:"node"`
	// Allocations are the live allocations that would have to be removed.
	Allocations []string `json:"allocations,omitempty"`
	// Stateful lists the subset holding durable data. Each needs an operator
	// decision before its allocation is destroyed, because recreating it
	// elsewhere is a data move rather than a reschedule.
	Stateful []string `json:"stateful,omitempty"`
}

// Empty reports whether the node holds nothing that needs moving.
func (e NodeEvacuation) Empty() bool { return len(e.Allocations) == 0 }

// PlanEvacuation reports what still runs on a node.
func PlanEvacuation(world World, nodeID string) NodeEvacuation {
	evacuation := NodeEvacuation{Node: nodeID}
	for _, allocation := range world.Allocations {
		if allocation.Node != nodeID || allocation.Phase == AllocationStopped {
			continue
		}
		evacuation.Allocations = append(evacuation.Allocations, allocation.ID)
		if allocation.Stateful {
			evacuation.Stateful = append(evacuation.Stateful, allocation.ID)
		}
	}
	sort.Strings(evacuation.Allocations)
	sort.Strings(evacuation.Stateful)
	return evacuation
}

// UnhealthyNodes lists nodes that are not healthy and not yet cordoned, in a
// stable order.
//
// This is the input to automatic cordoning. A node already cordoned is excluded
// so the decision is made once rather than re-proposed every round.
func UnhealthyNodes(world World) []string {
	var unhealthy []string
	for id, node := range world.Nodes {
		if !node.Healthy && !node.Cordoned {
			unhealthy = append(unhealthy, id)
		}
	}
	sort.Strings(unhealthy)
	return unhealthy
}
