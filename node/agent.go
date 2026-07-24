package node

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/arcbjorn/a4s/control"
)

// Agents is the node capability that bounds agent workloads.
//
// An agent workload runs in an ordinary container, so containerd already
// provides lifecycle, resource limits, and the hardening baseline. What
// containerd does not provide is the isolation that actually matters for
// agents: one instance's conversation, tool results, and credentials must never
// appear in another's context.
//
// That leak does not happen through a shared kernel namespace. It happens
// through shared state in a runtime: a provider client with a cached
// conversation, a shared scratch directory, one credential used by every
// instance. So this capability keeps per-instance state strictly per-instance
// and hands the runtime nothing that belongs to another allocation.
type Agents struct {
	// Root is where per-instance workspaces are created, one directory per
	// allocation. Nothing is ever shared between them.
	Root string

	// Providers reports which model providers this node can currently reach. An
	// agent readiness probe consults it rather than dialling the provider on
	// every measurement, because reachability changes on the scale of network
	// events, not probe intervals.
	Providers ProviderReach

	mu sync.Mutex
	// envelopes records the tool grant installed for each allocation. The node
	// keeps this so a runtime cannot ask for a capability it was not granted:
	// the answer comes from here, not from what the agent claims.
	envelopes map[string][]control.ToolGrant
	// tasks records which queue task each instance currently holds, which is
	// what makes a drain observable rather than assumed.
	tasks map[string]string
	// meters records each instance's ceiling and what it has consumed. The node
	// holds this because the controller is too far away to stop a runaway loop:
	// a round trip through evidence, projection, and a proposal takes longer
	// than an agent needs to spend the rest of its budget.
	meters map[string]*meter
}

// meter is one instance's budget ceiling and consumption.
type meter struct {
	budget control.Budget
	spent  control.Budget
	// refusals counts tool calls denied for want of a grant. It is reported as
	// evidence because an agent repeatedly reaching for a capability it does not
	// have is a fact an operator should see, whether it means a misconfigured
	// envelope or an agent doing something unintended.
	refusals int
}

// ProviderReach reports whether a model provider is reachable from this node.
//
// It is an interface so reachability can come from a real dial, a cached
// health check, or a test double. The node treats the answer as an observed
// fact either way.
type ProviderReach interface {
	Reachable(provider string) bool
}

// StaticProviders is a fixed reachability set, used where a node's egress is
// configured rather than discovered.
type StaticProviders map[string]bool

func (s StaticProviders) Reachable(provider string) bool { return s[provider] }

// NewAgents builds the agent capability rooted at a directory.
func NewAgents(root string) *Agents {
	return &Agents{
		Root:      root,
		envelopes: make(map[string][]control.ToolGrant),
		tasks:     make(map[string]string),
		meters:    make(map[string]*meter),
	}
}

func (a *Agents) Execute(ctx context.Context, action control.Action) (control.Evidence, error) {
	switch action.Kind {
	case control.ActionGrantTools:
		return a.grant(action)
	case control.ActionDrainAllocation:
		return a.drain(action)
	default:
		return control.Evidence{}, fmt.Errorf("agent capability cannot perform %q", action.Kind)
	}
}

