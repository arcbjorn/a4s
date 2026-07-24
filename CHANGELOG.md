# Changelog

All notable project changes should be recorded here. The project has no stable
release or compatibility guarantee yet.

## Unreleased

### Added

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
