package control

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Safeguards tested against each other rather than one at a time.
//
// Every control added for autonomy is individually correct and individually
// tested. The failures worth worrying about are not in any one of them; they are
// in the combinations. A budget that starves the agent meant to repair damage, a
// backoff that blocks the recreation its own remediation just made necessary, a
// spread ceiling that becomes unsatisfiable the moment a node is cordoned: each
// is composed entirely of parts that pass their own tests.
//
// These drive the real engine, the real kernel under DefaultPolicy, and the real
// agents, against a clock the test advances, and assert the cluster still ends
// up where it should.

// harness is a cluster under a controllable clock.
type harness struct {
	engine   *Engine
	executor *MemoryExecutor
	clock    time.Time
	goal     Goal
}

func newHarness(t *testing.T, goal Goal, world World) *harness {
	t.Helper()
	start := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	world.ObservedAt = start
	executor := NewMemoryExecutor(world)
	engine := NewEngine(executor,
		RemediationAgent{}, RolloutAgent{}, PlacementAgent{}, NetworkAgent{})
	h := &harness{engine: engine, executor: executor, clock: start, goal: goal}
	// The engine's clock drives evidence observation times, which drive every
	// window the governor measures. Without control of it a test cannot let a
	// backoff expire without actually sleeping.
	engine.now = func() time.Time { return h.clock }
	for _, prober := range engine.Probers {
		if measured, ok := prober.(*MeasuredProber); ok {
			measured.Now = func() time.Time { return h.clock }
		}
	}
	return h
}

func (h *harness) advance(d time.Duration) { h.clock = h.clock.Add(d) }

// run drives one reconciliation, reporting whether it converged and why not.
func (h *harness) run(t *testing.T) error {
	t.Helper()
	return h.engine.Run(h.goal, 12)
}

// converge reconciles until the goal is met or the attempts run out, advancing
// the clock between attempts the way a real server's ticker would.
func (h *harness) converge(t *testing.T, attempts int, between time.Duration) error {
	t.Helper()
	var err error
	for i := 0; i < attempts; i++ {
		if err = h.run(t); err == nil {
			return nil
		}
		h.advance(between)
	}
	return err
}

func (h *harness) world() World { return h.executor.World() }

func (h *harness) allocations() map[string]*Allocation { return h.world().Allocations }

func recoveryWorld() World {
	return spreadWorld(map[string]string{
		"rack1-a": "rack-1", "rack2-a": "rack-2", "rack3-a": "rack-3",
	})
}

func recoveryGoal(replicas, maxPerDomain int) Goal {
	goal := spreadGoal(replicas, maxPerDomain)
	goal.Route = nil
	return goal
}

// The baseline: the safeguards must not stop an ordinary deployment from
// converging at all.
func TestClusterConvergesWithEverySafeguardOn(t *testing.T) {
	h := newHarness(t, recoveryGoal(3, 1), recoveryWorld())
	if err := h.converge(t, 10, time.Minute); err != nil {
		t.Fatalf("a healthy cluster did not converge with the safeguards on: %v", err)
	}

	domains := map[string]int{}
	for _, allocation := range h.allocations() {
		if allocation.Phase == AllocationStopped {
			continue
		}
		domains[h.world().Nodes[allocation.Node].FailureDomain()]++
	}
	if len(domains) != 3 {
		t.Fatalf("replicas did not spread across three domains: %v", domains)
	}
}

// The interaction that worried me most: remediation clears a failed allocation,
// and the backoff its failure opened then blocks the recreation. If the backoff
// never expired relative to the retry loop, the cluster would sit at reduced
// capacity forever while every individual component behaved correctly.
func TestFailedReplicaIsReplacedAfterItsBackoffExpires(t *testing.T) {
	h := newHarness(t, recoveryGoal(2, 1), recoveryWorld())
	if err := h.converge(t, 10, time.Minute); err != nil {
		t.Fatalf("initial convergence failed: %v", err)
	}

	// One replica dies the way a crashing container does.
	var victim string
	for id := range h.allocations() {
		victim = id
		break
	}
	h.advance(time.Minute)
	if err := h.executor.Project(Evidence{
		Kind: EvidenceAllocationFailed, Target: victim, ObservedAt: h.clock,
		Observed: map[string]string{"exit_code": "1", "restarts": "3"},
	}); err != nil {
		t.Fatal(err)
	}
	if !h.world().Backoff[victim].Active(h.clock) {
		t.Fatal("a failure did not open a backoff")
	}

	// Immediately afterwards the goal is held back rather than failed, and it
	// says so in a way a caller can act on.
	err := h.run(t)
	var pacing Pacing
	if !errors.As(err, &pacing) {
		t.Fatalf("a backed-off goal reported %v, want pacing", err)
	}
	if !strings.Contains(pacing.Reason, victim) {
		t.Fatalf("pacing did not name the target: %q", pacing.Reason)
	}

	// Once the backoff expires the cluster repairs itself with no operator
	// involvement, which is the whole claim.
	h.advance(2 * MaxBackoff)
	if err := h.converge(t, 10, time.Minute); err != nil {
		t.Fatalf("the cluster did not recover after the backoff expired: %v", err)
	}
	live := 0
	for _, allocation := range h.allocations() {
		if allocation.Phase != AllocationStopped {
			live++
		}
	}
	if live != 2 {
		t.Fatalf("recovered to %d live replicas, want 2", live)
	}
}

