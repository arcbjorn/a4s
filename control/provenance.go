package control

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Image provenance: who built this, and do we trust them.
//
// A digest-pinned reference answers a different question. It proves the bytes
// cannot change under the goal, which is why a4s has always required it. It says
// nothing about where those bytes came from. On a cluster with a human deploying,
// that gap is covered by the human: somebody looked at the pipeline that produced
// the digest before pasting it into a manifest.
//
// a4s removes that person. An agent proposes a pull, the kernel authorizes it,
// and a node runs whatever the digest resolves to, with no step at which anyone
// vouched for it. Provenance is the deterministic replacement for the reviewer:
// a signed statement from a builder the cluster was told to trust, checked before
// the image is ever pulled.

// AttestationVersion is the image statement format version. A verifier refuses a
// version it does not understand rather than interpreting fields it cannot.
const AttestationVersion = 1

// ImageStatement is a builder's claim about one image.
//
// It names the image by its pinned digest, so the statement is bound to exact
// bytes. A statement about a tag would be a statement about whatever that tag
// points at today, which is the thing digest-pinning exists to prevent.
type ImageStatement struct {
	Version int `json:"version"`
	// Image is the digest-pinned reference this statement covers.
	Image string `json:"image"`
	// Builder names the pipeline or person that produced the image. It is
	// recorded for the audit trail; trust comes from the key, not this string.
	Builder string `json:"builder"`
	// KeyID names the signing key, carried inside the signed bytes so it cannot
	// be swapped for another without invalidating the signature.
	KeyID   string    `json:"key_id"`
	BuiltAt time.Time `json:"built_at"`
	// ExpiresAt optionally bounds how long the attestation stands. Zero means
	// it does not expire. An expiry is useful for a policy that wants images
	// re-attested periodically so a build that was fine last quarter does not
	// keep vouching for itself indefinitely.
	//
	// Tagged omitzero rather than omitempty: omitempty does not suppress a zero
	// time.Time, and a signed statement an operator reads should not carry a
	// year-one date that looks like a bug in the thing vouching for an image.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// SignedAttestation carries the exact signed bytes alongside the signature.
//
// The bytes travel with the signature for the same reason they do in an action
// envelope and in attested evidence: a verifier must check what was actually
// signed rather than a re-encoding, which a later field or a different encoder
// could render differently.
type SignedAttestation struct {
	StatementBytes json.RawMessage `json:"statement"`
	KeyID          string          `json:"key_id"`
	Signature      string          `json:"signature"`
}

// SignImageAttestation produces a builder's signed statement about an image.
func SignImageAttestation(statement ImageStatement, keyID string,
	key ed25519.PrivateKey) (SignedAttestation, error) {

	if keyID == "" || len(key) != ed25519.PrivateKeySize {
		return SignedAttestation{}, fmt.Errorf(
			"image attestation requires a key id and an ed25519 private key")
	}
	statement.Version = AttestationVersion
	statement.KeyID = keyID
	if err := validateStatement(statement); err != nil {
		return SignedAttestation{}, err
	}
	payload, err := json.Marshal(statement)
	if err != nil {
		return SignedAttestation{}, fmt.Errorf("encode image statement: %w", err)
	}
	return SignedAttestation{
		StatementBytes: payload, KeyID: keyID,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload)),
	}, nil
}

func validateStatement(statement ImageStatement) error {
	if statement.Version != AttestationVersion {
		return fmt.Errorf("unsupported image attestation version %d", statement.Version)
	}
	if !digestPattern.MatchString(statement.Image) {
		return fmt.Errorf("image attestation must name a digest-pinned image")
	}
	if statement.Builder == "" {
		return fmt.Errorf("image attestation must name a builder")
	}
	if statement.BuiltAt.IsZero() {
		return fmt.Errorf("image attestation requires a build time")
	}
	if !statement.ExpiresAt.IsZero() && !statement.ExpiresAt.After(statement.BuiltAt) {
		return fmt.Errorf("image attestation expiry must follow its build time")
	}
	return nil
}

// VerifyImageAttestation checks a statement against the trusted signers and the
// image it is supposed to cover.
//
// The signature is verified before the statement is decoded, so unverified bytes
// are never interpreted. The image is compared afterward, which is what stops a
// valid attestation for one image from vouching for another.
func VerifyImageAttestation(signed SignedAttestation, image string,
	signers map[string]ed25519.PublicKey, now time.Time) (ImageStatement, error) {

	if len(signers) == 0 {
		return ImageStatement{}, fmt.Errorf("no image signers are configured")
	}
	if len(signed.StatementBytes) == 0 {
		return ImageStatement{}, fmt.Errorf("image attestation carries no statement")
	}
	public, known := signers[signed.KeyID]
	if !known || len(public) != ed25519.PublicKeySize {
		// An unknown key id and a bad signature report identically, so probing
		// for trusted signer ids learns nothing from the difference.
		return ImageStatement{}, fmt.Errorf("image is not attested by a trusted signer")
	}
	signature, err := base64.StdEncoding.DecodeString(signed.Signature)
	if err != nil {
		return ImageStatement{}, fmt.Errorf("image attestation signature is malformed")
	}
	if !ed25519.Verify(public, signed.StatementBytes, signature) {
		return ImageStatement{}, fmt.Errorf("image is not attested by a trusted signer")
	}

	decoder := json.NewDecoder(bytes.NewReader(signed.StatementBytes))
	decoder.DisallowUnknownFields()
	var statement ImageStatement
	if err := decoder.Decode(&statement); err != nil {
		return ImageStatement{}, fmt.Errorf("image attestation is malformed")
	}
	if decoder.More() {
		return ImageStatement{}, fmt.Errorf("image attestation has trailing content")
	}
	if err := validateStatement(statement); err != nil {
		return ImageStatement{}, err
	}
	if statement.KeyID != signed.KeyID {
		return ImageStatement{}, fmt.Errorf("image attestation key id does not match its signature")
	}
	if statement.Image != image {
		// A signature covers one image. Without this an attestation for a
		// harmless build would authorize running anything else the holder had.
		return ImageStatement{}, fmt.Errorf(
			"image attestation covers %q, not %q", statement.Image, image)
	}
	if !statement.ExpiresAt.IsZero() && !now.Before(statement.ExpiresAt) {
		return ImageStatement{}, fmt.Errorf(
			"image attestation expired at %s", statement.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return statement, nil
}

// checkImageProvenance refuses a proposal that would run an image nobody trusted
// vouched for.
//
// An attestation that is present is always verified, whether or not the policy
// requires one. Accepting an unverifiable attestation because it happened to be
// optional would make attaching a forged one strictly better than attaching
// nothing, which is the wrong incentive to build into a trust decision.
func (k Kernel) checkImageProvenance(goal Goal, world World, proposal Proposal) error {
	runsImage := false
	for _, action := range proposal.Actions {
		switch action.Kind {
		case ActionPullImage, ActionCreateAllocation, ActionStartAllocation:
			runsImage = true
		}
	}
	if !runsImage {
		return nil
	}

	attestation := goal.Workload.Attestation
	if attestation == nil {
		if k.Policy.RequireSignedImages {
			return fmt.Errorf(
				"image %q carries no provenance attestation and policy requires one",
				goal.Workload.Image)
		}
		return nil
	}
	if _, err := VerifyImageAttestation(*attestation, goal.Workload.Image,
		k.Policy.ImageSigners, world.Now()); err != nil {
		return fmt.Errorf("image provenance: %w", err)
	}
	return nil
}
