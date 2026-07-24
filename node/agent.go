package node

import (
	"context"
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

	mu sync.Mutex
	// envelopes records the tool grant installed for each allocation. The node
	// keeps this so a runtime cannot ask for a capability it was not granted:
	// the answer comes from here, not from what the agent claims.
	envelopes map[string][]control.ToolGrant
	// tasks records which queue task each instance currently holds, which is
	// what makes a drain observable rather than assumed.
	tasks map[string]string
}

// NewAgents builds the agent capability rooted at a directory.
func NewAgents(root string) *Agents {
	return &Agents{
		Root:      root,
		envelopes: make(map[string][]control.ToolGrant),
		tasks:     make(map[string]string),
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
	for _, grant := range a.envelopes[allocation] {
		if grant.Name == tool && grant.Scope == scope {
			return true
		}
	}
	return false
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
