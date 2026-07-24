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
				ActionAttachNetwork:    true,
				ActionMountSecret:      true,
				ActionCreateVolume:     true,
				ActionAttachVolume:     true,
				ActionStartAllocation:  true,
			},
			"network-agent": {
				ActionPublishRoute: true,
			},
			// The rollout agent may retire an allocation but may not create
			// one. Replacement is placement's job, which keeps destruction and
			// creation in separate capability sets.
			"rollout-agent": {
				ActionStopAllocation:   true,
				ActionDeleteAllocation: true,
				// Releasing a volume is part of retiring an allocation, but the
				// rollout agent may not create, snapshot, or restore one.
				ActionDetachVolume: true,
			},
			// The storage agent may protect and recover data but may not place
			// or start workloads. Backup authority and execution authority stay
			// in separate capability sets.
			"storage-agent": {
				ActionSnapshotVolume:  true,
				ActionRestoreSnapshot: true,
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

	case ActionCreateVolume:
		if action.Volume == nil {
			return fmt.Errorf("create volume requires a volume reference")
		}
		if !workloadDeclaresVolume(goal, action.Volume.Name) {
			return fmt.Errorf("volume %q is not declared by the goal", action.Volume.Name)
		}
		node, ok := world.Nodes[action.Node]
		if !ok || !node.Healthy {
			return fmt.Errorf("node %q is missing or unhealthy", action.Node)
		}
		if existing, exists := world.Volumes[action.Volume.Name]; exists && existing.Node != action.Node {
			// Creating the same volume on a second node would silently produce
			// two divergent copies of what the operator thinks is one volume.
			return fmt.Errorf("volume %q already exists on node %q", action.Volume.Name, existing.Node)
		}

	case ActionAttachVolume:
		if action.Volume == nil {
			return fmt.Errorf("attach volume requires a volume reference")
		}
		allocation, ok := world.Allocations[action.Target]
		if !ok {
			return fmt.Errorf("allocation %q does not exist", action.Target)
		}
		if allocation.Phase != AllocationCreated {
			return fmt.Errorf("volumes must be attached before allocation %q starts", action.Target)
		}
		volume, ok := world.Volumes[action.Volume.Name]
		if !ok {
			return fmt.Errorf("volume %q does not exist", action.Volume.Name)
		}
		if !workloadDeclaresVolume(goal, action.Volume.Name) {
			return fmt.Errorf("volume %q is not declared by the goal", action.Volume.Name)
		}
		// The single-writer rule. Attaching a volume that another allocation
		// still owns is how two processes end up writing one filesystem.
		if volume.Owner != "" && volume.Owner != action.Target {
			return fmt.Errorf("volume %q is owned by allocation %q", action.Volume.Name, volume.Owner)
		}
		// Local storage stays local. Attaching across nodes would mean the data
		// is not where the workload is.
		if volume.Node != allocation.Node {
			return fmt.Errorf("volume %q lives on node %q but allocation %q is on %q",
				action.Volume.Name, volume.Node, action.Target, allocation.Node)
		}

	case ActionDetachVolume:
		if action.Volume == nil {
			return fmt.Errorf("detach volume requires a volume reference")
		}
		volume, ok := world.Volumes[action.Volume.Name]
		if !ok {
			return fmt.Errorf("volume %q does not exist", action.Volume.Name)
		}
		if volume.Owner != "" && volume.Owner != action.Target {
			return fmt.Errorf("allocation %q does not own volume %q", action.Target, action.Volume.Name)
		}
		// Detaching from a running writer would pull storage out from under a
		// live process. Stopping first is what makes the release safe.
		if allocation, ok := world.Allocations[action.Target]; ok && allocation.Phase == AllocationRunning {
			return fmt.Errorf("allocation %q must stop before releasing volume %q",
				action.Target, action.Volume.Name)
		}

	case ActionSnapshotVolume:
		if action.Volume == nil {
			return fmt.Errorf("snapshot volume requires a volume reference")
		}
		volume, ok := world.Volumes[action.Volume.Name]
		if !ok {
			return fmt.Errorf("volume %q does not exist", action.Volume.Name)
		}
		// A snapshot taken from a live writer may be internally inconsistent,
		// and an operator would later trust it for restore.
		if volume.Owner != "" {
			return fmt.Errorf("volume %q must be quiesced before snapshotting; it is attached to %q",
				action.Volume.Name, volume.Owner)
		}

	case ActionRestoreSnapshot:
		if action.Volume == nil {
			return fmt.Errorf("restore snapshot requires a volume reference")
		}
		if action.Snapshot == "" {
			return fmt.Errorf("restore snapshot requires a snapshot id")
		}
		volume, ok := world.Volumes[action.Volume.Name]
		if !ok {
			return fmt.Errorf("volume %q does not exist", action.Volume.Name)
		}
		// Only a snapshot this cluster took and verified may be restored. An
		// operator cannot name arbitrary content and have it written over data.
		if _, known := volume.Snapshots[action.Snapshot]; !known {
			return fmt.Errorf("snapshot %q of volume %q was never recorded",
				action.Snapshot, action.Volume.Name)
		}
		// Restoring over a live writer would replace the filesystem underneath
		// a running process.
		if volume.Owner != "" {
			return fmt.Errorf("volume %q must be detached before restore; it is attached to %q",
				action.Volume.Name, volume.Owner)
		}
		// Restore overwrites durable data irreversibly. Like destruction, it
		// needs a separately authenticated decision rather than an agent's.
		if !hasApproval(world, goal.ID, "restore-volume") {
			return fmt.Errorf("restoring volume %q requires restore-volume approval", action.Volume.Name)
		}

	case ActionMountSecret:
		allocation, ok := world.Allocations[action.Target]
		if !ok {
			return fmt.Errorf("allocation %q does not exist", action.Target)
		}
		if allocation.Phase != AllocationCreated {
			return fmt.Errorf("secrets must be mounted before allocation %q starts", action.Target)
		}
		if action.Secret == nil {
			return fmt.Errorf("mount secret requires a secret reference")
		}
		// The reference must be one the goal actually declared. Otherwise an
		// agent could mount material the operator never authorized for this
		// workload.
		if !goalDeclaresSecret(goal, *action.Secret) {
			return fmt.Errorf("secret %q is not declared by the goal", action.Secret.Name)
		}

	case ActionAttachNetwork:
		allocation, ok := world.Allocations[action.Target]
		if !ok {
			return fmt.Errorf("allocation %q does not exist", action.Target)
		}
		if allocation.Phase != AllocationCreated {
			return fmt.Errorf("allocation %q must be attached before it starts", action.Target)
		}
		if action.Workload != allocation.Workload {
			return fmt.Errorf("workload differs from allocation")
		}

	case ActionStartAllocation:
		allocation, ok := world.Allocations[action.Target]
		if !ok || allocation.Phase != AllocationCreated {
			return fmt.Errorf("allocation %q is not created", action.Target)
		}
		if action.Workload != goal.Workload.Name || allocation.Workload != action.Workload {
			return fmt.Errorf("workload differs from goal")
		}
		// Every declared volume must be attached at the current generation. A
		// stale generation means this allocation was fenced while it was
		// unreachable and must not resume writing.
		for _, ref := range goal.Workload.Volumes {
			volume, ok := world.Volumes[ref.Name]
			if !ok {
				return fmt.Errorf("allocation %q needs volume %q, which does not exist", action.Target, ref.Name)
			}
			attached, held := allocation.Volumes[ref.Name]
			if !held {
				return fmt.Errorf("allocation %q is missing volume %q", action.Target, ref.Name)
			}
			if attached != volume.Generation {
				return fmt.Errorf("allocation %q holds a fenced generation of volume %q", action.Target, ref.Name)
			}
			if volume.Owner != action.Target {
				return fmt.Errorf("volume %q is no longer owned by allocation %q", ref.Name, action.Target)
			}
		}
		// Every declared secret must be mounted before the workload starts, or
		// it would run without credentials it was promised.
		for _, ref := range goal.Workload.Secrets {
			if allocation.Secrets[ref.Name] != ref.Version {
				return fmt.Errorf("allocation %q is missing secret %q version %q",
					action.Target, ref.Name, ref.Version)
			}
		}
		// A workload with a port needs its own address before it starts.
		// Starting first would leave it either unreachable or, without a
		// namespace, contending with its own replicas for a host port.
		if goal.Workload.Port > 0 && allocation.Address == "" {
			return fmt.Errorf("allocation %q has no network address", action.Target)
		}

	case ActionStopAllocation:
		allocation, ok := world.Allocations[action.Target]
		if !ok {
			return fmt.Errorf("allocation %q does not exist", action.Target)
		}
		if allocation.Phase != AllocationRunning {
			return fmt.Errorf("allocation %q is not running", action.Target)
		}
		if action.Workload != allocation.Workload {
			return fmt.Errorf("workload differs from allocation")
		}
		// The kernel enforces the availability floor independently. An agent
		// that respects its own budget is convenient; an agent that cannot
		// exceed it is a safety property.
		if allocation.ReadyAt(world.Now()) {
			floor := disruptionFloor(goal)
			if remaining := servingAllocations(goal, world) - 1; remaining < floor {
				return fmt.Errorf("stopping %q would leave %d ready replicas, below the availability floor of %d",
					action.Target, remaining, floor)
			}
		}

	case ActionDeleteAllocation:
		allocation, ok := world.Allocations[action.Target]
		if !ok {
			return fmt.Errorf("allocation %q does not exist", action.Target)
		}
		// Deleting a running allocation would destroy a workload without the
		// operator ever observing it stop. Stop is a required prior step.
		if allocation.Phase == AllocationRunning {
			return fmt.Errorf("allocation %q must be stopped before deletion", action.Target)
		}
		if action.Workload != allocation.Workload {
			return fmt.Errorf("workload differs from allocation")
		}
		// Deleting an allocation that still owns a volume would orphan the
		// storage, leaving data no workload can reach and no operator expects.
		if len(allocation.Volumes) > 0 {
			return fmt.Errorf("allocation %q must release its volumes before deletion", action.Target)
		}
		// Destroying durable data is the one action that cannot be undone by
		// reconciliation, so it requires a separately authenticated approval
		// rather than an agent's judgement.
		if allocation.Stateful && !hasApproval(world, goal.ID, "destroy-stateful") {
			return fmt.Errorf("deleting stateful allocation %q requires destroy-stateful approval", action.Target)
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
		ObservedAt:  world.ObservedAt,
		Nodes:       make(map[string]*Node, len(world.Nodes)),
		Allocations: make(map[string]*Allocation, len(world.Allocations)),
		Routes:      make(map[string]*Route, len(world.Routes)),
		Volumes:     make(map[string]*Volume, len(world.Volumes)),
		Approvals:   make(map[string]*Approval, len(world.Approvals)),
		KnownGood:   make(map[string]string, len(world.KnownGood)),
	}
	for workload, image := range world.KnownGood {
		clone.KnownGood[workload] = image
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
		if allocation.Volumes != nil {
			copyAllocation.Volumes = make(map[string]uint64, len(allocation.Volumes))
			for name, generation := range allocation.Volumes {
				copyAllocation.Volumes[name] = generation
			}
		}
		if allocation.Secrets != nil {
			copyAllocation.Secrets = make(map[string]string, len(allocation.Secrets))
			for name, version := range allocation.Secrets {
				copyAllocation.Secrets[name] = version
			}
		}
		clone.Allocations[id] = &copyAllocation
	}
	for host, route := range world.Routes {
		copyRoute := *route
		clone.Routes[host] = &copyRoute
	}
	for name, volume := range world.Volumes {
		copyVolume := *volume
		if volume.Snapshots != nil {
			copyVolume.Snapshots = make(map[string]string, len(volume.Snapshots))
			for id, checksum := range volume.Snapshots {
				copyVolume.Snapshots[id] = checksum
			}
		}
		clone.Volumes[name] = &copyVolume
	}
	for id, approval := range world.Approvals {
		copyApproval := *approval
		clone.Approvals[id] = &copyApproval
	}
	return clone
}

