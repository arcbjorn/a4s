package control

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// diagnosisFor renders a diagnosis and returns it with its text, since an
// operator reads the text and a test should check what they will see.
func diagnosisFor(t *testing.T, world World, events []Event) (Diagnosis, string) {
	t.Helper()
	diagnosis := LogDiagnoser{}.Diagnose("web-public", events, world)
	return diagnosis, diagnosis.String()
}

func dispatchEvent(target string) Event {
	return Event{
		Sequence: 1, Type: EventActionDispatched, GoalID: "web-public",
		ActionID: "create-" + target, Target: target,
	}
}

func blockedEvent(message string) Event {
	return Event{
		Sequence: 2, Type: EventGoalBlocked, GoalID: "web-public", Message: message,
	}
}

// A goal held back by a safeguard looks exactly like a broken one unless the
// diagnosis says which.
func TestDiagnosisExplainsBackoff(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1"})
	world.ObservedAt = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	world.Backoff = map[string]*Backoff{}
	recordFailure(&world, "web-0", world.ObservedAt)

	diagnosis, text := diagnosisFor(t, world, []Event{
		dispatchEvent("web-0"), blockedEvent("goal is blocked"),
	})
	if diagnosis.Converged {
		t.Fatal("a blocked goal reported converged")
	}
	if !strings.Contains(text, "waiting out a failure backoff") {
		t.Fatalf("the diagnosis did not explain the backoff:\n%s", text)
	}
	if !strings.Contains(text, "1 consecutive failures") {
		t.Fatalf("the diagnosis omitted the failure count:\n%s", text)
	}
}

// A backoff on another goal's target must not be reported here, or every
// diagnosis on a busy cluster would blame unrelated work.
func TestDiagnosisScopesBackoffToTheGoalFootprint(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1"})
	world.ObservedAt = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	world.Backoff = map[string]*Backoff{}
	recordFailure(&world, "unrelated-0", world.ObservedAt)

	_, text := diagnosisFor(t, world, []Event{
		dispatchEvent("web-0"), blockedEvent("goal is blocked"),
	})
	if strings.Contains(text, "unrelated-0") {
		t.Fatalf("a diagnosis reported another goal's backoff:\n%s", text)
	}
}

// When nothing can accept work, that is the whole answer.
func TestDiagnosisReportsNoSchedulableNode(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1", "node-b": "rack-2"})
	world.Nodes["node-a"].Cordoned = true
	world.Nodes["node-a"].CordonReason = "disk failing"
	world.Nodes["node-b"].Healthy = false

	_, text := diagnosisFor(t, world, []Event{blockedEvent("goal is blocked")})
	if !strings.Contains(text, "no node can accept work") {
		t.Fatalf("the diagnosis missed an unschedulable cluster:\n%s", text)
	}
	if !strings.Contains(text, "disk failing") {
		t.Fatalf("the diagnosis omitted the cordon reason:\n%s", text)
	}
}

// A partly cordoned cluster is worth reporting without claiming it is fatal.
func TestDiagnosisReportsCordonedNodesWithoutOverclaiming(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1", "node-b": "rack-2"})
	world.Nodes["node-a"].Cordoned = true

	_, text := diagnosisFor(t, world, []Event{blockedEvent("goal is blocked")})
	if !strings.Contains(text, "nodes are cordoned") {
		t.Fatalf("the diagnosis missed a cordoned node:\n%s", text)
	}
	if strings.Contains(text, "no node can accept work") {
		t.Fatalf("the diagnosis overclaimed with a schedulable node left:\n%s", text)
	}
}

// Slowness caused by pacing is not a fault, and saying so stops an operator
// fixing a working brake.
func TestDiagnosisReportsDisruptionPacing(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1"})
	world.ObservedAt = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	world.Disruptions = []Disruption{
		{Target: "web-0", Domain: "rack-1", At: world.ObservedAt.Add(-time.Minute)},
		{Target: "web-1", Domain: "rack-2", At: world.ObservedAt.Add(-2 * time.Minute)},
	}

	_, text := diagnosisFor(t, world, []Event{blockedEvent("goal is blocked")})
	if !strings.Contains(text, "pacing disruption") {
		t.Fatalf("the diagnosis did not mention pacing:\n%s", text)
	}
	if !strings.Contains(text, "2 disruptive actions across 2 failure domains") {
		t.Fatalf("the pacing finding lost its numbers:\n%s", text)
	}
}

