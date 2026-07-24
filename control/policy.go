package control

import (
	"fmt"
	"strings"
)

type Policy struct {
	MaxActionsPerProposal int
	Grants                map[string]map[ActionKind]bool
}

func DefaultPolicy() Policy {
	return Policy{
		MaxActionsPerProposal: 8,
		Grants: map[string]map[ActionKind]bool{
			"placement-agent": {
				ActionPullImage:        true,
				ActionCreateAllocation: true,
				ActionStartAllocation:  true,
			},
			"network-agent": {
				ActionPublishRoute: true,
			},
		},
	}
}

type Kernel struct {
	Policy Policy
}

// Authorize validates the entire proposal against a cloned world. Agents never
// execute a partial plan that the kernel has not proved valid end to end.
func (k Kernel) Authorize(actor AgentDescriptor, goal Goal, world World, proposal Proposal) error {
	if proposal.AgentID != actor.ID {
		return fmt.Errorf("proposal agent %q does not match authenticated actor %q", proposal.AgentID, actor.ID)
	}
	if proposal.AgentID == "" || proposal.GoalID != goal.ID {
		return fmt.Errorf("proposal identity does not match goal")
	}
	if proposal.BasedOnRevision != world.Revision {
		return fmt.Errorf("stale proposal: based on revision %d, current revision is %d", proposal.BasedOnRevision, world.Revision)
	}
	if len(proposal.Actions) == 0 {
		return fmt.Errorf("proposal contains no actions")
	}
	if len(proposal.Actions) > k.Policy.MaxActionsPerProposal {
		return fmt.Errorf("proposal exceeds %d action limit", k.Policy.MaxActionsPerProposal)
	}
	grants := k.Policy.Grants[proposal.AgentID]
	if len(grants) == 0 {
		return fmt.Errorf("agent %q has no capability grants", proposal.AgentID)
	}

	sim := cloneWorld(world)
	completed := make(map[string]bool, len(proposal.Actions))
	for _, action := range proposal.Actions {
		if action.ID == "" || completed[action.ID] {
			return fmt.Errorf("action ids must be non-empty and unique")
		}
		if !grants[action.Kind] {
			return fmt.Errorf("agent %q is not granted %s", proposal.AgentID, action.Kind)
		}
		for _, dependency := range action.DependsOn {
			if !completed[dependency] {
				return fmt.Errorf("action %q has unsatisfied dependency %q", action.ID, dependency)
			}
		}
		if err := validateAction(goal, sim, action); err != nil {
			return fmt.Errorf("action %q: %w", action.ID, err)
		}
		if err := simulateAction(&sim, action); err != nil {
			return fmt.Errorf("simulate action %q: %w", action.ID, err)
		}
		completed[action.ID] = true
	}
	if err := requireEvidenceChecks(proposal); err != nil {
		return err
	}
	return nil
}

