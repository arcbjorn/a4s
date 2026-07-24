package control

import (
	"strings"
	"testing"
	"time"
)

func agentRuntime() *AgentRuntime {
	return &AgentRuntime{
		Name: "a4s.agent/v1", Provider: "anthropic", Model: "claude-opus-5",
		Budget: Budget{Tokens: 100000, CostMillis: 5000, WallSeconds: 900, ToolCalls: 50},
		Tools: []ToolGrant{
			{Name: "repo.read", Scope: "github.com/arcbjorn/a4s"},
		},
	}
}

func agentGoal() Goal {
	goal := validScenario().Goal
	goal.Route = nil
	goal.Workload.Name = "triage"
	goal.Workload.Replicas = 1
	// An agent reaches the world through granted tools, not an inbound port.
	goal.Workload.Port = 0
	goal.Workload.Runtime = agentRuntime()
	return goal
}

func agentWorld(t *testing.T) World {
	t.Helper()
	world := cloneWorld(validScenario().World)
	world.normalize()
	// Reachability is perishable, so the fixture pins an evaluation time and
	// gives every measurement a live expiry. A world without one would read as
	// having no current provider evidence at all.
	world.ObservedAt = time.Unix(1_700_000_000, 0).UTC()
	for _, node := range world.Nodes {
		node.Providers = map[string]ProviderReach{
			"anthropic": reachableNow(world.ObservedAt),
		}
		node.BudgetCapacity = Budget{
			Tokens: 1000000, CostMillis: 50000, WallSeconds: 9000, ToolCalls: 500,
		}
	}
	return world
}

// reachableNow is a live reachability measurement taken at the given time.
func reachableNow(at time.Time) ProviderReach {
	return ProviderReach{
		Reachable: true, ObservedAt: at, ExpiresAt: at.Add(2 * time.Minute),
	}
}

func agentScenario(t *testing.T) Scenario {
	t.Helper()
	return Scenario{Goal: agentGoal(), World: agentWorld(t)}
}

// An agent workload is a workload kind, not a control-plane agent. Declaring a
// runtime is what makes the kernel treat its budget and tools as real.
func TestAgentWorkloadValidates(t *testing.T) {
	scenario := agentScenario(t)
	if err := scenario.NormalizeAndValidate(); err != nil {
		t.Fatalf("valid agent workload rejected: %v", err)
	}
}

// A forgotten ceiling must not be the case that grants unlimited spend.
func TestAgentWithoutBudgetIsRefused(t *testing.T) {
	for _, missing := range []struct {
		name   string
		mutate func(*Budget)
		want   string
	}{
		{"tokens", func(b *Budget) { b.Tokens = 0 }, "token ceiling"},
		{"cost", func(b *Budget) { b.CostMillis = 0 }, "cost ceiling"},
		{"wall clock", func(b *Budget) { b.WallSeconds = 0 }, "wall-clock ceiling"},
		{"tool calls", func(b *Budget) { b.ToolCalls = 0 }, "tool-call ceiling"},
	} {
		t.Run(missing.name, func(t *testing.T) {
			scenario := agentScenario(t)
			missing.mutate(&scenario.Goal.Workload.Runtime.Budget)
			err := scenario.NormalizeAndValidate()
			if err == nil || !strings.Contains(err.Error(), missing.want) {
				t.Fatalf("expected missing %s ceiling to be refused, got %v", missing.name, err)
			}
		})
	}
}

// An unpinned model changes the workload's behavior without a goal change, the
// same hazard as a floating image tag.
func TestAgentWithoutPinnedModelIsRefused(t *testing.T) {
	scenario := agentScenario(t)
	scenario.Goal.Workload.Runtime.Model = ""
	err := scenario.NormalizeAndValidate()
	if err == nil || !strings.Contains(err.Error(), "model must be pinned") {
		t.Fatalf("expected unpinned model to be refused, got %v", err)
	}
}

// An unscoped tool is granted whatever its credential allows, which makes the
// declared envelope a description of nothing.
func TestUnscopedToolGrantIsRefused(t *testing.T) {
	scenario := agentScenario(t)
	scenario.Goal.Workload.Runtime.Tools = []ToolGrant{{Name: "repo.write"}}
	err := scenario.NormalizeAndValidate()
	if err == nil || !strings.Contains(err.Error(), "must declare a scope") {
		t.Fatalf("expected unscoped tool to be refused, got %v", err)
	}
}