// Every refusal the new safeguards produce must map to a next step, or the
// diagnosis names a cause an operator cannot act on.
func TestDiagnosisSuggestsAStepForEverySafeguard(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1"})
	for _, sample := range []struct {
		message string
		expect  string
	}{
		{`failure domain "rack-1" already holds 1 of at most 1 replicas of workload "web"`, "max_per_domain"},
		{`workload "web" wants 3 replicas but 2 failure domains at 1 per domain hold only 2`, "replica count"},
		{"disruption budget exhausted: 12 disruptions in the last 10m0s plus 1 proposed exceeds 12", "window"},
		{"proposal disrupts 2 failure domains at once; disrupt one at a time", "one failure domain"},
		{`failure domain "rack-1" was disrupted within the last 30s; wait before disrupting "rack-2"`, "cooldown"},
		{`target "web-0" failed 2 times and is in backoff until 2026-07-01T12:00:00Z`, "observed ready"},
		{`node "node-a" is cordoned: disk failing`, "uncordon"},
		{`image "x" carries no provenance attestation and policy requires one`, "a4s attest"},
		{"image provenance: image is not attested by a trusted signer", "re-sign"},
		{"cluster cpu ceiling reached: 800 millis committed plus 400 proposed exceeds 800", "ceiling"},
	} {
		got := suggestFor(sample.message, world)
		if got == "" {
			t.Fatalf("no suggestion for %q", sample.message)
		}
		if !strings.Contains(got, sample.expect) {
			t.Fatalf("suggestion for %q was %q, wanted it to mention %q",
				sample.message, got, sample.expect)
		}
	}
}

// A healthy cluster must not accumulate governor findings, or every diagnosis
// would carry noise that buries the real cause.
func TestDiagnosisStaysQuietWhenNothingIsHeldBack(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1"})
	world.ObservedAt = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	diagnosis, text := diagnosisFor(t, world, []Event{blockedEvent("goal is blocked")})
	for _, unwanted := range []string{
		"waiting out a failure backoff", "nodes are cordoned",
		"no node can accept work", "pacing disruption",
	} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("a quiet cluster produced %q:\n%s", unwanted, text)
		}
	}
	if len(diagnosis.Findings) == 0 {
		t.Fatal("a blocked goal produced no findings at all")
	}
}

// The model-backed diagnoser reasons over the same facts, so the context it is
// given has to carry them. The context is built by subtraction, which means a
// new field is invisible until deliberately copied.
func TestModelContextCarriesGovernorFacts(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1"})
	world.ObservedAt = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	world.Nodes["node-a"].Cordoned = true
	world.Allocations["web-0"] = &Allocation{
		ID: "web-0", Workload: "web", Node: "node-a", Image: spreadImage,
		Phase: AllocationStopped,
	}
	world.Backoff = map[string]*Backoff{}
	recordFailure(&world, "web-0", world.ObservedAt)

	context := BuildModelContext(spreadGoal(1, 0), world, nil)
	if len(context.Nodes) != 1 {
		t.Fatalf("expected one node in context, got %d", len(context.Nodes))
	}
	if !context.Nodes[0].Cordoned {
		t.Fatal("the model context hid a cordoned node")
	}
	if context.Nodes[0].Domain != "rack-1" {
		t.Fatalf("model node domain = %q, want rack-1", context.Nodes[0].Domain)
	}
	if len(context.Allocations) != 1 {
		t.Fatalf("expected one allocation in context, got %d", len(context.Allocations))
	}
	if !context.Allocations[0].InBackoff || context.Allocations[0].Failures != 1 {
		t.Fatalf("the model context hid the backoff: %+v", context.Allocations[0])
	}
}

// The cordon reason is operator prose and is deliberately not copied into model
// input. The fact that a node is out of service is what explains a refusal.
func TestModelContextOmitsCordonReason(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1"})
	world.Nodes["node-a"].Cordoned = true
	world.Nodes["node-a"].CordonReason = "replacing the failed nvme in bay 3"

	payload, err := json.Marshal(BuildModelContext(spreadGoal(1, 0), world, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "nvme") {
		t.Fatalf("operator prose reached model input:\n%s", payload)
	}
}
