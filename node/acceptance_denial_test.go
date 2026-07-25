package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// The denial half of the acceptance matrix. The convergence tests prove the
// system does what it should; these prove it refuses what it must, end to end
// over the real protocol rather than at a unit boundary.

// signedFor builds a capability the way a server would, so a test can then
// corrupt exactly one field and observe the node's answer.
func signedFor(t *testing.T, key ed25519.PrivateKey, keyID string,
	envelope ActionEnvelope) SignedAction {

	t.Helper()
	signed, err := Sign(envelope, keyID, key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func acceptanceEnvelope(nodeID string) ActionEnvelope {
	now := time.Now().UTC()
	return ActionEnvelope{
		Version: EnvelopeVersion, ID: "env-1", NodeID: nodeID,
		GoalID: "web-public", ProposalID: "p-1", WorldRevision: 1,
		LeaseID: "lease-1", IdempotencyKey: "key-1",
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
		Action: control.Action{
			ID: "create-web-0", Kind: control.ActionCreateAllocation, Target: "web-0",
			Workload: "web", Node: nodeID, Image: acceptanceImage,
			Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
		},
	}
}

// denialRig is a dispatcher wired the way the acceptance rig wires one, without
// the engine. These tests drive the node directly to assert its refusals.
func denialRig(t *testing.T) (*Dispatcher, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	backend := &supervisedBackend{states: map[string]BackendState{}}
	backend.pullDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	ledger, err := OpenFileLedger(t.TempDir() + "/ledger.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	desired, err := OpenDesiredState(t.TempDir() + "/desired.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	return &Dispatcher{
		NodeID: "base", Keys: map[string]ed25519.PublicKey{"control-1": publicKey},
		Runtime: &CompositeRuntime{Containers: NewContainerRuntime(backend)},
		Ledger:  ledger, Desired: desired, Now: time.Now,
	}, privateKey
}

// A capability signed by a key the node does not trust must be refused before
// its payload is interpreted.
func TestAcceptanceRefusesUnknownSigningKey(t *testing.T) {
	dispatcher, _ := denialRig(t)
	_, stranger, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	signed := signedFor(t, stranger, "control-1", acceptanceEnvelope("base"))
	if _, err := dispatcher.Dispatch(context.Background(), signed); err == nil {
		t.Fatal("node executed an action signed by an untrusted key")
	}
}

// Editing the envelope after signing must invalidate it. This is the check that
// makes every other field in the envelope meaningful.
func TestAcceptanceRefusesTamperedEnvelope(t *testing.T) {
	dispatcher, key := denialRig(t)
	signed := signedFor(t, key, "control-1", acceptanceEnvelope("base"))

	var envelope ActionEnvelope
	if err := json.Unmarshal(signed.EnvelopeBytes, &envelope); err != nil {
		t.Fatal(err)
	}
	// Raise the resource limits after the server authorized them.
	envelope.Action.Resources.MemoryMB = 65536
	edited, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	signed.EnvelopeBytes = edited

	if _, err := dispatcher.Dispatch(context.Background(), signed); err == nil {
		t.Fatal("node executed an action whose envelope was edited after signing")
	}
}

// A capability addressed to another node must be refused even when correctly
// signed, or one node's compromise would reach the whole fleet.
func TestAcceptanceRefusesCapabilityForAnotherNode(t *testing.T) {
	dispatcher, key := denialRig(t)
	signed := signedFor(t, key, "control-1", acceptanceEnvelope("other-node"))

	if _, err := dispatcher.Dispatch(context.Background(), signed); err == nil {
		t.Fatal("node executed a capability addressed to a different node")
	}
}

// An expired capability must be refused, so a captured envelope stops working.
func TestAcceptanceRefusesExpiredCapability(t *testing.T) {
	dispatcher, key := denialRig(t)
	envelope := acceptanceEnvelope("base")
	past := time.Now().UTC().Add(-10 * time.Minute)
	envelope.IssuedAt = past
	envelope.ExpiresAt = past.Add(time.Minute)

	signed := signedFor(t, key, "control-1", envelope)
	if _, err := dispatcher.Dispatch(context.Background(), signed); err == nil {
		t.Fatal("node executed an expired capability")
	}
}

// Reusing one idempotency key for different work must be refused, or a replayed
// key could smuggle an action the server never authorized.
func TestAcceptanceRefusesIdempotencyKeyReuse(t *testing.T) {
	dispatcher, key := denialRig(t)
	first := acceptanceEnvelope("base")
	if _, err := dispatcher.Dispatch(context.Background(),
		signedFor(t, key, "control-1", first)); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}

	// Same idempotency key, different action.
	second := acceptanceEnvelope("base")
	second.ID = "env-2"
	second.Action.ID = "create-web-1"
	second.Action.Target = "web-1"
	if _, err := dispatcher.Dispatch(context.Background(),
		signedFor(t, key, "control-1", second)); err == nil {
		t.Fatal("node accepted one idempotency key for two different actions")
	}
}

// A genuine retry must return the stored result rather than mutating twice.
// This is what makes a network retry safe after a node crash.
func TestAcceptanceReplayReturnsStoredResult(t *testing.T) {
	dispatcher, key := denialRig(t)
	envelope := acceptanceEnvelope("base")

	first, err := dispatcher.Dispatch(context.Background(),
		signedFor(t, key, "control-1", envelope))
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	second, err := dispatcher.Dispatch(context.Background(),
		signedFor(t, key, "control-1", envelope))
	if err != nil {
		t.Fatalf("replayed dispatch: %v", err)
	}

	if first.Evidence.Kind != second.Evidence.Kind || first.Evidence.Target != second.Evidence.Target {
		t.Fatalf("replay produced different evidence:\n%+v\n%+v", first, second)
	}
}

// A stale observation must not satisfy a goal. Readiness expires, and traffic
// must never be sent somewhere nobody has recently measured as serving.
func TestAcceptanceStaleReadinessDoesNotSatisfyGoal(t *testing.T) {
	rig := newAcceptanceRig(t)
	defer rig.stop()

	if err := rig.engine.Run(acceptanceGoal(), 12); err != nil {
		t.Fatalf("converge: %v", err)
	}

	// Age the readiness evidence past its expiry and rebuild the world.
	for index := range rig.recorded.items {
		if rig.recorded.items[index].Kind == control.EvidenceAllocationReady {
			rig.recorded.items[index].ExpiresAt = time.Now().UTC().Add(-time.Minute)
		}
	}
	rebuilt, err := control.NewDurableProjector(acceptanceWorld(), rig.recorded)
	if err != nil {
		t.Fatal(err)
	}

	world := rebuilt.World()
	directory := control.BuildDirectory(world, map[string]int{"web": 8080})
	if len(directory["web"].Endpoints) != 0 {
		t.Fatalf("expired readiness still produced endpoints: %+v", directory["web"])
	}
}

// A node restart between mutation and result recording must not duplicate
// runtime state when the action is replayed.
func TestAcceptanceReplayAfterNodeRestartDoesNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	backend := &supervisedBackend{states: map[string]BackendState{}}
	backend.pullDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	newDispatcher := func() (*Dispatcher, func()) {
		ledger, err := OpenFileLedger(dir + "/ledger.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		desired, err := OpenDesiredState(dir + "/desired.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		return &Dispatcher{
			NodeID: "base", Keys: map[string]ed25519.PublicKey{"control-1": publicKey},
			Runtime: &CompositeRuntime{Containers: NewContainerRuntime(backend)},
			Ledger:  ledger, Desired: desired, Now: time.Now,
		}, func() { _ = ledger.Close() }
	}

	envelope := acceptanceEnvelope("base")
	signed := signedFor(t, privateKey, "control-1", envelope)

	first, stop := newDispatcher()
	if _, err := first.Dispatch(context.Background(), signed); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	created := len(backend.states)
	stop()

	// The node restarts and the server, having never recorded a result,
	// replays the same capability.
	second, stopSecond := newDispatcher()
	defer stopSecond()
	if _, err := second.Dispatch(context.Background(), signed); err != nil {
		t.Fatalf("replayed dispatch after restart: %v", err)
	}

	if len(backend.states) != created {
		t.Fatalf("replay after restart created %d containers, want %d",
			len(backend.states), created)
	}
}

// Deleting a service must remove the route as well as the container, so a
// deleted workload leaves no hostname pointing at nothing.
func TestAcceptanceDeletionRemovesRouteAndContainer(t *testing.T) {
	rig := newAcceptanceRig(t)
	defer rig.stop()

	if err := rig.engine.Run(acceptanceGoal(), 12); err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(rig.backend.states) == 0 {
		t.Fatal("nothing was created to delete")
	}

	// Retire the workload the way a rollout would: stop, then delete.
	//
	// The node remembers the lease that last claimed this target and refuses a
	// contradicting one, which is what stops two proposals interleaving. A
	// teardown therefore continues under the lease already held rather than
	// inventing a competing one.
	leaseID := rig.dispatcher.leases["web-0"].leaseID
	if leaseID == "" {
		t.Fatal("no lease was recorded for the allocation")
	}
	rig.executor.Bind("web-public", "teardown", rig.projector.World().Revision, leaseID)
	for _, action := range []control.Action{
		{ID: "stop-web-0", Kind: control.ActionStopAllocation, Target: "web-0",
			Workload: "web", Node: "base"},
		{ID: "delete-web-0", Kind: control.ActionDeleteAllocation, Target: "web-0",
			Workload: "web", Node: "base"},
	} {
		evidence, err := rig.executor.Execute(action)
		if err != nil {
			t.Fatalf("%s: %v", action.Kind, err)
		}
		if err := rig.projector.Project(evidence); err != nil {
			t.Fatalf("project %s: %v", action.Kind, err)
		}
	}

	if _, present := rig.projector.World().Allocations["web-0"]; present {
		t.Fatal("the allocation survived deletion")
	}
	// With nothing serving, the route resolves to no endpoints rather than
	// silently keeping the last known one.
	snapshots := control.BuildRouteSnapshots(rig.projector.World(),
		map[string]int{"web": 8080})
	for _, snapshot := range snapshots {
		if len(snapshot.Endpoints) != 0 {
			t.Fatalf("route %q still names endpoints after deletion: %+v",
				snapshot.Host, snapshot.Endpoints)
		}
	}
}

// An unapproved public route must be refused end to end, not merely at the
// unit boundary. This is the approval gate the whole model rests on.
//
// The workload is driven all the way to ready first, so the network agent
// actually proposes the route. A goal that stalled earlier would pass this test
// without the approval check ever being reached.
func TestAcceptanceUnapprovedPublicRouteIsRefused(t *testing.T) {
	rig := newAcceptanceRig(t)
	defer rig.stop()

	// Converge with the approval in place so the workload reaches readiness.
	if err := rig.engine.Run(acceptanceGoal(), 12); err != nil {
		t.Fatalf("converge: %v", err)
	}

	// Take the converged world and withdraw every approval from it. The
	// workload is still ready, so the network agent has real work to propose;
	// the only thing that changed is that nobody authorized public exposure.
	unapproved := rig.projector.World()
	unapproved.Approvals = nil
	// The route from the first run must not be carried into this check, or the
	// agent would consider the work already done and propose nothing.
	unapproved.Routes = map[string]*control.Route{}
	// Names are already current, so the agent's only remaining work is the
	// route itself.
	for _, node := range unapproved.Nodes {
		node.ZoneFingerprint = control.BuildServiceZone(unapproved,
			map[string]int{"web": 8080}, node.ID).Fingerprint()
	}

	proposal, err := (control.NetworkAgent{}).Propose(acceptanceGoal(), unapproved)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	var route *control.Action
	for index, action := range proposal.Actions {
		if action.Kind == control.ActionPublishRoute {
			route = &proposal.Actions[index]
		}
	}
	if route == nil {
		t.Fatalf("the network agent proposed no route to deny: %+v", proposal.Actions)
	}

	kernel := control.Kernel{Policy: control.DefaultPolicy()}
	err = kernel.Authorize((control.NetworkAgent{}).Descriptor(),
		acceptanceGoal(), unapproved, proposal)
	if err == nil {
		t.Fatal("the kernel authorized an unapproved public route")
	}
	if !strings.Contains(err.Error(), "public-route approval") {
		t.Fatalf("denial did not name the missing approval: %v", err)
	}
}

// attestingRig is a dispatcher that signs its evidence, paired with an executor
// that requires an attestation, so these tests exercise the boundary the way a
// hardened deployment runs it.
func attestingRig(t *testing.T) (*Dispatcher, *RemoteExecutor, ed25519.PrivateKey) {
	t.Helper()
	dispatcher, controlKey := denialRig(t)
	nodePublic, nodePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.IdentityKey = nodePrivate

	executor := NewRemoteExecutor("base", "control-1", controlKey, nil)
	executor.NodeKeys = map[string]ed25519.PublicKey{"base": nodePublic}
	executor.RequireAttestation = true
	return dispatcher, executor, controlKey
}

// The headline case: evidence a node signed is accepted, and the same evidence
// with the signature stripped is not. Without this, the transport's
// authentication would be all that stands behind every fact the world acts on.
func TestAcceptanceRequiresAttestedEvidence(t *testing.T) {
	dispatcher, executor, controlKey := attestingRig(t)
	action := acceptanceEnvelope("base").Action

	result, err := dispatcher.Dispatch(context.Background(),
		signedFor(t, controlKey, "control-1", acceptanceEnvelope("base")))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result.Attested == nil {
		t.Fatal("the node returned no attestation")
	}
	if _, err := executor.trustedEvidence(result, action); err != nil {
		t.Fatalf("attested evidence was refused: %v", err)
	}

	stripped := result
	stripped.Attested = nil
	if _, err := executor.trustedEvidence(stripped, action); err == nil {
		t.Fatal("unattested evidence was accepted while attestation was required")
	}
}

// A node that edits its own evidence after signing it must be caught. This is
// the compromised-node case: the runtime reported one thing and the node reports
// another.
func TestAcceptanceRefusesEditedAttestation(t *testing.T) {
	dispatcher, executor, controlKey := attestingRig(t)
	action := acceptanceEnvelope("base").Action

	result, err := dispatcher.Dispatch(context.Background(),
		signedFor(t, controlKey, "control-1", acceptanceEnvelope("base")))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var evidence control.Evidence
	if err := json.Unmarshal(result.Attested.EvidenceBytes, &evidence); err != nil {
		t.Fatal(err)
	}
	// Claim far more capacity was consumed than the server authorized.
	evidence.Observed["memory_mb"] = "65536"
	edited, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	result.Attested.EvidenceBytes = edited

	if _, err := executor.trustedEvidence(result, action); err == nil {
		t.Fatal("evidence edited after attestation was accepted")
	}
}

// Evidence attested by a different enrolled node must be refused even though the
// signature verifies, or one node could answer for work another performed.
func TestAcceptanceRefusesAttestationFromAnotherNode(t *testing.T) {
	dispatcher, executor, controlKey := attestingRig(t)
	action := acceptanceEnvelope("base").Action

	otherPublic, otherPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// The impostor is enrolled, so this is not an unknown-key rejection.
	executor.NodeKeys["edge-9"] = otherPublic

	result, err := dispatcher.Dispatch(context.Background(),
		signedFor(t, controlKey, "control-1", acceptanceEnvelope("base")))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var evidence control.Evidence
	if err := json.Unmarshal(result.Attested.EvidenceBytes, &evidence); err != nil {
		t.Fatal(err)
	}
	evidence.Observed["node"] = "edge-9"
	attested, err := control.SignEvidence(evidence, "edge-9", otherPrivate)
	if err != nil {
		t.Fatal(err)
	}
	result.Attested = &attested

	_, err = executor.trustedEvidence(result, action)
	if err == nil {
		t.Fatal("evidence attested by a different node was accepted")
	}
	if !strings.Contains(err.Error(), "edge-9") {
		t.Fatalf("denial did not name the attesting node: %v", err)
	}
}

// A captured attestation must stop being useful. Replay protection on the
// envelope does not cover evidence, which travels in the other direction.
func TestAcceptanceRefusesReplayedAttestation(t *testing.T) {
	dispatcher, executor, controlKey := attestingRig(t)
	action := acceptanceEnvelope("base").Action

	result, err := dispatcher.Dispatch(context.Background(),
		signedFor(t, controlKey, "control-1", acceptanceEnvelope("base")))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// The same attestation, presented well after it was produced.
	executor.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := executor.trustedEvidence(result, action); err == nil {
		t.Fatal("a replayed attestation was accepted")
	}
}
