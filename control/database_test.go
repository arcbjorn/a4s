package control

import (
	"strings"
	"testing"
)

func databaseGoal() Goal {
	goal := validScenario().Goal
	goal.Route = nil
	goal.Workload.Name = "db"
	goal.Workload.Engine = "postgres"
	goal.Workload.Volumes = []VolumeRef{{Name: "db-data", MountPath: "/var/lib/postgresql/data"}}
	return goal
}

func databaseWorld(t *testing.T, goal Goal) World {
	t.Helper()
	world := cloneWorld(validScenario().World)
	world.normalize()
	world.Volumes["db-data"] = &Volume{Name: "db-data", Node: "base", Owner: "db-0", Generation: 1}
	world.Allocations["db-0"] = &Allocation{
		ID: "db-0", Workload: "db", Node: "base", Image: testImage,
		Resources: goal.Workload.Resources, Phase: AllocationRunning,
		Stateful: true, Volumes: map[string]uint64{"db-data": 1},
	}
	return world
}

// A running database's files are inconsistent when copied, so a raw filesystem
// snapshot must be refused in favour of the engine's own consistent backup.
func TestRawSnapshotOfDatabaseIsRefused(t *testing.T) {
	goal := databaseGoal()
	world := databaseWorld(t, goal)
	// Detach so the only remaining objection is that it is a database.
	world.Volumes["db-data"].Owner = ""
	world.Allocations["db-0"].Volumes = nil

	ref := VolumeRef{Name: "db-data", MountPath: "/var/lib/postgresql/data"}
	proposal := Proposal{
		ID: "p1", AgentID: "storage-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "snap", Kind: ActionSnapshotVolume, Target: "db-data",
			Node: "base", Volume: &ref, Snapshot: "s1",
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "storage-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "use database_backup") {
		t.Fatalf("a raw snapshot of a database was allowed: %v", err)
	}
}

// A database backup runs against the live, attached database, unlike a
// filesystem snapshot which requires a detached volume.
func TestDatabaseBackupRequiresAttachedDatabase(t *testing.T) {
	goal := databaseGoal()
	world := databaseWorld(t, goal)
	// Detached: the database is not running, so there is nothing to back up.
	world.Volumes["db-data"].Owner = ""

	ref := VolumeRef{Name: "db-data"}
	proposal := Proposal{
		ID: "p1", AgentID: "storage-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "backup", Kind: ActionDatabaseBackup, Target: "db-data",
			Workload: "db", Node: "base", Volume: &ref, Snapshot: "base-1",
			Engine: "postgres",
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "storage-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "must be attached to back it up") {
		t.Fatalf("a database backup ran against a stopped database: %v", err)
	}
}

// A database backup on a live database is authorized, exercising the path that
// replaces the raw snapshot.
func TestDatabaseBackupIsAuthorizedWhenAttached(t *testing.T) {
	goal := databaseGoal()
	world := databaseWorld(t, goal)

	ref := VolumeRef{Name: "db-data"}
	proposal := Proposal{
		ID: "p1", AgentID: "storage-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "backup", Kind: ActionDatabaseBackup, Target: "db-data",
			Workload: "db", Node: "base", Volume: &ref, Snapshot: "base-1",
			Engine: "postgres",
		}},
	}
	if err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "storage-agent"}, goal, world, proposal); err != nil {
		t.Fatalf("a live database backup was refused: %v", err)
	}
}

