package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

const acceptanceImage = "registry.example/web@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// recordingSource collects evidence the way an event log would, so the world
// projection can be rebuilt from it after a simulated server restart.
type recordingSource struct{ items []control.Evidence }

func (r *recordingSource) ReplayEvidence() ([]control.Evidence, error) {
	return append([]control.Evidence(nil), r.items...), nil
}

// acceptanceRig runs a real control engine against a real node dispatcher over
// the real transport. Only containerd itself is faked, so the control loop,
// signing, projection, probing, and supervision are all genuinely exercised.
type acceptanceRig struct {
	engine    *control.Engine
	projector *control.DurableProjector
	executor  *RemoteExecutor
	backend   *supervisedBackend
	desired   *DesiredState
	recorded  *recordingSource
	stop      func()
}

func acceptanceWorld() control.World {
	world := control.World{
		Nodes: map[string]*control.Node{
			"base": {
				ID: "base", Healthy: true, Labels: map[string]string{"pool": "base"},
				Capacity: control.Resources{CPUMillis: 4000, MemoryMB: 8192},
			},
		},
		Approvals: map[string]*control.Approval{
			"approve-web": {
				ID: "approve-web", GoalID: "web-public", Scope: "public-route",
				IssuedBy: "operator:test", Granted: true,
			},
		},
	}
	return world
}

func acceptanceGoal() control.Goal {
	return control.Goal{
		APIVersion: control.APIVersion, ID: "web-public",
		Objective: "keep one web replica publicly reachable",
		Workload: control.WorkloadSpec{
			Name: "web", Image: acceptanceImage, Replicas: 1, Port: 8080,
			Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
		},
		Route:       &control.RouteSpec{Host: "web.example.com", Port: 443, Exposure: "public"},
		Constraints: control.Constraints{RequiredLabels: map[string]string{"pool": "base"}},
	}
}

