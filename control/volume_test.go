package control

import (
	"strings"
	"testing"
)

func statefulScenario(t *testing.T) Scenario {
	t.Helper()
	scenario := validScenario()
	scenario.Goal.Route = nil
	scenario.Goal.Workload.Stateful = true
	scenario.Goal.Workload.Volumes = []VolumeRef{
		{Name: "app-data", MountPath: "/var/lib/app"},
	}
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	return scenario
}

// statefulWorld builds a world with a volume already owned by an allocation.
func statefulWorld(t *testing.T, goal Goal) World {
	t.Helper()
	world := cloneWorld(validScenario().World)
	world.normalize()
	world.Volumes["app-data"] = &Volume{
		Name: "app-data", Node: "base", Owner: "app-0", Generation: 1,
	}
	world.Allocations["app-0"] = &Allocation{
		ID: "app-0", Workload: "app", Node: "base", Image: testImage,
		Resources: goal.Workload.Resources, Phase: AllocationRunning,
		Stateful: true, Volumes: map[string]uint64{"app-data": 1},
	}
	return world
}

// The single-writer rule. Two processes writing one local filesystem is data
// corruption, and it is the failure this entire subsystem exists to prevent.
func TestVolumeCannotBeAttachedTwice(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)
	world.Allocations["app-1"] = &Allocation{
		ID: "app-1", Workload: "app", Node: "base", Image: testImage,
		Resources: scenario.Goal.Workload.Resources, Phase: AllocationCreated,
	}

	ref := VolumeRef{Name: "app-data", MountPath: "/var/lib/app"}
	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: scenario.Goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "attach", Kind: ActionAttachVolume, Target: "app-1",
			Workload: "app", Node: "base", Volume: &ref,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(PlacementAgent{}).Descriptor(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "is owned by allocation") {
		t.Fatalf("kernel allowed a second writer: %v", err)
	}
}

// Projection is the last line of defence: even if a proposal slipped through,
// evidence claiming a second owner must be refused.
func TestProjectionRefusesSecondOwner(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)
	world.Allocations["app-1"] = &Allocation{
		ID: "app-1", Workload: "app", Node: "base", Phase: AllocationCreated,
	}

	_, err := Project(world, Evidence{
		Kind: EvidenceVolumeAttached, Target: "app-data",
		Observed: map[string]string{"allocation": "app-1"},
	})
	if err == nil || !strings.Contains(err.Error(), "is owned by allocation") {
		t.Fatalf("projection accepted a second owner: %v", err)
	}
}

// An allocation that was fenced while unreachable must not resume writing. The
// generation is what makes that detectable.
func TestFencedAllocationCannotStart(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)
	world.Allocations["app-0"].Phase = AllocationCreated
	// Ownership moved on while this allocation was out of contact.
	world.Volumes["app-data"].Generation = 5

	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: scenario.Goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "start-app-0", Kind: ActionStartAllocation,
			Target: "app-0", Workload: "app", Node: "base",
		}},
		ExpectedEvidence: []Check{{Kind: CheckAllocationReady, Target: "app-0", Want: "true"}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(PlacementAgent{}).Descriptor(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "fenced generation") {
		t.Fatalf("a fenced allocation was allowed to start: %v", err)
	}
}

