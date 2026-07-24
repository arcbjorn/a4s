package control

import "fmt"

// Executor mutates the data plane and reports what it observed. It is not the
// source of truth for world state: the engine advances the world by projecting
// returned evidence, never by trusting an executor's internal view.
type Executor interface {
	Execute(Action) (Evidence, error)
}

// WorldSource supplies the materialized world projection. In the spike it is
// backed by in-memory projection of evidence; the server will rebuild it from
// the durable event log.
type WorldSource interface {
	World() World
}

// BoundExecutor is implemented by executors that issue capabilities on behalf
// of an authorized proposal. The engine tells such an executor which
// authorization the following actions belong to, so the capability it issues
// carries that provenance and cannot be reused for unrelated work.
type BoundExecutor interface {
	Bind(goalID, proposalID string, revision uint64, leaseID string)
}

// MemoryExecutor is the deterministic data plane used by the spike. The real
// node executor maps the same typed actions to containerd, CNI, volumes, and a
// gateway without changing the agent/kernel contract.
//
// It deliberately reports only what a real executor could observe. Readiness in
// particular is not asserted here: the executor reports allocation.running, and
// a separate probe must produce allocation.ready evidence.
type MemoryExecutor struct {
	world World
}

func NewMemoryExecutor(world World) *MemoryExecutor {
	world.normalize()
	return &MemoryExecutor{world: cloneWorld(world)}
}

func (e *MemoryExecutor) World() World {
	return cloneWorld(e.world)
}

// ObserveReadiness implements ReadinessObserver for the in-memory data plane.
// It reports readiness only for an allocation the simulated world shows as
// running, which mirrors what a real process probe can determine.
func (e *MemoryExecutor) ObserveReadiness(target ProbeTarget) (bool, map[string]string, error) {
	allocation, ok := e.world.Allocations[target.Allocation]
	if !ok {
		return false, nil, fmt.Errorf("allocation %q does not exist", target.Allocation)
	}
	return allocation.Phase == AllocationRunning, map[string]string{
		"phase":    string(allocation.Phase),
		"observer": "memory-executor",
	}, nil
}

// Project advances the executor's view of the world from evidence. The engine
// owns this call; the executor never advances the world from its own actions.
func (e *MemoryExecutor) Project(evidence Evidence) error {
	next, err := Project(e.world, evidence)
	if err != nil {
		return err
	}
	e.world = next
	return nil
}

