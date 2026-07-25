package control

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// AttestedEvidence is an observation with the reporting node's signature over it.
//
// Evidence is the only input that advances the world, so whoever can forge it can
// move the cluster. Until this existed, every observation was carried under the
// controller's own signing key: the node authenticated when it connected, but the
// individual facts it reported were not separately attributable, so a compromised
// node could report any readiness, spend, or volume ownership it liked and the
// projection had no way to tell.
//
// The signature is detached rather than a field inside Evidence, because a
// signature cannot cover the struct it lives in. The signed bytes travel
// alongside it for the same reason a node action envelope carries its own bytes:
// a verifier must check exactly what was signed, not a re-encoding that a future
// field or a different encoder might render differently.
type AttestedEvidence struct {
	EvidenceBytes []byte `json:"evidence_bytes"`
	// NodeID names the claimed reporter. It is checked against the key that
	// actually signed, so a node cannot attribute its observation to another.
	NodeID    string `json:"node_id"`
	Signature string `json:"signature"`
}

// SignEvidence attests an observation with a node's identity key.
//
// The key is the same Ed25519 identity the node proved possession of during
// enrollment, which is deliberate: introducing a second per-node key would mean a
// second distribution and rotation problem for no additional authority. The
// server already holds the public half.
func SignEvidence(evidence Evidence, nodeID string, key ed25519.PrivateKey) (AttestedEvidence, error) {
	if nodeID == "" {
		return AttestedEvidence{}, fmt.Errorf("evidence attestation requires a node id")
	}
	if len(key) != ed25519.PrivateKeySize {
		return AttestedEvidence{}, fmt.Errorf("evidence attestation requires an ed25519 private key")
	}
	if evidence.Kind == "" {
		return AttestedEvidence{}, fmt.Errorf("evidence attestation requires a kind")
	}
	payload, err := json.Marshal(evidence)
	if err != nil {
		return AttestedEvidence{}, fmt.Errorf("encode evidence: %w", err)
	}
	return AttestedEvidence{
		EvidenceBytes: payload, NodeID: nodeID,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload)),
	}, nil
}

// VerifyEvidence authenticates attested evidence against the enrolled node keys.
//
// This is the boundary that makes the world projection trustworthy. Every
// readiness measurement, spend report, and ownership claim that reaches the
// projection passes through here, and a node that is not enrolled has no path in.
//
// maxAge bounds how long an observation may travel before it is refused, which
// stops a captured attestation from being replayed indefinitely. Pass zero to
// skip the check for state-change evidence that does not decay.
func VerifyEvidence(attested AttestedEvidence, keys map[string]ed25519.PublicKey,
	now time.Time, maxAge time.Duration) (Evidence, error) {

	if len(keys) == 0 {
		return Evidence{}, fmt.Errorf("no enrolled node keys are configured")
	}
	publicKey, known := keys[attested.NodeID]
	if !known || len(publicKey) != ed25519.PublicKeySize {
		// An unknown node and a bad signature are reported identically, so a
		// caller probing for enrolled node ids learns nothing from the difference.
		return Evidence{}, fmt.Errorf("evidence is not signed by an enrolled node")
	}
	signature, err := base64.StdEncoding.DecodeString(attested.Signature)
	if err != nil {
		return Evidence{}, fmt.Errorf("evidence signature is malformed")
	}
	if !ed25519.Verify(publicKey, attested.EvidenceBytes, signature) {
		return Evidence{}, fmt.Errorf("evidence is not signed by an enrolled node")
	}

	// Only authenticated bytes are decoded. Unknown fields are refused because
	// evidence carrying something this build does not understand may have been
	// signed against different semantics.
	var evidence Evidence
	decoder := json.NewDecoder(bytes.NewReader(attested.EvidenceBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return Evidence{}, fmt.Errorf("decode attested evidence: %w", err)
	}
	if decoder.More() {
		return Evidence{}, fmt.Errorf("attested evidence has trailing content")
	}
	if evidence.Kind == "" {
		return Evidence{}, fmt.Errorf("attested evidence has no kind")
	}

	// The signing node must be the node the evidence claims observed it.
	// Otherwise one enrolled node could attest to another's observations, which
	// is exactly the impersonation this boundary exists to stop.
	if observer := evidence.Observed["node"]; observer != "" && observer != attested.NodeID {
		return Evidence{}, fmt.Errorf(
			"evidence claims node %q but was signed by %q", observer, attested.NodeID)
	}
	if maxAge > 0 && !evidence.ObservedAt.IsZero() {
		if now.Sub(evidence.ObservedAt) > maxAge {
			return Evidence{}, fmt.Errorf("attested evidence is older than %s", maxAge)
		}
		if evidence.ObservedAt.After(now.Add(30 * time.Second)) {
			// The same clock tolerance the action envelope allows.
			return Evidence{}, fmt.Errorf("attested evidence was observed in the future")
		}
	}
	return evidence, nil
}
