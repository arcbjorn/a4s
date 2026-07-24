package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

type enrollFixture struct {
	registry   *Registry
	listener   *Listener
	nodeKey    ed25519.PrivateKey
	serverKey  ed25519.PrivateKey
	dispatcher *Dispatcher
	backend    *supervisedBackend
	errs       chan error
	stop       func()
}

func newEnrollFixture(t *testing.T) *enrollFixture {
	t.Helper()
	nodePublic, nodePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverPublic, serverPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	registry := NewRegistry()
	errs := make(chan error, 8)
	listener, err := Listen("tcp", "127.0.0.1:0", registry, ListenerConfig{
		NodeKeys:         map[string]ed25519.PublicKey{"base": nodePublic},
		ServerKeyID:      "control-1",
		HandshakeTimeout: 2 * time.Second,
		OnError:          func(err error) { errs <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = listener.Serve(ctx)
	}()

	dir := t.TempDir()
	ledger, err := OpenFileLedger(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	desired, err := OpenDesiredState(filepath.Join(dir, "desired.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	backend := &supervisedBackend{states: map[string]BackendState{}}
	dispatcher := &Dispatcher{
		NodeID: "base", Keys: map[string]ed25519.PublicKey{"control-1": serverPublic},
		Runtime: NewContainerRuntime(backend), Ledger: ledger, Desired: desired, Now: time.Now,
	}

	return &enrollFixture{
		registry: registry, listener: listener, nodeKey: nodePrivate,
		serverKey: serverPrivate, dispatcher: dispatcher, backend: backend, errs: errs,
		stop: func() {
			cancel()
			_ = listener.Close()
			registry.CloseAll()
			wg.Wait()
			_ = ledger.Close()
		},
	}
}

// waitForNode blocks until the node registers or the deadline passes.
func waitForNode(t *testing.T, registry *Registry, nodeID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := registry.Get(nodeID); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("node %q never enrolled", nodeID)
}

// A node that holds its enrolled key is admitted, and the server can then issue
// capabilities to it over the same connection.
func TestEnrolledNodeReceivesCapabilities(t *testing.T) {
	fixture := newEnrollFixture(t)
	defer fixture.stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = DialServer(ctx, "tcp", fixture.listener.Addr().String(), "base",
			fixture.nodeKey, fixture.dispatcher, 2*time.Second)
	}()
	waitForNode(t, fixture.registry, "base")

	executor := NewRegistryExecutor(fixture.registry, "control-1", fixture.serverKey)
	executor.Bind("web-public", "placement-r0", 0, "lease-1")
	evidence, err := executor.Execute(control.Action{
		ID: "create-web-0", Kind: control.ActionCreateAllocation, Target: "web-0",
		Workload: "web", Node: "base", Image: testImage,
		Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	})
	if err != nil {
		t.Fatalf("enrolled node did not accept a capability: %v", err)
	}
	if evidence.Kind != control.EvidenceAllocationCreated {
		t.Fatalf("unexpected evidence: %+v", evidence)
	}
}

// A node whose key is not enrolled must be refused, and must never appear in the
// registry where it could be issued capabilities.
func TestUnenrolledNodeIsRefused(t *testing.T) {
	fixture := newEnrollFixture(t)
	defer fixture.stop()

	_, impostorKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", fixture.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// The impostor claims an enrolled identity but holds the wrong key.
	_, err = ConnectToServer(conn, "base", impostorKey, 2*time.Second)
	if err == nil {
		t.Fatal("an impostor completed enrollment")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("unexpected enrollment error: %v", err)
	}
	if _, registered := fixture.registry.Get("base"); registered {
		t.Fatal("a refused connection was registered")
	}
}

// A node claiming an identity that was never enrolled is refused with the same
// generic reason, so an attacker cannot enumerate valid node identities.
func TestUnknownNodeIdentityIsRefusedGenerically(t *testing.T) {
	fixture := newEnrollFixture(t)
	defer fixture.stop()

	conn, err := net.Dial("tcp", fixture.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, err = ConnectToServer(conn, "does-not-exist", fixture.nodeKey, 2*time.Second)
	if err == nil {
		t.Fatal("an unknown node completed enrollment")
	}
	// The wire reason must not distinguish "unknown node" from "bad signature".
	if !strings.Contains(err.Error(), "enrollment refused") {
		t.Fatalf("refusal leaked why it failed: %v", err)
	}
}

// A node must refuse a server that names a signing key the node does not
// already trust, or a reachable impostor could nominate its own key.
func TestNodeRefusesUntrustedServerKey(t *testing.T) {
	nodePublic, nodePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	listener, err := Listen("tcp", "127.0.0.1:0", registry, ListenerConfig{
		NodeKeys: map[string]ed25519.PublicKey{"base": nodePublic},
		// The server names a key the node's trust map does not contain.
		ServerKeyID:      "rogue-key",
		HandshakeTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = listener.Serve(ctx) }()

	dir := t.TempDir()
	ledger, err := OpenFileLedger(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	trusted, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &Dispatcher{
		NodeID: "base", Keys: map[string]ed25519.PublicKey{"control-1": trusted},
		Runtime: NewContainerRuntime(&supervisedBackend{states: map[string]BackendState{}}),
		Ledger:  ledger, Now: time.Now,
	}

	err = DialServer(ctx, "tcp", listener.Addr().String(), "base", nodePrivate, dispatcher, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "untrusted signing key") {
		t.Fatalf("node accepted an untrusted server key: %v", err)
	}
}

// A capability for a node that is not connected must fail rather than silently
// doing nothing.
func TestRegistryExecutorRefusesDisconnectedNode(t *testing.T) {
	registry := NewRegistry()
	_, serverKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewRegistryExecutor(registry, "control-1", serverKey)
	executor.Bind("web-public", "placement-r0", 0, "lease-1")

	_, err = executor.Execute(control.Action{
		ID: "create-web-0", Kind: control.ActionCreateAllocation,
		Target: "web-0", Workload: "web", Node: "absent",
	})
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("expected a disconnected-node error, got %v", err)
	}
}

// An action with no node cannot be routed and must be reported rather than
// guessed at.
func TestRegistryExecutorRefusesUnroutableAction(t *testing.T) {
	registry := NewRegistry()
	_, serverKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewRegistryExecutor(registry, "control-1", serverKey)
	executor.Bind("web-public", "placement-r0", 0, "lease-1")

	_, err = executor.Execute(control.Action{
		ID: "start-web-0", Kind: control.ActionStartAllocation, Target: "web-0",
	})
	if err == nil || !strings.Contains(err.Error(), "does not name a node") {
		t.Fatalf("expected an unroutable-action error, got %v", err)
	}
}

// A reconnecting node replaces its previous session, so a stale peer cannot keep
// receiving capabilities after the node has reconnected.
func TestReconnectReplacesPriorSession(t *testing.T) {
	fixture := newEnrollFixture(t)
	defer fixture.stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = DialServer(ctx, "tcp", fixture.listener.Addr().String(), "base",
			fixture.nodeKey, fixture.dispatcher, 2*time.Second)
	}()
	waitForNode(t, fixture.registry, "base")
	first, _ := fixture.registry.Get("base")

	go func() {
		_ = DialServer(ctx, "tcp", fixture.listener.Addr().String(), "base",
			fixture.nodeKey, fixture.dispatcher, 2*time.Second)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if current, ok := fixture.registry.Get("base"); ok && current != first {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("reconnecting node did not replace its prior session")
}
