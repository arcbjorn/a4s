package control

import (
	"fmt"
	"sort"
)

// PlacementAgent is a deterministic reference agent. A future model-backed
// agent implements the same interface and receives no additional authority.
type PlacementAgent struct{}

func (PlacementAgent) Descriptor() AgentDescriptor {
	return AgentDescriptor{
		ID: "placement-agent", Role: "place and start workload allocations",
		Capabilities: []ActionKind{ActionPullImage, ActionCreateAllocation, ActionStartAllocation},
	}
}

func (PlacementAgent) Propose(goal Goal, world World) (Proposal, error) {
	descriptor := (PlacementAgent{}).Descriptor()
	proposal := Proposal{
		ID: fmt.Sprintf("%s-r%d", descriptor.ID, world.Revision), AgentID: descriptor.ID,
		GoalID: goal.ID, BasedOnRevision: world.Revision,
		Reasoning: "place missing replicas on healthy nodes that satisfy hard constraints and capacity",
	}
	existing := make(map[int]bool)
	for _, allocation := range world.Allocations {
		if allocation.Workload != goal.Workload.Name || allocation.Phase == AllocationStopped {
			continue
		}
		node := world.Nodes[allocation.Node]
		if node == nil || allocation.Image != goal.Workload.Image ||
			allocation.Resources != goal.Workload.Resources || !nodeAllowed(goal.Constraints, *node) {
			// A drifted allocation is the rollout agent's business. Placement
			// must not treat the replica slot as filled, or the replacement
			// would never be created after the rollout retires it.
			continue
		}
		existing[allocation.Replica] = true
	}
	reserved := make(map[string]Resources)
	for replica := 0; replica < goal.Workload.Replicas; replica++ {
		if existing[replica] {
			continue
		}
		node, err := selectNode(goal, world, reserved)
		if err != nil {
			return proposal, err
		}
		allocationID := fmt.Sprintf("%s-%d", goal.Workload.Name, replica)
		var pullID string
		if !node.Images[goal.Workload.Image] {
			pullID = "pull-" + allocationID
			proposal.Actions = append(proposal.Actions, Action{
				ID: pullID, Kind: ActionPullImage, Target: goal.Workload.Image,
				Node: node.ID, Image: goal.Workload.Image,
			})
		}
		createID := "create-" + allocationID
		create := Action{
			ID: createID, Kind: ActionCreateAllocation, Target: allocationID,
			Workload: goal.Workload.Name, Node: node.ID, Image: goal.Workload.Image,
			Replica: replica, Resources: goal.Workload.Resources,
		}
		if pullID != "" {
			create.DependsOn = []string{pullID}
		}
		proposal.Actions = append(proposal.Actions, create, Action{
			ID: "start-" + allocationID, Kind: ActionStartAllocation,
			Target: allocationID, Workload: goal.Workload.Name, Node: node.ID,
			DependsOn: []string{createID},
		})
		proposal.ExpectedEvidence = append(proposal.ExpectedEvidence, Check{Kind: "allocation_ready", Target: allocationID, Want: "true"})
		reserved[node.ID] = reserved[node.ID].Add(goal.Workload.Resources)
		// One replica per proposal keeps the mutation budget small. A fresh
		// observation and revision are required before placing the next.
		break
	}
	return proposal, nil
}

func selectNode(goal Goal, world World, reserved map[string]Resources) (*Node, error) {
	ids := make([]string, 0, len(world.Nodes))
	for id := range world.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var selected *Node
	bestFreeMemory := -1
	for _, id := range ids {
		node := world.Nodes[id]
		if !node.Healthy || !nodeAllowed(goal.Constraints, *node) {
			continue
		}
		used := node.Used.Add(reserved[id]).Add(goal.Workload.Resources)
		if !used.Fits(node.Capacity) {
			continue
		}
		freeMemory := node.Capacity.MemoryMB - used.MemoryMB
		if selected == nil || freeMemory > bestFreeMemory {
			selected = node
			bestFreeMemory = freeMemory
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("no healthy node satisfies placement and capacity constraints")
	}
	return selected, nil
}

type NetworkAgent struct{}

func (NetworkAgent) Descriptor() AgentDescriptor {
	return AgentDescriptor{
		ID: "network-agent", Role: "publish approved workload routes",
		Capabilities: []ActionKind{ActionPublishRoute},
	}
}

func (NetworkAgent) Propose(goal Goal, world World) (Proposal, error) {
	descriptor := (NetworkAgent{}).Descriptor()
	proposal := Proposal{
		ID: fmt.Sprintf("%s-r%d", descriptor.ID, world.Revision), AgentID: descriptor.ID,
		GoalID: goal.ID, BasedOnRevision: world.Revision,
		Reasoning: "publish the requested route only after every desired replica is ready",
	}
	if goal.Route == nil || world.Routes[goal.Route.Host] != nil {
		return proposal, nil
	}
	if matchingReadyAllocations(goal, world) < goal.Workload.Replicas {
		return proposal, nil
	}
	proposal.Actions = []Action{{
		ID: "publish-" + goal.Workload.Name, Kind: ActionPublishRoute,
		Target: goal.Route.Host, Workload: goal.Workload.Name,
		Node: routeNode(goal, world),
		Port: goal.Route.Port, Exposure: goal.Route.Exposure,
	}}
	proposal.ExpectedEvidence = []Check{{Kind: "route_reachable", Target: goal.Route.Host, Want: "true"}}
	return proposal, nil
}

// routeNode picks the node that will serve a route. Routes are published by the
// gateway on a node already running the workload, so traffic does not depend on
// a node that holds no replica.
func routeNode(goal Goal, world World) string {
	nodes := make([]string, 0, len(world.Allocations))
	for _, allocation := range world.Allocations {
		if allocation.Workload == goal.Workload.Name && allocation.Ready {
			nodes = append(nodes, allocation.Node)
		}
	}
	if len(nodes) == 0 {
		return ""
	}
	sort.Strings(nodes)
	return nodes[0]
}
