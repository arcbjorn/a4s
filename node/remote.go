package node

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arcbjorn/a4s/control"
)

// DefaultEnvelopeTTL is how long an issued envelope remains valid. It is short
// so a captured envelope has little replay value, and it is well under the
// five-minute ceiling the node enforces.
const DefaultEnvelopeTTL = 60 * time.Second

// Transport carries signed actions to a node and returns its responses. It is
// deliberately a byte-stream contract so the same executor works over a pipe, a
// subprocess, or a network connection over the tailnet.
type Transport interface {
	Send(SignedAction) (DispatchResponse, error)
	Close() error
}

// RemoteExecutor implements control.Executor by issuing signed envelopes to a
// node and returning the evidence the node reports.
//
// This is the seam that separates the control plane from the data plane: the
// kernel authorizes, the executor issues a narrow signed capability, and the
// node performs the mutation. The server never touches containerd itself.
type RemoteExecutor struct {
	NodeID     string
	KeyID      string
	PrivateKey ed25519.PrivateKey
	Transport  Transport
	TTL        time.Duration
	Now        func() time.Time
	// GoalID and ProposalID identify the authorization this action belongs to.
	// They are set by the caller before dispatching a proposal's actions.
	GoalID     string
	ProposalID string
	Revision   uint64
	LeaseID    string

	sequence atomic.Uint64
	mu       sync.Mutex
}

func NewRemoteExecutor(nodeID, keyID string, key ed25519.PrivateKey, transport Transport) *RemoteExecutor {
	return &RemoteExecutor{
		NodeID: nodeID, KeyID: keyID, PrivateKey: key,
		Transport: transport, TTL: DefaultEnvelopeTTL, Now: time.Now,
	}
}

// Bind records which authorization the following actions belong to. The kernel
// approves a whole proposal, so every envelope it produces carries that
// proposal's identity and the revision it was authorized against.
func (e *RemoteExecutor) Bind(goalID, proposalID string, revision uint64, leaseID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.GoalID, e.ProposalID, e.Revision, e.LeaseID = goalID, proposalID, revision, leaseID
}

func (e *RemoteExecutor) Execute(action control.Action) (control.Evidence, error) {
	e.mu.Lock()
	goalID, proposalID, revision, leaseID := e.GoalID, e.ProposalID, e.Revision, e.LeaseID
	e.mu.Unlock()
	if e.Transport == nil {
		return control.Evidence{}, fmt.Errorf("remote executor has no transport")
	}
	if goalID == "" || proposalID == "" {
		return control.Evidence{}, fmt.Errorf("remote executor was not bound to an authorized proposal")
	}

	now := e.now()
	ttl := e.TTL
	if ttl <= 0 {
		ttl = DefaultEnvelopeTTL
	}
	if leaseID == "" {
		leaseID = proposalID
	}
	envelope := ActionEnvelope{
		Version: EnvelopeVersion,
		// The envelope ID is unique per issuance; the idempotency key is not.
		ID:     fmt.Sprintf("%s-%d", proposalID, e.sequence.Add(1)),
		NodeID: e.NodeID, GoalID: goalID, ProposalID: proposalID,
		WorldRevision: revision, LeaseID: leaseID,
		// The idempotency key is derived from the authorization, not the
		// attempt, so a retried action is recognized as the same work.
		IdempotencyKey: fmt.Sprintf("%s/%s", proposalID, action.ID),
		IssuedAt:       now, ExpiresAt: now.Add(ttl),
		Action: action,
	}
	signed, err := Sign(envelope, e.KeyID, e.PrivateKey)
	if err != nil {
		return control.Evidence{}, fmt.Errorf("sign action %q: %w", action.ID, err)
	}
	response, err := e.Transport.Send(signed)
	if err != nil {
		return control.Evidence{}, fmt.Errorf("dispatch action %q: %w", action.ID, err)
	}
	if response.Error != "" {
		return control.Evidence{}, fmt.Errorf("node rejected action %q: %s", action.ID, response.Error)
	}
	if response.Result == nil {
		return control.Evidence{}, fmt.Errorf("node returned no result for action %q", action.ID)
	}
	return response.Result.Evidence, nil
}

func (e *RemoteExecutor) Close() error {
	if e.Transport == nil {
		return nil
	}
	return e.Transport.Close()
}

func (e *RemoteExecutor) now() time.Time {
	if e.Now == nil {
		return time.Now().UTC()
	}
	return e.Now().UTC()
}

// StreamTransport speaks the node's newline-delimited JSON protocol over any
// reader/writer pair. The same framing works across a pipe to a subprocess and
// a connection over the tailnet, so the transport can change without the
// control plane noticing.
type StreamTransport struct {
	mu      sync.Mutex
	writer  io.Writer
	decoder *json.Decoder
	closer  io.Closer
}

func NewStreamTransport(writer io.Writer, reader io.Reader, closer io.Closer) *StreamTransport {
	decoder := json.NewDecoder(bufio.NewReader(reader))
	decoder.DisallowUnknownFields()
	return &StreamTransport{writer: writer, decoder: decoder, closer: closer}
}

func (t *StreamTransport) Send(signed SignedAction) (DispatchResponse, error) {
	// Serialize request/response pairs: the protocol is a synchronous stream,
	// so concurrent senders would interleave and mismatch replies.
	t.mu.Lock()
	defer t.mu.Unlock()
	payload, err := json.Marshal(signed)
	if err != nil {
		return DispatchResponse{}, fmt.Errorf("encode signed action: %w", err)
	}
	if _, err := t.writer.Write(append(payload, '\n')); err != nil {
		return DispatchResponse{}, fmt.Errorf("write signed action: %w", err)
	}
	if flusher, ok := t.writer.(interface{ Flush() error }); ok {
		if err := flusher.Flush(); err != nil {
			return DispatchResponse{}, fmt.Errorf("flush signed action: %w", err)
		}
	}
	var response DispatchResponse
	if err := t.decoder.Decode(&response); err != nil {
		return DispatchResponse{}, fmt.Errorf("read dispatch response: %w", err)
	}
	return response, nil
}

func (t *StreamTransport) Close() error {
	if t.closer == nil {
		return nil
	}
	return t.closer.Close()
}

// Serve reads signed actions from a stream and writes one response per message.
// It is the node side of the same protocol and never terminates because of a
// rejected or failed action.
func Serve(ctx context.Context, dispatcher *Dispatcher, reader io.Reader, writer io.Writer) error {
	decoder := json.NewDecoder(bufio.NewReader(reader))
	decoder.DisallowUnknownFields()
	encoder := json.NewEncoder(writer)
	for {
		var signed SignedAction
		if err := decoder.Decode(&signed); err != nil {
			if err == io.EOF {
				return nil
			}
			// A malformed frame desynchronizes the stream; the reader cannot
			// know where the next envelope begins.
			return fmt.Errorf("decode signed action: %w", err)
		}
		response := DispatchResponse{EnvelopeID: signed.Envelope().ID}
		result, err := dispatcher.Dispatch(ctx, signed)
		if err != nil {
			response.Error = err.Error()
		} else {
			response.Result = &result
		}
		if err := encoder.Encode(response); err != nil {
			return fmt.Errorf("encode dispatch response: %w", err)
		}
	}
}
