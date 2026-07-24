package node

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

const testImage = "registry.example/a4s/web@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeBackend struct {
	pulled       string
	created      ContainerSpec
	startedID    string
	startLog     string
	closed       bool
	pullErr      error
	stoppedID    string
	stopDeadline time.Duration
	stopResult   BackendStop
	deletedID    string
	deleteErr    error
	state        BackendState
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

func (f *fakeBackend) Stop(_ context.Context, id string, deadline time.Duration) (BackendStop, error) {
	f.stoppedID = id
	f.stopDeadline = deadline
	return f.stopResult, nil
}

func (f *fakeBackend) Delete(_ context.Context, id string) (bool, error) {
	f.deletedID = id
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	return true, nil
}

func (f *fakeBackend) Inspect(_ context.Context, _ string) (BackendState, error) {
	return f.state, nil
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

func TestContainerRuntimeStopAndDeleteContract(t *testing.T) {
	backend := &fakeBackend{stopResult: BackendStop{ExitCode: 137, Killed: true}}
	runtime := NewContainerRuntime(backend)
	ctx := context.Background()

	stop, err := runtime.Execute(ctx, control.Action{Kind: control.ActionStopAllocation, Target: "web-0"})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if backend.stoppedID != "web-0" || backend.stopDeadline != DefaultKillDeadline {
		t.Fatalf("stop did not pass a bounded kill deadline: id=%q deadline=%v", backend.stoppedID, backend.stopDeadline)
	}
	if stop.Kind != control.EvidenceAllocationStopped || stop.Observed["exit_code"] != "137" || stop.Observed["killed"] != "true" {
		t.Fatalf("unexpected stop evidence: %+v", stop)
	}

	del, err := runtime.Execute(ctx, control.Action{Kind: control.ActionDeleteAllocation, Target: "web-0"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if backend.deletedID != "web-0" || del.Kind != control.EvidenceAllocationDeleted {
		t.Fatalf("unexpected delete evidence: %+v", del)
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
