package node

import (
	"context"
	"fmt"
	"sort"
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
	// Seccomp requests the runtime's default seccomp profile, which blocks the
	// syscalls a container has no business making. An empty capability set stops
	// a process using privileged syscalls it holds no capability for; seccomp
	// stops it reaching the kernel surface those syscalls live on at all, which
	// is where most container escapes are found.
	Seccomp bool
	// AppArmor names a loaded profile to confine the container. Empty leaves
	// AppArmor unset, because naming a profile the host has not loaded makes the
	// container fail to start rather than run unconfined.
	AppArmor string
	// User runs the container as a specific uid:gid instead of the image default.
	// Empty keeps the image's own user, which is commonly root.
	User string
	// ReadOnlyRoot mounts the root filesystem read-only. A workload that needs
	// scratch space should declare a volume rather than write into its image
	// layer, which does not survive a restart anyway.
	ReadOnlyRoot bool
	// UserNamespace maps container uids to unprivileged host uids, so root inside
	// the container is not root on the host. HostUIDBase is the first host uid of
	// the mapped range and UIDMapSize its length.
	UserNamespace bool
	HostUIDBase   uint32
	UIDMapSize    uint32
}

// SandboxProfile is the host-level container hardening the node applies to every
// allocation it creates.
//
// It lives on the runtime rather than in an action, so a proposal cannot ask for
// a weaker sandbox than the node was configured to enforce. An action carries
// what to run; the host decides how tightly to confine it.
type SandboxProfile struct {
	Seccomp       bool
	AppArmor      string
	User          string
	ReadOnlyRoot  bool
	UserNamespace bool
	HostUIDBase   uint32
	UIDMapSize    uint32
}

// DefaultSandboxProfile is the profile applied when none is configured.
//
// Seccomp is on because the runtime's default profile is well tested and costs
// nothing. User namespaces and a read-only root are off by default: both can
// break a workload that was not written for them, and a default that makes
// working images fail to start would push operators to disable hardening
// wholesale rather than adopt it incrementally.
func DefaultSandboxProfile() SandboxProfile {
	return SandboxProfile{Seccomp: true}
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
	// Sandbox is the host hardening applied to every container this runtime
	// creates. The zero value means DefaultSandboxProfile.
	Sandbox SandboxProfile
}

func NewContainerRuntime(backend ContainerBackend) *ContainerRuntime {
	return &ContainerRuntime{backend: backend, Sandbox: DefaultSandboxProfile()}
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
			// Hardening comes from the node's profile, never from the action, so
			// an authorized proposal cannot ask to be confined less.
			Seccomp:       r.Sandbox.Seccomp,
			AppArmor:      r.Sandbox.AppArmor,
			User:          r.Sandbox.User,
			ReadOnlyRoot:  r.Sandbox.ReadOnlyRoot,
			UserNamespace: r.Sandbox.UserNamespace,
			HostUIDBase:   r.Sandbox.HostUIDBase,
			UIDMapSize:    r.Sandbox.UIDMapSize,
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

	case control.ActionCollectImages:
		return r.collectImages(ctx, action)

	default:
		return control.Evidence{}, fmt.Errorf("container runtime does not support action kind %q", action.Kind)
	}
}

// ImageCollector is the optional backend capability that reclaims image
// storage. It is separate from ContainerBackend so a backend that cannot
// enumerate its content store is still a valid runtime, in the same way orphan
// discovery is optional.
type ImageCollector interface {
	// ListImages reports every image reference the content store holds.
	ListImages(context.Context) ([]string, error)
	// RemoveImage deletes one image and reports whether it was present.
	RemoveImage(context.Context, string) (bool, error)
}

// collectImages reclaims images the control plane does not reference.
//
// The protected set arrives with the authorized action rather than being
// decided here. A node choosing for itself what is unreferenced would be acting
// on a local view of a cluster-wide fact, and would eventually delete an image
// the control plane was about to use.
func (r *ContainerRuntime) collectImages(ctx context.Context,
	action control.Action) (control.Evidence, error) {

	collector, ok := r.backend.(ImageCollector)
	if !ok {
		return control.Evidence{}, fmt.Errorf("runtime backend cannot collect images")
	}
	present, err := collector.ListImages(ctx)
	if err != nil {
		return control.Evidence{}, fmt.Errorf("list images: %w", err)
	}

	protected := make(map[string]bool, len(action.Protected))
	for _, image := range action.Protected {
		protected[image] = true
	}

	var reclaimed []string
	var skipped []string
	for _, image := range present {
		if protected[image] {
			skipped = append(skipped, image)
			continue
		}
		if action.DryRun {
			// A dry run reports exactly what a real run would remove, which is
			// what makes the evidence reviewable before anything is destroyed.
			reclaimed = append(reclaimed, image)
			continue
		}
		removed, err := collector.RemoveImage(ctx, image)
		if err != nil {
			return control.Evidence{}, fmt.Errorf("remove image %q: %w", image, err)
		}
		if removed {
			reclaimed = append(reclaimed, image)
		}
	}
	sort.Strings(reclaimed)
	sort.Strings(skipped)

	return control.Evidence{
		Kind: control.EvidenceImagesCollected, Target: action.Node,
		Observed: map[string]string{
			"reclaimed": strings.Join(reclaimed, "\n"),
			"protected": strings.Join(skipped, "\n"),
			"dry_run":   fmt.Sprintf("%t", action.DryRun),
			"scanned":   fmt.Sprintf("%d", len(present)),
		},
	}, nil
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
