package control

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ApprovalScopes are the decisions an operator can authorize.
//
// The set is closed. A free-form scope would let a goal or an importer invent
// an authorization the kernel never asked for, and a typo would silently grant
// nothing while appearing to grant something.
var ApprovalScopes = map[string]string{
	"public-route":         "expose a workload on the public internet",
	"destroy-stateful":     "delete an allocation holding durable data",
	"restore-volume":       "overwrite a volume from a snapshot",
	"move-volume":          "relocate a volume to another node",
	"agent-mutating-tools": "grant an agent tools that change state outside a4s",
}

// MaxApprovalLifetime bounds how long a grant may stand.
//
// An approval is a judgement about a situation, and situations change. An
// unbounded grant would become a standing permission nobody remembers issuing,
// which is how a one-time decision turns into ambient authority.
const MaxApprovalLifetime = 24 * time.Hour

// DefaultApprovalLifetime is used when an operator names no expiry.
const DefaultApprovalLifetime = time.Hour

// maxApprovalReason bounds the operator's note. It is prose for a later
// reviewer, not a payload.
const maxApprovalReason = 512

// ApprovalGrant is what an operator signs.
//
// It is a separate type from Approval because the two answer different
// questions: this is the authenticated statement an operator made, while
// Approval is the world's materialized view of it. Keeping them apart means the
// projection can record a grant without the signed bytes ever being mutable.
type ApprovalGrant struct {
	ID     string `json:"id"`
	GoalID string `json:"goal_id"`
	Scope  string `json:"scope"`
	// IssuedBy names the operator principal. It is carried in the signed bytes,
	// so it cannot be edited after the fact without invalidating the signature.
	IssuedBy string `json:"issued_by"`
	// KeyID names which operator key signed this, so a compromised key can be
	// traced to exactly the grants it issued.
	KeyID     string    `json:"key_id"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Revision  uint64    `json:"revision,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// SignedApproval is a grant with its operator signature.
//
// The signed bytes travel alongside the signature rather than being recomputed
// from the struct, for the same reason a node action envelope does: a verifier
// must check exactly what was signed, not a re-encoding that might differ.
type SignedApproval struct {
	GrantBytes []byte `json:"grant_bytes"`
	KeyID      string `json:"key_id"`
	Signature  string `json:"signature"`
}

// SignApproval produces an operator-signed grant.
func SignApproval(grant ApprovalGrant, keyID string, key ed25519.PrivateKey) (SignedApproval, error) {
	if keyID == "" || len(key) != ed25519.PrivateKeySize {
		return SignedApproval{}, fmt.Errorf("approval requires a key id and an ed25519 private key")
	}
	grant.KeyID = keyID
	if err := validateGrant(grant); err != nil {
		return SignedApproval{}, err
	}
	payload, err := json.Marshal(grant)
	if err != nil {
		return SignedApproval{}, fmt.Errorf("encode approval: %w", err)
	}
	return SignedApproval{
		GrantBytes: payload, KeyID: keyID,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload)),
	}, nil
}

