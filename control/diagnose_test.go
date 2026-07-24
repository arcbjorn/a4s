package control

import (
	"strings"
	"testing"
)

// The safety property that makes a diagnoser the right place for model-backed
// reasoning: it holds no capability grant, so even a wrong or adversarial
// diagnosis cannot mutate anything.
func TestDiagnoserHasNoAuthority(t *testing.T) {
	// A Diagnoser is not an Agent. It cannot be registered with the engine and
	// therefore cannot propose actions.
	var diagnoser any = LogDiagnoser{}
	if _, isAgent := diagnoser.(Agent); isAgent {
		t.Fatal("the diagnoser implements Agent and could propose actions")
	}
	// It also holds no grants, so even if it were somehow given a proposal, the
	// kernel would refuse it.
	if grants := DefaultPolicy().Grants["log-diagnoser"]; len(grants) != 0 {
		t.Fatalf("the diagnoser was granted capabilities: %+v", grants)
	}
}

// A converged goal must be reported as such rather than having failure invented
// for it.
func TestDiagnoseReportsConvergence(t *testing.T) {
	events, executor := runScenario(t, nil)
	diagnosis := LogDiagnoser{}.Diagnose("app-public", events, executor.World())

	if !diagnosis.Converged {
		t.Fatalf("a converged goal was diagnosed as failing: %s", diagnosis)
	}
	if diagnosis.Suggestion != "" {
		t.Fatalf("a converged goal produced a suggestion: %q", diagnosis.Suggestion)
	}
}

// Placement failing on constraints must be explained in operator language, with
// a next step rather than only a restatement of the error.
func TestDiagnoseExplainsUnsatisfiableConstraints(t *testing.T) {
	events, executor := runScenario(t, func(scenario *Scenario) {
		scenario.Goal.Constraints.RequiredLabels = map[string]string{"pool": "absent"}
	})
	diagnosis := LogDiagnoser{}.Diagnose("app-public", events, executor.World())

	if diagnosis.Converged {
		t.Fatal("an unsatisfiable goal was diagnosed as converged")
	}
	if len(diagnosis.Findings) == 0 {
		t.Fatal("no findings for an unsatisfiable goal")
	}
	rendered := diagnosis.String()
	if !strings.Contains(rendered, "no healthy node") {
		t.Fatalf("diagnosis did not name the cause: %s", rendered)
	}
	if !strings.Contains(diagnosis.Suggestion, "placement constraints") {
		t.Fatalf("diagnosis gave no actionable next step: %q", diagnosis.Suggestion)
	}
}

// Insufficient capacity should suggest the concrete shortfall, not a generic
// message, because the numbers are what an operator needs.
func TestDiagnoseReportsCapacityShortfall(t *testing.T) {
	world := World{Nodes: map[string]*Node{
		"base": {
			ID: "base", Healthy: true, Labels: map[string]string{"pool": "base"},
			Capacity: Resources{CPUMillis: 4000, MemoryMB: 8192},
			Used:     Resources{CPUMillis: 3950, MemoryMB: 8100},
		},
	}}
	world.normalize()
	events := []Event{
		{Sequence: 1, Type: EventGoalAccepted, GoalID: "app-public", Actor: "operator"},
		{
			Sequence: 2, Type: EventGoalBlocked, GoalID: "app-public", Actor: "policy-kernel",
			Message: `action "create-app-0": node "base" lacks capacity`,
		},
	}
	diagnosis := LogDiagnoser{}.Diagnose("app-public", events, world)
	if !strings.Contains(diagnosis.Suggestion, "free capacity on base") {
		t.Fatalf("capacity diagnosis lacked concrete numbers: %q", diagnosis.Suggestion)
	}
}

// The crash window is the most important thing a diagnosis can surface: the
// recorded world may disagree with the host.
func TestDiagnoseSurfacesDispatchWithoutCompletion(t *testing.T) {
	events := []Event{
		{Sequence: 1, Type: EventGoalAccepted, GoalID: "web-public", Actor: "operator"},
		{
			Sequence: 2, Type: EventActionDispatched, GoalID: "web-public", Actor: "coordinator",
			ProposalID: "p1", ActionID: "create-web-0", Target: "web-0", Kind: "create_allocation",
		},
		// The controller crashed before recording a completion.
	}
	diagnosis := LogDiagnoser{}.Diagnose("web-public", events, World{})

	first := diagnosis.Findings[0]
	if !strings.Contains(first.Cause, "without recorded completion") {
		t.Fatalf("the crash window was not reported first: %+v", diagnosis.Findings)
	}
	if len(first.Targets) != 1 || first.Targets[0] != "web-0" {
		t.Fatalf("crash window did not name the affected target: %+v", first.Targets)
	}
	if !strings.Contains(diagnosis.Suggestion, "node ledger") {
		t.Fatalf("no recovery step suggested for the crash window: %q", diagnosis.Suggestion)
	}
}

