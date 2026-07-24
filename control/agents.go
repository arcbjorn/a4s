package control

import (
	"fmt"
	"sort"
)

// MaxReplicasPerProposal bounds how many replicas one authorization may create.
//
// Each replica costs several actions, so this also keeps a proposal within the
// kernel's action limit. More importantly it bounds blast radius: a proposal
// that placed every replica at once would commit the whole workload to an image
// before any of it had been observed running.
const MaxReplicasPerProposal = 2

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
	placed := 0
	for replica := 0; replica < goal.Workload.Replicas; replica++ {
		if existing[replica] {
			continue
		}
		node, err := selectNode(goal, world, reserved)
		if err != nil {
			return proposal, err
		}
		// Durable data does not move. A workload whose volume already exists is
		// pinned to that node, whatever placement would otherwise prefer.
		if moving := volumeInFlight(goal, world); moving != "" {
			// A workload must not be placed while its data is in flight. The
			// move will settle on one node, and starting before it does could
			// attach a copy that is about to be superseded.
			return proposal, fmt.Errorf(
				"volume %q for workload %q is being moved; wait for the handoff to finish",
				moving, goal.Workload.Name)
		}
		if home := volumeHome(goal, world); home != "" {
			pinned, ok := world.Nodes[home]
			if !ok || !pinned.Healthy {
				return proposal, fmt.Errorf(
					"volume for workload %q lives on node %q, which is not available",
					goal.Workload.Name, home)
			}
			node = pinned
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
		proposal.Actions = append(proposal.Actions, create)

		startDeps := []string{createID}
		// Storage must be in place before the process starts, or the workload
		// comes up and writes to an empty directory that is not its volume.
		for _, ref := range goal.Workload.Volumes {
			volume := ref
			attachDeps := []string{createID}
			if _, exists := world.Volumes[ref.Name]; !exists {
				createVolumeID := "create-volume-" + ref.Name
				proposal.Actions = append(proposal.Actions, Action{
					ID: createVolumeID, Kind: ActionCreateVolume, Target: ref.Name,
					Workload: goal.Workload.Name, Node: node.ID, Volume: &volume,
				})
				attachDeps = append(attachDeps, createVolumeID)
			}
			attachID := "attach-volume-" + ref.Name + "-" + allocationID
			proposal.Actions = append(proposal.Actions, Action{
				ID: attachID, Kind: ActionAttachVolume, Target: allocationID,
				Workload: goal.Workload.Name, Node: node.ID, Volume: &volume,
				DependsOn: attachDeps,
			})
			startDeps = append(startDeps, attachID)
		}
		// Credentials must be in place before the process starts, or the
		// workload comes up without them and fails in a way that looks like an
		// application bug rather than a missing mount.
		for _, ref := range goal.Workload.Secrets {
			mountID := "mount-" + ref.Name + "-" + allocationID
			secret := ref
			proposal.Actions = append(proposal.Actions, Action{
				ID: mountID, Kind: ActionMountSecret, Target: allocationID,
				Workload: goal.Workload.Name, Node: node.ID, Secret: &secret,
				DependsOn: []string{createID},
			})
			startDeps = append(startDeps, mountID)
		}
		// A workload that serves a port needs its own address before it starts,
		// so replicas on one node do not contend for a host port.
		if goal.Workload.Port > 0 {
			attachID := "attach-" + allocationID
			proposal.Actions = append(proposal.Actions, Action{
				ID: attachID, Kind: ActionAttachNetwork, Target: allocationID,
				Workload: goal.Workload.Name, Node: node.ID, Port: goal.Workload.Port,
				DependsOn: []string{createID},
			})
			startDeps = append(startDeps, attachID)
		}
		proposal.Actions = append(proposal.Actions, Action{
			ID: "start-" + allocationID, Kind: ActionStartAllocation,
			Target: allocationID, Workload: goal.Workload.Name, Node: node.ID,
			DependsOn: startDeps,
		})
		proposal.ExpectedEvidence = append(proposal.ExpectedEvidence, Check{Kind: CheckAllocationReady, Target: allocationID, Want: "true"})
		reserved[node.ID] = reserved[node.ID].Add(goal.Workload.Resources)
		placed++
		// Placing every missing replica in one proposal would make the blast
		// radius of a single authorization the whole workload. Batching keeps
		// each authorized mutation small and forces re-observation before the
		// next batch, which is what lets a bad image be caught after one
		// replica rather than all of them.
		if placed >= MaxReplicasPerProposal {
			break
		}
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

// volumeHome reports the node a workload's existing volumes live on.
//
// Local storage stays local, so an existing volume determines placement rather
// than the other way around. A workload with no volumes yet is placed freely.
func volumeHome(goal Goal, world World) string {
	for _, ref := range goal.Workload.Volumes {
		if volume, ok := world.Volumes[ref.Name]; ok {
			return volume.Node
		}
	}
	return ""
}

// volumeInFlight reports a volume the workload needs that is mid-move.
func volumeInFlight(goal Goal, world World) string {
	for _, ref := range goal.Workload.Volumes {
		if volume, ok := world.Volumes[ref.Name]; ok && volume.Handoff != nil {
			return ref.Name
		}
	}
	return ""
}
