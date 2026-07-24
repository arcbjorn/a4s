package control

import (
	"testing"
	"time"
)

func verifiableGoal() Goal {
	goal := validScenario().Goal
	goal.Route = nil
	goal.Workload.Stateful = true
	goal.Workload.Volumes = []VolumeRef{{Name: "app-data", MountPath: "/var/lib/app"}}
	return goal
}

func verifiableWorld(t *testing.T, verifiedAt time.Time) World {
	t.Helper()
	world := cloneWorld(validScenario().World)
	world.normalize()
	world.ObservedAt = time.Unix(1_000_000, 0).UTC()
	world.Volumes["app-data"] = &Volume{
		Name: "app-data", Node: "base",
		Snapshots:    map[string]string{"s1": "s1-checksum"},
		LastSnapshot: "s1", VerifiedAt: verifiedAt,
	}
	return world
}

// The agent proposes verifying a backup that has never been checked.
func TestStorageAgentVerifiesUncheckedBackup(t *testing.T) {
	goal := verifiableGoal()
	world := verifiableWorld(t, time.Time{}) // never verified

	proposal, err := (StorageAgent{}).Propose(goal, world)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Actions) != 1 || proposal.Actions[0].Kind != ActionVerifyBackup {
		t.Fatalf("expected one verify action, got %+v", proposal.Actions)
	}
	if proposal.Actions[0].Snapshot != "s1" {
		t.Fatalf("verification did not target the last snapshot: %+v", proposal.Actions[0])
	}
	// The action carries the recorded checksum so the node can confirm it.
	if proposal.Actions[0].Volume.Checksum != "s1-checksum" {
		t.Fatalf("verify action lost the checksum: %+v", proposal.Actions[0].Volume)
	}
}

// A backup verified within the interval is not re-checked, so verification does
// not run needlessly.
func TestStorageAgentSkipsFreshlyVerifiedBackup(t *testing.T) {
	goal := verifiableGoal()
	world := verifiableWorld(t, time.Time{})
	// Verified an hour ago, well within the default interval.
	world.Volumes["app-data"].VerifiedAt = world.ObservedAt.Add(-time.Hour)

	proposal, err := (StorageAgent{}).Propose(goal, world)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Actions) != 0 {
		t.Fatalf("a freshly verified backup was re-checked: %+v", proposal.Actions)
	}
}

// A backup whose verification has aged past the interval is re-checked.
func TestStorageAgentReverifiesStaleBackup(t *testing.T) {
	goal := verifiableGoal()
	world := verifiableWorld(t, time.Time{})
	world.Volumes["app-data"].VerifiedAt = world.ObservedAt.Add(-30 * 24 * time.Hour)

	proposal, err := (StorageAgent{Interval: 7 * 24 * time.Hour}).Propose(goal, world)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Actions) != 1 {
		t.Fatalf("a stale backup was not re-verified: %+v", proposal.Actions)
	}
}

// A volume mid-move is not burdened with verification load.
func TestStorageAgentSkipsMovingVolume(t *testing.T) {
	goal := verifiableGoal()
	world := verifiableWorld(t, time.Time{})
	world.Volumes["app-data"].Handoff = &VolumeHandoff{From: "base", To: "other", Phase: HandoffSnapshotted}

	proposal, err := (StorageAgent{}).Propose(goal, world)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Actions) != 0 {
		t.Fatalf("verification was proposed during a move: %+v", proposal.Actions)
	}
}

// A successful verification records when it happened, so an operator can see
// how recently the backup was proven recoverable.
func TestSuccessfulVerificationRecordsTime(t *testing.T) {
	world := verifiableWorld(t, time.Time{})
	observedAt := world.ObservedAt.Add(time.Hour)

	world, err := Project(world, Evidence{
		Kind: EvidenceBackupVerified, Target: "app-data", ObservedAt: observedAt,
		Observed: map[string]string{"snapshot": "s1", "verified": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	volume := world.Volumes["app-data"]
	if !volume.VerifiedAt.Equal(observedAt) {
		t.Fatalf("verification time was not recorded: %v", volume.VerifiedAt)
	}
	if volume.VerifiedSnapshot != "s1" {
		t.Fatalf("verified snapshot was not recorded: %q", volume.VerifiedSnapshot)
	}
}

// A failed verification records nothing as verified. Recording a time on
// failure would tell an operator a broken backup is recoverable.
func TestFailedVerificationRecordsNothing(t *testing.T) {
	world := verifiableWorld(t, time.Time{})

	world, err := Project(world, Evidence{
		Kind: EvidenceBackupVerified, Target: "app-data", ObservedAt: world.ObservedAt,
		Observed: map[string]string{"snapshot": "s1", "verified": "false", "reason": "checksum mismatch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !world.Volumes["app-data"].VerifiedAt.IsZero() {
		t.Fatal("a failed verification recorded a verification time")
	}
}

// The operator-facing report lists backups overdue for verification.
func TestStaleBackupsReport(t *testing.T) {
	world := verifiableWorld(t, time.Time{})
	interval := 7 * 24 * time.Hour

	// Never verified -> stale.
	if stale := StaleBackups(world, interval, world.ObservedAt); len(stale) != 1 || stale[0] != "app-data" {
		t.Fatalf("an unverified backup was not reported stale: %+v", stale)
	}
	// Verified recently -> not stale.
	world.Volumes["app-data"].VerifiedAt = world.ObservedAt.Add(-time.Hour)
	if stale := StaleBackups(world, interval, world.ObservedAt); len(stale) != 0 {
		t.Fatalf("a freshly verified backup was reported stale: %+v", stale)
	}
}

// Verification is read-only and needs no approval, unlike restore or destroy.
func TestVerificationNeedsNoApproval(t *testing.T) {
	goal := verifiableGoal()
	world := verifiableWorld(t, time.Time{})
	// No approvals of any kind.
	world.Approvals = map[string]*Approval{}

	proposal, err := (StorageAgent{}).Propose(goal, world)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Actions) == 0 {
		t.Fatal("expected a verification proposal")
	}
	if err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(StorageAgent{}).Descriptor(), goal, world, proposal); err != nil {
		t.Fatalf("verification required approval it should not: %v", err)
	}
}

// The storage agent still may not run workloads, even with verification added.
func TestStorageAgentStaysReadOnlyForExecution(t *testing.T) {
	descriptor := (StorageAgent{}).Descriptor()
	grants := DefaultPolicy().Grants[descriptor.ID]
	for _, forbidden := range []ActionKind{
		ActionCreateAllocation, ActionStartAllocation, ActionAttachVolume,
	} {
		if grants[forbidden] {
			t.Errorf("storage agent was granted %s", forbidden)
		}
	}
	if !grants[ActionVerifyBackup] {
		t.Error("storage agent cannot verify backups")
	}
}
