# Project status

Status date: 2026-07-28

Version: `0.2.0-dev`

Maturity: complete vertical slice verified on real hardware; not yet a
production orchestrator. See "required before production" in
[security](security.md).

## Executive status

a4s currently proves two boundaries:

1. A deterministic agentic control loop can turn an outcome-oriented goal into
   bounded proposals, authorize the complete proposal against observed state,
   execute typed actions in a simulated world, and verify convergence.
2. A Linux node can accept a short-lived signed action envelope, deduplicate it
   durably, and translate the container actions into containerd API calls
   through a narrow runtime contract.

Those boundaries are connected. A `RemoteExecutor` issues signed capabilities for
an authorized proposal, a node dispatches them, and the returned evidence
advances a world projection rebuilt from the durable event log. The end-to-end
acceptance suite drives a real control engine against a real node dispatcher over
the real protocol, with only containerd faked.

`a4s server` holds durable history and rebuilds the world projection on every
start. Nodes enroll by proving possession of their identity key, and the session
that follows is encrypted with keys agreed inside the signed handshake, so the
transport does not assume a private network beneath it. An authenticated operator
API accepts goals, approvals, and queries over HTTP, so a goal reaches a running
control plane without a scenario file.

The round trip has been verified against live containerd on linux/amd64 and
linux/arm64, including allocation networking, the gateway, and durable volumes.
Evidence is signed by the node that measured it, the audit chain is anchored
outside its own store, containers run under a seccomp profile, and both daemons
ship as hardened service units.

What remains before production is narrower than the platform: the gateway does
not verify snapshot provenance independently, secret rotation still replaces the
allocation, containers run as the image's own user unless an operator pins one,
and nothing has been through sustained-failure or penetration testing. See
"required before production" in [security](security.md).

## Implemented

### Control kernel

- `a4s.io/v1alpha1` goal and observed-world types.
- Scenario normalization and strict JSON decoding.
- Deterministic placement and network agents.
- Agent identity checked against the authenticated descriptor supplied to the
  kernel.
- Per-agent action capability grants.
- Proposal action-count limit.
- World-revision binding and stale-proposal rejection.
- Ordered action dependencies.
- Whole-proposal simulation against a cloned world before mutation.
- Node health, placement-label, allowed-node, resource-capacity, replica-index,
  and image policy.
- Digest-pinned image requirement.
- Privileged workload rejection in v1alpha1.
- Database workloads with engine-consistent backup, connection-based readiness,
  and a raw snapshot of a running database refused.
- Agent workloads as a workload kind: pinned model and runtime, mandatory
  positive budget ceilings, scoped tool envelopes granted before start, mutating
  grants behind a separate approval, provider reachability and budget capacity
  as placement constraints, provider-and-budget readiness, monotonic spend
  evidence, drain-before-stop retirement, and queue-depth scaling capped by a
  worker ceiling the kernel recomputes.
- Node-side budget enforcement: a per-allocation meter reserved from the
  authorized action, a tool-call gate that refuses ungranted capabilities and
  charges the tool-call ceiling, supervisor-reported spend, and refusal to
  restart an exhausted agent. The kernel authorizes a ceiling; only the node is
  close enough to enforce it.
- A composite readiness observer routing each probe kind to the capability that
  owns it, and an agent probe measuring provider reachability, remaining budget,
  and container liveness.
- An operator surface: `a4s approve` issues and revokes Ed25519-signed grants
  for the five gated decisions, and `a4s history` narrows recorded history by
  goal, target, kind, or a window bounded at both ends. The filter itself is one
  implementation shared by the CLI and the operator API, so reading a log file
  and querying a running server cannot answer the same question differently.
  Approvals carry a mandatory expiry, are checked against the world's
  observation time, and survive restart because they are appended to durable
  history before the projection is updated.