// One container cannot be both a database and an agent: the kernel would hold
// two contradictory sets of rules for backing it up and probing it.
func TestAgentThatIsAlsoDatabaseIsRefused(t *testing.T) {
	scenario := agentScenario(t)
	scenario.Goal.Workload.Engine = "postgres"
	scenario.Goal.Workload.Volumes = []VolumeRef{{Name: "d", MountPath: "/data"}}
	err := scenario.NormalizeAndValidate()
	if err == nil || !strings.Contains(err.Error(), "cannot be both") {
		t.Fatalf("expected database-agent hybrid to be refused, got %v", err)
	}
}

// An unknown runtime would be started as a generic container, leaving its
// budget ceilings and tool envelope unenforced.
func TestUnknownAgentRuntimeIsRefused(t *testing.T) {
	scenario := agentScenario(t)
	scenario.Goal.Workload.Runtime.Name = "homemade/v9"
	err := scenario.NormalizeAndValidate()
	if err == nil || !strings.Contains(err.Error(), "is not supported") {
		t.Fatalf("expected unknown runtime to be refused, got %v", err)
	}
}

// An agent placed where its provider is unreachable can never become ready, so
// it is infeasible at placement rather than discovered at probe time.
func TestAgentIsNotPlacedWithoutProviderReach(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	for _, node := range world.Nodes {
		node.Providers = map[string]ProviderReach{}
	}
	_, err := (PlacementAgent{}).Propose(goal, world)
	if err == nil || !strings.Contains(err.Error(), "reach provider") {
		t.Fatalf("expected unreachable provider to block placement, got %v", err)
	}
}

// The kernel recomputes provider feasibility rather than trusting the agent.
func TestKernelRefusesAgentOnUnreachableProvider(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	world.Nodes["base"].Providers = map[string]ProviderReach{}
	world.Nodes["base"].Images[testImage] = true

	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "create", Kind: ActionCreateAllocation, Target: "triage-0",
			Workload: "triage", Node: "base", Image: testImage,
			Resources: goal.Workload.Resources, Budget: goal.Workload.Runtime.Budget,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "placement-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "cannot reach provider") {
		t.Fatalf("expected kernel to refuse unreachable provider, got %v", err)
	}
}

// Budget is a resource the node commits, the same way it commits memory.
func TestAgentIsRefusedWhenNodeBudgetIsExhausted(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	world.Nodes["base"].Images[testImage] = true
	world.Nodes["base"].BudgetCapacity = Budget{
		Tokens: 1000, CostMillis: 10, WallSeconds: 10, ToolCalls: 1,
	}

	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "create", Kind: ActionCreateAllocation, Target: "triage-0",
			Workload: "triage", Node: "base", Image: testImage,
			Resources: goal.Workload.Resources, Budget: goal.Workload.Runtime.Budget,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "placement-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "lacks agent budget") {
		t.Fatalf("expected budget capacity to be enforced, got %v", err)
	}
}

// An ordinary workload must not reserve agent budget, or it would consume a
// node's agent capacity without being subject to any of its ceilings.
func TestOrdinaryWorkloadMayNotReserveBudget(t *testing.T) {
	scenario := validScenario()
	goal := scenario.Goal
	world := scenario.World
	world.normalize()
	world.Nodes["base"].Images[testImage] = true

	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "create", Kind: ActionCreateAllocation, Target: "web-0",
			Workload: goal.Workload.Name, Node: "base", Image: testImage,
			Resources: goal.Workload.Resources,
			Budget:    Budget{Tokens: 10, CostMillis: 10, WallSeconds: 10, ToolCalls: 10},
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "placement-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "only agent workloads may reserve budget") {
		t.Fatalf("expected non-agent budget reservation to be refused, got %v", err)
	}
}

// An agent could otherwise receive a capability the operator never authorized,
// the tool-grant equivalent of mounting an undeclared secret.
func TestUndeclaredToolGrantIsRefused(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Image: testImage,
		Resources: goal.Workload.Resources, Phase: AllocationCreated,
		Budget: goal.Workload.Runtime.Budget,
	}

	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "grant", Kind: ActionGrantTools, Target: "triage-0",
			Workload: "triage", Node: "base",
			Tools: []ToolGrant{{Name: "shell.exec", Scope: "/", Mutating: true}},
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "placement-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "not declared by the goal") {
		t.Fatalf("expected undeclared tool grant to be refused, got %v", err)
	}
}