// Releasing a volume advances the generation, so a writer detached while
// unreachable cannot resume against the generation it remembers.
func TestDetachAdvancesGeneration(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)
	before := world.Volumes["app-data"].Generation

	world, err := Project(world, Evidence{
		Kind: EvidenceVolumeDetached, Target: "app-data",
		Observed: map[string]string{"allocation": "app-0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	volume := world.Volumes["app-data"]
	if volume.Owner != "" {
		t.Fatalf("detach left an owner: %+v", volume)
	}
	if volume.Generation <= before {
		t.Fatalf("detach did not advance the generation: %d -> %d", before, volume.Generation)
	}
	if len(world.Allocations["app-0"].Volumes) != 0 {
		t.Fatalf("detach left the volume attached to the allocation: %+v", world.Allocations["app-0"].Volumes)
	}
}

// A stale detach from a fenced writer must not release a volume the current
// owner is actively using.
func TestStaleDetachDoesNotReleaseVolume(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)

	world, err := Project(world, Evidence{
		Kind: EvidenceVolumeDetached, Target: "app-data",
		Observed: map[string]string{"allocation": "app-9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if world.Volumes["app-data"].Owner != "app-0" {
		t.Fatalf("a stale detach released a live volume: %+v", world.Volumes["app-data"])
	}
}

// Durable data does not move. A workload whose volume exists is pinned to that
// node, whatever placement would otherwise prefer.
func TestPlacementPinsToVolumeNode(t *testing.T) {
	scenario := statefulScenario(t)
	world := cloneWorld(scenario.World)
	world.normalize()
	// The volume lives on a node placement would not otherwise choose.
	world.Nodes["other"].Labels = map[string]string{"pool": "base"}
	world.Volumes["app-data"] = &Volume{Name: "app-data", Node: "other"}

	proposal, err := (PlacementAgent{}).Propose(scenario.Goal, world)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range proposal.Actions {
		if action.Kind == ActionCreateAllocation && action.Node != "other" {
			t.Fatalf("placement ignored the volume's node: %+v", action)
		}
	}
}

// If the node holding the data is unavailable, the workload waits. Relocating
// would mean starting it without its data, which is worse than an outage.
func TestUnavailableVolumeNodeBlocksPlacement(t *testing.T) {
	scenario := statefulScenario(t)
	world := cloneWorld(scenario.World)
	world.normalize()
	world.Volumes["app-data"] = &Volume{Name: "app-data", Node: "other"}
	world.Nodes["other"].Healthy = false

	_, err := (PlacementAgent{}).Propose(scenario.Goal, world)
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("placement relocated a stateful workload off its data: %v", err)
	}
}

// A missing heartbeat is not evidence that a workload stopped writing. The
// architecture is explicit that this must never trigger relocation.
func TestUnhealthyNodeDoesNotReleaseOwnership(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)
	world.Nodes["base"].Healthy = false

	if world.Volumes["app-data"].Owner != "app-0" {
		t.Fatal("ownership changed without evidence")
	}

	// The allocation is still recorded as running, so placement proposes
	// nothing rather than starting a second copy elsewhere. Silence is the
	// correct response: a missing heartbeat is not evidence the writer stopped.
	proposal, err := (PlacementAgent{}).Propose(scenario.Goal, world)
	if err != nil {
		t.Fatalf("placement errored instead of waiting: %v", err)
	}
	for _, action := range proposal.Actions {
		if action.Kind == ActionCreateAllocation {
			t.Fatalf("placement created a second allocation for unreachable data: %+v", action)
		}
		if action.Kind == ActionAttachVolume {
			t.Fatalf("placement reassigned a volume whose owner is unreachable: %+v", action)
		}
	}

	// Even if the allocation is gone from the node's view, the volume's owner
	// must not be reassigned without an explicit release.
	delete(world.Allocations, "app-0")
	if _, err := (PlacementAgent{}).Propose(scenario.Goal, world); err == nil {
		t.Fatal("placement rebuilt a stateful workload on an unreachable node")
	}
	if world.Volumes["app-data"].Owner != "app-0" {
		t.Fatalf("ownership was released without evidence: %+v", world.Volumes["app-data"])
	}
}

// Deleting an allocation that still holds a volume would orphan the storage.
func TestDeleteRequiresVolumeRelease(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)
	world.Allocations["app-0"].Phase = AllocationStopped

	ref := VolumeRef{Name: "app-data", MountPath: "/var/lib/app"}
	_ = ref
	proposal := Proposal{
		ID: "p1", AgentID: "rollout-agent", GoalID: scenario.Goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "delete", Kind: ActionDeleteAllocation, Target: "app-0",
			Workload: "app", Node: "base",
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(RolloutAgent{}).Descriptor(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "release its volumes") {
		t.Fatalf("kernel allowed deleting an allocation holding a volume: %v", err)
	}
}

// Destroying durable data cannot be undone by reconciliation, so it requires a
// separately authenticated approval rather than an agent's judgement.
func TestStatefulDeleteRequiresApproval(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)
	world.Allocations["app-0"].Phase = AllocationStopped
	world.Allocations["app-0"].Volumes = nil
	world.Volumes["app-data"].Owner = ""

	proposal := Proposal{
		ID: "p1", AgentID: "rollout-agent", GoalID: scenario.Goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "delete", Kind: ActionDeleteAllocation, Target: "app-0",
			Workload: "app", Node: "base",
		}},
	}
	kernel := Kernel{Policy: DefaultPolicy()}
	err := kernel.Authorize((RolloutAgent{}).Descriptor(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "destroy-stateful approval") {
		t.Fatalf("stateful deletion proceeded without approval: %v", err)
	}

	// With the approval recorded, the same plan is authorized.
	world.Approvals["destroy"] = &Approval{
		ID: "destroy", GoalID: scenario.Goal.ID, Scope: "destroy-stateful",
		IssuedBy: "operator:test", Granted: true,
	}
	if err := kernel.Authorize((RolloutAgent{}).Descriptor(), scenario.Goal, world, proposal); err != nil {
		t.Fatalf("approved stateful deletion was refused: %v", err)
	}
}