- The first model-backed control agent: a diagnoser that explains a
  non-converging goal. It lives in `reason`, outside the stdlib-only `control`
  package, and falls back to the deterministic `LogDiagnoser` on every failure of
  the model path, so model availability is not a dependency. Input is a redacted,
  bounded context; output is strictly decoded and cannot express an action; every
  explanation records its model, template version, and observed revision.
- The `a4s.agent/v1` workload-facing runtime API on a Unix socket: claim, ack,
  requeue, spend, tool authorization, and identity. Allocation identity comes
  from a node-issued token resolved before any handler runs, so no endpoint
  accepts an allocation id and one instance cannot act as another.
- A durable node work queue with leased claims, bounded redelivery, stalled-task
  reporting, and measured depth. Claims are gated on the instance being metered,
  funded, not draining, and not already holding work, which is what makes a drain
  observe real held tasks rather than an empty slot.
- Measured provider reachability: a node-side monitor that checks egress on a
  timer, fails closed, and reports `provider.reachable` with an expiry. Node
  provider facts are measurements rather than flags, and unmeasured, unreachable,
  and expired all read alike, so a node that loses egress stops attracting agent
  placements.
- Failure domains and replica spread. A node declares the domain it fails with;
  a node that declares none is its own, so spreading works before any topology
  is described. A workload declares at most how many replicas may share a
  domain, the placement agent selects against it, and the kernel enforces it
  independently. Without it the availability floor, the canary ladder, and
  rolling replacement are all satisfied by replicas a single reboot ends.
- A cluster-wide disruption governor. A budget bounds how many disruptive
  actions the whole cluster may take in a window, and only one failure domain
  may be under disruption at a time. Both are derived from recorded evidence
  rather than counted in memory, so they survive restart. A failure is not
  charged, and neither is clearing an allocation that already stopped: the
  budget paces change the control plane causes, not damage it repairs.
- Per-target failure backoff. Consecutive failures escalate the delay before a
  target may be created or started again, and observed readiness resets it.
  Removal is never blocked, so backoff paces repair instead of preventing it.
  This is what stops a goal re-proposing the same failing placement every round.
- Readiness measured across the process boundary. Readiness has to be observed
  where the workload runs, and the control plane is a different process from
  every node. Until this existed the only wiring that worked was the acceptance
  test's, which builds the engine and the node runtime together and hands the
  observer straight to the prober: a real deployment passed no probers at all,
  so readiness was asserted once at creation and then left to expire with
  nothing to refresh it. A probe now travels as a signed action to the node
  holding the allocation, is answered from the capability that owns that probe
  kind, and comes back as evidence the node signs with its own identity. It is
  never served from the idempotency ledger, because a remembered readiness is
  precisely what an expiring observation must not be, and it never reaches the
  runtime, because measuring is not mutating.
- Node reachability as evidence. Node health was previously a fact nobody ever
  updated: it came from the scenario file and stayed there, so a node that died
  kept attracting placements and the remediation agent's first rung could never
  fire. The server records reachability from the connections it holds, only when
  it changes. Unreachable stops new placement and nothing more: a partitioned
  node keeps running what it was told to run, and relocating on silence is how
  one workload becomes two.
- Pacing distinguished from failure. A goal held back by a safeguard returns a
  typed result naming the constraint and when it lifts, counted and logged
  separately from a reconciliation that actually failed. Without it a working
  governor is indistinguishable from a broken cluster on every dashboard.
- Readiness re-measured every round rather than only after an action, and the
  world's snapshot time taken from the control plane's clock rather than from
  the last thing that happened. Both were deadlocks found by driving the
  safeguards against each other: a quiet cluster froze its own clock so nothing
  perishable expired, and a converged cluster never re-probed, so readiness
  lapsed and no agent had anything to propose about it.
- Diagnosis of the safeguards themselves. Each of the controls above stops work
  on purpose, and a goal stopped on purpose looks exactly like a broken one. The
  deterministic diagnoser reports which targets are waiting out a backoff, which
  nodes are cordoned, when nothing is schedulable at all, and when the cluster is
  pacing disruption, with a named next step for every refusal they produce. The
  redacted model context carries the same facts, so a model-backed explanation
  is not blind to them.
