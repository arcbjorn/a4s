package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

type fakeRuntime struct {
	calls int
}

func (r *fakeRuntime) Execute(_ context.Context, action control.Action) (control.Evidence, error) {
	r.calls++
	return control.Evidence{Kind: "test", Target: action.Target, Observed: map[string]string{"ok": "true"}}, nil
}

func (*fakeRuntime) Close() error { return nil }

func TestDispatcherVerifiesAndDeduplicatesSignedAction(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0).UTC()
	envelope := validEnvelope(now)
	signed, err := Sign(envelope, "server-1", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}
	dispatcher := Dispatcher{
		NodeID: "base", Keys: map[string]ed25519.PublicKey{"server-1": publicKey},
		Runtime: runtime, Ledger: NewMemoryLedger(), Now: func() time.Time { return now },
	}
	first, err := dispatcher.Dispatch(context.Background(), signed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := dispatcher.Dispatch(context.Background(), signed)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.calls != 1 || first.EnvelopeDigest != second.EnvelopeDigest {
		t.Fatalf("action was not deduplicated: calls=%d first=%+v second=%+v", runtime.calls, first, second)
	}
}

func TestDispatcherRejectsTamperedEnvelope(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0).UTC()
	signed, err := Sign(validEnvelope(now), "server-1", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	// Tamper with the transmitted bytes, which is what an attacker controls.
	envelope := signed.Envelope()
	envelope.Action.Target = "different"
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	signed.EnvelopeBytes = tampered
	dispatcher := Dispatcher{
		NodeID: "base", Keys: map[string]ed25519.PublicKey{"server-1": publicKey},
		Runtime: &fakeRuntime{}, Ledger: NewMemoryLedger(), Now: func() time.Time { return now },
	}
	if _, err := dispatcher.Dispatch(context.Background(), signed); err == nil || !strings.Contains(err.Error(), "invalid action signature") {
		t.Fatalf("expected signature error, got %v", err)
	}
}

// A verifier that re-marshals a decoded struct authenticates the re-encoded
// bytes rather than the bytes that were actually signed. Any payload that
// decodes to the same struct then passes, regardless of what was transmitted.
// Verification must therefore operate on the received bytes.
func TestDispatcherRejectsReencodedEnvelopePayload(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0).UTC()
	signed, err := Sign(validEnvelope(now), "server-1", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	// Semantically identical to the signed envelope, but not the signed bytes.
	// A re-marshaling verifier would accept this; a byte-exact one must not.
	reencoded := append([]byte(" "), signed.EnvelopeBytes...)
	signed.EnvelopeBytes = reencoded
	dispatcher := Dispatcher{
		NodeID: "base", Keys: map[string]ed25519.PublicKey{"server-1": publicKey},
		Runtime: &fakeRuntime{}, Ledger: NewMemoryLedger(), Now: func() time.Time { return now },
	}
	if _, err := dispatcher.Dispatch(context.Background(), signed); err == nil || !strings.Contains(err.Error(), "invalid action signature") {
		t.Fatalf("expected signature error over exact bytes, got %v", err)
	}
}

// Unknown fields in the signed payload must be rejected rather than silently
// dropped, so a node never executes an envelope it did not fully understand.
func TestDispatcherRejectsUnknownEnvelopeFields(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0).UTC()
	payload, err := json.Marshal(validEnvelope(now))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	fields["unrecognized_privilege"] = true
	extended, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, extended)
	signed := SignedAction{
		EnvelopeBytes: extended, KeyID: "server-1",
		Signature: base64.RawStdEncoding.EncodeToString(signature),
	}
	dispatcher := Dispatcher{
		NodeID: "base", Keys: map[string]ed25519.PublicKey{"server-1": publicKey},
		Runtime: &fakeRuntime{}, Ledger: NewMemoryLedger(), Now: func() time.Time { return now },
	}
	if _, err := dispatcher.Dispatch(context.Background(), signed); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field rejection, got %v", err)
	}
}

func TestDispatcherRejectsExpiredEnvelope(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issued := time.Unix(1000, 0).UTC()
	signed, err := Sign(validEnvelope(issued), "server-1", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := Dispatcher{
		NodeID: "base", Keys: map[string]ed25519.PublicKey{"server-1": publicKey},
		Runtime: &fakeRuntime{}, Ledger: NewMemoryLedger(), Now: func() time.Time { return issued.Add(time.Minute) },
	}
	if _, err := dispatcher.Dispatch(context.Background(), signed); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expiry error, got %v", err)
	}
}

func TestDispatcherRejectsIdempotencyKeyReuse(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0).UTC()
	firstEnvelope := validEnvelope(now)
	first, err := Sign(firstEnvelope, "server-1", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}
	dispatcher := Dispatcher{
		NodeID: "base", Keys: map[string]ed25519.PublicKey{"server-1": publicKey},
		Runtime: runtime, Ledger: NewMemoryLedger(), Now: func() time.Time { return now },
	}
	if _, err := dispatcher.Dispatch(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	secondEnvelope := firstEnvelope
	secondEnvelope.ID = "action-2"
	secondEnvelope.Action.Target = "other"
	second, err := Sign(secondEnvelope, "server-1", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.Dispatch(context.Background(), second); err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("expected key reuse error, got %v", err)
	}
}

func validEnvelope(now time.Time) ActionEnvelope {
	return ActionEnvelope{
		Version: EnvelopeVersion, ID: "action-1", NodeID: "base",
		GoalID: "goal", ProposalID: "proposal", WorldRevision: 7,
		LeaseID: "lease", IdempotencyKey: "idempotent-1",
		IssuedAt: now, ExpiresAt: now.Add(30 * time.Second),
		Action: control.Action{
			ID: "pull", Kind: control.ActionPullImage, Target: "image",
			Node: "base", Image: "registry/image@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
}
