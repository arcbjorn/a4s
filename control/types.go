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
	// Secrets names material this workload needs. Only references appear here.
	Secrets []SecretRef `json:"secrets,omitempty"`
	// Volumes names durable storage this workload needs. A workload declaring
	// volumes is stateful, which changes what the kernel will authorize.
	Volumes []VolumeRef `json:"volumes,omitempty"`
}

// VolumeRef names durable storage a workload requires.
type VolumeRef struct {
	// Name identifies the volume within the cluster.
	Name string `json:"name"`
	// MountPath is where the workload expects to find it.
	MountPath string `json:"mount_path"`
	// ReadOnly mounts without write access, which is safe to share.
	ReadOnly bool `json:"read_only,omitempty"`
	// Checksum is the recorded checksum a restore must verify against. The
	// controller supplies it from the world; the node refuses a mismatch rather
	// than writing unverified content over live data.
	Checksum string `json:"checksum,omitempty"`
}

// Volume is durable storage with an owner and a home.
//
// A volume is an explicit object rather than an implicit side effect of a
// container, because the thing that must not be lost has to be nameable
// independently of the process using it.
type Volume struct {
	Name string `json:"name"`
	// Node is where the data physically lives. Local storage stays local: a
	// volume does not follow a workload to another node without an explicit,
	// evidenced handoff.
	Node string `json:"node"`
	// Owner is the allocation currently permitted to write. Empty means the
	// volume is unattached and available.
	Owner string `json:"owner,omitempty"`
	// Generation increments on every ownership change. A node holding an older
	// generation is fenced: it may no longer write, even if it still believes
	// it owns the volume. This is what makes a partition safe.
	Generation uint64 `json:"generation"`
	// SizeMB is the provisioned size.
	SizeMB int `json:"size_mb,omitempty"`
	// LastSnapshot records the most recent verified snapshot, which is what
	// makes a destructive action recoverable.
	LastSnapshot string `json:"last_snapshot,omitempty"`
	// Snapshots records every verified snapshot by id, with its checksum. A
	// restore names one of these, so an operator cannot restore something this
	// cluster never took and verified.
	Snapshots map[string]string `json:"snapshots,omitempty"`
	// RestoredFrom records the snapshot this volume was last restored from,
	// which is what an operator needs to know after a recovery.
	RestoredFrom string `json:"restored_from,omitempty"`
}

