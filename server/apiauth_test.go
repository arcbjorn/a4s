package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func signingPair(t *testing.T) (map[string]ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]ed25519.PublicKey{"operator-arc": public}, private
}

func validEnvelope(now time.Time) RequestEnvelope {
	return RequestEnvelope{
		Nonce: "n1", Method: "GET", Path: "/v1/status", IssuedBy: "arc",
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}
}

func TestVerifyRequestAcceptsAGenuineRequest(t *testing.T) {
	keys, private := signingPair(t)
	now := time.Now().UTC()
	signed, err := SignRequest(validEnvelope(now), "operator-arc", private)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := VerifyRequest(signed, keys, "GET", "/v1/status", nil, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if envelope.IssuedBy != "arc" {
		t.Fatalf("issued_by = %q", envelope.IssuedBy)
	}
}

// Editing the envelope after signing must invalidate it, which is the whole
// reason the signed bytes travel rather than being recomputed from the struct.
func TestVerifyRequestRefusesEditedEnvelope(t *testing.T) {
	keys, private := signingPair(t)
	now := time.Now().UTC()
	signed, err := SignRequest(validEnvelope(now), "operator-arc", private)
	if err != nil {
		t.Fatal(err)
	}

	var envelope RequestEnvelope
	if err := json.Unmarshal(signed.EnvelopeBytes, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Path = "/v1/goals"
	edited, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	signed.EnvelopeBytes = edited

	if _, err := VerifyRequest(signed, keys, "GET", "/v1/goals", nil, now); err == nil {
		t.Fatal("expected an edited envelope to fail verification")
	}
}

func TestVerifyRequestRefusesUnknownFields(t *testing.T) {
	keys, private := signingPair(t)
	now := time.Now().UTC()

	// Sign bytes that carry a field this build does not define. The signature is
	// valid, so only strict decoding can refuse it.
	payload := []byte(`{"version":1,"nonce":"n1","method":"GET","path":"/v1/status",` +
		`"issued_by":"arc","key_id":"operator-arc","issued_at":"` +
		now.Format(time.RFC3339Nano) + `","expires_at":"` +
		now.Add(time.Minute).Format(time.RFC3339Nano) + `","escalate":true}`)
	signed := SignedRequest{
		EnvelopeBytes: payload, KeyID: "operator-arc",
		Signature: signBase64(private, payload),
	}
	if _, err := VerifyRequest(signed, keys, "GET", "/v1/status", nil, now); err == nil {
		t.Fatal("expected an envelope with unknown fields to be refused")
	}
}

func TestVerifyRequestRefusesOverlongLifetime(t *testing.T) {
	_, private := signingPair(t)
	now := time.Now().UTC()
	envelope := validEnvelope(now)
	envelope.ExpiresAt = now.Add(MaxRequestLifetime + time.Minute)

	if _, err := SignRequest(envelope, "operator-arc", private); err == nil {
		t.Fatal("expected an overlong request lifetime to be refused at signing")
	}
}

func TestVerifyRequestRefusesFutureIssueTime(t *testing.T) {
	keys, private := signingPair(t)
	now := time.Now().UTC()
	future := now.Add(10 * time.Minute)
	signed, err := SignRequest(validEnvelope(future), "operator-arc", private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRequest(signed, keys, "GET", "/v1/status", nil, now); err == nil {
		t.Fatal("expected a request issued in the future to be refused")
	}
}

// Small clock differences between an operator workstation and the server are
// normal and must not break signing.
func TestVerifyRequestToleratesSmallClockSkew(t *testing.T) {
	keys, private := signingPair(t)
	now := time.Now().UTC()
	slightlyAhead := now.Add(10 * time.Second)
	signed, err := SignRequest(validEnvelope(slightlyAhead), "operator-arc", private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRequest(signed, keys, "GET", "/v1/status", nil, now); err != nil {
		t.Fatalf("small skew refused: %v", err)
	}
}

func TestVerifyRequestRefusesWhenNoKeysConfigured(t *testing.T) {
	_, private := signingPair(t)
	now := time.Now().UTC()
	signed, err := SignRequest(validEnvelope(now), "operator-arc", private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRequest(signed, nil, "GET", "/v1/status", nil, now); err == nil {
		t.Fatal("expected verification to fail with no operator keys configured")
	}
}

func TestNonceLedgerExpiresEntries(t *testing.T) {
	ledger := newNonceLedger()
	now := time.Now()
	if !ledger.observe("k", "n1", now.Add(time.Minute), now) {
		t.Fatal("first use rejected")
	}
	if ledger.observe("k", "n1", now.Add(time.Minute), now) {
		t.Fatal("replay accepted")
	}

	// Once the request could no longer be accepted on expiry grounds, the entry
	// is dropped so the ledger stays a bounded cost.
	later := now.Add(2 * time.Minute)
	if !ledger.observe("k", "n2", later.Add(time.Minute), later) {
		t.Fatal("unrelated nonce rejected")
	}
	if len(ledger.seen) != 1 {
		t.Fatalf("ledger retained %d entries, want 1 after expiry", len(ledger.seen))
	}
}

// Two operators choosing the same nonce must not lock each other out.
func TestNonceLedgerScopesByKey(t *testing.T) {
	ledger := newNonceLedger()
	now := time.Now()
	if !ledger.observe("operator-a", "shared", now.Add(time.Minute), now) {
		t.Fatal("first operator rejected")
	}
	if !ledger.observe("operator-b", "shared", now.Add(time.Minute), now) {
		t.Fatal("second operator was blocked by the first operator's nonce")
	}
}

func signBase64(key ed25519.PrivateKey, payload []byte) string {
	return base64Std(ed25519.Sign(key, payload))
}

func base64Std(raw []byte) string {
	return base64.StdEncoding.EncodeToString(raw)
}
