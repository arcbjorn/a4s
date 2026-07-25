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

The current implementation addresses agent authority, stale plans, concurrent
mutation of one target, signed node actions, replay, node identity, an encrypted
authenticated transport, and a baseline OCI profile. It does not yet address a
compromised node or controller.

## Trust boundaries

```text
operator / Git / external API
            |
       authentication
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
       node dispatcher         <---- enrolled node identity, keys, ledger
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

### Agent workloads

An agent workload is untrusted cargo, not a control-plane actor. It holds no
`ActionKind` grants and has no vocabulary for proposing infrastructure changes,
so a compromised agent workload cannot escalate into the control plane. Its
authority is exactly the `ToolGrant` envelope the kernel installed before it
started, and it cannot widen that at runtime.

Two properties carry the boundary:

- Envelopes are stored per allocation on the node. A runtime asks the node what
  it may call rather than deciding for itself, and there is no path from one
  allocation's query to another's grants. Context leakage between agents comes
  from shared runtime state rather than a shared kernel namespace, which is why
  credentials, workspaces, and envelopes are per instance.
- A mutating tool acts outside a4s, where no compensation, lease, or event log
  reaches. Mutating envelopes therefore require a separately authenticated
  `agent-mutating-tools` approval rather than an agent's judgement.

Budget ceilings are a safety control, not a cost feature: an unbounded agent is
an unbounded actor. The kernel refuses a workload whose ceilings are absent or
zero, and refuses to start one that has already exhausted its budget.

### The workload-facing runtime API

Every other node surface talks to the control plane, which is authenticated and
trusted. `a4s.agent/v1` talks to the agent, which is neither. It is the only
place in a4s where an untrusted party initiates a request, so it is the boundary
most worth attacking.

Three properties hold it:

- **Identity is issued, not asserted.** The node mints a per-allocation token at
  creation and resolves it before any handler runs. No endpoint accepts an
  allocation id, so an agent has no way to name another instance's budget,
  envelope, or task slot. Without this, every per-allocation control in this
  document would be bypassable by claiming a different identity.
- **It is not reachable off-host.** A Unix socket, never a port. A per-instance
  credential on a TCP listener would become a cluster-wide attack surface.
- **The token is treated as material.** Owner-readable file, not an environment
  variable, since env vars surface in process listings and crash dumps.
  Re-issuing invalidates the previous token; deleting an allocation revokes it,
  so a credential never outlives its workload.

Requests carrying unknown fields are refused rather than ignored, and bodies are
size-bounded so an untrusted caller cannot exhaust node memory without spending
a token.

### Kernel

The kernel is trusted to authenticate the agent identity supplied by its caller,
apply kernel-owned grants, reject stale proposals, simulate the complete plan,
and enforce deterministic policy. The kernel accepts an `AgentDescriptor` from
the engine, which is sound only because every agent runs in process. An
out-of-process agent must have its descriptor derived from an authenticated
session rather than taken from request JSON.

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
- SHA-256 digest of the authorized work used for idempotency conflict detection,
  so a retry with fresh timestamps is recognized while a key reused for
  different work is refused.
- Target leases, so two proposals cannot mutate one allocation concurrently.

### Node enrollment

- A node proves possession of its enrolled Ed25519 key over a server-chosen
  32-byte nonce before any capability is issued to it.
- The proof must name the same identity as the opening hello.
- Refusals are generic on the wire, so an unenrolled peer cannot enumerate valid
  node identities by probing.
- The node refuses a server that names a signing key the node does not already
  trust, so a reachable impostor cannot nominate its own key.
- Handshakes are deadline-bounded, so a stalled peer cannot hold resources.

Enrollment also agrees a session key. Both peers offer an ephemeral X25519
share inside the payload they sign, so the key agreement is authenticated by
the same identity keys that prove who they are: an attacker in the middle
cannot substitute their own share without invalidating the signature. Records
are then sealed with ChaCha20-Poly1305 under per-direction keys, with the
sequence number as the nonce, so a reordered or replayed record fails
authentication rather than being interpreted.

The shares are ephemeral, so recording a session today and compromising a node
identity key tomorrow does not reveal what was said. That is deliberately
different from the sealed-secret path, which uses long-lived identity keys
because material must survive to be decrypted later.

A peer that offers no share still enrolls and runs unencrypted, which keeps an
older node working during an upgrade. `--require-encryption` refuses those
peers; set it on any network you do not already trust, because otherwise a
downgrade to plaintext is available to anyone who can strip a field from the
opening hello.

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

Evidence is signed by the reporting node's own identity key, the same key it
proves possession of at enrollment, and verified before it reaches the world
projection. The signature is detached and covers the serialized evidence, so a
field edited in transit fails verification rather than being projected.

Two checks carry this boundary, and only together:

- The signature must verify against an enrolled node's public key, which is what
  keeps an unenrolled peer out.
- The signer must be the node the evidence claims observed it. Without this,
  any enrolled node could attest to another's observations, which is precisely
  the impersonation the attestation exists to prevent.

An attestation also expires. Replay protection on an action envelope does not
cover evidence, which travels the other way, so a captured attestation stops
being accepted once it is older than the configured window.

`--require-attestation` refuses unattested evidence outright. Without it the
control plane falls back to trusting the authenticated channel, which cannot
establish which node made a measurement.

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

Approvals are now signed operator decisions rather than starting-world fields.
`SignApproval` produces an Ed25519-signed grant; `VerifyApproval` is the only
way one becomes authority. An agent has no path to either: it holds no operator
key, and a proposal carries no signature field to smuggle one in.

What is implemented:

- **Authenticated principal.** A grant names its issuer and the key that signed
  it, both inside the signed bytes. The key id must match the key that actually
  signed, so a grant cannot be re-signed by one operator while attributing
  itself to another.
- **Closed scope set.** `ApprovalScopes` enumerates the five decisions the
  kernel gates on. A free-form scope would let a goal or an importer invent an
  authorization the kernel never asked for, and a typo would silently grant
  nothing while appearing to grant something. The projection re-checks the scope
  on replay, so an ungated scope cannot be reintroduced through the log.
- **Mandatory expiry.** Every grant records issuance and expiry, bounded by
  `MaxApprovalLifetime`. An unbounded grant becomes standing permission nobody
  remembers issuing. `hasApproval` checks expiry against the world's observation
  time, so authorization is evaluated deterministically alongside every other
  policy check.
- **Recorded revision and reason.** The grant carries the world revision the
  operator saw and their own words. The revision is advisory — refusing on drift
  would make approvals unusable on a live cluster — but it is what makes a later
  review meaningful.
- **Authenticated revocation.** Withdrawing a grant is as consequential as
  issuing one, so it requires the same signature. A revoked grant is kept rather
  than deleted: an operator reviewing history needs to see that a grant existed
  and was withdrawn, not an absence that looks like it was never issued.
- **No signature in the log.** Evidence records that an approval was accepted
  and by whom. Storing the signed bytes would put a replayable authorization
  into a file meant to be readable.

Remaining: a server with no configured operator keys refuses every approval,
which is the correct default but means key distribution is still manual.

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

This is implemented. `SecretRef` carries a name, version, and mount path, and
has no field capable of holding a value: a struct with nowhere to put a secret
cannot leak one however it is serialized, logged, or handed to a model.

Material is sealed to a node's X25519 key, derived from the Ed25519 identity it
already uses for enrollment, so an operator manages one key per node. The
control plane distributes material it cannot itself read, and a stolen sealed
file is useless without that node's key. Name, version, and node are bound into
the ciphertext, so a renamed file cannot impersonate another secret. `a4s seal`
performs this encryption; it reads the material from a file rather than an
argument, because a command line is visible in shell history and process
listings.

The node decrypts into a tmpfs directory at mode `0400` and binds it read-only,
`nosuid`, `nodev`, `noexec` into the container. Deleting an allocation removes
its material. Evidence reports the name, version, and mount path only.

`SecretMaterial` refuses to marshal and renders as `[redacted]` under every
formatting verb, so a debug print or wrapped error cannot expose it. Decryption
failures report no detail, avoiding an oracle.

Redaction is tested by running a real reconciliation with a secret and scanning
every serialized artifact — goal, world, events, plan, explanation, diagnosis —
for the value. That catches a future field that carries material, not just
today's paths.

Remaining gaps: material is not zeroed from process memory after writing, and
rotation replaces the allocation rather than remounting in place.

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

### Implemented model integration: diagnosis

The first model-backed agent explains why a goal did not converge. It was chosen
because it is the smallest useful surface: it proposes nothing, holds no
capability grants, and cannot mutate anything. A model influences what an
operator reads, never what the kernel executes.

The controls above map to code as follows:

- **Isolated runtime and identity.** The model client lives in `reason`, outside
  `control`. Nothing in `control` imports it, so the kernel and its deterministic
  agents build and run with no model provider present.
- **Redacted input.** `control.BuildModelContext` is the only supported way to
  produce model input. It is built by subtraction: a field reaches a model only
  because someone copied it there, so a new field on `World` or `Event` does not
  silently become model input. Secret versions and mount paths, image digests,
  spend amounts, task payloads, and other workloads' allocations are excluded.
  Operator text is stripped of control characters, because a goal objective
  containing role markers would otherwise read as instructions.
- **Strict decoding.** `control.DecodeModelDiagnosis` refuses unknown fields,
  oversized responses, and findings above a count limit, and drops any target the
  world does not contain. The decoded type has nowhere to put an action, a
  proposal, or a capability.
- **Bounded context.** History is capped at `MaxModelEvents`, messages are
  truncated, and the response is size-limited before decoding.
- **Deterministic fallback.** Every failure of the model path — provider down,
  timeout, malformed output, a response naming things that do not exist — lands
  on `LogDiagnoser`. A model can improve an explanation; it can never remove one.
- **Full audit.** Every diagnosis records model id, template version, world
  revision, event count, and whether it fell back, as `diagnosis.recorded`
  evidence. The context and the model's raw output are not stored: the audit
  answers what produced an explanation, which does not require keeping what was
  sent.

One projection rule is load-bearing: `diagnosis.recorded` changes no world state
**and does not advance the world revision**. Advancing it would let a read-only
explanation invalidate every in-flight proposal through the stale-revision check,
which is a way for a model-influenced artifact to disrupt reconciliation without
ever being authorized to act.

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

Closed:

- Authenticated server API and separately authenticated approvals. Operator
  requests carry a signed envelope bound to method, path, and body, made
  single-use by a nonce ledger.
- Mutual controller/node authentication and encrypted transport.
- Controller key rotation, through an active/accepted/retired keyset that
  rotates without a coordinated fleet restart.
- Node-attributed, expiring observations. Evidence carries the reporting node's
  identity and observation time, and stale readiness stops satisfying a goal.
- Enforced target leases and conflict recovery.
- Tested backup and restore, including detection of a truncated archive by
  anchoring the chain head outside the file.
- Stop/delete lifecycle with operator-approved compensation and crash
  reconciliation.
- Independent readiness and liveness probes.
- CNI and network-policy enforcement compiled from typed intent, verified
  against a real kernel.
- Single-writer stateful ownership: volumes are generation-fenced durably on the
  node, and handoff is gated step by step on evidence.
- Fuzzing of scenario validation, model output decoding, approval verification,
  evidence projection, and event-store opening.
- Validation against live containerd on linux/amd64 and linux/arm64.

- Per-node evidence signing. Every observation carries a detached signature made
  with the reporting node's enrollment identity key, verified before the evidence
  reaches the world projection. The signature covers the node id and observation
  time, and the signer is checked against the node the evidence claims made the
  measurement, so one enrolled node cannot attest for another.
- External audit-hash anchoring. Chain heads are witnessed in an append-only file
  outside the store and checked before the projection is rebuilt, which detects
  wholesale replacement of a log whose own chain verifies.
- Container confinement beyond the OCI baseline: a default seccomp profile, and
  optional AppArmor, non-root user, read-only root, and user namespaces. The
  profile is host configuration, so an authorized action cannot request weaker
  confinement.
- Least-privilege service units with signal handling and graceful shutdown.

Still required:

- Gateway snapshot authenticity. The node applies whole snapshots, but the
  gateway does not verify their provenance independently.
- Secret rotation without workload restart.
- Resource and request limits on the agent runtime surface. Operator API
  decoders are already bounded before authentication.
- Non-root containers by default. The mechanism exists; the default is still the
  image's own user, because changing it breaks images not written for it.
- Penetration and sustained-failure testing beyond the verified round trip.

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
