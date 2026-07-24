package control

import (
	"strings"
	"testing"
)

// handoffWorld builds a world with a detached volume and a move approved.
func handoffWorld(t *testing.T, goal Goal) World {
	t.Helper()
	world := cloneWorld(validScenario().World)
	world.normalize()
	world.Nodes["other"].Labels = map[string]string{"pool": "base"}
	world.Volumes["app-data"] = &Volume{Name: "app-data", Node: "base"}
	world.Approvals["move"] = &Approval{
		ID: "move", GoalID: goal.ID, Scope: "move-volume",
		IssuedBy: "operator:test", Granted: true,
	}
	return world
}

func storageAgent() AgentDescriptor {
	return AgentDescriptor{ID: "storage-agent", Role: "protect and recover data"}
}

// handoffProposal builds a single-action proposal from the storage agent.
func handoffProposal(goal Goal, world World, action Action) Proposal {
	action.ID = "step"
	return Proposal{
		ID: "p1", AgentID: "storage-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision, Actions: []Action{action},
	}
}

func volumeRef() *VolumeRef {
	ref := VolumeRef{Name: "app-data", MountPath: "/var/lib/app"}
	return &ref
}

// The prescribed sequence, end to end: quiesce, snapshot, transfer, adopt. Each
// step is entered only on evidence from the last.
func TestHandoffFollowsPrescribedSequence(t *testing.T) {
	scenario := statefulScenario(t)
	world := handoffWorld(t, scenario.Goal)

	world, err := Project(world, Evidence{
		Kind: EvidenceVolumeQuiesced, Target: "app-data",
		Observed: map[string]string{"to": "other"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if world.Volumes["app-data"].Handoff.Phase != HandoffQuiesced {
		t.Fatalf("quiesce did not open a handoff: %+v", world.Volumes["app-data"].Handoff)
	}

	world, err = Project(world, Evidence{
		Kind: EvidenceVolumeSnapshotted, Target: "app-data",
		Observed: map[string]string{"snapshot": "move-1", "checksum": "abc123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	handoff := world.Volumes["app-data"].Handoff
	if handoff.Phase != HandoffSnapshotted || handoff.Checksum != "abc123" {
		t.Fatalf("snapshot did not advance the handoff: %+v", handoff)
	}

	world, err = Project(world, Evidence{
		Kind: EvidenceVolumeTransferred, Target: "app-data",
		Observed: map[string]string{"node": "other", "checksum": "abc123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if world.Volumes["app-data"].Handoff.Phase != HandoffTransferred {
		t.Fatalf("transfer did not advance the handoff: %+v", world.Volumes["app-data"].Handoff)
	}
	// The origin is still authoritative until adoption.
	if world.Volumes["app-data"].Node != "base" {
		t.Fatalf("the volume moved before adoption: %+v", world.Volumes["app-data"])
	}

	world, err = Project(world, Evidence{
		Kind: EvidenceVolumeAdopted, Target: "app-data",
		Observed: map[string]string{"node": "other"},
	})
	if err != nil {
		t.Fatal(err)
	}
	volume := world.Volumes["app-data"]
	if volume.Node != "other" || volume.Handoff != nil {
		t.Fatalf("adoption did not complete the move: %+v", volume)
	}
	// The generation advances so any writer holding the old node's view is
	// fenced.
	if volume.Generation == 0 {
		t.Fatal("adoption did not fence the previous generation")
	}
}

// No step may be skipped. Each of these jumps ahead of the evidence that would
// justify it.
func TestHandoffStepsCannotBeSkipped(t *testing.T) {
	scenario := statefulScenario(t)

	t.Run("transfer before snapshot", func(t *testing.T) {
		world := handoffWorld(t, scenario.Goal)
		world, err := Project(world, Evidence{
			Kind: EvidenceVolumeQuiesced, Target: "app-data",
			Observed: map[string]string{"to": "other"},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = Project(world, Evidence{
			Kind: EvidenceVolumeTransferred, Target: "app-data",
			Observed: map[string]string{"node": "other", "checksum": "abc123"},
		})
		if err == nil || !strings.Contains(err.Error(), "must be snapshotted before transfer") {
			t.Fatalf("transfer proceeded without a snapshot: %v", err)
		}
	})

	t.Run("adopt before transfer", func(t *testing.T) {
		world := handoffWorld(t, scenario.Goal)
		world, err := Project(world, Evidence{
			Kind: EvidenceVolumeQuiesced, Target: "app-data",
			Observed: map[string]string{"to": "other"},
		})
		if err != nil {
			t.Fatal(err)
		}
		world, err = Project(world, Evidence{
			Kind: EvidenceVolumeSnapshotted, Target: "app-data",
			Observed: map[string]string{"snapshot": "move-1", "checksum": "abc123"},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = Project(world, Evidence{
			Kind: EvidenceVolumeAdopted, Target: "app-data",
			Observed: map[string]string{"node": "other"},
		})
		if err == nil || !strings.Contains(err.Error(), "must be transferred before adoption") {
			t.Fatalf("adoption proceeded without a transfer: %v", err)
		}
	})

	t.Run("adopt with no handoff", func(t *testing.T) {
		world := handoffWorld(t, scenario.Goal)
		_, err := Project(world, Evidence{
			Kind: EvidenceVolumeAdopted, Target: "app-data",
			Observed: map[string]string{"node": "other"},
		})
		if err == nil || !strings.Contains(err.Error(), "no handoff in progress") {
			t.Fatalf("adoption proceeded with no move underway: %v", err)
		}
	})
}

// A target that cannot reproduce the snapshot's checksum does not hold the data
// it claims to, so ownership must not move to it.
func TestTransferRequiresMatchingChecksum(t *testing.T) {
	scenario := statefulScenario(t)
	world := handoffWorld(t, scenario.Goal)

	world, err := Project(world, Evidence{
		Kind: EvidenceVolumeQuiesced, Target: "app-data",
		Observed: map[string]string{"to": "other"},
	})
	if err != nil {
		t.Fatal(err)
	}
	world, err = Project(world, Evidence{
		Kind: EvidenceVolumeSnapshotted, Target: "app-data",
		Observed: map[string]string{"snapshot": "move-1", "checksum": "abc123"},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = Project(world, Evidence{
		Kind: EvidenceVolumeTransferred, Target: "app-data",
		Observed: map[string]string{"node": "other", "checksum": "something-else"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not match the snapshot checksum") {
		t.Fatalf("a transfer with the wrong content was accepted: %v", err)
	}
}

// A move must not begin while a writer still holds the volume, or the snapshot
// would capture data that is still changing.
func TestQuiesceRequiresDetachedVolume(t *testing.T) {
	scenario := statefulScenario(t)
	world := handoffWorld(t, scenario.Goal)
	world.Volumes["app-data"].Owner = "app-0"

	proposal := handoffProposal(scenario.Goal, world, Action{
		Kind: ActionQuiesceVolume, Target: "app-data",
		Workload: "app", Node: "other", Volume: volumeRef(),
	})
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		storageAgent(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "must be detached before a move") {
		t.Fatalf("a move began under a live writer: %v", err)
	}
}

// Moving data is irreversible in practice, so it needs a separately
// authenticated decision.
func TestMoveRequiresApproval(t *testing.T) {
	scenario := statefulScenario(t)
	world := handoffWorld(t, scenario.Goal)
	delete(world.Approvals, "move")

	proposal := handoffProposal(scenario.Goal, world, Action{
		Kind: ActionQuiesceVolume, Target: "app-data",
		Workload: "app", Node: "other", Volume: volumeRef(),
	})
	kernel := Kernel{Policy: DefaultPolicy()}
	err := kernel.Authorize(storageAgent(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "move-volume approval") {
		t.Fatalf("a move proceeded without approval: %v", err)
	}

	world.Approvals["move"] = &Approval{
		ID: "move", GoalID: scenario.Goal.ID, Scope: "move-volume",
		IssuedBy: "operator:test", Granted: true,
	}
	if err := kernel.Authorize(storageAgent(), scenario.Goal, world, proposal); err != nil {
		t.Fatalf("an approved move was refused: %v", err)
	}
}

// The volume must never be writable during the move. Attaching it on either
// node would let a writer diverge from what is being transferred.
func TestVolumeCannotBeAttachedDuringHandoff(t *testing.T) {
	scenario := statefulScenario(t)
	world := handoffWorld(t, scenario.Goal)
	world.Volumes["app-data"].Handoff = &VolumeHandoff{
		From: "base", To: "other", Phase: HandoffTransferred,
	}
	world.Allocations["app-0"] = &Allocation{
		ID: "app-0", Workload: "app", Node: "base", Image: testImage,
		Resources: scenario.Goal.Workload.Resources, Phase: AllocationCreated,
	}

	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: scenario.Goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "attach", Kind: ActionAttachVolume, Target: "app-0",
			Workload: "app", Node: "base", Volume: volumeRef(),
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(PlacementAgent{}).Descriptor(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "is being moved") {
		t.Fatalf("a volume was attached mid-move: %v", err)
	}
}

// A workload must not be placed while its data is in flight.
func TestPlacementWaitsForHandoff(t *testing.T) {
	scenario := statefulScenario(t)
	world := handoffWorld(t, scenario.Goal)
	world.Volumes["app-data"].Handoff = &VolumeHandoff{
		From: "base", To: "other", Phase: HandoffSnapshotted,
	}

	_, err := (PlacementAgent{}).Propose(scenario.Goal, world)
	if err == nil || !strings.Contains(err.Error(), "is being moved") {
		t.Fatalf("placement proceeded during a move: %v", err)
	}
}

// A failed transfer leaves the origin authoritative, so the data is still
// reachable where it always was.
func TestFailedTransferLeavesOriginAuthoritative(t *testing.T) {
	scenario := statefulScenario(t)
	world := handoffWorld(t, scenario.Goal)

	world, err := Project(world, Evidence{
		Kind: EvidenceVolumeQuiesced, Target: "app-data",
		Observed: map[string]string{"to": "other"},
	})
	if err != nil {
		t.Fatal(err)
	}
	world, err = Project(world, Evidence{
		Kind: EvidenceVolumeSnapshotted, Target: "app-data",
		Observed: map[string]string{"snapshot": "move-1", "checksum": "abc123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The transfer fails: no transferred evidence ever arrives.
	volume := world.Volumes["app-data"]
	if volume.Node != "base" {
		t.Fatalf("the volume moved without a completed transfer: %+v", volume)
	}
	if volume.Handoff.Phase != HandoffSnapshotted {
		t.Fatalf("the handoff advanced past its evidence: %+v", volume.Handoff)
	}
	// The snapshot taken for the move is still a usable local backup.
	if volume.Snapshots["move-1"] != "abc123" {
		t.Fatalf("the move's snapshot was lost: %+v", volume.Snapshots)
	}
}

// Adoption by a node other than the handoff target would point the cluster at
// data nobody verified.
func TestAdoptionMustMatchHandoffTarget(t *testing.T) {
	scenario := statefulScenario(t)
	world := handoffWorld(t, scenario.Goal)
	world.Volumes["app-data"].Handoff = &VolumeHandoff{
		From: "base", To: "other", Phase: HandoffTransferred, Checksum: "abc123",
	}

	_, err := Project(world, Evidence{
		Kind: EvidenceVolumeAdopted, Target: "app-data",
		Observed: map[string]string{"node": "somewhere-else"},
	})
	if err == nil || !strings.Contains(err.Error(), "not the handoff target") {
		t.Fatalf("a third node adopted the volume: %v", err)
	}
}

// A move to the node that already holds the data is a mistake worth catching.
func TestMoveToSameNodeIsRefused(t *testing.T) {
	scenario := statefulScenario(t)
	world := handoffWorld(t, scenario.Goal)

	proposal := handoffProposal(scenario.Goal, world, Action{
		Kind: ActionQuiesceVolume, Target: "app-data",
		Workload: "app", Node: "base", Volume: volumeRef(),
	})
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		storageAgent(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "already lives on node") {
		t.Fatalf("a move to the same node was allowed: %v", err)
	}
}

// A move to an unhealthy node would strand the data somewhere unreachable.
func TestMoveToUnhealthyNodeIsRefused(t *testing.T) {
	scenario := statefulScenario(t)
	world := handoffWorld(t, scenario.Goal)
	world.Nodes["other"].Healthy = false

	proposal := handoffProposal(scenario.Goal, world, Action{
		Kind: ActionQuiesceVolume, Target: "app-data",
		Workload: "app", Node: "other", Volume: volumeRef(),
	})
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		storageAgent(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "missing or unhealthy") {
		t.Fatalf("a move to an unhealthy node was allowed: %v", err)
	}
}

// The storage agent may move data but must not gain the ability to run
// workloads against it.
func TestStorageAgentCannotPlaceWorkloads(t *testing.T) {
	grants := DefaultPolicy().Grants["storage-agent"]
	for _, forbidden := range []ActionKind{
		ActionCreateAllocation, ActionStartAllocation, ActionAttachVolume,
	} {
		if grants[forbidden] {
			t.Errorf("storage agent was granted %s", forbidden)
		}
	}
	for _, required := range []ActionKind{
		ActionQuiesceVolume, ActionTransferVolume, ActionAdoptVolume,
	} {
		if !grants[required] {
			t.Errorf("storage agent is missing %s", required)
		}
	}
}
