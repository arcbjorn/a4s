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
	// Kind is "process", "tcp", "http", or "database".
	Kind string `json:"kind"`
	Port int    `json:"port,omitempty"`
	Path string `json:"path,omitempty"`
	// Address is the allocation's own IP. Probing this rather than loopback is
	// what makes a measurement attributable to one replica.
	Address string `json:"address,omitempty"`
	// Engine names the database engine for a database probe.
	Engine string `json:"engine,omitempty"`
}

const (
	ProbeProcess = "process"
	ProbeTCP     = "tcp"
	ProbeHTTP    = "http"
	// ProbeDatabase asks the engine whether it accepts connections. A TCP probe
	// only proves the port is open, which a database that is still recovering
	// its write-ahead log will pass while refusing every query.
	ProbeDatabase = "database"
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
	if p == nil || p.Observer == nil || check.Kind != CheckAllocationReady {
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
		Kind: EvidenceAllocationReady, Target: check.Target,
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
