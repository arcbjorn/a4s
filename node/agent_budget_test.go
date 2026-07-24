package node

import (
	"context"
	"errors"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

func testBudget() control.Budget {
	return control.Budget{Tokens: 1000, CostMillis: 100, WallSeconds: 60, ToolCalls: 5}
}

func meteredAgents(t *testing.T, allocation string, tools ...control.ToolGrant) *Agents {
	t.Helper()
	agents := NewAgents(t.TempDir())
	agents.Providers = StaticProviders{"anthropic": true}
	agents.Reserve(allocation, testBudget())
	if len(tools) > 0 {
		if _, err := agents.grant(control.Action{
			Kind: control.ActionGrantTools, Target: allocation, Tools: tools,
		}); err != nil {
			t.Fatalf("grant failed: %v", err)
		}
	}
	return agents
}

// The controller is too far away to stop a runaway loop: a round trip through
// evidence, projection, and a proposal takes longer than an agent needs to
// spend the rest of its budget. The node holds the kill switch.
func TestSpendStopsAtCeiling(t *testing.T) {
	agents := meteredAgents(t, "triage-0")

	if _, within := agents.Spend("triage-0", control.Budget{Tokens: 400}); !within {
		t.Fatal("expected spend below the ceiling to be permitted")
	}
	if _, within := agents.Spend("triage-0", control.Budget{Tokens: 700}); within {
		t.Fatal("expected spend past the ceiling to be refused")
	}
	if !agents.Exhausted("triage-0") {
		t.Fatal("expected the instance to read as exhausted")
	}
}

// A runtime reporting less than before would otherwise buy itself more room.
func TestSpendIgnoresNegativeReports(t *testing.T) {
	agents := meteredAgents(t, "triage-0")
	if _, within := agents.Spend("triage-0", control.Budget{Tokens: 500}); !within {
		t.Fatal("expected first spend to be permitted")
	}
	spent, _ := agents.Spend("triage-0", control.Budget{Tokens: -400})
	if spent.Tokens != 500 {
		t.Fatalf("expected negative delta to be clamped, got %d tokens", spent.Tokens)
	}
}

// An instance the node holds no reservation for has no authorization to spend.
func TestSpendWithoutReservationIsRefused(t *testing.T) {
	agents := NewAgents(t.TempDir())
	if _, within := agents.Spend("ghost-0", control.Budget{Tokens: 1}); within {
		t.Fatal("expected spend without a reservation to be refused")
	}
}

// A restarted dispatch must not zero the meter and hand a spent agent a fresh
// ceiling.
func TestReserveReplayKeepsSpend(t *testing.T) {
	agents := meteredAgents(t, "triage-0")
	if _, within := agents.Spend("triage-0", control.Budget{Tokens: 900}); !within {
		t.Fatal("expected spend to be permitted")
	}
	agents.Reserve("triage-0", testBudget())
	spent, ok := agents.Spent("triage-0")
	if !ok || spent.Tokens != 900 {
		t.Fatalf("expected replayed reservation to keep spend, got %d", spent.Tokens)
	}
}

// The envelope stops being a declaration and becomes an enforcement point here.
func TestToolCallOutsideEnvelopeIsRefused(t *testing.T) {
	agents := meteredAgents(t, "triage-0",
		control.ToolGrant{Name: "repo.read", Scope: "org/a"})

	if err := agents.AuthorizeToolCall("triage-0", "repo.read", "org/a"); err != nil {
		t.Fatalf("granted tool refused: %v", err)
	}
	err := agents.AuthorizeToolCall("triage-0", "repo.write", "org/a")
	if !errors.Is(err, ErrToolNotGranted) {
		t.Fatalf("expected ungranted tool to be refused, got %v", err)
	}
	// A grant is scoped, so the same tool at another scope is a different
	// capability.
	if err := agents.AuthorizeToolCall("triage-0", "repo.read", "org/other"); !errors.Is(err, ErrToolNotGranted) {
		t.Fatalf("expected out-of-scope call to be refused, got %v", err)
	}
	if got := agents.ToolRefusals("triage-0"); got != 2 {
		t.Fatalf("expected 2 recorded refusals, got %d", got)
	}
}

// An agent thrashing between two granted tools stays under every other ceiling
// indefinitely. The tool-call budget is what stops it.
func TestToolCallsChargeTheBudget(t *testing.T) {
	agents := meteredAgents(t, "triage-0",
		control.ToolGrant{Name: "repo.read", Scope: "org/a"})

	for i := 0; i < testBudget().ToolCalls; i++ {
		if err := agents.AuthorizeToolCall("triage-0", "repo.read", "org/a"); err != nil {
			t.Fatalf("call %d refused early: %v", i, err)
		}
	}
	if !agents.Exhausted("triage-0") {
		t.Fatal("expected the tool-call ceiling to exhaust the instance")
	}
	err := agents.AuthorizeToolCall("triage-0", "repo.read", "org/a")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("expected an exhausted instance to be refused, got %v", err)
	}
}

