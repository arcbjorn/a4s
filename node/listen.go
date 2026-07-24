package node

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// NodeConnection is an enrolled node's live session. The server holds one per
// connected node and issues capabilities through it.
type NodeConnection struct {
	NodeID    string
	transport *StreamTransport
	conn      net.Conn
}

func (c *NodeConnection) Send(signed SignedAction) (DispatchResponse, error) {
	return c.transport.Send(signed)
}

func (c *NodeConnection) Close() error { return c.conn.Close() }

// Registry holds the currently connected, authenticated nodes.
//
// A node appears here only after proving possession of its enrolled key, so
// looking a node up in the registry is equivalent to having authenticated it.
type Registry struct {
	mu    sync.Mutex
	nodes map[string]*NodeConnection
}

func NewRegistry() *Registry {
	return &Registry{nodes: make(map[string]*NodeConnection)}
}

func (r *Registry) add(connection *NodeConnection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A reconnecting node replaces its prior session; the old connection is
	// closed so a stale peer cannot keep receiving capabilities.
	if existing, ok := r.nodes[connection.NodeID]; ok {
		_ = existing.Close()
	}
	r.nodes[connection.NodeID] = connection
}

// Get returns the live connection for a node, if it is currently enrolled.
func (r *Registry) Get(nodeID string) (*NodeConnection, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	connection, ok := r.nodes[nodeID]
	return connection, ok
}

// Remove drops a node's session, for example after its connection fails.
func (r *Registry) Remove(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if connection, ok := r.nodes[nodeID]; ok {
		_ = connection.Close()
		delete(r.nodes, nodeID)
	}
}

// Nodes lists the currently connected node IDs.
func (r *Registry) Nodes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.nodes))
	for id := range r.nodes {
		ids = append(ids, id)
	}
	return ids
}

func (r *Registry) CloseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, connection := range r.nodes {
		_ = connection.Close()
		delete(r.nodes, id)
	}
}

// ListenerConfig describes the server's node-facing listener.
type ListenerConfig struct {
	// NodeKeys maps node ID to the public key that node must prove it holds.
	// A node absent from this map cannot enroll.
	NodeKeys map[string]ed25519.PublicKey
	// ServerKeyID names the signing key the server will use for envelopes.
	ServerKeyID string
	// HandshakeTimeout bounds enrollment.
	HandshakeTimeout time.Duration
	// OnError reports rejected or failed connections. Enrollment failures are
	// expected in normal operation and must not stop the listener.
	OnError func(error)
	// RequireEncryption refuses any node that does not negotiate a channel.
	//
	// The default is permissive so an older node keeps working during an
	// upgrade. On an untrusted network an operator should set this, because
	// otherwise a downgrade to plaintext is available to anyone who can strip
	// the ephemeral key from a hello.
	RequireEncryption bool
}

// Listener accepts node connections, enrolls them, and registers the
// authenticated sessions.
//
// The enrollment handshake establishes who the peer is and, when both sides
// offer an ephemeral key, agrees a session key that encrypts everything after
// it. Set RequireEncryption to refuse peers that do not negotiate a channel.
type Listener struct {
	config   ListenerConfig
	registry *Registry
	listener net.Listener
}

func Listen(network, address string, registry *Registry, config ListenerConfig) (*Listener, error) {
	if config.ServerKeyID == "" {
		return nil, fmt.Errorf("listener requires a server key id")
	}
	if len(config.NodeKeys) == 0 {
		return nil, fmt.Errorf("listener requires at least one enrolled node key")
	}
	raw, err := net.Listen(network, address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s/%s: %w", network, address, err)
	}
	return &Listener{config: config, registry: registry, listener: raw}, nil
}

func (l *Listener) Addr() net.Addr { return l.listener.Addr() }

func (l *Listener) Close() error { return l.listener.Close() }

// Serve accepts connections until the listener is closed or the context ends.
func (l *Listener) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = l.listener.Close()
	}()
	for {
		conn, err := l.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept node connection: %w", err)
		}
		go l.enroll(conn)
	}
}

