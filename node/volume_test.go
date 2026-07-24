package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

func volumeFixture(t *testing.T) (*Volumes, string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "volumes")
	state := filepath.Join(dir, "volume-state.jsonl")
	volumes, err := OpenVolumes(root, state)
	if err != nil {
		t.Fatal(err)
	}
	return volumes, root, state
}

func volumeAction(kind control.ActionKind, allocation, name string) control.Action {
	ref := control.VolumeRef{Name: name, MountPath: "/var/lib/app"}
	return control.Action{
		Kind: kind, Target: allocation, Workload: "app", Node: "base", Volume: &ref,
	}
}

func createAction(name string) control.Action {
	ref := control.VolumeRef{Name: name, MountPath: "/var/lib/app"}
	return control.Action{
		Kind: control.ActionCreateVolume, Target: name, Workload: "app",
		Node: "base", Volume: &ref,
	}
}

// The node enforces single-writer itself. Trusting the controller alone would
// hand a volume to a second writer whenever the controller's view is stale.
func TestNodeRefusesSecondWriter(t *testing.T) {
	volumes, _, _ := volumeFixture(t)
	ctx := context.Background()

	if _, err := volumes.Execute(ctx, createAction("app-data")); err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-0", "app-data")); err != nil {
		t.Fatal(err)
	}
	_, err := volumes.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-1", "app-data"))
	if err == nil || !strings.Contains(err.Error(), "is held by allocation") {
		t.Fatalf("node handed a volume to a second writer: %v", err)
	}
}

// Ownership must survive node restart. A node that came back believing a volume
// was free could hand it to a second writer while the first still runs.
func TestOwnershipSurvivesNodeRestart(t *testing.T) {
	volumes, root, state := volumeFixture(t)
	ctx := context.Background()

	if _, err := volumes.Execute(ctx, createAction("app-data")); err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-0", "app-data")); err != nil {
		t.Fatal(err)
	}
	_, generation, _ := volumes.Owner("app-data")

	// The node process restarts against the same durable state.
	reopened, err := OpenVolumes(root, state)
	if err != nil {
		t.Fatal(err)
	}
	owner, recovered, exists := reopened.Owner("app-data")
	if !exists || owner != "app-0" {
		t.Fatalf("restart lost ownership: owner=%q exists=%t", owner, exists)
	}
	if recovered != generation {
		t.Fatalf("restart changed the generation: %d -> %d", generation, recovered)
	}
	// A different allocation must still be refused after the restart.
	if _, err := reopened.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-1", "app-data")); err == nil {
		t.Fatal("a restarted node handed the volume to a second writer")
	}
}

// Attaching is idempotent, so a replayed action does not churn the generation
// and fence the very writer it belongs to.
func TestAttachIsIdempotentForSameOwner(t *testing.T) {
	volumes, _, _ := volumeFixture(t)
	ctx := context.Background()

	if _, err := volumes.Execute(ctx, createAction("app-data")); err != nil {
		t.Fatal(err)
	}
	first, err := volumes.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-0", "app-data"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := volumes.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-0", "app-data"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Observed["generation"] != second.Observed["generation"] {
		t.Fatalf("replayed attach advanced the generation: %s -> %s",
			first.Observed["generation"], second.Observed["generation"])
	}
}

// Re-creating a volume must never touch existing data. A create that emptied a
// volume would be indistinguishable from data loss.
func TestRecreatePreservesData(t *testing.T) {
	volumes, root, _ := volumeFixture(t)
	ctx := context.Background()

	if _, err := volumes.Execute(ctx, createAction("app-data")); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "app-data", "important.db")
	if err := os.WriteFile(marker, []byte("irreplaceable"), 0o600); err != nil {
		t.Fatal(err)
	}

	evidence, err := volumes.Execute(ctx, createAction("app-data"))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Observed["created"] != "false" {
		t.Fatalf("re-create was reported as fresh: %+v", evidence.Observed)
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "irreplaceable" {
		t.Fatalf("re-creating the volume destroyed data: %v", err)
	}
}

// A stale detach from a fenced writer must not release a volume the current
// owner is using.
func TestNodeRefusesStaleDetach(t *testing.T) {
	volumes, _, _ := volumeFixture(t)
	ctx := context.Background()

	if _, err := volumes.Execute(ctx, createAction("app-data")); err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-0", "app-data")); err != nil {
		t.Fatal(err)
	}
	_, err := volumes.Execute(ctx, volumeAction(control.ActionDetachVolume, "app-9", "app-data"))
	if err == nil || !strings.Contains(err.Error(), "does not hold volume") {
		t.Fatalf("a stale detach released a live volume: %v", err)
	}
	if owner, _, _ := volumes.Owner("app-data"); owner != "app-0" {
		t.Fatalf("ownership changed after a refused detach: %q", owner)
	}
}