func (e *MemoryExecutor) Execute(action Action) (Evidence, error) {
	switch action.Kind {
	case ActionPullImage:
		if _, ok := e.world.Nodes[action.Node]; !ok {
			return Evidence{}, fmt.Errorf("node %q does not exist", action.Node)
		}
		return Evidence{
			Kind: EvidenceImagePresent, Target: action.Image,
			Observed: map[string]string{"node": action.Node, "image": action.Image},
		}, nil

	case ActionCreateAllocation:
		if _, ok := e.world.Nodes[action.Node]; !ok {
			return Evidence{}, fmt.Errorf("node %q does not exist", action.Node)
		}
		return Evidence{
			Kind: EvidenceAllocationCreated, Target: action.Target,
			Observed: map[string]string{
				"node": action.Node, "workload": action.Workload,
				"image": action.Image, "replica": fmt.Sprint(action.Replica),
				"cpu_millis": fmt.Sprint(action.Resources.CPUMillis),
				"memory_mb":  fmt.Sprint(action.Resources.MemoryMB),
			},
		}, nil

	case ActionCreateVolume:
		if action.Volume == nil {
			return Evidence{}, fmt.Errorf("create volume requires a volume reference")
		}
		if _, ok := e.world.Nodes[action.Node]; !ok {
			return Evidence{}, fmt.Errorf("node %q does not exist", action.Node)
		}
		return Evidence{
			Kind: EvidenceVolumeCreated, Target: action.Volume.Name,
			Observed: map[string]string{"node": action.Node},
		}, nil

	case ActionAttachVolume:
		if action.Volume == nil {
			return Evidence{}, fmt.Errorf("attach volume requires a volume reference")
		}
		if _, ok := e.world.Allocations[action.Target]; !ok {
			return Evidence{}, fmt.Errorf("allocation %q does not exist", action.Target)
		}
		return Evidence{
			Kind: EvidenceVolumeAttached, Target: action.Volume.Name,
			Observed: map[string]string{
				"allocation": action.Target,
				"mount_path": action.Volume.MountPath,
			},
		}, nil

	case ActionDetachVolume:
		if action.Volume == nil {
			return Evidence{}, fmt.Errorf("detach volume requires a volume reference")
		}
		return Evidence{
			Kind: EvidenceVolumeDetached, Target: action.Volume.Name,
			Observed: map[string]string{"allocation": action.Target},
		}, nil

	case ActionSnapshotVolume:
		if action.Volume == nil {
			return Evidence{}, fmt.Errorf("snapshot volume requires a volume reference")
		}
		return Evidence{
			Kind: EvidenceVolumeSnapshotted, Target: action.Volume.Name,
			Observed: map[string]string{"snapshot": action.Volume.Name + "-simulated"},
		}, nil

	case ActionMountSecret:
		if _, ok := e.world.Allocations[action.Target]; !ok {
			return Evidence{}, fmt.Errorf("allocation %q does not exist", action.Target)
		}
		if action.Secret == nil {
			return Evidence{}, fmt.Errorf("mount secret requires a secret reference")
		}
		// Evidence reports the version and where it landed, never the material.
		return Evidence{
			Kind: EvidenceSecretMounted, Target: action.Target,
			Observed: map[string]string{
				"secret":     action.Secret.Name,
				"version":    action.Secret.Version,
				"mount_path": action.Secret.MountPath,
			},
		}, nil

	case ActionAttachNetwork:
		allocation, ok := e.world.Allocations[action.Target]
		if !ok {
			return Evidence{}, fmt.Errorf("allocation %q does not exist", action.Target)
		}
		// The in-memory data plane hands out distinct addresses so replicas do
		// not appear to share one, exactly as a real IPAM would.
		return Evidence{
			Kind: EvidenceNetworkAttached, Target: action.Target,
			Observed: map[string]string{
				"node":    allocation.Node,
				"address": fmt.Sprintf("10.42.%d.%d", allocation.Replica/256+1, allocation.Replica%256+2),
			},
		}, nil

	case ActionStartAllocation:
		allocation, ok := e.world.Allocations[action.Target]
		if !ok {
			return Evidence{}, fmt.Errorf("allocation %q does not exist", action.Target)
		}
		return Evidence{
			Kind: EvidenceAllocationRunning, Target: action.Target,
			Observed: map[string]string{"node": allocation.Node, "phase": string(AllocationRunning)},
		}, nil

	case ActionStopAllocation:
		if _, ok := e.world.Allocations[action.Target]; !ok {
			return Evidence{}, fmt.Errorf("allocation %q does not exist", action.Target)
		}
		return Evidence{
			Kind: EvidenceAllocationStopped, Target: action.Target,
			Observed: map[string]string{"phase": string(AllocationStopped)},
		}, nil

	case ActionDeleteAllocation:
		if _, ok := e.world.Allocations[action.Target]; !ok {
			return Evidence{}, fmt.Errorf("allocation %q does not exist", action.Target)
		}
		return Evidence{
			Kind: EvidenceAllocationDeleted, Target: action.Target,
			Observed: map[string]string{"deleted": "true"},
		}, nil

	case ActionPublishRoute:
		return Evidence{
			Kind: EvidenceRouteReachable, Target: action.Target,
			Observed: map[string]string{
				"workload": action.Workload, "exposure": action.Exposure,
				"port": fmt.Sprint(action.Port),
			},
		}, nil

	default:
		return Evidence{}, fmt.Errorf("unsupported action %q", action.Kind)
	}
}

