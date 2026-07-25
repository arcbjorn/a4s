// Package server hosts the long-running a4s control plane: durable history, a
// world projection rebuilt from that history, and reconciliation of submitted
// goals against connected nodes.
package server

import (
	"crypto/ed25519"
	"fmt"
	"sort"
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
	// OperatorKeys are the public keys permitted to sign approvals, by key id.
	// An empty map means no approval can be accepted, which is the correct
	// default: a server that has not been told who its operators are must not
	// authorize public exposure or destroying data.
	OperatorKeys map[string]ed25519.PublicKey
	// Anchor is an absolute path to the external witness of the chain head. The
	// hash chain catches an edit; only an outside record catches replacement of
	// the whole store. Empty disables it, which leaves that gap open.
	Anchor string
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
	// operatorKeys authenticate approvals. The server holds public keys only:
	// it verifies operator decisions and can never make one.
	operatorKeys map[string]ed25519.PublicKey
	// anchor witnesses the chain head outside the store. Nil when disabled.
	anchor *eventlog.Anchor
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
	// The anchor is checked before the projection is built. Rebuilding a world
	// from a substituted log and only then noticing would mean the server had
	// already acted on forged history.
	var anchor *eventlog.Anchor
	if config.Anchor != "" {
		anchor, err = eventlog.OpenAnchor(config.Anchor)
		if err != nil {
			log.Close()
			return nil, err
		}
		if err := anchor.Check(log); err != nil {
			log.Close()
			return nil, fmt.Errorf("event log failed its anchor check: %w", err)
		}
	}
	projector, err := control.NewDurableProjector(config.Base, log)
	if err != nil {
		log.Close()
		return nil, fmt.Errorf("rebuild world projection: %w", err)
	}
	if anchor != nil {
		// Witness the recovered head so a later replacement is detectable even if
		// nothing is appended this run.
		head := log.Head()
		if head.Hash != "" {
			if err := anchor.Witness(head.Sequence, head.Hash); err != nil {
				log.Close()
				return nil, fmt.Errorf("witness recovered chain head: %w", err)
			}
		}
	}
	rounds := config.MaxRounds
	if rounds <= 0 {
		rounds = 12
	}
	keys := make(map[string]ed25519.PublicKey, len(config.OperatorKeys))
	for id, key := range config.OperatorKeys {
		keys[id] = key
	}
	return &Server{
		log: log, projector: projector, agents: agents,
		leases: control.NewLeaseManager(), maxRounds: rounds,
		goals: make(map[string]control.Goal), operatorKeys: keys,
		anchor: anchor,
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
	err := engine.Run(goal, s.maxRounds)
	// Witness whatever history now exists, including when the run failed: the
	// events recorded before the failure are still history worth anchoring.
	if anchorErr := s.witnessHead(); anchorErr != nil && err == nil {
		err = anchorErr
	}
	return err
}

// witnessHead records the current chain head in the external anchor.
func (s *Server) witnessHead() error {
	s.mu.Lock()
	anchor, log := s.anchor, s.log
	s.mu.Unlock()
	if anchor == nil || log == nil {
		return nil
	}
	head := log.Head()
	if head.Hash == "" {
		return nil
	}
	return anchor.Witness(head.Sequence, head.Hash)
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

// Approve records a signed operator decision.
//
// The signature is verified before anything is written, so an unauthenticated
// grant never reaches the log or the world. This is the only path by which
// authority enters the system from outside: every other input is either an
// observation of a fact or an agent proposal, and neither can authorize
// public exposure, data destruction, or a mutating tool grant.
func (s *Server) Approve(signed control.SignedApproval) (control.ApprovalGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	grant, err := control.VerifyApproval(signed, s.operatorKeys, time.Now())
	if err != nil {
		return control.ApprovalGrant{}, err
	}
	// The goal must already exist. Approving something not yet submitted would
	// let an operator pre-authorize a goal whose contents they have not seen.
	if _, known := s.goals[grant.GoalID]; !known {
		return control.ApprovalGrant{}, fmt.Errorf(
			"goal %q has not been submitted; approve it after submitting", grant.GoalID)
	}
	if err := s.recordDecision(grant, grant.Evidence(), "approval granted"); err != nil {
		return control.ApprovalGrant{}, err
	}
	return grant, nil
}

// recordDecision appends an operator decision to durable history, then applies
// it to the projection.
//
// Order matters: a projection updated without a durable record would vanish on
// restart, silently withdrawing an authorization the operator was told had been
// granted.
func (s *Server) recordDecision(grant control.ApprovalGrant, evidence control.Evidence,
	message string) error {

	if err := s.log.Append(control.Event{
		Sequence: s.log.NextSequence(), At: evidence.ObservedAt,
		Type: control.EventObservationRecorded, Actor: "operator:" + grant.IssuedBy,
		GoalID: grant.GoalID, Target: grant.ID, Kind: evidence.Kind,
		Message: message + ": " + grant.Scope, Evidence: &evidence,
	}); err != nil {
		return fmt.Errorf("record operator decision: %w", err)
	}
	// Witnessed inline rather than through witnessHead, because callers already
	// hold the mutex. An operator decision is exactly the kind of history worth
	// anchoring: it is what authorizes public exposure and destroying data.
	if s.anchor != nil {
		if head := s.log.Head(); head.Hash != "" {
			if err := s.anchor.Witness(head.Sequence, head.Hash); err != nil {
				return fmt.Errorf("witness operator decision: %w", err)
			}
		}
	}
	return s.projector.Project(evidence)
}

// Revoke withdraws a grant before it expires.
//
// Revocation is authenticated the same way granting is: withdrawing another
// operator's approval is as consequential as issuing one, and an unauthenticated
// revoke would be a denial-of-service on every gated action.
func (s *Server) Revoke(signed control.SignedApproval) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	grant, err := control.VerifyApproval(signed, s.operatorKeys, time.Now())
	if err != nil {
		return err
	}
	if _, known := s.projector.World().Approvals[grant.ID]; !known {
		return fmt.Errorf("approval %q was never granted", grant.ID)
	}
	return s.recordDecision(grant, control.Evidence{
		Kind: control.EvidenceApprovalRevoked, Target: grant.ID,
		Source: "operator:" + grant.IssuedBy, ObservedAt: time.Now().UTC(),
		Observed: map[string]string{
			"goal": grant.GoalID, "scope": grant.Scope,
			"revoked_by": grant.IssuedBy, "reason": grant.Reason,
		},
	}, "approval revoked")
}

// OperatorKeys returns the public keys permitted to sign operator statements.
//
// A copy is returned so a caller cannot widen the server's trust by mutating
// the map it was handed. The server holds public keys only and can never sign
// an operator decision itself.
func (s *Server) OperatorKeys() map[string]ed25519.PublicKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make(map[string]ed25519.PublicKey, len(s.operatorKeys))
	for id, key := range s.operatorKeys {
		keys[id] = key
	}
	return keys
}