// VerifyApproval authenticates a signed grant against the known operator keys.
//
// This is the boundary the whole approval model rests on. Everything the kernel
// gates behind an approval — public exposure, destroying durable data, restoring
// over live data, granting an agent mutating tools — is authorized by a grant
// that passed through here. An agent has no path to this function: it holds no
// operator key, and a proposal carries no signature field to smuggle one in.
func VerifyApproval(signed SignedApproval, keys map[string]ed25519.PublicKey,
	now time.Time) (ApprovalGrant, error) {

	if len(keys) == 0 {
		return ApprovalGrant{}, fmt.Errorf("no operator keys are configured")
	}
	publicKey, known := keys[signed.KeyID]
	if !known || len(publicKey) != ed25519.PublicKeySize {
		// An unknown key id and a bad signature are reported the same way, so a
		// caller probing for valid key ids learns nothing from the difference.
		return ApprovalGrant{}, fmt.Errorf("approval is not signed by a known operator key")
	}
	signature, err := base64.StdEncoding.DecodeString(signed.Signature)
	if err != nil {
		return ApprovalGrant{}, fmt.Errorf("approval signature is malformed")
	}
	if !ed25519.Verify(publicKey, signed.GrantBytes, signature) {
		return ApprovalGrant{}, fmt.Errorf("approval is not signed by a known operator key")
	}

	var grant ApprovalGrant
	// Unknown fields are refused: a grant carrying something this build does not
	// understand may have been signed against different semantics.
	decoder := json.NewDecoder(strings.NewReader(string(signed.GrantBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&grant); err != nil {
		return ApprovalGrant{}, fmt.Errorf("decode approval: %w", err)
	}
	// The key that signed must be the key the grant names, or a grant could be
	// re-signed by one operator while still attributing itself to another.
	if grant.KeyID != signed.KeyID {
		return ApprovalGrant{}, fmt.Errorf("approval names key %q but was signed by %q",
			grant.KeyID, signed.KeyID)
	}
	if err := validateGrant(grant); err != nil {
		return ApprovalGrant{}, err
	}
	if !grant.ExpiresAt.After(now) {
		return ApprovalGrant{}, fmt.Errorf("approval expired at %s",
			grant.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return grant, nil
}

// Approval materializes the world's view of a verified grant.
func (g ApprovalGrant) Approval() *Approval {
	return &Approval{
		ID: g.ID, GoalID: g.GoalID, Scope: g.Scope, IssuedBy: g.IssuedBy,
		Granted: true, IssuedAt: g.IssuedAt, ExpiresAt: g.ExpiresAt,
		Revision: g.Revision, Reason: g.Reason,
	}
}

// Evidence renders a verified grant for the durable log.
//
// The signature is not recorded. The log proves an approval was accepted and by
// whom; re-verifying it later would require the signed bytes, and storing them
// would put a replayable authorization into a file that is meant to be readable.
func (g ApprovalGrant) Evidence() Evidence {
	observed := map[string]string{
		"goal":       g.GoalID,
		"scope":      g.Scope,
		"issued_by":  g.IssuedBy,
		"key_id":     g.KeyID,
		"expires_at": g.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if g.Revision > 0 {
		observed["revision"] = fmt.Sprint(g.Revision)
	}
	if g.Reason != "" {
		observed["reason"] = g.Reason
	}
	return Evidence{
		Kind: EvidenceApprovalGranted, Target: g.ID, Source: "operator:" + g.IssuedBy,
		ObservedAt: g.IssuedAt, ExpiresAt: g.ExpiresAt, Observed: observed,
	}
}

// validateGrant enforces the shape of an operator decision.
func validateGrant(grant ApprovalGrant) error {
	if !namePattern.MatchString(grant.ID) {
		return fmt.Errorf("approval id must be lowercase DNS-style text")
	}
	if !namePattern.MatchString(grant.GoalID) {
		return fmt.Errorf("approval must name a valid goal id")
	}
	if _, known := ApprovalScopes[grant.Scope]; !known {
		return fmt.Errorf("approval scope %q is not one the kernel gates on", grant.Scope)
	}
	if strings.TrimSpace(grant.IssuedBy) == "" {
		return fmt.Errorf("approval must name the operator who issued it")
	}
	if grant.KeyID == "" {
		return fmt.Errorf("approval must name the key that signed it")
	}
	if grant.IssuedAt.IsZero() || grant.ExpiresAt.IsZero() {
		return fmt.Errorf("approval must record issuance and expiry")
	}
	if !grant.ExpiresAt.After(grant.IssuedAt) {
		return fmt.Errorf("approval expires before it was issued")
	}
	// A grant that outlives the maximum would become standing authority.
	if grant.ExpiresAt.Sub(grant.IssuedAt) > MaxApprovalLifetime {
		return fmt.Errorf("approval lifetime exceeds the %s maximum", MaxApprovalLifetime)
	}
	if len(grant.Reason) > maxApprovalReason {
		return fmt.Errorf("approval reason is longer than %d characters", maxApprovalReason)
	}
	return nil
}