- `a4s status` reports schedulable nodes against total, targets in backoff, and
  recent disruptions, and the same counts are exposed as metrics. A brake nobody
  can see is indistinguishable from a fault.
- Node cordon and evacuation, for both the unplanned and the planned case. The
  remediation agent cordons a node it has measured as failing; `a4s cordon`
  covers maintenance, where nothing is wrong yet and nothing will observe a
  reason to stop scheduling. An operator cordon is authenticated by the request
  signature, recorded in durable history against whoever issued it, and survives
  restart. Cordoning is separate from health, because health
  is measured and clears itself while a cordon is a decision that stands.
  Draining has no action of its own: it is a cordon plus ordinary stop and
  delete, so evacuation passes through the same authorization, disruption
  budget, and stateful-data approval as any other removal. Cordon settles in
  the control plane rather than on the node, since the usual reason to cordon
  is that the node has stopped answering.
- A remediation agent that closes the loop diagnosis opened. It walks a fixed
  ladder cheapest-first: cordon an unhealthy node, retire an allocation that
  stopped and is holding its replica slot, then evacuate a cordoned node one
  allocation at a time. It may subtract but never add, so a remediation loop
  that went wrong cannot conjure capacity, and it stops after a bounded number
  of attempts so a goal that cannot be repaired stays visibly unconverged.
- Image provenance as kernel policy. A build signer's attestation names the
  digest it covers, and the kernel verifies it against trusted signers before
  authorizing a pull. An attestation that is supplied is always verified, so
  attaching a forged one is never better than attaching none. `a4s attest`
  produces them. Requiring one is off by default for compatibility with goals
  written before it existed; see "required before production".
- Cluster-wide ceilings on compute, allocation count, and agent spend. Node
  capacity bounds one machine and per-node budget capacity bounds one node's
  agents; these bound the total, so a runaway control loop has a maximum cost.
- Stateful workloads limited to one replica, pinned to the node holding their
  data, and never relocated on a missing heartbeat.
- Separate authenticated approval record for public routes.
- Required evidence declarations for allocation start and route publication.
- Bounded reconciliation rounds and blocked-goal events.
- Executor and world projection separated: evidence is the only input that
  advances the world, and executors cannot assert state directly.
- Pure, idempotent world projection that never mutates its input and does not
  double-count capacity when an action is replayed.
- Prober interface separating readiness observation from the executor that
  performed the mutation.
- Observation freshness: readiness evidence expires, and expired readiness stops
  satisfying a goal.
- Stop and delete actions, with deletion refused while an allocation is running.
- Durable world projection rebuilt from recorded evidence, so a restarted server
  recovers authoritative state instead of losing it.
- Remote executor that binds every issued capability to the proposal that
  authorized it.
- Per-allocation network addressing, with the kernel refusing to start a
  networked workload that has no address.
- Multi-replica placement batched to bound blast radius.
- Service discovery derived from verified evidence, and route snapshots
  resolving each route to the endpoints observed serving it.
- Opaque secret references with node-sealed material, version-only evidence, and
  redaction proven by scanning every serialized artifact.
- Volumes with generation-fenced single-writer ownership, durable on the node so
  a restart cannot produce a second writer.
- Checksummed snapshots and restore that verifies before overwriting, so a
  corrupt backup is refused rather than written over the data it should
  recover.
- Off-host backup with fallback restore, so a volume survives the loss of the
  node holding it.
- Snapshot retention with dry-run pruning that protects the last-known-good and
  backed-up snapshots and never removes the last recovery point.
- Scheduled restore verification that proves a backup recoverable without
  touching the live volume, and a storage agent that re-checks stale backups.
- Cross-node handoff gated step by step on evidence, with the origin remaining
  authoritative until the target proves it holds the data, and the node actually
  moving the bytes through the shared store.
