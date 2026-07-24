package control

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public
}

func TestNewKeySetHoldsOneActiveKey(t *testing.T) {
	set, err := NewKeySet("control-1", newKey(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := set.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	active, err := set.Active()
	if err != nil {
		t.Fatal(err)
	}
	if active.KeyID != "control-1" {
		t.Fatalf("active = %q", active.KeyID)
	}
}

// This is the property rotation exists for: after rotating, the old key still
// verifies, so envelopes in flight and nodes that have not yet received the new
// keyset keep working.
func TestRotationKeepsThePreviousKeyVerifying(t *testing.T) {
	now := time.Now()
	set, err := NewKeySet("control-1", newKey(t), now)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := set.Rotate("control-2", newKey(t), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	active, err := rotated.Active()
	if err != nil {
		t.Fatal(err)
	}
	if active.KeyID != "control-2" {
		t.Fatalf("active after rotation = %q, want control-2", active.KeyID)
	}

	trusted, err := rotated.TrustMap()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := trusted["control-1"]; !ok {
		t.Fatal("the previous key stopped verifying immediately after rotation")
	}
	if _, ok := trusted["control-2"]; !ok {
		t.Fatal("the new key does not verify")
	}
}

// Retiring is what actually ends trust, and it must be a separate deliberate
// step from rotation.
func TestRetiringRemovesAKeyFromTheTrustMap(t *testing.T) {
	now := time.Now()
	set, _ := NewKeySet("control-1", newKey(t), now)
	rotated, err := set.Rotate("control-2", newKey(t), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	retired, err := rotated.Retire("control-1", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("retire: %v", err)
	}

	trusted, err := retired.TrustMap()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := trusted["control-1"]; ok {
		t.Fatal("a retired key still verifies")
	}
	if len(trusted) != 1 {
		t.Fatalf("trust map holds %d keys, want 1", len(trusted))
	}
}

// Retiring the only signing key would leave the control plane unable to issue
// any capability, turning key hygiene into an outage.
func TestRetiringTheActiveKeyIsRefused(t *testing.T) {
	set, _ := NewKeySet("control-1", newKey(t), time.Now())
	_, err := set.Retire("control-1", time.Now())
	if err == nil {
		t.Fatal("expected retiring the active key to be refused")
	}
	if !strings.Contains(err.Error(), "rotate") {
		t.Fatalf("error should point at rotation: %v", err)
	}
}

// Reusing a key id would silently change what a historical signature means.
func TestRotatingToAnExistingKeyIDIsRefused(t *testing.T) {
	set, _ := NewKeySet("control-1", newKey(t), time.Now())
	if _, err := set.Rotate("control-1", newKey(t), time.Now()); err == nil {
		t.Fatal("expected a duplicate key id to be refused")
	}
}

func TestRetiringAnUnknownKeyIsRefused(t *testing.T) {
	set, _ := NewKeySet("control-1", newKey(t), time.Now())
	if _, err := set.Retire("control-9", time.Now()); err == nil {
		t.Fatal("expected retiring an unknown key to be refused")
	}
}

func TestValidateRefusesTwoActiveKeys(t *testing.T) {
	now := time.Now()
	set, _ := NewKeySet("control-1", newKey(t), now)
	rotated, err := set.Rotate("control-2", newKey(t), now)
	if err != nil {
		t.Fatal(err)
	}
	// Force the demoted key back to active, which no API allows but a
	// hand-edited file could produce.
	for index := range rotated.Keys {
		rotated.Keys[index].State = KeyActive
	}
	if err := rotated.Validate(); err == nil {
		t.Fatal("expected two active keys to be refused")
	}
}

func TestValidateRefusesUnknownState(t *testing.T) {
	set, _ := NewKeySet("control-1", newKey(t), time.Now())
	set.Keys[0].State = KeyState("provisional")
	if err := set.Validate(); err == nil {
		t.Fatal("expected an unknown key state to be refused")
	}
}

func TestValidateRefusesMalformedKey(t *testing.T) {
	set, _ := NewKeySet("control-1", newKey(t), time.Now())
	set.Keys[0].PublicKey = "not-base64!"
	if err := set.Validate(); err == nil {
		t.Fatal("expected a malformed public key to be refused")
	}
}

// A keyset is distributed to nodes, so a document carrying a field this build
// does not understand may have been written against different semantics.
func TestDecodeKeySetRefusesUnknownFields(t *testing.T) {
	set, _ := NewKeySet("control-1", newKey(t), time.Now())
	encoded, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(encoded),
		`"version":1`, `"version":1,"emergency_bypass":true`, 1)
	if _, err := DecodeKeySet([]byte(tampered)); err == nil {
		t.Fatal("expected an unknown keyset field to be refused")
	}
}

func TestDecodeKeySetRoundTrips(t *testing.T) {
	now := time.Now()
	set, _ := NewKeySet("control-1", newKey(t), now)
	rotated, err := set.Rotate("control-2", newKey(t), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(rotated)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeKeySet(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Keys) != 2 {
		t.Fatalf("decoded %d keys, want 2", len(decoded.Keys))
	}
	active, err := decoded.Active()
	if err != nil || active.KeyID != "control-2" {
		t.Fatalf("active after round trip = %q (%v)", active.KeyID, err)
	}
}

// A private key must never reach a keyset, which is distributed to every node.
func TestKeySetCarriesOnlyPublicMaterial(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	set, err := NewKeySet("control-1", public, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	if bytesContain(encoded, private.Seed()) {
		t.Fatal("keyset serialization contains private key material")
	}
}

func TestSortedOrdersByLifecycle(t *testing.T) {
	now := time.Now()
	set, _ := NewKeySet("control-1", newKey(t), now)
	rotated, _ := set.Rotate("control-2", newKey(t), now.Add(time.Hour))
	third, err := rotated.Rotate("control-3", newKey(t), now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	retired, err := third.Retire("control-1", now.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	sorted := retired.Sorted()
	if sorted[0].State != KeyActive {
		t.Fatalf("first key state = %q, want active", sorted[0].State)
	}
	if sorted[len(sorted)-1].State != KeyRetired {
		t.Fatalf("last key state = %q, want retired", sorted[len(sorted)-1].State)
	}
}

func bytesContain(haystack, needle []byte) bool {
	return len(needle) > 0 && strings.Contains(string(haystack), string(needle))
}
