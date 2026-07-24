# Project status

Status date: 2026-07-22

Version: `0.2.0-dev`

Maturity: architecture experiment and executable vertical slice; not a
production orchestrator.

## Executive status

a4s currently proves two boundaries:

1. A deterministic agentic control loop can turn an outcome-oriented goal into
   bounded proposals, authorize the complete proposal against observed state,
   execute typed actions in a simulated world, and verify convergence.
2. A Linux node can accept a short-lived signed action envelope, deduplicate it
   durably, and translate the container actions into containerd API calls
   through a narrow runtime contract.

Those boundaries are now connected. A `RemoteExecutor` issues signed
capabilities for an authorized proposal, a node dispatches them, and the
returned evidence advances a world projection that can be rebuilt from the
durable event log. The end-to-end acceptance suite drives a real control engine
against a real node dispatcher over the real protocol, with only containerd
faked.

A server package and `a4s server` command hold durable history and rebuild the
world projection on every start, and nodes enroll over a real network transport
by proving possession of their identity key. What remains is channel encryption
beneath the transport and the data-plane work: CNI, gateways, storage, secrets. The implemented transport is a byte-stream protocol currently
carried over a pipe; moving it onto the tailnet does not change the control
contract.

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
- Target leases acquired before the first mutation and released on every exit
  path, so two proposals cannot interleave on one allocation. Leases expire so
  an abandoned holder cannot block a target indefinitely.
- Rollout agent retiring drifted allocations one at a time, with the kernel
  independently enforcing the availability floor.

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

- Newline-delimited event records.
- Monotonic sequence validation.
- SHA-256 hash chaining and replay-time corruption detection.
- Append followed by file sync.
- Mode `0600` enforced on the log file.
- Intent persisted as `action.dispatched` before mutation.

The hash chain detects edits and reordering relative to the local first record.
It does not prevent undetected truncation or replacement unless the latest hash
is anchored outside the file. It is an integrity aid, not yet a complete audit
security system.

### Server

- Durable event log opened at startup and world projection rebuilt from it, so
  recovery is the normal startup path rather than a special case.
- Goal admission validated before acceptance.
- Lease manager shared across reconciliations, so goals touching the same
  allocation cannot interleave.
- Repeatable rebuild, proving the projection is a function of the log.

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

- `validate`, `simulate`, `node`, `server`, `keygen`, `plan`, `explain`,
  `diagnose`, `version`, and `help` CLI commands.
- Race-tested unit and contract tests.
- Linux amd64 cross-build verification.
- Generic web-service example with an explicit public-route approval.

## Simulated only

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

- External goal API. The server package and `a4s server` command exist, but
  goals are supplied from a scenario file rather than an authenticated API.
- Channel encryption. Enrollment authenticates who the peer is; it does not
  protect the channel. The transport is intended to run over an already
  encrypted tailnet, and needs TLS beneath it on an untrusted network.
- Controller key custody and key rotation.
- Canary rollout and compensating-action execution. Rolling replacement, the
  disruption budget, and known-good rollback detection are implemented;
  applying the rollback is deliberately an operator decision.
- Garbage collection of unreferenced images and snapshots.
- Node-local DNS and cross-node service routing. Service discovery resolves a
  workload to endpoints on the node that owns them; a service name does not yet
  resolve across nodes.
- nftables policy compilation from typed network intent.
- Public ingress or TLS automation.
- Off-host backup and ownership handoff between nodes. Node-local snapshots and
  verified restore are implemented; copying a snapshot off the host and moving a
  volume to another node are not.
- Scheduled restore tests. Restore is verified when it runs, but nothing
  exercises it on a schedule.
- Secret rotation without a workload restart, and a Vault-backed broker. The
  file broker and the mount path are implemented; rotation currently means
  changing the goal's version, which replaces the allocation.
- Seccomp/AppArmor selection, user namespaces, or rootless containers.
- Operator API and separately authenticated approval workflow.
- Model-backed agents, agent sandboxing, or agent resource budgets.
- SQLite event storage, multi-server consensus, or HA. The world projection is
  rebuilt from the hash-chained file event log.
- Kubernetes manifest importer.

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
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/a4s-node.test ./node
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/a4s ./cmd/a4s
go run ./cmd/a4s simulate --file examples/web-service.json
git diff --check
```

The Linux binary cross-built successfully. It has not yet been exercised
against a live Linux containerd socket.

## Exact next milestone

The action and evidence round trip, restart recovery, and outage survival are
proven in the acceptance suite against a faked containerd. The next milestone
replaces the fake with real hardware:

1. Generate and install a temporary control signing key.
2. Start containerd and `a4s node` on a disposable Linux host.
3. Converge the example goal against a real digest-pinned image.
4. Kill the container out from under the node and confirm the supervisor
   restarts it while the control plane is stopped.
5. Restart `a4s node` and confirm replayed actions do not duplicate runtime
   state and that orphan discovery reports anything left behind.
6. Restart the server and confirm the world projection rebuilds from the event
   log without redoing work.
7. Record measured failure behavior, especially anything the faked backend did
   not model.

Only after that should the transport move onto an authenticated tailnet
connection. Do not begin CNI or storage until the round trip survives real node
and server restarts on real hardware.
