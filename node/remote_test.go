package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// roundTrip wires a RemoteExecutor to a real Dispatcher over the real stream
// protocol, so the test exercises signing, framing, verification, dispatch, and
// the response path rather than a mock of them.
type roundTrip struct {
	executor   *RemoteExecutor
	dispatcher *Dispatcher
	backend    *supervisedBackend
	desired    *DesiredState
	ledgerPath string
	stop       func()
}

func newRoundTrip(t *testing.T) *roundTrip {
	t.Helper()
	dir := t.TempDir()
	return newRoundTripAt(t, filepath.Join(dir, "ledger.jsonl"), filepath.Join(dir, "desired.jsonl"),
		&supervisedBackend{states: map[string]BackendState{}})
}

func newRoundTripAt(t *testing.T, ledgerPath, desiredPath string, backend *supervisedBackend) *roundTrip {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := OpenFileLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := OpenDesiredState(desiredPath)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &Dispatcher{
		NodeID: "base", Keys: map[string]ed25519.PublicKey{"control-1": publicKey},
		Runtime: NewContainerRuntime(backend), Ledger: ledger, Desired: desired,
		Now: time.Now,
	}

	// A pipe pair stands in for the eventual network transport. The framing and
	// protocol are identical.
	toNode, fromServer := io.Pipe()
	toServer, fromNode := io.Pipe()
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = Serve(context.Background(), dispatcher, toNode, fromNode)
	}()

	executor := NewRemoteExecutor("base", "control-1", privateKey,
		NewStreamTransport(fromServer, toServer, nil))
	executor.Bind("web-public", "placement-agent-r0", 0, "lease-1")

	return &roundTrip{
		executor: executor, dispatcher: dispatcher, backend: backend, desired: desired,
		ledgerPath: ledgerPath,
		stop: func() {
			_ = fromServer.Close()
			<-served
			_ = ledger.Close()
		},
	}
}

