package control

// Prober produces evidence independently of the executor that performed the
// mutation. Invariant: an executor may report what it did, but only a probe may
// report that the result is actually serving. Keeping these sources separate is
// what prevents a faulty or adversarial executor from declaring its own success.
type Prober interface {
	// Probe returns fresh evidence for a target, or false when the prober has
	// nothing to say about it.
	Probe(World, Check) (Evidence, bool)
}

// OptimisticProber marks any running allocation ready and any present route
// reachable. It exists so the spike can close the control loop without a real
// probe implementation, and it is the deliberate stand-in for the process, TCP,
// and HTTP probes that must replace it before any real deployment.
type OptimisticProber struct{}

func (OptimisticProber) Probe(world World, check Check) (Evidence, bool) {
	switch check.Kind {
	case CheckAllocationReady:
		allocation, ok := world.Allocations[check.Target]
		if !ok || allocation.Phase != AllocationRunning || allocation.Ready {
			return Evidence{}, false
		}
		return Evidence{
			Kind: EvidenceAllocationReady, Target: check.Target,
			Observed: map[string]string{"ready": "true", "source": "optimistic-prober"},
		}, true

	case CheckRouteReachable:
		if world.Routes[check.Target] == nil {
			return Evidence{}, false
		}
		return Evidence{}, false

	default:
		return Evidence{}, false
	}
}
