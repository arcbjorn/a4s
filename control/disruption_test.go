package control

import (
	"strings"
	"testing"
	"time"
)

func disruptionWorld(t *testing.T) World {
	t.Helper()
	world := spreadWorld(map[string]string{"node-a": "rack-1", "node-b": "rack-2"})
	world.ObservedAt = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"web-0", "web-1"} {
		node := "node-a"
		if id == "web-1" {
			node = "node-b"
		}
		world.Allocations[id] = &Allocation{
			ID: id, Workload: "web", Node: node, Image: spreadImage,
			Phase: AllocationRunning, Ready: true,
			Resources: Resources{CPUMillis: 100, MemoryMB: 128},
		}
	}
	return world
}

func stopProposal(targets ...string) Proposal {
	proposal := Proposal{
		ID: "p1", AgentID: "rollout-agent", GoalID: "web-public",
	}
	for _, target := range targets {
		proposal.Actions = append(proposal.Actions, Action{
			ID: "stop-" + target, Kind: ActionStopAllocation,
			Target: target, Workload: "web",
		})
	}
	return proposal
}

// The budget bounds the whole cluster, not one proposal. A control plane that
// has started thrashing produces many individually legal proposals.
func TestDisruptionBudgetBoundsTheCluster(t *testing.T) {
	world := disruptionWorld(t)
	kernel := Kernel{Policy: Policy{
		MaxDisruptionsPerWindow: 2, DisruptionWindow: 10 * time.Minute,
	}}

	// Two disruptions already recorded inside the window.
	for i := 0; i < 2; i++ {
		world.Disruptions = append(world.Disruptions, Disruption{
			Target: "other", Domain: "rack-1", Kind: EvidenceAllocationStopped,
			At: world.ObservedAt.Add(-time.Minute),
		})
	}
	err := kernel.checkDisruptionBudget(world, stopProposal("web-0"))
	if err == nil || !strings.Contains(err.Error(), "disruption budget exhausted") {
		t.Fatalf("expected the budget to refuse, got %v", err)
	}

	// The same proposal is fine once those disruptions age out of the window.
	world.ObservedAt = world.ObservedAt.Add(time.Hour)
	if err := kernel.checkDisruptionBudget(world, stopProposal("web-0")); err != nil {
		t.Fatalf("budget refused after the window passed: %v", err)
	}
}

// Disrupting two failure domains at once takes out more capacity than any single
// workload's availability floor was defending.
func TestDisruptionRefusesTwoDomainsAtOnce(t *testing.T) {
	world := disruptionWorld(t)
	kernel := Kernel{Policy: DefaultPolicy()}

	err := kernel.checkDisruptionBudget(world, stopProposal("web-0", "web-1"))
	if err == nil || !strings.Contains(err.Error(), "failure domains at once") {
		t.Fatalf("expected a multi-domain denial, got %v", err)
	}
	// Either one alone is allowed.
	if err := kernel.checkDisruptionBudget(world, stopProposal("web-0")); err != nil {
		t.Fatalf("a single-domain proposal was refused: %v", err)
	}
}

// A domain stays under disruption for its cooldown, so a second domain cannot be
// disrupted immediately after the first.
func TestDisruptionCooldownSerializesDomains(t *testing.T) {
	world := disruptionWorld(t)
	kernel := Kernel{Policy: DefaultPolicy()}
	world.Disruptions = append(world.Disruptions, Disruption{
		Target: "web-0", Domain: "rack-1", Kind: EvidenceAllocationStopped,
		At: world.ObservedAt.Add(-5 * time.Second),
	})

	err := kernel.checkDisruptionBudget(world, stopProposal("web-1"))
	if err == nil || !strings.Contains(err.Error(), "was disrupted within the last") {
		t.Fatalf("expected a cooldown denial, got %v", err)
	}
	// Continuing to work on the same domain is not blocked by its own cooldown.
	if err := kernel.checkDisruptionBudget(world, stopProposal("web-0")); err != nil {
		t.Fatalf("the disrupting domain blocked itself: %v", err)
	}
	// Once the cooldown lapses the other domain is available.
	world.ObservedAt = world.ObservedAt.Add(DefaultDisruptionCooldown + time.Second)
	if err := kernel.checkDisruptionBudget(world, stopProposal("web-1")); err != nil {
		t.Fatalf("cooldown did not lapse: %v", err)
	}
}

// Adding capacity is never disruptive, or the governor would slow recovery
// instead of protecting it.
func TestDisruptionBudgetIgnoresCreation(t *testing.T) {
	world := disruptionWorld(t)
	kernel := Kernel{Policy: Policy{MaxDisruptionsPerWindow: 1, DisruptionWindow: time.Hour}}
	for i := 0; i < 5; i++ {
		world.Disruptions = append(world.Disruptions, Disruption{
			Target: "other", Domain: "rack-1", At: world.ObservedAt.Add(-time.Minute),
		})
	}
	creation := Proposal{ID: "p", AgentID: "placement-agent", Actions: []Action{
		{ID: "create", Kind: ActionCreateAllocation, Target: "web-2"},
		{ID: "start", Kind: ActionStartAllocation, Target: "web-2"},
	}}
	if err := kernel.checkDisruptionBudget(world, creation); err != nil {
		t.Fatalf("creation was charged to the disruption budget: %v", err)
	}
}

