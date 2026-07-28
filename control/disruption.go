package control

import (
	"fmt"
	"sort"
	"time"
)

// A human operator was the rate limiter. Nothing in a4s replaced them.
//
// Per-proposal action limits bound one authorization, and a rollout's
// availability floor bounds one workload, but neither bounds the cluster. Four
// agents proposing every reconciliation round can retire allocations across
// twenty goals in the same instant, each proposal individually legal. A goal
// that fails and re-proposes does so at reconciliation frequency, forever. Both
// are failure modes that only appear once nobody is watching, which is exactly
// the condition a4s is built for.
//
// The governor below is the deterministic replacement for the person who would
// have said "not all at once" and "stop retrying that".
const (
	// DefaultDisruptionWindow is the period the disruption budget is measured
	// over.
	DefaultDisruptionWindow = 10 * time.Minute
	// DefaultMaxDisruptions is how many disruptive actions the cluster may take
	// within that window. It is deliberately loose: the budget exists to catch
	// a runaway loop, not to pace a deployment an operator is watching.
	DefaultMaxDisruptions = 12
	// DefaultDisruptionCooldown is how long a failure domain counts as under
	// disruption after one of its allocations was disrupted. While it is, no
	// other domain may be disrupted.
	//
	// It is set to the readiness TTL on purpose: one cooldown is roughly one
	// observation cycle, so the effect of the last disruption has been measured
	// before another domain is touched. The cost is rollout latency, and it is
	// real — a rolling update that crosses domains advances no faster than this,
	// and a large workload feels it. That is the trade being made, not an
	// oversight: an unattended control plane that replaces faster than it can
	// observe is the thing this exists to prevent.
	DefaultDisruptionCooldown = 30 * time.Second
	// DisruptionRetention bounds how much disruption history the world carries.
	// The projection is rebuilt from the whole log, so without a bound this
	// would grow with the age of the cluster rather than with its activity.
	DisruptionRetention = time.Hour
)

// Backoff bounds how fast a repeatedly failing target may be retried.
const (
	// BaseBackoff is the delay after a target's first failure. Each further
	// consecutive failure doubles it.
	BaseBackoff = 15 * time.Second
	// MaxBackoff caps the doubling. Beyond this the goal is not going to fix
	// itself and an operator needs to look at it; waiting longer only delays
	// the recovery once they do.
	MaxBackoff = 10 * time.Minute
)

// Disruption is one recorded disruptive change to a running workload.
//
// It records the domain rather than deriving it later, because the node may
// have left the world by the time the record is read, and a disruption whose
// domain became unknowable would silently stop counting against the one-domain
// rule it was recorded to enforce.
type Disruption struct {
	Target   string    `json:"target"`
	Workload string    `json:"workload,omitempty"`
	Node     string    `json:"node,omitempty"`
	Domain   string    `json:"domain,omitempty"`
	Kind     string    `json:"kind"`
	At       time.Time `json:"at"`
}

// Backoff records consecutive failures on one target and when it may next be
// created or started.
//
// It bounds re-creation rather than removal. A failed allocation must still be
// stoppable and deletable, or backoff would prevent the very remediation it
// exists to pace.
type Backoff struct {
	Failures int       `json:"failures"`
	Until    time.Time `json:"until"`
	// LastFailure is kept so an operator reading the world can tell a target
	// that is waiting out a backoff from one that failed long ago.
	LastFailure time.Time `json:"last_failure,omitempty"`
}

// Active reports whether the target is still waiting out its backoff.
func (b *Backoff) Active(now time.Time) bool {
	return b != nil && !b.Until.IsZero() && now.Before(b.Until)
}

// backoffFor returns the delay after a given number of consecutive failures,
// doubling from BaseBackoff and capped at MaxBackoff.
func backoffFor(failures int) time.Duration {
	if failures <= 1 {
		return BaseBackoff
	}
	delay := BaseBackoff
	for i := 1; i < failures; i++ {
		delay *= 2
		if delay >= MaxBackoff {
			return MaxBackoff
		}
	}
	return delay
}

// disruptiveKinds are the actions that take capacity away from a running
// workload.
//
// Creation and starting are absent on purpose: they add capacity, and pacing
// them would slow recovery rather than protect it. Pulling an image mutates a
// node's content store and disrupts nothing.
var disruptiveKinds = map[ActionKind]bool{
	ActionStopAllocation:   true,
	ActionDeleteAllocation: true,
	ActionDrainAllocation:  true,
}

