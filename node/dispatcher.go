package node

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sync"
	"time"

	"github.com/arcbjorn/a4s/control"
)

type Runtime interface {
	Execute(context.Context, control.Action) (control.Evidence, error)
	Close() error
}

type DispatchResult struct {
	EnvelopeDigest string           `json:"envelope_digest"`
	Evidence       control.Evidence `json:"evidence"`
}

// DispatchResponse is the per-message reply. A rejected or failed action
// produces an error response rather than terminating the node, so one bad
// envelope cannot take the daemon down.
type DispatchResponse struct {
	EnvelopeID string          `json:"envelope_id"`
	Result     *DispatchResult `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type Ledger interface {
	Get(string) (DispatchResult, bool)
	Put(string, DispatchResult) error
}

type MemoryLedger struct {
	mu      sync.Mutex
	results map[string]DispatchResult
}

func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{results: make(map[string]DispatchResult)}
}

func (l *MemoryLedger) Get(key string) (DispatchResult, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	result, ok := l.results[key]
	return result, ok
}

func (l *MemoryLedger) Put(key string, result DispatchResult) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.results[key]; exists {
		return fmt.Errorf("idempotency key %q already stored", key)
	}
	l.results[key] = result
	return nil
}

type Dispatcher struct {
	NodeID  string
	Keys    map[string]ed25519.PublicKey
	Runtime Runtime
	Ledger  Ledger
	Now     func() time.Time
	mu      sync.Mutex
}

func (d *Dispatcher) Dispatch(ctx context.Context, signed SignedAction) (DispatchResult, error) {
	// Serialize the first implementation so a duplicate cannot race the ledger
	// check. Later, use per-target leases while retaining this guarantee.
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Runtime == nil || d.Ledger == nil || d.Now == nil {
		return DispatchResult{}, fmt.Errorf("dispatcher is not initialized")
	}
	envelope, digest, err := Verify(signed, d.Keys, d.NodeID, d.Now().UTC())
	if err != nil {
		return DispatchResult{}, err
	}
	key := envelope.IdempotencyKey
	if previous, ok := d.Ledger.Get(key); ok {
		if previous.EnvelopeDigest != digest {
			return DispatchResult{}, fmt.Errorf("idempotency key %q was reused for a different envelope", key)
		}
		return previous, nil
	}
	evidence, err := d.Runtime.Execute(ctx, envelope.Action)
	if err != nil {
		return DispatchResult{}, err
	}
	result := DispatchResult{EnvelopeDigest: digest, Evidence: evidence}
	if err := d.Ledger.Put(key, result); err != nil {
		return DispatchResult{}, err
	}
	return result, nil
}