// A database backup becomes a first-class snapshot: a recovery point that can be
// verified, pruned, and restored like any other.
func TestDatabaseBackupBecomesRecoveryPoint(t *testing.T) {
	goal := databaseGoal()
	world := databaseWorld(t, goal)

	world, err := Project(world, Evidence{
		Kind: EvidenceDatabaseBackedUp, Target: "db-data",
		Observed: map[string]string{"label": "base-1", "checksum": "abc123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	volume := world.Volumes["db-data"]
	if volume.Snapshots["base-1"] != "abc123" {
		t.Fatalf("database backup did not become a snapshot: %+v", volume.Snapshots)
	}
	if volume.LastSnapshot != "base-1" {
		t.Fatalf("database backup is not the recovery point: %q", volume.LastSnapshot)
	}
	// It can be restored like any other snapshot.
	world.Volumes["db-data"].Owner = ""
	world.Approvals["restore"] = &Approval{
		ID: "restore", GoalID: goal.ID, Scope: "restore-volume",
		IssuedBy: "operator:test", Granted: true,
	}
	restoreRef := VolumeRef{Name: "db-data", MountPath: "/var/lib/postgresql/data"}
	proposal := Proposal{
		ID: "p2", AgentID: "storage-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "restore", Kind: ActionRestoreSnapshot, Target: "db-data",
			Node: "base", Volume: &restoreRef, Snapshot: "base-1",
		}},
	}
	if err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "storage-agent"}, goal, world, proposal); err != nil {
		t.Fatalf("a database backup could not be restored: %v", err)
	}
}

// A database workload gets a database readiness probe, not a bare TCP one, so
// readiness reflects the database accepting queries.
func TestDatabaseWorkloadGetsDatabaseProbe(t *testing.T) {
	goal := databaseGoal()
	goal.Workload.Port = 5432
	scenario := Scenario{Goal: goal, World: validScenario().World}
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}

	executor := NewMemoryExecutor(validScenario().World)
	engine := NewEngine(executor, PlacementAgent{})
	engine.registerProbeTarget(goal, Action{Kind: ActionCreateAllocation, Target: "db-0"})

	target := engine.probeTargets["db-0"]
	if target.Kind != ProbeDatabase {
		t.Fatalf("database workload got probe kind %q, not database", target.Kind)
	}
	if target.Engine != "postgres" {
		t.Fatalf("database probe did not carry the engine: %+v", target)
	}
}

// A database workload must declare a volume; without durable storage it is
// misconfigured.
func TestDatabaseWorkloadMustDeclareVolume(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Workload.Engine = "postgres"
	scenario.Goal.Workload.Volumes = nil
	if err := scenario.NormalizeAndValidate(); err == nil ||
		!strings.Contains(err.Error(), "must declare a volume") {
		t.Fatalf("a database with no volume was accepted: %v", err)
	}
}

// A database workload is single-writer; more than one replica would corrupt the
// shared data directory.
func TestDatabaseWorkloadIsSingleReplica(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Workload.Engine = "postgres"
	scenario.Goal.Workload.Volumes = []VolumeRef{{Name: "db-data", MountPath: "/data"}}
	scenario.Goal.Workload.Replicas = 2
	if err := scenario.NormalizeAndValidate(); err == nil ||
		!strings.Contains(err.Error(), "exactly one replica") {
		t.Fatalf("a multi-replica database was accepted: %v", err)
	}
}

// An unknown engine is refused, since the agents would fall back to copying
// its files, which is exactly what must not happen for a database.
func TestUnknownEngineIsRefused(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Workload.Engine = "mysql"
	scenario.Goal.Workload.Volumes = []VolumeRef{{Name: "db-data", MountPath: "/data"}}
	if err := scenario.NormalizeAndValidate(); err == nil ||
		!strings.Contains(err.Error(), "not supported") {
		t.Fatalf("an unsupported engine was accepted: %v", err)
	}
}

// The storage agent backs up a database with database_backup, never a raw
// snapshot.
func TestStorageAgentBacksUpDatabaseWithEngine(t *testing.T) {
	goal := databaseGoal()
	world := databaseWorld(t, goal)

	proposal, err := (StorageAgent{}).ProposeBackup(goal, world, "base-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Actions) != 1 {
		t.Fatalf("expected one backup action, got %+v", proposal.Actions)
	}
	action := proposal.Actions[0]
	if action.Kind != ActionDatabaseBackup {
		t.Fatalf("storage agent used %q instead of a database backup", action.Kind)
	}
	if action.Engine != "postgres" {
		t.Fatalf("backup action lost the engine: %+v", action)
	}
}
