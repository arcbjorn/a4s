package control

import (
	"fmt"
	"sort"
)

// Placement declares how a workload's replicas must be distributed across
// failure domains.
//
// Without it nothing stops every replica of a workload landing in one place:
// selection prefers the node with the most free memory, which is the same node
// for every replica until it fills. The availability floor, the canary step
// ladder, and rolling replacement all reason about how many replicas are ready,
// and all three are satisfied by four replicas that a single reboot ends. The
// constraint is what makes those guarantees describe reality rather than a
// count.
//
// It is expressed as a ceiling per domain rather than as a skew tolerance
// because the ceiling is the property an operator actually wants to state, and
// because a ceiling is decidable against a single candidate node. A skew rule
// can only be evaluated against a whole placement, which would mean the kernel
// could not authorize one allocation at a time.
type Placement struct {
	// MaxPerDomain bounds how many live replicas may run in one failure domain.
	// Zero means unconstrained, which is the behaviour of every goal written
	// before this existed.
	MaxPerDomain int `json:"max_per_domain,omitempty"`
}

// Validate checks a placement constraint is expressible.
func (p *Placement) Validate() error {
	if p == nil {
		return nil
	}
	if p.MaxPerDomain < 0 {
		return fmt.Errorf("placement max_per_domain cannot be negative")
	}
	return nil
}

// MaxPerDomain reports the effective per-domain ceiling for a goal, or zero when
// the workload declares none.
func (g Goal) MaxPerDomain() int {
	if g.Workload.Placement == nil {
		return 0
	}
	return g.Workload.Placement.MaxPerDomain
}

// FailureDomain reports which domain a node belongs to.
//
// A node that declares none is its own domain. That default is what makes the
// constraint useful before an operator has described their topology: spreading
// across domains degenerates to spreading across nodes, which is what most
// deployments mean by spreading anyway. Treating an undeclared domain as a
// single shared domain would instead make the constraint silently unsatisfiable
// on every cluster that had not been labelled yet.
func (n *Node) FailureDomain() string {
	if n == nil {
		return ""
	}
	if n.Domain != "" {
		return n.Domain
	}
	return n.ID
}

// DomainOccupancy counts a workload's live allocations per failure domain.
//
// Stopped allocations are excluded. A replica that is no longer running does not
// consume the availability its domain represents, and counting it would block
// the replacement that a rollout depends on being able to create.
func DomainOccupancy(world World, workload string) map[string]int {
	counts := make(map[string]int)
	for _, allocation := range world.Allocations {
		if allocation.Workload != workload || allocation.Phase == AllocationStopped {
			continue
		}
		node := world.Nodes[allocation.Node]
		if node == nil {
			// An allocation on a node the world does not know cannot be
			// attributed to a domain. It is not counted rather than being
			// charged to an arbitrary one.
			continue
		}
		counts[node.FailureDomain()]++
	}
	return counts
}

// FailureDomains lists the distinct domains the world's schedulable nodes
// occupy, in a stable order. A cordoned node's domain is not capacity a goal can
// be admitted against, because nothing new may be placed there.
func FailureDomains(world World) []string {
	seen := make(map[string]bool, len(world.Nodes))
	for _, node := range world.Nodes {
		if !node.Schedulable() {
			continue
		}
		seen[node.FailureDomain()] = true
	}
	domains := make([]string, 0, len(seen))
	for domain := range seen {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	return domains
}

// checkSpread refuses a placement that would overfill a failure domain.
//
// It is checked against the simulated world rather than the observed one, so a
// proposal placing two replicas into the same domain in one authorization is
// refused on its second action rather than passing both and discovering the
// violation after the fact.
func checkSpread(goal Goal, world World, action Action) error {
	ceiling := goal.MaxPerDomain()
	if ceiling <= 0 {
		return nil
	}
	node, ok := world.Nodes[action.Node]
	if !ok {
		// The caller has already refused an unknown node with a better message.
		return nil
	}
	domain := node.FailureDomain()
	if held := DomainOccupancy(world, goal.Workload.Name)[domain]; held >= ceiling {
		return fmt.Errorf(
			"failure domain %q already holds %d of at most %d replicas of workload %q",
			domain, held, ceiling, goal.Workload.Name)
	}
	return nil
}

// validateSpread checks a goal's spread constraint can be satisfied at all.
//
// A goal asking for more replicas than the cluster's domains can hold would
// otherwise be accepted and then block partway through reconciliation, where the
// cause is a placement failure several rounds removed from the declaration that
// caused it. Refusing at admission puts the error where the mistake is.
func validateSpread(goal Goal, world World) error {
	if err := goal.Workload.Placement.Validate(); err != nil {
		return err
	}
	ceiling := goal.MaxPerDomain()
	if ceiling <= 0 {
		return nil
	}
	domains := FailureDomains(world)
	if capacity := len(domains) * ceiling; capacity < goal.Workload.Replicas {
		return fmt.Errorf(
			"workload %q wants %d replicas but %d failure domains at %d per domain hold only %d",
			goal.Workload.Name, goal.Workload.Replicas, len(domains), ceiling, capacity)
	}
	return nil
}