// A mutating tool changes state outside a4s, where no compensation or event log
// reaches, so it needs a separately authenticated decision.
func TestMutatingToolGrantRequiresApproval(t *testing.T) {
	goal := agentGoal()
	mutating := ToolGrant{Name: "repo.write", Scope: "github.com/arcbjorn/a4s", Mutating: true}
	goal.Workload.Runtime.Tools = []ToolGrant{mutating}
	world := agentWorld(t)
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Image: testImage,
		Resources: goal.Workload.Resources, Phase: AllocationCreated,
		Budget: goal.Workload.Runtime.Budget,
	}

	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "grant", Kind: ActionGrantTools, Target: "triage-0",
			Workload: "triage", Node: "base", Tools: []ToolGrant{mutating},
		}},
	}
	kernel := Kernel{Policy: DefaultPolicy()}
	err := kernel.Authorize(AgentDescriptor{ID: "placement-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "agent-mutating-tools approval") {
		t.Fatalf("expected mutating grant to require approval, got %v", err)
	}

	world.Approvals["a1"] = &Approval{
		ID: "a1", GoalID: goal.ID, Scope: "agent-mutating-tools",
		IssuedBy: "operator", Granted: true,
	}
	if err := kernel.Authorize(AgentDescriptor{ID: "placement-agent"}, goal, world, proposal); err != nil {
		t.Fatalf("approved mutating grant rejected: %v", err)
	}
}

// Granting a tool to a started agent would widen a blast radius the kernel had
// already authorized.
func TestToolsMayNotBeGrantedAfterStart(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Image: testImage,
		Resources: goal.Workload.Resources, Phase: AllocationRunning,
		Budget: goal.Workload.Runtime.Budget,
	}

	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "grant", Kind: ActionGrantTools, Target: "triage-0",
			Workload: "triage", Node: "base",
			Tools: goal.Workload.Runtime.Tools,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "placement-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "before allocation") {
		t.Fatalf("expected late tool grant to be refused, got %v", err)
	}
}

// Restarting an exhausted agent would burn the same budget to reach the same
// exhausted state.
func TestExhaustedAgentIsNotStarted(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	budget := goal.Workload.Runtime.Budget
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Image: testImage,
		Resources: goal.Workload.Resources, Phase: AllocationCreated,
		Budget: budget, Tools: goal.Workload.Runtime.Tools,
		Spent: Budget{Tokens: budget.Tokens + 1},
	}

	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "start", Kind: ActionStartAllocation, Target: "triage-0",
			Workload: "triage", Node: "base",
		}},
		ExpectedEvidence: []Check{{Kind: CheckAgentReady, Target: "triage-0", Want: "true"}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "placement-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "exhausted its budget") {
		t.Fatalf("expected exhausted agent to be refused a start, got %v", err)
	}
}

// An agent that declared tools must hold its envelope before it runs, or the
// runtime would be deciding its own capabilities.
func TestAgentWithoutToolEnvelopeIsNotStarted(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Image: testImage,
		Resources: goal.Workload.Resources, Phase: AllocationCreated,
		Budget: goal.Workload.Runtime.Budget,
	}

	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "start", Kind: ActionStartAllocation, Target: "triage-0",
			Workload: "triage", Node: "base",
		}},
		ExpectedEvidence: []Check{{Kind: CheckAgentReady, Target: "triage-0", Want: "true"}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "placement-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "no tool grant") {
		t.Fatalf("expected start without tool envelope to be refused, got %v", err)
	}
}

// An agent instance holds task context a stateless replica does not. Stopping
// it mid-task destroys work rather than shifting load.
func TestAgentHoldingTaskMustDrainBeforeStop(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Image: testImage,
		Resources: goal.Workload.Resources, Phase: AllocationRunning,
		Budget: goal.Workload.Runtime.Budget, Task: "task-7",
	}

	proposal := Proposal{
		ID: "p1", AgentID: "rollout-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "stop", Kind: ActionStopAllocation, Target: "triage-0",
			Workload: "triage", Node: "base",
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "rollout-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "must be drained") {
		t.Fatalf("expected stop of working agent to be refused, got %v", err)
	}
}

