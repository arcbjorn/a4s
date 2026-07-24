package control

import (
	"strings"
	"testing"
)

// runScenario reconciles a goal and returns the recorded events, so explanation
// tests read real history rather than hand-written events. A test that invented
// its own events would prove only that the renderer works.
func runScenario(t *testing.T, mutate func(*Scenario)) ([]Event, *MemoryExecutor) {
	t.Helper()
	scenario := validScenario()
	if mutate != nil {
		mutate(&scenario)
	}
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	executor := NewMemoryExecutor(scenario.World)
	engine := NewEngine(executor, PlacementAgent{}, NetworkAgent{})
	_ = engine.Run(scenario.Goal, 8)
	return engine.Events, executor
}

// The headline capability: from the log alone, reconstruct why an allocation
// exists, who decided it, and what proved it working.
func TestExplainReconstructsCausalChain(t *testing.T) {
	events, _ := runScenario(t, nil)
	explanation := Explain(events, "app-0")

	if !explanation.Found {
		t.Fatal("no history found for an allocation that was created")
	}
	if explanation.Status != StateServing {
		t.Fatalf("expected a serving allocation, got %q", explanation.Status)
	}
	if len(explanation.Goals) != 1 || explanation.Goals[0] != "app-public" {
		t.Fatalf("explanation did not attribute the allocation to its goal: %+v", explanation.Goals)
	}

	// The chain must include the operator's goal, the agent's reasoning, the
	// kernel's authorization, and the independent probe. Losing any of those
	// means the log cannot answer "why does this exist".
	want := map[EventType]bool{
		EventGoalAccepted:        false,
		EventProposalCreated:     false,
		EventProposalApproved:    false,
		EventActionCompleted:     false,
		EventObservationRecorded: false,
	}
	for _, step := range explanation.Steps {
		if _, tracked := want[step.Type]; tracked {
			want[step.Type] = true
		}
	}
	for eventType, present := range want {
		if !present {
			t.Errorf("causal chain is missing %s", eventType)
		}
	}
}

// The agent's reasoning must survive into the explanation. That text is the
// only record of why a plan was chosen, and it is exactly what a reconciliation
// loop without an event log throws away.
func TestExplainPreservesAgentReasoning(t *testing.T) {
	events, _ := runScenario(t, nil)
	explanation := Explain(events, "app-0")

	found := false
	for _, step := range explanation.Steps {
		if step.Type == EventProposalCreated && strings.Contains(step.Detail, "place missing replicas") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the proposing agent's reasoning was lost: %s", explanation)
	}
}

// Explaining a route must reach back through the proposal to the goal, even
// though the route and the allocation are different targets.
func TestExplainCoversRoutes(t *testing.T) {
	events, _ := runScenario(t, nil)
	explanation := Explain(events, "app.example.com")

	if !explanation.Found || explanation.Status != StateServing {
		t.Fatalf("route history was not reconstructed: %+v", explanation)
	}
	approved := false
	for _, step := range explanation.Steps {
		if step.Type == EventProposalApproved {
			approved = true
		}
	}
	if !approved {
		t.Fatalf("route explanation omitted kernel authorization: %s", explanation)
	}
}

// A target with no history must say so plainly rather than inventing a chain.
func TestExplainReportsUnknownTarget(t *testing.T) {
	events, _ := runScenario(t, nil)
	explanation := Explain(events, "never-existed")

	if explanation.Found || len(explanation.Steps) != 0 {
		t.Fatalf("invented history for an unknown target: %+v", explanation)
	}
	if !strings.Contains(explanation.String(), "no recorded history") {
		t.Fatalf("unhelpful message for an unknown target: %s", explanation)
	}
}

// A blocked goal must be explainable too. Diagnosing failure is the case where
// an operator actually needs this.
func TestExplainSurfacesBlockage(t *testing.T) {
	events, _ := runScenario(t, func(scenario *Scenario) {
		// No node satisfies the constraints, so placement cannot proceed.
		scenario.Goal.Constraints.RequiredLabels = map[string]string{"pool": "absent"}
	})
	explanation := Explain(events, "app-0")
	if explanation.Found {
		t.Fatalf("an allocation that was never attempted has history: %+v", explanation)
	}

	// The goal itself still has a recorded blockage.
	blocked := false
	for _, event := range events {
		if event.Type == EventGoalBlocked || event.Type == EventProposalDenied {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("an unsatisfiable goal recorded no blockage")
	}
}

// An action dispatched without a completion is the crash window. It must read
// as pending rather than as serving or absent, because the host may have been
// mutated without the controller learning the outcome.
func TestExplainReportsDispatchWithoutCompletion(t *testing.T) {
	events := []Event{
		{Sequence: 1, Type: EventGoalAccepted, Actor: "operator", GoalID: "web-public"},
		{Sequence: 2, Type: EventProposalCreated, Actor: "placement-agent", GoalID: "web-public", ProposalID: "p1"},
		{Sequence: 3, Type: EventProposalApproved, Actor: "policy-kernel", GoalID: "web-public", ProposalID: "p1"},
		{
			Sequence: 4, Type: EventActionDispatched, Actor: "coordinator", GoalID: "web-public",
			ProposalID: "p1", ActionID: "create-web-0", Target: "web-0", Kind: "create_allocation",
		},
		// The controller died here: no completion was ever recorded.
	}
	explanation := Explain(events, "web-0")
	if explanation.Status != StatePending {
		t.Fatalf("a dispatched-but-uncompleted action should read pending, got %q", explanation.Status)
	}
	if !explanation.Found {
		t.Fatal("dispatch intent was not attributed to its target")
	}
}

// A later failure must override an earlier success, or the explanation would
// report a dead workload as serving.
func TestExplainPrefersLatestOutcome(t *testing.T) {
	events := []Event{
		{
			Sequence: 1, Type: EventObservationRecorded, Actor: "prober", GoalID: "web-public",
			Target: "web-0", Evidence: &Evidence{
				Kind: EvidenceAllocationReady, Target: "web-0",
				Observed: map[string]string{"ready": "true"},
			},
		},
		{
			Sequence: 2, Type: EventActionCompleted, Actor: "node-executor", GoalID: "web-public",
			Target: "web-0", Evidence: &Evidence{
				Kind: EvidenceAllocationStopped, Target: "web-0",
				Observed: map[string]string{"exit_code": "1"},
			},
		},
	}
	if state := Explain(events, "web-0").Status; state != StateRemoved {
		t.Fatalf("a stopped allocation still reads as %q", state)
	}
}
