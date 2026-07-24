package node

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

// HandshakeVersion is the enrollment protocol version. It is independent of
// both the goal API version and the action envelope version.
const HandshakeVersion = 1

// DefaultHandshakeTimeout bounds how long a peer may take to complete
// enrollment, so a stalled or hostile connection cannot hold resources.
const DefaultHandshakeTimeout = 10 * time.Second

// challengeSize is the nonce length. Thirty-two random bytes make replaying a
// previously observed proof computationally hopeless.
const challengeSize = 32

// Hello is the node's opening claim. It is only a claim: the server does not
// act on it until the node proves possession of the matching private key.
type Hello struct {
	Version int    `json:"version"`
	NodeID  string `json:"node_id"`
}

// Challenge is the server's random nonce. Binding the proof to a fresh nonce is
// what makes the handshake resistant to replay: a captured proof authenticates
// only the one connection it was produced for.
type Challenge struct {
	Version   int    `json:"version"`
	Nonce     string `json:"nonce"`
	ServerKey string `json:"server_key_id"`
}

// Proof is the node's signature over the challenge, demonstrating it holds the
// private key for the identity it claimed.
type Proof struct {
	Version   int    `json:"version"`
	NodeID    string `json:"node_id"`
	Signature string `json:"signature"`
}

// Enrolled is the server's acceptance, naming the key the node should trust for
// action envelopes.
type Enrolled struct {
	Version     int    `json:"version"`
	Accepted    bool   `json:"accepted"`
	Reason      string `json:"reason,omitempty"`
	ServerKeyID string `json:"server_key_id,omitempty"`
}

// challengePayload is the exact byte sequence both sides sign over. Building it
// from typed fields rather than concatenating strings keeps a node ID that
// contains delimiters from shifting the boundary between fields.
func challengePayload(nodeID string, nonce []byte) ([]byte, error) {
	return json.Marshal(struct {
		Version int    `json:"version"`
		NodeID  string `json:"node_id"`
		Nonce   []byte `json:"nonce"`
	}{Version: HandshakeVersion, NodeID: nodeID, Nonce: nonce})
}

// AcceptNode runs the server side of enrollment over an established connection.
//
// The server learns which node it is talking to by verification, never by
// assertion. A connection that cannot prove possession of an enrolled node key
// is closed without ever being issued a capability.
func AcceptNode(conn net.Conn, nodeKeys map[string]ed25519.PublicKey, serverKeyID string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = DefaultHandshakeTimeout
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return "", fmt.Errorf("set handshake deadline: %w", err)
	}
	// Clear the handshake deadline so it does not later expire mid-session.
	defer conn.SetDeadline(time.Time{})

	decoder := json.NewDecoder(bufio.NewReader(conn))
	decoder.DisallowUnknownFields()
	encoder := json.NewEncoder(conn)

	var hello Hello
	if err := decoder.Decode(&hello); err != nil {
		return "", fmt.Errorf("read hello: %w", err)
	}
	if hello.Version != HandshakeVersion {
		return "", fmt.Errorf("unsupported handshake version %d", hello.Version)
	}
	if hello.NodeID == "" {
		return "", fmt.Errorf("hello carries no node id")
	}

	nonce := make([]byte, challengeSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate challenge: %w", err)
	}
	challenge := Challenge{
		Version:   HandshakeVersion,
		Nonce:     base64.RawStdEncoding.EncodeToString(nonce),
		ServerKey: serverKeyID,
	}
	if err := encoder.Encode(challenge); err != nil {
		return "", fmt.Errorf("send challenge: %w", err)
	}

	var proof Proof
	if err := decoder.Decode(&proof); err != nil {
		return "", fmt.Errorf("read proof: %w", err)
	}
	reject := func(reason string) (string, error) {
		// Tell the peer it was refused, but keep the reason generic on the wire
		// so an unenrolled prober cannot distinguish "unknown node" from "bad
		// signature" and enumerate valid node identities.
		_ = encoder.Encode(Enrolled{Version: HandshakeVersion, Accepted: false, Reason: "enrollment refused"})
		return "", fmt.Errorf("%s", reason)
	}
	if proof.Version != HandshakeVersion {
		return reject(fmt.Sprintf("unsupported proof version %d", proof.Version))
	}
	// The proof must name the same identity as the hello, or a peer could claim
	// one identity and prove another.
	if subtle.ConstantTimeCompare([]byte(proof.NodeID), []byte(hello.NodeID)) != 1 {
		return reject("proof identity does not match hello")
	}
	publicKey, enrolled := nodeKeys[hello.NodeID]
	if !enrolled || len(publicKey) != ed25519.PublicKeySize {
		return reject(fmt.Sprintf("node %q is not enrolled", hello.NodeID))
	}
	signature, err := base64.RawStdEncoding.DecodeString(proof.Signature)
	if err != nil {
		return reject("proof signature is not valid base64")
	}
	payload, err := challengePayload(hello.NodeID, nonce)
	if err != nil {
		return "", err
	}
	if !ed25519.Verify(publicKey, payload, signature) {
		return reject(fmt.Sprintf("node %q failed to prove its identity", hello.NodeID))
	}

	if err := encoder.Encode(Enrolled{
		Version: HandshakeVersion, Accepted: true, ServerKeyID: serverKeyID,
	}); err != nil {
		return "", fmt.Errorf("send enrollment result: %w", err)
	}
	return hello.NodeID, nil
}

