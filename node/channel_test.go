package node

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// handshakePair runs a real enrollment over a socket pair and returns both
// negotiated sessions.
func handshakePair(t *testing.T) (*session, *session) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverConn, nodeConn := net.Pipe()
	t.Cleanup(func() { serverConn.Close(); nodeConn.Close() })

	var serverSession *session
	var serverErr error
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		serverSession, _, serverErr = acceptNode(serverConn,
			map[string]ed25519.PublicKey{"node-a": public}, "control-1", 5*time.Second)
	}()

	nodeSession, _, err := connectToServer(nodeConn, "node-a", private, 5*time.Second)
	if err != nil {
		t.Fatalf("node handshake: %v", err)
	}
	wait.Wait()
	if serverErr != nil {
		t.Fatalf("server handshake: %v", serverErr)
	}
	return serverSession, nodeSession
}

// Both sides must derive the same keys, mirrored by direction.
func TestHandshakeAgreesOnDirectionalKeys(t *testing.T) {
	serverSession, nodeSession := handshakePair(t)
	if serverSession == nil || nodeSession == nil {
		t.Fatal("handshake negotiated no session")
	}
	if !bytes.Equal(serverSession.sendKey, nodeSession.receiveKey) {
		t.Fatal("server send key does not match node receive key")
	}
	if !bytes.Equal(serverSession.receiveKey, nodeSession.sendKey) {
		t.Fatal("server receive key does not match node send key")
	}
	// Reusing one key in both directions would let a record be reflected back
	// at its sender.
	if bytes.Equal(serverSession.sendKey, serverSession.receiveKey) {
		t.Fatal("both directions share one key")
	}
}

// Each session must derive fresh keys, or recording one session and
// compromising a key later would reveal every other session.
func TestHandshakeDerivesFreshKeysPerSession(t *testing.T) {
	firstServer, _ := handshakePair(t)
	secondServer, _ := handshakePair(t)
	if bytes.Equal(firstServer.sendKey, secondServer.sendKey) {
		t.Fatal("two sessions derived the same key")
	}
}

func securePair(t *testing.T) (*secureConn, *secureConn) {
	t.Helper()
	serverSession, nodeSession := handshakePair(t)
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })

	server, err := newSecureConn(left, serverSession.sendKey, serverSession.receiveKey)
	if err != nil {
		t.Fatal(err)
	}
	node, err := newSecureConn(right, nodeSession.sendKey, nodeSession.receiveKey)
	if err != nil {
		t.Fatal(err)
	}
	return server, node
}

func TestChannelRoundTripsData(t *testing.T) {
	server, node := securePair(t)
	message := []byte(`{"envelope":"test","action":"start"}`)

	go func() { _, _ = server.Write(message) }()

	received := make([]byte, len(message))
	if _, err := io.ReadFull(node, received); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(received, message) {
		t.Fatalf("received %q, want %q", received, message)
	}
}

// The plaintext must not appear on the wire, which is the entire point.
func TestChannelDoesNotTransmitPlaintext(t *testing.T) {
	serverSession, _ := handshakePair(t)
	var wire bytes.Buffer
	secure, err := newSecureConn(nopConn{Writer: &wire},
		serverSession.sendKey, serverSession.receiveKey)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("start allocation web-0 with database password hunter2")
	if _, err := secure.Write(secret); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire.Bytes(), secret) {
		t.Fatal("plaintext appeared on the wire")
	}
	if bytes.Contains(wire.Bytes(), []byte("hunter2")) {
		t.Fatal("secret material appeared on the wire")
	}
}

// A flipped bit must fail authentication rather than yield altered plaintext.
func TestChannelRefusesTamperedRecord(t *testing.T) {
	serverSession, nodeSession := handshakePair(t)
	var wire bytes.Buffer
	sender, err := newSecureConn(nopConn{Writer: &wire},
		serverSession.sendKey, serverSession.receiveKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Write([]byte("stop allocation web-0")); err != nil {
		t.Fatal(err)
	}

	tampered := wire.Bytes()
	tampered[len(tampered)-1] ^= 0x01

	receiver, err := newSecureConn(nopConn{Reader: bytes.NewReader(tampered)},
		nodeSession.sendKey, nodeSession.receiveKey)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64)
	if _, err := receiver.Read(buffer); err == nil {
		t.Fatal("expected a tampered record to fail authentication")
	}
}

