//go:build linux

package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

type ContainerdConfig struct {
	Address     string
	Namespace   string
	Snapshotter string
	LogDir      string
}

type containerdBackend struct {
	client      *containerd.Client
	snapshotter string
	logDir      string
}

func OpenContainerd(ctx context.Context, config ContainerdConfig) (*ContainerRuntime, error) {
	config = defaultContainerdConfig(config)
	if !filepath.IsAbs(config.Address) || !filepath.IsAbs(config.LogDir) {
		return nil, fmt.Errorf("containerd address and log directory must be absolute paths")
	}
	if err := os.MkdirAll(config.LogDir, 0o750); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	client, err := containerd.New(config.Address, containerd.WithDefaultNamespace(config.Namespace))
	if err != nil {
		return nil, fmt.Errorf("connect to containerd: %w", err)
	}
	healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	serving, err := client.IsServing(healthCtx)
	if err != nil || !serving {
		_ = client.Close()
		if err != nil {
			return nil, fmt.Errorf("containerd health check: %w", err)
		}
		return nil, fmt.Errorf("containerd is not serving")
	}
	return NewContainerRuntime(&containerdBackend{
		client:      client,
		snapshotter: config.Snapshotter,
		logDir:      config.LogDir,
	}), nil
}

func defaultContainerdConfig(config ContainerdConfig) ContainerdConfig {
	if config.Address == "" {
		config.Address = "/run/containerd/containerd.sock"
	}
	if config.Namespace == "" {
		config.Namespace = "a4s"
	}
	if config.LogDir == "" {
		config.LogDir = "/var/log/a4s/allocations"
	}
	return config
}

func (b *containerdBackend) Pull(ctx context.Context, image string) (string, error) {
	opts := []containerd.RemoteOpt{containerd.WithPullUnpack}
	if b.snapshotter != "" {
		opts = append(opts, containerd.WithPullSnapshotter(b.snapshotter))
	}
	pulled, err := b.client.Pull(ctx, image, opts...)
	if err != nil {
		return "", err
	}
	return pulled.Target().Digest.String(), nil
}