// Draining is not enough on its own: the instance must be observed to have
// actually released its task.
func TestDrainingAgentStillHoldingTaskMayNotStop(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Image: testImage,
		Resources: goal.Workload.Resources, Phase: AllocationRunning,
		Budget: goal.Workload.Runtime.Budget, Task: "task-7", Draining: true,
	}

	proposal := Proposal{
		ID: "p1", AgentID: "rollout-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "stop", Kind: ActionStopAllocation, Target: "triage-0",
			Workload: "triage", Node: "base",
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "rollout-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "still working on task") {
		t.Fatalf("expected stop of still-working agent to be refused, got %v", err)
	}
}

// A drained instance holds nothing, so stopping it destroys no work.
func TestDrainedAgentMayStop(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Image: testImage,
		Resources: goal.Workload.Resources, Phase: AllocationRunning,
		Budget: goal.Workload.Runtime.Budget, Draining: true,
	}

	proposal := Proposal{
		ID: "p1", AgentID: "rollout-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "stop", Kind: ActionStopAllocation, Target: "triage-0",
			Workload: "triage", Node: "base",
		}},
	}
	if err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "rollout-agent"}, goal, world, proposal); err != nil {
		t.Fatalf("drained agent refused a stop: %v", err)
	}
}

// An exhausted agent cannot make progress on its task, so waiting for it to
// finish would wait forever.
func TestExhaustedAgentMayStopWithoutDraining(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	budget := goal.Workload.Runtime.Budget
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Image: testImage,
		Resources: goal.Workload.Resources, Phase: AllocationRunning,
		Budget: budget, Task: "task-7",
		Spent: Budget{Tokens: budget.Tokens + 1},
	}

	proposal := Proposal{
		ID: "p1", AgentID: "rollout-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "stop", Kind: ActionStopAllocation, Target: "triage-0",
			Workload: "triage", Node: "base",
		}},
	}
	if err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "rollout-agent"}, goal, world, proposal); err != nil {
		t.Fatalf("exhausted agent refused a stop: %v", err)
	}
}

// The rollout agent drains before it retires, so an agent's accumulated context
// is not discarded mid-task.
func TestRolloutDrainsAgentBeforeRetiring(t *testing.T) {
	goal := agentGoal()
	goal.Workload.Image = "registry.example.com/triage@sha256:" + strings.Repeat("b", 64)
	world := agentWorld(t)
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Image: testImage,
		Resources: goal.Workload.Resources, Phase: AllocationRunning,
		Budget: goal.Workload.Runtime.Budget, Task: "task-7",
	}

	proposal, err := (RolloutAgent{}).Propose(goal, world)
	if err != nil {
		t.Fatalf("rollout refused to propose: %v", err)
	}
	if len(proposal.Actions) != 1 || proposal.Actions[0].Kind != ActionDrainAllocation {
		t.Fatalf("expected a single drain action, got %+v", proposal.Actions)
	}
	if len(proposal.ExpectedEvidence) != 1 ||
		proposal.ExpectedEvidence[0].Kind != CheckAllocationDrained {
		t.Fatalf("expected drained evidence to be declared, got %+v", proposal.ExpectedEvidence)
	}
}

// An agent that spent its ceiling is finished whatever image it runs, so
// retiring it is what frees budget for a replacement.
func TestExhaustedAgentIsTreatedAsDrifted(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	budget := goal.Workload.Runtime.Budget
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Image: goal.Workload.Image,
		Resources: goal.Workload.Resources, Phase: AllocationRunning,
		Budget: budget, Spent: Budget{Tokens: budget.Tokens + 1},
	}

	drifted := driftedAllocations(goal, world)
	if len(drifted) != 1 || drifted[0].ID != "triage-0" {
		t.Fatalf("expected exhausted agent to be drifted, got %+v", drifted)
	}
}

// Reservation and consumption are different questions, and Exhausts is
// deliberately not the negation of Fits. An instance that consumed exactly its
// ceiling has nothing left; treating that as "still fits" would grant one extra
// unit on every dimension to every agent.
func TestExhaustsIsNotTheNegationOfFits(t *testing.T) {
	ceiling := Budget{Tokens: 5, CostMillis: 5, WallSeconds: 5, ToolCalls: 5}
	exact := ceiling

	if !exact.Fits(ceiling) {
		t.Fatal("a reservation equal to remaining capacity must be permitted")
	}
	if !exact.Exhausts(ceiling) {
		t.Fatal("consumption equal to the ceiling must read as exhausted")
	}

	// Exhaustion is per dimension: spending all of any one ceiling is enough,
	// since the agent cannot make progress without it.
	oneDimension := Budget{ToolCalls: 5}
	if !oneDimension.Exhausts(ceiling) {
		t.Fatal("expected a single spent dimension to exhaust the instance")
	}
	if under := (Budget{Tokens: 4, CostMillis: 4, WallSeconds: 4, ToolCalls: 4}); under.Exhausts(ceiling) {
		t.Fatal("expected consumption below every ceiling to leave room")
	}
}