// recordDisruption appends a disruption to the world and prunes the ledger.
//
// Pruning is relative to the observation being applied rather than to the wall
// clock, so a projection rebuilt from the log produces exactly the ledger the
// live server held. A wall-clock prune would make the rebuild depend on when it
// ran, which is the property the durable projection exists to avoid.
func recordDisruption(world *World, allocation *Allocation, kind string, at time.Time) {
	if allocation == nil || at.IsZero() {
		return
	}
	if allocation.Phase == AllocationStopped {
		// Already dead. Cleaning up an allocation that stopped on its own took
		// no capacity away, and charging it would spend the budget that paces
		// live disruption on repairing damage the cluster did not cause.
		return
	}
	domain := ""
	if node := world.Nodes[allocation.Node]; node != nil {
		domain = node.FailureDomain()
	}
	world.Disruptions = append(world.Disruptions, Disruption{
		Target: allocation.ID, Workload: allocation.Workload,
		Node: allocation.Node, Domain: domain, Kind: kind, At: at,
	})

	cutoff := at.Add(-DisruptionRetention)
	kept := world.Disruptions[:0]
	for _, disruption := range world.Disruptions {
		if disruption.At.After(cutoff) {
			kept = append(kept, disruption)
		}
	}
	world.Disruptions = kept
}

// recordFailure advances a target's backoff after an observed failure.
func recordFailure(world *World, target string, at time.Time) {
	if world.Backoff == nil {
		world.Backoff = make(map[string]*Backoff)
	}
	state := world.Backoff[target]
	if state == nil {
		state = &Backoff{}
		world.Backoff[target] = state
	}
	state.Failures++
	state.LastFailure = at
	state.Until = at.Add(backoffFor(state.Failures))
}

// clearBackoff forgets a target's failure history once it is observed healthy.
//
// Clearing on readiness rather than decaying on a timer is what makes the
// backoff track consecutive failures: a workload that recovers starts from zero,
// and one that keeps flapping keeps escalating.
func clearBackoff(world *World, target string) {
	delete(world.Backoff, target)
}

// Pacing describes a goal held back by a safeguard rather than by a fault.
//
// The distinction exists because the safeguards made these two look identical
// from outside. Before them, a reconciliation that produced no authorized action
// almost always meant something was wrong; now it routinely means a brake is on
// and the right response is to wait. A caller that cannot tell them apart counts
// deliberate pacing as failure, which is how a working governor turns into an
// alert nobody can act on.
type Pacing struct {
	// Reason is operator-facing text naming the safeguard.
	Reason string
	// Until is when the constraint lifts on its own.
	Until time.Time
}

func (p Pacing) Error() string {
	return fmt.Sprintf("held back until %s: %s", p.Until.UTC().Format(time.RFC3339), p.Reason)
}

// Paced reports that a goal is waiting on a safeguard that expires.
//
// Only constraints with a time at which they lift by themselves count. A cordon
// does not: it stands until something decides otherwise, so a goal waiting on
// one is genuinely stuck and should be reported that way. Treating an operator
// decision as transient would tell a caller to keep waiting for a change that
// will never come on its own.
func (k Kernel) Paced(goal Goal, world World) (Pacing, bool) {
	now := world.Now()
	var pacing Pacing

	// Backoff on this workload's own targets. The earliest expiry is the one
	// worth reporting: it is when something could next move.
	//
	// The replica slots are checked by name rather than by walking the world's
	// allocations, because the case that matters most is the one where the
	// allocation is gone: remediation deletes a failed replica so it can be
	// recreated, and the backoff on that id is then the only thing standing
	// between the cluster and its own repair. Looking only at allocations that
	// still exist would report nothing at exactly that moment.
	for _, id := range replicaSlots(goal, world) {
		state := world.Backoff[id]
		if !state.Active(now) {
			continue
		}
		if pacing.Until.IsZero() || state.Until.Before(pacing.Until) {
			pacing = Pacing{
				Reason: fmt.Sprintf("%s is in a failure backoff after %d consecutive failures",
					id, state.Failures),
				Until: state.Until,
			}
		}
	}

	// The disruption cooldown, when another domain currently holds it.
	if cooldown := k.Policy.DisruptionCooldown; cooldown > 0 {
		for _, disruption := range RecentDisruptions(world, now, cooldown) {
			until := disruption.At.Add(cooldown)
			if pacing.Until.IsZero() || until.Before(pacing.Until) {
				pacing = Pacing{
					Reason: fmt.Sprintf("failure domain %q is inside its disruption cooldown",
						disruption.Domain),
					Until: until,
				}
			}
		}
	}

	// The budget, once it is actually full. A budget with room left is not
	// holding anything back and reporting it would be noise.
	if ceiling := k.Policy.MaxDisruptionsPerWindow; ceiling > 0 {
		window := k.Policy.DisruptionWindow
		if window <= 0 {
			window = DefaultDisruptionWindow
		}
		if recent := RecentDisruptions(world, now, window); len(recent) >= ceiling {
			until := recent[0].At.Add(window)
			if pacing.Until.IsZero() || until.Before(pacing.Until) {
				pacing = Pacing{
					Reason: fmt.Sprintf("the disruption budget is full at %d in %s",
						len(recent), window),
					Until: until,
				}
			}
		}
	}
	return pacing, !pacing.Until.IsZero()
}

