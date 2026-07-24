package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"sync"
	"testing"
	"time"
)

// The handshake must complete promptly over a real socket. A blocking check in
// the key-agreement step would only show up here, because net.Pipe hides it.
func TestHandshakeCompletesPromptlyOverTCP(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	var wait sync.WaitGroup
	wait.Add(1)
	var serverSession *session
	var serverErr error
	go func() {
		defer wait.Done()
		conn, err := listener.Accept()
		if err != nil {
			serverErr = err
			return
		}
		defer conn.Close()
		serverSession, _, serverErr = acceptNode(conn,
			map[string]ed25519.PublicKey{"node-a": public}, "control-1", 10*time.Second)
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	start := time.Now()
	nodeSession, keyID, err := connectToServer(conn, "node-a", private, 10*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	wait.Wait()
	if serverErr != nil {
		t.Fatalf("server: %v", serverErr)
	}
	if keyID != "control-1" {
		t.Fatalf("key id = %q", keyID)
	}
	if nodeSession == nil || serverSession == nil {
		t.Fatal("no session negotiated")
	}
	// A blocking read would push this to the handshake timeout.
	if elapsed > time.Second {
		t.Fatalf("handshake took %v, expected well under a second", elapsed)
	}
	t.Logf("handshake completed in %v", elapsed)
}