- Target leases acquired before the first mutation and released on every exit
  path, so two proposals cannot interleave on one allocation. Leases expire so
  an abandoned holder cannot block a target indefinitely.
- Rollout agent retiring drifted allocations one at a time, with the kernel
  independently enforcing the availability floor.
- Operator-approved rollback execution. A failed rollout still blocks the goal
  and names the known-good digest; once an operator approves, the approval
  records both versions and the effective goal resolves to the known-good
  image for every agent in the round. Binding the decision to specific versions
  is what stops the rollback oscillating as observations arrive.
- Evidence-backed garbage collection of unreferenced images, with the protected
  set computed by the kernel from the world and checked against it, and a
  dry-run mode that reports exactly what a real run would reclaim.
- Cluster-wide service names. A workload resolves under `a4s.internal` from any
  node: locally to its allocation address, and elsewhere through the owning
  node's gateway, because an allocation address is only routable where it was
  assigned. Names are published as complete zones with a fingerprint, so an
  unchanged cluster stops republishing.
- A node-local DNS resolver serving only that zone. It never forwards, so a
  name it does not know fails rather than escaping to public DNS.
- Typed network policy compiled to nftables. Intent is declared as ingress and
  egress rules naming workloads or CIDRs; the compiler expands names to
  observed endpoints and fails closed when nothing is serving. a4s owns one
  table and replaces it wholesale, so applied state equals authorized state.
- Canary rollout with weighted traffic. A goal declares traffic steps and an
  optional hold; the kernel derives the authorized share from the proportion of
  ready allocations running the target image, capped by how long the target side
  has been continuously healthy, and the gateway applies per-endpoint weights.
  Because the share is derived rather than latched, a new version that stops
  being measured ready loses its traffic automatically instead of holding it on
  the strength of an earlier advance. The hold is measured from the target's
  least-established replica, so scaling the target side up starts the new step's
  hold rather than inheriting the previous step's.
- Scheduled and batch workloads. A workload declares a cron schedule, a run
  deadline, required completions, retries, and a concurrency policy. Schedules
  are evaluated as a pure function of the observed world's time and always in
  UTC, so placement stays deterministic and a daylight-saving transition cannot
  make a job run twice or not at all.
- Goals from a versioned repository. A git source mirrors a tracked ref into a
  bare repository and submits every goal document it finds through the same
  admission path as the operator API. Nothing is checked out, so a repository
  cannot write a working tree onto the control plane, and one malformed document
  is reported without stopping the rest of the repository from converging.

### Operator introspection

- Causal explanation of any allocation or route from durable history: the goal
  that requested it, the agent that proposed it, the kernel authorization, and
  the evidence that proved the result.
- Dry-run planning that reuses the kernel's own simulation, so a plan cannot
  drift from what execution would do. Steps contingent on unmeasured readiness
  are marked rather than promised.
- Deterministic diagnosis of a non-converging goal, with a suggested next step.
  The diagnoser reads events and writes text; it holds no grants and cannot
  mutate anything, which is why a model-backed implementation can replace it
  without widening authority.

### Event persistence

- SQLite in WAL mode with `synchronous=FULL`, so every acknowledged event is
  fsynced before the append returns. Verified by SIGKILLing a writer mid-append
  across repeated trials: no acknowledged event was lost and the chain verified
  on every recovery.
- A versioned schema with migrations applied transactionally. A store written
  by a newer build is refused rather than interpreted through a schema that no
  longer describes it.
- The hash chain is retained on top of the database rather than replaced by it.
  SQLite guarantees the rows are the ones that were committed; only the chain
  establishes that they are the ones a4s wrote, which is what catches an edit
  made through `sqlite3` directly.
- The chain verified when the log is opened, not on first read. A store whose
  event blobs cannot be decoded satisfies the head-and-count check, so it would
  otherwise come up and fail later in whichever caller read records first.
