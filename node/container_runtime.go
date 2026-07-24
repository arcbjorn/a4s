package node

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// DefaultKillDeadline bounds how long a task may take to exit after being
// signalled before it is killed.
const DefaultKillDeadline = 30 * time.Second

// ContainerSpec is the small, runtime-neutral contract between the a4s node
// daemon and a concrete container runtime. It intentionally exposes less
// authority than the containerd client.
type ContainerSpec struct {
	ID              string
	Workload        string
	Image           string
	Resources       control.Resources
	SnapshotKey     string
	LogPath         string
	NoNewPrivileges bool
	Capabilities    []string
	// VolumeMounts are durable storage bound into the container.
	VolumeMounts []VolumeMountSpec
	// SecretMounts are host paths bound read-only into the container. Material
	// stays on the node's tmpfs; the container sees a file, never a value that
	// passed through the control plane.
	SecretMounts []SecretMountSpec
	// Namespace is the CNI-created network namespace the container must join.
	// Empty means the container keeps the runtime default, which is only
	// appropriate for a workload that serves no port.
	Namespace string
}

// VolumeMountSpec binds one durable volume into a container.
type VolumeMountSpec struct {
	Source      string
	Destination string
	ReadOnly    bool
}

// SecretMountSpec binds one secret file into a container.
type SecretMountSpec struct {
	// Source is the node-side tmpfs path holding the material.
	Source string
	// Destination is where the workload expects to read it.
	Destination string
}

type BackendTask struct {
	PID            uint32
	AlreadyRunning bool
}

type ContainerBackend interface {
	Pull(context.Context, string) (string, error)
	Create(context.Context, ContainerSpec) (bool, error)
	Start(context.Context, string, string) (BackendTask, error)
	// Stop signals the task and waits up to the kill deadline before killing it.
	// It reports the observed exit code and whether the task was already absent.
	Stop(context.Context, string, time.Duration) (BackendStop, error)
	// Delete removes the container and its snapshot. It must succeed when the
	// container is already absent so a replayed delete stays idempotent.
	Delete(context.Context, string) (bool, error)
	// Inspect reports observed runtime state, which the node uses to detect
	// crashed tasks and orphans without trusting its own prior beliefs.
	Inspect(context.Context, string) (BackendState, error)
	Close() error
}

// BackendStop is the observed result of stopping a task.
type BackendStop struct {
	ExitCode    uint32
	AlreadyGone bool
	Killed      bool
}

// BackendState is observed container state, used for readiness probing,
// crash detection, and orphan discovery.
type BackendState struct {
	Exists   bool
	Running  bool
	PID      uint32
	ExitCode uint32
}

type ContainerRuntime struct {
	backend ContainerBackend
	// VolumeMountsFor resolves an allocation's volume mounts, so a container
	// receives the storage that was attached to it.
	VolumeMountsFor func(string) []VolumeMountSpec
	// SecretMountsFor resolves an allocation's secret mounts, so a container
	// receives the credentials that were mounted for it.
	SecretMountsFor func(string) []SecretMountSpec
	// Namespaces resolves an allocation's network namespace. It is set when the
	// node has a network capability, so a container joins the namespace CNI
	// created for it rather than sharing the host network.
	Namespaces func(string) string
}

func NewContainerRuntime(backend ContainerBackend) *ContainerRuntime {
	return &ContainerRuntime{backend: backend}
}

// volumeMounts resolves the volume mounts for an allocation, if any.
func (r *ContainerRuntime) volumeMounts(allocation string) []VolumeMountSpec {
	if r == nil || r.VolumeMountsFor == nil {
		return nil
	}
	return r.VolumeMountsFor(allocation)
}

// secretMounts resolves the secret mounts for an allocation, if any.
func (r *ContainerRuntime) secretMounts(allocation string) []SecretMountSpec {
	if r == nil || r.SecretMountsFor == nil {
		return nil
	}
	return r.SecretMountsFor(allocation)
}

// Namespace resolves the network namespace for an allocation, if one exists.
func (r *ContainerRuntime) Namespace(allocation string) string {
	if r == nil || r.Namespaces == nil {
		return ""
	}
	return r.Namespaces(allocation)
}