// ConnectToServer runs the node side of enrollment and returns the server key ID
// the node should trust for action envelopes.
func ConnectToServer(conn net.Conn, nodeID string, nodeKey ed25519.PrivateKey, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = DefaultHandshakeTimeout
	}
	if len(nodeKey) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("node identity key is invalid")
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return "", fmt.Errorf("set handshake deadline: %w", err)
	}
	defer conn.SetDeadline(time.Time{})

	decoder := json.NewDecoder(bufio.NewReader(conn))
	decoder.DisallowUnknownFields()
	encoder := json.NewEncoder(conn)

	if err := encoder.Encode(Hello{Version: HandshakeVersion, NodeID: nodeID}); err != nil {
		return "", fmt.Errorf("send hello: %w", err)
	}
	var challenge Challenge
	if err := decoder.Decode(&challenge); err != nil {
		return "", fmt.Errorf("read challenge: %w", err)
	}
	if challenge.Version != HandshakeVersion {
		return "", fmt.Errorf("unsupported challenge version %d", challenge.Version)
	}
	nonce, err := base64.RawStdEncoding.DecodeString(challenge.Nonce)
	if err != nil {
		return "", fmt.Errorf("decode challenge nonce: %w", err)
	}
	// A short nonce would weaken replay resistance, so refuse rather than sign
	// whatever the peer supplied.
	if len(nonce) < challengeSize {
		return "", fmt.Errorf("challenge nonce is too short")
	}

	payload, err := challengePayload(nodeID, nonce)
	if err != nil {
		return "", err
	}
	proof := Proof{
		Version: HandshakeVersion, NodeID: nodeID,
		Signature: base64.RawStdEncoding.EncodeToString(ed25519.Sign(nodeKey, payload)),
	}
	if err := encoder.Encode(proof); err != nil {
		return "", fmt.Errorf("send proof: %w", err)
	}

	var enrolled Enrolled
	if err := decoder.Decode(&enrolled); err != nil {
		if err == io.EOF {
			return "", fmt.Errorf("server closed the connection without enrolling this node")
		}
		return "", fmt.Errorf("read enrollment result: %w", err)
	}
	if !enrolled.Accepted {
		reason := enrolled.Reason
		if reason == "" {
			reason = "enrollment refused"
		}
		return "", fmt.Errorf("server refused enrollment: %s", reason)
	}
	if enrolled.ServerKeyID == "" {
		return "", fmt.Errorf("server did not name its signing key")
	}
	return enrolled.ServerKeyID, nil
}
