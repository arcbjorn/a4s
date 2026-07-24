package node

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

func testQueue(t *testing.T, now time.Time) *Queue {
	t.Helper()
	queue, err := OpenQueue(filepath.Join(t.TempDir(), "queue.jsonl"), "intake", "triage")
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	queue.Now = func() time.Time { return now }
	return queue
}

func queueAt(t *testing.T, queue *Queue, now time.Time) {
	t.Helper()
	queue.Now = func() time.Time { return now }
}

// Delivery is FIFO so a stuck task is diagnosable. Map iteration order would
// make one impossible to find.
func TestQueueDeliversInEnqueueOrder(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	queue := testQueue(t, now)

	for _, id := range []string{"a", "b", "c"} {
		if err := queue.Enqueue(id, "payload-"+id); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"a", "b", "c"} {
		task, claimed, err := queue.Claim("triage-0")
		if err != nil || !claimed {
			t.Fatalf("expected a task: %v %v", claimed, err)
		}
		if task.ID != want {
			t.Fatalf("expected %q, got %q", want, task.ID)
		}
		if err := queue.Ack("triage-0", task.ID); err != nil {
			t.Fatal(err)
		}
	}
}

// A producer must be able to retry after an ambiguous failure without
// delivering the same work twice.
func TestQueueEnqueueIsIdempotent(t *testing.T) {
	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())

	for i := 0; i < 3; i++ {
		if err := queue.Enqueue("a", "payload"); err != nil {
			t.Fatal(err)
		}
	}
	if waiting, _ := queue.Depth(); waiting != 1 {
		t.Fatalf("expected one task, got %d", waiting)
	}
}

// A claimed task is not available to another worker, or two agents would run
// the same work and pay for it twice.
func TestClaimedTaskIsNotRedelivered(t *testing.T) {
	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())
	if err := queue.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, _ := queue.Claim("triage-0"); !claimed {
		t.Fatal("expected the first claim to succeed")
	}
	if _, claimed, _ := queue.Claim("triage-1"); claimed {
		t.Fatal("a second worker claimed a held task")
	}
	waiting, inFlight := queue.Depth()
	if waiting != 0 || inFlight != 1 {
		t.Fatalf("expected the task in flight, got waiting=%d in_flight=%d", waiting, inFlight)
	}
}

// An instance that dies holding a task would otherwise strand it forever, and
// the control plane cannot redeliver work it never sees the contents of.
func TestLapsedClaimIsRedelivered(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	queue := testQueue(t, now)
	queue.Lease = time.Minute
	if err := queue.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, _ := queue.Claim("triage-0"); !claimed {
		t.Fatal("expected the first claim to succeed")
	}

	queueAt(t, queue, now.Add(2*time.Minute))
	task, claimed, err := queue.Claim("triage-1")
	if err != nil || !claimed {
		t.Fatalf("expected a lapsed claim to be redelivered: %v %v", claimed, err)
	}
	if task.Attempts != 2 {
		t.Fatalf("expected redelivery to count an attempt, got %d", task.Attempts)
	}
}

// Work that fails every instance it touches would otherwise burn budget
// indefinitely.
func TestQueueStopsDeliveringAfterMaxAttempts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	queue := testQueue(t, now)
	queue.MaxAttempts = 2
	if err := queue.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		task, claimed, err := queue.Claim("triage-0")
		if err != nil || !claimed {
			t.Fatalf("attempt %d: expected a task: %v %v", i, claimed, err)
		}
		if err := queue.Requeue("triage-0", task.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, claimed, _ := queue.Claim("triage-0"); claimed {
		t.Fatal("expected an exhausted task to stop being delivered")
	}
	// A queue that silently stops draining is worse than one that says so.
	if stalled := queue.Stalled(); len(stalled) != 1 || stalled[0] != "a" {
		t.Fatalf("expected the task to be reported stalled, got %v", stalled)
	}
	// It must also stop inflating depth, or scaling would chase work nobody can
	// be given.
	if waiting, _ := queue.Depth(); waiting != 0 {
		t.Fatalf("expected a stalled task not to count as waiting, got %d", waiting)
	}
}

// After a lease lapse the previous holder no longer speaks for the task.
func TestOnlyHolderMayAcknowledge(t *testing.T) {
	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())
	if err := queue.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, _ := queue.Claim("triage-0"); !claimed {
		t.Fatal("expected a claim")
	}
	if err := queue.Ack("triage-1", "a"); err == nil {
		t.Fatal("a non-holder acknowledged another instance's work")
	}
	if err := queue.Ack("triage-0", "a"); err != nil {
		t.Fatalf("holder could not acknowledge: %v", err)
	}
}