// SecretRef names secret material without carrying it.
//
// There is deliberately no field capable of holding a value. A goal travels
// through agent context, proposals, events, and the durable log, and a struct
// with nowhere to put a secret cannot leak one no matter how it is serialized,
// logged, or handed to a model.
type SecretRef struct {
	// Name identifies the secret to the broker. It is an opaque handle, not a
	// path and not a value.
	Name string `json:"name"`
	// Version pins which revision to mount. Rotating a secret means changing
	// this, so a rollout is an ordinary goal change with an audit trail.
	Version string `json:"version"`
	// MountPath is where the node places the decrypted material inside the
	// container's filesystem.
	MountPath string `json:"mount_path"`
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

// Subtract releases resources, clamping at zero. Capacity accounting must never
// go negative even if evidence arrives out of order or is replayed.
func (r Resources) Subtract(other Resources) Resources {
	result := Resources{CPUMillis: r.CPUMillis - other.CPUMillis, MemoryMB: r.MemoryMB - other.MemoryMB}
	if result.CPUMillis < 0 {
		result.CPUMillis = 0
	}
	if result.MemoryMB < 0 {
		result.MemoryMB = 0
	}
	return result
}

func (r Resources) Fits(capacity Resources) bool {
	return r.CPUMillis <= capacity.CPUMillis && r.MemoryMB <= capacity.MemoryMB
}

type World struct {
	Revision uint64 `json:"revision"`
	// ObservedAt is the time this snapshot represents. Freshness checks compare
	// perishable observations against it, so evaluation is deterministic and
	// does not depend on a hidden call to the system clock.
	ObservedAt  time.Time              `json:"observed_at,omitempty"`
	Nodes       map[string]*Node       `json:"nodes"`
	Allocations map[string]*Allocation `json:"allocations,omitempty"`
	Routes      map[string]*Route      `json:"routes,omitempty"`
	Volumes     map[string]*Volume     `json:"volumes,omitempty"`
	Approvals   map[string]*Approval   `json:"approvals,omitempty"`
	// KnownGood records, per workload, the last image digest observed serving.
	// A rollout can only roll back to a version this cluster actually saw
	// working, never to one that merely looks older.
	KnownGood map[string]string `json:"known_good,omitempty"`
}

// Now returns the snapshot's evaluation time, falling back to the wall clock
// for worlds built before observation time was recorded.
func (w World) Now() time.Time {
	if w.ObservedAt.IsZero() {
		return time.Now()
	}
	return w.ObservedAt
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
	// Volumes records the volumes attached to this allocation, with the
	// generation each was attached at. A generation mismatch means this
	// allocation has been fenced and must not write.
	Volumes map[string]uint64 `json:"volumes,omitempty"`
	// Secrets records the secret versions mounted for this allocation. Versions
	// only: the world projection never holds secret material.
	Secrets map[string]string `json:"secrets,omitempty"`
	// Address is the allocation's own IP, assigned by CNI. Each allocation has
	// its own network namespace, so replicas of one workload can run on the
	// same node without contending for a host port.
	Address  string `json:"address,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
	// ReadyExpiresAt is when the readiness observation stops being trustworthy.
	// Zero means readiness was never observed with an expiry.
	ReadyExpiresAt time.Time `json:"ready_expires_at,omitempty"`
}

// ReadyAt reports whether the allocation is ready and that readiness has not
// expired. Goal satisfaction must use this rather than the raw Ready flag, so a
// stale observation cannot keep a dead workload looking healthy.
func (a *Allocation) ReadyAt(now time.Time) bool {
	if a == nil || !a.Ready {
		return false
	}
	return a.ReadyExpiresAt.IsZero() || now.Before(a.ReadyExpiresAt)
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
	ActionAttachNetwork    ActionKind = "attach_network"
	ActionMountSecret      ActionKind = "mount_secret"
	ActionCreateVolume     ActionKind = "create_volume"
	ActionAttachVolume     ActionKind = "attach_volume"
	ActionDetachVolume     ActionKind = "detach_volume"
	ActionSnapshotVolume   ActionKind = "snapshot_volume"
	ActionRestoreSnapshot  ActionKind = "restore_snapshot"
	ActionStartAllocation  ActionKind = "start_allocation"
	ActionStopAllocation   ActionKind = "stop_allocation"
	ActionDeleteAllocation ActionKind = "delete_allocation"
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
	// Volume names the volume this action operates on.
	Volume *VolumeRef `json:"volume,omitempty"`
	// Snapshot names the snapshot to restore from.
	Snapshot string `json:"snapshot,omitempty"`
	// Secret names the reference to mount. An action carries the reference, not
	// the material, so a proposal remains safe to log in full.
	Secret    *SecretRef `json:"secret,omitempty"`
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
	Kind   string `json:"kind"`
	Target string `json:"target"`
	// Source names what produced the evidence. Executor-produced and
	// probe-produced evidence are deliberately distinguishable.
	Source string `json:"source,omitempty"`
	// ObservedAt and ExpiresAt bound how long an observation may be trusted. A
	// readiness observation is a perishable fact, not a permanent one: the
	// service can stop serving at any time after it was measured.
	ObservedAt time.Time         `json:"observed_at,omitempty"`
	ExpiresAt  time.Time         `json:"expires_at,omitempty"`
	Observed   map[string]string `json:"observed"`
}

// Fresh reports whether the observation can still be trusted at the given time.
// Evidence without an expiry is treated as fresh, because state-change evidence
// such as allocation.created does not decay.
func (e Evidence) Fresh(now time.Time) bool {
	return e.ExpiresAt.IsZero() || now.Before(e.ExpiresAt)
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
	Sequence   uint64    `json:"sequence"`
	At         time.Time `json:"at"`
	Type       EventType `json:"type"`
	Actor      string    `json:"actor"`
	GoalID     string    `json:"goal_id"`
	ProposalID string    `json:"proposal_id,omitempty"`
	ActionID   string    `json:"action_id,omitempty"`
	// Target names what the event acted on. It is recorded at dispatch, before
	// any evidence exists, so an action that never completed can still be
	// attributed to the allocation or route it was about.
	Target        string    `json:"target,omitempty"`
	Kind          string    `json:"kind,omitempty"`
	WorldRevision uint64    `json:"world_revision"`
	Message       string    `json:"message"`
	Evidence      *Evidence `json:"evidence,omitempty"`
}

type Scenario struct {
	Goal  Goal  `json:"goal"`
	World World `json:"world"`
}