// Spend only ever increases. Accepting a lower reading would let an exhausted
// agent look affordable again and be restarted into the same ceiling.
func TestSpendEvidenceNeverDecreases(t *testing.T) {
	world := agentWorld(t)
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Phase: AllocationRunning,
		Budget: Budget{Tokens: 100, CostMillis: 100, WallSeconds: 100, ToolCalls: 100},
		Spent:  Budget{Tokens: 80, CostMillis: 80},
	}

	next, err := Project(world, Evidence{
		Kind: EvidenceAgentSpent, Target: "triage-0",
		Observed: map[string]string{"tokens": "10", "cost_millis": "10"},
	})
	if err != nil {
		t.Fatalf("projection failed: %v", err)
	}
	if next.Allocations["triage-0"].Spent.Tokens != 80 {
		t.Fatalf("expected stale spend to be ignored, got %d",
			next.Allocations["triage-0"].Spent.Tokens)
	}
}

// An agent that hit its ceiling can do no work, so leaving it ready would let it
// satisfy a goal it cannot serve.
func TestExhaustedAgentBecomesNotReady(t *testing.T) {
	world := agentWorld(t)
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Phase: AllocationRunning,
		Ready:  true,
		Budget: Budget{Tokens: 100, CostMillis: 100, WallSeconds: 100, ToolCalls: 100},
	}

	next, err := Project(world, Evidence{
		Kind: EvidenceAgentSpent, Target: "triage-0",
		Observed: map[string]string{
			"tokens": "500", "cost_millis": "500",
			"wall_seconds": "500", "tool_calls": "500",
		},
	})
	if err != nil {
		t.Fatalf("projection failed: %v", err)
	}
	if next.Allocations["triage-0"].Ready {
		t.Fatal("expected exhausted agent to stop being ready")
	}
}

// Queue depth decides how many workers are useful, bounded by a ceiling the
// operator wrote down.
func TestQueueDepthScalesAgentReplicas(t *testing.T) {
	goal := agentGoal()
	goal.Workload.Runtime.Queue = "triage-work"
	world := agentWorld(t)
	world.ObservedAt = time.Now()
	world.Queues["triage-work"] = &Queue{
		Name: "triage-work", Workload: "triage", Depth: 3,
		MaxWorkers: 5, ObservedAt: world.ObservedAt,
	}

	if got := desiredReplicas(goal, world, 0); got != 3 {
		t.Fatalf("expected depth to justify 3 workers, got %d", got)
	}
	world.Queues["triage-work"].Depth = 99
	if got := desiredReplicas(goal, world, 0); got != 5 {
		t.Fatalf("expected MaxWorkers to cap scaling at 5, got %d", got)
	}
}

// Scaling on a stale depth would keep adding workers for work already drained.
func TestStaleQueueDepthDoesNotScale(t *testing.T) {
	goal := agentGoal()
	goal.Workload.Runtime.Queue = "triage-work"
	world := agentWorld(t)
	world.ObservedAt = time.Now()
	world.Queues["triage-work"] = &Queue{
		Name: "triage-work", Workload: "triage", Depth: 20, MaxWorkers: 10,
		ObservedAt: world.ObservedAt.Add(-2 * maxQueueDepthAge),
	}

	if got := desiredReplicas(goal, world, 0); got != goal.Workload.Replicas {
		t.Fatalf("expected stale depth to be ignored, got %d workers", got)
	}
}

// The kernel recomputes the replica ceiling rather than trusting the placement
// agent's arithmetic.
func TestKernelCapsQueueScaledReplicas(t *testing.T) {
	goal := agentGoal()
	goal.Workload.Runtime.Queue = "triage-work"
	world := agentWorld(t)
	world.Nodes["base"].Images[testImage] = true
	world.Queues["triage-work"] = &Queue{
		Name: "triage-work", Workload: "triage", Depth: 50, MaxWorkers: 2,
		ObservedAt: world.Now(),
	}

	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "create", Kind: ActionCreateAllocation, Target: "triage-9",
			Workload: "triage", Node: "base", Image: testImage, Replica: 9,
			Resources: goal.Workload.Resources, Budget: goal.Workload.Runtime.Budget,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "placement-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "outside goal") {
		t.Fatalf("expected replica above MaxWorkers to be refused, got %v", err)
	}
}

