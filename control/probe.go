package control

import "time"

// DefaultReadinessTTL bounds how long a readiness observation is trusted. A
// service that was healthy is not necessarily healthy now, so readiness must be
// re-observed rather than remembered indefinitely.
const DefaultReadinessTTL = 30 * time.Second

// Prober produces evidence independently of the executor that performed the
// mutation. Invariant: an executor may report what it did, but only a probe may
// report that the result is actually serving. Keeping these sources separate is
// what prevents a faulty or adversarial executor from declaring its own success.
type Prober interface {
	// Probe returns fresh evidence for a target, or false when the prober has
	// nothing to say about it.
	Probe(World, Check) (Evidence, bool)
}

// ProbeTarget describes what to measure for an allocation. The control plane
// holds the intent; the node performs the measurement.
type ProbeTarget struct {
	Allocation string `json:"allocation"`
	// Kind is "process", "tcp", "http", "database", or "agent".
	Kind string `json:"kind"`
	Port int    `json:"port,omitempty"`
	Path string `json:"path,omitempty"`
	// Provider names the model provider an agent probe must reach.
	Provider string `json:"provider,omitempty"`
	// Address is the allocation's own IP. Probing this rather than loopback is
	// what makes a measurement attributable to one replica.
	Address string `json:"address,omitempty"`
	// Engine names the database engine for a database probe.
	Engine string `json:"engine,omitempty"`
	// Node is where the measurement has to be taken. An allocation is only
	// observable from the node running it, so a prober that dispatches over the
	// network needs this to know who to ask; a co-located observer ignores it.
	Node string `json:"node,omitempty"`
}

const (
	ProbeProcess = "process"
	ProbeTCP     = "tcp"
	ProbeHTTP    = "http"
	// ProbeDatabase asks the engine whether it accepts connections. A TCP probe
	// only proves the port is open, which a database that is still recovering
	// its write-ahead log will pass while refusing every query.
	ProbeDatabase = "database"
	// ProbeAgent asks the runtime whether it can reach its provider with budget
	// remaining. A process probe would pass for an agent whose provider is
	// unreachable or whose ceiling is spent, both of which mean no work can be
	// done despite a healthy-looking container.
	ProbeAgent = "agent"
)

// ReadinessObserver measures whether an allocation is actually serving. It is
// implemented on the node, where the allocation runs, and its results reach the
// kernel as evidence rather than as a status field.
type ReadinessObserver interface {
	// ObserveReadiness reports whether the target is serving, plus a short
	// description of what was measured.
	ObserveReadiness(ProbeTarget) (bool, map[string]string, error)
}

// MeasuredProber turns a ReadinessObserver into probe evidence. Unlike the
// stand-in it replaces, it reports readiness only when a measurement succeeded,
// and it stamps every observation with an expiry.
type MeasuredProber struct {
	Observer ReadinessObserver
	Targets  map[string]ProbeTarget
	TTL      time.Duration
	Now      func() time.Time
}

func NewMeasuredProber(observer ReadinessObserver, targets map[string]ProbeTarget) *MeasuredProber {
	return &MeasuredProber{
		Observer: observer, Targets: targets,
		TTL: DefaultReadinessTTL, Now: time.Now,
	}
}

func (p *MeasuredProber) Probe(world World, check Check) (Evidence, bool) {
	if p == nil || p.Observer == nil {
		return Evidence{}, false
	}
	// Agent readiness is measured the same way, but reported as its own evidence
	// kind so the projection knows a provider and budget were checked rather
	// than a port.
	evidenceKind := EvidenceAllocationReady
	switch check.Kind {
	case CheckAllocationReady:
	case CheckAgentReady:
		evidenceKind = EvidenceAgentReady
	default:
		return Evidence{}, false
	}
	allocation, ok := world.Allocations[check.Target]
	if !ok || allocation.Phase != AllocationRunning {
		return Evidence{}, false
	}
	target, ok := p.Targets[check.Target]
	if !ok {
		// Without a declared probe there is nothing to measure, and readiness
		// must not be assumed.
		return Evidence{}, false
	}
	if target.Address == "" {
		// The world knows where the allocation actually lives.
		target.Address = allocation.Address
	}
	if target.Node == "" {
		// Same reason, for the node a remote observer has to reach. A target
		// registered before the allocation was placed would otherwise name
		// nowhere to measure.
		target.Node = allocation.Node
	}
	ready, observed, err := p.Observer.ObserveReadiness(target)
	if err != nil {
		// A failed measurement is not evidence of failure, only absence of
		// evidence. The verifier treats the allocation as not ready.
		return Evidence{}, false
	}
	if observed == nil {
		observed = map[string]string{}
	}
	observed["ready"] = boolText(ready)
	observed["probe"] = target.Kind

	now := p.now()
	ttl := p.TTL
	if ttl <= 0 {
		ttl = DefaultReadinessTTL
	}
	return Evidence{
		Kind: evidenceKind, Target: check.Target,
		Source: "prober:" + target.Kind, ObservedAt: now,
		ExpiresAt: now.Add(ttl), Observed: observed,
	}, true
}

func (p *MeasuredProber) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
