package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// RequestVersion is the operator request envelope version. A verifier refuses
// a version it does not understand rather than guessing at the semantics of
// fields it cannot interpret.
const RequestVersion = 1

// MaxRequestLifetime bounds how long a signed operator request stays valid.
//
// The window exists so a captured request cannot be replayed indefinitely. It
// is deliberately short: an operator request travels from a CLI to the server
// directly, so seconds are sufficient and anything longer only widens the
// window an attacker has to work with.
const MaxRequestLifetime = 5 * time.Minute

// clockSkewTolerance accepts a request issued slightly in the future, because
// operator workstations and servers do not share a clock. It is small enough
// that it does not meaningfully extend the replay window.
const clockSkewTolerance = 30 * time.Second

// RequestEnvelope is the authenticated statement an operator makes when calling
// the API.
//
// The envelope binds the signature to the specific call: method and path so a
// signed read cannot be replayed as a write against another endpoint, a body
// digest so the payload cannot be swapped, and a nonce plus issue time so a
// captured request cannot be replayed at all. Signing only the body would leave
// every one of those substitutions available.
type RequestEnvelope struct {
	Version int `json:"version"`
	// Nonce makes each request unique. The server remembers recently seen
	// nonces, which is what turns the signature into single-use authority.
	Nonce string `json:"nonce"`
	// Method and Path bind the signature to one endpoint and verb.
	Method string `json:"method"`
	Path   string `json:"path"`
	// BodyDigest is the hex SHA-256 of the request body, empty for no body.
	BodyDigest string `json:"body_digest,omitempty"`
	// IssuedBy names the operator principal, carried inside the signed bytes so
	// it cannot be edited without invalidating the signature.
	IssuedBy  string    `json:"issued_by"`
	KeyID     string    `json:"key_id"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SignedRequest carries the exact signed bytes alongside the signature, for the
// same reason the node action envelope does: the verifier must check what was
// actually signed rather than a re-encoding that might differ.
type SignedRequest struct {
	EnvelopeBytes json.RawMessage `json:"envelope"`
	KeyID         string          `json:"key_id"`
	Signature     string          `json:"signature"`
}

// SignRequest produces an operator-signed API request.
func SignRequest(envelope RequestEnvelope, keyID string,
	key ed25519.PrivateKey) (SignedRequest, error) {

	if keyID == "" || len(key) != ed25519.PrivateKeySize {
		return SignedRequest{}, fmt.Errorf("request requires a key id and an ed25519 private key")
	}
	envelope.Version = RequestVersion
	envelope.KeyID = keyID
	if err := validateRequestEnvelope(envelope); err != nil {
		return SignedRequest{}, err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return SignedRequest{}, fmt.Errorf("encode request envelope: %w", err)
	}
	return SignedRequest{
		EnvelopeBytes: payload, KeyID: keyID,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload)),
	}, nil
}

func validateRequestEnvelope(envelope RequestEnvelope) error {
	if envelope.Version != RequestVersion {
		return fmt.Errorf("unsupported request version %d", envelope.Version)
	}
	if envelope.Nonce == "" {
		return fmt.Errorf("request requires a nonce")
	}
	if envelope.Method == "" || envelope.Path == "" {
		return fmt.Errorf("request must name a method and path")
	}
	if envelope.IssuedBy == "" {
		return fmt.Errorf("request must name the operator that issued it")
	}
	if envelope.IssuedAt.IsZero() || envelope.ExpiresAt.IsZero() {
		return fmt.Errorf("request requires an issue time and expiry")
	}
	if !envelope.ExpiresAt.After(envelope.IssuedAt) {
		return fmt.Errorf("request expiry must follow its issue time")
	}
	if envelope.ExpiresAt.Sub(envelope.IssuedAt) > MaxRequestLifetime {
		return fmt.Errorf("request lifetime exceeds %s", MaxRequestLifetime)
	}
	return nil
}

// BodyDigest returns the digest an envelope must carry for this body.
func BodyDigest(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// VerifyRequest authenticates a signed operator request against known keys and
// the specific call being made.
//
// Signature verification happens before the envelope is decoded, so unverified
// bytes are never interpreted. The method, path, and body are checked against
// the signed envelope afterward, which is what prevents a valid signature for
// one call from authorizing a different one.
func VerifyRequest(signed SignedRequest, keys map[string]ed25519.PublicKey,
	method, path string, body []byte, now time.Time) (RequestEnvelope, error) {

	if len(keys) == 0 {
		return RequestEnvelope{}, fmt.Errorf("no operator keys are configured")
	}
	if len(signed.EnvelopeBytes) == 0 {
		return RequestEnvelope{}, fmt.Errorf("signed request carries no envelope")
	}
	publicKey, known := keys[signed.KeyID]
	if !known || len(publicKey) != ed25519.PublicKeySize {
		// An unknown key id and a bad signature report identically, so probing
		// for valid key ids learns nothing from the difference.
		return RequestEnvelope{}, fmt.Errorf("request is not signed by a known operator key")
	}
	signature, err := base64.StdEncoding.DecodeString(signed.Signature)
	if err != nil {
		return RequestEnvelope{}, fmt.Errorf("request signature is malformed")
	}
	if !ed25519.Verify(publicKey, signed.EnvelopeBytes, signature) {
		return RequestEnvelope{}, fmt.Errorf("request is not signed by a known operator key")
	}

	decoder := json.NewDecoder(bytes.NewReader(signed.EnvelopeBytes))
	decoder.DisallowUnknownFields()
	var envelope RequestEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return RequestEnvelope{}, fmt.Errorf("request envelope is malformed")
	}
	if decoder.More() {
		return RequestEnvelope{}, fmt.Errorf("request envelope has trailing content")
	}
	if err := validateRequestEnvelope(envelope); err != nil {
		return RequestEnvelope{}, err
	}
	if envelope.KeyID != signed.KeyID {
		return RequestEnvelope{}, fmt.Errorf("request key id does not match the signed envelope")
	}
	if envelope.Method != method || envelope.Path != path {
		// A signature authorizes one call. Without this check a signed read of
		// history would also authorize a delete.
		return RequestEnvelope{}, fmt.Errorf("request signature does not cover %s %s", method, path)
	}
	if envelope.BodyDigest != BodyDigest(body) {
		return RequestEnvelope{}, fmt.Errorf("request body does not match its signed digest")
	}
	if now.After(envelope.ExpiresAt) {
		return RequestEnvelope{}, fmt.Errorf("request expired at %s", envelope.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if envelope.IssuedAt.After(now.Add(clockSkewTolerance)) {
		return RequestEnvelope{}, fmt.Errorf("request was issued in the future")
	}
	return envelope, nil
}

// nonceLedger remembers recently accepted request nonces so a captured request
// cannot be replayed inside its validity window.
//
// Entries are dropped once the request that carried them could no longer be
// accepted anyway, which bounds the memory a caller can cause the server to
// hold. Expiry is what keeps this a fixed cost rather than a leak.
type nonceLedger struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newNonceLedger() *nonceLedger {
	return &nonceLedger{seen: make(map[string]time.Time)}
}

// observe records a nonce and reports whether it was already used. A nonce is
// scoped to the key that signed it, so two operators choosing the same nonce do
// not lock each other out.
func (l *nonceLedger) observe(keyID, nonce string, expiresAt, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, expiry := range l.seen {
		if now.After(expiry) {
			delete(l.seen, key)
		}
	}
	scoped := keyID + "\x00" + nonce
	if _, used := l.seen[scoped]; used {
		return false
	}
	l.seen[scoped] = expiresAt
	return true
}
