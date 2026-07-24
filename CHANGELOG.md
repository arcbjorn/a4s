# Changelog

All notable project changes should be recorded here. The project has no stable
release or compatibility guarantee yet.

## Unreleased

### Added

- `stop_allocation` and `delete_allocation` actions with kill deadline,
  snapshot cleanup, and capacity release.
- Real process, TCP, and HTTP readiness probes (`node.RuntimeObserver`).
- Observation freshness: readiness evidence expires and stale readiness no
  longer satisfies a goal.
- Durable world projection rebuilt from recorded evidence
  (`control.DurableProjector`), so a restarted server recovers its state.
- Node desired-state cache and supervisor with crash-loop budget, exponential
  backoff, and orphan discovery, so workloads survive a server outage.
- Controller-to-node transport: `RemoteExecutor` issues signed capabilities and
  `Serve` handles them, replacing the ad-hoc stdin harness.
- Router capability with atomic gateway route snapshots and rollback on a
  failed apply.
- End-to-end acceptance suite covering convergence over the transport, server
  restart recovery, node survival during an outage, replay after node restart,
  and failed readiness blocking a goal.
- Pure, idempotent world projection from evidence (`control.Project`).
- `Prober` interface and `OptimisticProber` stand-in separating readiness
  observation from action execution.
- `observation.recorded` event for independently produced probe evidence.
- Per-message node dispatch responses.

### Changed

- The world advances only by projecting evidence. `Executor` no longer owns
  world state; `WorldSource` and `Projector` supply it.
- Node envelopes are verified over the exact transmitted bytes and decoded only
  after the signature checks, removing the dependency on encoder stability.
  Unknown fields and trailing content are rejected.
- The node reports rejected and failed envelopes per message instead of exiting.

### Fixed

- Node deduplication compared whole envelopes, so a legitimate retry with fresh
  issue and expiry times was rejected as idempotency-key reuse. The ledger now
  compares a digest of the authorized work instead.
- Placement proposals accumulated readiness checks instead of overwriting them.
  Previously only the last replica's evidence declaration survived, which would
  have silently dropped evidence requirements once multi-replica placement was
  enabled.
- Replayed allocation evidence no longer double-counts node capacity.

## 0.2.0-dev - 2026-07-22

### Added

- v1alpha1 goal, world, proposal, action, evidence, and event vocabulary.
- Deterministic placement and network agents.
- Deterministic proposal authorization and complete-plan simulation.
- Memory executor and end-to-end reconciliation simulation.
- Hash-chained durable event file.
- Ed25519-signed node action envelopes.
- Persistent node idempotency ledger.
- Runtime-neutral container backend contract.
- Linux containerd v2 pull, create, and start adapter.
- OCI CPU, memory, PID, capability, privilege, and cgroup baseline.
- Comprehensive portable project handbook and decision records.

### Known limitations

- No live server or controller-to-node transport.
- Linux adapter has cross-built but not run against a live containerd in this
  project history.
- Networking, storage, secrets, probes, rollback, and complete lifecycle are
  not implemented.

## 0.1.0-dev

### Added

- Initial architecture and control-kernel experiment.
