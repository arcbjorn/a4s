# Changelog

All notable project changes should be recorded here. The project has no stable
release or compatibility guarantee yet.

## Unreleased

### Added

- Agent workloads as a workload kind. A workload may declare a `runtime` block
  the way a database declares an `engine`, which makes its cost measured in
  tokens rather than cpu-seconds, its readiness a question of provider reach and
  remaining budget, and its blast radius a declared tool envelope. An agent
  workload is not a control agent: it proposes nothing, holds no `ActionKind`
  grants, and the two grant vocabularies are deliberately disjoint.
- `Budget` as a resource dimension beside cpu and memory, with per-node
  `budget_capacity` and `budget_used`. A cpu limit bounds how fast an agent burns
  money, not how much, so budget is committed at placement the way memory is.
  Every ceiling must be positive: a zero is rejected rather than read as
  unlimited, so a forgotten field cannot grant infinite spend.
- `grant_tools`, which installs a scoped tool envelope while the allocation is
  still in `created` phase. This is what lets the kernel authorize an agent up
  front without knowing what it will decide to do: the envelope is checked
  before it starts and cannot be widened at runtime. Mutating grants require a
  separate `agent-mutating-tools` approval, since they change state outside a4s
  where no compensation or event log reaches.
- `drain_allocation` and drain-before-stop retirement. An agent instance
  accumulates task context a stateless replica does not, so stopping it mid-task
  destroys work already paid for in tokens. The kernel refuses to stop an agent
  holding a task until it is observed holding nothing, borrowing the shape of
  the volume handoff. An exhausted agent is exempt, since waiting for it would
  never end.
- Provider reachability as a node fact and a hard placement constraint. An agent
  placed where its provider is unreachable can never become ready, so it is
  refused at placement rather than discovered at probe time.
- An `agent` probe and `agent_ready` check. A process probe would pass for an
  agent whose provider is unreachable or whose ceiling is spent, both of which
  mean no work can be done despite a healthy-looking container.
- Monotonic `agent.spent` evidence. A lower reading is ignored as stale, because
  accepting it would let an exhausted agent look affordable again and be
  restarted into the same ceiling. An exhausted agent stops being ready, stops
  counting toward goal satisfaction, and is treated as drifted so it can be
  replaced.
- Work queues and demand-driven scaling between the goal's replica count and the
  queue's `max_workers`. The kernel recomputes that ceiling itself rather than
  trusting the placement agent's arithmetic, and depth older than 60 seconds
  does not scale, since workers consume the depth as it is read.
- A node agent capability holding tool envelopes strictly per allocation. Context
  leakage between agents comes from shared runtime state, not a shared kernel
  namespace, so envelopes, workspaces, and credentials are per instance and a
  deleted allocation's envelope is released.
- A durable node work queue. `Queue.Depth` drove agent scaling but nothing
  measured it, and `HoldTask`/`ReleaseTask` were never called, so a drain always
  observed an empty task slot and could stop an instance mid-work. Delivery now
  lives on the node, durably, and depth is measured on the supervision tick.
- Leased claims with bounded redelivery. An instance that dies holding a task
  would otherwise strand it forever, since the control plane cannot redeliver
  work whose contents it never sees. The lease is longer than a typical task
  because reclaiming from a merely slow instance means paying twice and possibly
  acting twice. A task that exhausts its attempts stops being delivered, stops
  counting toward depth, and is reported as stalled.
- `QueueBroker`, the seam between the queue and the agent lifecycle. An instance
  may claim only if it is metered, funded, not draining, and not already holding
  work. Authorization runs before the queue is touched, so a refused claim does
  not consume an attempt on a task the instance was never eligible for.
- Draining is now sticky on the node. An instance that finished its task and
  claimed another would never drain, stalling the rollout waiting on it.
- Deleting an allocation returns the work it held, rather than leaving a task
  undelivered until its lease lapses.
- Measured provider reachability. `Node.Providers` was read by the scheduler but
  never written, so in a real deployment every agent placement failed with
  "cannot reach provider". A node-side monitor now measures egress on a timer and
  reports `provider.reachable`.
- Provider facts are measurements rather than flags. Egress does not stay true
  once observed the way a pulled image does: a route, a credential, or an outage
  removes it between placements. Each entry carries an expiry, and `CanReach`
  treats unmeasured, unreachable, and expired identically, because the scheduler
  needs positive current evidence rather than an absence of bad news.
