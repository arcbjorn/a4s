package control

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	digestPattern = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)
)

func (s *Scenario) NormalizeAndValidate() error {
	s.World.normalize()
	if s.Goal.APIVersion != APIVersion {
		return fmt.Errorf("api_version must be %q", APIVersion)
	}
	if !namePattern.MatchString(s.Goal.ID) {
		return fmt.Errorf("goal id must be lowercase DNS-style text")
	}
	if strings.TrimSpace(s.Goal.Objective) == "" {
		return fmt.Errorf("goal objective is required")
	}
	w := s.Goal.Workload
	if !namePattern.MatchString(w.Name) {
		return fmt.Errorf("workload name must be lowercase DNS-style text")
	}
	if !digestPattern.MatchString(w.Image) {
		return fmt.Errorf("workload image must be pinned by sha256 digest")
	}
	if w.Replicas < 1 {
		return fmt.Errorf("workload replicas must be positive")
	}
	// An agent workload may have no inbound network at all: it reaches the world
	// outbound through granted tools. A scheduled job usually serves nothing
	// either. Requiring a listening port would force both to expose a surface
	// they do not need.
	if (w.Runtime == nil && w.Schedule == nil) || w.Port != 0 {
		if w.Port < 1 || w.Port > 65535 {
			return fmt.Errorf("workload port must be between 1 and 65535")
		}
	}
	if w.Resources.CPUMillis < 1 || w.Resources.MemoryMB < 1 {
		return fmt.Errorf("workload resources must be positive")
	}
	if w.Privileged {
		return fmt.Errorf("privileged workloads are outside the v1alpha1 safety policy")
	}
	if err := validateVolumes(w); err != nil {
		return err
	}
	if err := validateEngine(w); err != nil {
		return err
	}
	if err := validateRuntime(w); err != nil {
		return err
	}
	if err := validateSchedule(w); err != nil {
		return err
	}
	if err := validateCanary(s.Goal); err != nil {
		return err
	}
	if err := validateSpread(s.Goal, s.World); err != nil {
		return err
	}
	if err := validateQueues(s.Goal, &s.World); err != nil {
		return err
	}
	if err := validateSecrets(w.Secrets); err != nil {
		return err
	}
	if s.Goal.Route != nil {
		if strings.TrimSpace(s.Goal.Route.Host) == "" {
			return fmt.Errorf("route host is required")
		}
		if s.Goal.Route.Port < 1 || s.Goal.Route.Port > 65535 {
			return fmt.Errorf("route port must be between 1 and 65535")
		}
		switch s.Goal.Route.Exposure {
		case "tailnet", "public":
		default:
			return fmt.Errorf("route exposure must be tailnet or public")
		}
	}
	if len(s.World.Nodes) == 0 {
		return fmt.Errorf("world must contain at least one node")
	}
	for id, node := range s.World.Nodes {
		if node == nil || node.ID != id || !namePattern.MatchString(id) {
			return fmt.Errorf("node map key %q must match a valid node id", id)
		}
		if node.Capacity.CPUMillis < 1 || node.Capacity.MemoryMB < 1 {
			return fmt.Errorf("node %q capacity must be positive", id)
		}
	}
	for id, approval := range s.World.Approvals {
		if approval == nil || approval.ID != id || approval.GoalID != s.Goal.ID || approval.Scope == "" || approval.IssuedBy == "" {
			return fmt.Errorf("approval %q is malformed or belongs to another goal", id)
		}
	}
	return nil
}

func (w *World) normalize() {
	if w.Nodes == nil {
		w.Nodes = make(map[string]*Node)
	}
	if w.Volumes == nil {
		w.Volumes = make(map[string]*Volume)
	}
	if w.Allocations == nil {
		w.Allocations = make(map[string]*Allocation)
	}
	if w.Routes == nil {
		w.Routes = make(map[string]*Route)
	}
	if w.Queues == nil {
		w.Queues = make(map[string]*Queue)
	}
	if w.Approvals == nil {
		w.Approvals = make(map[string]*Approval)
	}
	if w.KnownGood == nil {
		w.KnownGood = make(map[string]string)
	}
	for _, node := range w.Nodes {
		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}
		if node.Images == nil {
			node.Images = make(map[string]bool)
		}
	}
}

