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
