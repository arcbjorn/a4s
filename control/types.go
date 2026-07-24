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
	// Engine names a database engine when this workload is one. A database is
	// not a generic container: its files are inconsistent when copied while it
	// runs, and it is ready only when it accepts connections. Declaring the
	// engine is what lets the kernel and agents treat it correctly.
	Engine string `json:"engine,omitempty"`
	// Runtime describes an agent workload when this workload is one. An agent is
	// not a generic container: its cost is tokens rather than cpu-seconds, it is
	// ready only when it can reach a model provider with budget remaining, and it
	// acts on the world through granted tools rather than through its network.
	// Declaring the runtime is what lets the kernel and agents treat it
	// correctly.
	//
	// This is a workload kind, not a control-plane Agent. A control agent
	// proposes plans and holds ActionKind grants; an agent workload is scheduled
	// cargo that holds tool grants and never proposes anything. The two never
	// share an authority path.
	Runtime *AgentRuntime `json:"runtime,omitempty"`
}

// AgentRuntime declares how an agent workload runs and what it may spend.
//
// Unlike Engine, which is a bare string, this is structured: an engine's
// behavior is implied by its name, but an agent's budget ceiling, tool grants,
// and provider are per-workload policy inputs the kernel has to enforce. They
// have to be declared fields rather than implied by a runtime name.
type AgentRuntime struct {
	// Name identifies the agent runtime image contract the workload implements.
	// The runtime is responsible for the model loop; a4s is responsible for
	// bounding it.
	Name string `json:"name"`
	// Provider names the model provider this agent needs to reach. It is a
	// scheduling input: a node without egress to this provider cannot run this
	// workload, the same way a node without a pinned image cannot.
	Provider string `json:"provider"`
	// Model pins the model this agent runs. Like an image digest, an unpinned
	// model means the workload silently changes behavior when a provider moves
	// its alias, so it is required rather than defaulted.
	Model string `json:"model"`
	// Budget bounds what one agent instance may consume before it is stopped.
	// This is the agent equivalent of a resource limit: without it, a looping
	// agent's cost is unbounded in a way a cpu limit does not constrain.
	Budget Budget `json:"budget"`
	// Tools lists the capabilities this agent may invoke. This is the agent's
	// blast radius and the reason the kernel can authorize an agent workload up
	// front despite not knowing what it will decide to do: the grant envelope is
	// checked before the agent starts, and the agent cannot widen it at runtime.
	Tools []ToolGrant `json:"tools,omitempty"`
	// Queue names the work queue this agent pulls tasks from. Empty means the
	// agent runs a single task per allocation rather than serving a queue.
	Queue string `json:"queue,omitempty"`
}

// Budget bounds an agent workload's consumption.
//
// These are a resource dimension distinct from Resources. A cpu limit bounds how
// fast an agent burns money; it does not bound how much. An agent that spends
// its context on a provider call is idle by cgroup accounting and expensive in
// every way that matters, so the kernel schedules against both.
type Budget struct {
	// Tokens is the ceiling on total tokens one instance may consume.
	Tokens int `json:"tokens"`
	// CostMillis is the ceiling in thousandths of a currency unit. Tokens alone
	// do not bound cost when models differ in price by an order of magnitude.
	CostMillis int `json:"cost_millis"`
	// WallSeconds bounds how long one task may run. An agent blocked on a slow
	// tool consumes neither tokens nor cost while still holding its allocation.
	WallSeconds int `json:"wall_seconds"`
	// ToolCalls bounds how many tool invocations one task may make. This is the
	// loop breaker: an agent thrashing between two tools can stay under every
	// other ceiling indefinitely.
	ToolCalls int `json:"tool_calls"`
}