// replicaSlots lists every allocation id this goal may occupy, whether or not
// the allocation currently exists.
//
// Both sources are needed. The names derived from the replica count cover slots
// waiting to be filled; the world's own allocations cover a queue-scaled agent
// workload that has grown past the goal's declared count.
func replicaSlots(goal Goal, world World) []string {
	seen := make(map[string]bool)
	var slots []string
	for replica := 0; replica < authorizedReplicas(goal, world); replica++ {
		id := AllocationID(goal.Workload.Name, replica)
		seen[id] = true
		slots = append(slots, id)
	}
	for id, allocation := range world.Allocations {
		if allocation.Workload == goal.Workload.Name && !seen[id] {
			seen[id] = true
			slots = append(slots, id)
		}
	}
	sort.Strings(slots)
	return slots
}

// RecentDisruptions returns the disruptions inside a window ending now.
func RecentDisruptions(world World, now time.Time, window time.Duration) []Disruption {
	if window <= 0 {
		return nil
	}
	cutoff := now.Add(-window)
	var recent []Disruption
	for _, disruption := range world.Disruptions {
		if disruption.At.After(cutoff) {
			recent = append(recent, disruption)
		}
	}
	sort.Slice(recent, func(i, j int) bool { return recent[i].At.Before(recent[j].At) })
	return recent
}

// disruptedDomains reports which failure domains are still inside their cooldown.
func disruptedDomains(world World, now time.Time, cooldown time.Duration) map[string]bool {
	domains := make(map[string]bool)
	for _, disruption := range RecentDisruptions(world, now, cooldown) {
		if disruption.Domain != "" {
			domains[disruption.Domain] = true
		}
	}
	return domains
}

// actionDomain reports the failure domain an action would disturb.
func actionDomain(world World, action Action) string {
	node := action.Node
	if allocation, ok := world.Allocations[action.Target]; ok && allocation.Node != "" {
		node = allocation.Node
	}
	return world.Nodes[node].FailureDomain()
}

// checkDisruptionBudget refuses a proposal that would exceed the cluster's
// tolerance for simultaneous change.
//
// Two separate rules, because they catch different failures. The budget catches
// a control plane that has started thrashing: many small legal proposals adding
// up to an outage nobody authorized. The one-domain rule catches the correlated
// case, where every disruption is individually within budget but they land in
// different racks at once and take out more capacity than any single workload's
// availability floor was defending.
func (k Kernel) checkDisruptionBudget(world World, proposal Proposal) error {
	var disruptive []Action
	for _, action := range proposal.Actions {
		if disruptiveKinds[action.Kind] {
			disruptive = append(disruptive, action)
		}
	}
	if len(disruptive) == 0 {
		return nil
	}
	now := world.Now()

	if k.Policy.MaxDisruptionsPerWindow > 0 {
		window := k.Policy.DisruptionWindow
		if window <= 0 {
			window = DefaultDisruptionWindow
		}
		recent := len(RecentDisruptions(world, now, window))
		if recent+len(disruptive) > k.Policy.MaxDisruptionsPerWindow {
			return fmt.Errorf(
				"disruption budget exhausted: %d disruptions in the last %s plus %d proposed exceeds %d",
				recent, window, len(disruptive), k.Policy.MaxDisruptionsPerWindow)
		}
	}

	if k.Policy.DisruptionCooldown > 0 {
		proposed := make(map[string]bool)
		for _, action := range disruptive {
			if domain := actionDomain(world, action); domain != "" {
				proposed[domain] = true
			}
		}
		if len(proposed) > 1 {
			return fmt.Errorf(
				"proposal disrupts %d failure domains at once; disrupt one at a time",
				len(proposed))
		}
		active := disruptedDomains(world, now, k.Policy.DisruptionCooldown)
		for domain := range proposed {
			for other := range active {
				if other != domain {
					return fmt.Errorf(
						"failure domain %q was disrupted within the last %s; wait before disrupting %q",
						other, k.Policy.DisruptionCooldown, domain)
				}
			}
		}
	}
	return nil
}

// checkBackoff refuses re-creating or restarting a target that is waiting out a
// failure backoff.
//
// This is the hysteresis. Without it a goal whose allocation fails on start
// re-proposes the identical placement on the next reconciliation round and every
// round after, which burns image pulls, node capacity, and event log forever
// while never converging. The node supervisor already bounds restart-in-place;
// this bounds the reschedule loop above it, which the node cannot see.
func checkBackoff(world World, proposal Proposal) error {
	now := world.Now()
	for _, action := range proposal.Actions {
		switch action.Kind {
		case ActionCreateAllocation, ActionStartAllocation:
		default:
			continue
		}
		state := world.Backoff[action.Target]
		if state.Active(now) {
			return fmt.Errorf(
				"target %q failed %d times and is in backoff until %s",
				action.Target, state.Failures, state.Until.UTC().Format(time.RFC3339))
		}
	}
	return nil
}