// Approvals reports operator decisions, live ones first.
//
// Expired and revoked grants are included rather than hidden. An operator
// asking "is this approved" is often really asking "was it, and what happened",
// and an empty answer cannot distinguish never-granted from lapsed.
func (s *Server) Approvals() []control.Approval {
	s.mu.Lock()
	defer s.mu.Unlock()

	world := s.projector.World()
	now := world.Now()
	approvals := make([]control.Approval, 0, len(world.Approvals))
	for _, approval := range world.Approvals {
		approvals = append(approvals, *approval)
	}
	sort.Slice(approvals, func(i, j int) bool {
		live, other := approvals[i].Valid(now), approvals[j].Valid(now)
		if live != other {
			return live
		}
		if !approvals[i].IssuedAt.Equal(approvals[j].IssuedAt) {
			return approvals[i].IssuedAt.After(approvals[j].IssuedAt)
		}
		return approvals[i].ID < approvals[j].ID
	})
	return approvals
}

// HistoryQuery narrows recorded history to what an operator asked about.
//
// Every field is an optional filter, and an empty query returns everything.
// The zero value therefore behaves exactly as the unfiltered History call it
// generalizes.
type HistoryQuery struct {
	GoalID string
	Target string
	// Kind matches either an event type or an evidence kind, because an
	// operator asking about "approval.granted" should not have to know which of
	// the two it is.
	Kind string
	// Since and Until bound the window.
	Since time.Time
	Until time.Time
	// Limit caps the result, keeping the most recent entries. Zero means all.
	Limit int
}