// The control plane issues a signed capability and the node performs the
// mutation. This is the seam the whole design rests on.
func TestRemoteExecutorRoundTrip(t *testing.T) {
	rig := newRoundTrip(t)
	defer rig.stop()

	evidence, err := rig.executor.Execute(control.Action{
		ID: "create-web-0", Kind: control.ActionCreateAllocation, Target: "web-0",
		Workload: "web", Node: "base", Image: testImage, Port: 8080,
		Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Kind != control.EvidenceAllocationCreated || evidence.Target != "web-0" {
		t.Fatalf("unexpected create evidence: %+v", evidence)
	}

	evidence, err = rig.executor.Execute(control.Action{
		ID: "start-web-0", Kind: control.ActionStartAllocation, Target: "web-0", Workload: "web",
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Kind != control.EvidenceAllocationRunning {
		t.Fatalf("unexpected start evidence: %+v", evidence)
	}

	// The node recorded server intent, which is what lets it supervise later.
	entry, ok := rig.desired.Get("web-0")
	if !ok || !entry.Running || entry.Probe.Port != 8080 {
		t.Fatalf("desired state was not recorded: %+v ok=%t", entry, ok)
	}
}

// An unauthorized action must be reported without killing the node, and the
// node must remain usable for the next action.
func TestRemoteExecutorSurvivesRejectedAction(t *testing.T) {
	rig := newRoundTrip(t)
	defer rig.stop()

	// Sign with the wrong key by swapping the executor's key material.
	_, wrongKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	good := rig.executor.PrivateKey
	rig.executor.PrivateKey = wrongKey
	_, err = rig.executor.Execute(control.Action{
		ID: "create-web-0", Kind: control.ActionCreateAllocation, Target: "web-0",
		Workload: "web", Node: "base", Image: testImage,
		Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid action signature") {
		t.Fatalf("expected signature rejection, got %v", err)
	}

	// The node is still alive and serves the next legitimate action.
	rig.executor.PrivateKey = good
	if _, err := rig.executor.Execute(control.Action{
		ID: "create-web-1", Kind: control.ActionCreateAllocation, Target: "web-1",
		Workload: "web", Node: "base", Image: testImage,
		Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	}); err != nil {
		t.Fatalf("node did not survive a rejected action: %v", err)
	}
}

// The crash window that matters: the node mutated the host, then died before
// recording the result. On restart the same action is replayed and must not
// duplicate runtime state.
func TestReplayAfterNodeRestartDoesNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	desiredPath := filepath.Join(dir, "desired.jsonl")
	backend := &supervisedBackend{states: map[string]BackendState{}}

	first := newRoundTripAt(t, ledgerPath, desiredPath, backend)
	action := control.Action{
		ID: "start-web-0", Kind: control.ActionStartAllocation, Target: "web-0", Workload: "web",
	}
	if _, err := first.executor.Execute(control.Action{
		ID: "create-web-0", Kind: control.ActionCreateAllocation, Target: "web-0",
		Workload: "web", Node: "base", Image: testImage,
		Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.executor.Execute(action); err != nil {
		t.Fatal(err)
	}
	startsBefore := backend.starts
	first.stop()

	// The node process restarts against the same durable ledger and state, and
	// the server retries the identical action.
	second := newRoundTripAt(t, ledgerPath, desiredPath, backend)
	defer second.stop()
	if _, err := second.executor.Execute(action); err != nil {
		t.Fatal(err)
	}
	if backend.starts != startsBefore {
		t.Fatalf("replayed action re-executed after restart: before=%d after=%d", startsBefore, backend.starts)
	}
}

// Reusing an idempotency key for different work must be refused. Otherwise a
// buggy or malicious controller could smuggle a new mutation past the ledger.
func TestReplayWithDifferentActionIsRejected(t *testing.T) {
	rig := newRoundTrip(t)
	defer rig.stop()

	if _, err := rig.executor.Execute(control.Action{
		ID: "create-web-0", Kind: control.ActionCreateAllocation, Target: "web-0",
		Workload: "web", Node: "base", Image: testImage,
		Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	}); err != nil {
		t.Fatal(err)
	}
	// Same action ID, so the same derived idempotency key, but different work.
	_, err := rig.executor.Execute(control.Action{
		ID: "create-web-0", Kind: control.ActionCreateAllocation, Target: "web-9",
		Workload: "web", Node: "base", Image: testImage,
		Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	})
	if err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("expected idempotency key reuse rejection, got %v", err)
	}
}

// A controller that resends an action after a timeout issues a fresh envelope
// with new issue/expiry times. That is the same work and must return the stored
// result, not be mistaken for a key collision.
func TestRetryWithFreshEnvelopeReturnsStoredResult(t *testing.T) {
	rig := newRoundTrip(t)
	defer rig.stop()

	action := control.Action{
		ID: "create-web-0", Kind: control.ActionCreateAllocation, Target: "web-0",
		Workload: "web", Node: "base", Image: testImage,
		Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	}
	base := time.Now()
	rig.executor.Now = func() time.Time { return base }
	first, err := rig.executor.Execute(action)
	if err != nil {
		t.Fatal(err)
	}

	// Time advances, so the retry's envelope differs in every timestamp field.
	rig.executor.Now = func() time.Time { return base.Add(20 * time.Second) }
	second, err := rig.executor.Execute(action)
	if err != nil {
		t.Fatalf("legitimate retry was rejected: %v", err)
	}
	if first.Target != second.Target || first.Kind != second.Kind {
		t.Fatalf("retry returned different evidence: first=%+v second=%+v", first, second)
	}
	if rig.backend.created.ID != "web-0" {
		t.Fatalf("unexpected backend state: %+v", rig.backend.created)
	}
}

// An expired envelope must be refused even though it is correctly signed.
func TestExpiredEnvelopeIsRejectedEndToEnd(t *testing.T) {
	rig := newRoundTrip(t)
	defer rig.stop()

	rig.executor.Now = func() time.Time { return time.Now().Add(-10 * time.Minute) }
	_, err := rig.executor.Execute(control.Action{
		ID: "create-web-0", Kind: control.ActionCreateAllocation, Target: "web-0",
		Workload: "web", Node: "base", Image: testImage,
		Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expiry rejection, got %v", err)
	}
}

// An executor that was never bound to an authorized proposal must refuse to
// issue a capability at all.
func TestUnboundExecutorRefusesToIssueCapability(t *testing.T) {
	rig := newRoundTrip(t)
	defer rig.stop()

	rig.executor.Bind("", "", 0, "")
	_, err := rig.executor.Execute(control.Action{
		ID: "create-web-0", Kind: control.ActionCreateAllocation, Target: "web-0",
	})
	if err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("expected unbound executor rejection, got %v", err)
	}
}
