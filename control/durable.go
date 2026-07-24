package control

import (
	"fmt"
	"sync"
)

// EvidenceSource replays recorded evidence in the order it was observed. The
// event log implements it, which is what lets a restarted server rebuild
// authoritative state instead of trusting an in-memory cache.
type EvidenceSource interface {
	ReplayEvidence() ([]Evidence, error)
}

// DurableProjector rebuilds the world from recorded evidence and keeps it
// current as new evidence arrives. It is the server-side counterpart to
// MemoryExecutor's in-process projection.
//
// The base world holds facts that are not derived from evidence: node
// inventory, capacity, and operator approvals. Everything else, including every
// allocation and route, is replayed. A projection that cannot be rebuilt from
// the log is a projection the server cannot recover after a crash.
type DurableProjector struct {
	mu    sync.Mutex
	base  World
	world World
}

// NewDurableProjector rebuilds a world by replaying every recorded piece of
// evidence over the supplied base. Replay failure is fatal rather than
// tolerated: silently discarding evidence would produce a world that disagrees
// with the durable history it claims to summarize.
func NewDurableProjector(base World, source EvidenceSource) (*DurableProjector, error) {
	base.normalize()
	projector := &DurableProjector{base: cloneWorld(base), world: cloneWorld(base)}
	if source == nil {
		return projector, nil
	}
	evidence, err := source.ReplayEvidence()
	if err != nil {
		return nil, fmt.Errorf("replay evidence: %w", err)
	}
	for index, item := range evidence {
		next, err := Project(projector.world, item)
		if err != nil {
			return nil, fmt.Errorf("replay evidence %d (%s for %q): %w", index+1, item.Kind, item.Target, err)
		}
		projector.world = next
	}
	return projector, nil
}

func (p *DurableProjector) World() World {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneWorld(p.world)
}

func (p *DurableProjector) Project(evidence Evidence) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	next, err := Project(p.world, evidence)
	if err != nil {
		return err
	}
	p.world = next
	return nil
}

// Rebuild recomputes the world from the base and the supplied evidence. It is
// the recovery path used after restart and the check that the projection really
// is a pure function of recorded history.
func (p *DurableProjector) Rebuild(source EvidenceSource) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	rebuilt := cloneWorld(p.base)
	if source != nil {
		evidence, err := source.ReplayEvidence()
		if err != nil {
			return fmt.Errorf("replay evidence: %w", err)
		}
		for index, item := range evidence {
			next, err := Project(rebuilt, item)
			if err != nil {
				return fmt.Errorf("replay evidence %d (%s for %q): %w", index+1, item.Kind, item.Target, err)
			}
			rebuilt = next
		}
	}
	p.world = rebuilt
	return nil
}
