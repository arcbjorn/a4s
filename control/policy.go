package control

import (
	"fmt"
	"strings"
	"time"
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
				// Installing an agent's tool envelope is part of preparing an
				// allocation to run, alongside mounting its secrets.
				ActionGrantTools: true,
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
				// Draining is how a rollout retires an agent instance without
				// destroying the task it holds.
				ActionDrainAllocation: true,
				// Releasing a volume is part of retiring an allocation, but the
				// rollout agent may not create, snapshot, or restore one.
				ActionDetachVolume: true,
			},
			// The storage agent may protect and recover data but may not place
			// or start workloads. Backup authority and execution authority stay
			// in separate capability sets.
			"storage-agent": {
				ActionSnapshotVolume:  true,
				ActionDatabaseBackup:  true,
				ActionBackupSnapshot:  true,
				ActionRestoreSnapshot: true,
				ActionQuiesceVolume:   true,
				ActionTransferVolume:  true,
				ActionAdoptVolume:     true,
				ActionPruneSnapshots:  true,
				ActionVerifyBackup:    true,
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
	if err := requireEvidenceChecks(goal, proposal); err != nil {
		return err
	}
	return nil
}

func requireEvidenceChecks(goal Goal, proposal Proposal) error {
	for _, action := range proposal.Actions {
		var requiredKind string
		switch action.Kind {
		case ActionStartAllocation:
			// An agent is ready when it has reached its provider with budget
			// remaining. A TCP accept would not establish that: an agent runtime
			// can be listening and unable to do any work at all.
			if goal.Workload.Runtime != nil {
				requiredKind = CheckAgentReady
			} else {
				requiredKind = CheckAllocationReady
			}
		case ActionPublishRoute:
			requiredKind = CheckRouteReachable
		case ActionDrainAllocation:
			requiredKind = CheckAllocationDrained
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

// actionValidators maps each action kind to the function that checks it against
// the goal and world. Splitting one validator per kind keeps the kernel's
// authorization logic reviewable case by case: each function is the complete,
// self-contained argument for why one action is safe.
var actionValidators = map[ActionKind]func(Goal, World, Action) error{
	ActionPullImage:        validatePullImage,
	ActionCreateAllocation: validateCreateAllocation,
	ActionCreateVolume:     validateCreateVolume,
	ActionAttachVolume:     validateAttachVolume,
	ActionDetachVolume:     validateDetachVolume,
	ActionSnapshotVolume:   validateSnapshotVolume,
	ActionQuiesceVolume:    validateQuiesceVolume,
	ActionTransferVolume:   validateTransferVolume,
	ActionAdoptVolume:      validateAdoptVolume,
	ActionDatabaseBackup:   validateDatabaseBackup,
	ActionVerifyBackup:     validateVerifyBackup,
	ActionPruneSnapshots:   validatePruneSnapshots,
	ActionBackupSnapshot:   validateBackupSnapshot,
	ActionRestoreSnapshot:  validateRestoreSnapshot,
	ActionMountSecret:      validateMountSecret,
	ActionGrantTools:       validateGrantTools,
	ActionDrainAllocation:  validateDrainAllocation,
	ActionAttachNetwork:    validateAttachNetwork,
	ActionStartAllocation:  validateStartAllocation,
	ActionStopAllocation:   validateStopAllocation,
	ActionDeleteAllocation: validateDeleteAllocation,
	ActionPublishRoute:     validatePublishRoute,
}

func validateAction(goal Goal, world World, action Action) error {
	validator, ok := actionValidators[action.Kind]
	if !ok {
		return fmt.Errorf("unknown action kind %q", action.Kind)
	}
	return validator(goal, world, action)
}

func validatePullImage(goal Goal, world World, action Action) error {
	node, ok := world.Nodes[action.Node]
	if !ok || !node.Healthy {
		return fmt.Errorf("node %q is missing or unhealthy", action.Node)
	}
	if action.Image != goal.Workload.Image || !strings.Contains(action.Image, "@sha256:") {
		return fmt.Errorf("image must match the goal's pinned digest")
	}
	return nil
}

func validateCreateAllocation(goal Goal, world World, action Action) error {
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
	// A queue-backed agent workload may exceed the goal's replica count, but
	// only up to the ceiling the queue declares. The kernel recomputes that
	// bound itself rather than trusting the agent's arithmetic.
	if action.Replica < 0 || action.Replica >= authorizedReplicas(goal, world) {
		return fmt.Errorf("replica index is outside goal")
	}
	return validateAgentPlacement(goal, *node, action, world.Now())
}

func validateCreateVolume(goal Goal, world World, action Action) error {
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

	return nil
}

func validateAttachVolume(goal Goal, world World, action Action) error {
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
	// A volume mid-move must not be attached. The data is in flight, and a
	// writer on either node could diverge from what is being transferred.
	if volume.Handoff != nil {
		return fmt.Errorf("volume %q is being moved to node %q and cannot be attached",
			action.Volume.Name, volume.Handoff.To)
	}
	// Local storage stays local. Attaching across nodes would mean the data
	// is not where the workload is.
	if volume.Node != allocation.Node {
		return fmt.Errorf("volume %q lives on node %q but allocation %q is on %q",
			action.Volume.Name, volume.Node, action.Target, allocation.Node)
	}

	return nil
}

func validateDetachVolume(goal Goal, world World, action Action) error {
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

	return nil
}

func validateSnapshotVolume(goal Goal, world World, action Action) error {
	if action.Volume == nil {
		return fmt.Errorf("snapshot volume requires a volume reference")
	}
	volume, ok := world.Volumes[action.Volume.Name]
	if !ok {
		return fmt.Errorf("volume %q does not exist", action.Volume.Name)
	}
	// A raw filesystem snapshot of a running database is inconsistent: its
	// files change under the copy, and a restore of it may not start. A
	// database must be backed up with database_backup instead.
	if goal.Workload.Engine != "" {
		return fmt.Errorf(
			"volume %q backs a %s database; use database_backup for a consistent copy",
			action.Volume.Name, goal.Workload.Engine)
	}
	// A snapshot taken from a live writer may be internally inconsistent,
	// and an operator would later trust it for restore.
	if volume.Owner != "" {
		return fmt.Errorf("volume %q must be quiesced before snapshotting; it is attached to %q",
			action.Volume.Name, volume.Owner)
	}

	return nil
}

func validateQuiesceVolume(goal Goal, world World, action Action) error {
	if action.Volume == nil {
		return fmt.Errorf("quiesce volume requires a volume reference")
	}
	volume, ok := world.Volumes[action.Volume.Name]
	if !ok {
		return fmt.Errorf("volume %q does not exist", action.Volume.Name)
	}
	// Quiescing means the writer has stopped. Beginning a move under a live
	// process would snapshot data that is still changing.
	if volume.Owner != "" {
		return fmt.Errorf("volume %q must be detached before a move; it is held by %q",
			action.Volume.Name, volume.Owner)
	}
	target, ok := world.Nodes[action.Node]
	if !ok || !target.Healthy {
		return fmt.Errorf("handoff target %q is missing or unhealthy", action.Node)
	}
	if action.Node == volume.Node {
		return fmt.Errorf("volume %q already lives on node %q", action.Volume.Name, action.Node)
	}
	// Moving data is irreversible in practice: the origin copy is expected
	// to be reclaimed. It needs a separately authenticated decision.
	if !hasApproval(world, goal.ID, "move-volume") {
		return fmt.Errorf("moving volume %q requires move-volume approval", action.Volume.Name)
	}

	return nil
}

func validateTransferVolume(goal Goal, world World, action Action) error {
	if action.Volume == nil {
		return fmt.Errorf("transfer volume requires a volume reference")
	}
	volume, ok := world.Volumes[action.Volume.Name]
	if !ok {
		return fmt.Errorf("volume %q does not exist", action.Volume.Name)
	}
	if volume.Handoff == nil {
		return fmt.Errorf("volume %q has no handoff in progress", action.Volume.Name)
	}
	if volume.Handoff.Phase != HandoffSnapshotted {
		return fmt.Errorf("volume %q must be snapshotted before transfer, not %q",
			action.Volume.Name, volume.Handoff.Phase)
	}
	if action.Node != volume.Handoff.To {
		return fmt.Errorf("transfer target %q is not the handoff target %q",
			action.Node, volume.Handoff.To)
	}

	return nil
}

func validateAdoptVolume(goal Goal, world World, action Action) error {
	if action.Volume == nil {
		return fmt.Errorf("adopt volume requires a volume reference")
	}
	volume, ok := world.Volumes[action.Volume.Name]
	if !ok {
		return fmt.Errorf("volume %q does not exist", action.Volume.Name)
	}
	if volume.Handoff == nil {
		return fmt.Errorf("volume %q has no handoff in progress", action.Volume.Name)
	}
	// Ownership moves only after the target proved it holds the data.
	if volume.Handoff.Phase != HandoffTransferred {
		return fmt.Errorf("volume %q must be transferred before adoption, not %q",
			action.Volume.Name, volume.Handoff.Phase)
	}
	if action.Node != volume.Handoff.To {
		return fmt.Errorf("adoption node %q is not the handoff target %q",
			action.Node, volume.Handoff.To)
	}

	return nil
}

func validateDatabaseBackup(goal Goal, world World, action Action) error {
	if action.Volume == nil {
		return fmt.Errorf("database backup requires a volume reference")
	}
	if action.Snapshot == "" {
		return fmt.Errorf("database backup requires a backup label")
	}
	if goal.Workload.Engine == "" {
		return fmt.Errorf("database backup is only for database workloads")
	}
	volume, ok := world.Volumes[action.Volume.Name]
	if !ok {
		return fmt.Errorf("volume %q does not exist", action.Volume.Name)
	}
	// Unlike a filesystem snapshot, a database backup runs against the live
	// database. The engine produces a consistent copy while serving, so it
	// must be attached and running, not detached.
	if volume.Owner == "" {
		return fmt.Errorf("database volume %q must be attached to back it up", action.Volume.Name)
	}

	return nil
}

func validateVerifyBackup(goal Goal, world World, action Action) error {
	if action.Volume == nil {
		return fmt.Errorf("verify backup requires a volume reference")
	}
	if action.Snapshot == "" {
		return fmt.Errorf("verify backup requires a snapshot id")
	}
	volume, ok := world.Volumes[action.Volume.Name]
	if !ok {
		return fmt.Errorf("volume %q does not exist", action.Volume.Name)
	}
	// Only a recorded snapshot can be verified; there is nothing else whose
	// recoverability is meaningful to check.
	if _, known := volume.Snapshots[action.Snapshot]; !known {
		return fmt.Errorf("snapshot %q of volume %q was never recorded",
			action.Snapshot, action.Volume.Name)
	}
	// Verification is read-only: it restores into scratch space and
	// discards. It needs no approval and can run against a live volume,
	// which is what lets it happen on a schedule without disruption.

	return nil
}

func validatePruneSnapshots(goal Goal, world World, action Action) error {
	if action.Volume == nil {
		return fmt.Errorf("prune snapshots requires a volume reference")
	}
	volume, ok := world.Volumes[action.Volume.Name]
	if !ok {
		return fmt.Errorf("volume %q does not exist", action.Volume.Name)
	}
	// Keeping zero snapshots would leave no recovery point at all, which
	// defeats the reason snapshots exist. Retention has a floor of one.
	if action.Retain < 1 {
		return fmt.Errorf("prune must retain at least one snapshot")
	}
	// Pruning during a move could remove the very snapshot being
	// transferred, stranding the handoff.
	if volume.Handoff != nil {
		return fmt.Errorf("volume %q is being moved and cannot be pruned", action.Volume.Name)
	}

	return nil
}

func validateBackupSnapshot(goal Goal, world World, action Action) error {
	if action.Volume == nil {
		return fmt.Errorf("backup snapshot requires a volume reference")
	}
	if action.Snapshot == "" {
		return fmt.Errorf("backup snapshot requires a snapshot id")
	}
	volume, ok := world.Volumes[action.Volume.Name]
	if !ok {
		return fmt.Errorf("volume %q does not exist", action.Volume.Name)
	}
	// Only a verified snapshot may be shipped. Backing up unverified
	// content would put something off-host that nobody has checked.
	if _, known := volume.Snapshots[action.Snapshot]; !known {
		return fmt.Errorf("snapshot %q of volume %q was never recorded",
			action.Snapshot, action.Volume.Name)
	}

	return nil
}

func validateRestoreSnapshot(goal Goal, world World, action Action) error {
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
	//
	// A snapshot recorded as backed up remains restorable even when the
	// node's local copy is gone, which is exactly the host-loss case.
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

	return nil
}

func validateMountSecret(goal Goal, world World, action Action) error {
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

	return nil
}

func validateGrantTools(goal Goal, world World, action Action) error {
	allocation, ok := world.Allocations[action.Target]
	if !ok {
		return fmt.Errorf("allocation %q does not exist", action.Target)
	}
	// The envelope is installed before the agent runs. Granting a tool to a
	// started agent would widen a blast radius the kernel already authorized,
	// which is the escalation this ordering exists to prevent.
	if allocation.Phase != AllocationCreated {
		return fmt.Errorf("tools must be granted before allocation %q starts", action.Target)
	}
	if goal.Workload.Runtime == nil {
		return fmt.Errorf("tool grants are only for agent workloads")
	}
	// Every granted tool must be one the goal declared. Otherwise an agent
	// could receive a capability the operator never authorized, which is the
	// tool-grant equivalent of mounting an undeclared secret.
	for _, grant := range action.Tools {
		if !goalDeclaresTool(goal, grant) {
			return fmt.Errorf("tool %q is not declared by the goal", grant.Name)
		}
	}
	// A mutating tool lets an agent change state outside a4s, where no
	// compensation or event log reaches. That needs a separately
	// authenticated decision rather than an agent's judgement.
	if actionGrantsMutatingTool(action) && !hasApproval(world, goal.ID, "agent-mutating-tools") {
		return fmt.Errorf("granting mutating tools to %q requires agent-mutating-tools approval",
			action.Target)
	}

	return nil
}

func validateDrainAllocation(goal Goal, world World, action Action) error {
	allocation, ok := world.Allocations[action.Target]
	if !ok {
		return fmt.Errorf("allocation %q does not exist", action.Target)
	}
	if allocation.Phase != AllocationRunning {
		return fmt.Errorf("allocation %q is not running", action.Target)
	}
	if goal.Workload.Runtime == nil {
		return fmt.Errorf("drain is only for agent workloads")
	}
	if action.Workload != allocation.Workload {
		return fmt.Errorf("workload differs from allocation")
	}

	return nil
}

func validateAttachNetwork(goal Goal, world World, action Action) error {
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

	return nil
}

func validateStartAllocation(goal Goal, world World, action Action) error {
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
	// An agent workload that declared tools must hold its envelope before it
	// runs. Starting without it would leave the runtime to decide its own
	// capabilities, which is precisely what the envelope removes.
	if runtime := goal.Workload.Runtime; runtime != nil {
		if len(runtime.Tools) > 0 && len(allocation.Tools) == 0 {
			return fmt.Errorf("agent allocation %q has no tool grant", action.Target)
		}
		if allocation.Budget.IsZero() {
			return fmt.Errorf("agent allocation %q holds no budget", action.Target)
		}
		// Starting an agent that already spent its ceiling would burn the
		// budget again to reach the same exhausted state.
		if allocation.Exhausted() {
			return fmt.Errorf("agent allocation %q has exhausted its budget", action.Target)
		}
	}

	return nil
}

func validateStopAllocation(goal Goal, world World, action Action) error {
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
	// An agent instance holds task context that a stateless replica does
	// not. Stopping it mid-task destroys work rather than shifting load, so
	// the instance must first be drained and observed holding nothing.
	//
	// An exhausted agent is exempt: it has hit a declared ceiling and cannot
	// make progress on its task, so waiting for it to finish would wait
	// forever.
	if goal.Workload.Runtime != nil && allocation.Task != "" && !allocation.Exhausted() {
		if !allocation.Draining {
			return fmt.Errorf("agent allocation %q must be drained before it stops", action.Target)
		}
		return fmt.Errorf("agent allocation %q is still working on task %q",
			action.Target, allocation.Task)
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

	return nil
}

func validateDeleteAllocation(goal Goal, world World, action Action) error {
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

	return nil
}

func validatePublishRoute(goal Goal, world World, action Action) error {
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
		Queues:      make(map[string]*Queue, len(world.Queues)),
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
		copyNode.Providers = make(map[string]ProviderReach, len(node.Providers))
		for provider, reach := range node.Providers {
			copyNode.Providers[provider] = reach
		}
		clone.Nodes[id] = &copyNode
	}
	for name, queue := range world.Queues {
		copyQueue := *queue
		clone.Queues[name] = &copyQueue
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
		if allocation.Tools != nil {
			copyAllocation.Tools = append([]ToolGrant(nil), allocation.Tools...)
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
		if volume.SnapshotOrder != nil {
			copyVolume.SnapshotOrder = append([]string(nil), volume.SnapshotOrder...)
		}
		if volume.Handoff != nil {
			copyHandoff := *volume.Handoff
			copyVolume.Handoff = &copyHandoff
		}
		if volume.Backups != nil {
			copyVolume.Backups = make(map[string]string, len(volume.Backups))
			for id, location := range volume.Backups {
				copyVolume.Backups[id] = location
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
			// A draining or exhausted agent is not serving capacity. Counting it
			// would let a goal look satisfied by instances that are on their way
			// out or can no longer afford to do anything.
			if allocation.Draining || allocation.Exhausted() {
				continue
			}
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

// authorizedReplicas is the highest replica count the kernel will permit.
//
// For an ordinary workload this is the goal's declared count. A queue-backed
// agent workload may scale above it on observed demand, but never above the
// queue's MaxWorkers: that ceiling is what keeps a queue spike from becoming an
// unbounded spend incident, so the kernel enforces it independently of whatever
// the placement agent calculated.
func authorizedReplicas(goal Goal, world World) int {
	runtime := goal.Workload.Runtime
	if runtime == nil || runtime.Queue == "" {
		return goal.Workload.Replicas
	}
	queue, ok := world.Queues[runtime.Queue]
	if !ok || queue.Workload != goal.Workload.Name {
		return goal.Workload.Replicas
	}
	if queue.MaxWorkers > goal.Workload.Replicas {
		return queue.MaxWorkers
	}
	return goal.Workload.Replicas
}

// goalDeclaresTool reports whether the goal authorized this exact grant.
//
// The comparison is exact, including scope and the mutating flag. A grant that
// matched by name alone would let a read-only declaration be installed as a
// mutating capability.
func goalDeclaresTool(goal Goal, grant ToolGrant) bool {
	if goal.Workload.Runtime == nil {
		return false
	}
	for _, declared := range goal.Workload.Runtime.Tools {
		if declared == grant {
			return true
		}
	}
	return false
}

// actionGrantsMutatingTool reports whether an envelope contains any capability
// that changes state outside a4s.
func actionGrantsMutatingTool(action Action) bool {
	for _, grant := range action.Tools {
		if grant.Mutating {
			return true
		}
	}
	return false
}

// validateAgentPlacement enforces the feasibility inputs unique to an agent.
//
// Placement for an ordinary workload is a question of cpu, memory, labels, and
// image presence. An agent adds two facts that are just as hard: it cannot work
// without egress to its provider, and its budget is a resource the node commits
// the same way it commits memory.
func validateAgentPlacement(goal Goal, node Node, action Action, now time.Time) error {
	runtime := goal.Workload.Runtime
	if runtime == nil {
		// An ordinary workload must not reserve agent budget. Allowing it would
		// let any workload consume a node's agent capacity without being subject
		// to any of the ceilings that capacity exists to enforce.
		if !action.Budget.IsZero() {
			return fmt.Errorf("only agent workloads may reserve budget")
		}
		return nil
	}
	if action.Budget != runtime.Budget {
		return fmt.Errorf("allocation budget differs from goal")
	}
	// An agent placed where its provider is unreachable cannot become ready. It
	// is the same class of infeasibility as a missing image, so it is refused at
	// placement rather than discovered at probe time.
	if !node.CanReach(runtime.Provider, now) {
		return fmt.Errorf("node %q cannot reach provider %q", node.ID, runtime.Provider)
	}
	if !node.BudgetUsed.Add(action.Budget).Fits(node.BudgetCapacity) {
		return fmt.Errorf("node %q lacks agent budget capacity", node.ID)
	}
	return nil
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
