package node

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// DefaultClaimLease bounds how long one agent may hold a task before the queue
// assumes it will never finish.
//
// It is deliberately longer than an agent's typical task: reclaiming work from
// an instance that is merely slow produces duplicate execution, which for an
// agent means paying twice and possibly acting twice. The wall-clock budget is
// the real bound on task length; this is the backstop for a node that died.
const DefaultClaimLease = 15 * time.Minute

// QueueTask is one unit of work an agent instance can claim.
type QueueTask struct {
	// ID identifies the task within its queue.
	ID string `json:"id"`
	// Payload is opaque to a4s. The control plane never inspects it, which is
	// what keeps task content out of goals, proposals, and the event log.
	Payload string `json:"payload,omitempty"`
	// EnqueuedAt orders delivery and lets an operator see how long work waited.
	EnqueuedAt time.Time `json:"enqueued_at"`
	// Claim records the current holder, empty when the task is waiting.
	Claim *TaskClaim `json:"claim,omitempty"`
	// Attempts counts how many times this task has been claimed. A task that
	// keeps being requeued is failing rather than merely slow, and delivering it
	// forever would burn budget on every instance that picks it up.
	Attempts int `json:"attempts"`
}

// TaskClaim is one instance's hold on a task.
type TaskClaim struct {
	// Allocation is the agent instance holding the task.
	Allocation string `json:"allocation"`
	// ExpiresAt is when the hold lapses and the task may be redelivered.
	ExpiresAt time.Time `json:"expires_at"`
}

// Queue is a node-local work queue agent instances pull from.
//
// It is durable for the same reason desired state is: an agent that claimed a
// task before a node restart must not silently lose it, and work that was
// enqueued must not disappear because a process exited. The node owns delivery;
// the control plane only ever observes depth.
//
// Claims are leased rather than permanent. An instance that dies holding a task
// would otherwise strand it forever, and the control plane cannot redeliver work
// it never sees the contents of.
type Queue struct {
	// Name identifies the queue.
	Name string
	// Workload is the agent workload authorized to pull from it. A queue serves
	// one workload so a scaling decision has an unambiguous subject.
	Workload string
	// MaxAttempts bounds redelivery. Zero means the package default.
	MaxAttempts int
	// Lease is how long a claim is held before it may be reclaimed.
	Lease time.Duration
	// Now is the clock, injectable so lease expiry is testable without sleeping.
	Now func() time.Time

	mu    sync.Mutex
	path  string
	tasks map[string]*QueueTask
	// order keeps delivery FIFO by enqueue time. A map alone would deliver
	// non-deterministically, which makes a stuck task impossible to diagnose.
	order []string
}

// DefaultMaxAttempts bounds how many times a task is redelivered before the
// queue stops offering it.
const DefaultMaxAttempts = 3

// OpenQueue loads or creates a durable queue.
func OpenQueue(path, name, workload string) (*Queue, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("queue path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create queue directory: %w", err)
	}
	queue := &Queue{
		Name: name, Workload: workload, Lease: DefaultClaimLease,
		MaxAttempts: DefaultMaxAttempts, Now: time.Now,
		path: path, tasks: make(map[string]*QueueTask),
	}
	if err := queue.load(); err != nil {
		return nil, err
	}
	return queue, nil
}

func (q *Queue) load() error {
	file, err := os.Open(q.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open queue: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			var task QueueTask
			if decodeErr := json.Unmarshal(line, &task); decodeErr != nil {
				return fmt.Errorf("decode queue task: %w", decodeErr)
			}
			if task.ID == "" {
				return fmt.Errorf("queue holds a task with no id")
			}
			if _, exists := q.tasks[task.ID]; !exists {
				q.order = append(q.order, task.ID)
			}
			stored := task
			q.tasks[task.ID] = &stored
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read queue: %w", err)
		}
	}
}