// Query returns recorded events matching a query, oldest first.
//
// This exists because History returns the entire log, which stops being usable
// on the first day a cluster is busy. It scans rather than indexes: the log is
// the authority, and an index that could disagree with it would be worse than a
// scan that cannot.
func (s *Server) Query(query HistoryQuery) []control.Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	var matched []control.Event
	for _, record := range s.log.Records() {
		if matchesQuery(record.Event, query) {
			matched = append(matched, record.Event)
		}
	}
	if query.Limit > 0 && len(matched) > query.Limit {
		matched = matched[len(matched)-query.Limit:]
	}
	return matched
}

func matchesQuery(event control.Event, query HistoryQuery) bool {
	if query.GoalID != "" && event.GoalID != query.GoalID {
		return false
	}
	if query.Target != "" && event.Target != query.Target {
		// Evidence often names the target the event itself does not, so an
		// operator searching for an allocation finds its observations too.
		if event.Evidence == nil || event.Evidence.Target != query.Target {
			return false
		}
	}
	if query.Kind != "" && string(event.Type) != query.Kind && event.Kind != query.Kind {
		return false
	}
	if !query.Since.IsZero() && event.At.Before(query.Since) {
		return false
	}
	if !query.Until.IsZero() && event.At.After(query.Until) {
		return false
	}
	return true
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

// Directory reports which endpoints are currently serving each workload. It is
// derived from verified evidence, so a service resolves only to instances that
// were actually observed serving.
func (s *Server) Directory() map[string]control.Service {
	return control.BuildDirectory(s.projector.World(), s.workloadPorts())
}

// RouteSnapshots resolves every published route to its serving endpoints. This
// is the complete, atomic input a gateway consumes.
// ServiceZone renders the names one node's resolver should answer.
//
// It is built from the same directory the gateway consumes, so a name resolves
// exactly when the route layer would route it. Deriving the two independently
// would let them disagree, and a name resolving to an instance the gateway
// will not serve is worse than one that does not resolve.
func (s *Server) ServiceZone(nodeID string) control.ServiceZone {
	return control.BuildServiceZone(s.projector.World(), s.workloadPorts(), nodeID)
}

// RouteSnapshots resolves every route to its serving endpoints, weighted by any
// canary the owning goal declares.
//
// The goals are passed in so a canary's traffic share is computed from the same
// accepted goal the rollout is working toward. Weights are the kernel's
// arithmetic over observed readiness; no agent supplies them.
func (s *Server) RouteSnapshots() []control.RouteSnapshot {
	return control.BuildWeightedRouteSnapshots(
		s.projector.World(), s.workloadPorts(), s.Goals())
}

// StaleRoutes reports routes with no healthy endpoint, which is what an
// operator needs when a hostname stops resolving.
func (s *Server) StaleRoutes() []string {
	world := s.projector.World()
	return control.StaleAfter(world, s.workloadPorts(), world.Now())
}

// workloadPorts derives dialable ports from accepted goals.
func (s *Server) workloadPorts() map[string]int {
	return control.WorkloadPorts(s.Goals())
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
