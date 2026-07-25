package control

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func nodeKeys(t *testing.T, ids ...string) (map[string]ed25519.PublicKey, map[string]ed25519.PrivateKey) {
	t.Helper()
	public := map[string]ed25519.PublicKey{}
	private := map[string]ed25519.PrivateKey{}
	for _, id := range ids {
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		public[id], private[id] = pub, priv
	}
	return public, private
}

func readyEvidence(node string, at time.Time) Evidence {
	return Evidence{
		Kind: EvidenceAllocationReady, Target: "web-0", Source: "prober:tcp",
		ObservedAt: at, ExpiresAt: at.Add(30 * time.Second),
		Observed: map[string]string{"node": node, "ready": "true"},
	}
}

func TestAttestedEvidenceRoundTrips(t *testing.T) {
	public, private := nodeKeys(t, "edge-1")
	now := time.Unix(1700000000, 0).UTC()

	attested, err := SignEvidence(readyEvidence("edge-1", now), "edge-1", private["edge-1"])
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := VerifyEvidence(attested, public, now, time.Minute)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if evidence.Kind != EvidenceAllocationReady || evidence.Target != "web-0" {
		t.Fatalf("evidence did not survive the round trip: %+v", evidence)
	}
	if evidence.Observed["ready"] != "true" {
		t.Fatalf("observed map lost content: %+v", evidence.Observed)
	}
}

// A node that is not enrolled has no path into the projection, however well
// formed its attestation is.
func TestUnenrolledNodeIsRefused(t *testing.T) {
	public, _ := nodeKeys(t, "edge-1")
	_, stranger := nodeKeys(t, "edge-9")
	now := time.Unix(1700000000, 0).UTC()

	attested, err := SignEvidence(readyEvidence("edge-9", now), "edge-9", stranger["edge-9"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyEvidence(attested, public, now, time.Minute); err == nil {
		t.Fatal("an unenrolled node's evidence was accepted")
	}
}

// The central impersonation case: an enrolled node signing evidence that claims
// another node made the observation.
func TestNodeCannotAttestForAnotherNode(t *testing.T) {
	public, private := nodeKeys(t, "edge-1", "edge-2")
	now := time.Unix(1700000000, 0).UTC()

	// edge-1 signs, but the evidence claims edge-2 observed it.
	attested, err := SignEvidence(readyEvidence("edge-2", now), "edge-1", private["edge-1"])
	if err != nil {
		t.Fatal(err)
	}
	_, err = VerifyEvidence(attested, public, now, time.Minute)
	if err == nil {
		t.Fatal("a node attested for another node's observation")
	}
	if !strings.Contains(err.Error(), "claims node") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Claiming another node's id outright fails on the signature, since the key that
// signed is looked up by the claimed id.
func TestForgedNodeIDFailsSignature(t *testing.T) {
	public, private := nodeKeys(t, "edge-1", "edge-2")
	now := time.Unix(1700000000, 0).UTC()

	attested, err := SignEvidence(readyEvidence("edge-2", now), "edge-2", private["edge-1"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyEvidence(attested, public, now, time.Minute); err == nil {
		t.Fatal("evidence signed by the wrong node key was accepted")
	}
}

func TestTamperedEvidenceIsRefused(t *testing.T) {
	public, private := nodeKeys(t, "edge-1")
	now := time.Unix(1700000000, 0).UTC()

	attested, err := SignEvidence(readyEvidence("edge-1", now), "edge-1", private["edge-1"])
	if err != nil {
		t.Fatal(err)
	}

	// Flip readiness after signing, which is the lie worth catching: it would
	// make a dead workload satisfy a goal.
	var evidence Evidence
	if err := json.Unmarshal(attested.EvidenceBytes, &evidence); err != nil {
		t.Fatal(err)
	}
	evidence.Observed["ready"] = "false"
	edited, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	attested.EvidenceBytes = edited

	if _, err := VerifyEvidence(attested, public, now, time.Minute); err == nil {
		t.Fatal("tampered evidence was accepted")
	}
}

func TestStaleAttestationIsRefused(t *testing.T) {
	public, private := nodeKeys(t, "edge-1")
	observed := time.Unix(1700000000, 0).UTC()

	attested, err := SignEvidence(readyEvidence("edge-1", observed), "edge-1", private["edge-1"])
	if err != nil {
		t.Fatal(err)
	}
	// Replayed an hour later.
	if _, err := VerifyEvidence(attested, public, observed.Add(time.Hour), time.Minute); err == nil {
		t.Fatal("a replayed attestation was accepted")
	}
	// The same attestation is fine inside the window.
	if _, err := VerifyEvidence(attested, public, observed.Add(10*time.Second), time.Minute); err != nil {
		t.Fatalf("fresh attestation refused: %v", err)
	}
	// maxAge zero skips the age check, for evidence that does not decay.
	if _, err := VerifyEvidence(attested, public, observed.Add(time.Hour), 0); err != nil {
		t.Fatalf("age check applied when disabled: %v", err)
	}
}

func TestFutureAttestationIsRefused(t *testing.T) {
	public, private := nodeKeys(t, "edge-1")
	now := time.Unix(1700000000, 0).UTC()

	attested, err := SignEvidence(readyEvidence("edge-1", now.Add(time.Hour)), "edge-1", private["edge-1"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyEvidence(attested, public, now, time.Minute); err == nil {
		t.Fatal("evidence observed in the future was accepted")
	}
}

func TestAttestationRefusesUnknownFields(t *testing.T) {
	public, private := nodeKeys(t, "edge-1")
	now := time.Unix(1700000000, 0).UTC()

	attested, err := SignEvidence(readyEvidence("edge-1", now), "edge-1", private["edge-1"])
	if err != nil {
		t.Fatal(err)
	}
	// A node signing a field this build does not understand may have been
	// operating under different semantics.
	edited := strings.Replace(string(attested.EvidenceBytes), `{"kind"`, `{"authority":"root","kind"`, 1)
	attested.EvidenceBytes = []byte(edited)
	attested.Signature = signBase64(private["edge-1"], attested.EvidenceBytes)

	if _, err := VerifyEvidence(attested, public, now, time.Minute); err == nil {
		t.Fatal("evidence with an unknown field was accepted")
	}
}

func TestSignEvidenceRejectsBadInput(t *testing.T) {
	_, private := nodeKeys(t, "edge-1")
	now := time.Unix(1700000000, 0).UTC()

	if _, err := SignEvidence(readyEvidence("edge-1", now), "", private["edge-1"]); err == nil {
		t.Fatal("signing without a node id was allowed")
	}
	if _, err := SignEvidence(readyEvidence("edge-1", now), "edge-1", nil); err == nil {
		t.Fatal("signing without a key was allowed")
	}
	if _, err := SignEvidence(Evidence{Target: "web-0"}, "edge-1", private["edge-1"]); err == nil {
		t.Fatal("signing evidence without a kind was allowed")
	}
}

func TestVerifyEvidenceRequiresConfiguredKeys(t *testing.T) {
	_, private := nodeKeys(t, "edge-1")
	now := time.Unix(1700000000, 0).UTC()
	attested, err := SignEvidence(readyEvidence("edge-1", now), "edge-1", private["edge-1"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyEvidence(attested, nil, now, time.Minute); err == nil {
		t.Fatal("verification succeeded with no enrolled keys")
	}
}
