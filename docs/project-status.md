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

Those boundaries are implemented but not connected. There is no running a4s
server and no controller-to-node network transport. The CLI simulation uses an
in-memory executor. The `a4s node` command reads signed actions from standard
input and writes results to standard output.

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
- Privileged and stateful workload rejection in v1alpha1.
- Separate authenticated approval record for public routes.
- Required evidence declarations for allocation start and route publication.
- Bounded reconciliation rounds and blocked-goal events.
- Executor and world projection separated: evidence is the only input that
  advances the world, and executors cannot assert state directly.
- Pure, idempotent world projection that never mutates its input and does not
  double-count capacity when an action is replayed.
- Prober interface separating readiness observation from the executor that
  performed the mutation.

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

- `validate`, `simulate`, `node`, `version`, and `help` CLI commands.
- Race-tested unit and contract tests.
- Linux amd64 cross-build verification.
- Generic web-service example with an explicit public-route approval.

## Simulated only

- World materialization and revision changes.
- Allocation readiness.
- Route creation and reachability.
- Placement across multiple observed nodes.
- Public-route approval ingestion.

No executor asserts readiness. The memory executor reports only what a real
executor could observe, and readiness arrives as separate probe evidence.
`OptimisticProber` currently supplies that evidence by declaring any running
allocation ready; it exists solely to close the loop and must be replaced by
process, TCP, and HTTP probes. It is now the single remaining place where
readiness is assumed rather than measured.

## Not implemented

- Long-running a4s server or external goal API.
- Controller identity, key storage, key rotation, or envelope issuance service.
- Controller-to-node transport and node-to-controller evidence transport.
- Persistent materialized world state and observation ingestion.
- Target leases despite the envelope carrying a `lease_id`.
- Compensating actions and rollback execution.
- Stop, kill, delete, garbage collection, and restart supervision.
- Live process, TCP, HTTP, or route probes.
- Containerd orphan discovery after daemon restart.
- CNI, allocation network namespaces, DNS, service gateways, or nftables.
- Public ingress or TLS automation.
- Volumes, snapshots, backups, or stateful ownership handoff.
- Secret broker and runtime credential mounts.
- Seccomp/AppArmor selection, user namespaces, or rootless containers.
- Operator API and separately authenticated approval workflow.
- Model-backed agents, agent sandboxing, or agent resource budgets.
- SQLite event storage, materialized projections, multi-server consensus, or HA.
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

Run one disposable Linux-node experiment that proves this sequence:

1. Generate and install a temporary control signing key.
2. Start containerd and the `a4s node` stream harness.
3. Sign and submit pull, create, and start envelopes for a real digest-pinned
   image.
4. Restart `a4s node` and resubmit the same envelopes.
5. Confirm the ledger returns prior results and containerd has exactly one
   matching container and task.
6. Add an independent process or HTTP probe and return readiness evidence.
7. Record failures and required code changes before designing the network
   transport.

After that experiment, implement a minimal server-issued envelope path with
mutual node identity. Do not begin CNI or storage until the action/evidence
round trip survives node and server restart.
