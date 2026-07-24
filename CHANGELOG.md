# Changelog

All notable project changes should be recorded here. The project has no stable
release or compatibility guarantee yet.

## Unreleased

### Added

- Volume snapshots: checksummed, immutable copies of a quiesced volume, staged
  and renamed so an interrupted run never leaves a partial tree under a name
  that looks complete.
- `restore_snapshot`, which verifies the recorded checksum before overwriting
  anything and stages the restored copy before swapping it in.
- A `storage-agent` grant covering snapshot and restore but not placement or
  execution, keeping backup authority separate from execution authority.
- Restore requires a granted `restore-volume` approval, and only a snapshot this
  cluster took and verified may be restored.

- Volumes: explicit durable objects with node affinity, single-writer ownership,
  and a generation counter that fences a writer superseded while unreachable.
- `create_volume`, `attach_volume`, `detach_volume`, and `snapshot_volume`
  actions with matching evidence.
- Node volume manager with ownership records that survive node restart, so a
  restarted node still refuses a second writer.
- Stateful workloads are now accepted, replacing the blanket rejection. A
  workload declaring volumes is pinned to the node holding its data and limited
  to one replica.
- Kernel rules: a volume cannot be attached to two allocations, attached across
  nodes, detached from a running writer, or deleted while still held. Destroying
  a stateful allocation requires a separately authenticated approval.

- Secrets: opaque `SecretRef` (name, version, mount path) on workloads, a
  `mount_secret` action, and `secret.mounted` evidence carrying only the
  version. `SecretRef` has no field capable of holding a value.
- Node secret broker sealing material to a node's identity with X25519 and
  ChaCha20-Poly1305, decrypting into a tmpfs mount bound read-only into the
  container. Vault or another backend satisfies the same interface.
- `SecretMaterial`, which refuses to serialize and renders as `[redacted]` under
  every formatting verb, so material cannot reach a log line or the event log.
- `a4s seal` for sealing material to a node, reading from a file rather than an
  argument so it never appears in shell history.
- Redaction tests that run a real reconciliation and scan every serialized
  artifact — goal, world, events, plan, explanation, diagnosis — for the value.
- Kernel rules: a workload cannot start before its declared secrets are mounted,
  and an agent cannot mount material the goal did not declare.

- Service discovery: a directory mapping workload names to the endpoints
  observed serving them. Derived from verified evidence only, so an allocation
  appears solely when it has an address and unexpired readiness.
- Caddy gateway backend applying whole route snapshots through the admin API,
  with native ACME for public routes and internal issuance for tailnet-only
  ones. This replaces ingress and cert-manager with one component.
- Route snapshots resolving each route to its serving endpoints, so the gateway
  proxies to real allocation addresses.

- Per-allocation networking: an `attach_network` action, a CNI backend invoking
  the standard bridge/host-local plugins, node-local IPAM, and containers that
  join the namespace CNI created for them. Each allocation now has its own
  address and namespace.
- Multi-replica placement, batched at two replicas per proposal to keep blast
  radius and the action budget bounded.
- `network.attached` and `network.detached` evidence, with the address recorded
  on the allocation.

- `a4s explain`: reconstructs why an allocation or route exists from the
  hash-chained log, including the agent's reasoning, the kernel's
  authorization, and the probe evidence that proved the outcome. An action
  dispatched without a completion reads as pending, making the crash window
  visible.
- `a4s plan`: dry-run reconciliation against the real world projection. It runs
  the real agents and the real kernel over a cloned world, mutating nothing, and
  marks steps contingent on readiness that simulation cannot measure.
- `a4s diagnose` and `control.Diagnoser`: synthesizes why a goal is not
  converging and suggests a next step. The diagnoser holds no capability grants
  and proposes no actions, so it is where model-backed reasoning can be
  substituted without granting new authority.
- `Target` and `Kind` on control events, recorded at dispatch, so an action that
  never completed can still be attributed to what it was about.

- Node enrollment: a challenge-response handshake in which a node proves
  possession of its enrolled Ed25519 key before the server issues any
  capability. Refusals are generic on the wire so node identities cannot be
  enumerated, while the server log records the real cause.
- Node-facing TCP listener, connection registry, and `RegistryExecutor` that
  routes each capability to the node an action names.
- `a4s keygen` for generating Ed25519 keypairs with restrictive permissions,
  and `a4s server --listen` / `a4s node --server` for real network operation.
- Known-good image tracking recorded from readiness evidence, so a rollback
  target is always a version the cluster actually observed serving.
- `RollbackRequired`, raised when a replacement is observed failing. The goal
  blocks and names the known-good digest rather than an agent silently running
  a version the operator did not request.
- Target leases (`control.LeaseManager`) enforced before the first mutation, so
  two proposals built against the same revision cannot interleave on one
  allocation. Leases expire, so an abandoned holder does not block a target.
- Node-side lease backstop rejecting an envelope that contradicts a live claim.
- Rollout agent that retires drifted allocations one at a time within an
  availability budget, with placement creating the replacements.
- Kernel-enforced disruption floor, so an agent cannot exceed the availability
  budget by proposing the stop anyway.
- Long-running server package and `a4s server` command holding durable history,
  a projection rebuilt from it on every start, and goal admission.
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
- `Prober` interface separating readiness observation from action execution.
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

- A route with no healthy endpoint is now answered with 503 rather than being
  dropped from the gateway. Dropping it let the hostname fall through to an
  unrelated site, which is worse than an honest error.

- Replicas of one workload could not share a node: containers ran on the host
  network and contended for the same port. Every allocation now has its own
  network namespace and address.
- Readiness probes fell back to dialing loopback, which could report a dead
  workload healthy because an unrelated process held the port, and could not
  distinguish replicas. The probe now targets the allocation's own address and
  refuses to guess when it has none.

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

- No live server or controller-to-node transport at the time of this release.
- Linux adapter has cross-built but not run against a live containerd in this
  project history.
- Networking, storage, secrets, probes, rollback, and complete lifecycle are
  not implemented.

## 0.1.0-dev

### Added

- Initial architecture and control-kernel experiment.