func newAcceptanceRig(t *testing.T) *acceptanceRig {
	t.Helper()
	dir := t.TempDir()
	backend := &supervisedBackend{states: map[string]BackendState{}}
	backend.pullDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := OpenFileLedger(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	desired, err := OpenDesiredState(filepath.Join(dir, "desired.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	gateway := &recordingGateway{}
	dispatcher := &Dispatcher{
		NodeID: "base", Keys: map[string]ed25519.PublicKey{"control-1": publicKey},
		Runtime: &CompositeRuntime{
			Containers: NewContainerRuntime(backend),
			Routes:     NewRouter(gateway),
		},
		Ledger: ledger, Desired: desired, Now: time.Now,
	}

	toNode, fromServer := io.Pipe()
	toServer, fromNode := io.Pipe()
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = Serve(context.Background(), dispatcher, toNode, fromNode)
	}()

	executor := NewRemoteExecutor("base", "control-1", privateKey,
		NewStreamTransport(fromServer, toServer, nil))

	recorded := &recordingSource{}
	projector, err := control.NewDurableProjector(acceptanceWorld(), recorded)
	if err != nil {
		t.Fatal(err)
	}

	// Readiness is measured on the node, exactly as it will be in production.
	observer := NewRuntimeObserver(NewContainerRuntime(backend))
	prober := control.NewMeasuredProber(observer, map[string]control.ProbeTarget{
		"web-0": {Allocation: "web-0", Kind: control.ProbeProcess},
	})

	engine := control.NewEngineWith(executor, projector,
		control.PlacementAgent{}, control.NetworkAgent{})
	engine.WithProbers(prober)

	return &acceptanceRig{
		engine: engine, projector: projector, executor: executor, backend: backend,
		desired: desired, recorded: recorded,
		stop: func() {
			_ = fromServer.Close()
			<-served
			_ = ledger.Close()
		},
	}
}

// recordingGateway stands in for Caddy or Envoy consuming a route snapshot.
// It captures each applied snapshot so tests can assert the gateway is given
// whole configurations rather than incremental edits.
type recordingGateway struct {
	snapshots [][]control.Route
	err       error
}

func (g *recordingGateway) Apply(_ context.Context, routes []control.Route) error {
	if g.err != nil {
		return g.err
	}
	g.snapshots = append(g.snapshots, append([]control.Route(nil), routes...))
	return nil
}

// The headline acceptance case: from an empty node, a goal converges to a
// running, independently verified workload with a published route, using only
// signed typed actions.
func TestAcceptanceGoalConvergesOverTransport(t *testing.T) {
	rig := newAcceptanceRig(t)
	defer rig.stop()

	if err := rig.engine.Run(acceptanceGoal(), 8); err != nil {
		t.Fatalf("goal did not converge: %v", err)
	}

	world := rig.projector.World()
	allocation := world.Allocations["web-0"]
	if allocation == nil || allocation.Phase != control.AllocationRunning {
		t.Fatalf("allocation is not running: %+v", allocation)
	}
	if !allocation.ReadyAt(world.Now()) {
		t.Fatalf("allocation was never independently observed ready: %+v", allocation)
	}
	if world.Routes["web.example.com"] == nil {
		t.Fatalf("route was not published: %+v", world.Routes)
	}
	if world.Nodes["base"].Used != (control.Resources{CPUMillis: 100, MemoryMB: 128}) {
		t.Fatalf("capacity accounting is wrong: %+v", world.Nodes["base"].Used)
	}

	// Readiness came from the prober, not from the executor.
	var readySource string
	for _, event := range rig.engine.Events {
		if event.Evidence != nil && event.Evidence.Kind == control.EvidenceAllocationReady {
			readySource = event.Evidence.Source
		}
	}
	if !strings.HasPrefix(readySource, "prober:") {
		t.Fatalf("readiness did not come from an independent probe: %q", readySource)
	}
}

// A server that restarts must rebuild exactly the world it had, from recorded
// evidence alone, and must then recognize the goal as already achieved rather
// than redoing work.
func TestAcceptanceServerRestartRecoversWorld(t *testing.T) {
	rig := newAcceptanceRig(t)
	defer rig.stop()

	if err := rig.engine.Run(acceptanceGoal(), 8); err != nil {
		t.Fatal(err)
	}
	// Capture the evidence the event log would have persisted.
	for _, event := range rig.engine.Events {
		if event.Evidence != nil {
			rig.recorded.items = append(rig.recorded.items, *event.Evidence)
		}
	}
	before := rig.projector.World()

	// A fresh server process rebuilds from the log.
	restarted, err := control.NewDurableProjector(acceptanceWorld(), rig.recorded)
	if err != nil {
		t.Fatalf("server could not rebuild its world: %v", err)
	}
	after := restarted.World()
	if after.Allocations["web-0"] == nil || after.Routes["web.example.com"] == nil {
		t.Fatalf("restart lost authoritative state: %+v", after)
	}
	if after.Nodes["base"].Used != before.Nodes["base"].Used {
		t.Fatalf("restart changed capacity accounting: before=%+v after=%+v",
			before.Nodes["base"].Used, after.Nodes["base"].Used)
	}

	startsBefore := rig.backend.starts
	recovered := control.NewEngineWith(rig.executor, restarted,
		control.PlacementAgent{}, control.NetworkAgent{})
	recovered.WithProbers(control.NewMeasuredProber(
		NewRuntimeObserver(NewContainerRuntime(rig.backend)),
		map[string]control.ProbeTarget{"web-0": {Allocation: "web-0", Kind: control.ProbeProcess}},
	))
	if err := recovered.Run(acceptanceGoal(), 4); err != nil {
		t.Fatalf("recovered server did not converge: %v", err)
	}
	if rig.backend.starts != startsBefore {
		t.Fatalf("recovered server redid work: starts before=%d after=%d", startsBefore, rig.backend.starts)
	}
}

// A workload that dies while the server is unreachable must be restarted by the
// node itself. This is the property that makes a control-plane outage
// survivable rather than fatal.
func TestAcceptanceNodeSurvivesServerOutage(t *testing.T) {
	rig := newAcceptanceRig(t)
	if err := rig.engine.Run(acceptanceGoal(), 8); err != nil {
		t.Fatal(err)
	}

	// The server goes away entirely.
	rig.stop()

	supervisor := NewSupervisor(NewContainerRuntime(rig.backend), rig.desired)
	supervisor.Backoff = 0
	startsBefore := rig.backend.starts
	// The workload crashes with no control plane to notice.
	rig.backend.states["web-0"] = BackendState{Exists: true, Running: false, ExitCode: 1}

	observations, err := supervisor.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rig.backend.starts != startsBefore+1 {
		t.Fatalf("node did not restart the workload during the outage: before=%d after=%d",
			startsBefore, rig.backend.starts)
	}
	if len(observations) != 1 || observations[0].Kind != control.EvidenceAllocationRunning {
		t.Fatalf("outage restart produced no reportable evidence: %+v", observations)
	}
	if !rig.backend.states["web-0"].Running {
		t.Fatal("workload is still down after supervision")
	}
}

// The gateway must receive a whole route snapshot, not an incremental edit, so
// a partial apply cannot leave routing in a state no one authorized.
func TestAcceptanceGatewayReceivesWholeSnapshot(t *testing.T) {
	gateway := &recordingGateway{}
	router := NewRouter(gateway)
	ctx := context.Background()

	for _, host := range []string{"b.example.com", "a.example.com"} {
		if _, err := router.Execute(ctx, control.Action{
			Kind: control.ActionPublishRoute, Target: host,
			Workload: "web", Port: 443, Exposure: "public",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(gateway.snapshots) != 2 {
		t.Fatalf("expected one snapshot per publish, got %d", len(gateway.snapshots))
	}
	last := gateway.snapshots[1]
	if len(last) != 2 || last[0].Host != "a.example.com" || last[1].Host != "b.example.com" {
		t.Fatalf("gateway did not receive a complete ordered snapshot: %+v", last)
	}
}

// A gateway that refuses a snapshot must not leave the node believing the route
// was published.
func TestAcceptanceFailedGatewayApplyRollsBack(t *testing.T) {
	gateway := &recordingGateway{err: errRouteRejected}
	router := NewRouter(gateway)

	_, err := router.Execute(context.Background(), control.Action{
		Kind: control.ActionPublishRoute, Target: "web.example.com",
		Workload: "web", Port: 443, Exposure: "public",
	})
	if err == nil {
		t.Fatal("expected a failed gateway apply to surface as an error")
	}
	if len(router.routes) != 0 {
		t.Fatalf("failed apply left a phantom route: %+v", router.routes)
	}
}

// A failed readiness measurement must block the goal instead of letting a dead
// workload look healthy.
func TestAcceptanceFailedReadinessBlocksGoal(t *testing.T) {
	rig := newAcceptanceRig(t)
	defer rig.stop()

	// The task never reaches a running state, so no probe can call it ready.
	rig.backend.startLeavesStopped = true

	err := rig.engine.Run(acceptanceGoal(), 4)
	if err == nil {
		t.Fatal("goal converged despite readiness never being observed")
	}
	world := rig.projector.World()
	if allocation := world.Allocations["web-0"]; allocation != nil && allocation.ReadyAt(world.Now()) {
		t.Fatalf("allocation was marked ready without a successful probe: %+v", allocation)
	}
	if world.Routes["web.example.com"] != nil {
		t.Fatal("route was published for a workload that was never ready")
	}
}

var errRouteRejected = errors.New("gateway rejected the route snapshot")
