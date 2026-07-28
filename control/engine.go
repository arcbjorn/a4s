package control

import (
	"errors"
	"fmt"
	"time"
)

type EventSink interface {
	Append(Event) error
	NextSequence() uint64
}

// Projector owns the materialized world. The engine advances it only by
// projecting evidence, so world state is always derived from observation rather
// than asserted by whatever performed the mutation.
type Projector interface {
	WorldSource
	Project(Evidence) error
}

type Engine struct {
	Kernel   Kernel
	Agents   []Agent
	Executor Executor
	World    Projector
	Probers  []Prober
	// Leases grant exclusive claims on mutation targets, so two proposals
	// built against the same revision cannot interleave on one allocation.
	Leases *LeaseManager
	Events []Event
	Sink   EventSink
	now    func() time.Time
	// probeTargets is populated from the goal so readiness is measured against
	// the port the workload actually declares.
	probeTargets map[string]ProbeTarget
}

// memoryProjector is the default projection used when an engine is built
// directly on a MemoryExecutor.
type memoryProjector struct{ executor *MemoryExecutor }

func (p memoryProjector) World() World             { return p.executor.World() }
func (p memoryProjector) Project(e Evidence) error { return p.executor.Project(e) }

// NewEngine wires an executor whose evidence is projected into the world. The
// spike's MemoryExecutor is also its own projection; a real deployment supplies
// a projection rebuilt from the event log via WithWorld.
func NewEngine(executor *MemoryExecutor, agents ...Agent) *Engine {
	engine := &Engine{
		Kernel: Kernel{Policy: DefaultPolicy()}, Agents: agents,
		Executor: executor, World: memoryProjector{executor: executor},
		Leases: NewLeaseManager(), now: time.Now,
	}
	// The simulation still measures readiness through the probe path rather
	// than assuming it, so the control loop exercises the same contract a real
	// node probe will use.
	prober := NewMeasuredProber(executor, map[string]ProbeTarget{})
	prober.Now = func() time.Time { return engine.now() }
	engine.Probers = []Prober{prober}
	engine.probeTargets = prober.Targets
	return engine
}

// NewEngineWith builds an engine over any executor and projection. This is the
// production wiring: a remote executor that issues signed capabilities to a
// node, and a world rebuilt from the durable event log.
func NewEngineWith(executor Executor, world Projector, agents ...Agent) *Engine {
	return &Engine{
		Kernel: Kernel{Policy: DefaultPolicy()}, Agents: agents,
		Executor: executor, World: world, probeTargets: map[string]ProbeTarget{},
		Leases: NewLeaseManager(), now: time.Now,
	}
}

func (e *Engine) WithEventSink(sink EventSink) *Engine {
	e.Sink = sink
	return e
}

// WithWorld replaces the projection, allowing a remote executor to be paired
// with a world rebuilt from durable events rather than from executor memory.
func (e *Engine) WithWorld(world Projector) *Engine {
	e.World = world
	return e
}

// WithProbers replaces the evidence sources consulted after execution.
func (e *Engine) WithProbers(probers ...Prober) *Engine {
	e.Probers = probers
	return e
}