// hasApproval reports whether a live operator grant covers this goal and scope.
//
// Expiry is checked against the world's own observation time rather than the
// wall clock, so authorization is evaluated deterministically against the same
// snapshot every other policy check reads.
func hasApproval(world World, goalID, scope string) bool {
	now := world.Now()
	for _, approval := range world.Approvals {
		if approval.GoalID == goalID && approval.Scope == scope && approval.Valid(now) {
			return true
		}
	}
	return false
}

// secretNamePattern keeps secret names to opaque handles. A name that looked
// like a path or carried arbitrary text would invite operators to smuggle
// material into a field that gets logged.
var secretNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,61}[a-z0-9])?$`)

// maxSecretVersionLength bounds a version string. A version is an identifier,
// and anything long enough to hold a key is not one.
const maxSecretVersionLength = 64

// validateSecrets enforces that only references, never material, reach a goal.
//
// The struct has no field for a value, so this guards the remaining risk: an
// operator encoding material into a name, a version, or a mount path, any of
// which are recorded in the durable log.
func validateSecrets(refs []SecretRef) error {
	seenNames := make(map[string]bool, len(refs))
	seenPaths := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if !secretNamePattern.MatchString(ref.Name) {
			return fmt.Errorf("secret name %q must be a short lowercase handle", ref.Name)
		}
		if ref.Version == "" || len(ref.Version) > maxSecretVersionLength {
			return fmt.Errorf("secret %q needs a version of at most %d characters",
				ref.Name, maxSecretVersionLength)
		}
		if strings.ContainsAny(ref.Version, "\n\r\t ") {
			return fmt.Errorf("secret %q version must not contain whitespace", ref.Name)
		}
		if !strings.HasPrefix(ref.MountPath, "/") {
			return fmt.Errorf("secret %q mount path must be absolute", ref.Name)
		}
		// A relative element could escape the mount directory and place secret
		// material somewhere the workload does not expect.
		if strings.Contains(ref.MountPath, "..") {
			return fmt.Errorf("secret %q mount path must not contain %q", ref.Name, "..")
		}
		if seenNames[ref.Name] {
			return fmt.Errorf("secret %q is referenced twice", ref.Name)
		}
		if seenPaths[ref.MountPath] {
			return fmt.Errorf("two secrets share mount path %q", ref.MountPath)
		}
		seenNames[ref.Name] = true
		seenPaths[ref.MountPath] = true
	}
	return nil
}

// validateVolumes enforces what a stateful workload may declare.
//
// A workload with volumes is single-writer by construction: more than one
// replica writing the same local volume is data corruption, not a scaling
// strategy. Refusing it here is far cheaper than detecting it later.
func validateVolumes(w WorkloadSpec) error {
	if len(w.Volumes) == 0 {
		if w.Stateful {
			return fmt.Errorf("a stateful workload must declare its volumes")
		}
		return nil
	}
	if w.Replicas != 1 {
		return fmt.Errorf("a workload with volumes must have exactly one replica, not %d", w.Replicas)
	}
	seenNames := make(map[string]bool, len(w.Volumes))
	seenPaths := make(map[string]bool, len(w.Volumes))
	for _, ref := range w.Volumes {
		if !namePattern.MatchString(ref.Name) {
			return fmt.Errorf("volume name %q must be lowercase DNS-style text", ref.Name)
		}
		if !strings.HasPrefix(ref.MountPath, "/") {
			return fmt.Errorf("volume %q mount path must be absolute", ref.Name)
		}
		if strings.Contains(ref.MountPath, "..") {
			return fmt.Errorf("volume %q mount path must not contain %q", ref.Name, "..")
		}
		if seenNames[ref.Name] {
			return fmt.Errorf("volume %q is referenced twice", ref.Name)
		}
		if seenPaths[ref.MountPath] {
			return fmt.Errorf("two volumes share mount path %q", ref.MountPath)
		}
		seenNames[ref.Name] = true
		seenPaths[ref.MountPath] = true
	}
	return nil
}

// supportedEngines are the database engines the agents know how to back up
// consistently. An unknown engine would be backed up as a generic volume, which
// for a running database means an inconsistent copy.
var supportedEngines = map[string]bool{
	"postgres": true,
}

// validateEngine enforces what a database workload may declare.
//
// A database is single-writer by nature and keeps its data on a volume, so it
// inherits the stateful constraints and adds one: the engine must be one the
// agents can back up with the database's own tooling.
func validateEngine(w WorkloadSpec) error {
	if w.Engine == "" {
		return nil
	}
	if !supportedEngines[w.Engine] {
		return fmt.Errorf("database engine %q is not supported", w.Engine)
	}
	if len(w.Volumes) == 0 {
		return fmt.Errorf("a database workload must declare a volume for its data")
	}
	if w.Replicas != 1 {
		return fmt.Errorf("a database workload must have exactly one replica, not %d", w.Replicas)
	}
	return nil
}

// supportedRuntimes are the agent runtime contracts the node knows how to bound.
// An unknown runtime would be started as a generic container, which means its
// budget ceilings and tool envelope would go unenforced.
var supportedRuntimes = map[string]bool{
	"a4s.agent/v1": true,
}

// maxToolGrants bounds an agent's tool envelope. An agent granted an unbounded
// number of tools has an unbounded blast radius, and the point of the envelope
// is that a human can read it before approving.
const maxToolGrants = 32

// validateRuntime enforces what an agent workload may declare.
//
// The rules here exist because an agent's failure modes are not a container's.
// A container with a bad config crashes; an agent with no ceiling runs a loop
// that costs money until someone notices. Every field checked below is one an
// operator would otherwise discover was missing from an invoice.
func validateRuntime(w WorkloadSpec) error {
	if w.Runtime == nil {
		return nil
	}
	r := w.Runtime
	if !supportedRuntimes[r.Name] {
		return fmt.Errorf("agent runtime %q is not supported", r.Name)
	}
	// An agent is a workload kind, and a database is another. One container
	// cannot be both, and declaring both would leave the kernel with two
	// contradictory sets of rules for backing it up and probing it.
	if w.Engine != "" {
		return fmt.Errorf("a workload cannot be both a %s database and an agent", w.Engine)
	}
	if !namePattern.MatchString(r.Provider) {
		return fmt.Errorf("agent provider must be lowercase DNS-style text")
	}
	// A model alias that a provider can repoint is the same hazard as a floating
	// image tag: the workload changes under the operator without a goal change.
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("agent model must be pinned")
	}
	if err := validateBudget(r.Budget); err != nil {
		return err
	}
	if err := validateToolGrants(r.Tools); err != nil {
		return err
	}
	if r.Queue != "" && !namePattern.MatchString(r.Queue) {
		return fmt.Errorf("agent queue name %q must be lowercase DNS-style text", r.Queue)
	}
	return nil
}

// validateBudget requires every ceiling to be present and positive.
//
// A zero ceiling is rejected rather than treated as unlimited. Unlimited is a
// decision an operator should have to write down somewhere other than by
// omission, and the common case of a forgotten field must not be the case that
// grants infinite spend.
func validateBudget(b Budget) error {
	if b.Tokens < 1 {
		return fmt.Errorf("agent budget must set a positive token ceiling")
	}
	if b.CostMillis < 1 {
		return fmt.Errorf("agent budget must set a positive cost ceiling")
	}
	if b.WallSeconds < 1 {
		return fmt.Errorf("agent budget must set a positive wall-clock ceiling")
	}
	if b.ToolCalls < 1 {
		return fmt.Errorf("agent budget must set a positive tool-call ceiling")
	}
	return nil
}

// toolNamePattern keeps tool names to opaque handles for the same reason secret
// names are constrained: the name reaches the durable log.
var toolNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,61}[a-z0-9])?$`)

