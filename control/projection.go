package control

import "fmt"

// Evidence kinds are the only vocabulary that advances the world projection.
// An executor cannot mutate observed state directly; it can only report
// evidence, which the projection independently interprets.
const (
	EvidenceImagePresent      = "image.present"
	EvidenceAllocationCreated = "allocation.created"
	EvidenceAllocationRunning = "allocation.running"
	EvidenceAllocationReady   = "allocation.ready"
	EvidenceAllocationStopped = "allocation.stopped"
	EvidenceAllocationDeleted = "allocation.deleted"
	EvidenceAllocationFailed  = "allocation.failed"
	EvidenceRouteReachable    = "route.reachable"
	EvidenceRouteRemoved      = "route.removed"
)

// Project applies one piece of evidence to the world and returns the updated
// copy. It is pure: the input world is never mutated, and applying the same
// evidence twice yields the same result. That idempotency is what makes crash
// recovery and action replay safe, because a replayed action produces the same
// evidence and therefore the same projected state.
//
// Project is deliberately the only path from observation to world state. The
// kernel simulates with simulateAction, executors mutate hosts, probes observe,
// and the resulting evidence is projected here.
func Project(world World, evidence Evidence) (World, error) {
	next := cloneWorld(world)
	if err := projectInto(&next, evidence); err != nil {
		return World{}, err
	}
	next.Revision = world.Revision + 1
	// Advance the snapshot's evaluation time so freshness checks compare
	// against when the world was last observed, not an arbitrary clock read.
	if !evidence.ObservedAt.IsZero() && evidence.ObservedAt.After(next.ObservedAt) {
		next.ObservedAt = evidence.ObservedAt
	}
	return next, nil
}

func projectInto(world *World, evidence Evidence) error {
	switch evidence.Kind {
	case EvidenceImagePresent:
		node, ok := world.Nodes[evidence.Observed["node"]]
		if !ok {
			return fmt.Errorf("evidence %q names unknown node %q", evidence.Kind, evidence.Observed["node"])
		}
		image := evidence.Observed["image"]
		if image == "" {
			return fmt.Errorf("evidence %q must observe an image", evidence.Kind)
		}
		node.Images[image] = true

	case EvidenceAllocationCreated:
		node, ok := world.Nodes[evidence.Observed["node"]]
		if !ok {
			return fmt.Errorf("evidence %q names unknown node %q", evidence.Kind, evidence.Observed["node"])
		}
		if evidence.Target == "" {
			return fmt.Errorf("evidence %q must name an allocation", evidence.Kind)
		}
		// Re-projecting creation evidence must not double-count capacity.
		// Idempotency here is what protects the projection from a replayed
		// action after a node crashed between mutation and result recording.
		if _, exists := world.Allocations[evidence.Target]; exists {
			return nil
		}
		resources, err := observedResources(evidence)
		if err != nil {
			return err
		}
		world.Allocations[evidence.Target] = &Allocation{
			ID: evidence.Target, Workload: evidence.Observed["workload"],
			Replica: observedInt(evidence, "replica"), Node: node.ID,
			Image: evidence.Observed["image"], Resources: resources,
			Phase: AllocationCreated,
		}
		node.Used = node.Used.Add(resources)

	case EvidenceAllocationRunning:
		allocation, ok := world.Allocations[evidence.Target]
		if !ok {
			return fmt.Errorf("evidence %q names unknown allocation %q", evidence.Kind, evidence.Target)
		}
		// Running is not ready. Readiness requires separate probe evidence so
		// that an executor cannot declare its own work successful.
		allocation.Phase = AllocationRunning

	case EvidenceAllocationReady:
		allocation, ok := world.Allocations[evidence.Target]
		if !ok {
			return fmt.Errorf("evidence %q names unknown allocation %q", evidence.Kind, evidence.Target)
		}
		if allocation.Phase != AllocationRunning {
			return fmt.Errorf("allocation %q cannot be ready in phase %q", evidence.Target, allocation.Phase)
		}
		allocation.Ready = evidence.Observed["ready"] == "true"
		// Carrying the expiry into the world lets the verifier reject a goal
		// whose readiness is merely remembered rather than currently observed.
		allocation.ReadyExpiresAt = evidence.ExpiresAt

	case EvidenceAllocationStopped:
		allocation, ok := world.Allocations[evidence.Target]
		if !ok {
			return fmt.Errorf("evidence %q names unknown allocation %q", evidence.Kind, evidence.Target)
		}
		// A stopped allocation is never ready. Clearing readiness here keeps a
		// stale ready flag from satisfying a goal or authorizing a route.
		allocation.Phase = AllocationStopped
		allocation.Ready = false

	case EvidenceAllocationFailed:
		allocation, ok := world.Allocations[evidence.Target]
		if !ok {
			return fmt.Errorf("evidence %q names unknown allocation %q", evidence.Kind, evidence.Target)
		}
		allocation.Phase = AllocationStopped
		allocation.Ready = false
		allocation.ExitCode = observedInt(evidence, "exit_code")
		allocation.Restarts = observedInt(evidence, "restarts")

	case EvidenceAllocationDeleted:
		allocation, ok := world.Allocations[evidence.Target]
		if !ok {
			// Deleting an already-absent allocation is the expected result of a
			// replayed delete, so the projection stays idempotent.
			return nil
		}
		if node, ok := world.Nodes[allocation.Node]; ok {
			node.Used = node.Used.Subtract(allocation.Resources)
		}
		delete(world.Allocations, evidence.Target)

	case EvidenceRouteReachable:
		if evidence.Target == "" {
			return fmt.Errorf("evidence %q must name a route host", evidence.Kind)
		}
		world.Routes[evidence.Target] = &Route{
			Host: evidence.Target, Workload: evidence.Observed["workload"],
			Port: observedInt(evidence, "port"), Exposure: evidence.Observed["exposure"],
		}

	case EvidenceRouteRemoved:
		delete(world.Routes, evidence.Target)

	default:
		return fmt.Errorf("unknown evidence kind %q", evidence.Kind)
	}
	return nil
}

func observedResources(evidence Evidence) (Resources, error) {
	resources := Resources{
		CPUMillis: observedInt(evidence, "cpu_millis"),
		MemoryMB:  observedInt(evidence, "memory_mb"),
	}
	if resources.CPUMillis < 1 || resources.MemoryMB < 1 {
		return Resources{}, fmt.Errorf("evidence %q must observe positive resources", evidence.Kind)
	}
	return resources, nil
}

func observedInt(evidence Evidence, key string) int {
	value := 0
	if _, err := fmt.Sscanf(evidence.Observed[key], "%d", &value); err != nil {
		return 0
	}
	return value
}