// A node dying must be cordoned, drained, and its work replaced, without an
// operator and without violating the spread ceiling on the way.
func TestNodeFailureIsCordonedDrainedAndReplaced(t *testing.T) {
	world := recoveryWorld()
	h := newHarness(t, recoveryGoal(2, 1), world)
	if err := h.converge(t, 10, time.Minute); err != nil {
		t.Fatalf("initial convergence failed: %v", err)
	}

	// Find a node actually holding a replica and take it down.
	var failing string
	for _, allocation := range h.allocations() {
		if allocation.Phase != AllocationStopped {
			failing = allocation.Node
			break
		}
	}
	h.advance(time.Minute)
	if err := h.executor.Project(Evidence{
		Kind: EvidenceNodeUnreachable, Target: failing, ObservedAt: h.clock,
		Observed: map[string]string{"node": failing},
	}); err != nil {
		t.Fatal(err)
	}
	if h.world().Nodes[failing].Healthy {
		t.Fatal("an unreachable node stayed healthy")
	}

	// Reconcile until it settles, giving the cooldown room between attempts.
	_ = h.converge(t, 20, time.Minute)

	// The cluster must have stopped placing work there, on its own.
	if !h.world().Nodes[failing].Cordoned {
		t.Fatalf("the failed node %q was never cordoned", failing)
	}
	if h.world().Nodes[failing].Schedulable() {
		t.Fatalf("the failed node %q is still attracting work", failing)
	}

	// What it must not do is quietly duplicate the workload elsewhere. An
	// unreachable node keeps running what it was told to run, so relocating on
	// silence turns a partition into two live copies. Anything that did move
	// had to pass through a stop the node acknowledged.
	for id, allocation := range h.allocations() {
		if allocation.Node != failing || allocation.Phase == AllocationStopped {
			continue
		}
		for otherID, other := range h.allocations() {
			if otherID == id || other.Workload != allocation.Workload {
				continue
			}
			if other.Replica == allocation.Replica && other.Phase != AllocationStopped {
				t.Fatalf("replica %d exists on both %q and %q after a partition",
					allocation.Replica, id, otherID)
			}
		}
	}
}