// Detaching from a running writer would pull storage out from under a live
// process.
func TestDetachRequiresStoppedAllocation(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)

	ref := VolumeRef{Name: "app-data", MountPath: "/var/lib/app"}
	proposal := Proposal{
		ID: "p1", AgentID: "rollout-agent", GoalID: scenario.Goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "detach", Kind: ActionDetachVolume, Target: "app-0",
			Workload: "app", Node: "base", Volume: &ref,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(RolloutAgent{}).Descriptor(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "must stop before releasing") {
		t.Fatalf("kernel released a volume from a running writer: %v", err)
	}
}

// A volume the goal never declared must not be attachable, or an agent could
// mount storage belonging to another workload.
func TestUndeclaredVolumeIsRefused(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)
	world.Allocations["app-0"].Phase = AllocationCreated
	world.Volumes["other-data"] = &Volume{Name: "other-data", Node: "base"}

	ref := VolumeRef{Name: "other-data", MountPath: "/var/lib/other"}
	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: scenario.Goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "attach", Kind: ActionAttachVolume, Target: "app-0",
			Workload: "app", Node: "base", Volume: &ref,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(PlacementAgent{}).Descriptor(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "not declared by the goal") {
		t.Fatalf("kernel attached an undeclared volume: %v", err)
	}
}

// A volume cannot be attached across nodes, because the data is not there.
func TestVolumeCannotAttachAcrossNodes(t *testing.T) {
	scenario := statefulScenario(t)
	world := cloneWorld(scenario.World)
	world.normalize()
	world.Volumes["app-data"] = &Volume{Name: "app-data", Node: "other"}
	world.Allocations["app-0"] = &Allocation{
		ID: "app-0", Workload: "app", Node: "base", Phase: AllocationCreated,
	}

	ref := VolumeRef{Name: "app-data", MountPath: "/var/lib/app"}
	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: scenario.Goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "attach", Kind: ActionAttachVolume, Target: "app-0",
			Workload: "app", Node: "base", Volume: &ref,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(PlacementAgent{}).Descriptor(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "lives on node") {
		t.Fatalf("kernel attached a volume across nodes: %v", err)
	}
}

// Re-creating a volume must never reset ownership, which would unfence a writer
// that has been superseded.
func TestRecreateDoesNotResetOwnership(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)

	world, err := Project(world, Evidence{
		Kind: EvidenceVolumeCreated, Target: "app-data",
		Observed: map[string]string{"node": "base"},
	})
	if err != nil {
		t.Fatal(err)
	}
	volume := world.Volumes["app-data"]
	if volume.Owner != "app-0" || volume.Generation != 1 {
		t.Fatalf("re-creating a volume reset its ownership: %+v", volume)
	}
}

// More than one replica writing one local volume is corruption, not scaling.
func TestStatefulWorkloadMustHaveOneReplica(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Workload.Volumes = []VolumeRef{{Name: "data", MountPath: "/data"}}
	scenario.Goal.Workload.Replicas = 3
	if err := scenario.NormalizeAndValidate(); err == nil ||
		!strings.Contains(err.Error(), "exactly one replica") {
		t.Fatalf("a multi-replica volume workload was accepted: %v", err)
	}
}

// A workload marked stateful without volumes is a declaration error worth
// catching at the boundary.
func TestStatefulWorkloadMustDeclareVolumes(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Workload.Stateful = true
	if err := scenario.NormalizeAndValidate(); err == nil ||
		!strings.Contains(err.Error(), "must declare its volumes") {
		t.Fatalf("a stateful workload with no volumes was accepted: %v", err)
	}
}

func TestVolumeValidationRejectsBadPaths(t *testing.T) {
	cases := map[string]VolumeRef{
		"relative path": {Name: "data", MountPath: "var/lib/app"},
		"traversal":     {Name: "data", MountPath: "/var/../etc"},
		"invalid name":  {Name: "Data Volume", MountPath: "/data"},
	}
	for name, ref := range cases {
		scenario := validScenario()
		scenario.Goal.Workload.Volumes = []VolumeRef{ref}
		if err := scenario.NormalizeAndValidate(); err == nil {
			t.Errorf("%s was accepted: %+v", name, ref)
		}
	}
}

// A stateful workload converges end to end: volume created, attached, and the
// allocation started only once storage is in place.
func TestStatefulWorkloadConverges(t *testing.T) {
	scenario := statefulScenario(t)
	executor := NewMemoryExecutor(scenario.World)
	engine := NewEngine(executor, PlacementAgent{})
	if err := engine.Run(scenario.Goal, 12); err != nil {
		t.Fatalf("stateful workload did not converge: %v", err)
	}

	world := executor.World()
	volume := world.Volumes["app-data"]
	if volume == nil {
		t.Fatal("volume was never created")
	}
	if volume.Owner != "app-0" {
		t.Fatalf("volume is not owned by the allocation: %+v", volume)
	}
	allocation := world.Allocations["app-0"]
	if allocation.Volumes["app-data"] != volume.Generation {
		t.Fatalf("allocation holds a different generation: %+v vs %+v",
			allocation.Volumes, volume.Generation)
	}
	if allocation.Phase != AllocationRunning {
		t.Fatalf("allocation is not running: %+v", allocation)
	}
}

