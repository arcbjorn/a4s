package control

import (
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
	Events   []Event
	Sink     EventSink
	now      func() time.Time
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
	return &Engine{
		Kernel: Kernel{Policy: DefaultPolicy()}, Agents: agents,
		Executor: executor, World: memoryProjector{executor: executor},
		Probers: []Prober{OptimisticProber{}}, now: time.Now,
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
		if goalAchieved(goal, e.World.World()) {
			return e.record(Event{Type: EventGoalAchieved, Actor: "verifier", GoalID: goal.ID, Message: "goal conditions verified from current world evidence"})
		}
		progress := false
		for _, agent := range e.Agents {
			world := e.World.World()
			proposal, err := agent.Propose(goal, world)
			if err != nil {
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
			if err := e.Kernel.Authorize(agent.Descriptor(), goal, world, proposal); err != nil {
				if recordErr := e.record(Event{Type: EventProposalDenied, Actor: "policy-kernel", GoalID: goal.ID, ProposalID: proposal.ID, Message: err.Error()}); recordErr != nil {
					return recordErr
				}
				continue
			}
			if err := e.record(Event{Type: EventProposalApproved, Actor: "policy-kernel", GoalID: goal.ID, ProposalID: proposal.ID, Message: "all actions authorized against a simulated world"}); err != nil {
				return err
			}
			for _, action := range proposal.Actions {
				// Persist intent before dispatch. If completion persistence fails,
				// recovery can query the node's idempotency ledger using this ID.
				if err := e.record(Event{Type: EventActionDispatched, Actor: "coordinator", GoalID: goal.ID, ProposalID: proposal.ID, ActionID: action.ID, Message: string(action.Kind)}); err != nil {
					return err
				}
				evidence, err := e.Executor.Execute(action)
				if err != nil {
					if recordErr := e.record(Event{Type: EventGoalBlocked, Actor: "node-executor", GoalID: goal.ID, ProposalID: proposal.ID, ActionID: action.ID, Message: err.Error()}); recordErr != nil {
						return fmt.Errorf("execute action: %v; persist failure: %w", err, recordErr)
					}
					return err
				}
				// Evidence, not the action, advances the world.
				if err := e.World.Project(evidence); err != nil {
					if recordErr := e.record(Event{Type: EventGoalBlocked, Actor: "projection", GoalID: goal.ID, ProposalID: proposal.ID, ActionID: action.ID, Message: err.Error()}); recordErr != nil {
						return recordErr
					}
					return fmt.Errorf("project evidence for action %q: %w", action.ID, err)
				}
				if err := e.record(Event{Type: EventActionCompleted, Actor: "node-executor", GoalID: goal.ID, ProposalID: proposal.ID, ActionID: action.ID, Message: string(action.Kind), Evidence: &evidence}); err != nil {
					return err
				}
				progress = true
			}
			// Independent probes supply the evidence the executor is not
			// permitted to assert, such as readiness.
			if err := e.observe(goal, proposal); err != nil {
				return err
			}
			if err := verifyChecks(e.World.World(), proposal.ExpectedEvidence); err != nil {
				if recordErr := e.record(Event{Type: EventGoalBlocked, Actor: "verifier", GoalID: goal.ID, ProposalID: proposal.ID, Message: err.Error()}); recordErr != nil {
					return recordErr
				}
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
				ProposalID: proposal.ID, Message: evidence.Kind, Evidence: &evidence,
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
			allocation := world.Allocations[check.Target]
			observed := allocation != nil && allocation.Ready
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
