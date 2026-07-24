// Package server hosts the long-running a4s control plane: durable history, a
// world projection rebuilt from that history, and reconciliation of submitted
// goals against connected nodes.
package server

import (
	"fmt"
	"sync"
	"time"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
)

// Config describes one server instance.
type Config struct {
	// EventLog is the absolute path to the durable, hash-chained history. It is
	// the only authoritative state; everything else is derived from it.
	EventLog string
	// Base holds facts not derived from evidence: node inventory, capacity, and
	// operator approvals.
	Base control.World
	// MaxRounds bounds a single reconciliation so a goal that cannot converge
	// fails loudly instead of looping.
	MaxRounds int
}

// Server owns durable history and the projection derived from it.
//
// It deliberately does not own the data plane. Reconciliation issues signed
// capabilities to nodes through an executor; the server never touches
// containerd, and losing the server does not stop running workloads.
type Server struct {
	mu        sync.Mutex
	log       *eventlog.File
	projector *control.DurableProjector
	agents    []control.Agent
	leases    *control.LeaseManager
	maxRounds int
	goals     map[string]control.Goal
}

// Open starts a server, rebuilding its world from durable history.
//
// Recovery is the default path, not a special case: a server that has just
// crashed and a server starting fresh run exactly the same code, so the
// recovery path cannot rot from disuse.
func Open(config Config, agents ...control.Agent) (*Server, error) {
	if config.EventLog == "" {
		return nil, fmt.Errorf("server requires an event log path")
	}
	log, err := eventlog.Open(config.EventLog)
	if err != nil {
		return nil, err
	}
	projector, err := control.NewDurableProjector(config.Base, log)
	if err != nil {
		log.Close()
		return nil, fmt.Errorf("rebuild world projection: %w", err)
	}
	rounds := config.MaxRounds
	if rounds <= 0 {
		rounds = 12
	}
	return &Server{
		log: log, projector: projector, agents: agents,
		leases: control.NewLeaseManager(), maxRounds: rounds,
		goals: make(map[string]control.Goal),
	}, nil
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		return nil
	}
	err := s.log.Close()
	s.log = nil
	return err
}

// World returns the current projection.
func (s *Server) World() control.World { return s.projector.World() }

// Submit records an operator goal. Validation happens before acceptance so an
// unsatisfiable goal is rejected at the boundary rather than failing later
// inside reconciliation.
func (s *Server) Submit(goal control.Goal) error {
	scenario := control.Scenario{Goal: goal, World: s.projector.World()}
	if err := scenario.NormalizeAndValidate(); err != nil {
		return fmt.Errorf("reject goal %q: %w", goal.ID, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.goals[goal.ID] = goal
	return nil
}

// Goals returns the accepted goals.
func (s *Server) Goals() []control.Goal {
	s.mu.Lock()
	defer s.mu.Unlock()
	goals := make([]control.Goal, 0, len(s.goals))
	for _, goal := range s.goals {
		goals = append(goals, goal)
	}
	return goals
}

// Reconcile drives one accepted goal toward convergence using the supplied
// executor and probers.
//
// The lease manager is shared across reconciliations, so two goals that touch
// the same allocation cannot interleave even when driven concurrently.
func (s *Server) Reconcile(goalID string, executor control.Executor, probers ...control.Prober) error {
	s.mu.Lock()
	goal, ok := s.goals[goalID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("goal %q was never accepted", goalID)
	}

	engine := control.NewEngineWith(executor, s.projector, s.agents...)
	engine.Leases = s.leases
	engine.WithEventSink(s.log)
	if len(probers) > 0 {
		engine.WithProbers(probers...)
	}
	return engine.Run(goal, s.maxRounds)
}

// Rebuild recomputes the world from durable history. It proves the projection
// really is a function of the log rather than of accumulated in-memory state.
func (s *Server) Rebuild() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		return fmt.Errorf("event log is closed")
	}
	return s.projector.Rebuild(s.log)
}

// History returns the durable event records.
func (s *Server) History() []eventlog.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		return nil
	}
	return s.log.Records()
}

// Events returns the recorded control events in order.
func (s *Server) Events() []control.Event {
	records := s.History()
	events := make([]control.Event, 0, len(records))
	for _, record := range records {
		events = append(events, record.Event)
	}
	return events
}

// Explain reconstructs why a target is in its current state from durable
// history. It reads the log and mutates nothing.
func (s *Server) Explain(target string) control.Explanation {
	return control.Explain(s.Events(), target)
}

// Diagnose explains why a goal is not converging. The diagnoser holds no
// capability grants, so this is safe to run at any time.
func (s *Server) Diagnose(goalID string, diagnoser control.Diagnoser) control.Diagnosis {
	if diagnoser == nil {
		diagnoser = control.LogDiagnoser{}
	}
	return diagnoser.Diagnose(goalID, s.Events(), s.World())
}

// Plan reports what reconciliation would do to the current world without
// touching anything.
func (s *Server) Plan(goalID string) (control.Plan, error) {
	s.mu.Lock()
	goal, ok := s.goals[goalID]
	s.mu.Unlock()
	if !ok {
		return control.Plan{}, fmt.Errorf("goal %q was never accepted", goalID)
	}
	kernel := control.Kernel{Policy: control.DefaultPolicy()}
	return control.DryRun(kernel, s.projector.World(), goal, s.agents...), nil
}

// Status summarizes the server for an operator.
type Status struct {
	Revision    uint64    `json:"revision"`
	ObservedAt  time.Time `json:"observed_at"`
	Goals       int       `json:"goals"`
	Nodes       int       `json:"nodes"`
	Allocations int       `json:"allocations"`
	Routes      int       `json:"routes"`
	Events      uint64    `json:"events"`
}

func (s *Server) Status() Status {
	world := s.projector.World()
	s.mu.Lock()
	goals := len(s.goals)
	events := uint64(0)
	if s.log != nil {
		events = s.log.NextSequence() - 1
	}
	s.mu.Unlock()
	return Status{
		Revision: world.Revision, ObservedAt: world.ObservedAt, Goals: goals,
		Nodes: len(world.Nodes), Allocations: len(world.Allocations),
		Routes: len(world.Routes), Events: events,
	}
}
