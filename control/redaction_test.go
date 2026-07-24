package control

import (
	"encoding/json"
	"strings"
	"testing"
)

// leakedValue is the material a correct implementation must never let escape.
// It is distinctive so a substring scan cannot match it accidentally.
const leakedValue = "SECRET-VALUE-a3f9c17e-MUST-NOT-APPEAR"

func secretScenario(t *testing.T) Scenario {
	t.Helper()
	scenario := validScenario()
	scenario.Goal.Route = nil
	scenario.Goal.Workload.Secrets = []SecretRef{
		{Name: "db-password", Version: "v3", MountPath: "/run/secrets/db"},
	}
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	return scenario
}

// The central guarantee, tested the only way that is meaningful: run a real
// reconciliation, serialize every artifact the system produces, and prove the
// value appears in none of them.
//
// Reviewing code for leaks catches today's paths. This catches tomorrow's,
// because any new field that carries material through these structures fails.
func TestSecretValueNeverReachesAnyArtifact(t *testing.T) {
	scenario := secretScenario(t)
	executor := NewMemoryExecutor(scenario.World)
	engine := NewEngine(executor, PlacementAgent{})
	_ = engine.Run(scenario.Goal, 10)

	artifacts := map[string]any{
		"goal":     scenario.Goal,
		"world":    executor.World(),
		"events":   engine.Events,
		"plan":     DryRun(Kernel{Policy: DefaultPolicy()}, scenario.World, scenario.Goal, PlacementAgent{}),
		"explain":  Explain(engine.Events, "app-0"),
		"diagnose": LogDiagnoser{}.Diagnose(scenario.Goal.ID, engine.Events, executor.World()),
	}
	for name, artifact := range artifacts {
		encoded, err := json.Marshal(artifact)
		if err != nil {
			t.Fatalf("could not serialize %s: %v", name, err)
		}
		if strings.Contains(string(encoded), leakedValue) {
			t.Fatalf("secret material leaked into %s: %s", name, encoded)
		}
	}
}

// A goal carries only a reference. The struct has no field for a value, so this
// pins that property against a future field being added.
func TestGoalCarriesOnlyReferences(t *testing.T) {
	scenario := secretScenario(t)
	encoded, err := json.Marshal(scenario.Goal)
	if err != nil {
		t.Fatal(err)
	}
	document := string(encoded)
	// The reference must survive.
	for _, want := range []string{"db-password", "v3", "/run/secrets/db"} {
		if !strings.Contains(document, want) {
			t.Fatalf("goal lost part of the secret reference %q: %s", want, document)
		}
	}
	// Nothing resembling material may appear.
	for _, forbidden := range []string{"value", "material", "password\":\"", "content"} {
		if strings.Contains(document, forbidden) {
			t.Fatalf("goal contains a value-bearing field %q: %s", forbidden, document)
		}
	}
}

// A proposal is logged in full and handed to agents. It must carry references
// only, so a proposal remains safe to record and to show a model.
func TestProposalCarriesOnlyReferences(t *testing.T) {
	scenario := secretScenario(t)
	proposal, err := (PlacementAgent{}).Propose(scenario.Goal, scenario.World)
	if err != nil {
		t.Fatal(err)
	}

	mounts := 0
	for _, action := range proposal.Actions {
		if action.Kind != ActionMountSecret {
			continue
		}
		mounts++
		if action.Secret == nil {
			t.Fatal("a mount action carries no reference")
		}
		if action.Secret.Name != "db-password" || action.Secret.Version != "v3" {
			t.Fatalf("mount action lost the reference: %+v", action.Secret)
		}
	}
	if mounts != 1 {
		t.Fatalf("expected one mount action, got %d", mounts)
	}

	encoded, _ := json.Marshal(proposal)
	if strings.Contains(string(encoded), leakedValue) {
		t.Fatalf("proposal leaked material: %s", encoded)
	}
}