// A zero budget is off, so a Policy written before the governor existed behaves
// exactly as it did.
func TestDisruptionBudgetOffByZero(t *testing.T) {
	world := disruptionWorld(t)
	for i := 0; i < 50; i++ {
		world.Disruptions = append(world.Disruptions, Disruption{
			Target: "other", Domain: "rack-1", At: world.ObservedAt.Add(-time.Second),
		})
	}
	kernel := Kernel{Policy: Policy{MaxActionsPerProposal: 8}}
	if err := kernel.checkDisruptionBudget(world, stopProposal("web-0", "web-1")); err != nil {
		t.Fatalf("a disabled governor refused a proposal: %v", err)
	}
}

// The hysteresis: a target that keeps failing must not be re-created every
// reconciliation round forever.
func TestBackoffStopsARescheduleLoop(t *testing.T) {
	world := disruptionWorld(t)
	world.Backoff = map[string]*Backoff{}
	recordFailure(&world, "web-0", world.ObservedAt)

	create := Proposal{ID: "p", AgentID: "placement-agent", Actions: []Action{
		{ID: "create", Kind: ActionCreateAllocation, Target: "web-0"},
	}}
	err := checkBackoff(world, create)
	if err == nil || !strings.Contains(err.Error(), "backoff") {
		t.Fatalf("expected a backoff denial, got %v", err)
	}

	// Removal is still allowed, or backoff would block the remediation it is
	// meant to pace.
	if err := checkBackoff(world, stopProposal("web-0")); err != nil {
		t.Fatalf("backoff blocked removal: %v", err)
	}

	// It lapses.
	world.ObservedAt = world.ObservedAt.Add(BaseBackoff + time.Second)
	if err := checkBackoff(world, create); err != nil {
		t.Fatalf("backoff did not lapse: %v", err)
	}
}

// Consecutive failures escalate, and a recovery resets the ladder.
func TestBackoffEscalatesAndResets(t *testing.T) {
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	world := World{Backoff: map[string]*Backoff{}}

	recordFailure(&world, "web-0", at)
	first := world.Backoff["web-0"].Until.Sub(at)
	recordFailure(&world, "web-0", at)
	second := world.Backoff["web-0"].Until.Sub(at)
	if second <= first {
		t.Fatalf("second failure did not escalate: %s then %s", first, second)
	}
	if world.Backoff["web-0"].Failures != 2 {
		t.Fatalf("failures = %d, want 2", world.Backoff["web-0"].Failures)
	}

	clearBackoff(&world, "web-0")
	if world.Backoff["web-0"] != nil {
		t.Fatal("recovery did not reset the backoff")
	}

	// The ladder is capped so an operator's fix is not delayed indefinitely.
	if got := backoffFor(100); got != MaxBackoff {
		t.Fatalf("backoffFor(100) = %s, want %s", got, MaxBackoff)
	}
}

// The ledger is derived from evidence and bounded, or a long-lived cluster would
// rebuild an ever-growing world from its log.
func TestDisruptionLedgerIsDerivedAndBounded(t *testing.T) {
	world := disruptionWorld(t)
	at := world.ObservedAt

	next, err := Project(world, Evidence{
		Kind: EvidenceAllocationStopped, Target: "web-0", ObservedAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Disruptions) != 1 {
		t.Fatalf("stop recorded %d disruptions, want 1", len(next.Disruptions))
	}
	if next.Disruptions[0].Domain != "rack-1" {
		t.Fatalf("disruption domain = %q, want rack-1", next.Disruptions[0].Domain)
	}
	// The input world must not have been touched.
	if len(world.Disruptions) != 0 {
		t.Fatal("Project mutated the world it was given")
	}

	// An entry older than the retention bound is dropped on the next record.
	next.Disruptions = append([]Disruption{{
		Target: "ancient", Domain: "rack-1", At: at.Add(-2 * DisruptionRetention),
	}}, next.Disruptions...)
	later, err := Project(next, Evidence{
		Kind: EvidenceAllocationStopped, Target: "web-1", ObservedAt: at.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, disruption := range later.Disruptions {
		if disruption.Target == "ancient" {
			t.Fatal("the ledger kept an entry past its retention bound")
		}
	}
}

// A failure is the workload losing itself, not the control plane spending
// capacity. Charging it to the budget would stop the cluster repairing it.
func TestFailureIsNotChargedToTheBudget(t *testing.T) {
	world := disruptionWorld(t)
	next, err := Project(world, Evidence{
		Kind: EvidenceAllocationFailed, Target: "web-0",
		ObservedAt: world.ObservedAt,
		Observed:   map[string]string{"exit_code": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Disruptions) != 0 {
		t.Fatalf("a failure was charged to the disruption budget: %v", next.Disruptions)
	}
	if !next.Backoff["web-0"].Active(world.ObservedAt) {
		t.Fatal("a failure did not open a backoff")
	}
}

// Readiness clears the failure history, which is what makes it consecutive.
func TestReadinessClearsBackoff(t *testing.T) {
	world := disruptionWorld(t)
	world.Allocations["web-0"].Phase = AllocationRunning
	world.Backoff = map[string]*Backoff{}
	recordFailure(&world, "web-0", world.ObservedAt)
	world.Allocations["web-0"].Phase = AllocationRunning

	next, err := Project(world, Evidence{
		Kind: EvidenceAllocationReady, Target: "web-0",
		ObservedAt: world.ObservedAt, ExpiresAt: world.ObservedAt.Add(time.Minute),
		Observed: map[string]string{"ready": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Backoff["web-0"] != nil {
		t.Fatal("readiness did not clear the backoff")
	}
}
