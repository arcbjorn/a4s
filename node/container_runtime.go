package node

import (
	"context"
	"fmt"
	"strings"

	"github.com/arcbjorn/a4s/control"
)

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
}

type BackendTask struct {
	PID            uint32
	AlreadyRunning bool
}

type ContainerBackend interface {
	Pull(context.Context, string) (string, error)
	Create(context.Context, ContainerSpec) (bool, error)
	Start(context.Context, string, string) (BackendTask, error)
	Close() error
}

type ContainerRuntime struct {
	backend ContainerBackend
}

func NewContainerRuntime(backend ContainerBackend) *ContainerRuntime {
	return &ContainerRuntime{backend: backend}
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

	default:
		return control.Evidence{}, fmt.Errorf("container runtime does not support action kind %q", action.Kind)
	}
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
