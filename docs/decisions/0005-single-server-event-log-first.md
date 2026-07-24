# ADR 0005: Single-server event log first

- Status: Accepted
- Date: 2026-07-22

## Context

Consensus can improve control-plane availability, but it also adds membership,
quorum, split-brain, snapshot, upgrade, and recovery complexity. The initial
target is a small environment where workloads should continue during a control
server outage.

The project first needs correct event ordering, materialized-state rebuilding,
action recovery, backup, and restore on one server.

## Decision

Build a single-server append-only event architecture first. The spike uses a
hash-chained JSONL file. The next server milestone may use embedded SQLite WAL
with rebuildable projections. Nodes continue last accepted allocations when the
server is unavailable and reject expired new actions.

Do not introduce Raft or another consensus layer until single-server recovery
is proven and measured availability requirements justify it.

## Consequences

Benefits:

- Backup, restore, and action recovery remain understandable.
- Event semantics stabilize before replication semantics.
- Smaller bootstrap and operational footprint.

Costs:

- New control decisions pause when the single server is unavailable.
- Operator API availability is limited by one server.
- Audit anchoring and off-host backup are required to protect history.

## Revisit when

Revisit after the single-server implementation survives tested crash recovery,
backup restore, and node reconnection, and when a concrete recovery-time or
availability target cannot be met by fast restart on another host.
