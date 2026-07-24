package control

import (
	"strings"
	"testing"
	"time"
)

// Two proposals built against the same revision are both non-stale, so revision
// binding alone cannot separate them. The lease must.
func TestLeaseExcludesConcurrentProposalOnSameTarget(t *testing.T) {
	manager := NewLeaseManager()
	if _, err := manager.Acquire("web-public", "placement-r0", []string{"web-0"}); err != nil {
		t.Fatal(err)
	}
	_, err := manager.Acquire("web-public", "rollout-r0", []string{"web-0"})
	if err == nil || !strings.Contains(err.Error(), "is leased by proposal") {
		t.Fatalf("expected lease conflict, got %v", err)
	}
}

// Acquisition is all-or-nothing. A proposal that grabbed part of its plan could
// interleave with another holding the rest.
func TestLeaseAcquisitionIsAllOrNothing(t *testing.T) {
	manager := NewLeaseManager()
	if _, err := manager.Acquire("web-public", "first", []string{"web-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire("web-public", "second", []string{"web-0", "web-1"}); err == nil {
		t.Fatal("expected conflict on the contended target")
	}
	// The uncontended target must remain free, not half-claimed by the failure.
	if _, held := manager.Holder("web-0"); held {
		t.Fatal("failed acquisition claimed an uncontended target")
	}
}

// Releasing frees every target the lease covered, so the next proposal can run
// immediately instead of waiting out the TTL.
func TestReleaseFreesEveryTarget(t *testing.T) {
	manager := NewLeaseManager()
	leaseID, err := manager.Acquire("web-public", "first", []string{"web-0", "web-1"})
	if err != nil {
		t.Fatal(err)
	}
	manager.Release(leaseID)
	if _, err := manager.Acquire("web-public", "second", []string{"web-0", "web-1"}); err != nil {
		t.Fatalf("targets were not released: %v", err)
	}
}

// A holder that dies mid-execution must not block its targets forever.
func TestExpiredLeaseIsReclaimed(t *testing.T) {
	now := time.Unix(5000, 0).UTC()
	manager := NewLeaseManager().WithTTL(time.Minute).WithClock(func() time.Time { return now })
	if _, err := manager.Acquire("web-public", "abandoned", []string{"web-0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire("web-public", "next", []string{"web-0"}); err == nil {
		t.Fatal("a live lease should still exclude")
	}

	now = now.Add(2 * time.Minute)
	if _, err := manager.Acquire("web-public", "next", []string{"web-0"}); err != nil {
		t.Fatalf("expired lease was not reclaimed: %v", err)
	}
}

// Re-acquiring one's own lease is a retry, not a conflict, or a controller
// could deadlock against itself after a transient failure.
func TestReacquiringOwnLeaseSucceeds(t *testing.T) {
	manager := NewLeaseManager()
	if _, err := manager.Acquire("web-public", "placement-r0", []string{"web-0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire("web-public", "placement-r0", []string{"web-0"}); err != nil {
		t.Fatalf("a proposal could not re-acquire its own lease: %v", err)
	}
}

// Only mutating targets need exclusivity. Pulling an image touches a node's
// content store, not something another proposal contends for.
func TestLeaseTargetsCoverOnlyMutatingActions(t *testing.T) {
	proposal := Proposal{Actions: []Action{
		{ID: "pull", Kind: ActionPullImage, Target: testImage},
		{ID: "create", Kind: ActionCreateAllocation, Target: "web-0"},
		{ID: "start", Kind: ActionStartAllocation, Target: "web-0"},
		{ID: "route", Kind: ActionPublishRoute, Target: "web.example.com"},
	}}
	targets := LeaseTargets(proposal)
	if len(targets) != 2 || targets[0] != "web-0" || targets[1] != "web.example.com" {
		t.Fatalf("unexpected lease targets: %+v", targets)
	}
}

// The engine must deny a proposal whose targets are already leased, rather than
// executing it and racing the holder.
func TestEngineDeniesProposalWhenTargetIsLeased(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	executor := NewMemoryExecutor(scenario.World)
	engine := NewEngine(executor, PlacementAgent{}, NetworkAgent{})

	// Another controller already holds the allocation this goal needs.
	if _, err := engine.Leases.Acquire("other-goal", "other-proposal", []string{"app-0"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(scenario.Goal, 3); err == nil {
		t.Fatal("engine executed against a leased target")
	}

	denied := false
	for _, event := range engine.Events {
		if event.Type == EventProposalDenied && strings.Contains(event.Message, "is leased by proposal") {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("expected a lease denial event: %+v", engine.Events)
	}
	if len(executor.World().Allocations) != 0 {
		t.Fatal("engine mutated a leased target")
	}
}

// A completed proposal must not strand its targets, or the next reconciliation
// round would deny itself.
func TestEngineReleasesLeasesAfterExecution(t *testing.T) {
	scenario := validScenario()
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(NewMemoryExecutor(scenario.World), PlacementAgent{}, NetworkAgent{})
	if err := engine.Run(scenario.Goal, 5); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"app-0", "app.example.com"} {
		if lease, held := engine.Leases.Holder(target); held {
			t.Fatalf("target %q is still leased after convergence: %+v", target, lease)
		}
	}
}
