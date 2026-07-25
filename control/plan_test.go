package control

import (
	"reflect"
	"strings"
	"testing"
)

func planKernel() Kernel { return Kernel{Policy: DefaultPolicy()} }

// The defining property of a dry run: it must change nothing. A plan that
// mutated the world would be worse than no plan at all.
func TestDryRunDoesNotMutateWorld(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	before := cloneWorld(scenario.World)

	plan := DryRun(planKernel(), scenario.World, scenario.Goal, PlacementAgent{}, NetworkAgent{})
	if len(plan.Steps) == 0 {
		t.Fatal("dry run produced no plan for an unsatisfied goal")
	}

	if scenario.World.Revision != before.Revision {
		t.Fatalf("dry run advanced the world revision: %d -> %d", before.Revision, scenario.World.Revision)
	}
	if len(scenario.World.Allocations) != len(before.Allocations) {
		t.Fatalf("dry run created allocations: %+v", scenario.World.Allocations)
	}
	if len(scenario.World.Routes) != len(before.Routes) {
		t.Fatalf("dry run published routes: %+v", scenario.World.Routes)
	}
	for id, node := range scenario.World.Nodes {
		if node.Used != before.Nodes[id].Used {
			t.Fatalf("dry run charged capacity on %s: %+v", id, node.Used)
		}
		if len(node.Images) != len(before.Nodes[id].Images) {
			t.Fatalf("dry run marked images present on %s: %+v", id, node.Images)
		}
	}
}

// The plan must agree with what execution actually does. If the two disagree,
// the plan is worse than useless because an operator would trust it.
//
// Only steps the dry run states unconditionally are compared. A step marked
// assumed depends on readiness that simulation cannot measure, so it is a
// prediction rather than a claim.
func TestPlanMatchesExecution(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	plan := DryRun(planKernel(), scenario.World, scenario.Goal, PlacementAgent{}, NetworkAgent{})

	var planned []string
	for _, step := range plan.Steps {
		if step.Assumed {
			continue
		}
		planned = append(planned, string(step.Kind)+":"+step.Target)
	}

	// Execute for real and collect the first round's dispatched actions, which
	// is what a single dry run predicts.
	executor := NewMemoryExecutor(scenario.World)
	engine := NewEngine(executor, PlacementAgent{}, NetworkAgent{})
	_ = engine.Run(scenario.Goal, 8)

	var executed []string
	firstProposal := ""
	for _, event := range engine.Events {
		if event.Type != EventActionDispatched {
			continue
		}
		if firstProposal == "" {
			firstProposal = event.ProposalID
		}
		if event.ProposalID != firstProposal {
			break
		}
		executed = append(executed, event.Kind+":"+event.Target)
	}

	if !reflect.DeepEqual(planned, executed) {
		t.Fatalf("plan disagreed with execution:\nplanned:  %v\nexecuted: %v", planned, executed)
	}
}

// Simulation optimistically assumes a started allocation becomes ready, because
// the kernel must be able to authorize a whole plan. The dry run must therefore
// mark everything downstream of that assumption as contingent, or it would
// promise a route that depends on a probe result nobody has measured.
func TestDryRunMarksStepsContingentOnReadiness(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	plan := DryRun(planKernel(), scenario.World, scenario.Goal, PlacementAgent{}, NetworkAgent{})

	var route *PlannedStep
	for i, step := range plan.Steps {
		if step.Kind == ActionPublishRoute {
			route = &plan.Steps[i]
		}
		// Nothing up to and including the start is contingent.
		if step.Kind == ActionCreateAllocation || step.Kind == ActionStartAllocation {
			if step.Assumed {
				t.Fatalf("%s was marked contingent but depends on nothing assumed", step.Kind)
			}
		}
	}
	if route == nil {
		t.Fatal("plan omitted the route entirely")
	}
	if !route.Assumed {
		t.Fatal("route was promised unconditionally despite depending on unmeasured readiness")
	}
	if !strings.Contains(plan.String(), "only if readiness is confirmed") {
		t.Fatalf("rendered plan did not disclose the assumption: %s", plan)
	}
}

// A satisfied goal must plan nothing, so an operator can distinguish "nothing
// to do" from "cannot proceed".
func TestDryRunReportsAchievedGoal(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	executor := NewMemoryExecutor(scenario.World)
	engine := NewEngine(executor, PlacementAgent{}, NetworkAgent{})
	if err := engine.Run(scenario.Goal, 8); err != nil {
		t.Fatal(err)
	}

	plan := DryRun(planKernel(), executor.World(), scenario.Goal, PlacementAgent{}, NetworkAgent{})
	if !plan.Achieved || len(plan.Steps) != 0 {
		t.Fatalf("a converged goal still planned work: %+v", plan)
	}
	if !strings.Contains(plan.String(), "already satisfied") {
		t.Fatalf("unclear message for an achieved goal: %s", plan)
	}
}