- Chain invariants enforced by the schema: sequence is the primary key, hashes
  are unique, and CHECK constraints reject a malformed link. A record and the
  chain head advance in one transaction, and the head update is conditional on
  the sequence the append observed, so two writers cannot fork history.
- Backups taken with `VACUUM INTO`, which is transactionally consistent without
  stopping writers, and verified read-only so checking a recovery point cannot
  alter it.
- Pure-Go driver, so `CGO_ENABLED=0` cross-builds still produce statically
  linked binaries for linux/amd64 and linux/arm64.
- Monotonic sequence validation.
- SHA-256 hash chaining and replay-time corruption detection.
- Mode `0600` enforced on the event log.
- Intent persisted as `action.dispatched` before mutation.

The hash chain detects edits and reordering relative to the local first record.
Truncation and wholesale replacement are caught by the external anchor, which
witnesses the chain head outside the store it describes; without an anchor path
configured that gap stays open. The anchor is a single local file, so it raises
the cost of forging history rather than making it impossible: an attacker with
write access to both the store and the anchor can still produce a consistent
pair.

### Operator API and observability

- An authenticated HTTP operator API. Every request carries an Ed25519-signed
  envelope binding the signature to the method, path, and body digest, with a
  nonce ledger that makes a captured request single-use and a five-minute
  maximum lifetime. Reads are authenticated too: the world projection and
  history describe the whole cluster. Only liveness is public.
- Request and decoder limits applied before authentication, so an unbounded
  body cannot exhaust the control plane without a valid signature.
- `a4s submit`, `a4s status`, and `a4s events` speak to a running server, so a
  goal reaches the control plane without a scenario file.
- Structured logging in both daemons, text or JSON, with the level and format
  chosen at startup and an unrecognized value refused rather than defaulted.
- In-process metrics with a Prometheus text endpoint: world revision, node,
  allocation, route and event counts, connected nodes, reconciliation
  outcomes, and operator request outcomes from a closed set.

### Controller state and key custody

- Verified backup and restore of the authoritative event log. A backup records
  the chain head outside the file, which closes the truncation gap the hash
  chain alone cannot detect, and a restore verifies before touching the
  destination and preserves any log it supersedes.
- Controller signing keys as a distributable keyset with active, accepted, and
  retired states. Rotation demotes the previous key to accepted rather than
  removing it, so a fleet rotates without a coordinated restart; retiring the
  active key is refused.

### Server

- Durable event log opened at startup and world projection rebuilt from it, so
  recovery is the normal startup path rather than a special case.
- Goal admission validated before acceptance.
- Lease manager shared across reconciliations, so goals touching the same
  allocation cannot interleave.
- Repeatable rebuild, proving the projection is a function of the log.
- An anchored warm standby, with `a4s standby` following a primary over the
  operator API. The primary serves hashed records in batches; the follower
  re-derives every hash against its own chain, so agreement means it computed
  the same history rather than faithfully copying what it was sent, and a
  divergence stops replication rather than being stored. Promotion is refused
  unless the chain verifies and the follower is at or beyond the externally
  witnessed head, which is the check that stops a replica promoted
  mid-replication from silently rolling history back. It is not consensus and
  holds no election: it makes an operator's failover decision safe to make.
  Two deployment requirements fail quietly if got wrong, and both are checked
  in operations: the anchor must be the primary's, on storage both can reach,
  because a replica-local one witnesses whatever the replica last ingested and
  agrees with itself; and the follower needs the same base world, since the log
  carries node inventory and pre-existing approvals nowhere.

### Node trust boundary

- Ed25519 signatures over the exact transmitted envelope bytes, verified before
  the payload is decoded or interpreted.