func (r *ContainerRuntime) Execute(ctx context.Context, action control.Action) (control.Evidence, error) {
	if r == nil || r.backend == nil {
		return control.Evidence{}, fmt.Errorf("container runtime is not initialized")
	}

	switch action.Kind {
	case control.ActionPullImage:
		want, err := imageDigest(action.Image)
		if err != nil {
			return control.Evidence{}, err
		}
		got, err := r.backend.Pull(ctx, action.Image)
		if err != nil {
			return control.Evidence{}, fmt.Errorf("pull image %q: %w", action.Image, err)
		}
		if got != want {
			return control.Evidence{}, fmt.Errorf("pulled image digest %q does not match requested digest %q", got, want)
		}
		return control.Evidence{
			Kind:   "image.present",
			Target: action.Image,
			Observed: map[string]string{
				"digest": got,
			},
		}, nil

	case control.ActionCreateAllocation:
		if action.Target == "" || action.Workload == "" || action.Image == "" {
			return control.Evidence{}, fmt.Errorf("create allocation requires target, workload, and image")
		}
		if action.Resources.CPUMillis <= 0 || action.Resources.MemoryMB <= 0 {
			return control.Evidence{}, fmt.Errorf("create allocation requires positive resource limits")
		}
		created, err := r.backend.Create(ctx, ContainerSpec{
			VolumeMounts:    r.volumeMounts(action.Target),
			SecretMounts:    r.secretMounts(action.Target),
			Namespace:       r.Namespace(action.Target),
			ID:              action.Target,
			Workload:        action.Workload,
			Image:           action.Image,
			Resources:       action.Resources,
			SnapshotKey:     action.Target + "-rootfs",
			LogPath:         action.Target + ".log",
			NoNewPrivileges: true,
			Capabilities:    []string{},
		})
		if err != nil {
			return control.Evidence{}, fmt.Errorf("create allocation %q: %w", action.Target, err)
		}
		return control.Evidence{
			Kind:   "allocation.created",
			Target: action.Target,
			Observed: map[string]string{
				"created": fmt.Sprintf("%t", created),
				"image":   action.Image,
			},
		}, nil

	case control.ActionStartAllocation:
		if action.Target == "" {
			return control.Evidence{}, fmt.Errorf("start allocation requires target")
		}
		task, err := r.backend.Start(ctx, action.Target, action.Target+".log")
		if err != nil {
			return control.Evidence{}, fmt.Errorf("start allocation %q: %w", action.Target, err)
		}
		return control.Evidence{
			Kind:   "allocation.running",
			Target: action.Target,
			Observed: map[string]string{
				"pid":             fmt.Sprintf("%d", task.PID),
				"already_running": fmt.Sprintf("%t", task.AlreadyRunning),
			},
		}, nil

	case control.ActionStopAllocation:
		if action.Target == "" {
			return control.Evidence{}, fmt.Errorf("stop allocation requires target")
		}
		stopped, err := r.backend.Stop(ctx, action.Target, DefaultKillDeadline)
		if err != nil {
			return control.Evidence{}, fmt.Errorf("stop allocation %q: %w", action.Target, err)
		}
		return control.Evidence{
			Kind:   control.EvidenceAllocationStopped,
			Target: action.Target,
			Observed: map[string]string{
				"exit_code":    fmt.Sprintf("%d", stopped.ExitCode),
				"already_gone": fmt.Sprintf("%t", stopped.AlreadyGone),
				"killed":       fmt.Sprintf("%t", stopped.Killed),
			},
		}, nil

	case control.ActionDeleteAllocation:
		if action.Target == "" {
			return control.Evidence{}, fmt.Errorf("delete allocation requires target")
		}
		deleted, err := r.backend.Delete(ctx, action.Target)
		if err != nil {
			return control.Evidence{}, fmt.Errorf("delete allocation %q: %w", action.Target, err)
		}
		return control.Evidence{
			Kind:   control.EvidenceAllocationDeleted,
			Target: action.Target,
			Observed: map[string]string{
				"deleted": fmt.Sprintf("%t", deleted),
			},
		}, nil

	default:
		return control.Evidence{}, fmt.Errorf("container runtime does not support action kind %q", action.Kind)
	}
}

// Inspect exposes observed container state for probing and reconciliation.
func (r *ContainerRuntime) Inspect(ctx context.Context, id string) (BackendState, error) {
	if r == nil || r.backend == nil {
		return BackendState{}, fmt.Errorf("container runtime is not initialized")
	}
	return r.backend.Inspect(ctx, id)
}

func (r *ContainerRuntime) Close() error {
	if r == nil || r.backend == nil {
		return nil
	}
	return r.backend.Close()
}

func imageDigest(ref string) (string, error) {
	_, digest, ok := strings.Cut(ref, "@")
	if !ok || !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", fmt.Errorf("image %q must use an immutable sha256 digest", ref)
	}
	for _, char := range strings.TrimPrefix(digest, "sha256:") {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return "", fmt.Errorf("image %q has an invalid sha256 digest", ref)
		}
	}
	return digest, nil
}
