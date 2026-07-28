package node

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sync"
	"time"

	"github.com/arcbjorn/a4s/control"
)

type Runtime interface {
	Execute(context.Context, control.Action) (control.Evidence, error)
	Close() error
}

type DispatchResult struct {
	EnvelopeDigest string           `json:"envelope_digest"`
	Evidence       control.Evidence `json:"evidence"`
	// Attested carries the same evidence signed by this node's identity key. The
	// plain Evidence field remains for callers that already hold the node's
	// result locally; anything crossing to the control plane is trusted only
	// after the attestation verifies, so the two must agree.
	Attested *control.AttestedEvidence `json:"attested,omitempty"`
}

// DispatchResponse is the per-message reply. A rejected or failed action
// produces an error response rather than terminating the node, so one bad
// envelope cannot take the daemon down.
type DispatchResponse struct {
	EnvelopeID string          `json:"envelope_id"`
	Result     *DispatchResult `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type Ledger interface {
	Get(string) (DispatchResult, bool)
	Put(string, DispatchResult) error
}

type MemoryLedger struct {
	mu      sync.Mutex
	results map[string]DispatchResult
}

func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{results: make(map[string]DispatchResult)}
}

func (l *MemoryLedger) Get(key string) (DispatchResult, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	result, ok := l.results[key]
	return result, ok
}

func (l *MemoryLedger) Put(key string, result DispatchResult) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.results[key]; exists {
		return fmt.Errorf("idempotency key %q already stored", key)
	}
	l.results[key] = result
	return nil
}

type Dispatcher struct {
	NodeID  string
	Keys    map[string]ed25519.PublicKey
	Runtime Runtime
	Ledger  Ledger
	Now     func() time.Time
	// IdentityKey signs the evidence this node reports. It is the same key the
	// node proves possession of when it enrolls, so the server already trusts its
	// public half. Without it the node still executes, but its evidence carries
	// no attestation and a server requiring one will refuse it.
	IdentityKey ed25519.PrivateKey
	// Desired records server-authorized intent so the node can keep workloads
	// running while the server is unreachable. Optional: without it the node
	// stays purely reactive.
	Desired *DesiredState
	// Observer measures readiness where the workload runs. Optional: a node
	// without one refuses probes rather than answering them wrongly, which is
	// the difference between a control plane that knows it cannot measure and
	// one that believes nothing is ready.
	Observer control.ReadinessObserver
	mu       sync.Mutex
	// leases records the last accepted lease per target so the node can refuse
	// an envelope that contradicts a live claim. Guarded by mu.
	leases map[string]heldLease
}

type heldLease struct {
	leaseID   string
	expiresAt time.Time
}

func (d *Dispatcher) Dispatch(ctx context.Context, signed SignedAction) (DispatchResult, error) {
	// Serialize the first implementation so a duplicate cannot race the ledger
	// check. Later, use per-target leases while retaining this guarantee.
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Runtime == nil || d.Ledger == nil || d.Now == nil {
		return DispatchResult{}, fmt.Errorf("dispatcher is not initialized")
	}
	envelope, _, err := Verify(signed, d.Keys, d.NodeID, d.Now().UTC())
	if err != nil {
		return DispatchResult{}, err
	}
	// A readiness probe is answered before the ledger is consulted, and never
	// written to it.
	//
	// The ledger exists to return the stored result of work already performed,
	// which is right for a mutation and exactly wrong for a measurement: a
	// cached readiness is a stale readiness, and the whole reason readiness
	// expires is that a remembered one must not satisfy a goal. It takes no
	// lease either, because measuring conflicts with nothing.
	if envelope.Action.Kind == control.ActionProbeReadiness {
		return d.probe(envelope)
	}
	// Deduplicate on the work being authorized rather than on the exact
	// envelope, so a legitimate retry is recognized while a key reused for
	// different work is still refused.
	digest, err := WorkDigest(envelope)
	if err != nil {
		return DispatchResult{}, err
	}
	key := envelope.IdempotencyKey
	if previous, ok := d.Ledger.Get(key); ok {
		if previous.EnvelopeDigest != digest {
			return DispatchResult{}, fmt.Errorf("idempotency key %q was reused for a different envelope", key)
		}
		return previous, nil
	}
	// A node cannot see the controller's lease table, but it can refuse an
	// envelope that contradicts one it already accepted for the same target.
	// This is a local backstop against a controller that lost track of its own
	// leases, not a replacement for kernel-side exclusion.
	if err := d.checkLease(envelope); err != nil {
		return DispatchResult{}, err
	}
	evidence, err := d.Runtime.Execute(ctx, envelope.Action)
	if err != nil {
		return DispatchResult{}, err
	}
	// The node stamps its own identity and observation time. A runtime adapter
	// reports what it did; only the node knows where it happened.
	evidence = d.attribute(evidence, envelope.Action)
	// Attest after attribution, never before: the signature has to cover the
	// node id and observation time the projection will act on, or those fields
	// could be edited in transit while the signature still verified.
	attested, err := d.attest(evidence)
	if err != nil {
		return DispatchResult{}, err
	}
	// Record intent only after the mutation succeeded, so the node never
	// supervises a workload the runtime failed to create.
	if err := d.recordDesired(envelope.Action); err != nil {
		return DispatchResult{}, err
	}
	d.noteLease(envelope)
	result := DispatchResult{EnvelopeDigest: digest, Evidence: evidence, Attested: attested}
	if err := d.Ledger.Put(key, result); err != nil {
		return DispatchResult{}, err
	}
	return result, nil
}

// probe measures readiness for one allocation and returns attested evidence.
//
// The measurement is attributed and attested exactly like the result of a
// mutation. That matters more here than anywhere else: readiness is the fact
// that decides whether a goal is satisfied and whether a route carries traffic,
// so a node that could report it unsigned would be the cheapest way to lie about
// the state of the cluster.
func (d *Dispatcher) probe(envelope ActionEnvelope) (DispatchResult, error) {
	if d.Observer == nil {
		return DispatchResult{}, fmt.Errorf("node has no readiness observer")
	}
	if envelope.Action.Probe == nil {
		return DispatchResult{}, fmt.Errorf("probe action carries no target")
	}
	target := *envelope.Action.Probe
	if target.Allocation == "" {
		return DispatchResult{}, fmt.Errorf("probe target names no allocation")
	}

	ready, observed, err := d.Observer.ObserveReadiness(target)
	if err != nil {
		// A probe that could not be taken is reported as a failure rather than
		// as a not-ready answer. They mean different things: one is a broken
		// measurement, the other is a working measurement of a broken workload,
		// and collapsing them would make a misconfigured node look like an
		// outage.
		return DispatchResult{}, fmt.Errorf("probe %q: %w", target.Allocation, err)
	}
	if observed == nil {
		observed = map[string]string{}
	}
	observed["ready"] = fmt.Sprint(ready)

	kind := control.EvidenceAllocationReady
	if target.Kind == control.ProbeAgent {
		kind = control.EvidenceAgentReady
	}
	evidence := d.attribute(control.Evidence{
		Kind: kind, Target: target.Allocation, Observed: observed,
		// The expiry travels with the measurement, so a readiness the control
		// plane keeps is one it knows the age of.
		ExpiresAt: d.Now().UTC().Add(control.DefaultReadinessTTL),
	}, envelope.Action)
	attested, err := d.attest(evidence)
	if err != nil {
		return DispatchResult{}, err
	}
	return DispatchResult{Evidence: evidence, Attested: attested}, nil
}

// attest signs evidence with this node's identity key.
//
// A node without an identity key reports unattested evidence rather than
// failing, so the stdin harness and existing tests keep working. The server
// decides whether that is acceptable: with --require-attestation it is not.
func (d *Dispatcher) attest(evidence control.Evidence) (*control.AttestedEvidence, error) {
	if len(d.IdentityKey) == 0 {
		return nil, nil
	}
	attested, err := control.SignEvidence(evidence, d.NodeID, d.IdentityKey)
	if err != nil {
		return nil, fmt.Errorf("attest evidence: %w", err)
	}
	return &attested, nil
}

// checkLease refuses an envelope whose target is already claimed by a different
// live lease. The node trusts the controller to allocate leases, so this only
// catches a controller that contradicts itself within a lease's lifetime.
func (d *Dispatcher) checkLease(envelope ActionEnvelope) error {
	target := envelope.Action.Target
	if target == "" || envelope.LeaseID == "" {
		return nil
	}
	held, ok := d.leases[target]
	if !ok || !d.Now().UTC().Before(held.expiresAt) {
		return nil
	}
	if held.leaseID == envelope.LeaseID {
		return nil
	}
	return fmt.Errorf("target %q is held by lease %q until %s",
		target, held.leaseID, held.expiresAt.UTC().Format(time.RFC3339))
}

// noteLease records the accepted lease for a target. It expires with the
// envelope, so a stale claim cannot block a target indefinitely.
func (d *Dispatcher) noteLease(envelope ActionEnvelope) {
	target := envelope.Action.Target
	if target == "" || envelope.LeaseID == "" {
		return
	}
	if d.leases == nil {
		d.leases = make(map[string]heldLease)
	}
	d.leases[target] = heldLease{leaseID: envelope.LeaseID, expiresAt: envelope.ExpiresAt}
}

// attribute completes evidence with facts only the node can supply: which node
// observed it, when, and the allocation details the world projection needs to
// account for capacity.
func (d *Dispatcher) attribute(evidence control.Evidence, action control.Action) control.Evidence {
	if evidence.Observed == nil {
		evidence.Observed = map[string]string{}
	}
	evidence.Source = "node:" + d.NodeID
	if evidence.ObservedAt.IsZero() {
		evidence.ObservedAt = d.Now().UTC()
	}
	evidence.Observed["node"] = d.NodeID
	if action.Kind == control.ActionPullImage {
		evidence.Observed["image"] = action.Image
	}
	if action.Kind == control.ActionCreateAllocation {
		evidence.Observed["workload"] = action.Workload
		evidence.Observed["image"] = action.Image
		evidence.Observed["replica"] = fmt.Sprint(action.Replica)
		evidence.Observed["cpu_millis"] = fmt.Sprint(action.Resources.CPUMillis)
		evidence.Observed["memory_mb"] = fmt.Sprint(action.Resources.MemoryMB)
	}
	if action.Kind == control.ActionPublishRoute {
		evidence.Observed["workload"] = action.Workload
		evidence.Observed["port"] = fmt.Sprint(action.Port)
		evidence.Observed["exposure"] = action.Exposure
	}
	return evidence
}

// recordDesired translates an authorized action into local supervision intent.
// The node only ever mirrors what the server authorized; it never adds a
// workload of its own.
func (d *Dispatcher) recordDesired(action control.Action) error {
	if d.Desired == nil {
		return nil
	}
	switch action.Kind {
	case control.ActionCreateAllocation:
		return d.Desired.Record(DesiredAllocation{
			ID: action.Target, Workload: action.Workload, Image: action.Image,
			Resources: action.Resources, Running: false,
			Probe: control.ProbeTarget{
				Allocation: action.Target, Kind: probeKindFor(action.Port),
				Port: action.Port,
			},
			UpdatedAt: d.Now().UTC(),
		})
	case control.ActionAttachVolume:
		if action.Volume == nil {
			return nil
		}
		return d.Desired.AddVolume(action.Target, *action.Volume)
	case control.ActionStartAllocation:
		return d.Desired.SetRunning(action.Target, true)
	case control.ActionStopAllocation:
		return d.Desired.SetRunning(action.Target, false)
	case control.ActionDeleteAllocation:
		return d.Desired.Forget(action.Target)
	default:
		return nil
	}
}

func probeKindFor(port int) string {
	if port > 0 {
		return control.ProbeTCP
	}
	return control.ProbeProcess
}