// Fits reports whether this budget is within the given ceiling.
//
// This is the reservation question: may a node commit this much. It is
// inclusive, because a reservation exactly equal to remaining capacity is
// legitimate. Consumption asks a different question; see Exhausts.
func (b Budget) Fits(ceiling Budget) bool {
	return b.Tokens <= ceiling.Tokens && b.CostMillis <= ceiling.CostMillis &&
		b.WallSeconds <= ceiling.WallSeconds && b.ToolCalls <= ceiling.ToolCalls
}

// Exhausts reports whether this much consumption uses up the given ceiling.
//
// This is the spending question, and it is deliberately not the negation of
// Fits. An instance that has consumed exactly its ceiling has nothing left: a
// budget of five tool calls permits five calls and refuses the sixth. Treating
// that as "still fits" would grant one more of everything than was authorized,
// on every dimension, for every agent.
func (b Budget) Exhausts(ceiling Budget) bool {
	return b.Tokens >= ceiling.Tokens || b.CostMillis >= ceiling.CostMillis ||
		b.WallSeconds >= ceiling.WallSeconds || b.ToolCalls >= ceiling.ToolCalls
}

// Add accumulates budget, which is how per-instance ceilings sum into what a
// node has committed.
func (b Budget) Add(other Budget) Budget {
	return Budget{
		Tokens:      b.Tokens + other.Tokens,
		CostMillis:  b.CostMillis + other.CostMillis,
		WallSeconds: b.WallSeconds + other.WallSeconds,
		ToolCalls:   b.ToolCalls + other.ToolCalls,
	}
}

// Subtract releases budget, clamping at zero. Like Resources.Subtract, this must
// never go negative even if evidence arrives out of order or is replayed.
func (b Budget) Subtract(other Budget) Budget {
	result := Budget{
		Tokens:      b.Tokens - other.Tokens,
		CostMillis:  b.CostMillis - other.CostMillis,
		WallSeconds: b.WallSeconds - other.WallSeconds,
		ToolCalls:   b.ToolCalls - other.ToolCalls,
	}
	if result.Tokens < 0 {
		result.Tokens = 0
	}
	if result.CostMillis < 0 {
		result.CostMillis = 0
	}
	if result.WallSeconds < 0 {
		result.WallSeconds = 0
	}
	if result.ToolCalls < 0 {
		result.ToolCalls = 0
	}
	return result
}

// IsZero reports whether no budget is declared at all.
func (b Budget) IsZero() bool {
	return b == Budget{}
}

