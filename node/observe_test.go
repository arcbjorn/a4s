package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// scriptedObserver answers with whatever the test currently wants, so a second
// probe can legitimately differ from the first.
type scriptedObserver struct {
	ready bool
	calls int
	err   error
}

func (o *scriptedObserver) ObserveReadiness(target control.ProbeTarget) (bool, map[string]string, error) {
	o.calls++
	if o.err != nil {
		return false, nil, o.err
	}
	return o.ready, map[string]string{"observer": "scripted", "kind": target.Kind}, nil
}

// probeEnvelope builds a signed readiness probe for one allocation.
func probeEnvelope(now time.Time, key string) ActionEnvelope {
	return ActionEnvelope{
		Version: EnvelopeVersion, ID: "probe-1", NodeID: "base",
		GoalID: "readiness", ProposalID: "probe", WorldRevision: 0,
		LeaseID: "probe", IdempotencyKey: key,
		IssuedAt: now, ExpiresAt: now.Add(30 * time.Second),
		Action: control.Action{
			ID: "probe", Kind: control.ActionProbeReadiness, Target: "web-0",
			Node: "base",
			Probe: &control.ProbeTarget{
				Allocation: "web-0", Kind: control.ProbeProcess, Node: "base",
			},
		},
	}
}

func probeRig(t *testing.T, observer control.ReadinessObserver) (
	Dispatcher, ed25519.PrivateKey, ed25519.PublicKey) {

	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nodePublic, nodePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Dispatcher{
		NodeID: "base", Keys: map[string]ed25519.PublicKey{"server-1": publicKey},
		Runtime: &fakeRuntime{}, Ledger: NewMemoryLedger(),
		Now:         func() time.Time { return time.Unix(1000, 0).UTC() },
		Observer:    observer,
		IdentityKey: nodePrivate,
	}, privateKey, nodePublic
}

// A measurement must never be served from the idempotency ledger. That ledger
// exists to return the stored result of work already done, and a remembered
// readiness is exactly what an expiring observation must not be.
func TestProbeIsNeverServedFromTheLedger(t *testing.T) {
	observer := &scriptedObserver{ready: true}
	dispatcher, signingKey, _ := probeRig(t, observer)
	now := time.Unix(1000, 0).UTC()

	// The same idempotency key twice, which for a mutation would return the
	// stored result without touching the runtime.
	signed, err := Sign(probeEnvelope(now, "same-key"), "server-1", signingKey)
	if err != nil {
		t.Fatal(err)
	}
	first, err := dispatcher.Dispatch(context.Background(), signed)
	if err != nil {
		t.Fatal(err)
	}
	if first.Evidence.Observed["ready"] != "true" {
		t.Fatalf("first probe reported %q", first.Evidence.Observed["ready"])
	}

	// The workload stops serving between measurements.
	observer.ready = false
	second, err := dispatcher.Dispatch(context.Background(), signed)
	if err != nil {
		t.Fatal(err)
	}
	if second.Evidence.Observed["ready"] != "false" {
		t.Fatal("the second probe returned a cached measurement")
	}
	if observer.calls != 2 {
		t.Fatalf("the observer was consulted %d times, want 2", observer.calls)
	}
}

// Readiness decides whether a goal is satisfied and whether a route carries
// traffic, so a node that could report it unsigned would be the cheapest way to
// lie about the state of the cluster.
func TestProbeEvidenceIsAttestedAndAttributed(t *testing.T) {
	dispatcher, signingKey, nodePublic := probeRig(t, &scriptedObserver{ready: true})
	now := time.Unix(1000, 0).UTC()
	signed, err := Sign(probeEnvelope(now, "k1"), "server-1", signingKey)
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.Dispatch(context.Background(), signed)
	if err != nil {
		t.Fatal(err)
	}
	if result.Attested == nil {
		t.Fatal("probe evidence carried no attestation")
	}
	verified, err := control.VerifyEvidence(*result.Attested,
		map[string]ed25519.PublicKey{"base": nodePublic}, now, time.Minute)
	if err != nil {
		t.Fatalf("probe attestation did not verify: %v", err)
	}
	if verified.Kind != control.EvidenceAllocationReady || verified.Target != "web-0" {
		t.Fatalf("unexpected probe evidence %+v", verified)
	}
	if verified.Source == "" {
		t.Fatal("probe evidence was not attributed to the node")
	}
	// The measurement has to carry its own expiry, or the control plane cannot
	// tell a fresh observation from one it has been holding for an hour.
	if !verified.ExpiresAt.After(now) {
		t.Fatalf("probe evidence expiry %s is not in the future", verified.ExpiresAt)
	}
}

// An agent is ready on different terms, and the evidence kind has to say so or
// the projection records a port check as a provider check.
func TestProbeReportsAgentReadinessAsItsOwnKind(t *testing.T) {
	dispatcher, signingKey, _ := probeRig(t, &scriptedObserver{ready: true})
	now := time.Unix(1000, 0).UTC()
	envelope := probeEnvelope(now, "k1")
	envelope.Action.Probe.Kind = control.ProbeAgent
	signed, err := Sign(envelope, "server-1", signingKey)
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.Dispatch(context.Background(), signed)
	if err != nil {
		t.Fatal(err)
	}
	if result.Evidence.Kind != control.EvidenceAgentReady {
		t.Fatalf("agent probe reported %q", result.Evidence.Kind)
	}
}