- The monitor fails closed. A timeout, a refused connection, and a 5xx all read
  as unreachable; a 401 does not, since the question is whether the node has a
  working path and treating an auth failure as a network failure would misreport
  a credential problem. The projection also refuses an observation older than the
  one it holds, so a stale success cannot overwrite a fresh failure.
- Node-side budget enforcement. The kernel can authorize a ceiling but cannot
  enforce one: a round trip through evidence, projection, and a proposal takes
  longer than an agent needs to spend everything it has left. The node now holds
  a per-allocation meter, reserved from the authorized action rather than from
  anything the runtime claims.
- A tool-call gate. `AuthorizeToolCall` refuses capabilities outside the
  envelope, refuses everything once an instance is exhausted, and charges the
  tool-call ceiling on success, which is what stops an agent thrashing between
  two granted tools while staying cheap on every other dimension. Refusals are
  counted and reported, since an agent repeatedly reaching for a capability it
  lacks is a fact an operator should see.
- `Budget.Exhausts`, distinct from `Fits`. Reservation is inclusive: committing
  exactly the remaining capacity is legitimate. Consumption is not: an instance
  that spent exactly its ceiling has nothing left. Using `!Fits` for consumption
  granted one extra unit on every dimension to every agent.
- An agent readiness probe measuring provider reachability, remaining budget,
  and container liveness, plus a composite observer routing each probe kind to
  the capability that owns it. Agent workloads previously failed readiness on a
  real node with "unsupported probe kind".
- Supervisor-reported spend, including for stopped allocations, and refusal to
  restart an exhausted agent. An agent that spent its ceiling did not crash, it
  finished; restarting it would burn a fresh ceiling to reach the same state.
- `examples/agent-workload.json` and [agent workloads](docs/agent-workloads.md).

- Database workloads: a workload may declare an `engine` (postgres), which makes
  it single-writer, volume-backed, and readiness-checked by an accepted
  connection rather than an open port.
- `database_backup`, which invokes the engine's own consistent-backup tool
  (pg_basebackup) against the live database. A raw filesystem snapshot of a
  running database is now refused, since its files are torn when copied.
- A PostgreSQL engine backend: pg_basebackup for backups and a real connection
  for readiness, so a database still replaying its WAL is not reported ready.
- Database backups are first-class recovery points: verifiable, prunable, and
  restorable like any other snapshot.

- Scheduled restore verification: a `verify_backup` action restores a snapshot
  into scratch space, checksums it, and discards it, proving a backup is
  recoverable without touching the live volume. A restore test that could damage
  the data it protects would be worse than none.
- A storage agent that proposes verifying the least recently checked backup once
  its verification ages past an interval, and records when each backup was last
  proven recoverable.
- `StaleBackups`, reporting volumes overdue for verification, so an operator can
  see their recovery posture rather than assuming it.

- Snapshot retention and a `prune_snapshots` action. Pruning keeps the most
  recent N snapshots and never removes the last-known-good, a backed-up
  snapshot, or the last one standing, so a prune cannot leave a volume with no
  recovery point.
- Dry-run pruning that reports exactly what it would remove without touching the
  disk, and matches the subsequent real prune.
- Snapshot ordering on volumes, so pruning has a deterministic notion of oldest.

- Node-side volume transfer: the origin ships a verified snapshot through the
  shared store, the target fetches it and proves receipt by reproducing the
  checksum, and adoption materializes the data on the target. The origin keeps
  every byte until adoption, so a stalled move leaves the data where it was.

- Cross-node volume handoff following the prescribed sequence: quiesce, verified
  snapshot, transfer, and explicit adoption. Each phase is entered only on
  evidence from the previous one, and none can be skipped.
- A volume mid-move cannot be attached and its workload cannot be placed, so no
  writer can diverge from what is being transferred.
- Adoption advances the volume generation, fencing any writer still holding the
  origin node's view.
- Moving a volume requires a granted `move-volume` approval.

- Off-host backup: `backup_snapshot` ships a verified snapshot to a store
  outside the node, and restore falls back to it when the node's local snapshot
  is gone. That is the host-loss case backups exist for.
- `DirectoryBackupStore`, which refuses a path inside the volume root because a
  backup on the same disk as its data does not survive that disk.
- Restore evidence records whether recovery came from a local snapshot or the
  backup store, which is what an operator needs to know after an incident.

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
