package node

import (
	"context"
	"errors"
	"testing"

	"github.com/arcbjorn/a4s/control"
)

const testImage = "registry.example/a4s/web@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeBackend struct {
	pulled    string
	created   ContainerSpec
	startedID string
	startLog  string
	closed    bool
	pullErr   error
}

func (f *fakeBackend) Pull(_ context.Context, image string) (string, error) {
	f.pulled = image
	return "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", f.pullErr
}

func (f *fakeBackend) Create(_ context.Context, spec ContainerSpec) (bool, error) {
	f.created = spec
	return true, nil
}

func (f *fakeBackend) Start(_ context.Context, id, logPath string) (BackendTask, error) {
	f.startedID = id
	f.startLog = logPath
	return BackendTask{PID: 42}, nil
}

func (f *fakeBackend) Close() error {
	f.closed = true
	return nil
}

func TestContainerRuntimeContract(t *testing.T) {
	backend := &fakeBackend{}
	runtime := NewContainerRuntime(backend)
	ctx := context.Background()

	pull, err := runtime.Execute(ctx, control.Action{Kind: control.ActionPullImage, Image: testImage})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if backend.pulled != testImage || pull.Kind != "image.present" {
		t.Fatalf("unexpected pull contract: backend=%q evidence=%+v", backend.pulled, pull)
	}

	create, err := runtime.Execute(ctx, control.Action{
		Kind:      control.ActionCreateAllocation,
		Target:    "web-0",
		Workload:  "web",
		Image:     testImage,
		Resources: control.Resources{CPUMillis: 250, MemoryMB: 128},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if create.Kind != "allocation.created" || !backend.created.NoNewPrivileges || len(backend.created.Capabilities) != 0 {
		t.Fatalf("unsafe or incorrect create contract: %+v %+v", create, backend.created)
	}
	if backend.created.SnapshotKey != "web-0-rootfs" {
		t.Fatalf("unexpected snapshot key %q", backend.created.SnapshotKey)
	}

	start, err := runtime.Execute(ctx, control.Action{Kind: control.ActionStartAllocation, Target: "web-0"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if start.Kind != "allocation.running" || backend.startedID != "web-0" || backend.startLog != "web-0.log" {
		t.Fatalf("unexpected start contract: %+v", start)
	}
	if err := runtime.Close(); err != nil || !backend.closed {
		t.Fatalf("close: err=%v closed=%t", err, backend.closed)
	}
}

func TestContainerRuntimeRejectsMutableOrMismatchedImage(t *testing.T) {
	backend := &fakeBackend{}
	runtime := NewContainerRuntime(backend)

	_, err := runtime.Execute(context.Background(), control.Action{Kind: control.ActionPullImage, Image: "registry.example/web:latest"})
	if err == nil {
		t.Fatal("expected mutable image reference to be rejected")
	}

	backend.pullErr = errors.New("registry unavailable")
	_, err = runtime.Execute(context.Background(), control.Action{Kind: control.ActionPullImage, Image: testImage})
	if err == nil {
		t.Fatal("expected pull failure")
	}
}
