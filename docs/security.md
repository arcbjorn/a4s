# Security model

This document defines the intended trust boundaries and records which controls
exist today. a4s is not production-ready. Treat every item under “required
before production” as a blocker, not optional hardening.

## Security objective

Allow untrusted or fallible control agents to reason about infrastructure
without giving them direct authority over hosts, runtimes, networks, storage,
or secrets.

The central rule is:

```text
agent output is untrusted proposal data
```

Only deterministic code may convert a proposal into a signed, typed node
capability, and only a node executor may invoke privileged host APIs.

## Protected assets

- Host root and containerd socket access.
- Controller signing keys and node identity keys.
- Goal, approval, and policy authenticity.
- Event-log integrity and availability.
- Node idempotency ledger integrity.
- Workload image provenance and OCI configuration.
- Service exposure and network policy.
- Stateful ownership, volumes, snapshots, and backups.
- Secret values and secret-backend credentials.
- Evidence authenticity and freshness.
- Control-plane and node availability.

## Threat actors and failures

The design assumes any of these can occur:

- A model-backed agent is prompt-injected, hallucinates, or acts adversarially.
- A deterministic agent contains a bug.
- An agent lies about its identity, capabilities, reasoning, or success.
- A stale proposal races a newer world revision.
- A client tries to embed an approval in a goal.
- A network peer replays, delays, modifies, or redirects an action.
- A node process crashes after mutation but before recording completion.
- An image or registry is compromised.
- A container attempts privilege escalation or host escape.
- A local user tampers with the event log or node ledger.
- Controller and node clocks differ.
- A node or controller signing key is stolen.
- An unavailable node is mistaken for an empty node, duplicating durable state.
- Secret values leak through prompts, reasoning, events, logs, or evidence.

The current spike primarily addresses agent authority, stale plans, signed node
actions, replay, and a baseline OCI profile. It does not yet address a
compromised node or controller.

## Trust boundaries

```text
operator / Git / external API
            |
       authentication                 future
            v
 goals + approvals + observations
            |
            v
    agent coordinator
            |
       untrusted proposal
            v
 deterministic policy kernel  <---- trusted policy and world projection
            |
      signed typed envelope
            v
       node dispatcher         <---- local trusted keys and ledger
            |
   narrow runtime capability
            v
 containerd / CNI / volumes / gateway
            |
       independent probes
            v
       signed evidence                    future
```

### Agents

Agents are never trusted, even when compiled into the server. Their process,
model, prompt, explanation, and declared descriptor do not authorize actions.
They receive observations, not runtime credentials.

### Kernel

The kernel is trusted to authenticate the agent identity supplied by its caller,
apply kernel-owned grants, reject stale proposals, simulate the complete plan,
and enforce deterministic policy. The current library accepts an
`AgentDescriptor` from the engine; a future server must derive it from the
registered agent session rather than request JSON.

### Node

The node trusts locally installed controller public keys and the containerd
socket. It rejects invalid envelopes before runtime dispatch. A node compromise
therefore exposes that node's workloads and locally available credentials. It
must not automatically grant authority over other nodes or controller signing
keys.

### Data-plane tools

containerd, runc, CNI, nftables, volume tools, and gateways are privileged
implementation mechanisms. Their full APIs must not cross into agent action
schemas. Each a4s adapter exposes a smaller contract.

## Implemented controls

### Proposal authorization

- Agent ID equality with the authenticated descriptor.
- Kernel-owned action grants by stable agent ID.
- Exact goal ID and world revision.
- Maximum eight actions per proposal.
- Unique nonempty action IDs and ordered dependencies.
- Full-plan simulation before the first real action.
- Goal equality for image, workload, resources, route, and exposure.
- Node health, label, allowed-node, capacity, and replica-index checks.
- Explicit public-route approval.
- Privileged and stateful workload rejection.

### Node envelope

- Ed25519 signature over the exact transmitted envelope bytes. The signature is
  verified before the payload is decoded, so the node never interprets
  unauthenticated input and the signature does not depend on encoder stability.
- Unknown fields and trailing content in a signed envelope are rejected, so a
  node never executes an envelope it did not fully understand.
- Exact local node target.
- Known key ID.
- Five-minute maximum validity and strict expiration.
- Thirty-second future-clock tolerance.
- Action/envelope node consistency.
- SHA-256 envelope digest used for idempotency conflict detection.

### Evidence and world integrity

- Evidence is the only input that advances the world projection. No executor,
  agent, or action mutates world state directly.
- Projection is pure and never mutates the authoritative world in place, so a
  rejected projection cannot leave state half-updated.
- Projection is idempotent for replayed evidence, so an action repeated after a
  crash between host mutation and result recording does not double-count node
  capacity or duplicate allocations.
- Readiness is separated from execution. An executor may report
  `allocation.running`; only a prober may report `allocation.ready`. The
  component that started a workload cannot declare it healthy.
- Unknown evidence kinds are rejected rather than ignored.

- Readiness observations expire. An allocation whose readiness measurement has
  aged out stops counting toward a goal, so a dead workload cannot keep looking
  healthy on the strength of an old probe.
- A probe that cannot complete produces no evidence rather than a false
  negative. "Could not measure" and "measured as unhealthy" stay distinct.
- The node stamps its own identity and observation time onto evidence a runtime
  adapter produces.

The remaining gap is that probe evidence is not yet signed by a node identity
distinct from the controller signing key, so a compromised node can still lie
about what it observed. That must be closed before production.