// Acknowledging an absent task is the expected result of a retry after the
// first ack succeeded.
func TestAckIsIdempotent(t *testing.T) {
	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())
	if err := queue.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, _ := queue.Claim("triage-0"); !claimed {
		t.Fatal("expected a claim")
	}
	if err := queue.Ack("triage-0", "a"); err != nil {
		t.Fatal(err)
	}
	if err := queue.Ack("triage-0", "a"); err != nil {
		t.Fatalf("expected a repeated ack to be idempotent, got %v", err)
	}
}

// An agent that claimed a task before a node restart must not silently lose it.
func TestQueueSurvivesRestart(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "queue.jsonl")

	first, err := OpenQueue(path, "intake", "triage")
	if err != nil {
		t.Fatal(err)
	}
	first.Now = func() time.Time { return now }
	if err := first.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}
	if err := first.Enqueue("b", "payload"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, _ := first.Claim("triage-0"); !claimed {
		t.Fatal("expected a claim")
	}

	reopened, err := OpenQueue(path, "intake", "triage")
	if err != nil {
		t.Fatalf("reopen queue: %v", err)
	}
	reopened.Now = func() time.Time { return now }
	waiting, inFlight := reopened.Depth()
	if waiting != 1 || inFlight != 1 {
		t.Fatalf("expected state to survive restart, got waiting=%d in_flight=%d", waiting, inFlight)
	}
}

// The node knows an instance is gone long before the queue would infer it from
// a lease.
func TestReleaseAllocationRequeuesWork(t *testing.T) {
	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())
	if err := queue.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, _ := queue.Claim("triage-0"); !claimed {
		t.Fatal("expected a claim")
	}
	if err := queue.ReleaseAllocation("triage-0"); err != nil {
		t.Fatal(err)
	}
	if waiting, _ := queue.Depth(); waiting != 1 {
		t.Fatalf("expected released work to be waiting again, got %d", waiting)
	}
}

// Depth is measured by the node rather than reported by an agent, because an
// agent describing its own backlog could scale its own replica count.
func TestQueueEvidenceReportsDepth(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	queue := testQueue(t, now)
	for _, id := range []string{"a", "b", "c"} {
		if err := queue.Enqueue(id, "payload"); err != nil {
			t.Fatal(err)
		}
	}
	if _, claimed, _ := queue.Claim("triage-0"); !claimed {
		t.Fatal("expected a claim")
	}

	evidence := queue.Evidence()
	if evidence.Kind != control.EvidenceQueueObserved || evidence.Target != "intake" {
		t.Fatalf("unexpected evidence %+v", evidence)
	}
	if evidence.Observed["depth"] != "2" || evidence.Observed["in_flight"] != "1" {
		t.Fatalf("expected measured depth, got %v", evidence.Observed)
	}
	if evidence.ObservedAt != now {
		t.Fatalf("expected the measurement to carry its time, got %v", evidence.ObservedAt)
	}
}

// Recording the hold is what makes a drain observable. Without it the control
// plane would stop an instance mid-task believing it idle.
func TestBrokerClaimMarksInstanceBusy(t *testing.T) {
	agents := meteredAgents(t, "triage-0")
	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())
	broker := &QueueBroker{Queue: queue, Agents: agents}
	if err := queue.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}

	task, claimed, err := broker.Claim("triage-0")
	if err != nil || !claimed {
		t.Fatalf("expected a claim: %v %v", claimed, err)
	}
	evidence, err := agents.drain(control.Action{
		Kind: control.ActionDrainAllocation, Target: "triage-0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Kind != control.EvidenceAgentDraining {
		t.Fatalf("expected a busy instance to report draining, got %q", evidence.Kind)
	}
	if evidence.Observed["task"] != task.ID {
		t.Fatalf("expected the held task to be reported, got %v", evidence.Observed)
	}

	if err := broker.Ack("triage-0", task.ID); err != nil {
		t.Fatal(err)
	}
	evidence, err = agents.drain(control.Action{
		Kind: control.ActionDrainAllocation, Target: "triage-0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Kind != control.EvidenceAllocationDrained {
		t.Fatalf("expected an idle instance to report drained, got %q", evidence.Kind)
	}
}

// An instance that finished its task and then claimed another would never
// actually drain, and the rollout waiting on it would stall forever.
func TestDrainingInstanceMayNotClaim(t *testing.T) {
	agents := meteredAgents(t, "triage-0")
	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())
	broker := &QueueBroker{Queue: queue, Agents: agents}
	if err := queue.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}
	if _, err := agents.drain(control.Action{
		Kind: control.ActionDrainAllocation, Target: "triage-0",
	}); err != nil {
		t.Fatal(err)
	}

	_, claimed, err := broker.Claim("triage-0")
	if claimed || !errors.Is(err, ErrDraining) {
		t.Fatalf("expected a draining instance to be refused work, got %v %v", claimed, err)
	}
	// The task must remain available for a worker that can actually run it.
	if waiting, _ := queue.Depth(); waiting != 1 {
		t.Fatalf("expected the refused task to stay waiting, got %d", waiting)
	}
}

