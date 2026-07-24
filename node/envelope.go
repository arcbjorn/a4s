package node

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/arcbjorn/a4s/control"
)

const EnvelopeVersion = 1

type ActionEnvelope struct {
	Version        int            `json:"version"`
	ID             string         `json:"id"`
	NodeID         string         `json:"node_id"`
	GoalID         string         `json:"goal_id"`
	ProposalID     string         `json:"proposal_id"`
	WorldRevision  uint64         `json:"world_revision"`
	LeaseID        string         `json:"lease_id"`
	IdempotencyKey string         `json:"idempotency_key"`
	IssuedAt       time.Time      `json:"issued_at"`
	ExpiresAt      time.Time      `json:"expires_at"`
	Action         control.Action `json:"action"`
}

// SignedAction transmits the exact bytes that were signed rather than a decoded
// envelope. Verifying a re-marshaled struct would silently depend on encoder
// stability across Go versions, languages, and future field changes; the
// verifier therefore checks the signature over EnvelopeBytes and only then
// decodes them.
type SignedAction struct {
	EnvelopeBytes json.RawMessage `json:"envelope"`
	KeyID         string          `json:"key_id"`
	Signature     string          `json:"signature"`
}

// Envelope decodes the carried bytes without checking the signature. Callers
// that act on the result must use Verify instead; this exists for logging and
// error reporting on rejected input.
func (s SignedAction) Envelope() ActionEnvelope {
	var envelope ActionEnvelope
	_ = json.Unmarshal(s.EnvelopeBytes, &envelope)
	return envelope
}

func Sign(envelope ActionEnvelope, keyID string, privateKey ed25519.PrivateKey) (SignedAction, error) {
	if err := validateEnvelope(envelope); err != nil {
		return SignedAction{}, err
	}
	if keyID == "" || len(privateKey) != ed25519.PrivateKeySize {
		return SignedAction{}, fmt.Errorf("valid signing key id and private key are required")
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return SignedAction{}, fmt.Errorf("encode action envelope: %w", err)
	}
	signature := ed25519.Sign(privateKey, payload)
	return SignedAction{
		EnvelopeBytes: json.RawMessage(payload), KeyID: keyID,
		Signature: base64.RawStdEncoding.EncodeToString(signature),
	}, nil
}

// Verify authenticates the signed bytes and returns the decoded envelope with
// its digest. The signature is checked before the payload is interpreted.
func Verify(signed SignedAction, keys map[string]ed25519.PublicKey, nodeID string, now time.Time) (ActionEnvelope, string, error) {
	if len(signed.EnvelopeBytes) == 0 {
		return ActionEnvelope{}, "", fmt.Errorf("signed action carries no envelope")
	}
	publicKey := keys[signed.KeyID]
	if len(publicKey) != ed25519.PublicKeySize {
		return ActionEnvelope{}, "", fmt.Errorf("unknown signing key %q", signed.KeyID)
	}
	signature, err := base64.RawStdEncoding.DecodeString(signed.Signature)
	if err != nil {
		return ActionEnvelope{}, "", fmt.Errorf("decode action signature: %w", err)
	}
	if !ed25519.Verify(publicKey, signed.EnvelopeBytes, signature) {
		return ActionEnvelope{}, "", fmt.Errorf("invalid action signature")
	}

	// Only authenticated bytes are decoded and interpreted.
	var envelope ActionEnvelope
	decoder := json.NewDecoder(bytes.NewReader(signed.EnvelopeBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return ActionEnvelope{}, "", fmt.Errorf("decode action envelope: %w", err)
	}
	if decoder.More() {
		return ActionEnvelope{}, "", fmt.Errorf("action envelope has trailing content")
	}
	if err := validateEnvelope(envelope); err != nil {
		return ActionEnvelope{}, "", err
	}
	if envelope.NodeID != nodeID {
		return ActionEnvelope{}, "", fmt.Errorf("action targets node %q, dispatcher is %q", envelope.NodeID, nodeID)
	}
	if now.Before(envelope.IssuedAt.Add(-30 * time.Second)) {
		return ActionEnvelope{}, "", fmt.Errorf("action issue time is in the future")
	}
	if !now.Before(envelope.ExpiresAt) {
		return ActionEnvelope{}, "", fmt.Errorf("action envelope expired")
	}
	if envelope.ExpiresAt.Sub(envelope.IssuedAt) > 5*time.Minute {
		return ActionEnvelope{}, "", fmt.Errorf("action envelope lifetime exceeds five minutes")
	}
	digest := sha256.Sum256(signed.EnvelopeBytes)
	return envelope, hex.EncodeToString(digest[:]), nil
}

func validateEnvelope(envelope ActionEnvelope) error {
	if envelope.Version != EnvelopeVersion {
		return fmt.Errorf("unsupported action envelope version %d", envelope.Version)
	}
	if envelope.ID == "" || envelope.NodeID == "" || envelope.GoalID == "" || envelope.ProposalID == "" || envelope.LeaseID == "" || envelope.IdempotencyKey == "" {
		return fmt.Errorf("action envelope identity fields are required")
	}
	if envelope.IssuedAt.IsZero() || envelope.ExpiresAt.IsZero() || !envelope.ExpiresAt.After(envelope.IssuedAt) {
		return fmt.Errorf("action envelope time window is invalid")
	}
	if envelope.Action.Node != "" && envelope.Action.Node != envelope.NodeID {
		return fmt.Errorf("action node and envelope node differ")
	}
	return nil
}
