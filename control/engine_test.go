package control

import (
	"strings"
	"testing"
)

const testImage = "registry.example/app@sha256:0000000000000000000000000000000000000000000000000000000000000000"

func TestEngineAchievesGoalThroughMultipleAgents(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	executor := NewMemoryExecutor(scenario.World)
	engine := NewEngine(executor, PlacementAgent{}, NetworkAgent{})
	if err := engine.Run(scenario.Goal, 5); err != nil {
		t.Fatal(err)
	}
	world := executor.World()
	if !goalAchieved(scenario.Goal, world) {
		t.Fatalf("goal was not achieved: %+v", world)
	}
	if world.Allocations["app-0"].Node != "base" {
		t.Fatalf("allocation ignored placement: %+v", world.Allocations["app-0"])
	}
	wantTypes := []EventType{
		EventGoalAccepted, EventProposalCreated, EventProposalApproved,
		EventActionCompleted, EventGoalAchieved,
	}
	for _, want := range wantTypes {
		if !containsEvent(engine.Events, want) {
			t.Errorf("missing event %s", want)
		}
	}
}

// Every action requiring evidence must have its own declared check. Assigning
// rather than appending would silently drop all but the last declaration, so
// the kernel would reject plans whose evidence requirements went missing.
func TestPlacementAgentDeclaresEvidencePerAllocation(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Workload.Replicas = 3
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	world := cloneWorld(scenario.World)
	world.Allocations["app-0"] = &Allocation{
		ID: "app-0", Workload: "app", Node: "base", Replica: 0,
		Image: testImage, Resources: scenario.Goal.Workload.Resources,
		Phase: AllocationRunning, Ready: true,
	}
	proposal, err := (PlacementAgent{}).Propose(scenario.Goal, world)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range proposal.Actions {
		if action.Kind != ActionStartAllocation {
			continue
		}
		found := false
		for _, check := range proposal.ExpectedEvidence {
			if check.Kind == CheckAllocationReady && check.Target == action.Target {
				found = true
			}
		}
		if !found {
			t.Fatalf("action %q has no declared readiness evidence: %+v", action.ID, proposal.ExpectedEvidence)
		}
	}
	if err := (Kernel{Policy: DefaultPolicy()}).Authorize((PlacementAgent{}).Descriptor(), scenario.Goal, world, proposal); err != nil {
		t.Fatalf("kernel rejected a plan with complete evidence: %v", err)
	}
}

func TestKernelDeniesPublicRouteWithoutApproval(t *testing.T) {
	scenario := validScenario()
	scenario.World.Approvals = nil
	world := cloneWorld(scenario.World)
	world.Allocations["app-0"] = &Allocation{
		ID: "app-0", Workload: "app", Node: "base", Replica: 0,
		Image: testImage, Resources: scenario.Goal.Workload.Resources,
		Phase: AllocationRunning, Ready: true,
	}
	proposal, err := (NetworkAgent{}).Propose(scenario.Goal, world)
	if err != nil {
		t.Fatal(err)
	}
	err = (Kernel{Policy: DefaultPolicy()}).Authorize((NetworkAgent{}).Descriptor(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "requires public-route approval") {
		t.Fatalf("expected approval denial, got %v", err)
	}
}

func TestKernelRejectsStaleProposal(t *testing.T) {
	scenario := validScenario()
	proposal, err := (PlacementAgent{}).Propose(scenario.Goal, scenario.World)
	if err != nil {
		t.Fatal(err)
	}
	world := cloneWorld(scenario.World)
	world.Revision++
	err = (Kernel{Policy: DefaultPolicy()}).Authorize((PlacementAgent{}).Descriptor(), scenario.Goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "stale proposal") {
		t.Fatalf("expected stale proposal denial, got %v", err)
	}
}

func TestKernelRejectsSelfAssertedAgentIdentity(t *testing.T) {
	scenario := validScenario()
	proposal, err := (PlacementAgent{}).Propose(scenario.Goal, scenario.World)
	if err != nil {
		t.Fatal(err)
	}
	proposal.AgentID = "network-agent"
	err = (Kernel{Policy: DefaultPolicy()}).Authorize((PlacementAgent{}).Descriptor(), scenario.Goal, scenario.World, proposal)
	if err == nil || !strings.Contains(err.Error(), "authenticated actor") {
		t.Fatalf("expected agent identity denial, got %v", err)
	}
}

func TestPlacementAgentRejectsInsufficientCapacity(t *testing.T) {
	scenario := validScenario()
	scenario.World.Nodes["base"].Capacity = Resources{CPUMillis: 50, MemoryMB: 64}
	_, err := (PlacementAgent{}).Propose(scenario.Goal, scenario.World)
	if err == nil || !strings.Contains(err.Error(), "no healthy node") {
		t.Fatalf("expected capacity error, got %v", err)
	}
}

func TestScenarioRejectsPrivilegedWorkload(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Workload.Privileged = true
	err := scenario.NormalizeAndValidate()
	if err == nil || !strings.Contains(err.Error(), "privileged") {
		t.Fatalf("expected privileged workload denial, got %v", err)
	}
}

func validScenario() Scenario {
	return Scenario{
		Goal: Goal{
			APIVersion: APIVersion, ID: "app-public", Objective: "keep app reachable",
			Workload: WorkloadSpec{
				Name: "app", Image: testImage, Replicas: 1, Port: 8080,
				Resources: Resources{CPUMillis: 100, MemoryMB: 128},
			},
			Route:       &RouteSpec{Host: "app.example.com", Port: 443, Exposure: "public"},
			Constraints: Constraints{RequiredLabels: map[string]string{"pool": "base"}},
		},
		World: World{Approvals: map[string]*Approval{
			"approve-public": {
				ID: "approve-public", GoalID: "app-public", Scope: "public-route",
				IssuedBy: "operator:test", Granted: true,
			},
		}, Nodes: map[string]*Node{
			"base": {
				ID: "base", Healthy: true, Labels: map[string]string{"pool": "base"},
				Capacity: Resources{CPUMillis: 4000, MemoryMB: 8192},
			},
			"other": {
				ID: "other", Healthy: true, Labels: map[string]string{"pool": "other"},
				Capacity: Resources{CPUMillis: 8000, MemoryMB: 16384},
			},
		}},
	}
}

func containsEvent(events []Event, want EventType) bool {
	for _, event := range events {
		if event.Type == want {
			return true
		}
	}
	return false
}
