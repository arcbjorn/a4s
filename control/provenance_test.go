package control

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const otherImage = "registry.example/other@sha256:" +
	"4444444444444444444444444444444444444444444444444444444444444444"

func signerKey(t *testing.T) (map[string]ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]ed25519.PublicKey{"builder-1": public}, private
}

func attestationFor(t *testing.T, key ed25519.PrivateKey, image string,
	expires time.Time) SignedAttestation {

	t.Helper()
	statement := ImageStatement{
		Image: image, Builder: "ci", BuiltAt: time.Unix(1700000000, 0).UTC(),
		ExpiresAt: expires,
	}
	signed, err := SignImageAttestation(statement, "builder-1", key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestAttestationVerifies(t *testing.T) {
	signers, key := signerKey(t)
	signed := attestationFor(t, key, spreadImage, time.Time{})

	statement, err := VerifyImageAttestation(signed, spreadImage, signers, time.Now())
	if err != nil {
		t.Fatalf("a valid attestation was refused: %v", err)
	}
	if statement.Builder != "ci" {
		t.Fatalf("builder = %q, want ci", statement.Builder)
	}
}

// A signature covers one image. Without that binding an attestation for a
// harmless build would authorize running anything else its holder had.
func TestAttestationDoesNotCoverAnotherImage(t *testing.T) {
	signers, key := signerKey(t)
	signed := attestationFor(t, key, spreadImage, time.Time{})

	_, err := VerifyImageAttestation(signed, otherImage, signers, time.Now())
	if err == nil || !strings.Contains(err.Error(), "covers") {
		t.Fatalf("expected an image mismatch denial, got %v", err)
	}
}

func TestAttestationRefusesUnknownSignerAndTampering(t *testing.T) {
	signers, key := signerKey(t)
	signed := attestationFor(t, key, spreadImage, time.Time{})

	// A signer the cluster was never told to trust.
	_, stranger, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	foreign := attestationFor(t, stranger, spreadImage, time.Time{})
	if _, err := VerifyImageAttestation(foreign, spreadImage, signers, time.Now()); err == nil {
		t.Fatal("an untrusted signer was accepted")
	}

	// The statement swapped under a valid signature.
	tampered := signed
	tampered.StatementBytes = []byte(strings.Replace(
		string(signed.StatementBytes), "ci", "xx", 1))
	if _, err := VerifyImageAttestation(tampered, spreadImage, signers, time.Now()); err == nil {
		t.Fatal("a tampered statement was accepted")
	}

	// No signers configured at all.
	if _, err := VerifyImageAttestation(signed, spreadImage, nil, time.Now()); err == nil {
		t.Fatal("verification succeeded with no trusted signers")
	}
}

func TestAttestationExpires(t *testing.T) {
	signers, key := signerKey(t)
	builtAt := time.Unix(1700000000, 0).UTC()
	signed := attestationFor(t, key, spreadImage, builtAt.Add(time.Hour))

	if _, err := VerifyImageAttestation(signed, spreadImage, signers, builtAt); err != nil {
		t.Fatalf("a live attestation was refused: %v", err)
	}
	_, err := VerifyImageAttestation(signed, spreadImage, signers, builtAt.Add(2*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected an expiry denial, got %v", err)
	}
}

// The kernel refuses to run an unattested image when policy requires one. This
// is the denial that replaces the human who used to review the deploy.
func TestKernelRequiresProvenanceWhenConfigured(t *testing.T) {
	signers, key := signerKey(t)
	world := spreadWorld(map[string]string{"node-a": "rack-1"})
	goal := spreadGoal(1, 0)
	proposal := Proposal{ID: "p", AgentID: "placement-agent", Actions: []Action{
		{ID: "pull", Kind: ActionPullImage, Target: spreadImage},
	}}

	strict := Kernel{Policy: Policy{RequireSignedImages: true, ImageSigners: signers}}
	err := strict.checkImageProvenance(goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "no provenance attestation") {
		t.Fatalf("expected an unattested denial, got %v", err)
	}

	goal.Workload.Attestation = ptrAttestation(attestationFor(t, key, spreadImage, time.Time{}))
	if err := strict.checkImageProvenance(goal, world, proposal); err != nil {
		t.Fatalf("an attested image was refused: %v", err)
	}
}

// An attestation that is present is verified whether or not policy demands one,
// so attaching a forged statement is never better than attaching none.
func TestBogusAttestationIsRefusedEvenWhenOptional(t *testing.T) {
	signers, _ := signerKey(t)
	_, stranger, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	world := spreadWorld(map[string]string{"node-a": "rack-1"})
	goal := spreadGoal(1, 0)
	goal.Workload.Attestation = ptrAttestation(attestationFor(t, stranger, spreadImage, time.Time{}))
	proposal := Proposal{ID: "p", AgentID: "placement-agent", Actions: []Action{
		{ID: "pull", Kind: ActionPullImage, Target: spreadImage},
	}}

	relaxed := Kernel{Policy: Policy{RequireSignedImages: false, ImageSigners: signers}}
	err = relaxed.checkImageProvenance(goal, world, proposal)
	if err == nil || !strings.Contains(err.Error(), "image provenance") {
		t.Fatalf("a forged attestation passed an optional policy: %v", err)
	}
}

// A goal written before provenance existed still reconciles, which is what makes
// the default a compatibility choice rather than a silent hole.
func TestUnattestedImageRunsWhenPolicyDoesNotRequireIt(t *testing.T) {
	world := spreadWorld(map[string]string{"node-a": "rack-1"})
	proposal := Proposal{ID: "p", AgentID: "placement-agent", Actions: []Action{
		{ID: "pull", Kind: ActionPullImage, Target: spreadImage},
	}}
	if err := (Kernel{Policy: DefaultPolicy()}).checkImageProvenance(
		spreadGoal(1, 0), world, proposal); err != nil {
		t.Fatalf("an unattested image was refused by the default policy: %v", err)
	}
}

// A proposal that runs nothing needs no provenance, so publishing a route is not
// gated on an image statement.
func TestProvenanceIgnoresProposalsThatRunNothing(t *testing.T) {
	signers, _ := signerKey(t)
	world := spreadWorld(map[string]string{"node-a": "rack-1"})
	strict := Kernel{Policy: Policy{RequireSignedImages: true, ImageSigners: signers}}
	routeOnly := Proposal{ID: "p", AgentID: "network-agent", Actions: []Action{
		{ID: "publish", Kind: ActionPublishRoute, Target: "web.example.com"},
	}}
	if err := strict.checkImageProvenance(spreadGoal(1, 0), world, routeOnly); err != nil {
		t.Fatalf("a route publication was gated on image provenance: %v", err)
	}
}

func TestSignImageAttestationRefusesBadInput(t *testing.T) {
	_, key := signerKey(t)
	if _, err := SignImageAttestation(ImageStatement{
		Image: "registry.example/web:latest", Builder: "ci", BuiltAt: time.Now(),
	}, "builder-1", key); err == nil {
		t.Fatal("an unpinned image was attested")
	}
	if _, err := SignImageAttestation(ImageStatement{
		Image: spreadImage, BuiltAt: time.Now(),
	}, "builder-1", key); err == nil {
		t.Fatal("an attestation with no builder was accepted")
	}
	if _, err := SignImageAttestation(ImageStatement{
		Image: spreadImage, Builder: "ci", BuiltAt: time.Now(),
	}, "", key); err == nil {
		t.Fatal("an attestation with no key id was accepted")
	}
}

// An attestation has to survive being written to a file and read back. The
// statement travels as a raw message so the verifier sees exactly what was
// signed, which means any re-encoding of the enclosing document that reformats
// it invalidates every attestation ever produced.
func TestAttestationSurvivesEncodingRoundTrip(t *testing.T) {
	signers, key := signerKey(t)
	signed := attestationFor(t, key, spreadImage, time.Time{})

	encoded, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SignedAttestation
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyImageAttestation(decoded, spreadImage, signers, time.Now()); err != nil {
		t.Fatalf("an attestation did not survive a round trip: %v", err)
	}

	// Indenting rewrites the signed bytes. This is the mistake the writer must
	// not make, pinned here so nobody reintroduces it for prettier output.
	indented, err := json.MarshalIndent(signed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var reindented SignedAttestation
	if err := json.Unmarshal(indented, &reindented); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyImageAttestation(reindented, spreadImage, signers, time.Now()); err == nil {
		t.Fatal("a reformatted statement still verified; the binding is not to exact bytes")
	}
}

func ptrAttestation(signed SignedAttestation) *SignedAttestation { return &signed }
