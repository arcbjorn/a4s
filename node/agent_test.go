package node

import (
	"context"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

func grantAction(allocation string, tools ...control.ToolGrant) control.Action {
	return control.Action{
		Kind: control.ActionGrantTools, Target: allocation, Tools: tools,
	}
}

// An agent's capabilities are exactly what the kernel authorized for it. One
// instance must never be able to reach another's scope.
func TestToolEnvelopesAreIsolatedPerAllocation(t *testing.T) {
	agents := NewAgents(t.TempDir())
	ctx := context.Background()

	readOnly := control.ToolGrant{Name: "repo.read", Scope: "org/a"}
	writable := control.ToolGrant{Name: "repo.write", Scope: "org/b", Mutating: true}
	if _, err := agents.Execute(ctx, grantAction("triage-0", readOnly)); err != nil {
		t.Fatalf("grant failed: %v", err)
	}
	if _, err := agents.Execute(ctx, grantAction("triage-1", writable)); err != nil {
		t.Fatalf("grant failed: %v", err)
	}

	if !agents.Allows("triage-0", "repo.read", "org/a") {
		t.Fatal("expected triage-0 to hold its own grant")
	}
	// The isolation that matters: one instance's envelope is not the other's.
	if agents.Allows("triage-0", "repo.write", "org/b") {
		t.Fatal("triage-0 reached triage-1's tool grant")
	}
	if agents.Allows("triage-1", "repo.read", "org/a") {
		t.Fatal("triage-1 reached triage-0's tool grant")
	}
}

// A tool granted at one scope must not be usable at another, or the scope would
// describe nothing.
func TestToolGrantDoesNotCrossScopes(t *testing.T) {
	agents := NewAgents(t.TempDir())
	if _, err := agents.Execute(context.Background(),
		grantAction("triage-0", control.ToolGrant{Name: "repo.read", Scope: "org/a"})); err != nil {
		t.Fatalf("grant failed: %v", err)
	}
	if agents.Allows("triage-0", "repo.read", "org/other") {
		t.Fatal("expected scope to bound the grant")
	}
}

// Re-granting the same envelope is an idempotent retry; granting a different one
// would widen a blast radius the kernel approved against what it saw.
func TestRegrantingDifferentEnvelopeIsRefused(t *testing.T) {
	agents := NewAgents(t.TempDir())
	ctx := context.Background()
	original := control.ToolGrant{Name: "repo.read", Scope: "org/a"}
	if _, err := agents.Execute(ctx, grantAction("triage-0", original)); err != nil {
		t.Fatalf("grant failed: %v", err)
	}
	// A replayed envelope must be accepted, since the node cannot distinguish a
	// retry from the original.
	if _, err := agents.Execute(ctx, grantAction("triage-0", original)); err != nil {
		t.Fatalf("expected identical re-grant to be idempotent, got %v", err)
	}
	_, err := agents.Execute(ctx, grantAction("triage-0",
		control.ToolGrant{Name: "shell.exec", Scope: "/", Mutating: true}))
	if err == nil || !strings.Contains(err.Error(), "different tool envelope") {
		t.Fatalf("expected widened envelope to be refused, got %v", err)
	}
}

// An instance still holding a task reports draining, not drained, so the
// controller refuses to stop it and the task is not discarded mid-flight.
func TestDrainReportsWorkStillHeld(t *testing.T) {
	agents := NewAgents(t.TempDir())
	ctx := context.Background()
	agents.HoldTask("triage-0", "task-7")

	evidence, err := agents.Execute(ctx, control.Action{
		Kind: control.ActionDrainAllocation, Target: "triage-0",
	})
	if err != nil {
		t.Fatalf("drain failed: %v", err)
	}
	if evidence.Kind != control.EvidenceAgentDraining {
		t.Fatalf("expected draining evidence while a task is held, got %q", evidence.Kind)
	}
	if evidence.Observed["task"] != "task-7" {
		t.Fatalf("expected the held task to be reported, got %q", evidence.Observed["task"])
	}

	agents.ReleaseTask("triage-0")
	evidence, err = agents.Execute(ctx, control.Action{
		Kind: control.ActionDrainAllocation, Target: "triage-0",
	})
	if err != nil {
		t.Fatalf("drain failed: %v", err)
	}
	if evidence.Kind != control.EvidenceAllocationDrained {
		t.Fatalf("expected drained evidence once the task is released, got %q", evidence.Kind)
	}
}

// A later allocation reusing an identifier must not inherit capabilities nobody
// granted it.
func TestReleaseClearsToolEnvelope(t *testing.T) {
	agents := NewAgents(t.TempDir())
	if _, err := agents.Execute(context.Background(),
		grantAction("triage-0", control.ToolGrant{Name: "repo.read", Scope: "org/a"})); err != nil {
		t.Fatalf("grant failed: %v", err)
	}
	agents.Release("triage-0")
	if agents.Allows("triage-0", "repo.read", "org/a") {
		t.Fatal("expected release to clear the envelope")
	}
	if len(agents.Tools("triage-0")) != 0 {
		t.Fatal("expected no residual grants after release")
	}
}

// The agent capability performs only the two actions it owns. A node capability
// that accepted anything else would be a general executor.
func TestAgentCapabilityRefusesForeignActions(t *testing.T) {
	agents := NewAgents(t.TempDir())
	_, err := agents.Execute(context.Background(), control.Action{
		Kind: control.ActionDeleteAllocation, Target: "triage-0",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot perform") {
		t.Fatalf("expected foreign action to be refused, got %v", err)
	}
}

// A node without the agent capability must refuse agent actions rather than
// silently ignoring them.
func TestCompositeRuntimeRefusesAgentActionsWithoutCapability(t *testing.T) {
	runtime := &CompositeRuntime{}
	_, err := runtime.Execute(context.Background(), control.Action{
		Kind: control.ActionGrantTools, Target: "triage-0",
	})
	if err == nil || !strings.Contains(err.Error(), "no agent capability") {
		t.Fatalf("expected missing capability to be reported, got %v", err)
	}
}
