package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// Crash-loop budget. A workload that keeps dying is a problem the control plane
// must see, not one the node should hide by restarting forever.
const (
	DefaultMaxRestarts   = 5
	DefaultRestartWindow = 10 * time.Minute
	DefaultBackoff       = 5 * time.Second
	DefaultMaxBackoff    = 2 * time.Minute
)

// Supervisor reconciles observed local state toward the node's last
// server-authorized desired state.
//
// This is what makes a node survivable rather than merely obedient. The server
// decides what should run; the node keeps that true while the server is
// unreachable. The node never invents new workloads, changes images, or expands
// its own authority: it can only restart something it was already told to run.
type Supervisor struct {
	Runtime *ContainerRuntime
	Desired *DesiredState
	// Agents reports agent spend, which the supervisor forwards as evidence. A
	// budget the control plane never hears about cannot bound anything above the
	// node, so metering has to reach the world view on the same schedule as
	// every other supervised fact.
	Agents *Agents
	// Providers measures egress to model providers. The scheduler refuses to
	// place an agent on a node that cannot reach its provider, so a node that
	// never reports reachability attracts no agent workloads at all.
	Providers *ProviderMonitor
	// Queues are the work queues this node serves. Depth drives agent scaling,
	// and it is measured here rather than reported by an agent, because an agent
	// describing its own backlog could scale its own replica count.
	Queues []*Queue
	// NodeID attributes node-scoped observations. Provider reachability is a
	// fact about this node rather than about any one allocation, so the evidence
	// has to say which node measured it.
	NodeID string
	// Evidence receives observations produced by supervision so they can be
	// forwarded to the server when it is reachable again.
	Evidence func(control.Evidence)
	Now      func() time.Time

	MaxRestarts   int
	RestartWindow time.Duration
	Backoff       time.Duration
	MaxBackoff    time.Duration

	mu      sync.Mutex
	history map[string]*restartHistory
}

type restartHistory struct {
	count       int
	windowStart time.Time
	nextAttempt time.Time
	trippedAt   time.Time
}

func NewSupervisor(runtime *ContainerRuntime, desired *DesiredState) *Supervisor {
	return &Supervisor{
		Runtime: runtime, Desired: desired, Now: time.Now,
		MaxRestarts: DefaultMaxRestarts, RestartWindow: DefaultRestartWindow,
		Backoff: DefaultBackoff, MaxBackoff: DefaultMaxBackoff,
		history: make(map[string]*restartHistory),
	}
}

// Reconcile performs one supervision pass and returns the evidence it observed.
// It is safe to call repeatedly and does nothing when everything already
// matches desired state.
func (s *Supervisor) Reconcile(ctx context.Context) ([]control.Evidence, error) {
	if s.Runtime == nil || s.Desired == nil {
		return nil, fmt.Errorf("supervisor is not initialized")
	}
	var observations []control.Evidence
	// Provider reachability is measured before allocations are reconciled, so a
	// node that has just lost egress reports that fact in the same round it
	// reports the agents which are about to stop being ready.
	observations = append(observations, s.refreshProviders(ctx)...)
	observations = append(observations, s.observeQueues()...)
	for _, entry := range s.Desired.List() {
		evidence, err := s.reconcileOne(ctx, entry)
		if err != nil {
			return observations, err
		}
		observations = append(observations, evidence...)
	}
	return observations, nil
}

// refreshProviders measures egress and attributes the result to this node.
func (s *Supervisor) refreshProviders(ctx context.Context) []control.Evidence {
	if s.Providers == nil {
		return nil
	}
	observations := s.Providers.Refresh(ctx)
	for i := range observations {
		observations[i].Source = "node-supervisor"
		if observations[i].Observed == nil {
			observations[i].Observed = map[string]string{}
		}
		// The projection keys this fact by node, and only the node knows which
		// one it is.
		observations[i].Observed["node"] = s.NodeID
		s.emit(observations[i])
	}
	return observations
}

func (s *Supervisor) reconcileOne(ctx context.Context, entry DesiredAllocation) ([]control.Evidence, error) {
	state, err := s.Runtime.Inspect(ctx, entry.ID)
	if err != nil {
		return nil, fmt.Errorf("inspect %q: %w", entry.ID, err)
	}
	// Spend is reported before any restart decision and regardless of whether
	// the allocation is meant to be running. An agent that exhausted its budget
	// and stopped is exactly the case the control plane needs to hear about, and
	// returning early on a stopped entry would withhold it.
	var observations []control.Evidence
	if spend, ok := s.spendEvidence(entry.ID); ok {
		observations = append(observations, spend)
		s.emit(spend)
	}
	if !entry.Running {
		// The server asked for this to be stopped. The node does not restart it,
		// and does not delete it either: deletion is a separate authorized action.
		return observations, nil
	}
	if state.Running {
		return observations, nil
	}
	if !state.Exists {
		// The container is gone entirely. Recreating it would require pulling and
		// building a spec, which is the control plane's job, so the node reports
		// the fact and waits rather than inventing a replacement.
		return append(observations, s.failure(entry, state, "container is absent")), nil
	}

	now := s.now()
	// An agent that spent its ceiling did not crash; it finished. Restarting it
	// would burn a fresh ceiling to reach the same exhausted state, so the node
	// reports the fact and leaves replacement to the control plane.
	if s.Agents != nil && s.Agents.Exhausted(entry.ID) {
		return append(observations, s.failure(entry, state, "agent budget exhausted")), nil
	}
	if !s.allowRestart(entry.ID, now) {
		// Crash-loop budget exhausted. Report and stop trying so the control
		// plane can decide, instead of hiding a broken workload behind restarts.
		return append(observations, s.failure(entry, state, "restart budget exhausted")), nil
	}

	if _, err := s.Runtime.backend.Start(ctx, entry.ID, entry.ID+".log"); err != nil {
		return append(observations, s.failure(entry, state, "restart failed: "+err.Error())), nil
	}
	if err := s.Desired.recordRestart(entry.ID); err != nil {
		return observations, err
	}
	restarted := control.Evidence{
		Kind: control.EvidenceAllocationRunning, Target: entry.ID,
		Source: "node-supervisor", ObservedAt: now,
		Observed: map[string]string{
			"restarted": "true",
			"exit_code": fmt.Sprintf("%d", state.ExitCode),
		},
	}
	s.emit(restarted)
	return append(observations, restarted), nil
}

