package control

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func operatorKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

func validGrant(now time.Time) ApprovalGrant {
	return ApprovalGrant{
		ID: "web-public-route", GoalID: "homepage-public", Scope: "public-route",
		IssuedBy: "arc", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
		Revision: 4, Reason: "launch review passed",
	}
}

// Everything the kernel gates on is authorized by a grant that passed
// verification, so a grant that does not verify must not become an approval.
func TestApprovalRoundTripsThroughVerification(t *testing.T) {
	public, private := operatorKey(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	signed, err := SignApproval(validGrant(now), "operator-arc", private)
	if err != nil {
		t.Fatalf("signing failed: %v", err)
	}
	grant, err := VerifyApproval(signed,
		map[string]ed25519.PublicKey{"operator-arc": public}, now)
	if err != nil {
		t.Fatalf("verification failed: %v", err)
	}
	if grant.Scope != "public-route" || grant.IssuedBy != "arc" {
		t.Fatalf("grant did not survive the round trip: %+v", grant)
	}
	if grant.KeyID != "operator-arc" {
		t.Fatal("expected the signing key to be recorded on the grant")
	}
}

// An agent holds no operator key, so the only way it could authorize itself is
// by forging a signature.
func TestForgedApprovalIsRefused(t *testing.T) {
	public, _ := operatorKey(t)
	_, attacker := operatorKey(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	signed, err := SignApproval(validGrant(now), "operator-arc", attacker)
	if err != nil {
		t.Fatal(err)
	}
	// The attacker signs with its own key but claims the operator's key id.
	if _, err := VerifyApproval(signed,
		map[string]ed25519.PublicKey{"operator-arc": public}, now); err == nil {
		t.Fatal("a forged signature was accepted")
	}
}

// A grant edited after signing must not verify, or the signature protects
// nothing.
func TestTamperedApprovalIsRefused(t *testing.T) {
	public, private := operatorKey(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	signed, err := SignApproval(validGrant(now), "operator-arc", private)
	if err != nil {
		t.Fatal(err)
	}
	// Escalate the scope in the signed bytes.
	tampered := strings.Replace(string(signed.GrantBytes),
		`"scope":"public-route"`, `"scope":"destroy-stateful"`, 1)
	signed.GrantBytes = []byte(tampered)

	if _, err := VerifyApproval(signed,
		map[string]ed25519.PublicKey{"operator-arc": public}, now); err == nil {
		t.Fatal("a tampered grant was accepted")
	}
}

// A server that has not been told who its operators are must not authorize
// anything.
func TestApprovalNeedsConfiguredKeys(t *testing.T) {
	_, private := operatorKey(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	signed, err := SignApproval(validGrant(now), "operator-arc", private)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyApproval(signed, nil, now); err == nil {
		t.Fatal("an approval was accepted with no operator keys configured")
	}
	if _, err := VerifyApproval(signed,
		map[string]ed25519.PublicKey{"someone-else": make(ed25519.PublicKey, 32)}, now); err == nil {
		t.Fatal("an approval was accepted from an unknown key id")
	}
}

// A grant could otherwise be re-signed by one operator while still attributing
// itself to another.
func TestApprovalKeyIDMustMatchSigner(t *testing.T) {
	publicA, privateA := operatorKey(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	grant := validGrant(now)
	grant.KeyID = "operator-someone-else"
	payload, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	signed := SignedApproval{
		GrantBytes: payload, KeyID: "operator-arc",
		Signature: signBase64(privateA, payload),
	}
	_, err = VerifyApproval(signed,
		map[string]ed25519.PublicKey{"operator-arc": publicA}, now)
	if err == nil || !strings.Contains(err.Error(), "names key") {
		t.Fatalf("expected a key-id mismatch to be refused, got %v", err)
	}
}

// A free-form scope would let an importer invent an authorization the kernel
// never asked for.
func TestApprovalScopeMustBeGated(t *testing.T) {
	_, private := operatorKey(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	grant := validGrant(now)
	grant.Scope = "make-me-root"
	if _, err := SignApproval(grant, "operator-arc", private); err == nil {
		t.Fatal("an ungated scope was accepted")
	}
}

// An unbounded grant becomes standing permission nobody remembers issuing.
func TestApprovalLifetimeIsBounded(t *testing.T) {
	_, private := operatorKey(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	for _, bad := range []struct {
		name  string
		grant ApprovalGrant
	}{
		{"no expiry", func() ApprovalGrant {
			g := validGrant(now)
			g.ExpiresAt = time.Time{}
			return g
		}()},
		{"expires before issued", func() ApprovalGrant {
			g := validGrant(now)
			g.ExpiresAt = now.Add(-time.Minute)
			return g
		}()},
		{"beyond the maximum", func() ApprovalGrant {
			g := validGrant(now)
			g.ExpiresAt = now.Add(MaxApprovalLifetime + time.Hour)
			return g
		}()},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if _, err := SignApproval(bad.grant, "operator-arc", private); err == nil {
				t.Fatalf("expected %s to be refused", bad.name)
			}
		})
	}
}

// An expired grant is not an approval, however well signed it is.
func TestExpiredApprovalDoesNotVerify(t *testing.T) {
	public, private := operatorKey(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	signed, err := SignApproval(validGrant(now), "operator-arc", private)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Hour)
	if _, err := VerifyApproval(signed,
		map[string]ed25519.PublicKey{"operator-arc": public}, later); err == nil {
		t.Fatal("an expired grant verified")
	}
}

// The kernel must stop honoring a grant once it lapses, or expiry is cosmetic.
func TestExpiredApprovalStopsAuthorizing(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	world := World{
		ObservedAt: now,
		Approvals: map[string]*Approval{
			"a1": {
				ID: "a1", GoalID: "homepage-public", Scope: "public-route",
				IssuedBy: "arc", Granted: true, IssuedAt: now,
				ExpiresAt: now.Add(time.Hour),
			},
		},
	}
	if !hasApproval(world, "homepage-public", "public-route") {
		t.Fatal("a live grant should authorize")
	}

	world.ObservedAt = now.Add(2 * time.Hour)
	if hasApproval(world, "homepage-public", "public-route") {
		t.Fatal("an expired grant still authorized")
	}
}

// A revoked grant must stop authorizing while remaining visible, so a reviewer
// can tell withdrawn from never-issued.
func TestRevokedApprovalIsKeptButPowerless(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	world := World{ObservedAt: now, Approvals: map[string]*Approval{}}

	granted, err := Project(world, Evidence{
		Kind: EvidenceApprovalGranted, Target: "a1", ObservedAt: now,
		ExpiresAt: now.Add(time.Hour),
		Observed: map[string]string{
			"goal": "homepage-public", "scope": "public-route", "issued_by": "arc",
		},
	})
	if err != nil {
		t.Fatalf("granting failed: %v", err)
	}
	if !hasApproval(granted, "homepage-public", "public-route") {
		t.Fatal("a projected grant should authorize")
	}

	revoked, err := Project(granted, Evidence{
		Kind: EvidenceApprovalRevoked, Target: "a1", ObservedAt: now,
	})
	if err != nil {
		t.Fatalf("revoking failed: %v", err)
	}
	if hasApproval(revoked, "homepage-public", "public-route") {
		t.Fatal("a revoked grant still authorized")
	}
	if _, kept := revoked.Approvals["a1"]; !kept {
		t.Fatal("expected the revoked grant to remain visible for review")
	}
}

// A log is replayed on every start, so a scope the kernel does not gate on must
// not materialize as an approval that appears to authorize something.
func TestProjectionRefusesUngatedScope(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	world := World{ObservedAt: now, Approvals: map[string]*Approval{}}

	_, err := Project(world, Evidence{
		Kind: EvidenceApprovalGranted, Target: "a1", ObservedAt: now,
		Observed: map[string]string{
			"goal": "homepage-public", "scope": "make-me-root", "issued_by": "arc",
		},
	})
	if err == nil {
		t.Fatal("an ungated scope was projected into the world")
	}
}

// The signature must not reach the durable log, which is meant to be readable
// and must not carry a replayable authorization.
func TestApprovalEvidenceCarriesNoSignature(t *testing.T) {
	public, private := operatorKey(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	signed, err := SignApproval(validGrant(now), "operator-arc", private)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := VerifyApproval(signed,
		map[string]ed25519.PublicKey{"operator-arc": public}, now)
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(grant.Evidence())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), signed.Signature) {
		t.Fatalf("approval evidence leaked the signature: %s", encoded)
	}
	// The operator's own words are what a reviewer wants, and must survive.
	if !strings.Contains(string(encoded), "launch review passed") {
		t.Fatal("expected the operator's reason to be recorded")
	}
}

func signBase64(key ed25519.PrivateKey, payload []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))
}