// Detaching advances the generation, so a writer released while unreachable
// cannot resume against the generation it remembers.
func TestNodeDetachAdvancesGeneration(t *testing.T) {
	volumes, _, _ := volumeFixture(t)
	ctx := context.Background()

	if _, err := volumes.Execute(ctx, createAction("app-data")); err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-0", "app-data")); err != nil {
		t.Fatal(err)
	}
	_, before, _ := volumes.Owner("app-data")

	if _, err := volumes.Execute(ctx, volumeAction(control.ActionDetachVolume, "app-0", "app-data")); err != nil {
		t.Fatal(err)
	}
	owner, after, _ := volumes.Owner("app-data")
	if owner != "" {
		t.Fatalf("detach left an owner: %q", owner)
	}
	if after <= before {
		t.Fatalf("detach did not advance the generation: %d -> %d", before, after)
	}
	// The volume is now available to a different allocation.
	if _, err := volumes.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-1", "app-data")); err != nil {
		t.Fatalf("a released volume could not be reattached: %v", err)
	}
}

// Only the owning allocation receives a volume mount, so a container cannot be
// handed storage it does not hold.
func TestMountsOnlyForOwner(t *testing.T) {
	volumes, _, _ := volumeFixture(t)
	ctx := context.Background()
	refs := []control.VolumeRef{{Name: "app-data", MountPath: "/var/lib/app"}}

	if _, err := volumes.Execute(ctx, createAction("app-data")); err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-0", "app-data")); err != nil {
		t.Fatal(err)
	}
	if mounts := volumes.Mounts("app-0", refs); len(mounts) != 1 {
		t.Fatalf("the owner received no mount: %+v", mounts)
	}
	if mounts := volumes.Mounts("app-1", refs); len(mounts) != 0 {
		t.Fatalf("a non-owner received a mount: %+v", mounts)
	}
}

// Snapshotting a live writer would produce a copy that may be internally
// inconsistent, which an operator would later trust for restore.
func TestSnapshotRefusesAttachedVolume(t *testing.T) {
	volumes, _, _ := volumeFixture(t)
	ctx := context.Background()

	if _, err := volumes.Execute(ctx, createAction("app-data")); err != nil {
		t.Fatal(err)
	}
	if _, err := volumes.Execute(ctx, volumeAction(control.ActionAttachVolume, "app-0", "app-data")); err != nil {
		t.Fatal(err)
	}
	_, err := volumes.Execute(ctx, volumeAction(control.ActionSnapshotVolume, "app-data", "app-data"))
	if err == nil || !strings.Contains(err.Error(), "quiesce") {
		t.Fatalf("snapshot of a live writer was allowed: %v", err)
	}
}

// The container must receive its volume as a writable bind mount.
func TestContainerReceivesVolumeMount(t *testing.T) {
	backend := &fakeBackend{}
	runtime := NewContainerRuntime(backend)
	runtime.VolumeMountsFor = func(string) []VolumeMountSpec {
		return []VolumeMountSpec{{Source: "/var/lib/a4s/volumes/app-data", Destination: "/var/lib/app"}}
	}

	if _, err := runtime.Execute(context.Background(), control.Action{
		Kind: control.ActionCreateAllocation, Target: "app-0", Workload: "app",
		Image: testImage, Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	}); err != nil {
		t.Fatal(err)
	}
	if len(backend.created.VolumeMounts) != 1 {
		t.Fatalf("container spec omitted the volume: %+v", backend.created)
	}
	mount := backend.created.VolumeMounts[0]
	if mount.Destination != "/var/lib/app" || mount.ReadOnly {
		t.Fatalf("unexpected volume mount: %+v", mount)
	}
}

// A volume that does not exist cannot be attached, rather than silently
// creating an empty directory the workload would write to.
func TestAttachRequiresExistingVolume(t *testing.T) {
	volumes, _, _ := volumeFixture(t)
	_, err := volumes.Execute(context.Background(),
		volumeAction(control.ActionAttachVolume, "app-0", "missing"))
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("attached a nonexistent volume: %v", err)
	}
}