- Rejection of unknown fields and trailing content in a signed envelope.
- Local key-ID trust map.
- Exact node targeting.
- Issue time, expiry, and maximum five-minute envelope lifetime.
- Thirty-second future-clock tolerance.
- Envelope/action node agreement.
- Persistent idempotency-key-to-envelope-digest ledger.
- Rejection when one idempotency key is reused for a different envelope.
- Serialized dispatch in the initial implementation.
- Per-message dispatch responses, so a rejected or failed action does not
  terminate the node.
- Deduplication on a digest of the authorized work rather than the whole
  envelope, so a legitimate retry returns the stored result while a key reused
  for different work is refused.
- Durable desired-state cache and a supervisor that restarts crashed workloads
  during a control-plane outage, bounded by a crash-loop budget and backoff.
- Orphan discovery for a4s-managed containers absent from desired state.
- Node attribution of evidence: the node stamps its own identity and observation
  time onto everything a runtime adapter reports.
- Channel encryption beneath the transport. The enrollment handshake carries
  ephemeral X25519 shares inside the signed payload, so the key agreement is
  authenticated and a man in the middle cannot substitute their own. Records
  are sealed with ChaCha20-Poly1305 under per-direction keys and sequence
  nonces, which makes a reordered or replayed record fail authentication.
  `--require-encryption` refuses a peer that does not negotiate a channel.

### Linux container runtime

- Official containerd v2 Go client integration behind a Linux build tag.
- Configurable containerd socket, namespace, snapshotter, and log directory.
- Five-second containerd health check at startup.
- Pull and unpack by immutable image reference.
- Pulled manifest digest compared with the requested digest.
- Idempotent a4s-managed container creation with ownership labels.
- Snapshot and OCI specification creation.
- CPU, memory, and PID limits.
- `noNewPrivileges`, empty capability set, and namespaced cgroup.
- Idempotent task start and allocation log file creation.
- Evidence differentiates `allocation.running` from readiness.

### Developer tooling

- `validate`, `simulate`, `node`, `server`, `keygen`, `keys`, `seal`, `plan`,
  `explain`, `diagnose`, `approve`, `history`, `backup`, `restore`, `submit`,
  `status`, `events`, `version`, and `help` CLI commands.
- `seal` encrypts secret material to a node's public identity, so the sealed
  file is readable only by the node it was sealed for and never transits the
  control plane in the clear.
- Race-tested unit and contract tests, including a denial matrix that drives
  the node over the real protocol: unknown signing key, tampered envelope,
  wrong node, expired capability, idempotency-key reuse, replay after node
  restart, stale readiness, complete deletion, and an unapproved public route.
- CI running race tests, vet, gofmt, `go mod tidy`, Linux and arm64
  cross-builds, the example simulation, and documentation link checking. The
  committed fuzz seed corpora run with the ordinary tests; fuzz campaigns are
  run locally rather than on every push.
- Build-time version, commit, and date stamping, with a release script
  producing checksummed binaries for four platforms.
- `scripts/check-nftables.sh`, which applies the compiler's own output to a
  real Linux kernel rather than asserting the rendered text looks correct.
- Generic web-service example with an explicit public-route approval.

## Simulated in `a4s simulate`

These are supplied by the in-memory executor when running a scenario without a
node. Against a real node each one is a measurement instead.

- World materialization and revision changes.
- Allocation readiness.
- Route creation and reachability.
- Placement across multiple observed nodes.
- Public-route approval ingestion.

No executor asserts readiness. Readiness is measured by a prober and carries an
expiry, so a stale observation stops satisfying a goal. The remaining assumption
is that the in-memory simulation's observer reports a running task as ready; the
node's `RuntimeObserver` performs real process, TCP, and HTTP measurements.

## Not implemented

- Multi-server consensus or automatic failover. One server owns the event log.
  A warm standby replicates over the operator API and refuses to promote unless
  it can prove it is caught up, but nothing elects it: promotion is an operator
  or supervisor decision, and the anchor it checks against has to be on storage
  both machines can reach.
- A dedicated transfer transport. The node moves data through the shared backup
  store, so a move needs a store both nodes can reach; direct node-to-node
  streaming is not implemented.
