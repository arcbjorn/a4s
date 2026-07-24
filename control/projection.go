package control

import "fmt"

// Evidence kinds are the only vocabulary that advances the world projection.
// An executor cannot mutate observed state directly; it can only report
// evidence, which the projection independently interprets.
const (
	EvidenceImagePresent      = "image.present"
	EvidenceAllocationCreated = "allocation.created"
	EvidenceSecretMounted     = "secret.mounted"
	EvidenceVolumeCreated     = "volume.created"
	EvidenceVolumeAttached    = "volume.attached"
	EvidenceVolumeDetached    = "volume.detached"
	EvidenceVolumeSnapshotted = "volume.snapshotted"
	EvidenceVolumeRestored    = "volume.restored"
	EvidenceVolumeBackedUp    = "volume.backed_up"
	EvidenceNetworkAttached   = "network.attached"
	EvidenceNetworkDetached   = "network.detached"
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

	case EvidenceVolumeCreated:
		if evidence.Target == "" {
			return fmt.Errorf("evidence %q must name a volume", evidence.Kind)
		}
		node := evidence.Observed["node"]
		if _, ok := world.Nodes[node]; !ok {
			return fmt.Errorf("evidence %q names unknown node %q", evidence.Kind, node)
		}
		// Re-creating an existing volume must not reset its ownership or
		// generation, which would unfence a writer that has been superseded.
		if _, exists := world.Volumes[evidence.Target]; exists {
			return nil
		}
		world.Volumes[evidence.Target] = &Volume{
			Name: evidence.Target, Node: node, SizeMB: observedInt(evidence, "size_mb"),
		}

	case EvidenceVolumeAttached:
		volume, ok := world.Volumes[evidence.Target]
		if !ok {
			return fmt.Errorf("evidence %q names unknown volume %q", evidence.Kind, evidence.Target)
		}
		owner := evidence.Observed["allocation"]
		if owner == "" {
			return fmt.Errorf("evidence %q must observe an allocation", evidence.Kind)
		}
		allocation, ok := world.Allocations[owner]
		if !ok {
			return fmt.Errorf("evidence %q names unknown allocation %q", evidence.Kind, owner)
		}
		// Attaching to the current owner is an idempotent repeat. Attaching to
		// a different one while the volume is still held would create a second
		// writer, which is the failure this whole subsystem exists to prevent.
		if volume.Owner != "" && volume.Owner != owner {
			return fmt.Errorf("volume %q is owned by allocation %q", evidence.Target, volume.Owner)
		}
		if volume.Owner != owner {
			volume.Owner = owner
			volume.Generation++
		}
		if allocation.Volumes == nil {
			allocation.Volumes = make(map[string]uint64)
		}
		allocation.Volumes[evidence.Target] = volume.Generation

	case EvidenceVolumeDetached:
		volume, ok := world.Volumes[evidence.Target]
		if !ok {
			// Detaching an absent volume is the expected result of a replayed
			// teardown.
			return nil
		}
		owner := evidence.Observed["allocation"]
		// Only the current owner may release. A stale detach from a fenced
		// writer must not free a volume the new owner is already using.
		if volume.Owner != "" && owner != "" && volume.Owner != owner {
			return nil
		}
		if allocation, ok := world.Allocations[volume.Owner]; ok {
			delete(allocation.Volumes, evidence.Target)
		}
		volume.Owner = ""
		// The generation advances on release as well, so a writer that was
		// detached while unreachable cannot resume against the same generation.
		volume.Generation++

	case EvidenceVolumeSnapshotted:
		volume, ok := world.Volumes[evidence.Target]
		if !ok {
			return fmt.Errorf("evidence %q names unknown volume %q", evidence.Kind, evidence.Target)
		}
		snapshot := evidence.Observed["snapshot"]
		checksum := evidence.Observed["checksum"]
		if snapshot == "" || checksum == "" {
			// A snapshot without a checksum cannot be verified at restore time,
			// which makes it a guess rather than a backup.
			return fmt.Errorf("evidence %q must observe a snapshot id and checksum", evidence.Kind)
		}
		if volume.Snapshots == nil {
			volume.Snapshots = make(map[string]string)
		}
		if existing, taken := volume.Snapshots[snapshot]; taken && existing != checksum {
			// Two different contents under one snapshot id means one of them is
			// not what the operator thinks it is.
			return fmt.Errorf("snapshot %q of volume %q already exists with a different checksum",
				snapshot, evidence.Target)
		}
		volume.Snapshots[snapshot] = checksum
		volume.LastSnapshot = snapshot

	case EvidenceVolumeBackedUp:
		volume, ok := world.Volumes[evidence.Target]
		if !ok {
			return fmt.Errorf("evidence %q names unknown volume %q", evidence.Kind, evidence.Target)
		}
		snapshot := evidence.Observed["snapshot"]
		location := evidence.Observed["location"]
		checksum := evidence.Observed["checksum"]
		if snapshot == "" || location == "" {
			return fmt.Errorf("evidence %q must observe a snapshot id and location", evidence.Kind)
		}
		// A backup of content that does not match the snapshot it claims to be
		// would restore something other than what was verified.
		if recorded, known := volume.Snapshots[snapshot]; !known {
			return fmt.Errorf("volume %q has no snapshot %q to back up", evidence.Target, snapshot)
		} else if checksum != "" && checksum != recorded {
			return fmt.Errorf("backup of snapshot %q does not match its recorded checksum", snapshot)
		}
		if volume.Backups == nil {
			volume.Backups = make(map[string]string)
		}
		volume.Backups[snapshot] = location

	case EvidenceVolumeRestored:
		volume, ok := world.Volumes[evidence.Target]
		if !ok {
			return fmt.Errorf("evidence %q names unknown volume %q", evidence.Kind, evidence.Target)
		}
		snapshot := evidence.Observed["snapshot"]
		if snapshot == "" {
			return fmt.Errorf("evidence %q must observe a snapshot id", evidence.Kind)
		}
		if _, known := volume.Snapshots[snapshot]; !known {
			return fmt.Errorf("volume %q was restored from unknown snapshot %q", evidence.Target, snapshot)
		}
		volume.RestoredFrom = snapshot

	case EvidenceSecretMounted:
		allocation, ok := world.Allocations[evidence.Target]
		if !ok {
			return fmt.Errorf("evidence %q names unknown allocation %q", evidence.Kind, evidence.Target)
		}
		name := evidence.Observed["secret"]
		version := evidence.Observed["version"]
		if name == "" || version == "" {
			return fmt.Errorf("evidence %q must observe a secret name and version", evidence.Kind)
		}
		if allocation.Secrets == nil {
			allocation.Secrets = make(map[string]string)
		}
		// Only the version is recorded. The projection is serialized into the
		// durable log, so anything stored here is stored forever.
		allocation.Secrets[name] = version

	case EvidenceNetworkAttached:
		allocation, ok := world.Allocations[evidence.Target]
		if !ok {
			return fmt.Errorf("evidence %q names unknown allocation %q", evidence.Kind, evidence.Target)
		}
		address := evidence.Observed["address"]
		if address == "" {
			return fmt.Errorf("evidence %q must observe an address", evidence.Kind)
		}
		allocation.Address = address

	case EvidenceNetworkDetached:
		allocation, ok := world.Allocations[evidence.Target]
		if !ok {
			// Detaching a network from an allocation that is already gone is
			// the expected result of a replayed teardown.
			return nil
		}
		allocation.Address = ""

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
		if allocation.Ready {
			// An image that has been observed serving becomes the version a
			// failed rollout may return to. Recording it here, from evidence,
			// means a rollback target is always one this cluster saw working.
			if world.KnownGood == nil {
				world.KnownGood = make(map[string]string)
			}
			world.KnownGood[allocation.Workload] = allocation.Image
		}

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