func requireEvidenceChecks(proposal Proposal) error {
	for _, action := range proposal.Actions {
		var requiredKind string
		switch action.Kind {
		case ActionStartAllocation:
			requiredKind = CheckAllocationReady
		case ActionPublishRoute:
			requiredKind = CheckRouteReachable
		default:
			continue
		}
		found := false
		for _, check := range proposal.ExpectedEvidence {
			if check.Kind == requiredKind && check.Target == action.Target && check.Want == "true" {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("action %q requires %s evidence", action.ID, requiredKind)
		}
	}
	return nil
}

func validateAction(goal Goal, world World, action Action) error {
	switch action.Kind {
	case ActionPullImage:
		node, ok := world.Nodes[action.Node]
		if !ok || !node.Healthy {
			return fmt.Errorf("node %q is missing or unhealthy", action.Node)
		}
		if action.Image != goal.Workload.Image || !strings.Contains(action.Image, "@sha256:") {
			return fmt.Errorf("image must match the goal's pinned digest")
		}

	case ActionCreateAllocation:
		if _, exists := world.Allocations[action.Target]; exists {
			return fmt.Errorf("allocation %q already exists", action.Target)
		}
		node, ok := world.Nodes[action.Node]
		if !ok || !node.Healthy {
			return fmt.Errorf("node %q is missing or unhealthy", action.Node)
		}
		if !nodeAllowed(goal.Constraints, *node) {
			return fmt.Errorf("node %q violates placement constraints", action.Node)
		}
		if action.Workload != goal.Workload.Name {
			return fmt.Errorf("workload differs from goal")
		}
		if action.Image != goal.Workload.Image || !node.Images[action.Image] {
			return fmt.Errorf("pinned image is not present on node %q", action.Node)
		}
		if action.Resources != goal.Workload.Resources {
			return fmt.Errorf("resources differ from goal")
		}
		if !node.Used.Add(action.Resources).Fits(node.Capacity) {
			return fmt.Errorf("node %q lacks capacity", action.Node)
		}
		if action.Replica < 0 || action.Replica >= goal.Workload.Replicas {
			return fmt.Errorf("replica index is outside goal")
		}

	case ActionStartAllocation:
		allocation, ok := world.Allocations[action.Target]
		if !ok || allocation.Phase != AllocationCreated {
			return fmt.Errorf("allocation %q is not created", action.Target)
		}
		if action.Workload != goal.Workload.Name || allocation.Workload != action.Workload {
			return fmt.Errorf("workload differs from goal")
		}

	case ActionPublishRoute:
		if goal.Route == nil {
			return fmt.Errorf("goal does not request a route")
		}
		if action.Target != goal.Route.Host || action.Port != goal.Route.Port || action.Exposure != goal.Route.Exposure {
			return fmt.Errorf("route differs from goal")
		}
		if action.Workload != goal.Workload.Name {
			return fmt.Errorf("workload differs from goal")
		}
		if action.Exposure == "public" && !hasApproval(world, goal.ID, "public-route") {
			return fmt.Errorf("public route requires public-route approval")
		}
		if matchingReadyAllocations(goal, world) < goal.Workload.Replicas {
			return fmt.Errorf("workload is not ready")
		}

	default:
		return fmt.Errorf("unknown action kind %q", action.Kind)
	}
	return nil
}

func nodeAllowed(constraints Constraints, node Node) bool {
	if len(constraints.AllowedNodes) > 0 {
		allowed := false
		for _, id := range constraints.AllowedNodes {
			if node.ID == id {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	for key, value := range constraints.RequiredLabels {
		if node.Labels[key] != value {
			return false
		}
	}
	return true
}

func cloneWorld(world World) World {
	clone := World{
		Revision:    world.Revision,
		Nodes:       make(map[string]*Node, len(world.Nodes)),
		Allocations: make(map[string]*Allocation, len(world.Allocations)),
		Routes:      make(map[string]*Route, len(world.Routes)),
		Approvals:   make(map[string]*Approval, len(world.Approvals)),
	}
	for id, node := range world.Nodes {
		copyNode := *node
		copyNode.Labels = make(map[string]string, len(node.Labels))
		for key, value := range node.Labels {
			copyNode.Labels[key] = value
		}
		copyNode.Images = make(map[string]bool, len(node.Images))
		for image, present := range node.Images {
			copyNode.Images[image] = present
		}
		clone.Nodes[id] = &copyNode
	}
	for id, allocation := range world.Allocations {
		copyAllocation := *allocation
		clone.Allocations[id] = &copyAllocation
	}
	for host, route := range world.Routes {
		copyRoute := *route
		clone.Routes[host] = &copyRoute
	}
	for id, approval := range world.Approvals {
		copyApproval := *approval
		clone.Approvals[id] = &copyApproval
	}
	return clone
}

func matchingReadyAllocations(goal Goal, world World) int {
	ready := 0
	for _, allocation := range world.Allocations {
		node := world.Nodes[allocation.Node]
		if node != nil && allocation.Ready && allocation.Workload == goal.Workload.Name &&
			allocation.Image == goal.Workload.Image && allocation.Resources == goal.Workload.Resources &&
			nodeAllowed(goal.Constraints, *node) {
			ready++
		}
	}
	return ready
}