// The world projection is serialized into the durable log, so anything stored
// there is stored permanently. Only versions may appear.
func TestWorldRecordsVersionsNotMaterial(t *testing.T) {
	world := projectionWorld()
	world, err := Project(world, createdEvidence())
	if err != nil {
		t.Fatal(err)
	}
	world, err = Project(world, Evidence{
		Kind: EvidenceSecretMounted, Target: "app-0",
		Observed: map[string]string{
			"secret": "db-password", "version": "v3", "mount_path": "/run/secrets/db",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	allocation := world.Allocations["app-0"]
	if allocation.Secrets["db-password"] != "v3" {
		t.Fatalf("world did not record the secret version: %+v", allocation.Secrets)
	}
	encoded, _ := json.Marshal(world)
	if strings.Contains(string(encoded), leakedValue) {
		t.Fatalf("world leaked material: %s", encoded)
	}
}

// Mount evidence without a version is incoherent: the whole point is recording
// which revision is in use so rotation is auditable.
func TestMountEvidenceRequiresNameAndVersion(t *testing.T) {
	world := projectionWorld()
	world, err := Project(world, createdEvidence())
	if err != nil {
		t.Fatal(err)
	}
	for _, observed := range []map[string]string{
		{"secret": "db-password"},
		{"version": "v3"},
	} {
		_, err := Project(world, Evidence{
			Kind: EvidenceSecretMounted, Target: "app-0", Observed: observed,
		})
		if err == nil || !strings.Contains(err.Error(), "name and version") {
			t.Fatalf("incomplete mount evidence was accepted: %+v", observed)
		}
	}
}

// A workload must not start before its declared secrets are mounted, or it
// comes up without credentials and fails in a way that looks like an
// application bug rather than a missing mount.
func TestKernelRefusesStartWithoutMountedSecret(t *testing.T) {
	scenario := secretScenario(t)
	world := cloneWorld(scenario.World)
	world.Allocations["app-0"] = &Allocation{
		ID: "app-0", Workload: "app", Node: "base", Image: testImage,
		Resources: scenario.Goal.Workload.Resources, Phase: AllocationCreated,
		Address: "10.42.0.2",
	}

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
	if err == nil || !strings.Contains(err.Error(), "missing secret") {
		t.Fatalf("workload started without its declared secret: %v", err)
	}
}

// An agent must not mount material the operator never declared for this
// workload, which would be a path to credentials it was not granted.
func TestKernelRefusesUndeclaredSecret(t *testing.T) {
	scenario := secretScenario(t)
	world := cloneWorld(scenario.World)
	world.Allocations["app-0"] = &Allocation{
		ID: "app-0", Workload: "app", Node: "base", Image: testImage,
		Resources: scenario.Goal.Workload.Resources, Phase: AllocationCreated,
	}

	stolen := SecretRef{Name: "root-token", Version: "v1", MountPath: "/run/secrets/root"}
	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: scenario.Goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "mount-root", Kind: ActionMountSecret, Target: "app-0",
			Workload: "app", Node: "base", Secret: &stolen,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(PlacementAgent{}).Descriptor(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "not declared by the goal") {
		t.Fatalf("kernel allowed an undeclared secret: %v", err)
	}
}

// Mounting a different version than the goal declares is also undeclared. A
// version mismatch would silently pin a workload to stale credentials.
func TestKernelRefusesWrongSecretVersion(t *testing.T) {
	scenario := secretScenario(t)
	world := cloneWorld(scenario.World)
	world.Allocations["app-0"] = &Allocation{
		ID: "app-0", Workload: "app", Node: "base", Image: testImage,
		Resources: scenario.Goal.Workload.Resources, Phase: AllocationCreated,
	}

	old := SecretRef{Name: "db-password", Version: "v1", MountPath: "/run/secrets/db"}
	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: scenario.Goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "mount-old", Kind: ActionMountSecret, Target: "app-0",
			Workload: "app", Node: "base", Secret: &old,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(PlacementAgent{}).Descriptor(), scenario.Goal, world, proposal)
	if err == nil {
		t.Fatal("kernel allowed a version the goal did not declare")
	}
}

// Validation must reject secret fields shaped to smuggle material, since names,
// versions, and paths are all recorded in the durable log.
func TestValidationRejectsValueBearingSecretFields(t *testing.T) {
	cases := map[string]SecretRef{
		"name carrying material":  {Name: leakedValue, Version: "v1", MountPath: "/run/secrets/db"},
		"oversized version":       {Name: "db", Version: strings.Repeat("k", 200), MountPath: "/run/secrets/db"},
		"version with whitespace": {Name: "db", Version: "v1 extra", MountPath: "/run/secrets/db"},
		"relative mount path":     {Name: "db", Version: "v1", MountPath: "run/secrets/db"},
		"traversing mount path":   {Name: "db", Version: "v1", MountPath: "/run/../etc/shadow"},
		"empty version":           {Name: "db", Version: "", MountPath: "/run/secrets/db"},
	}
	for name, ref := range cases {
		scenario := validScenario()
		scenario.Goal.Workload.Secrets = []SecretRef{ref}
		if err := scenario.NormalizeAndValidate(); err == nil {
			t.Errorf("%s was accepted: %+v", name, ref)
		}
	}
}

// Two secrets sharing a mount path would silently overwrite each other.
func TestValidationRejectsConflictingSecrets(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Workload.Secrets = []SecretRef{
		{Name: "first", Version: "v1", MountPath: "/run/secrets/shared"},
		{Name: "second", Version: "v1", MountPath: "/run/secrets/shared"},
	}
	if err := scenario.NormalizeAndValidate(); err == nil ||
		!strings.Contains(err.Error(), "share mount path") {
		t.Fatalf("conflicting mount paths were accepted: %v", err)
	}

	scenario.Goal.Workload.Secrets = []SecretRef{
		{Name: "same", Version: "v1", MountPath: "/run/secrets/a"},
		{Name: "same", Version: "v2", MountPath: "/run/secrets/b"},
	}
	if err := scenario.NormalizeAndValidate(); err == nil ||
		!strings.Contains(err.Error(), "referenced twice") {
		t.Fatalf("a duplicated secret name was accepted: %v", err)
	}
}

// Rotation is an ordinary goal change: bumping the version makes the world
// disagree, which is what drives a re-mount.
func TestVersionChangeIsVisibleAsDrift(t *testing.T) {
	scenario := secretScenario(t)
	world := cloneWorld(scenario.World)
	world.Allocations["app-0"] = &Allocation{
		ID: "app-0", Workload: "app", Node: "base", Image: testImage,
		Resources: scenario.Goal.Workload.Resources, Phase: AllocationCreated,
		Address: "10.42.0.2", Secrets: map[string]string{"db-password": "v3"},
	}

	rotated := scenario.Goal
	rotated.Workload.Secrets = []SecretRef{
		{Name: "db-password", Version: "v4", MountPath: "/run/secrets/db"},
	}
	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: rotated.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "start-app-0", Kind: ActionStartAllocation,
			Target: "app-0", Workload: "app", Node: "base",
		}},
		ExpectedEvidence: []Check{{Kind: CheckAllocationReady, Target: "app-0", Want: "true"}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		(PlacementAgent{}).Descriptor(), rotated, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "missing secret") {
		t.Fatalf("a rotated secret did not register as drift: %v", err)
	}
}