// grant installs an allocation's tool envelope.
//
// The envelope is stored per allocation and never merged with another's. An
// instance's capabilities are exactly what the kernel authorized for it, which
// is what keeps one agent from reaching another's scope.
func (a *Agents) grant(action control.Action) (control.Evidence, error) {
	if action.Target == "" {
		return control.Evidence{}, fmt.Errorf("tool grant requires an allocation")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.envelopes == nil {
		a.envelopes = make(map[string][]control.ToolGrant)
	}
	// Re-granting the same envelope is an idempotent repeat of an authorized
	// action. Granting a different one is not: it would widen a blast radius the
	// kernel approved against the envelope it saw.
	if existing, ok := a.envelopes[action.Target]; ok && !sameEnvelope(existing, action.Tools) {
		return control.Evidence{}, fmt.Errorf(
			"allocation %q already holds a different tool envelope", action.Target)
	}
	a.envelopes[action.Target] = append([]control.ToolGrant(nil), action.Tools...)
	return control.Evidence{
		Kind: control.EvidenceToolsGranted, Target: action.Target,
		Observed: map[string]string{"count": fmt.Sprint(len(action.Tools))},
	}, nil
}

// drain tells an instance to stop accepting work and finish what it holds.
//
// The evidence distinguishes the two states deliberately. An instance still
// holding a task reports draining; only an instance that has released its task
// reports drained. The controller refuses to stop an agent on the former, so a
// drain that never completes stalls the rollout instead of silently discarding
// a task mid-flight.
func (a *Agents) drain(action control.Action) (control.Evidence, error) {
	if action.Target == "" {
		return control.Evidence{}, fmt.Errorf("drain requires an allocation")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	task := a.tasks[action.Target]
	if task != "" {
		return control.Evidence{
			Kind: control.EvidenceAgentDraining, Target: action.Target,
			Observed: map[string]string{"draining": "true", "task": task},
		}, nil
	}
	return control.Evidence{
		Kind: control.EvidenceAllocationDrained, Target: action.Target,
		Observed: map[string]string{"drained": "true"},
	}, nil
}

// Reserve records the ceiling an allocation may spend against.
//
// The node learns the ceiling from the authorized action rather than from the
// runtime, so an agent cannot raise its own limit by reporting a larger one.
func (a *Agents) Reserve(allocation string, budget control.Budget) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.meters == nil {
		a.meters = make(map[string]*meter)
	}
	existing, ok := a.meters[allocation]
	if !ok {
		a.meters[allocation] = &meter{budget: budget}
		return
	}
	// Re-reserving is the replay of an authorized create. Keep whatever has
	// already been spent, or a restarted dispatch would zero the meter and hand
	// a spent agent a fresh ceiling.
	existing.budget = budget
}

// Spend records consumption reported by a runtime and reports whether the
// instance may continue.
//
// Consumption is additive and never trusted to decrease: a runtime that
// reported less than before would otherwise buy itself more room. The returned
// bool is the local kill switch, which is the only enforcement fast enough to
// matter.
func (a *Agents) Spend(allocation string, delta control.Budget) (control.Budget, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	record, ok := a.meters[allocation]
	if !ok {
		// An instance with no reserved ceiling has no authorization to spend.
		return control.Budget{}, false
	}
	// Negative deltas would let a runtime claw back spend it already reported.
	record.spent = record.spent.Add(clampBudget(delta))
	return record.spent, !record.spent.Exhausts(record.budget)
}

// Spent reports an instance's consumption and whether it is within its ceiling.
func (a *Agents) Spent(allocation string) (control.Budget, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	record, ok := a.meters[allocation]
	if !ok {
		return control.Budget{}, false
	}
	return record.spent, true
}

// Exhausted reports whether an instance has consumed its ceiling.
func (a *Agents) Exhausted(allocation string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	record, ok := a.meters[allocation]
	if !ok {
		return false
	}
	return record.spent.Exhausts(record.budget)
}

// SpendEvidence reports an instance's consumption to the control plane.
//
// The projection treats spend as monotonic, so this reports the running total
// rather than a delta. A lost or reordered report then costs accuracy, never
// correctness.
func (a *Agents) SpendEvidence(allocation string) (control.Evidence, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	record, ok := a.meters[allocation]
	if !ok {
		return control.Evidence{}, false
	}
	observed := map[string]string{
		"tokens":       fmt.Sprint(record.spent.Tokens),
		"cost_millis":  fmt.Sprint(record.spent.CostMillis),
		"wall_seconds": fmt.Sprint(record.spent.WallSeconds),
		"tool_calls":   fmt.Sprint(record.spent.ToolCalls),
		"exhausted":    fmt.Sprint(record.spent.Exhausts(record.budget)),
	}
	if record.refusals > 0 {
		observed["tool_refusals"] = fmt.Sprint(record.refusals)
	}
	return control.Evidence{
		Kind: control.EvidenceAgentSpent, Target: allocation,
		Observed: observed,
	}, true
}

// clampBudget floors every dimension at zero.
func clampBudget(b control.Budget) control.Budget {
	if b.Tokens < 0 {
		b.Tokens = 0
	}
	if b.CostMillis < 0 {
		b.CostMillis = 0
	}
	if b.WallSeconds < 0 {
		b.WallSeconds = 0
	}
	if b.ToolCalls < 0 {
		b.ToolCalls = 0
	}
	return b
}

// Tools reports the envelope installed for an allocation.
//
// A runtime asks the node what it may call rather than deciding for itself.
// This is the enforcement point for the grant: an agent that requests a tool
// absent from its own envelope is refused here, with no path to another
// allocation's grants.
func (a *Agents) Tools(allocation string) []control.ToolGrant {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]control.ToolGrant(nil), a.envelopes[allocation]...)
}

// Allows reports whether an allocation may invoke a tool at a given scope.
func (a *Agents) Allows(allocation, tool, scope string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.allowsLocked(allocation, tool, scope)
}

func (a *Agents) allowsLocked(allocation, tool, scope string) bool {
	for _, grant := range a.envelopes[allocation] {
		if grant.Name == tool && grant.Scope == scope {
			return true
		}
	}
	return false
}

// ErrToolNotGranted is returned when an agent reaches for a capability outside
// its envelope. It is a distinct error so a caller can tell an authorization
// refusal from a tool that failed on its own terms.
var ErrToolNotGranted = errors.New("tool is not in the allocation's grant envelope")

// ErrBudgetExhausted is returned when an instance has spent its ceiling.
var ErrBudgetExhausted = errors.New("allocation has exhausted its budget")