// ToolGrant is one capability an agent workload may invoke.
//
// A tool grant is deliberately not an ActionKind. Control agents propose typed
// infrastructure actions the kernel executes; agent workloads call tools that
// act outside a4s entirely. Sharing one vocabulary would make it possible to
// grant a workload an infrastructure mutation, which is exactly the authority
// path that must not exist.
type ToolGrant struct {
	// Name identifies the tool to the runtime.
	Name string `json:"name"`
	// Scope narrows what the tool may touch, such as a repository, a bucket
	// prefix, or a read-only qualifier. A tool without a scope is granted
	// whatever the runtime's credential allows, so the kernel requires one.
	Scope string `json:"scope"`
	// Mutating marks a tool that changes state outside a4s. Mutating grants are
	// what make an agent's blast radius real, so they are approved separately
	// from read-only ones.
	Mutating bool `json:"mutating,omitempty"`
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
	// SnapshotOrder lists snapshot ids oldest first. Pruning needs an order,
	// and taking it from a map would be non-deterministic. The list is the
	// authority on which snapshots exist; a checksum in Snapshots without an
	// entry here is a bug.
	SnapshotOrder []string `json:"snapshot_order,omitempty"`
	// RestoredFrom records the snapshot this volume was last restored from,
	// which is what an operator needs to know after a recovery.
	RestoredFrom string `json:"restored_from,omitempty"`
	// VerifiedAt is when a backup of this volume was last proven recoverable.
	// A backup nobody has restored is a guess, so this is what an operator
	// consults to know whether recovery would actually work.
	VerifiedAt time.Time `json:"verified_at,omitempty"`
	// VerifiedSnapshot is the snapshot the last successful verification used.
	VerifiedSnapshot string `json:"verified_snapshot,omitempty"`
	// Handoff tracks a move in progress. Movement is a sequence of evidenced
	// steps rather than a single action, because each step must be proven
	// before the next is safe: quiesce, snapshot, transfer, verify, adopt.
	Handoff *VolumeHandoff `json:"handoff,omitempty"`
	// Backups records which snapshots have been shipped off-host, by id. A
	// snapshot that exists only on the volume's own node does not survive the
	// loss of that node, which is the failure backups exist for.
	Backups map[string]string `json:"backups,omitempty"`
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
	Queues      map[string]*Queue      `json:"queues,omitempty"`
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
	// IssuedAt and ExpiresAt bound how long a decision stands. An approval is a
	// human judgement about a situation, and situations change: a grant to move
	// a volume made last month should not still authorize the move today.
	IssuedAt  time.Time `json:"issued_at,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// Revision is the world revision the operator saw when deciding. It is
	// advisory rather than enforced: refusing an approval because the world
	// moved would make approvals unusable on a live cluster, but recording what
	// was on screen is what makes an after-the-fact review meaningful.
	Revision uint64 `json:"revision,omitempty"`
	// Reason is the operator's own words. It is the only field here a human
	// writes freely, and it is what a later reviewer actually wants to read.
	Reason string `json:"reason,omitempty"`
}

// Valid reports whether an approval authorizes anything at the given time.
//
// A revoked or expired grant is not an approval. Checking this at read time
// rather than pruning expired records keeps the decision auditable: the log
// still shows that someone approved, and that the authorization has lapsed.
func (a *Approval) Valid(now time.Time) bool {
	if a == nil || !a.Granted {
		return false
	}
	return a.ExpiresAt.IsZero() || now.Before(a.ExpiresAt)
}

type Node struct {
	ID       string            `json:"id"`
	Labels   map[string]string `json:"labels,omitempty"`
	Capacity Resources         `json:"capacity"`
	Used     Resources         `json:"used"`
	Images   map[string]bool   `json:"images,omitempty"`
	Healthy  bool              `json:"healthy"`
	// Providers records which model providers this node can currently reach, as
	// an observed fact rather than a configured intent. Provider egress is a
	// scheduling constraint for agent workloads in the same way a pinned image
	// is: an agent placed where its provider is unreachable cannot become ready.
	Providers map[string]ProviderReach `json:"providers,omitempty"`
	// BudgetCapacity is the total agent budget this node may have committed at
	// once, and BudgetUsed is what running agent allocations already hold.
	// Bounding this per node keeps one node's agents from consuming a whole
	// cluster's spend before any other node schedules one.
	BudgetCapacity Budget `json:"budget_capacity,omitempty"`
	BudgetUsed     Budget `json:"budget_used,omitempty"`
}

// ProviderReach is one measured answer to whether a node can reach a provider.
//
// Unlike an image, which stays present once pulled, egress is perishable: a
// route, a credential, or a provider outage can remove it between one placement
// and the next. So reachability carries an expiry and is never simply
// remembered.
type ProviderReach struct {
	// Reachable is what the last measurement found.
	Reachable bool `json:"reachable"`
	// ObservedAt and ExpiresAt bound how long the measurement may be trusted.
	ObservedAt time.Time `json:"observed_at,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	// Detail describes a failure, so an operator can tell a DNS problem from a
	// revoked credential without reading node logs.
	Detail string `json:"detail,omitempty"`
}