// A queue that no worker count can bound turns a spike into unbounded spend.
func TestQueueWithoutWorkerCeilingIsRefused(t *testing.T) {
	scenario := agentScenario(t)
	scenario.Goal.Workload.Runtime.Queue = "triage-work"
	scenario.World.Queues = map[string]*Queue{
		"triage-work": {Name: "triage-work", Workload: "triage", Depth: 1},
	}
	err := scenario.NormalizeAndValidate()
	if err == nil || !strings.Contains(err.Error(), "cap workers") {
		t.Fatalf("expected uncapped queue to be refused, got %v", err)
	}
}

// A queue serving another workload would let one workload's demand scale
// another's replicas.
func TestQueueForAnotherWorkloadIsRefused(t *testing.T) {
	scenario := agentScenario(t)
	scenario.Goal.Workload.Runtime.Queue = "triage-work"
	scenario.World.Queues = map[string]*Queue{
		"triage-work": {Name: "triage-work", Workload: "other", MaxWorkers: 3},
	}
	err := scenario.NormalizeAndValidate()
	if err == nil || !strings.Contains(err.Error(), "serves workload") {
		t.Fatalf("expected mismatched queue to be refused, got %v", err)
	}
}

// A draining or exhausted agent is not serving capacity, so a goal must not
// look satisfied by instances on their way out.
func TestDrainingAgentDoesNotSatisfyGoal(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	world.ObservedAt = time.Now()
	world.Allocations["triage-0"] = &Allocation{
		ID: "triage-0", Workload: "triage", Node: "base", Image: goal.Workload.Image,
		Resources: goal.Workload.Resources, Phase: AllocationRunning,
		Ready: true, Budget: goal.Workload.Runtime.Budget, Draining: true,
	}

	if got := matchingReadyAllocations(goal, world); got != 0 {
		t.Fatalf("expected draining agent not to count as ready, got %d", got)
	}
}

// The placement agent installs the tool envelope before the agent starts, and
// declares agent readiness rather than process readiness.
func TestPlacementGrantsToolsBeforeStartingAgent(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	world.Nodes["base"].Images[testImage] = true

	proposal, err := (PlacementAgent{}).Propose(goal, world)
	if err != nil {
		t.Fatalf("placement refused to propose: %v", err)
	}
	var grantID, startID string
	var startDeps []string
	for _, action := range proposal.Actions {
		switch action.Kind {
		case ActionGrantTools:
			grantID = action.ID
		case ActionStartAllocation:
			startID, startDeps = action.ID, action.DependsOn
		}
	}
	if grantID == "" || startID == "" {
		t.Fatalf("expected both a grant and a start action, got %+v", proposal.Actions)
	}
	found := false
	for _, dep := range startDeps {
		if dep == grantID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected start to depend on the tool grant, got deps %v", startDeps)
	}
	if len(proposal.ExpectedEvidence) == 0 ||
		proposal.ExpectedEvidence[0].Kind != CheckAgentReady {
		t.Fatalf("expected agent readiness to be declared, got %+v", proposal.ExpectedEvidence)
	}

	if err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "placement-agent"}, goal, world, proposal); err != nil {
		t.Fatalf("kernel rejected the placement agent's own agent-workload plan: %v", err)
	}
}

// Egress is perishable. Treating a remembered measurement as current would
// place agents onto a node that has since lost its route.
func TestExpiredProviderReachIsNotReachable(t *testing.T) {
	world := agentWorld(t)
	node := world.Nodes["base"]

	if !node.CanReach("anthropic", world.Now()) {
		t.Fatal("expected a live measurement to count as reachable")
	}
	// One second past expiry is not reachability.
	past := node.Providers["anthropic"].ExpiresAt.Add(time.Second)
	if node.CanReach("anthropic", past) {
		t.Fatal("expected an expired measurement to stop counting")
	}
}