// Replaying a record out of order must fail, because the nonce is the sequence
// number and a repeated record decrypts under the wrong one.
func TestChannelRefusesReorderedRecords(t *testing.T) {
	serverSession, nodeSession := handshakePair(t)
	var wire bytes.Buffer
	sender, err := newSecureConn(nopConn{Writer: &wire},
		serverSession.sendKey, serverSession.receiveKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	firstRecord := append([]byte(nil), wire.Bytes()...)
	if _, err := sender.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}

	// Feed the first record twice. The second copy arrives where the receiver
	// expects sequence 1, so it must not authenticate.
	replayed := append(append([]byte(nil), firstRecord...), firstRecord...)
	receiver, err := newSecureConn(nopConn{Reader: bytes.NewReader(replayed)},
		nodeSession.sendKey, nodeSession.receiveKey)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64)
	if _, err := receiver.Read(buffer); err != nil {
		t.Fatalf("first record should decrypt: %v", err)
	}
	if _, err := receiver.Read(buffer); err == nil {
		t.Fatal("expected a replayed record to be refused")
	}
}

// A peer-controlled length prefix must be bounded before allocation.
func TestChannelRefusesOversizedFrame(t *testing.T) {
	serverSession, _ := handshakePair(t)
	frame := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	receiver, err := newSecureConn(nopConn{Reader: bytes.NewReader(frame)},
		serverSession.sendKey, serverSession.receiveKey)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 16)
	_, err = receiver.Read(buffer)
	if err == nil {
		t.Fatal("expected an oversized frame to be refused")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// An attacker who substitutes their own ephemeral key must be caught, because
// the shares are inside the signed payload.
func TestHandshakeRefusesSubstitutedEphemeralKey(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverConn, nodeConn := net.Pipe()
	defer serverConn.Close()
	defer nodeConn.Close()

	var serverErr error
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		_, _, serverErr = acceptNode(serverConn,
			map[string]ed25519.PublicKey{"node-a": public}, "control-1", 5*time.Second)
	}()

	// Act as a node, but sign over a different ephemeral key than the one sent,
	// which is what a man in the middle rewriting the hello would produce.
	decoder := json.NewDecoder(nodeConn)
	encoder := json.NewEncoder(nodeConn)

	_, honestPublic, err := generateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	_, attackerPublic, err := generateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(Hello{
		Version: HandshakeVersion, NodeID: "node-a",
		EphemeralKey: base64.RawStdEncoding.EncodeToString(attackerPublic),
	}); err != nil {
		t.Fatal(err)
	}
	var challenge Challenge
	if err := decoder.Decode(&challenge); err != nil {
		t.Fatal(err)
	}
	nonce, err := base64.RawStdEncoding.DecodeString(challenge.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	serverEphemeral, err := decodeEphemeral(challenge.EphemeralKey)
	if err != nil {
		t.Fatal(err)
	}
	// Sign the honest share while having transmitted the attacker's.
	payload, err := challengePayload("node-a", nonce, honestPublic, serverEphemeral)
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(Proof{
		Version: HandshakeVersion, NodeID: "node-a",
		Signature: base64.RawStdEncoding.EncodeToString(ed25519.Sign(private, payload)),
	}); err != nil {
		t.Fatal(err)
	}
	var enrolled Enrolled
	_ = decoder.Decode(&enrolled)
	wait.Wait()

	if serverErr == nil {
		t.Fatal("expected a substituted ephemeral key to fail the handshake")
	}
	if enrolled.Accepted {
		t.Fatal("server accepted a node whose ephemeral key was substituted")
	}
}

// nopConn adapts a reader and writer to net.Conn for tests that only exercise
// the record layer.
type nopConn struct {
	io.Reader
	io.Writer
}

func (nopConn) Close() error                       { return nil }
func (nopConn) LocalAddr() net.Addr                { return nil }
func (nopConn) RemoteAddr() net.Addr               { return nil }
func (nopConn) SetDeadline(t time.Time) error      { return nil }
func (nopConn) SetReadDeadline(t time.Time) error  { return nil }
func (nopConn) SetWriteDeadline(t time.Time) error { return nil }

func (c nopConn) Read(p []byte) (int, error) {
	if c.Reader == nil {
		return 0, io.EOF
	}
	return c.Reader.Read(p)
}

func (c nopConn) Write(p []byte) (int, error) {
	if c.Writer == nil {
		return len(p), nil
	}
	return c.Writer.Write(p)
}