// validateToolGrants enforces that an agent's blast radius is explicit.
func validateToolGrants(grants []ToolGrant) error {
	if len(grants) > maxToolGrants {
		return fmt.Errorf("agent declares %d tools, above the limit of %d", len(grants), maxToolGrants)
	}
	seen := make(map[string]bool, len(grants))
	for _, grant := range grants {
		if !toolNamePattern.MatchString(grant.Name) {
			return fmt.Errorf("tool name %q must be a short lowercase handle", grant.Name)
		}
		// An unscoped tool is granted whatever its credential allows, which makes
		// the declared envelope a description of nothing.
		if strings.TrimSpace(grant.Scope) == "" {
			return fmt.Errorf("tool %q must declare a scope", grant.Name)
		}
		if seen[grant.Name] {
			return fmt.Errorf("tool %q is granted twice", grant.Name)
		}
		seen[grant.Name] = true
	}
	return nil
}

// validateQueues enforces that a declared queue is coherent with its workload.
func validateQueues(goal Goal, world *World) error {
	for name, queue := range world.Queues {
		if queue == nil || queue.Name != name || !namePattern.MatchString(name) {
			return fmt.Errorf("queue map key %q must match a valid queue name", name)
		}
		if queue.Depth < 0 || queue.InFlight < 0 {
			return fmt.Errorf("queue %q cannot have negative depth or in-flight count", name)
		}
		// Scaling without a ceiling turns a queue spike into unbounded spend, so
		// a queue that no worker count can bound is refused outright.
		if queue.MaxWorkers < 1 {
			return fmt.Errorf("queue %q must cap workers at one or more", name)
		}
	}
	runtime := goal.Workload.Runtime
	if runtime == nil || runtime.Queue == "" {
		return nil
	}
	queue, ok := world.Queues[runtime.Queue]
	if !ok {
		return fmt.Errorf("agent workload references queue %q, which does not exist", runtime.Queue)
	}
	// A queue serving a different workload would let one workload's demand scale
	// another's replicas.
	if queue.Workload != goal.Workload.Name {
		return fmt.Errorf("queue %q serves workload %q, not %q",
			runtime.Queue, queue.Workload, goal.Workload.Name)
	}
	return nil
}