func (e *Engine) Run(goal Goal, maxRounds int) error {
	if err := e.record(Event{Type: EventGoalAccepted, Actor: "operator", GoalID: goal.ID, Message: goal.Objective}); err != nil {
		return err
	}
	for round := 0; round < maxRounds; round++ {
		// An operator-approved rollback changes which image the goal means. It
		// is resolved once per round, before anything reads the goal, so every
		// agent and validator in the round works from the same version.
		effective, rolledBackFrom, compensating := CompensatedGoal(goal, e.World.World())
		if compensating {
			if err := e.record(Event{
				Type: EventGoalCompensating, Actor: "coordinator", GoalID: goal.ID,
				Kind: string(ActionCreateAllocation), Target: goal.Workload.Name,
				Message: fmt.Sprintf("operator approved rollback from %s to %s",
					rolledBackFrom, effective.Workload.Image),
			}); err != nil {
				return err
			}
		}

		if goalAchieved(effective, e.World.World()) {
			return e.record(Event{Type: EventGoalAchieved, Actor: "verifier", GoalID: goal.ID, Message: "goal conditions verified from current world evidence"})
		}
		progress := false
		for _, agent := range e.Agents {
			world := e.World.World()
			proposal, err := agent.Propose(effective, world)
			if err != nil {
				// A required rollback is an operator decision, not a denial to
				// retry. Blocking here surfaces the known-good digest rather
				// than looping against a version observed to be failing.
				var rollback *RollbackRequired
				if errors.As(err, &rollback) {
					if recordErr := e.record(Event{Type: EventGoalBlocked, Actor: agent.Descriptor().ID, GoalID: goal.ID, Message: err.Error()}); recordErr != nil {
						return recordErr
					}
					return err
				}
				if recordErr := e.record(Event{Type: EventProposalDenied, Actor: agent.Descriptor().ID, GoalID: goal.ID, Message: err.Error()}); recordErr != nil {
					return recordErr
				}
				continue
			}
			if len(proposal.Actions) == 0 {
				continue
			}
			if err := e.record(Event{Type: EventProposalCreated, Actor: proposal.AgentID, GoalID: goal.ID, ProposalID: proposal.ID, Message: proposal.Reasoning}); err != nil {
				return err
			}
			if err := e.Kernel.Authorize(agent.Descriptor(), effective, world, proposal); err != nil {
				if recordErr := e.record(Event{Type: EventProposalDenied, Actor: "policy-kernel", GoalID: goal.ID, ProposalID: proposal.ID, Message: err.Error()}); recordErr != nil {
					return recordErr
				}
				continue
			}
			if err := e.record(Event{Type: EventProposalApproved, Actor: "policy-kernel", GoalID: goal.ID, ProposalID: proposal.ID, Message: "all actions authorized against a simulated world"}); err != nil {
				return err
			}
			// Claim every target before the first mutation. Revision binding
			// alone would let two proposals built on the same revision both
			// proceed and interleave on the same allocation.
			leaseID, err := e.Leases.Acquire(goal.ID, proposal.ID, LeaseTargets(proposal))
			if err != nil {
				if recordErr := e.record(Event{Type: EventProposalDenied, Actor: "policy-kernel", GoalID: goal.ID, ProposalID: proposal.ID, Message: err.Error()}); recordErr != nil {
					return recordErr
				}
				continue
			}
			// Bind the executor to this authorization so every capability it
			// issues names the proposal and lease that justified it.
			if bound, ok := e.Executor.(BoundExecutor); ok {
				bound.Bind(goal.ID, proposal.ID, proposal.BasedOnRevision, leaseID)
			}
			executed, err := e.executeProposal(effective, proposal, leaseID)
			if executed {
				progress = true
			}
			if err != nil {
				return err
			}
		}
		if !progress {
			if err := e.record(Event{Type: EventGoalBlocked, Actor: "coordinator", GoalID: goal.ID, Message: "no agent produced an authorized action"}); err != nil {
				return err
			}
			return fmt.Errorf("goal %q is blocked", goal.ID)
		}
	}
	if err := e.record(Event{Type: EventGoalBlocked, Actor: "coordinator", GoalID: goal.ID, Message: "reconciliation round limit reached"}); err != nil {
		return err
	}
	return fmt.Errorf("goal %q did not converge after %d rounds", goal.ID, maxRounds)
}

// executeProposal runs an authorized plan and reports whether it mutated
// anything. The lease is released on every exit path, so a failed plan does not
// strand its targets until expiry.
func (e *Engine) executeProposal(goal Goal, proposal Proposal, leaseID string) (bool, error) {
	defer e.Leases.Release(leaseID)

	progress := false
	for _, action := range proposal.Actions {
		// Persist intent before dispatch. If completion persistence fails,
		// recovery can query the node's idempotency ledger using this ID.
		if err := e.record(Event{Type: EventActionDispatched, Actor: "coordinator", GoalID: goal.ID, ProposalID: proposal.ID, ActionID: action.ID, Target: action.Target, Kind: string(action.Kind), Message: string(action.Kind)}); err != nil {
			return progress, err
		}
		evidence, err := e.Executor.Execute(action)
		if err != nil {
			if recordErr := e.record(Event{Type: EventGoalBlocked, Actor: "node-executor", GoalID: goal.ID, ProposalID: proposal.ID, ActionID: action.ID, Target: action.Target, Kind: string(action.Kind), Message: err.Error()}); recordErr != nil {
				return progress, fmt.Errorf("execute action: %v; persist failure: %w", err, recordErr)
			}
			return progress, err
		}
		// Declare what readiness means for a new allocation before any
		// probe runs, so readiness is measured rather than assumed.
		e.registerProbeTarget(goal, action)
		// A node stamps its own observation time onto everything it reports, and
		// that stamp is never overwritten here. An in-memory executor has no
		// node to do it, so the engine fills the gap: evidence with no time is
		// invisible to every rule measured against a window, which would leave
		// the disruption budget silently inert in simulation and let a dry run
		// predict work a real cluster would refuse.
		if evidence.ObservedAt.IsZero() {
			evidence.ObservedAt = e.now().UTC()
		}
		// Evidence, not the action, advances the world.
		if err := e.World.Project(evidence); err != nil {
			if recordErr := e.record(Event{Type: EventGoalBlocked, Actor: "projection", GoalID: goal.ID, ProposalID: proposal.ID, ActionID: action.ID, Target: action.Target, Kind: string(action.Kind), Message: err.Error()}); recordErr != nil {
				return progress, recordErr
			}
			return progress, fmt.Errorf("project evidence for action %q: %w", action.ID, err)
		}
		if err := e.record(Event{Type: EventActionCompleted, Actor: "node-executor", GoalID: goal.ID, ProposalID: proposal.ID, ActionID: action.ID, Target: action.Target, Kind: string(action.Kind), Message: string(action.Kind), Evidence: &evidence}); err != nil {
			return progress, err
		}
		progress = true
	}
	// Independent probes supply the evidence the executor is not permitted to
	// assert, such as readiness.
	if err := e.observe(goal, proposal); err != nil {
		return progress, err
	}
	if err := verifyChecks(e.World.World(), proposal.ExpectedEvidence); err != nil {
		if recordErr := e.record(Event{Type: EventGoalBlocked, Actor: "verifier", GoalID: goal.ID, ProposalID: proposal.ID, Message: err.Error()}); recordErr != nil {
			return progress, recordErr
		}
		return progress, err
	}
	return progress, nil
}