// The useful failure case: a plan that cannot proceed must name the obstacle
// before anything is mutated.
func TestDryRunSurfacesObstacleBeforeMutating(t *testing.T) {
	scenario := validScenario()
	scenario.Goal.Constraints.RequiredLabels = map[string]string{"pool": "absent"}
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}

	plan := DryRun(planKernel(), scenario.World, scenario.Goal, PlacementAgent{}, NetworkAgent{})
	if len(plan.Steps) != 0 {
		t.Fatalf("planned work despite an unsatisfiable constraint: %+v", plan.Steps)
	}
	if len(plan.Blocked) == 0 {
		t.Fatal("no obstacle reported for an unsatisfiable goal")
	}
	if !strings.Contains(plan.Blocked[0].Reason, "no healthy node") {
		t.Fatalf("unexpected obstacle: %+v", plan.Blocked[0])
	}
	if plan.Blocked[0].Stage != "propose" {
		t.Fatalf("obstacle should be attributed to proposal, got %q", plan.Blocked[0].Stage)
	}
}

// A failed rollout must surface the known-good digest in the plan, so an
// operator sees the rollback target without running anything.
func TestDryRunNamesRollbackTarget(t *testing.T) {
	goal := validScenario().Goal
	goal.Route = nil
	goal.Workload.Image = nextImage

	world := rolloutWorld(goal, 1, nextImage)
	world.KnownGood = map[string]string{"app": testImage}
	world.Allocations["app-0"].Phase = AllocationStopped
	world.Allocations["app-0"].Ready = false
	world.Allocations["app-0"].ExitCode = 1

	plan := DryRun(planKernel(), world, goal, RolloutAgent{}, PlacementAgent{})
	found := false
	for _, obstacle := range plan.Blocked {
		if obstacle.RollbackTarget == testImage {
			found = true
		}
	}
	if !found {
		t.Fatalf("plan did not name the known-good rollback target: %+v", plan.Blocked)
	}
	if !strings.Contains(plan.String(), "last known-good image") {
		t.Fatalf("rendered plan omitted the rollback target: %s", plan)
	}
}

// The kernel must reject an unauthorized plan during dry run exactly as it
// would during execution, so a dry run cannot understate what policy forbids.
func TestDryRunAppliesKernelAuthorization(t *testing.T) {
	scenario := validScenario()
	scenario.World.Approvals = nil
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	executor := NewMemoryExecutor(scenario.World)
	engine := NewEngine(executor, PlacementAgent{})
	_ = engine.Run(scenario.Goal, 8)

	// The workload is ready, so the network agent will propose a public route
	// that has no approval. Publishing service names is authorized and may
	// appear first; what must never appear is the unapproved route.
	plan := DryRun(planKernel(), executor.World(), scenario.Goal, NetworkAgent{})
	for _, step := range plan.Steps {
		if step.Kind == ActionPublishRoute {
			t.Fatalf("dry run authorized an unapproved public route: %+v", step)
		}
	}
	denied := false
	for _, obstacle := range plan.Blocked {
		if obstacle.Stage == "authorize" && strings.Contains(obstacle.Reason, "public-route approval") {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("kernel authorization was not applied during dry run: %+v", plan.Blocked)
	}
}

// Consequences must describe the world the plan produces, so an operator can
// see capacity impact before committing.
func TestDryRunReportsConsequences(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	plan := DryRun(planKernel(), scenario.World, scenario.Goal, PlacementAgent{}, NetworkAgent{})

	if len(plan.Consequences.CreatedAllocs) != 1 || plan.Consequences.CreatedAllocs[0] != "app-0" {
		t.Fatalf("consequences did not report the created allocation: %+v", plan.Consequences)
	}
	usage, reported := plan.Consequences.NodeUsage["base"]
	if !reported || !strings.Contains(usage, "->") {
		t.Fatalf("consequences did not report capacity impact: %+v", plan.Consequences.NodeUsage)
	}
}

// Repeated dry runs against the same world must agree, or the plan is not a
// function of observed state.
func TestDryRunIsRepeatable(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	first := DryRun(planKernel(), scenario.World, scenario.Goal, PlacementAgent{}, NetworkAgent{})
	second := DryRun(planKernel(), scenario.World, scenario.Goal, PlacementAgent{}, NetworkAgent{})
	if !reflect.DeepEqual(first.Steps, second.Steps) {
		t.Fatalf("dry run is not repeatable:\nfirst:  %+v\nsecond: %+v", first.Steps, second.Steps)
	}
}