// disruptionFloor is the number of ready replicas that must survive any single
// authorized disruption. A single-replica workload cannot be updated without a
// gap, so its floor is zero; anything larger keeps a replica serving.
func disruptionFloor(goal Goal) int {
	if goal.Workload.Replicas <= 1 {
		return 0
	}
	return goal.Workload.Replicas - 1
}

// servingAllocations counts replicas currently serving the workload, whatever
// image they run.
//
// Availability during a rollout is about what users can reach, not about which
// version is deployed. Counting only allocations matching the goal's new image
// would read as zero availability at the start of every rollout and either
// block it forever or permit unlimited disruption.
func servingAllocations(goal Goal, world World) int {
	serving := 0
	now := world.Now()
	for _, allocation := range world.Allocations {
		if allocation.Workload == goal.Workload.Name && allocation.ReadyAt(now) {
			serving++
		}
	}
	return serving
}

func matchingReadyAllocations(goal Goal, world World) int {
	ready := 0
	now := world.Now()
	for _, allocation := range world.Allocations {
		node := world.Nodes[allocation.Node]
		if node != nil && allocation.ReadyAt(now) && allocation.Workload == goal.Workload.Name &&
			allocation.Image == goal.Workload.Image && allocation.Resources == goal.Workload.Resources &&
			nodeAllowed(goal.Constraints, *node) {
			ready++
		}
	}
	return ready
}

// goalDeclaresSecret reports whether the goal authorized this exact reference.
func goalDeclaresSecret(goal Goal, ref SecretRef) bool {
	for _, declared := range goal.Workload.Secrets {
		if declared == ref {
			return true
		}
	}
	return false
}

// workloadDeclaresVolume reports whether the goal authorized this volume.
func workloadDeclaresVolume(goal Goal, name string) bool {
	for _, ref := range goal.Workload.Volumes {
		if ref.Name == name {
			return true
		}
	}
	return false
}