// simulateAction advances a cloned world during kernel authorization. It models
// the intended effect of an action so the whole plan can be checked before the
// first mutation. It is not an execution path and never touches a host.
func simulateAction(world *World, action Action) error {
	switch action.Kind {
	case ActionPullImage:
		node, ok := world.Nodes[action.Node]
		if !ok {
			return fmt.Errorf("node %q does not exist", action.Node)
		}
		node.Images[action.Image] = true

	case ActionCreateAllocation:
		node, ok := world.Nodes[action.Node]
		if !ok {
			return fmt.Errorf("node %q does not exist", action.Node)
		}
		world.Allocations[action.Target] = &Allocation{
			ID: action.Target, Workload: action.Workload, Replica: action.Replica,
			Node: action.Node, Image: action.Image, Resources: action.Resources,
			Phase: AllocationCreated,
		}
		node.Used = node.Used.Add(action.Resources)

	case ActionCreateVolume:
		if action.Volume == nil {
			return fmt.Errorf("create volume requires a volume reference")
		}
		if _, exists := world.Volumes[action.Volume.Name]; !exists {
			world.Volumes[action.Volume.Name] = &Volume{
				Name: action.Volume.Name, Node: action.Node,
			}
		}

	case ActionAttachVolume:
		if action.Volume == nil {
			return fmt.Errorf("attach volume requires a volume reference")
		}
		volume, ok := world.Volumes[action.Volume.Name]
		if !ok {
			return fmt.Errorf("volume %q does not exist", action.Volume.Name)
		}
		allocation, ok := world.Allocations[action.Target]
		if !ok {
			return fmt.Errorf("allocation %q does not exist", action.Target)
		}
		if volume.Owner != action.Target {
			volume.Owner = action.Target
			volume.Generation++
		}
		if allocation.Volumes == nil {
			allocation.Volumes = make(map[string]uint64)
		}
		allocation.Volumes[action.Volume.Name] = volume.Generation

	case ActionDetachVolume:
		if action.Volume == nil {
			return fmt.Errorf("detach volume requires a volume reference")
		}
		if volume, ok := world.Volumes[action.Volume.Name]; ok {
			if allocation, ok := world.Allocations[volume.Owner]; ok {
				delete(allocation.Volumes, action.Volume.Name)
			}
			volume.Owner = ""
			volume.Generation++
		}

	case ActionSnapshotVolume:
		if action.Volume == nil {
			return fmt.Errorf("snapshot volume requires a volume reference")
		}
		if volume, ok := world.Volumes[action.Volume.Name]; ok {
			volume.LastSnapshot = "simulated"
		}

	case ActionMountSecret:
		allocation, ok := world.Allocations[action.Target]
		if !ok {
			return fmt.Errorf("allocation %q does not exist", action.Target)
		}
		if action.Secret == nil {
			return fmt.Errorf("mount secret requires a secret reference")
		}
		if allocation.Secrets == nil {
			allocation.Secrets = make(map[string]string)
		}
		allocation.Secrets[action.Secret.Name] = action.Secret.Version

	case ActionAttachNetwork:
		allocation, ok := world.Allocations[action.Target]
		if !ok {
			return fmt.Errorf("allocation %q does not exist", action.Target)
		}
		// Simulation only needs to know an address exists, not which one. The
		// real address comes from CNI and arrives as evidence.
		allocation.Address = "simulated"

	case ActionStartAllocation:
		allocation, ok := world.Allocations[action.Target]
		if !ok {
			return fmt.Errorf("allocation %q does not exist", action.Target)
		}
		// Simulation assumes the optimistic outcome so that dependent actions
		// in the same proposal can be checked. Real readiness still requires
		// probe evidence before the goal is considered achieved.
		allocation.Phase = AllocationRunning
		allocation.Ready = true

	case ActionStopAllocation:
		allocation, ok := world.Allocations[action.Target]
		if !ok {
			return fmt.Errorf("allocation %q does not exist", action.Target)
		}
		allocation.Phase = AllocationStopped
		allocation.Ready = false

	case ActionDeleteAllocation:
		allocation, ok := world.Allocations[action.Target]
		if !ok {
			return fmt.Errorf("allocation %q does not exist", action.Target)
		}
		if node, ok := world.Nodes[allocation.Node]; ok {
			node.Used = node.Used.Subtract(allocation.Resources)
		}
		delete(world.Allocations, action.Target)

	case ActionPublishRoute:
		world.Routes[action.Target] = &Route{
			Host: action.Target, Workload: action.Workload,
			Port: action.Port, Exposure: action.Exposure,
		}

	default:
		return fmt.Errorf("unsupported action %q", action.Kind)
	}
	return nil
}