// A spent agent must not keep calling tools that happen to be free.
func TestExhaustedInstanceMayNotCallGrantedTools(t *testing.T) {
	agents := meteredAgents(t, "triage-0",
		control.ToolGrant{Name: "repo.read", Scope: "org/a"})
	agents.Spend("triage-0", control.Budget{Tokens: 5000})

	err := agents.AuthorizeToolCall("triage-0", "repo.read", "org/a")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("expected exhausted instance to be refused, got %v", err)
	}
}

// An instance with no reservation was never prepared on this node.
func TestToolCallWithoutReservationIsRefused(t *testing.T) {
	agents := NewAgents(t.TempDir())
	err := agents.AuthorizeToolCall("ghost-0", "repo.read", "org/a")
	if err == nil {
		t.Fatal("expected a call without a reservation to be refused")
	}
}

// An agent is ready only when it can reach its provider with budget remaining.
func TestAgentReadinessRequiresProviderAndBudget(t *testing.T) {
	agents := meteredAgents(t, "triage-0")
	target := control.ProbeTarget{
		Allocation: "triage-0", Kind: control.ProbeAgent, Provider: "anthropic",
	}

	ready, observed, err := agents.ObserveReadiness(target)
	if err != nil || !ready {
		t.Fatalf("expected a reachable, funded agent to be ready: %v %v", ready, err)
	}
	if observed["provider"] != "anthropic" {
		t.Fatalf("expected the provider to be reported, got %v", observed)
	}

	agents.Spend("triage-0", control.Budget{Tokens: 5000})
	ready, observed, err = agents.ObserveReadiness(target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready || observed["reason"] != "budget exhausted" {
		t.Fatalf("expected an exhausted agent to be unready, got %v %v", ready, observed)
	}
}

// A container that looks perfectly healthy is not evidence an agent can work.
func TestAgentReadinessFailsOnUnreachableProvider(t *testing.T) {
	agents := meteredAgents(t, "triage-0")
	agents.Providers = StaticProviders{}

	ready, observed, err := agents.ObserveReadiness(control.ProbeTarget{
		Allocation: "triage-0", Kind: control.ProbeAgent, Provider: "anthropic",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready || observed["reason"] != "provider unreachable" {
		t.Fatalf("expected unreachable provider to block readiness, got %v %v", ready, observed)
	}
}

// Without a reachability source the node cannot establish the fact the probe
// exists to establish. That is absence of evidence, not evidence of health.
func TestAgentReadinessErrorsWithoutReachabilitySource(t *testing.T) {
	agents := NewAgents(t.TempDir())
	agents.Reserve("triage-0", testBudget())

	ready, _, err := agents.ObserveReadiness(control.ProbeTarget{
		Allocation: "triage-0", Kind: control.ProbeAgent, Provider: "anthropic",
	})
	if err == nil {
		t.Fatal("expected a missing reachability source to be an error, not a false reading")
	}
	if ready {
		t.Fatal("expected an unmeasurable agent not to be ready")
	}
}

// Reporting an instance this node never reserved would assert something the
// node cannot know.
func TestAgentReadinessRefusesUnknownAllocation(t *testing.T) {
	agents := NewAgents(t.TempDir())
	agents.Providers = StaticProviders{"anthropic": true}

	ready, observed, err := agents.ObserveReadiness(control.ProbeTarget{
		Allocation: "ghost-0", Kind: control.ProbeAgent, Provider: "anthropic",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready || observed["reason"] != "no budget reservation on this node" {
		t.Fatalf("expected an unknown allocation to be unready, got %v %v", ready, observed)
	}
}

// The projection treats spend as monotonic, so evidence carries the running
// total rather than a delta.
func TestSpendEvidenceReportsRunningTotal(t *testing.T) {
	agents := meteredAgents(t, "triage-0")
	agents.Spend("triage-0", control.Budget{Tokens: 100, CostMillis: 10})
	agents.Spend("triage-0", control.Budget{Tokens: 250, CostMillis: 20})

	evidence, ok := agents.SpendEvidence("triage-0")
	if !ok {
		t.Fatal("expected spend evidence for a metered instance")
	}
	if evidence.Kind != control.EvidenceAgentSpent {
		t.Fatalf("unexpected evidence kind %q", evidence.Kind)
	}
	if evidence.Observed["tokens"] != "350" || evidence.Observed["cost_millis"] != "30" {
		t.Fatalf("expected accumulated totals, got %v", evidence.Observed)
	}
	if evidence.Observed["exhausted"] != "false" {
		t.Fatalf("expected the instance to read as funded, got %v", evidence.Observed)
	}
}

// An agent repeatedly reaching for a capability it lacks is a fact an operator
// should see, whether it means a misconfigured envelope or an agent doing
// something unintended.
func TestSpendEvidenceReportsToolRefusals(t *testing.T) {
	agents := meteredAgents(t, "triage-0")
	_ = agents.AuthorizeToolCall("triage-0", "shell.exec", "/")

	evidence, ok := agents.SpendEvidence("triage-0")
	if !ok {
		t.Fatal("expected spend evidence")
	}
	if evidence.Observed["tool_refusals"] != "1" {
		t.Fatalf("expected the refusal to be reported, got %v", evidence.Observed)
	}
}

// A deleted allocation must leave no meter behind for an identifier reuse to
// inherit.
func TestReleaseClearsTheMeter(t *testing.T) {
	agents := meteredAgents(t, "triage-0")
	agents.Spend("triage-0", control.Budget{Tokens: 900})
	agents.Release("triage-0")

	if _, ok := agents.Spent("triage-0"); ok {
		t.Fatal("expected release to clear the meter")
	}
	if _, ok := agents.SpendEvidence("triage-0"); ok {
		t.Fatal("expected no spend evidence after release")
	}
}

// Readiness means something different per workload kind, and only the owning
// capability can establish it.
func TestCompositeObserverRoutesByProbeKind(t *testing.T) {
	agents := meteredAgents(t, "triage-0")
	observer := &CompositeObserver{Agents: agents}

	ready, _, err := observer.ObserveReadiness(control.ProbeTarget{
		Allocation: "triage-0", Kind: control.ProbeAgent, Provider: "anthropic",
	})
	if err != nil || !ready {
		t.Fatalf("expected the agent probe to route to the agent capability: %v %v", ready, err)
	}
}

// A missing capability is a wiring mistake and must be reported as one rather
// than read as an unhealthy workload.
func TestCompositeObserverReportsMissingCapability(t *testing.T) {
	observer := &CompositeObserver{}

	if _, _, err := observer.ObserveReadiness(control.ProbeTarget{
		Allocation: "triage-0", Kind: control.ProbeAgent, Provider: "anthropic",
	}); err == nil {
		t.Fatal("expected a missing agent capability to be an error")
	}
	if _, _, err := observer.ObserveReadiness(control.ProbeTarget{
		Allocation: "db-0", Kind: control.ProbeDatabase, Engine: "postgres",
	}); err == nil {
		t.Fatal("expected a missing database capability to be an error")
	}
}

// Reaching the runtime observer with a delegated kind means the observer was
// never composed, which is a wiring mistake rather than a missing feature.
func TestRuntimeObserverRejectsDelegatedProbeKinds(t *testing.T) {
	observer := NewRuntimeObserver(NewContainerRuntime(&fakeBackend{
		state: BackendState{Exists: true, Running: true},
	}))

	_, _, err := observer.ObserveReadiness(control.ProbeTarget{
		Allocation: "triage-0", Kind: control.ProbeAgent, Provider: "anthropic",
	})
	if err == nil {
		t.Fatal("expected the runtime observer to refuse an agent probe")
	}
}

// A budget the control plane never hears about cannot bound anything above the
// node, so metering reaches the world view on the same schedule as every other
// supervised fact.
func TestSupervisorReportsAgentSpend(t *testing.T) {
	supervisor, backend, desired := newSupervisorFixture(t)
	agents := meteredAgents(t, "triage-0")
	supervisor.Agents = agents
	agents.Spend("triage-0", control.Budget{Tokens: 250, CostMillis: 20})

	if err := desired.Record(DesiredAllocation{
		ID: "triage-0", Workload: "triage", Running: true,
	}); err != nil {
		t.Fatal(err)
	}
	backend.states["triage-0"] = BackendState{Exists: true, Running: true}

	observations, err := supervisor.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var spend *control.Evidence
	for i := range observations {
		if observations[i].Kind == control.EvidenceAgentSpent {
			spend = &observations[i]
		}
	}
	if spend == nil {
		t.Fatalf("expected spend evidence from supervision, got %+v", observations)
	}
	if spend.Observed["tokens"] != "250" {
		t.Fatalf("expected reported spend to match the meter, got %v", spend.Observed)
	}
	if spend.Source != "node-supervisor" {
		t.Fatalf("expected the supervisor to attribute the observation, got %q", spend.Source)
	}
}

// An agent that exhausted its budget and stopped is exactly the case the
// control plane needs to hear about, so a stopped entry must not withhold it.
func TestSupervisorReportsSpendForStoppedAgent(t *testing.T) {
	supervisor, backend, desired := newSupervisorFixture(t)
	agents := meteredAgents(t, "triage-0")
	supervisor.Agents = agents
	agents.Spend("triage-0", control.Budget{Tokens: 5000})

	if err := desired.Record(DesiredAllocation{
		ID: "triage-0", Workload: "triage", Running: false,
	}); err != nil {
		t.Fatal(err)
	}
	backend.states["triage-0"] = BackendState{Exists: true, Running: false}

	observations, err := supervisor.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) == 0 || observations[0].Kind != control.EvidenceAgentSpent {
		t.Fatalf("expected spend evidence for a stopped agent, got %+v", observations)
	}
	if observations[0].Observed["exhausted"] != "true" {
		t.Fatalf("expected exhaustion to be reported, got %v", observations[0].Observed)
	}
}

// An agent that spent its ceiling did not crash, it finished. Restarting it
// would burn a fresh ceiling to reach the same exhausted state.
func TestSupervisorDoesNotRestartExhaustedAgent(t *testing.T) {
	supervisor, backend, desired := newSupervisorFixture(t)
	agents := meteredAgents(t, "triage-0")
	supervisor.Agents = agents
	agents.Spend("triage-0", control.Budget{Tokens: 5000})

	if err := desired.Record(DesiredAllocation{
		ID: "triage-0", Workload: "triage", Running: true,
	}); err != nil {
		t.Fatal(err)
	}
	backend.states["triage-0"] = BackendState{Exists: true, Running: false, ExitCode: 0}

	observations, err := supervisor.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if backend.starts != 0 {
		t.Fatalf("supervisor restarted an exhausted agent: starts=%d", backend.starts)
	}
	var reported bool
	for _, evidence := range observations {
		if evidence.Observed["reason"] == "agent budget exhausted" {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("expected exhaustion to be reported as the reason, got %+v", observations)
	}
}

// A non-agent workload has no meter, and supervision must be unchanged for it.
func TestSupervisorIgnoresSpendForOrdinaryWorkloads(t *testing.T) {
	supervisor, backend, desired := newSupervisorFixture(t)
	supervisor.Agents = NewAgents(t.TempDir())

	if err := desired.Record(DesiredAllocation{
		ID: "web-0", Workload: "web", Running: true,
	}); err != nil {
		t.Fatal(err)
	}
	backend.states["web-0"] = BackendState{Exists: true, Running: false, ExitCode: 137}

	observations, err := supervisor.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if backend.starts != 1 {
		t.Fatalf("expected an ordinary workload to still restart, got %d starts", backend.starts)
	}
	for _, evidence := range observations {
		if evidence.Kind == control.EvidenceAgentSpent {
			t.Fatal("reported agent spend for a workload with no meter")
		}
	}
}
