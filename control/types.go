// Package control contains the agentic control-plane kernel.
//
// Agents reason and propose. The kernel authorizes. Executors mutate. Probes
// produce evidence. Keeping those roles separate is the primary safety
// boundary of a4s.
package control

import "time"

const APIVersion = "a4s.io/v1alpha1"

type Goal struct {
	APIVersion  string       `json:"api_version"`
	ID          string       `json:"id"`
	Objective   string       `json:"objective"`
	Workload    WorkloadSpec `json:"workload"`
	Route       *RouteSpec   `json:"route,omitempty"`
	Constraints Constraints  `json:"constraints"`
}

type WorkloadSpec struct {
	Name       string    `json:"name"`
	Image      string    `json:"image"`
	Replicas   int       `json:"replicas"`
	Port       int       `json:"port"`
	Resources  Resources `json:"resources"`
	Privileged bool      `json:"privileged,omitempty"`
	Stateful   bool      `json:"stateful,omitempty"`
}

type RouteSpec struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Exposure string `json:"exposure"`
}

type Constraints struct {
	RequiredLabels map[string]string `json:"required_labels,omitempty"`
	AllowedNodes   []string          `json:"allowed_nodes,omitempty"`
}

type Resources struct {
	CPUMillis int `json:"cpu_millis"`
	MemoryMB  int `json:"memory_mb"`
}

func (r Resources) Add(other Resources) Resources {
	return Resources{CPUMillis: r.CPUMillis + other.CPUMillis, MemoryMB: r.MemoryMB + other.MemoryMB}
}

func (r Resources) Fits(capacity Resources) bool {
	return r.CPUMillis <= capacity.CPUMillis && r.MemoryMB <= capacity.MemoryMB
}

type World struct {
	Revision    uint64                 `json:"revision"`
	Nodes       map[string]*Node       `json:"nodes"`
	Allocations map[string]*Allocation `json:"allocations,omitempty"`
	Routes      map[string]*Route      `json:"routes,omitempty"`
	Approvals   map[string]*Approval   `json:"approvals,omitempty"`
}

// Approval is materialized from a separately authenticated operator event. It
// is never accepted from an agent proposal or self-asserted by a Goal.
type Approval struct {
	ID       string `json:"id"`
	GoalID   string `json:"goal_id"`
	Scope    string `json:"scope"`
	IssuedBy string `json:"issued_by"`
	Granted  bool   `json:"granted"`
}

type Node struct {
	ID       string            `json:"id"`
	Labels   map[string]string `json:"labels,omitempty"`
	Capacity Resources         `json:"capacity"`
	Used     Resources         `json:"used"`
	Images   map[string]bool   `json:"images,omitempty"`
	Healthy  bool              `json:"healthy"`
}

type Allocation struct {
	ID        string          `json:"id"`
	Workload  string          `json:"workload"`
	Replica   int             `json:"replica"`
	Node      string          `json:"node"`
	Image     string          `json:"image"`
	Resources Resources       `json:"resources"`
	Phase     AllocationPhase `json:"phase"`
	Ready     bool            `json:"ready"`
	Stateful  bool            `json:"stateful,omitempty"`
}

type AllocationPhase string

const (
	AllocationCreated AllocationPhase = "created"
	AllocationRunning AllocationPhase = "running"
	AllocationStopped AllocationPhase = "stopped"
)

type Route struct {
	Host     string `json:"host"`
	Workload string `json:"workload"`
	Port     int    `json:"port"`
	Exposure string `json:"exposure"`
}

type AgentDescriptor struct {
	ID           string       `json:"id"`
	Role         string       `json:"role"`
	Capabilities []ActionKind `json:"capabilities"`
}

type Agent interface {
	Descriptor() AgentDescriptor
	Propose(Goal, World) (Proposal, error)
}

type Proposal struct {
	ID               string   `json:"id"`
	AgentID          string   `json:"agent_id"`
	GoalID           string   `json:"goal_id"`
	BasedOnRevision  uint64   `json:"based_on_revision"`
	Reasoning        string   `json:"reasoning"`
	Actions          []Action `json:"actions"`
	ExpectedEvidence []Check  `json:"expected_evidence,omitempty"`
}

type ActionKind string

const (
	ActionPullImage        ActionKind = "pull_image"
	ActionCreateAllocation ActionKind = "create_allocation"
	ActionStartAllocation  ActionKind = "start_allocation"
	ActionPublishRoute     ActionKind = "publish_route"
)

type Action struct {
	ID        string     `json:"id"`
	Kind      ActionKind `json:"kind"`
	Target    string     `json:"target"`
	Workload  string     `json:"workload,omitempty"`
	Node      string     `json:"node,omitempty"`
	Image     string     `json:"image,omitempty"`
	Replica   int        `json:"replica,omitempty"`
	Resources Resources  `json:"resources,omitempty"`
	Port      int        `json:"port,omitempty"`
	Exposure  string     `json:"exposure,omitempty"`
	DependsOn []string   `json:"depends_on,omitempty"`
}

// Check declares evidence a proposal must produce before it is considered
// complete. The kernel requires these declarations up front so that an agent
// states its success criteria before acting.
const (
	CheckAllocationReady = "allocation_ready"
	CheckRouteReachable  = "route_reachable"
)

type Check struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Want   string `json:"want"`
}

type Evidence struct {
	Kind     string            `json:"kind"`
	Target   string            `json:"target"`
	Observed map[string]string `json:"observed"`
}

type EventType string

const (
	EventGoalAccepted     EventType = "goal.accepted"
	EventProposalCreated  EventType = "proposal.created"
	EventProposalApproved EventType = "proposal.approved"
	EventProposalDenied   EventType = "proposal.denied"
	EventActionDispatched EventType = "action.dispatched"
	EventActionCompleted  EventType = "action.completed"
	// EventObservationRecorded carries probe evidence produced independently
	// of the executor that performed the mutation.
	EventObservationRecorded EventType = "observation.recorded"
	EventGoalAchieved        EventType = "goal.achieved"
	EventGoalBlocked         EventType = "goal.blocked"
)

type Event struct {
	Sequence      uint64    `json:"sequence"`
	At            time.Time `json:"at"`
	Type          EventType `json:"type"`
	Actor         string    `json:"actor"`
	GoalID        string    `json:"goal_id"`
	ProposalID    string    `json:"proposal_id,omitempty"`
	ActionID      string    `json:"action_id,omitempty"`
	WorldRevision uint64    `json:"world_revision"`
	Message       string    `json:"message"`
	Evidence      *Evidence `json:"evidence,omitempty"`
}

type Scenario struct {
	Goal  Goal  `json:"goal"`
	World World `json:"world"`
}