// persist writes the whole queue atomically, matching how desired state is
// stored: a partially written queue would lose or duplicate work.
func (q *Queue) persist() error {
	temporary := q.path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open queue temp: %w", err)
	}
	writer := bufio.NewWriter(file)
	for _, id := range q.order {
		task, ok := q.tasks[id]
		if !ok {
			continue
		}
		record, err := json.Marshal(task)
		if err != nil {
			file.Close()
			return fmt.Errorf("encode queue task: %w", err)
		}
		if _, err := writer.Write(append(record, '\n')); err != nil {
			file.Close()
			return fmt.Errorf("write queue: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		file.Close()
		return fmt.Errorf("flush queue: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync queue: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close queue temp: %w", err)
	}
	if err := os.Rename(temporary, q.path); err != nil {
		return fmt.Errorf("replace queue: %w", err)
	}
	return nil
}

// Enqueue adds work, ignoring a repeat of an id already present.
//
// Idempotency on the id is what lets a producer retry after an ambiguous
// failure without delivering the same work twice.
func (q *Queue) Enqueue(id, payload string) error {
	if id == "" {
		return fmt.Errorf("queue task requires an id")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.tasks[id]; exists {
		return nil
	}
	q.tasks[id] = &QueueTask{ID: id, Payload: payload, EnqueuedAt: q.now()}
	q.order = append(q.order, id)
	return q.persist()
}

// Claim hands the oldest available task to an instance.
//
// Availability accounts for lapsed leases: a task whose holder died is
// redelivered, which is the whole reason claims are leased. A task that has
// exhausted its attempts is left in place rather than delivered again, since
// work that fails every instance it touches would otherwise burn budget
// indefinitely.
func (q *Queue) Claim(allocation string) (QueueTask, bool, error) {
	if allocation == "" {
		return QueueTask{}, false, fmt.Errorf("claim requires an allocation")
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	now := q.now()
	for _, id := range q.order {
		task, ok := q.tasks[id]
		if !ok || !q.availableLocked(task, now) {
			continue
		}
		if task.Attempts >= q.maxAttempts() {
			continue
		}
		task.Attempts++
		task.Claim = &TaskClaim{Allocation: allocation, ExpiresAt: now.Add(q.lease())}
		if err := q.persist(); err != nil {
			return QueueTask{}, false, err
		}
		return *task, true, nil
	}
	return QueueTask{}, false, nil
}

// Ack removes a completed task.
//
// Only the holder may acknowledge. An instance acknowledging another's work
// would delete a task that is still running somewhere, and after a lease lapse
// the previous holder no longer speaks for it.
func (q *Queue) Ack(allocation, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	task, ok := q.tasks[id]
	if !ok {
		// Acknowledging an absent task is the expected result of a retry after
		// the first ack succeeded, so it stays idempotent.
		return nil
	}
	if task.Claim == nil || task.Claim.Allocation != allocation {
		return fmt.Errorf("allocation %q does not hold task %q", allocation, id)
	}
	delete(q.tasks, id)
	q.removeOrderLocked(id)
	return q.persist()
}

// Requeue returns a task to the waiting set, for an instance that cannot finish
// it. The attempt is already counted, so a task that keeps coming back
// eventually stops being delivered.
func (q *Queue) Requeue(allocation, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	task, ok := q.tasks[id]
	if !ok {
		return fmt.Errorf("task %q does not exist", id)
	}
	if task.Claim == nil || task.Claim.Allocation != allocation {
		return fmt.Errorf("allocation %q does not hold task %q", allocation, id)
	}
	task.Claim = nil
	return q.persist()
}

// ReleaseAllocation requeues everything an instance holds.
//
// A deleted or crashed instance must not strand its work until the lease
// lapses, and the node knows the instance is gone long before the queue would
// infer it.
func (q *Queue) ReleaseAllocation(allocation string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	changed := false
	for _, task := range q.tasks {
		if task.Claim != nil && task.Claim.Allocation == allocation {
			task.Claim = nil
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return q.persist()
}

// Depth reports waiting and in-flight counts.
//
// Waiting excludes tasks that exhausted their attempts: they are not work any
// worker can take, and counting them would scale up instances to chase tasks
// nobody will be given.
func (q *Queue) Depth() (waiting int, inFlight int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	for _, task := range q.tasks {
		switch {
		case q.availableLocked(task, now):
			if task.Attempts < q.maxAttempts() {
				waiting++
			}
		case task.Claim != nil:
			inFlight++
		}
	}
	return waiting, inFlight
}

// Stalled reports tasks that exhausted their attempts, which is what an
// operator needs to see rather than a queue that silently stops draining.
func (q *Queue) Stalled() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	var stalled []string
	for _, id := range q.order {
		task, ok := q.tasks[id]
		if ok && q.availableLocked(task, now) && task.Attempts >= q.maxAttempts() {
			stalled = append(stalled, id)
		}
	}
	sort.Strings(stalled)
	return stalled
}

// Evidence reports measured depth to the control plane.
//
// Depth is measured here rather than reported by an agent, because an agent
// describing its own backlog could scale its own replica count.
func (q *Queue) Evidence() control.Evidence {
	waiting, inFlight := q.Depth()
	observed := map[string]string{
		"depth": fmt.Sprint(waiting), "in_flight": fmt.Sprint(inFlight),
	}
	if stalled := q.Stalled(); len(stalled) > 0 {
		observed["stalled"] = fmt.Sprint(len(stalled))
	}
	return control.Evidence{
		Kind: control.EvidenceQueueObserved, Target: q.Name,
		ObservedAt: q.now(), Observed: observed,
	}
}

// QueueBroker joins a queue to the agent lifecycle.
//
// The queue knows about work and the agent capability knows about budgets,
// drains, and task slots. Neither should know about the other, so the broker
// owns the one rule that spans both: an instance may only take work it is
// permitted to take, and taking it must be visible to a drain.
type QueueBroker struct {
	Queue  *Queue
	Agents *Agents
}

// Claim hands an instance its next task, if it is allowed one.
//
// The authorization check happens before the queue is touched. Claiming first
// and validating after would consume an attempt on a task the instance was
// never eligible for, which for a task near its attempt limit means losing it
// to an instance that could not have run it.
func (b *QueueBroker) Claim(allocation string) (QueueTask, bool, error) {
	if b.Queue == nil || b.Agents == nil {
		return QueueTask{}, false, fmt.Errorf("queue broker is not initialized")
	}
	if err := b.Agents.MayClaim(allocation); err != nil {
		return QueueTask{}, false, err
	}
	task, claimed, err := b.Queue.Claim(allocation)
	if err != nil || !claimed {
		return QueueTask{}, false, err
	}
	// Recording the hold is what makes a drain observable. Without it the
	// control plane would stop an instance mid-task believing it idle.
	b.Agents.HoldTask(allocation, task.ID)
	return task, true, nil
}

// Ack completes a task and frees the instance's slot.
func (b *QueueBroker) Ack(allocation, id string) error {
	if b.Queue == nil || b.Agents == nil {
		return fmt.Errorf("queue broker is not initialized")
	}
	if err := b.Queue.Ack(allocation, id); err != nil {
		return err
	}
	b.Agents.ReleaseTask(allocation)
	return nil
}

// Requeue returns work an instance could not finish and frees its slot.
func (b *QueueBroker) Requeue(allocation, id string) error {
	if b.Queue == nil || b.Agents == nil {
		return fmt.Errorf("queue broker is not initialized")
	}
	if err := b.Queue.Requeue(allocation, id); err != nil {
		return err
	}
	b.Agents.ReleaseTask(allocation)
	return nil
}

// Release requeues everything an instance holds and clears its slot, for an
// allocation being deleted.
func (b *QueueBroker) Release(allocation string) error {
	if b.Queue == nil || b.Agents == nil {
		return nil
	}
	if err := b.Queue.ReleaseAllocation(allocation); err != nil {
		return err
	}
	b.Agents.ReleaseTask(allocation)
	return nil
}

// availableLocked reports whether a task may be handed out. The caller holds
// the lock.
func (q *Queue) availableLocked(task *QueueTask, now time.Time) bool {
	if task.Claim == nil {
		return true
	}
	// A lapsed lease means the holder is presumed gone.
	return !task.Claim.ExpiresAt.IsZero() && !now.Before(task.Claim.ExpiresAt)
}

func (q *Queue) removeOrderLocked(id string) {
	for i, existing := range q.order {
		if existing == id {
			q.order = append(q.order[:i], q.order[i+1:]...)
			return
		}
	}
}

func (q *Queue) lease() time.Duration {
	if q.Lease > 0 {
		return q.Lease
	}
	return DefaultClaimLease
}

func (q *Queue) maxAttempts() int {
	if q.MaxAttempts > 0 {
		return q.MaxAttempts
	}
	return DefaultMaxAttempts
}

func (q *Queue) now() time.Time {
	if q.Now == nil {
		return time.Now()
	}
	return q.Now()
}