// A failed rollout must be diagnosed with the known-good digest as the way out.
func TestDiagnoseRecommendsRollback(t *testing.T) {
	world := World{Allocations: map[string]*Allocation{
		"app-0": {
			ID: "app-0", Workload: "app", Image: nextImage,
			Phase: AllocationStopped, ExitCode: 137, Restarts: 3,
		},
	}}
	world.normalize()
	events := []Event{
		{Sequence: 1, Type: EventGoalAccepted, GoalID: "app-public", Actor: "operator"},
		{
			Sequence: 2, Type: EventGoalBlocked, GoalID: "app-public", Actor: "rollout-agent",
			Message: `workload "app" failed on ` + nextImage + `; last known-good image is ` + testImage,
		},
	}
	diagnosis := LogDiagnoser{}.Diagnose("app-public", events, world)

	if !strings.Contains(diagnosis.Suggestion, "known-good digest") {
		t.Fatalf("no rollback suggested for a failed rollout: %q", diagnosis.Suggestion)
	}
	crashed := false
	for _, finding := range diagnosis.Findings {
		if strings.Contains(finding.Cause, "exited abnormally") {
			crashed = true
			if len(finding.Targets) != 1 || !strings.Contains(finding.Targets[0], "exit 137") {
				t.Fatalf("crash finding lacked the exit code: %+v", finding.Targets)
			}
		}
	}
	if !crashed {
		t.Fatalf("the crashed allocation was not reported: %+v", diagnosis.Findings)
	}
}

// A lease conflict is transient, and the diagnosis should say so rather than
// sending an operator to change configuration.
func TestDiagnoseRecognizesLeaseContention(t *testing.T) {
	events := []Event{
		{Sequence: 1, Type: EventGoalAccepted, GoalID: "app-public", Actor: "operator"},
		{
			Sequence: 2, Type: EventGoalBlocked, GoalID: "app-public", Actor: "policy-kernel",
			Message: `target "app-0" is leased by proposal "other-r0" until 2026-07-23T12:00:00Z`,
		},
	}
	diagnosis := LogDiagnoser{}.Diagnose("app-public", events, World{})
	if !strings.Contains(diagnosis.Suggestion, "expire") {
		t.Fatalf("lease contention was not recognized as transient: %q", diagnosis.Suggestion)
	}
}

// Diagnosis must only consider the goal it was asked about, or an unrelated
// failure would be reported as this goal's cause.
func TestDiagnoseIgnoresOtherGoals(t *testing.T) {
	events := []Event{
		{Sequence: 1, Type: EventGoalAccepted, GoalID: "app-public", Actor: "operator"},
		{Sequence: 2, Type: EventGoalAchieved, GoalID: "app-public", Actor: "verifier"},
		{
			Sequence: 3, Type: EventGoalBlocked, GoalID: "other-goal", Actor: "policy-kernel",
			Message: "unrelated failure",
		},
	}
	diagnosis := LogDiagnoser{}.Diagnose("app-public", events, World{})
	if !diagnosis.Converged {
		t.Fatalf("an unrelated goal's failure was attributed here: %s", diagnosis)
	}
}

// Re-accepting a goal starts a fresh attempt, so an earlier failure must not be
// reported as the current state.
func TestDiagnoseResetsOnGoalReacceptance(t *testing.T) {
	events := []Event{
		{Sequence: 1, Type: EventGoalAccepted, GoalID: "app-public", Actor: "operator"},
		{Sequence: 2, Type: EventGoalBlocked, GoalID: "app-public", Actor: "policy-kernel", Message: "old failure"},
		{Sequence: 3, Type: EventGoalAccepted, GoalID: "app-public", Actor: "operator"},
		{Sequence: 4, Type: EventGoalAchieved, GoalID: "app-public", Actor: "verifier"},
	}
	diagnosis := LogDiagnoser{}.Diagnose("app-public", events, World{})
	if !diagnosis.Converged {
		t.Fatalf("a stale failure survived goal re-acceptance: %s", diagnosis)
	}
}

// A goal with no recorded history must say so rather than fabricating a cause.
func TestDiagnoseReportsAbsentHistory(t *testing.T) {
	diagnosis := LogDiagnoser{}.Diagnose("never-run", nil, World{})
	if len(diagnosis.Findings) != 1 || diagnosis.Findings[0].Cause != "no recorded cause" {
		t.Fatalf("fabricated a cause for a goal with no history: %+v", diagnosis.Findings)
	}
}