// An exhausted instance cannot pay for what it would start.
func TestExhaustedInstanceMayNotClaim(t *testing.T) {
	agents := meteredAgents(t, "triage-0")
	agents.Spend("triage-0", control.Budget{Tokens: 5000})
	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())
	broker := &QueueBroker{Queue: queue, Agents: agents}
	if err := queue.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}

	_, claimed, err := broker.Claim("triage-0")
	if claimed || !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("expected an exhausted instance to be refused, got %v %v", claimed, err)
	}
}

// Claiming first and validating after would consume an attempt on a task the
// instance was never eligible for.
func TestRefusedClaimDoesNotConsumeAnAttempt(t *testing.T) {
	agents := meteredAgents(t, "triage-0")
	agents.Spend("triage-0", control.Budget{Tokens: 5000})
	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())
	broker := &QueueBroker{Queue: queue, Agents: agents}
	if err := queue.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := broker.Claim("triage-0"); err == nil {
		t.Fatal("expected the claim to be refused")
	}

	// A healthy worker must find the task untouched.
	healthy := meteredAgents(t, "triage-1")
	other := &QueueBroker{Queue: queue, Agents: healthy}
	task, claimed, err := other.Claim("triage-1")
	if err != nil || !claimed {
		t.Fatalf("expected a healthy worker to claim the task: %v %v", claimed, err)
	}
	if task.Attempts != 1 {
		t.Fatalf("expected the refused claim not to count, got %d attempts", task.Attempts)
	}
}

// The node tracks one task slot per instance, and a drain waits on it being
// empty, so a second concurrent claim would make completion unobservable.
func TestInstanceMayNotHoldTwoTasks(t *testing.T) {
	agents := meteredAgents(t, "triage-0")
	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())
	broker := &QueueBroker{Queue: queue, Agents: agents}
	for _, id := range []string{"a", "b"} {
		if err := queue.Enqueue(id, "payload"); err != nil {
			t.Fatal(err)
		}
	}
	if _, claimed, err := broker.Claim("triage-0"); err != nil || !claimed {
		t.Fatalf("expected the first claim to succeed: %v %v", claimed, err)
	}
	if _, claimed, err := broker.Claim("triage-0"); claimed || err == nil {
		t.Fatalf("expected a second concurrent claim to be refused, got %v %v", claimed, err)
	}
}

// An instance the node holds no reservation for was never prepared here.
func TestUnmeteredInstanceMayNotClaim(t *testing.T) {
	agents := NewAgents(t.TempDir())
	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())
	broker := &QueueBroker{Queue: queue, Agents: agents}
	if err := queue.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := broker.Claim("ghost-0"); claimed || err == nil {
		t.Fatalf("expected an unmetered instance to be refused, got %v %v", claimed, err)
	}
}

// Depth is the most perishable fact the scheduler consumes, so it is measured
// on the supervision tick.
func TestSupervisorReportsQueueDepth(t *testing.T) {
	supervisor, _, _ := newSupervisorFixture(t)
	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())
	supervisor.Queues = []*Queue{queue}
	if err := queue.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}

	observations, err := supervisor.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var depth *control.Evidence
	for i := range observations {
		if observations[i].Kind == control.EvidenceQueueObserved {
			depth = &observations[i]
		}
	}
	if depth == nil {
		t.Fatalf("expected queue evidence from supervision, got %+v", observations)
	}
	if depth.Observed["depth"] != "1" {
		t.Fatalf("expected measured depth, got %v", depth.Observed)
	}
	if depth.Source != "node-supervisor" {
		t.Fatalf("expected supervisor attribution, got %q", depth.Source)
	}
}

// Waiting for a claim lease to lapse would leave a task undelivered for minutes
// when the node already knows the holder is gone.
func TestDeletingAllocationReturnsItsWork(t *testing.T) {
	queue := testQueue(t, time.Unix(1_700_000_000, 0).UTC())
	agents := meteredAgents(t, "triage-0")
	runtime := &CompositeRuntime{
		Containers: NewContainerRuntime(&fakeBackend{
			state: BackendState{Exists: true, Running: false},
		}),
		Agents: agents,
		Queues: []*Queue{queue},
	}
	if err := queue.Enqueue("a", "payload"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, _ := queue.Claim("triage-0"); !claimed {
		t.Fatal("expected a claim")
	}

	if _, err := runtime.Execute(context.Background(), control.Action{
		Kind: control.ActionDeleteAllocation, Target: "triage-0",
	}); err != nil {
		t.Fatal(err)
	}
	if waiting, _ := queue.Depth(); waiting != 1 {
		t.Fatalf("expected the deleted instance's work to return, got %d waiting", waiting)
	}
}
