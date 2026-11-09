package control

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func TestVerifyApprovalRefusesFutureIssuance(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{"op-key": public}
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	sign := func(issued time.Time) SignedApproval {
		t.Helper()
		signed, err := SignApproval(ApprovalGrant{ID: "grant-1", GoalID: "web", Scope: "public-route",
			IssuedBy: "op", IssuedAt: issued, ExpiresAt: issued.Add(time.Hour)}, "op-key", private)
		if err != nil {
			t.Fatal(err)
		}
		return signed
	}

	if _, err := VerifyApproval(sign(now.AddDate(10, 0, 0)), keys, now); err == nil {
		t.Fatal("a grant issued ten years ahead verified, and would stand for ten years")
	}
	if _, err := VerifyApproval(sign(now.Add(maxApprovalSkew+time.Second)), keys, now); err == nil {
		t.Fatal("a grant issued beyond the skew allowance verified")
	}
	if _, err := VerifyApproval(sign(now.Add(maxApprovalSkew)), keys, now); err != nil {
		t.Fatalf("a grant within the skew allowance was refused: %v", err)
	}
	if _, err := VerifyApproval(sign(now), keys, now); err != nil {
		t.Fatalf("a grant issued now was refused: %v", err)
	}
}

func TestCheckRegrant(t *testing.T) {
	issued := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	revoked := issued.Add(time.Minute)
	existing := &Approval{ID: "a", IssuedAt: issued, RevokedAt: revoked}
	tests := map[string]struct {
		existing *Approval
		issuedAt time.Time
		allowed  bool
	}{
		"first grant":             {nil, issued, true},
		"replay of original":      {&Approval{ID: "a", IssuedAt: issued}, issued, false},
		"older than the original": {&Approval{ID: "a", IssuedAt: issued}, issued.Add(-time.Second), false},
		"newer than the original": {&Approval{ID: "a", IssuedAt: issued}, issued.Add(time.Second), true},
		"before the revocation":   {existing, revoked.Add(-time.Second), false},
		"at the revocation":       {existing, revoked, false},
		"after the revocation":    {existing, revoked.Add(time.Nanosecond), true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := CheckRegrant(test.existing, ApprovalGrant{ID: "a", IssuedAt: test.issuedAt})
			if test.allowed != (err == nil) {
				t.Fatalf("allowed=%t, got %v", test.allowed, err)
			}
		})
	}
}

func TestRevocationTimeNeverPrecedesItsToken(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	ahead := ApprovalGrant{IssuedAt: now.Add(20 * time.Second)}
	if got := RevocationTime(ahead, now); !got.Equal(ahead.IssuedAt) {
		t.Fatalf("revocation recorded at %s, before its own token was issued", got)
	}
	behind := ApprovalGrant{IssuedAt: now.Add(-time.Second)}
	if got := RevocationTime(behind, now); !got.Equal(now) {
		t.Fatalf("revocation recorded at %s, want now", got)
	}
}
