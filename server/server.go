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
	// ImageSigners are the build signers whose provenance attestations this
	// cluster accepts, by key id. Public keys only: the server verifies that
	// something trusted built an image and can never attest to one itself.
	ImageSigners map[string]ed25519.PublicKey
	// RequireSignedImages refuses to run any image without a valid attestation.
	// A production deployment should set it; see docs/security.md.
	RequireSignedImages bool
	// ClusterCeiling, MaxAllocations, and ClusterBudget bound what the whole
	// cluster may commit at once. Zero in any dimension means no ceiling there,
	// which is the behaviour of a server configured before these existed.
	ClusterCeiling control.Resources
	MaxAllocations int
	ClusterBudget  control.Budget
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
	// policy is the kernel policy every reconciliation and plan runs under, so
	// a dry run cannot be authorized under different rules than the execution
	// it is supposed to predict.
	policy control.Policy
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
	policy := control.DefaultPolicy()
	policy.RequireSignedImages = config.RequireSignedImages
	policy.ClusterCeiling = config.ClusterCeiling
	policy.MaxAllocations = config.MaxAllocations
	policy.ClusterBudget = config.ClusterBudget
	policy.ImageSigners = make(map[string]ed25519.PublicKey, len(config.ImageSigners))
	for id, key := range config.ImageSigners {
		policy.ImageSigners[id] = key
	}
	return &Server{
		log: log, projector: projector, agents: agents,
		leases: control.NewLeaseManager(), maxRounds: rounds,
		goals: make(map[string]control.Goal), operatorKeys: keys,
		anchor: anchor, policy: policy,
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
	engine.Kernel = control.Kernel{Policy: s.policy}
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

// MaxRecordBatch bounds how many records one replication request returns.
//
// The follower's response reader is bounded, so an unbounded batch would be
// truncated mid-record and the chain would fail to derive rather than fail to
// arrive — a confusing way to report "the log is long". Batching makes catching
// up a loop instead of one large transfer.
const MaxRecordBatch = 500

// Records returns hashed records after a sequence, for a follower catching up.
//
// Hashes travel with the events on purpose. A follower re-derives every hash
// against its own chain and compares, which is what makes agreement mean it
// computed the same history rather than that it faithfully copied whatever it
// was sent. Sending bare events would make that check impossible.
func (s *Server) Records(after uint64, limit int) []eventlog.Record {
	if limit <= 0 || limit > MaxRecordBatch {
		limit = MaxRecordBatch
	}
	records := s.History()
	if uint64(len(records)) <= after {
		return nil
	}
	batch := records[after:]
	if len(batch) > limit {
		batch = batch[:limit]
	}
	return batch
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

	return s.appendObservation("operator:"+grant.IssuedBy, grant.GoalID, grant.ID,
		message+": "+grant.Scope, evidence)
}

// appendObservation writes a control-plane-originated fact to durable history,
// witnesses it, and applies it to the projection.
//
// Order matters: a projection updated without a durable record would vanish on
// restart, silently withdrawing a decision the operator was told had taken
// effect. Every such change goes through here so none of them can acquire a
// different durability story by accident.
//
// The actor is passed whole rather than assembled here, because these are not
// all operators: a reachability observation is the controller reporting on its
// own transport, and attributing it to a person would put a decision nobody
// made into the audit trail.
func (s *Server) appendObservation(actor, goalID, target, message string,
	evidence control.Evidence) error {

	if s.log == nil {
		return fmt.Errorf("event log is closed")
	}
	if err := s.log.Append(control.Event{
		Sequence: s.log.NextSequence(), At: evidence.ObservedAt,
		Type: control.EventObservationRecorded, Actor: actor,
		GoalID: goalID, Target: target, Kind: evidence.Kind,
		Message: message, Evidence: &evidence,
	}); err != nil {
		return fmt.Errorf("record observation: %w", err)
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

// Cordon takes a node out of service on an operator's instruction.
//
// The remediation agent already cordons a node it has observed to be failing,
// which covers the unplanned case. This covers the other one: a machine an
// operator is about to open up, where nothing is wrong yet and nothing will
// observe a reason to stop scheduling onto it until something goes wrong.
//
// Authority comes from the request signature the API already verified, the same
// way a submitted goal's does. A separate signature over the cordon itself would
// only matter if the kernel re-checked it later, and it does not: the durable
// artifact is the evidence, and the hash chain is what protects that.
func (s *Server) Cordon(nodeID, reason, operator string) error {
	return s.setCordon(nodeID, reason, operator, true)
}

// Uncordon returns a node to service.
func (s *Server) Uncordon(nodeID, operator string) error {
	return s.setCordon(nodeID, "", operator, false)
}

func (s *Server) setCordon(nodeID, reason, operator string, cordoned bool) error {
	if nodeID == "" {
		return fmt.Errorf("a node is required")
	}
	if operator == "" {
		return fmt.Errorf("an operator is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Checked against the projection rather than accepted blindly, so a typo
	// becomes a refusal instead of a cordon on a node that does not exist and
	// an operator who believes a machine is out of service when it is not.
	if _, known := s.projector.World().Nodes[nodeID]; !known {
		return fmt.Errorf("node %q is not in the observed world", nodeID)
	}

	kind, message := control.EvidenceNodeCordoned, "node cordoned"
	if !cordoned {
		kind, message = control.EvidenceNodeUncordoned, "node returned to service"
	}
	evidence := control.Evidence{
		Kind: kind, Target: nodeID, Source: "operator:" + operator,
		ObservedAt: time.Now().UTC(),
		Observed:   map[string]string{"node": nodeID, "reason": reason},
	}
	if reason != "" {
		message += ": " + reason
	}
	return s.appendObservation("operator:"+operator, "", nodeID, message, evidence)
}

// ObserveNodes records which nodes the control plane can currently reach.
//
// Node health was previously a fact nobody ever updated: it came from the
// scenario file and stayed there, so a node that died kept attracting
// placements and the remediation agent's first rung could never fire. The
// server already knows which nodes hold a connection; this is what turns that
// into evidence the world is built from.
//
// Evidence is recorded only when reachability changes. A tick that observes the
// same thing as the last one has learned nothing, and appending it every few
// seconds would grow the log with the age of the cluster rather than with its
// activity.
//
// Unreachable stops new placement and nothing more. It is not read as the
// workloads there having stopped: a partitioned node keeps running what it was
// told to run, and relocating on silence is how one workload becomes two.
// Moving anything off it still requires reaching it, or an operator.
func (s *Server) ObserveNodes(connected []string) error {
	reachable := make(map[string]bool, len(connected))
	for _, id := range connected {
		reachable[id] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	world := s.projector.World()

	ids := make([]string, 0, len(world.Nodes))
	for id := range world.Nodes {
		ids = append(ids, id)
	}
	// Sorted so a tick that changes several nodes records them in a stable
	// order, and a rebuilt projection matches the live one exactly.
	sort.Strings(ids)

	for _, id := range ids {
		node := world.Nodes[id]
		if node.Healthy == reachable[id] {
			continue
		}
		kind, message := control.EvidenceNodeUnreachable, "node stopped answering"
		if reachable[id] {
			kind, message = control.EvidenceNodeReachable, "node is answering again"
		}
		evidence := control.Evidence{
			Kind: kind, Target: id, Source: "controller",
			ObservedAt: time.Now().UTC(),
			Observed:   map[string]string{"node": id},
		}
		if err := s.appendObservation("controller", "", id, message, evidence); err != nil {
			return err
		}
	}
	return nil
}

// Evacuation reports what draining a node would cost an operator, without
// changing anything. Cordoning stops new work arriving; this says what is still
// there and which of it holds data that cannot simply be recreated elsewhere.
func (s *Server) Evacuation(nodeID string) control.NodeEvacuation {
	return control.PlanEvacuation(s.projector.World(), nodeID)
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
	// A closed server answers nothing rather than dereferencing a log it no
	// longer holds. History and Rebuild already guard this; a query that did
	// not would turn an ordinary shutdown race into a panic.
	if s.log == nil {
		return nil
	}

	records := s.log.Records()
	events := make([]control.Event, 0, len(records))
	for _, record := range records {
		events = append(events, record.Event)
	}
	return query.Apply(events)
}

// Apply filters events to those matching the query, oldest first, and caps the
// result at the query's limit.
//
// It is exported because the CLI answers `a4s history` straight from an event
// log file rather than through a running server. Both paths run this function,
// which is what keeps them from drifting into answering the same question
// differently.
func (q HistoryQuery) Apply(events []control.Event) []control.Event {
	var matched []control.Event
	for _, event := range events {
		if q.Matches(event) {
			matched = append(matched, event)
		}
	}
	if q.Limit > 0 && len(matched) > q.Limit {
		matched = matched[len(matched)-q.Limit:]
	}
	return matched
}

// Matches reports whether one event satisfies every filter in the query.
func (q HistoryQuery) Matches(event control.Event) bool {
	if q.GoalID != "" && event.GoalID != q.GoalID {
		return false
	}
	if q.Target != "" && event.Target != q.Target {
		// Evidence often names the target the event itself does not, so an
		// operator searching for an allocation finds its observations too.
		if event.Evidence == nil || event.Evidence.Target != q.Target {
			return false
		}
	}
	if q.Kind != "" && string(event.Type) != q.Kind && event.Kind != q.Kind {
		return false
	}
	if !q.Since.IsZero() && event.At.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && event.At.After(q.Until) {
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
	// The same policy reconciliation runs under. A plan authorized under looser
	// rules than execution would predict work the kernel then refuses, which is
	// the one thing a dry run must never do.
	return control.DryRun(control.Kernel{Policy: s.policy},
		s.projector.World(), goal, s.agents...), nil
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
	// The autonomy safeguards, counted so an operator can see them working.
	// Each of these makes a cluster deliberately slower or smaller, and a brake
	// nobody can see is indistinguishable from a fault: without these numbers,
	// "why is nothing happening" has no answer short of reading the log.
	//
	// Schedulable is reported alongside Nodes rather than instead of it,
	// because the gap between the two is the whole point.
	Schedulable int `json:"schedulable"`
	Cordoned    int `json:"cordoned"`
	// Disruptions counts disruptive actions inside the governor's window.
	Disruptions int `json:"disruptions"`
	// BackingOff counts targets that may not be recreated yet.
	BackingOff int `json:"backing_off"`
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

	now := world.Now()
	schedulable, cordoned := 0, 0
	for _, node := range world.Nodes {
		if node.Schedulable() {
			schedulable++
		}
		if node.Cordoned {
			cordoned++
		}
	}
	backingOff := 0
	for _, state := range world.Backoff {
		if state.Active(now) {
			backingOff++
		}
	}
	return Status{
		Revision: world.Revision, ObservedAt: world.ObservedAt, Goals: goals,
		Nodes: len(world.Nodes), Allocations: len(world.Allocations),
		Routes: len(world.Routes), Events: events,
		Schedulable: schedulable, Cordoned: cordoned,
		Disruptions: len(control.RecentDisruptions(world, now, control.DefaultDisruptionWindow)),
		BackingOff:  backingOff,
	}
}