// registerProbeTarget records how readiness should be measured for an
// allocation. The goal's declared port is what a TCP or HTTP probe connects to.
func (e *Engine) registerProbeTarget(goal Goal, action Action) {
	if e.probeTargets == nil || action.Kind != ActionCreateAllocation {
		return
	}
	kind := ProbeProcess
	if goal.Workload.Port > 0 {
		kind = ProbeTCP
	}
	// A database is ready only when it accepts a connection, which a plain TCP
	// probe cannot distinguish from a port that is merely open.
	if goal.Workload.Engine != "" {
		kind = ProbeDatabase
	}
	// An agent is ready only when it can reach its provider with budget left. A
	// process probe would pass for one that can do no work at all.
	provider := ""
	if goal.Workload.Runtime != nil {
		kind = ProbeAgent
		provider = goal.Workload.Runtime.Provider
	}
	e.probeTargets[action.Target] = ProbeTarget{
		Allocation: action.Target, Kind: kind, Port: goal.Workload.Port,
		Engine: goal.Workload.Engine, Provider: provider,
	}
}

// observe collects fresh probe evidence for the proposal's declared checks and
// projects it. Probe failure is not fatal: the verifier decides whether the
// resulting world satisfies the checks.
func (e *Engine) observe(goal Goal, proposal Proposal) error {
	for _, check := range proposal.ExpectedEvidence {
		for _, prober := range e.Probers {
			evidence, ok := prober.Probe(e.World.World(), check)
			if !ok {
				continue
			}
			if err := e.World.Project(evidence); err != nil {
				return fmt.Errorf("project probe evidence for %q: %w", check.Target, err)
			}
			if err := e.record(Event{
				Type: EventObservationRecorded, Actor: "prober", GoalID: goal.ID,
				ProposalID: proposal.ID, Target: check.Target, Kind: evidence.Kind,
				Message: evidence.Kind, Evidence: &evidence,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifyChecks(world World, checks []Check) error {
	for _, check := range checks {
		switch check.Kind {
		case CheckAllocationReady:
			observed := world.Allocations[check.Target].ReadyAt(world.Now())
			if fmt.Sprint(observed) != check.Want {
				return fmt.Errorf("check %s for %s: observed %t, want %s", check.Kind, check.Target, observed, check.Want)
			}
		case CheckAgentReady:
			// An agent is ready only if it is serving: reachable and within
			// budget. A draining or exhausted instance is observably running and
			// unable to do work, so readiness alone would overstate it.
			allocation := world.Allocations[check.Target]
			observed := allocation.ReadyAt(world.Now()) &&
				!allocation.Draining && !allocation.Exhausted()
			if fmt.Sprint(observed) != check.Want {
				return fmt.Errorf("check %s for %s: observed %t, want %s", check.Kind, check.Target, observed, check.Want)
			}
		case CheckAllocationDrained:
			observed := world.Allocations[check.Target].Drained()
			if fmt.Sprint(observed) != check.Want {
				return fmt.Errorf("check %s for %s: observed %t, want %s", check.Kind, check.Target, observed, check.Want)
			}
		case CheckRouteReachable:
			observed := world.Routes[check.Target] != nil
			if fmt.Sprint(observed) != check.Want {
				return fmt.Errorf("check %s for %s: observed %t, want %s", check.Kind, check.Target, observed, check.Want)
			}
		default:
			return fmt.Errorf("unknown evidence check %q", check.Kind)
		}
	}
	return nil
}

func (e *Engine) record(event Event) error {
	event.Sequence = uint64(len(e.Events) + 1)
	if e.Sink != nil {
		event.Sequence = e.Sink.NextSequence()
	}
	event.At = e.now().UTC()
	event.WorldRevision = e.World.World().Revision
	if e.Sink != nil {
		if err := e.Sink.Append(event); err != nil {
			return fmt.Errorf("persist %s event: %w", event.Type, err)
		}
	}
	e.Events = append(e.Events, event)
	return nil
}

func goalAchieved(goal Goal, world World) bool {
	if matchingReadyAllocations(goal, world) != goal.Workload.Replicas {
		return false
	}
	if goal.Route == nil {
		return true
	}
	route := world.Routes[goal.Route.Host]
	return route != nil && route.Workload == goal.Workload.Name && route.Port == goal.Route.Port && route.Exposure == goal.Route.Exposure
}
