package server

import (
	"fmt"

	"github.com/arcbjorn/a4s/control"
	"github.com/arcbjorn/a4s/eventlog"
)

// A standby that refuses to take authority it cannot prove it holds.
//
// One server owns the event log, and that was a deliberate choice: decision
// record 0005 chose a single writer and durable history over consensus, because
// consensus is a large amount of machinery to get wrong and the failure it
// prevents is not the one a small cluster actually hits. Losing the server does
// not stop workloads; nodes keep running what they were told to run. What is
// lost is the ability to change anything, and that is what this closes.
//
// It is not consensus and does not pretend to be. There is no election and no
// quorum: promotion is an operator's decision, or an external supervisor's. What
// this adds is the check that makes such a decision safe to make, which is the
// part that is easy to get wrong by hand. A standby that had fallen behind and
// was promoted anyway would silently roll the cluster's history backwards,
// resurrecting approvals that were revoked and forgetting allocations that
// exist. The anchor already witnesses the true head outside any single store,
// so the standby can be made to prove it is not behind before it is allowed to
// act.
type Standby struct {
	config Config
	log    *eventlog.File
	anchor *eventlog.Anchor
}

// OpenStandby opens a follower over its own copy of the event log.
//
// The anchor is required rather than optional. Without an outside witness the
// standby has no way to tell "I am fully caught up" from "I am missing the last
// hour", because both look like a valid chain that simply ends where it ends.
// Promotion would then be a guess, and this type exists to remove the guess.
//
// The config must carry the same Base world and operator keys as the primary.
// Base holds what the log cannot: node inventory, capacity, and any approvals
// granted outside recorded history. Replication reproduces the log and nothing
// else, so a follower configured with an empty Base promotes successfully and
// comes up missing exactly those facts, which is a quiet failure rather than a
// loud one. There is no way to check this from here, because the follower has
// never seen the primary's configuration; it is the one part of standby setup
// that has to be got right by whoever deploys it.
func OpenStandby(config Config) (*Standby, error) {
	if config.EventLog == "" {
		return nil, fmt.Errorf("standby requires an event log path")
	}
	if config.Anchor == "" {
		return nil, fmt.Errorf(
			"standby requires an anchor: without one it cannot prove it is caught up")
	}
	log, err := eventlog.Open(config.EventLog)
	if err != nil {
		return nil, err
	}
	anchor, err := eventlog.OpenAnchor(config.Anchor)
	if err != nil {
		log.Close()
		return nil, err
	}
	return &Standby{config: config, log: log, anchor: anchor}, nil
}

func (s *Standby) Close() error {
	if s.log == nil {
		return nil
	}
	err := s.log.Close()
	s.log = nil
	return err
}

// Ingest applies records shipped from the primary.
//
// The transport is the caller's problem on purpose. Records are ordinary values,
// so a deployment can ship them over the operator API, a file copy, or a shared
// filesystem without this package growing a replication protocol it would then
// have to keep secure. What this owns is the part that must not be reinvented
// per transport: every record is re-derived against the follower's own chain, so
// a divergence is refused here rather than discovered at promotion.
func (s *Standby) Ingest(records []eventlog.Record) (int, error) {
	if s.log == nil {
		return 0, fmt.Errorf("standby is closed")
	}
	appended, err := s.log.Ingest(records)
	if err != nil {
		return appended, err
	}
	// Witness what was accepted. A standby that never anchored would be unable
	// to detect its own replacement, which is the same gap the primary's anchor
	// closes for the primary.
	if head := s.log.Head(); head.Hash != "" {
		if err := s.anchor.Witness(head.Sequence, head.Hash); err != nil {
			return appended, fmt.Errorf("witness ingested head: %w", err)
		}
	}
	return appended, nil
}

// Head reports the follower's current chain tip.
func (s *Standby) Head() eventlog.Record {
	if s.log == nil {
		return eventlog.Record{}
	}
	return s.log.Head()
}

// Behind reports how many records the follower is missing relative to a head it
// has been told about. Zero means caught up as far as it knows.
func (s *Standby) Behind(primary eventlog.Record) uint64 {
	head := s.Head()
	if primary.Sequence <= head.Sequence {
		return 0
	}
	return primary.Sequence - head.Sequence
}

// Promote turns the follower into a live server, or refuses and says why.
//
// Three checks, and each rejects a different way a promotion goes wrong. The
// chain must verify, or the standby would take over history it cannot prove is
// the history a4s wrote. It must not be behind the witnessed head, which is the
// check that catches a follower promoted mid-replication and is the reason the
// anchor is mandatory here. And the returned server is opened through the
// ordinary path, so recovery, verification, and re-anchoring all run exactly as
// they do on a normal start rather than through a promotion-only shortcut that
// nothing else exercises.
func (s *Standby) Promote(agents ...control.Agent) (*Server, error) {
	if s.log == nil {
		return nil, fmt.Errorf("standby is closed")
	}
	if err := s.log.Verify(); err != nil {
		return nil, fmt.Errorf("standby refuses to promote: %w", err)
	}
	witnessed := s.anchor.Last()
	head := s.log.Head()
	if head.Sequence < witnessed.Sequence {
		return nil, fmt.Errorf(
			"standby refuses to promote: it holds %d records but %d were witnessed; it is behind the primary",
			head.Sequence, witnessed.Sequence)
	}
	if err := s.anchor.Check(s.log); err != nil {
		return nil, fmt.Errorf("standby refuses to promote: %w", err)
	}

	// Released before reopening: two handles on one SQLite log would contend,
	// and the promoted server must own it outright.
	if err := s.log.Close(); err != nil {
		return nil, fmt.Errorf("release standby log: %w", err)
	}
	s.log = nil

	server, err := Open(s.config, agents...)
	if err != nil {
		return nil, fmt.Errorf("promote standby: %w", err)
	}
	return server, nil
}
