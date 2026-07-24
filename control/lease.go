package control

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// DefaultLeaseTTL bounds how long a proposal may hold its targets. A holder
// that dies mid-execution must not block those targets forever, so leases
// expire rather than requiring explicit release.
const DefaultLeaseTTL = 5 * time.Minute

// Lease records exclusive intent to mutate one target.
type Lease struct {
	Target     string    `json:"target"`
	ProposalID string    `json:"proposal_id"`
	GoalID     string    `json:"goal_id"`
	ID         string    `json:"id"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// LeaseManager grants exclusive, expiring claims on mutation targets.
//
// Revision binding alone does not prevent conflict: two proposals built against
// the same revision are both non-stale, and the second would still be executing
// against state the first has already begun changing. Leases close that window
// by making concurrent mutation of one target impossible rather than merely
// unlikely.
type LeaseManager struct {
	mu     sync.Mutex
	leases map[string]Lease
	ttl    time.Duration
	now    func() time.Time
}

func NewLeaseManager() *LeaseManager {
	return &LeaseManager{leases: make(map[string]Lease), ttl: DefaultLeaseTTL, now: time.Now}
}

// WithClock replaces the lease clock, which keeps expiry behavior testable.
func (m *LeaseManager) WithClock(now func() time.Time) *LeaseManager {
	m.now = now
	return m
}

func (m *LeaseManager) WithTTL(ttl time.Duration) *LeaseManager {
	m.ttl = ttl
	return m
}

// Acquire claims every target a proposal intends to mutate, or nothing at all.
// Partial acquisition would let two proposals each hold part of a plan and
// deadlock or interleave, so acquisition is all-or-nothing.
func (m *LeaseManager) Acquire(goalID, proposalID string, targets []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()

	// Sorting makes acquisition order deterministic, so concurrent proposals
	// contend predictably instead of racing in map order.
	ordered := append([]string(nil), targets...)
	sort.Strings(ordered)

	for _, target := range ordered {
		existing, held := m.leases[target]
		if !held || !now.Before(existing.ExpiresAt) {
			continue
		}
		// Re-acquiring one's own lease is a retry, not a conflict.
		if existing.ProposalID == proposalID {
			continue
		}
		return "", fmt.Errorf("target %q is leased by proposal %q until %s",
			target, existing.ProposalID, existing.ExpiresAt.UTC().Format(time.RFC3339))
	}

	leaseID := fmt.Sprintf("lease-%s-%d", proposalID, now.UnixNano())
	expiry := now.Add(m.ttl)
	for _, target := range ordered {
		m.leases[target] = Lease{
			Target: target, ProposalID: proposalID, GoalID: goalID,
			ID: leaseID, ExpiresAt: expiry,
		}
	}
	return leaseID, nil
}

// Release drops every lease held under a lease ID. Releasing is best effort:
// expiry is the authoritative bound, so a missed release only delays reuse.
func (m *LeaseManager) Release(leaseID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for target, lease := range m.leases {
		if lease.ID == leaseID {
			delete(m.leases, target)
		}
	}
}

// Holder reports the live lease on a target, if any.
func (m *LeaseManager) Holder(target string) (Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, held := m.leases[target]
	if !held || !m.clock().Before(lease.ExpiresAt) {
		return Lease{}, false
	}
	return lease, true
}

func (m *LeaseManager) clock() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}

// LeaseTargets returns the distinct targets a proposal will mutate. Read-only
// actions do not need exclusivity, so only mutating kinds are claimed.
func LeaseTargets(proposal Proposal) []string {
	seen := make(map[string]bool)
	var targets []string
	for _, action := range proposal.Actions {
		switch action.Kind {
		case ActionCreateAllocation, ActionStartAllocation,
			ActionStopAllocation, ActionDeleteAllocation, ActionPublishRoute:
		default:
			// Pulling an image mutates a node's content store, not a target
			// another proposal could conflict with.
			continue
		}
		if action.Target == "" || seen[action.Target] {
			continue
		}
		seen[action.Target] = true
		targets = append(targets, action.Target)
	}
	sort.Strings(targets)
	return targets
}