func (l *Listener) enroll(conn net.Conn) {
	negotiated, nodeID, err := acceptNode(conn, l.config.NodeKeys,
		l.config.ServerKeyID, l.config.HandshakeTimeout)
	if err != nil {
		// A refused connection is closed and reported, never registered.
		_ = conn.Close()
		if l.config.OnError != nil {
			l.config.OnError(err)
		}
		return
	}

	// Once a session was negotiated, every subsequent byte travels encrypted.
	// The signed envelope inside remains the authority boundary; this only
	// removes the assumption that the network path is private.
	stream := net.Conn(conn)
	if negotiated != nil {
		secure, err := newSecureConn(conn, negotiated.sendKey, negotiated.receiveKey, negotiated.buffered)
		if err != nil {
			_ = conn.Close()
			if l.config.OnError != nil {
				l.config.OnError(err)
			}
			return
		}
		stream = secure
	} else if l.config.RequireEncryption {
		// A node that offered no ephemeral key cannot be encrypted. When the
		// operator has demanded encryption, refusing is the only safe answer.
		_ = conn.Close()
		if l.config.OnError != nil {
			l.config.OnError(fmt.Errorf(
				"node %q enrolled without channel encryption, which this server requires", nodeID))
		}
		return
	}

	l.registry.add(&NodeConnection{
		NodeID: nodeID, conn: conn,
		transport: NewStreamTransport(stream, stream, conn),
	})
}

// DialServer connects to the server, proves this node's identity, and serves
// the action stream over the same connection until it closes.
//
// The node treats the server key named during enrollment as the only key whose
// envelopes it will execute, so a server that cannot be authenticated by the
// node's local trust map cannot direct it.
func DialServer(ctx context.Context, network, address string, nodeID string,
	nodeKey ed25519.PrivateKey, dispatcher *Dispatcher, timeout time.Duration) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return fmt.Errorf("dial server: %w", err)
	}
	defer conn.Close()

	negotiated, serverKeyID, err := connectToServer(conn, nodeID, nodeKey, timeout)
	if err != nil {
		return err
	}
	// The server may only name a key this node already trusts. Otherwise a
	// reachable impostor could nominate its own key and issue capabilities.
	if _, trusted := dispatcher.Keys[serverKeyID]; !trusted {
		return fmt.Errorf("server named untrusted signing key %q", serverKeyID)
	}

	stream := net.Conn(conn)
	if negotiated != nil {
		secure, err := newSecureConn(conn, negotiated.sendKey, negotiated.receiveKey, negotiated.buffered)
		if err != nil {
			return err
		}
		stream = secure
	}
	return Serve(ctx, dispatcher, stream, stream)
}

// RegistryExecutor issues capabilities to whichever enrolled node an action
// targets, so one server can drive several nodes.
type RegistryExecutor struct {
	Registry *Registry
	KeyID    string
	Key      ed25519.PrivateKey
	TTL      time.Duration
	Now      func() time.Time

	mu         sync.Mutex
	goalID     string
	proposalID string
	revision   uint64
	leaseID    string
	executors  map[string]*RemoteExecutor
}

func NewRegistryExecutor(registry *Registry, keyID string, key ed25519.PrivateKey) *RegistryExecutor {
	return &RegistryExecutor{
		Registry: registry, KeyID: keyID, Key: key,
		TTL: DefaultEnvelopeTTL, Now: time.Now,
		executors: make(map[string]*RemoteExecutor),
	}
}

func (e *RegistryExecutor) Bind(goalID, proposalID string, revision uint64, leaseID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.goalID, e.proposalID, e.revision, e.leaseID = goalID, proposalID, revision, leaseID
	for _, executor := range e.executors {
		executor.Bind(goalID, proposalID, revision, leaseID)
	}
}

func (e *RegistryExecutor) Execute(action control.Action) (control.Evidence, error) {
	nodeID := action.Node
	if nodeID == "" {
		// Actions that do not name a node act on an allocation the server has
		// already placed; without placement context there is nowhere to send it.
		return control.Evidence{}, fmt.Errorf("action %q does not name a node", action.ID)
	}
	executor, err := e.executorFor(nodeID)
	if err != nil {
		return control.Evidence{}, err
	}
	return executor.Execute(action)
}

func (e *RegistryExecutor) executorFor(nodeID string) (*RemoteExecutor, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if executor, ok := e.executors[nodeID]; ok {
		return executor, nil
	}
	connection, ok := e.Registry.Get(nodeID)
	if !ok {
		return nil, fmt.Errorf("node %q is not connected", nodeID)
	}
	executor := NewRemoteExecutor(nodeID, e.KeyID, e.Key, connection)
	executor.TTL, executor.Now = e.TTL, e.Now
	executor.Bind(e.goalID, e.proposalID, e.revision, e.leaseID)
	e.executors[nodeID] = executor
	return executor, nil
}