// CanReach reports whether the node can reach a provider at the given time.
//
// An expired observation is not reachability. Treating a remembered measurement
// as current would place agents onto a node that has since lost its egress,
// which is exactly the failure the expiry exists to prevent. An unmeasured
// provider is likewise not reachable: the scheduler must have positive evidence,
// not an absence of bad news.
func (n *Node) CanReach(provider string, now time.Time) bool {
	if n == nil {
		return false
	}
	reach, measured := n.Providers[provider]
	if !measured || !reach.Reachable {
		return false
	}
	return reach.ExpiresAt.IsZero() || now.Before(reach.ExpiresAt)
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
	// Budget is the ceiling this agent allocation holds against its node, and
	// Spent is what it has consumed so far. Both are empty for ordinary
	// workloads. Spent comes from runtime evidence, never from the agent.
	Budget Budget `json:"budget,omitempty"`
	Spent  Budget `json:"spent,omitempty"`
	// Draining marks an agent allocation that has been told to stop accepting
	// work and finish what it holds. An agent instance accumulates task context
	// that a stateless replica does not, so stopping it mid-task destroys work
	// rather than merely shifting load.
	Draining bool `json:"draining,omitempty"`
	// Task names the queue task this agent instance currently holds, which is
	// what makes a drain observable: the instance is drained when this is empty.
	Task string `json:"task,omitempty"`
	// Tools records the envelope granted to this agent allocation. The world
	// projection holds capability names and scopes, never the credentials the
	// node resolves them to.
	Tools []ToolGrant `json:"tools,omitempty"`
}

// Exhausted reports whether this allocation has consumed its budget.
//
// An exhausted agent is not failed: it stopped because it hit a ceiling that was
// declared for it. Distinguishing the two matters, because restarting an
// exhausted agent just burns the same budget again.
func (a *Allocation) Exhausted() bool {
	if a == nil || a.Budget.IsZero() {
		return false
	}
	return a.Spent.Exhausts(a.Budget)
}

