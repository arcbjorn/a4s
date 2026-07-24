package node

import (
	"context"
	"strings"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

// collectBackend is a container backend that can enumerate and remove images.
type collectBackend struct {
	supervisedBackend
	images  []string
	removed []string
}

func (b *collectBackend) ListImages(context.Context) ([]string, error) {
	return append([]string(nil), b.images...), nil
}

func (b *collectBackend) RemoveImage(_ context.Context, name string) (bool, error) {
	for index, image := range b.images {
		if image == name {
			b.images = append(b.images[:index:index], b.images[index+1:]...)
			b.removed = append(b.removed, name)
			return true, nil
		}
	}
	return false, nil
}

func collectRuntime(images ...string) (*ContainerRuntime, *collectBackend) {
	backend := &collectBackend{
		supervisedBackend: supervisedBackend{states: map[string]BackendState{}},
		images:            images,
	}
	return NewContainerRuntime(backend), backend
}

// Only images outside the protected set may be reclaimed.
func TestCollectImagesReclaimsUnprotectedImages(t *testing.T) {
	runtime, backend := collectRuntime("keep@sha256:aaa", "drop@sha256:bbb")

	evidence, err := runtime.Execute(context.Background(), control.Action{
		ID: "gc", Kind: control.ActionCollectImages, Node: "base",
		Protected: []string{"keep@sha256:aaa"},
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if evidence.Kind != control.EvidenceImagesCollected {
		t.Fatalf("unexpected evidence kind %q", evidence.Kind)
	}
	if got := evidence.Observed["reclaimed"]; got != "drop@sha256:bbb" {
		t.Fatalf("reclaimed = %q, want the unprotected image", got)
	}
	if len(backend.removed) != 1 || backend.removed[0] != "drop@sha256:bbb" {
		t.Fatalf("backend removals = %v", backend.removed)
	}
	// The protected image must still be present.
	if len(backend.images) != 1 || backend.images[0] != "keep@sha256:aaa" {
		t.Fatalf("remaining images = %v", backend.images)
	}
}

// A dry run must report exactly what a real run would remove while removing
// nothing, or the preview an operator reviews is not the change they approve.
func TestCollectImagesDryRunRemovesNothing(t *testing.T) {
	runtime, backend := collectRuntime("keep@sha256:aaa", "drop@sha256:bbb")

	evidence, err := runtime.Execute(context.Background(), control.Action{
		ID: "gc", Kind: control.ActionCollectImages, Node: "base",
		Protected: []string{"keep@sha256:aaa"}, DryRun: true,
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got := evidence.Observed["reclaimed"]; got != "drop@sha256:bbb" {
		t.Fatalf("dry run reported %q, want what it would remove", got)
	}
	if evidence.Observed["dry_run"] != "true" {
		t.Fatal("evidence does not record that this was a dry run")
	}
	if len(backend.removed) != 0 {
		t.Fatalf("dry run removed %v", backend.removed)
	}
	if len(backend.images) != 2 {
		t.Fatalf("dry run changed the content store: %v", backend.images)
	}
}

// With everything protected there is nothing to reclaim, and the run must be a
// no-op rather than an error.
func TestCollectImagesWithEverythingProtected(t *testing.T) {
	runtime, backend := collectRuntime("a@sha256:1", "b@sha256:2")

	evidence, err := runtime.Execute(context.Background(), control.Action{
		ID: "gc", Kind: control.ActionCollectImages, Node: "base",
		Protected: []string{"a@sha256:1", "b@sha256:2"},
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if evidence.Observed["reclaimed"] != "" {
		t.Fatalf("reclaimed %q with everything protected", evidence.Observed["reclaimed"])
	}
	if len(backend.removed) != 0 {
		t.Fatalf("removed %v with everything protected", backend.removed)
	}
	if got := evidence.Observed["scanned"]; got != "2" {
		t.Fatalf("scanned = %q, want 2", got)
	}
}

// Replaying a collection must be safe, because an already-absent image reports
// as not removed rather than failing.
func TestCollectImagesIsIdempotent(t *testing.T) {
	runtime, backend := collectRuntime("drop@sha256:bbb")
	action := control.Action{
		ID: "gc", Kind: control.ActionCollectImages, Node: "base",
	}
	if _, err := runtime.Execute(context.Background(), action); err != nil {
		t.Fatalf("first collect: %v", err)
	}
	evidence, err := runtime.Execute(context.Background(), action)
	if err != nil {
		t.Fatalf("replayed collect: %v", err)
	}
	if evidence.Observed["reclaimed"] != "" {
		t.Fatalf("replay reclaimed %q, want nothing left", evidence.Observed["reclaimed"])
	}
	if len(backend.removed) != 1 {
		t.Fatalf("replay removed the image twice: %v", backend.removed)
	}
}

// A backend that cannot enumerate its content store must refuse rather than
// silently report an empty collection.
func TestCollectImagesRefusedByIncapableBackend(t *testing.T) {
	runtime := NewContainerRuntime(&supervisedBackend{states: map[string]BackendState{}})
	_, err := runtime.Execute(context.Background(), control.Action{
		ID: "gc", Kind: control.ActionCollectImages, Node: "base",
	})
	if err == nil {
		t.Fatal("expected an incapable backend to refuse collection")
	}
	if !strings.Contains(err.Error(), "collect images") {
		t.Fatalf("unexpected error: %v", err)
	}
}
