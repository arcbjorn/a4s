package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net"
	"testing"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// recordingConn copies everything crossing the wire so a test can assert what
// an observer on the network would actually see.
type recordingConn struct {
	net.Conn
	seen *bytes.Buffer
}

func (c recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.seen.Write(p[:n])
	}
	return n, err
}

func (c recordingConn) Write(p []byte) (int, error) {
	c.seen.Write(p)
	return c.Conn.Write(p)
}

// An observer on the network must not be able to read the action an operator
// authorized, which is the property channel encryption exists to provide.
func TestWireCarriesNoPlaintextAction(t *testing.T) {
	fixture := newEnrollFixture(t)
	defer fixture.stop()

	seen := &bytes.Buffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		var dialer net.Dialer
		raw, err := dialer.DialContext(ctx, "tcp", fixture.listener.Addr().String())
		if err != nil {
			return
		}
		defer raw.Close()
		conn := recordingConn{Conn: raw, seen: seen}

		negotiated, serverKeyID, err := connectToServer(conn, "base", fixture.nodeKey, 2*time.Second)
		if err != nil {
			return
		}
		if _, trusted := fixture.dispatcher.Keys[serverKeyID]; !trusted {
			return
		}
		if negotiated == nil {
			t.Error("no channel was negotiated")
			return
		}
		secure, err := newSecureConn(conn, negotiated.sendKey, negotiated.receiveKey, negotiated.buffered)
		if err != nil {
			return
		}
		_ = Serve(ctx, fixture.dispatcher, secure, secure)
	}()
	waitForNode(t, fixture.registry, "base")

	executor := NewRegistryExecutor(fixture.registry, "control-1", fixture.serverKey)
	executor.Bind("web-public", "placement-r0", 0, "lease-1")
	if _, err := executor.Execute(control.Action{
		ID: "create-web-0", Kind: control.ActionCreateAllocation, Target: "web-0",
		Workload: "web", Node: "base", Image: testImage,
		Resources: control.Resources{CPUMillis: 100, MemoryMB: 128},
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	wire := seen.String()
	// The handshake itself is plaintext and names the node, which is expected.
	// What must never appear is the authorized action.
	for _, secret := range []string{"create-web-0", "create_allocation", "web-0", testImage} {
		if bytes.Contains([]byte(wire), []byte(secret)) {
			t.Fatalf("action detail %q appeared in plaintext on the wire", secret)
		}
	}
}

var _ = ed25519.PublicKey{}