// The scheduler must have positive evidence of reach, not an absence of bad
// news.
func TestUnmeasuredProviderIsNotReachable(t *testing.T) {
	world := agentWorld(t)
	node := world.Nodes["base"]

	if node.CanReach("openai", world.Now()) {
		t.Fatal("expected a provider that was never measured to be unreachable")
	}
	node.Providers["openai"] = ProviderReach{
		Reachable: false, ObservedAt: world.Now(),
		ExpiresAt: world.Now().Add(time.Minute), Detail: "dial tcp: no route to host",
	}
	if node.CanReach("openai", world.Now()) {
		t.Fatal("expected a failed measurement to be unreachable")
	}
}

// A node whose reachability observation aged out must stop attracting agent
// placements, even though the fact is still in the world.
func TestStaleProviderReachBlocksPlacement(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	// Advance evaluation past every provider measurement's expiry.
	world.ObservedAt = world.Nodes["base"].Providers["anthropic"].ExpiresAt.Add(time.Minute)

	_, err := (PlacementAgent{}).Propose(goal, world)
	if err == nil || !strings.Contains(err.Error(), "reach provider") {
		t.Fatalf("expected stale reachability to block placement, got %v", err)
	}
}

// The kernel recomputes staleness itself rather than trusting that the agent
// checked it.
func TestKernelRefusesStaleProviderReach(t *testing.T) {
	goal := agentGoal()
	world := agentWorld(t)
	world.Nodes["base"].Images[testImage] = true
	world.ObservedAt = world.Nodes["base"].Providers["anthropic"].ExpiresAt.Add(time.Minute)

	proposal := Proposal{
		ID: "p1", AgentID: "placement-agent", GoalID: goal.ID,
		BasedOnRevision: world.Revision,
		Actions: []Action{{
			ID: "create", Kind: ActionCreateAllocation, Target: "triage-0",
			Workload: "triage", Node: "base", Image: testImage,
			Resources: goal.Workload.Resources, Budget: goal.Workload.Runtime.Budget,
		}},
	}
	err := (Kernel{Policy: DefaultPolicy()}).Authorize(
		AgentDescriptor{ID: "placement-agent"}, goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "cannot reach provider") {
		t.Fatalf("expected the kernel to refuse stale reachability, got %v", err)
	}
}

// Nothing wrote node provider facts before this evidence existed, so an agent
// workload could never be placed in a real deployment.
func TestProviderEvidenceRecordsReachability(t *testing.T) {
	world := agentWorld(t)
	world.Nodes["base"].Providers = map[string]ProviderReach{}
	observed := world.Now()

	next, err := Project(world, Evidence{
		Kind: EvidenceProviderReachable, Target: "anthropic",
		ObservedAt: observed, ExpiresAt: observed.Add(2 * time.Minute),
		Observed: map[string]string{"node": "base", "reachable": "true"},
	})
	if err != nil {
		t.Fatalf("projection failed: %v", err)
	}
	if !next.Nodes["base"].CanReach("anthropic", observed) {
		t.Fatal("expected projected evidence to make the provider reachable")
	}
}

// A stale success overwriting a fresh failure is the direction that places
// agents onto a node which has lost its egress.
func TestOlderProviderEvidenceDoesNotOverwrite(t *testing.T) {
	world := agentWorld(t)
	now := world.Now()
	world.Nodes["base"].Providers = map[string]ProviderReach{
		"anthropic": {
			Reachable: false, ObservedAt: now,
			ExpiresAt: now.Add(time.Minute), Detail: "connection refused",
		},
	}

	next, err := Project(world, Evidence{
		Kind: EvidenceProviderReachable, Target: "anthropic",
		ObservedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute),
		Observed: map[string]string{"node": "base", "reachable": "true"},
	})
	if err != nil {
		t.Fatalf("projection failed: %v", err)
	}
	if next.Nodes["base"].CanReach("anthropic", now) {
		t.Fatal("a stale success overwrote a fresh failure")
	}
}

// An operator should be able to tell a DNS failure from a provider outage
// without reading node logs.
func TestProviderEvidenceCarriesFailureDetail(t *testing.T) {
	world := agentWorld(t)
	next, err := Project(world, Evidence{
		Kind: EvidenceProviderReachable, Target: "anthropic",
		ObservedAt: world.Now().Add(time.Second),
		Observed: map[string]string{
			"node": "base", "reachable": "false", "detail": "provider returned 503",
		},
	})
	if err != nil {
		t.Fatalf("projection failed: %v", err)
	}
	reach := next.Nodes["base"].Providers["anthropic"]
	if reach.Reachable || reach.Detail != "provider returned 503" {
		t.Fatalf("expected the failure detail to be recorded, got %+v", reach)
	}
}