// Restoring overwrites durable data irreversibly. Like destruction, it needs a
// separately authenticated decision rather than an agent's judgement.
func TestRestoreRequiresApproval(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)
	world.Volumes["app-data"].Owner = ""
	world.Volumes["app-data"].Snapshots = map[string]string{"backup-1": "abc123"}
	world.Allocations["app-0"].Volumes = nil

	ref := VolumeRef{Name: "app-data", MountPath: "/var/lib/app"}
	proposal := Proposal{
		ID: "p1", AgentID: "storage-agent", GoalID: scenario.Goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "restore", Kind: ActionRestoreSnapshot, Target: "app-data",
			Workload: "app", Node: "base", Volume: &ref, Snapshot: "backup-1",
		}},
	}
	kernel := Kernel{Policy: DefaultPolicy()}
	descriptor := AgentDescriptor{ID: "storage-agent", Role: "protect and recover data"}

	err := kernel.Authorize(descriptor, scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "restore-volume approval") {
		t.Fatalf("restore proceeded without approval: %v", err)
	}

	world.Approvals["restore"] = &Approval{
		ID: "restore", GoalID: scenario.Goal.ID, Scope: "restore-volume",
		IssuedBy: "operator:test", Granted: true,
	}
	if err := kernel.Authorize(descriptor, scenario.Goal, world, proposal); err != nil {
		t.Fatalf("approved restore was refused: %v", err)
	}
}

// Only a snapshot this cluster took and verified may be restored. An operator
// cannot name arbitrary content and have it written over live data.
func TestRestoreRejectsUnrecordedSnapshot(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)
	world.Volumes["app-data"].Owner = ""
	world.Allocations["app-0"].Volumes = nil
	world.Approvals["restore"] = &Approval{
		ID: "restore", GoalID: scenario.Goal.ID, Scope: "restore-volume",
		IssuedBy: "operator:test", Granted: true,
	}

	ref := VolumeRef{Name: "app-data", MountPath: "/var/lib/app"}
	proposal := Proposal{
		ID: "p1", AgentID: "storage-agent", GoalID: scenario.Goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "restore", Kind: ActionRestoreSnapshot, Target: "app-data",
			Workload: "app", Node: "base", Volume: &ref, Snapshot: "invented",
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "storage-agent"}, scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "never recorded") {
		t.Fatalf("kernel restored a snapshot it never took: %v", err)
	}
}

// A snapshot id recorded twice with different content means one of them is not
// what the operator thinks it is.
func TestConflictingSnapshotChecksumIsRefused(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)

	world, err := Project(world, Evidence{
		Kind: EvidenceVolumeSnapshotted, Target: "app-data",
		Observed: map[string]string{"snapshot": "backup-1", "checksum": "abc123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Project(world, Evidence{
		Kind: EvidenceVolumeSnapshotted, Target: "app-data",
		Observed: map[string]string{"snapshot": "backup-1", "checksum": "different"},
	})
	if err == nil || !strings.Contains(err.Error(), "different checksum") {
		t.Fatalf("conflicting snapshot contents were accepted: %v", err)
	}
}

// A snapshot without a checksum cannot be verified at restore time, which makes
// it a guess rather than a backup.
func TestSnapshotEvidenceRequiresChecksum(t *testing.T) {
	scenario := statefulScenario(t)
	world := statefulWorld(t, scenario.Goal)

	_, err := Project(world, Evidence{
		Kind: EvidenceVolumeSnapshotted, Target: "app-data",
		Observed: map[string]string{"snapshot": "backup-1"},
	})
	if err == nil || !strings.Contains(err.Error(), "id and checksum") {
		t.Fatalf("a snapshot without a checksum was recorded: %v", err)
	}
}

// The storage agent may protect and recover data but may not place or start
// workloads. Backup authority and execution authority stay separate.
func TestStorageAgentCannotStartWorkloads(t *testing.T) {
	grants := DefaultPolicy().Grants["storage-agent"]
	for _, forbidden := range []ActionKind{
		ActionCreateAllocation, ActionStartAllocation, ActionDeleteAllocation,
		ActionAttachVolume, ActionDetachVolume,
	} {
		if grants[forbidden] {
			t.Errorf("storage agent was granted %s", forbidden)
		}
	}
	for _, required := range []ActionKind{ActionSnapshotVolume, ActionRestoreSnapshot} {
		if !grants[required] {
			t.Errorf("storage agent is missing %s", required)
		}
	}
}