- Model-backed storage scheduling. The storage agent re-verifies stale backups
  deterministically; its cadence comes from reconciliation frequency rather
  than an internal timer.
- Secret rotation without a workload restart, and a Vault-backed broker. The
  file broker and mount path are implemented; rotation currently means changing
  the goal's version, which replaces the allocation.
- Kubernetes manifest importer.
- Distributed tracing. Structured logs and metrics exist; spans do not.
- IPv6 allocation addressing. The CNI configuration and the policy compiler
  both assume IPv4.

## Known implementation constraints

- The module requires Go 1.26.3 because containerd v2.3.2 declares that Go
  version.
- The example image digest is all zeroes and exists only for simulation. It
  cannot be pulled on a real node.
- The node harness reports rejected and failed envelopes per message and keeps
  running. It still exits on an undecodable frame, because a malformed frame
  desynchronizes the stream.
- One dispatcher serializes all actions. There are no per-target concurrent
  leases yet.
- The file ledger records success after host mutation. Repeated execution is
  therefore safe because each implemented container action is idempotent and
  the world projection is idempotent for replayed evidence.
- Existing stopped containerd tasks are reported as an error; lifecycle repair
  is not implemented.
- The container is hardened relative to image defaults, but it may still run as
  root inside its namespace. Do not treat the current OCI profile as a complete
  production sandbox.

## Last verified commands

The following passed on the status date:

```bash
go test -race ./...
go vet ./...
gofmt -l .
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/a4s-node.test ./node
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/a4s ./cmd/a4s
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/a4s-arm64 ./cmd/a4s
go run ./cmd/a4s validate --file examples/web-service.json
go run ./cmd/a4s simulate --file examples/web-service.json
./scripts/check-doc-links.sh
./scripts/check-nftables.sh
git diff --check
```

`check-nftables.sh` applies the policy compiler's own output to a real Linux
kernel and prints the resulting table, so the compiled ruleset is verified
against nftables rather than against an assertion about its text.

The full round trip has been verified against a live containerd socket on
linux/amd64 and linux/arm64, including allocation networking through CNI, the
gateway, and durable volumes.

## Exact next milestone

The control plane is verified end to end: the action and evidence round trip,
restart recovery, outage survival, encrypted transport, key rotation, backup and
restore, rollback execution, cross-node name resolution, and firewall
compilation, with the denial matrix exercised over the real protocol and the
runtime adapter driven against real containerd.

Four of the five milestones this section previously named are closed: evidence
is signed by the node that measured it, the chain head is anchored outside its
own store, containers run under a seccomp profile with user-namespace mapping
available, and both daemons ship as hardened systemd units under a
least-privilege identity.

What remains is the operational surface a production deployment needs, in
risk order:

1. Finish the container sandbox. Seccomp and user namespaces are in place, but
   a container still runs as the image's own user unless an operator pins one,
   and no AppArmor profile is selected.
2. Record measured failure behavior under sustained load and multi-node
   operation, beyond the single verified round trip. The safeguards are now
   driven against each other in simulation, which found two deadlocks, but
   nothing has observed them pacing a real cluster in trouble. The remote probe
   path is unit-tested over the real transport and has not run against live
   containerd.
3. Turn on `--require-signed-images` with a real build signer. The mechanism
   and the `a4s attest` tool exist; nothing in this repository's own examples
   uses them, because their digests are simulation placeholders.
4. Verify snapshot provenance in the gateway independently, rather than
   trusting the node that produced it.
5. Rotate a secret without replacing the allocation that mounts it.
6. Automatic failover. A follower replicates and refuses an unsafe promotion,
   but nothing elects one: stopping the primary and starting a server on the
   replica is an operator or supervisor decision.

Do not treat this as production-ready until those are closed. The runtime and
audit boundaries are proven; the sandbox boundary and sustained-failure
behavior are not.