// AuthorizeToolCall is the gate a runtime passes before invoking a tool.
//
// This is where the envelope stops being a declaration and becomes an
// enforcement point. It is deliberately the node's decision rather than the
// runtime's: an agent that could decide its own authorization would make the
// grant advisory.
//
// A successful call charges the tool-call budget, so the ceiling bounds
// behavior even when an agent stays cheap in tokens. An agent thrashing between
// two granted tools stays under every other limit indefinitely; this is what
// stops it.
func (a *Agents) AuthorizeToolCall(allocation, tool, scope string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	record, ok := a.meters[allocation]
	if !ok {
		return fmt.Errorf("allocation %q holds no budget reservation", allocation)
	}
	// An exhausted instance may not act, whatever its envelope says. Checking
	// this first means a spent agent cannot keep calling free tools.
	if record.spent.Exhausts(record.budget) {
		return ErrBudgetExhausted
	}
	if !a.allowsLocked(allocation, tool, scope) {
		// A refusal is counted rather than silently dropped. An agent reaching
		// repeatedly for a capability it lacks is a fact worth surfacing.
		record.refusals++
		return fmt.Errorf("allocation %q calling %q at scope %q: %w",
			allocation, tool, scope, ErrToolNotGranted)
	}
	// A ceiling of N means N calls are permitted and the next is refused. The
	// charge lands before the return, so an instance that has used its last call
	// reads as exhausted immediately rather than only after attempting one more.
	record.spent.ToolCalls++
	return nil
}

// ToolRefusals reports how many calls an instance made outside its envelope.
func (a *Agents) ToolRefusals(allocation string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	record, ok := a.meters[allocation]
	if !ok {
		return 0
	}
	return record.refusals
}

// HoldTask records that an instance picked up work, and ReleaseTask that it
// finished. Both come from the runtime's own report of its task slot, which is
// what a drain waits on.
func (a *Agents) HoldTask(allocation, task string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tasks == nil {
		a.tasks = make(map[string]string)
	}
	a.tasks[allocation] = task
}

func (a *Agents) ReleaseTask(allocation string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tasks, allocation)
}

// Release forgets everything held for an allocation.
//
// A deleted agent must not leave its envelope behind on the node. A later
// allocation could otherwise reuse the identifier and inherit capabilities
// nobody granted it.
func (a *Agents) Release(allocation string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.envelopes, allocation)
	delete(a.tasks, allocation)
	delete(a.meters, allocation)
}

// ObserveReadiness implements the readiness observer for agent probes.
//
// An agent is ready only when it can reach its provider with budget remaining.
// A process probe would pass for an agent whose provider is unreachable or
// whose ceiling is spent, both of which mean no work can be done despite a
// container that looks perfectly healthy.
func (a *Agents) ObserveReadiness(target control.ProbeTarget) (bool, map[string]string, error) {
	if target.Kind != control.ProbeAgent {
		return false, nil, fmt.Errorf("agent capability only serves agent probes")
	}
	if target.Provider == "" {
		return false, nil, fmt.Errorf("agent probe for %q names no provider", target.Allocation)
	}
	observed := map[string]string{"probe": control.ProbeAgent, "provider": target.Provider}

	a.mu.Lock()
	record, metered := a.meters[target.Allocation]
	var spent, budget control.Budget
	if metered {
		spent, budget = record.spent, record.budget
	}
	a.mu.Unlock()

	if !metered {
		// An instance the node holds no reservation for was never prepared here.
		// Reporting it ready would assert something this node cannot know.
		observed["reason"] = "no budget reservation on this node"
		return false, observed, nil
	}
	observed["tokens_spent"] = fmt.Sprint(spent.Tokens)
	observed["cost_millis_spent"] = fmt.Sprint(spent.CostMillis)
	if spent.Exhausts(budget) {
		observed["reason"] = "budget exhausted"
		return false, observed, nil
	}
	if a.Providers == nil {
		// Without a reachability source the node cannot establish the fact the
		// probe exists to establish. Absence of evidence, not evidence of health.
		return false, observed, fmt.Errorf("node has no provider reachability source")
	}
	if !a.Providers.Reachable(target.Provider) {
		observed["reason"] = "provider unreachable"
		return false, observed, nil
	}
	return true, observed, nil
}

// sameEnvelope compares two grant sets independently of order.
func sameEnvelope(a, b []control.ToolGrant) bool {
	if len(a) != len(b) {
		return false
	}
	left := append([]control.ToolGrant(nil), a...)
	right := append([]control.ToolGrant(nil), b...)
	less := func(grants []control.ToolGrant) func(i, j int) bool {
		return func(i, j int) bool {
			if grants[i].Name != grants[j].Name {
				return grants[i].Name < grants[j].Name
			}
			return grants[i].Scope < grants[j].Scope
		}
	}
	sort.Slice(left, less(left))
	sort.Slice(right, less(right))
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