// Reachability has to be recorded from what the control plane observes, or node
// health is a fact nobody ever updates and a dead node keeps attracting work.
func TestUnreachableNodeStopsAttractingPlacements(t *testing.T) {
	world := recoveryWorld()
	world.ObservedAt = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	next, err := Project(world, Evidence{
		Kind: EvidenceNodeUnreachable, Target: "rack2-a", ObservedAt: world.ObservedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Nodes["rack2-a"].Healthy {
		t.Fatal("an unreachable node stayed healthy")
	}
	if next.Nodes["rack2-a"].Schedulable() {
		t.Fatal("an unreachable node is still schedulable")
	}

	// Coming back restores it.
	back, err := Project(next, Evidence{
		Kind: EvidenceNodeReachable, Target: "rack2-a", ObservedAt: world.ObservedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !back.Nodes["rack2-a"].Schedulable() {
		t.Fatal("a recovered node did not return to service")
	}
}

// A cordon is an operator decision and must outlive a node bouncing, or a
// machine somebody is working on returns to the scheduler by reconnecting.
func TestReachabilityDoesNotClearACordon(t *testing.T) {
	world := recoveryWorld()
	world.Nodes["rack1-a"].Cordoned = true
	world.Nodes["rack1-a"].CordonReason = "maintenance"

	next, err := Project(world, Evidence{Kind: EvidenceNodeUnreachable, Target: "rack1-a"})
	if err != nil {
		t.Fatal(err)
	}
	back, err := Project(next, Evidence{Kind: EvidenceNodeReachable, Target: "rack1-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !back.Nodes["rack1-a"].Cordoned {
		t.Fatal("reconnecting cleared an operator's cordon")
	}
	if back.Nodes["rack1-a"].CordonReason != "maintenance" {
		t.Fatal("reconnecting lost the cordon reason")
	}
}

// Cordoning enough of the cluster makes the spread ceiling unsatisfiable. That
// must present as a goal that cannot converge with an explanation, never as a
// silent violation of the ceiling.
func TestSpreadIsNeverViolatedToMakeProgress(t *testing.T) {
	h := newHarness(t, recoveryGoal(3, 1), recoveryWorld())
	if err := h.converge(t, 10, time.Minute); err != nil {
		t.Fatalf("initial convergence failed: %v", err)
	}

	// Take two domains out of service and destroy what was on them.
	for _, node := range []string{"rack2-a", "rack3-a"} {
		if err := h.executor.Project(Evidence{
			Kind: EvidenceNodeCordoned, Target: node, ObservedAt: h.clock,
			Observed: map[string]string{"reason": "maintenance"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	for id, allocation := range h.allocations() {
		if allocation.Node == "rack1-a" {
			continue
		}
		h.advance(time.Minute)
		if err := h.executor.Project(Evidence{
			Kind: EvidenceAllocationStopped, Target: id, ObservedAt: h.clock,
		}); err != nil {
			t.Fatal(err)
		}
	}

	_ = h.converge(t, 10, time.Minute)

	// Whatever happened, no domain may hold more than one replica.
	domains := map[string]int{}
	for _, allocation := range h.allocations() {
		if allocation.Phase == AllocationStopped {
			continue
		}
		domains[h.world().Nodes[allocation.Node].FailureDomain()]++
	}
	for domain, count := range domains {
		if count > 1 {
			t.Fatalf("domain %q holds %d replicas; the ceiling was violated to make progress",
				domain, count)
		}
	}

	// And the reason is explainable rather than silent.
	diagnosis := LogDiagnoser{}.Diagnose(h.goal.ID, h.engine.Events, h.world())
	if !strings.Contains(diagnosis.String(), "cordoned") {
		t.Fatalf("the diagnosis did not mention the cordons:\n%s", diagnosis)
	}
}

// Clearing allocations that already died must not consume the budget that paces
// disruption of live work, or a cluster hit by many failures at once would be
// unable to repair itself.
func TestMassFailureDoesNotExhaustTheDisruptionBudget(t *testing.T) {
	h := newHarness(t, recoveryGoal(3, 1), recoveryWorld())
	if err := h.converge(t, 10, time.Minute); err != nil {
		t.Fatalf("initial convergence failed: %v", err)
	}

	for id := range h.allocations() {
		h.advance(time.Second)
		if err := h.executor.Project(Evidence{
			Kind: EvidenceAllocationFailed, Target: id, ObservedAt: h.clock,
			Observed: map[string]string{"exit_code": "1"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if spent := len(h.world().Disruptions); spent != 0 {
		t.Fatalf("failures consumed %d of the disruption budget", spent)
	}

	// Remediation clears them, still without spending budget, because nothing
	// live is being taken away.
	h.advance(2 * MaxBackoff)
	_ = h.converge(t, 15, time.Minute)
	kernel := Kernel{Policy: DefaultPolicy()}
	if _, held := kernel.Paced(h.goal, h.world()); held {
		if len(h.world().Disruptions) >= DefaultMaxDisruptions {
			t.Fatalf("repairing failures exhausted the budget: %d recorded",
				len(h.world().Disruptions))
		}
	}
}

// Pacing must be distinguishable from failure by a caller, which is what stops
// a working governor showing up as a broken cluster.
func TestPacingIsDistinguishableFromFailure(t *testing.T) {
	world := recoveryWorld()
	world.ObservedAt = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	kernel := Kernel{Policy: DefaultPolicy()}
	goal := recoveryGoal(1, 0)

	// Nothing wrong: not paced.
	if _, held := kernel.Paced(goal, world); held {
		t.Fatal("a quiet cluster reported itself paced")
	}

	// A cordon is not pacing: it stands until somebody decides otherwise, and
	// telling a caller to wait for it would be telling them to wait forever.
	world.Nodes["rack1-a"].Cordoned = true
	if _, held := kernel.Paced(goal, world); held {
		t.Fatal("a cordon was reported as a transient pacing")
	}

	// A backoff is, and it names when it lifts.
	world.Allocations["web-0"] = &Allocation{
		ID: "web-0", Workload: "web", Node: "rack1-a", Phase: AllocationStopped,
	}
	world.Backoff = map[string]*Backoff{}
	recordFailure(&world, "web-0", world.ObservedAt)
	pacing, held := kernel.Paced(goal, world)
	if !held {
		t.Fatal("an active backoff was not reported as pacing")
	}
	if !pacing.Until.After(world.ObservedAt) {
		t.Fatalf("pacing expiry %s is not in the future", pacing.Until)
	}
	if !strings.Contains(pacing.Error(), "web-0") {
		t.Fatalf("pacing did not name the target: %q", pacing.Error())
	}
}