// A broken measurement and a working measurement of a broken workload are
// different facts, and collapsing them would make a misconfigured node look
// like an outage.
func TestProbeFailureIsNotAReadinessAnswer(t *testing.T) {
	dispatcher, signingKey, _ := probeRig(t, &scriptedObserver{err: errProbe})
	now := time.Unix(1000, 0).UTC()
	signed, err := Sign(probeEnvelope(now, "k1"), "server-1", signingKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.Dispatch(context.Background(), signed); err == nil {
		t.Fatal("a failed measurement was reported as evidence")
	}

	// A node with no observer refuses rather than answering not-ready, which
	// would report every workload as down because of a wiring mistake.
	blind, signingKey, _ := probeRig(t, nil)
	signed, err = Sign(probeEnvelope(now, "k2"), "server-1", signingKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = blind.Dispatch(context.Background(), signed)
	if err == nil || !strings.Contains(err.Error(), "readiness observer") {
		t.Fatalf("expected a missing-observer refusal, got %v", err)
	}
}

// A probe with no target names nothing to measure and must be refused.
func TestProbeRequiresATarget(t *testing.T) {
	dispatcher, signingKey, _ := probeRig(t, &scriptedObserver{ready: true})
	now := time.Unix(1000, 0).UTC()
	envelope := probeEnvelope(now, "k1")
	envelope.Action.Probe = nil
	signed, err := Sign(envelope, "server-1", signingKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.Dispatch(context.Background(), signed); err == nil {
		t.Fatal("a probe with no target was accepted")
	}
}

// Measuring is not mutating: a probe must not be able to reach the runtime.
func TestProbeDoesNotTouchTheRuntime(t *testing.T) {
	dispatcher, signingKey, _ := probeRig(t, &scriptedObserver{ready: true})
	runtime := dispatcher.Runtime.(*fakeRuntime)
	now := time.Unix(1000, 0).UTC()
	signed, err := Sign(probeEnvelope(now, "k1"), "server-1", signingKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.Dispatch(context.Background(), signed); err != nil {
		t.Fatal(err)
	}
	if runtime.calls != 0 {
		t.Fatalf("a probe reached the runtime %d times", runtime.calls)
	}
}

// The claim the acceptance rig makes but does not test: readiness measured on
// the node, over the wire, by a control plane in a different process.
//
// Every other readiness test in this package hands the observer straight to the
// prober in one process, which exercises everything except the part that is
// different in production and is why a real deployment measured nothing at all.
func TestReadinessTravelsOverTheTransport(t *testing.T) {
	observer := &scriptedObserver{ready: true}
	dispatcher, signingKey, nodePublic := probeRig(t, observer)
	dispatcher.Now = time.Now

	toNode, fromServer := io.Pipe()
	toServer, fromNode := io.Pipe()
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = Serve(context.Background(), &dispatcher, toNode, fromNode)
	}()
	defer func() {
		fromServer.Close()
		toServer.Close()
		<-served
	}()

	remote := NewRemoteExecutor("base", "server-1", signingKey,
		NewStreamTransport(fromServer, toServer, nil))
	remote.NodeKeys = map[string]ed25519.PublicKey{"base": nodePublic}
	// Trusted on exactly the terms a mutation's result is: the node signs what
	// it measured, and the control plane checks that signature.
	remote.RequireAttestation = true
	remote.Bind("readiness", "probe", 0, "probe")

	target := control.ProbeTarget{
		Allocation: "web-0", Kind: control.ProbeProcess, Node: "base",
	}
	evidence, err := remote.Execute(control.Action{
		ID: "probe-web-0", Kind: control.ActionProbeReadiness,
		Target: "web-0", Node: "base", Probe: &target,
	})
	if err != nil {
		t.Fatalf("readiness did not survive the transport: %v", err)
	}
	if evidence.Observed["ready"] != "true" {
		t.Fatalf("remote probe reported %q", evidence.Observed["ready"])
	}
	if evidence.Source == "" {
		t.Fatal("remote probe evidence was not attributed to the node")
	}
	if observer.calls != 1 {
		t.Fatalf("the node's observer was consulted %d times, want 1", observer.calls)
	}

	// A second measurement crosses the wire again rather than being answered
	// from anything the node or the executor remembered.
	observer.ready = false
	evidence, err = remote.Execute(control.Action{
		ID: "probe-web-0", Kind: control.ActionProbeReadiness,
		Target: "web-0", Node: "base", Probe: &target,
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Observed["ready"] != "false" {
		t.Fatal("a repeated remote probe returned a stale measurement")
	}
}

var errProbe = probeError("container is gone")

type probeError string

func (e probeError) Error() string { return string(e) }