// validateSchedule checks a scheduled workload declares a usable schedule and
// does not also claim to be a kind that contradicts it.
func validateSchedule(w WorkloadSpec) error {
	if w.Schedule == nil {
		return nil
	}
	if err := w.Schedule.Validate(); err != nil {
		return err
	}
	// A scheduled run exits when its work is done. A database is expected to keep
	// running and is backed up while it does, so the two sets of rules for
	// readiness and completion directly contradict each other.
	if w.Engine != "" {
		return fmt.Errorf("a %s database cannot be a scheduled workload", w.Engine)
	}
	// An agent workload drains before stopping and is retired on budget
	// exhaustion, which is a different lifecycle from running to completion.
	if w.Runtime != nil {
		return fmt.Errorf("an agent workload cannot be a scheduled workload")
	}
	// A stateful scheduled job would need its volume handed between runs, and
	// nothing coordinates that yet. Refusing is better than a job that silently
	// races itself for a volume.
	if w.Stateful || len(w.Volumes) > 0 {
		return fmt.Errorf("a scheduled workload cannot be stateful")
	}
	// More completions than replicas can never be satisfied: each replica exits
	// once per run, so the goal would be permanently unreachable.
	if w.Schedule.RequiredCompletions() > w.Replicas {
		return fmt.Errorf(
			"schedule requires %d completions but the workload runs %d replicas",
			w.Schedule.RequiredCompletions(), w.Replicas)
	}
	return nil
}

// validateCanary checks a canary is usable and applies to a workload that can
// actually receive split traffic.
func validateCanary(goal Goal) error {
	if goal.Canary == nil {
		return nil
	}
	if err := goal.Canary.Validate(); err != nil {
		return err
	}
	// Splitting traffic requires a route to split. Without one there is nothing
	// for a weight to apply to, and the canary would silently do nothing.
	if goal.Route == nil {
		return fmt.Errorf("a canary requires a route to shift traffic across")
	}
	// One replica cannot serve two versions at once, so a canary on it would jump
	// straight from 0% to 100% and provide none of the protection it implies.
	if goal.Workload.Replicas < 2 {
		return fmt.Errorf("a canary needs at least two replicas, got %d", goal.Workload.Replicas)
	}
	// A run-to-completion job has no steady traffic to shift.
	if goal.Workload.Schedule != nil {
		return fmt.Errorf("a scheduled workload cannot use a canary")
	}
	return nil
}