// observeQueues measures the depth of every queue this node serves.
//
// Depth is the most perishable fact the scheduler consumes, because the workers
// are draining it as it is read. Measuring on the supervision tick keeps the
// observation as fresh as the control plane's own cycle.
func (s *Supervisor) observeQueues() []control.Evidence {
	observations := make([]control.Evidence, 0, len(s.Queues))
	for _, queue := range s.Queues {
		if queue == nil {
			continue
		}
		evidence := queue.Evidence()
		evidence.Source = "node-supervisor"
		s.emit(evidence)
		observations = append(observations, evidence)
	}
	return observations
}

// spendEvidence reports an agent allocation's consumption, if it has any.
func (s *Supervisor) spendEvidence(allocation string) (control.Evidence, bool) {
	if s.Agents == nil {
		return control.Evidence{}, false
	}
	evidence, ok := s.Agents.SpendEvidence(allocation)
	if !ok {
		return control.Evidence{}, false
	}
	evidence.Source = "node-supervisor"
	evidence.ObservedAt = s.now()
	return evidence, true
}

func (s *Supervisor) failure(entry DesiredAllocation, state BackendState, reason string) control.Evidence {
	evidence := control.Evidence{
		Kind: control.EvidenceAllocationFailed, Target: entry.ID,
		Source: "node-supervisor", ObservedAt: s.now(),
		Observed: map[string]string{
			"exit_code": fmt.Sprintf("%d", state.ExitCode),
			"restarts":  fmt.Sprintf("%d", entry.Restarts),
			"reason":    reason,
		},
	}
	s.emit(evidence)
	return evidence
}

func (s *Supervisor) emit(evidence control.Evidence) {
	if s.Evidence != nil {
		s.Evidence(evidence)
	}
}

// allowRestart applies the crash-loop budget and backoff.
func (s *Supervisor) allowRestart(id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	window := s.RestartWindow
	if window <= 0 {
		window = DefaultRestartWindow
	}
	maxRestarts := s.MaxRestarts
	if maxRestarts <= 0 {
		maxRestarts = DefaultMaxRestarts
	}

	entry, ok := s.history[id]
	if !ok {
		s.history[id] = &restartHistory{count: 1, windowStart: now, nextAttempt: now.Add(s.backoff(1))}
		return true
	}
	// A workload that has been stable for a full window earns a fresh budget.
	if now.Sub(entry.windowStart) > window {
		entry.count = 1
		entry.windowStart = now
		entry.trippedAt = time.Time{}
		entry.nextAttempt = now.Add(s.backoff(1))
		return true
	}
	if entry.count >= maxRestarts {
		if entry.trippedAt.IsZero() {
			entry.trippedAt = now
		}
		return false
	}
	if now.Before(entry.nextAttempt) {
		return false
	}
	entry.count++
	entry.nextAttempt = now.Add(s.backoff(entry.count))
	return true
}

// backoff grows exponentially so a crashing workload is retried with
// decreasing frequency rather than in a tight loop. A zero Backoff disables
// waiting entirely, which keeps the budget itself testable in isolation.
func (s *Supervisor) backoff(attempt int) time.Duration {
	base := s.Backoff
	if base < 0 {
		base = DefaultBackoff
	}
	if base == 0 {
		return 0
	}
	maximum := s.MaxBackoff
	if maximum <= 0 {
		maximum = DefaultMaxBackoff
	}
	delay := base
	for i := 1; i < attempt && delay < maximum; i++ {
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	return delay
}

func (s *Supervisor) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// Orphans reports a4s-managed containers on this node that desired state does
// not know about. They are the residue of a crash between a host mutation and
// the record of it, and they must be surfaced rather than silently deleted:
// deletion is an authorized action, not a cleanup detail.
func (s *Supervisor) Orphans(ctx context.Context) ([]string, error) {
	lister, ok := s.Runtime.backend.(interface {
		ListManaged(context.Context) ([]string, error)
	})
	if !ok {
		return nil, fmt.Errorf("runtime backend cannot enumerate managed containers")
	}
	managed, err := lister.ListManaged(ctx)
	if err != nil {
		return nil, fmt.Errorf("list managed containers: %w", err)
	}
	var orphans []string
	for _, id := range managed {
		if _, known := s.Desired.Get(id); !known {
			orphans = append(orphans, id)
		}
	}
	return orphans, nil
}