// Drained reports whether a draining allocation has finished its work and is
// safe to stop without destroying task context.
func (a *Allocation) Drained() bool {
	return a != nil && a.Draining && a.Task == ""
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

// Queue is pending work agent instances pull from.
//
// A queue exists so that agent replicas can be scaled against observed demand
// rather than a fixed count. It is an explicit object for the same reason a
// Volume is: the thing that determines how many workers are needed has to be
// nameable independently of the workers themselves.
type Queue struct {
	// Name identifies the queue within the cluster.
	Name string `json:"name"`
	// Workload is the agent workload authorized to pull from it. A queue serves
	// one workload, so a scaling decision has an unambiguous subject.
	Workload string `json:"workload"`
	// Depth is the number of tasks waiting, from queue evidence rather than from
	// any agent's report of its own backlog.
	Depth int `json:"depth"`
	// InFlight is the number of tasks currently held by agent instances.
	InFlight int `json:"in_flight"`
	// MaxWorkers caps how far queue depth may scale this workload. Demand-driven
	// scaling without a ceiling is how a queue spike becomes a spend incident.
	MaxWorkers int `json:"max_workers"`
	// ObservedAt is when depth was last measured. Scaling on a stale depth would
	// keep adding workers for work that has already drained.
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

// DesiredWorkers is how many agent instances the observed depth justifies,
// bounded by MaxWorkers.
//
// In-flight tasks are already held by a worker, so only waiting depth calls for
// another one. Counting both would double-count every task at the moment it is
// picked up and oscillate the replica count.
func (q *Queue) DesiredWorkers(running int) int {
	if q == nil {
		return running
	}
	desired := running + q.Depth
	if desired > q.MaxWorkers {
		desired = q.MaxWorkers
	}
	if desired < 0 {
		return 0
	}
	return desired
}

// HandoffPhase is how far a volume move has progressed.
//
// The phases are ordered and each is entered only on evidence from the last.
// A move that stalls stays in its phase rather than advancing on assumption.
type HandoffPhase string

const (
	// HandoffQuiesced means the writer has stopped and the data is at rest.
	HandoffQuiesced HandoffPhase = "quiesced"
	// HandoffSnapshotted means a verified snapshot exists to move.
	HandoffSnapshotted HandoffPhase = "snapshotted"
	// HandoffTransferred means the target node holds a verified copy. The
	// origin is still authoritative at this point.
	HandoffTransferred HandoffPhase = "transferred"
	// HandoffAdopted means ownership moved. This is the only irreversible step.
	HandoffAdopted HandoffPhase = "adopted"
)

// VolumeHandoff records an in-progress move between nodes.
type VolumeHandoff struct {
	// From and To are the origin and target nodes.
	From string `json:"from"`
	To   string `json:"to"`
	// Phase is how far the move has progressed.
	Phase HandoffPhase `json:"phase"`
	// Snapshot is the verified snapshot being moved.
	Snapshot string `json:"snapshot,omitempty"`
	// Checksum is what the target must reproduce to prove it holds the data.
	Checksum string `json:"checksum,omitempty"`
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
	ActionBackupSnapshot   ActionKind = "backup_snapshot"
	ActionQuiesceVolume    ActionKind = "quiesce_volume"
	ActionTransferVolume   ActionKind = "transfer_volume"
	ActionAdoptVolume      ActionKind = "adopt_volume"
	ActionPruneSnapshots   ActionKind = "prune_snapshots"
	// ActionCollectImages reclaims image and snapshot storage no allocation
	// references. It is separate from prune_snapshots, which retains volume
	// recovery points; this reclaims the content-addressed layers underneath.
	ActionCollectImages    ActionKind = "collect_images"
	ActionVerifyBackup     ActionKind = "verify_backup"
	ActionDatabaseBackup   ActionKind = "database_backup"
	ActionStartAllocation  ActionKind = "start_allocation"
	ActionStopAllocation   ActionKind = "stop_allocation"
	ActionDeleteAllocation ActionKind = "delete_allocation"
	ActionPublishRoute     ActionKind = "publish_route"
	// ActionGrantTools installs an agent allocation's tool envelope before it
	// starts. Granting is a separate authorized step rather than a field read at
	// start time, so the blast radius appears in the event log as its own
	// decision.
	ActionGrantTools ActionKind = "grant_tools"
	// ActionDrainAllocation tells an agent instance to stop accepting work and
	// finish what it holds. It is the agent equivalent of quiescing a volume:
	// the step that makes the following stop non-destructive.
	ActionDrainAllocation ActionKind = "drain_allocation"
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
	// Engine names the database engine for a database_backup action.
	Engine string `json:"engine,omitempty"`
	// Retain is how many recent snapshots a prune keeps.
	Retain int `json:"retain,omitempty"`
	// DryRun asks a prune or collection to report what it would remove without
	// removing it.
	DryRun bool `json:"dry_run,omitempty"`
	// Protected lists the images a collect_images action must not reclaim. The
	// set is computed by the kernel from the world and travels inside the
	// signed action, so a node never decides for itself what is unreferenced.
	Protected []string `json:"protected,omitempty"`
	// Secret names the reference to mount. An action carries the reference, not
	// the material, so a proposal remains safe to log in full.
	Secret *SecretRef `json:"secret,omitempty"`
	// Tools is the grant envelope a grant_tools action installs. Like a secret
	// reference, these are capability names and scopes, never credentials.
	Tools []ToolGrant `json:"tools,omitempty"`
	// Budget is the ceiling a create_allocation action reserves for an agent
	// instance. It is empty for ordinary workloads.
	Budget    Budget   `json:"budget,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
}

// Check declares evidence a proposal must produce before it is considered
// complete. The kernel requires these declarations up front so that an agent
// states its success criteria before acting.
const (
	CheckAllocationReady = "allocation_ready"
	CheckRouteReachable  = "route_reachable"
	// CheckAgentReady is readiness for an agent workload. An agent is ready when
	// it has reached its provider with budget remaining, which a TCP accept does
	// not establish: an agent runtime can be listening and unable to work.
	CheckAgentReady = "agent_ready"
	// CheckAllocationDrained is proof an agent instance finished its task and
	// holds no work, which is what makes stopping it safe.
	CheckAllocationDrained = "allocation_drained"
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
