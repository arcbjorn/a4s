package node

import (
	"crypto/ed25519"
	"fmt"
	"sync"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// Measuring readiness across the process boundary.
//
// Readiness has to be measured where the workload runs, and the control plane is
// a different process on a different machine from every node. Until this existed
// the only wiring that worked was the acceptance test's, which constructs the
// engine and the node runtime in one process and hands the observer straight to
// the prober. That is a genuine end-to-end test of everything except the part
// that is different in production, and it hid the fact that a real deployment
// measured nothing at all: the server's reconcile loop passed no probers, so
// readiness was never observed, only asserted at creation and then left to
// expire.
//
// RegistryObserver closes that gap by routing each measurement to the node
// holding the allocation, over the same authenticated channel actions already
// use.

// RegistryObserver measures readiness on whichever node holds the allocation.
//
// It is the observer counterpart of RegistryExecutor and deliberately mirrors
// it: one connection per node, resolved on demand, reusing the enrolled session
// rather than opening anything new. A probe is an ordinary signed envelope, so
// it inherits node targeting, expiry, and evidence attestation without a second
// trust boundary to keep correct.
type RegistryObserver struct {
	Registry *Registry
	KeyID    string
	Key      ed25519.PrivateKey
	// NodeKeys and RequireAttestation mirror the executor's settings, so a
	// measurement is trusted on exactly the terms a mutation's result is.
	NodeKeys           map[string]ed25519.PublicKey
	RequireAttestation bool
	AttestationMaxAge  time.Duration
	TTL                time.Duration
	Now                func() time.Time

	mu        sync.Mutex
	executors map[string]*RemoteExecutor
	sequence  uint64
}

func NewRegistryObserver(registry *Registry, keyID string, key ed25519.PrivateKey) *RegistryObserver {
	return &RegistryObserver{
		Registry: registry, KeyID: keyID, Key: key,
		TTL: DefaultEnvelopeTTL, Now: time.Now,
		executors: make(map[string]*RemoteExecutor),
	}
}

// ObserveReadiness asks the node holding the allocation whether it is serving.
//
// A node that is not connected produces an error rather than a not-ready
// answer. The two mean different things: one is the control plane failing to
// measure, the other is a measurement that came back negative, and reporting the
// first as the second would turn every partition into a false report that the
// workload had stopped serving.
func (o *RegistryObserver) ObserveReadiness(target control.ProbeTarget) (bool, map[string]string, error) {
	if target.Node == "" {
		return false, nil, fmt.Errorf(
			"probe target %q does not name a node to measure on", target.Allocation)
	}
	executor, err := o.executorFor(target.Node)
	if err != nil {
		return false, nil, err
	}

	// Bound to no proposal, because a probe authorizes nothing. The executor
	// still requires a binding, so the probe names itself as its own
	// provenance: it is the control plane measuring, not an agent's plan being
	// carried out, and the event log should say so.
	o.mu.Lock()
	o.sequence++
	probeID := fmt.Sprintf("probe-%s-%d", target.Allocation, o.sequence)
	o.mu.Unlock()
	executor.Bind("readiness", probeID, 0, probeID)

	probe := target
	evidence, err := executor.Execute(control.Action{
		ID: probeID, Kind: control.ActionProbeReadiness,
		Target: target.Allocation, Node: target.Node, Probe: &probe,
	})
	if err != nil {
		return false, nil, err
	}
	return evidence.Observed["ready"] == "true", evidence.Observed, nil
}

func (o *RegistryObserver) executorFor(nodeID string) (*RemoteExecutor, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if executor, ok := o.executors[nodeID]; ok {
		return executor, nil
	}
	connection, ok := o.Registry.Get(nodeID)
	if !ok {
		return nil, fmt.Errorf("node %q is not connected", nodeID)
	}
	executor := NewRemoteExecutor(nodeID, o.KeyID, o.Key, connection)
	executor.TTL, executor.Now = o.TTL, o.Now
	executor.NodeKeys = o.NodeKeys
	executor.RequireAttestation = o.RequireAttestation
	executor.AttestationMaxAge = o.AttestationMaxAge
	o.executors[nodeID] = executor
	return executor, nil
}

// Forget drops the cached connection for a node, so a reconnected node is
// measured over its new session rather than a closed one.
func (o *RegistryObserver) Forget(nodeID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.executors, nodeID)
}
