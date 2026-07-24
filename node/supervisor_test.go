package node

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

type supervisedBackend struct {
	fakeBackend
	starts     int
	states     map[string]BackendState
	pullDigest string
	// startLeavesStopped models a container that starts and immediately dies,
	// so readiness can never be observed.
	startLeavesStopped bool
}

func (b *supervisedBackend) Pull(_ context.Context, image string) (string, error) {
	b.pulled = image
	if b.pullDigest != "" {
		return b.pullDigest, b.pullErr
	}
	return "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", b.pullErr
}

func (b *supervisedBackend) Create(_ context.Context, spec ContainerSpec) (bool, error) {
	b.created = spec
	state := b.states[spec.ID]
	state.Exists = true
	b.states[spec.ID] = state
	return true, nil
}

func (b *supervisedBackend) Start(_ context.Context, id, _ string) (BackendTask, error) {
	b.starts++
	state := b.states[id]
	state.Exists = true
	state.Running = !b.startLeavesStopped
	b.states[id] = state
	return BackendTask{PID: 99}, nil
}

func (b *supervisedBackend) Inspect(_ context.Context, id string) (BackendState, error) {
	return b.states[id], nil
}

func (b *supervisedBackend) ListManaged(context.Context) ([]string, error) {
	ids := make([]string, 0, len(b.states))
	for id := range b.states {
		ids = append(ids, id)
	}
	return ids, nil
}

func newSupervisorFixture(t *testing.T) (*Supervisor, *supervisedBackend, *DesiredState) {
	t.Helper()
	backend := &supervisedBackend{states: map[string]BackendState{}}
	desired, err := OpenDesiredState(filepath.Join(t.TempDir(), "desired.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	supervisor := NewSupervisor(NewContainerRuntime(backend), desired)
	supervisor.Backoff = 0
	supervisor.MaxBackoff = 0
	return supervisor, backend, desired
}

// The reason the node holds desired state at all: a workload that dies while
// the server is unreachable must come back without the server's help.
func TestSupervisorRestartsCrashedAllocation(t *testing.T) {
	supervisor, backend, desired := newSupervisorFixture(t)
	if err := desired.Record(DesiredAllocation{ID: "web-0", Workload: "web", Running: true}); err != nil {
		t.Fatal(err)
	}
	// The container exists but its task died.
	backend.states["web-0"] = BackendState{Exists: true, Running: false, ExitCode: 137}

	observations, err := supervisor.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if backend.starts != 1 {
		t.Fatalf("expected one restart, got %d", backend.starts)
	}
	if len(observations) != 1 || observations[0].Kind != control.EvidenceAllocationRunning {
		t.Fatalf("expected running evidence after restart: %+v", observations)
	}
	if observations[0].Observed["restarted"] != "true" {
		t.Fatalf("restart was not reported as such: %+v", observations[0].Observed)
	}
	if entry, _ := desired.Get("web-0"); entry.Restarts != 1 {
		t.Fatalf("restart was not counted: %+v", entry)
	}

	// A second pass with a healthy task must do nothing.
	if _, err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backend.starts != 1 {
		t.Fatalf("supervisor restarted a healthy allocation: starts=%d", backend.starts)
	}
}

// A node must not restart something the server asked to stop. Doing so would
// let the data plane override control-plane intent.
func TestSupervisorDoesNotRestartStoppedIntent(t *testing.T) {
	supervisor, backend, desired := newSupervisorFixture(t)
	if err := desired.Record(DesiredAllocation{ID: "web-0", Workload: "web", Running: false}); err != nil {
		t.Fatal(err)
	}
	backend.states["web-0"] = BackendState{Exists: true, Running: false}

	if _, err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backend.starts != 0 {
		t.Fatalf("supervisor restarted an intentionally stopped allocation: starts=%d", backend.starts)
	}
}

// A crash loop must eventually stop and be reported, rather than being hidden
// behind an endless restart loop.
func TestSupervisorEnforcesCrashLoopBudget(t *testing.T) {
	supervisor, backend, desired := newSupervisorFixture(t)
	supervisor.MaxRestarts = 3
	now := time.Unix(2000, 0).UTC()
	supervisor.Now = func() time.Time { return now }
	if err := desired.Record(DesiredAllocation{ID: "web-0", Workload: "web", Running: true}); err != nil {
		t.Fatal(err)
	}

	var last []control.Evidence
	for i := 0; i < 6; i++ {
		// The task dies again immediately after every restart.
		backend.states["web-0"] = BackendState{Exists: true, Running: false, ExitCode: 1}
		observations, err := supervisor.Reconcile(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		last = observations
		now = now.Add(time.Second)
	}
	if backend.starts != 3 {
		t.Fatalf("expected restarts to stop at the budget, got %d", backend.starts)
	}
	if len(last) != 1 || last[0].Kind != control.EvidenceAllocationFailed {
		t.Fatalf("expected failure evidence once the budget tripped: %+v", last)
	}
	if last[0].Observed["reason"] != "restart budget exhausted" {
		t.Fatalf("unexpected failure reason: %+v", last[0].Observed)
	}
}

// A workload that has been stable long enough earns a fresh restart budget, so
// one bad hour does not permanently disable supervision.
func TestRestartBudgetResetsAfterStableWindow(t *testing.T) {
	supervisor, backend, desired := newSupervisorFixture(t)
	supervisor.MaxRestarts = 2
	supervisor.RestartWindow = time.Minute
	now := time.Unix(3000, 0).UTC()
	supervisor.Now = func() time.Time { return now }
	if err := desired.Record(DesiredAllocation{ID: "web-0", Workload: "web", Running: true}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		backend.states["web-0"] = BackendState{Exists: true, Running: false}
		if _, err := supervisor.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	exhausted := backend.starts

	now = now.Add(2 * time.Minute)
	backend.states["web-0"] = BackendState{Exists: true, Running: false}
	if _, err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backend.starts != exhausted+1 {
		t.Fatalf("budget did not reset after a stable window: before=%d after=%d", exhausted, backend.starts)
	}
}

// Containers left behind by a crash must be surfaced, not silently removed:
// deletion is an authorized action, not a cleanup detail.
func TestSupervisorReportsOrphans(t *testing.T) {
	supervisor, backend, desired := newSupervisorFixture(t)
	if err := desired.Record(DesiredAllocation{ID: "web-0", Workload: "web", Running: true}); err != nil {
		t.Fatal(err)
	}
	backend.states["web-0"] = BackendState{Exists: true, Running: true}
	backend.states["leftover-9"] = BackendState{Exists: true, Running: true}

	orphans, err := supervisor.Orphans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0] != "leftover-9" {
		t.Fatalf("unexpected orphan set: %+v", orphans)
	}
}

// Desired state must survive a node process restart, or the supervision loop
// has nothing to reconcile toward after a reboot.
func TestDesiredStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desired.jsonl")
	first, err := OpenDesiredState(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Record(DesiredAllocation{
		ID: "web-0", Workload: "web", Image: testImage, Running: true,
		Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	}); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenDesiredState(path)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reopened.Get("web-0")
	if !ok || !entry.Running || entry.Image != testImage {
		t.Fatalf("desired state did not survive restart: %+v ok=%t", entry, ok)
	}
}