func (b *containerdBackend) Create(ctx context.Context, spec ContainerSpec) (bool, error) {
	if existing, err := b.client.LoadContainer(ctx, spec.ID); err == nil {
		labels, labelErr := existing.Labels(ctx)
		if labelErr != nil {
			return false, labelErr
		}
		if labels["a4s.io/managed"] != "true" || labels["a4s.io/workload"] != spec.Workload || labels["a4s.io/image"] != spec.Image {
			return false, fmt.Errorf("container %q exists but is not the requested a4s allocation", spec.ID)
		}
		return false, nil
	} else if !errdefs.IsNotFound(err) {
		return false, err
	}

	image, err := b.client.GetImage(ctx, spec.Image)
	if err != nil {
		return false, err
	}
	opts := []oci.SpecOpts{
		oci.WithImageConfig(image),
		oci.WithHostname(spec.ID),
		oci.WithNamespacedCgroup(),
		oci.WithMemoryLimit(uint64(spec.Resources.MemoryMB) * 1024 * 1024),
		oci.WithCPUCFS(int64(spec.Resources.CPUMillis)*100, 100000),
		oci.WithPidsLimit(256),
	}
	if spec.Namespace != "" {
		// Join the namespace CNI created for this allocation. Without this the
		// container shares the host network and replicas of one workload
		// collide on the same port.
		opts = append(opts, oci.WithLinuxNamespace(specs.LinuxNamespace{
			Type: specs.NetworkNamespace, Path: spec.Namespace,
		}))
	}
	if spec.NoNewPrivileges {
		opts = append(opts, oci.WithNoNewPrivileges)
	}
	opts = append(opts, oci.WithCapabilities(spec.Capabilities))

	_, err = b.client.NewContainer(
		ctx,
		spec.ID,
		containerd.WithImage(image),
		containerd.WithNewSnapshot(spec.SnapshotKey, image),
		containerd.WithNewSpec(opts...),
		containerd.WithContainerLabels(map[string]string{
			"a4s.io/managed":  "true",
			"a4s.io/workload": spec.Workload,
			"a4s.io/image":    spec.Image,
		}),
	)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (b *containerdBackend) Start(ctx context.Context, id, logName string) (BackendTask, error) {
	container, err := b.client.LoadContainer(ctx, id)
	if err != nil {
		return BackendTask{}, err
	}
	if task, taskErr := container.Task(ctx, nil); taskErr == nil {
		status, statusErr := task.Status(ctx)
		if statusErr != nil {
			return BackendTask{}, statusErr
		}
		if status.Status == containerd.Running {
			return BackendTask{PID: task.Pid(), AlreadyRunning: true}, nil
		}
		return BackendTask{}, fmt.Errorf("task %q exists in state %q", id, status.Status)
	} else if !errdefs.IsNotFound(taskErr) {
		return BackendTask{}, taskErr
	}

	logPath := filepath.Join(b.logDir, filepath.Base(logName))
	task, err := container.NewTask(ctx, cio.LogFile(logPath))
	if err != nil {
		return BackendTask{}, err
	}
	if _, err := task.Wait(ctx); err != nil {
		_, _ = task.Delete(ctx)
		return BackendTask{}, err
	}
	if err := task.Start(ctx); err != nil {
		_, _ = task.Delete(ctx)
		return BackendTask{}, err
	}
	return BackendTask{PID: task.Pid()}, nil
}

func (b *containerdBackend) Stop(ctx context.Context, id string, deadline time.Duration) (BackendStop, error) {
	container, err := b.client.LoadContainer(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return BackendStop{AlreadyGone: true}, nil
		}
		return BackendStop{}, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return BackendStop{AlreadyGone: true}, nil
		}
		return BackendStop{}, err
	}
	exitCh, err := task.Wait(ctx)
	if err != nil {
		return BackendStop{}, err
	}
	if err := task.Kill(ctx, syscall.SIGTERM); err != nil && !errdefs.IsNotFound(err) {
		return BackendStop{}, err
	}

	killed := false
	var status containerd.ExitStatus
	select {
	case status = <-exitCh:
	case <-time.After(deadline):
		// The task ignored the graceful signal, so escalate. The kill deadline
		// bounds how long a stop may block the control loop.
		killed = true
		if err := task.Kill(ctx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) {
			return BackendStop{}, err
		}
		select {
		case status = <-exitCh:
		case <-ctx.Done():
			return BackendStop{}, ctx.Err()
		}
	case <-ctx.Done():
		return BackendStop{}, ctx.Err()
	}
	if err := status.Error(); err != nil {
		return BackendStop{}, err
	}
	if _, err := task.Delete(ctx); err != nil && !errdefs.IsNotFound(err) {
		return BackendStop{}, err
	}
	return BackendStop{ExitCode: status.ExitCode(), Killed: killed}, nil
}

func (b *containerdBackend) Delete(ctx context.Context, id string) (bool, error) {
	container, err := b.client.LoadContainer(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			// A replayed delete must succeed rather than fail on absence.
			return false, nil
		}
		return false, err
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return false, err
	}
	// Never delete a container a4s does not own.
	if labels["a4s.io/managed"] != "true" {
		return false, fmt.Errorf("container %q is not an a4s allocation", id)
	}
	if task, taskErr := container.Task(ctx, nil); taskErr == nil {
		status, statusErr := task.Status(ctx)
		if statusErr != nil {
			return false, statusErr
		}
		if status.Status == containerd.Running {
			return false, fmt.Errorf("container %q is still running", id)
		}
		if _, err := task.Delete(ctx); err != nil && !errdefs.IsNotFound(err) {
			return false, err
		}
	} else if !errdefs.IsNotFound(taskErr) {
		return false, taskErr
	}
	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

func (b *containerdBackend) Inspect(ctx context.Context, id string) (BackendState, error) {
	container, err := b.client.LoadContainer(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return BackendState{}, nil
		}
		return BackendState{}, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return BackendState{Exists: true}, nil
		}
		return BackendState{}, err
	}
	status, err := task.Status(ctx)
	if err != nil {
		return BackendState{}, err
	}
	return BackendState{
		Exists:   true,
		Running:  status.Status == containerd.Running,
		PID:      task.Pid(),
		ExitCode: status.ExitStatus,
	}, nil
}

// ListManaged returns the IDs of every a4s-managed container on this node. It
// is the basis for orphan discovery: anything present here but absent from
// desired state was left behind by a crash.
func (b *containerdBackend) ListManaged(ctx context.Context) ([]string, error) {
	containers, err := b.client.Containers(ctx, "labels.\"a4s.io/managed\"==true")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(containers))
	for _, container := range containers {
		ids = append(ids, container.ID())
	}
	return ids, nil
}

func (b *containerdBackend) Close() error {
	return b.client.Close()
}