### Node autonomy limits

The node keeps a durable record of server-authorized intent so workloads survive
a control-plane outage. That autonomy is deliberately narrow:

- The node restarts only allocations the server already authorized to run.
- It never creates a workload, changes an image, or alters resources on its own.
- An allocation the server stopped is not restarted, so the data plane cannot
  override control-plane intent.
- Restarts are bounded by a crash-loop budget, so a broken workload becomes
  visible instead of being hidden behind an endless restart loop.
- Orphaned containers are reported, never deleted automatically, because
  deletion is an authorized action rather than a cleanup detail.

### Replay and crash behavior

- Successful results are appended to a mode-`0600` ledger and synced.
- Same key and same envelope returns prior evidence without execution.
- Same key and different envelope is rejected.
- Pull, create, and start also detect safe repeat conditions at containerd.
- Controller intent is persisted before executor invocation.

### Container baseline

- Immutable SHA-256 image reference required.
- Pulled target digest must equal requested digest.
- CPU, memory, and PID limits.
- Empty Linux capability set.
- `noNewPrivileges`.
- Namespaced cgroup.
- Existing containers require a4s ownership, workload, and image labels.
- Allocation log path is constrained beneath the configured directory.

These controls do not prove image trust, application safety, or host isolation.

## Approval security

An approval is a separate control object because the entity requesting an
outcome must not authorize its own elevated risk. In the simulation, approval
records appear in the starting world for convenience.

The future API must:

- Authenticate the operator or automation principal.
- Bind the approval to an exact goal revision and scope.
- Record issuance and optional expiry.
- Prevent an agent, goal submitter, or imported manifest from setting approval
  state directly.
- Require stronger approval for destructive volume operations, host-wide
  changes, secret-scope changes, and high-blast-radius rollout.

## Key management requirements

The current node loads one base64 Ed25519 public key. Production key management
must add:

- Controller private keys outside agent context and ordinary event payloads.
- Node-specific trust roots or tightly scoped signing intermediates.
- Explicit key IDs, activation windows, overlap, and revocation.
- Secure initial node enrollment.
- Hardware-backed storage when justified.
- Audit events for trust-set changes.
- Recovery procedure for controller-key loss and compromise.
- Evidence signing by node identity distinct from controller action signing.

Do not add a CLI that casually prints private keys or accepts them through
command-line arguments. Command lines, shell history, and process listings are
not key stores.

## Audit integrity

The event file hash chain detects modification, insertion, and reordering from
the first locally available record. It does not by itself detect:

- Truncating valid records from the end.
- Replacing the entire file with an older valid copy.
- A privileged attacker rewriting the file and every subsequent hash.
- A compromised process emitting false-but-well-formed events.

Before relying on it for security audit, periodically sign or publish the
latest hash to an independent location, protect backup retention, and separate
audit-reader from audit-writer authority.

## Secrets policy

Secret values must never appear in:

- Goals or objectives.
- Agent prompts or reasoning.
- Proposals or actions.
- World observations.
- Evidence maps.
- Events, log messages, filenames, or labels.
- Container command-line arguments when avoidable.

The planned secret action should reference opaque secret identity and version.
The node should obtain node-scoped encrypted material from a broker, mount it
in tmpfs or a runtime credential facility, and report only version and mount
status.

## Model-backed agent policy

A model integration must not precede these controls:

- Isolated agent runtime with authenticated identity.
- Strict structured proposal decoder with size limits.
- Kernel grants independent of agent declaration.
- Input context redaction and secret exclusion.
- Time, token, action, and proposal-count budgets.
- Deterministic fallback agent for bootstrap and known reconciliation.
- Full audit of model version, prompt/template version, supplied observation
  references, and proposal outcome without storing secret content.

Never interpret natural-language model output as a shell command or direct host
operation.

## Supply-chain requirements

Digest pinning provides immutability, not provenance. Before production add:

- Registry allowlists.
- Signature or attestation verification.
- Policy for trusted builders and source repositories.
- Vulnerability and malware scanning policy.
- SBOM retention.
- Controlled dependency update and Go checksum verification.
- Reproducible release builds and signed a4s binaries.

## Required before production

- Authenticated server API and separately authenticated approvals.
- Mutual controller/node authentication and encrypted transport.
- Controller key rotation and node enrollment.
- Signed, fresh node observations and evidence.
- Enforced target leases and conflict recovery.
- External audit-hash anchoring and tested backup/restore.
- Stop/delete lifecycle with compensation and crash reconciliation.
- Independent readiness/liveness probes.
- Seccomp/AppArmor policy and non-root/user-namespace strategy.
- CNI/network-policy enforcement and gateway snapshot authenticity.
- Secret broker with redaction tests.
- Stateful ownership protocol before any durable workload.
- Resource and request limits on all decoders and agent runtimes.
- Fuzzing of protocol decoders, kernel authorization, event replay, and node
  envelope verification.
- Disposable-node penetration and failure testing.

## Security-review trigger

Create or update an architectural decision record whenever a change:

- Adds an action kind or broadens action fields.
- Changes who signs, verifies, or stores keys.
- Moves policy into an agent or model.
- Adds a transport or externally reachable API.
- Handles secrets or durable data.
- Changes audit durability or integrity.
- Permits privileged, host-network, host-mount, device, or arbitrary OCI
  configuration.
