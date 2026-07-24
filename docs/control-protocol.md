# Control protocol reference

This document describes the implemented `a4s.io/v1alpha1` control vocabulary
and the current node envelope. It is a behavioral reference, not a generated
schema. The Go types in `control/types.go` and `node/envelope.go` remain the
executable source of truth.

## Design rules

- Desired outcomes are represented as goals, not Kubernetes-style object
  graphs.
- Observed state is revisioned and supplied to agents as a world snapshot.
- Agents return proposals but never execute actions.
- The deterministic kernel authorizes the entire ordered proposal before its
  first mutation.
- Node mutation uses short-lived, signed, typed action envelopes.
- Completion requires evidence; agent reasoning is never evidence.
- Every mutating action must be safe to repeat with the same semantic identity.

## Scenario document

The current CLI accepts a `Scenario` JSON document:

```json
{
  "goal": {},
  "world": {}
}
```

The decoder rejects unknown fields. The scenario wrapper is a simulation and
validation input, not the planned external server API. A future server will
ingest goals, observations, and approvals as separately authenticated events.

## Goal

```json
{
  "api_version": "a4s.io/v1alpha1",
  "id": "web-public",
  "objective": "Keep one healthy web replica publicly reachable.",
  "workload": {
    "name": "web",
    "image": "registry.example/web@sha256:<64 lowercase hex characters>",
    "replicas": 1,
    "port": 8080,
    "resources": {
      "cpu_millis": 100,
      "memory_mb": 128
    }
  },
  "route": {
    "host": "web.example.com",
    "port": 443,
    "exposure": "public"
  },
  "constraints": {
    "required_labels": {
      "pool": "edge"
    },
    "allowed_nodes": ["edge-1"]
  }
}
```

### Goal fields

| Field | Required | Current rule |
|---|---:|---|
| `api_version` | yes | Exactly `a4s.io/v1alpha1` |
| `id` | yes | Lowercase DNS-style label, maximum 63 characters |
| `objective` | yes | Non-whitespace human-readable outcome |
| `workload` | yes | One stateless OCI service specification |
| `route` | no | One `tailnet` or `public` route |
| `constraints` | yes in Go shape | May contain labels or allowed-node IDs |

### Workload fields

| Field | Rule |
|---|---|
| `name` | Lowercase DNS-style label |
| `image` | Immutable reference ending in an exact lowercase SHA-256 digest |
| `replicas` | At least 1 |
| `port` | 1 through 65535 |
| `resources.cpu_millis` | Positive integer |
| `resources.memory_mb` | Positive integer |
| `privileged` | Must be false in v1alpha1 |
| `stateful` | Must be false until volume ownership exists |

`objective` is preserved in the accepted-goal event and can guide an agent, but
hard authorization derives from structured fields and policy.

## World

A `World` is a materialized snapshot of accepted observations and approvals:

| Field | Meaning |
|---|---|
| `revision` | Monotonic version against which proposals are bound |
| `nodes` | Node identity, labels, total and used resources, image presence, health |
| `allocations` | Materialized workload replicas and their phases |
| `routes` | Materialized service routes |
| `approvals` | Separately authenticated operator decisions |

The simulation accepts the starting world from JSON. In the intended server,
agents never submit a replacement world. Projections rebuild it from durable
events and expiring observations.

### Approval

An approval contains `id`, `goal_id`, `scope`, `issued_by`, and `granted`.
Public exposure currently requires a granted approval with scope
`public-route` for the exact goal.

The fact that simulation JSON contains both the goal and approval does not mean
a caller may self-approve in production. The future API must authenticate and
persist approvals separately from goals and proposals.

## Agent and proposal

An agent exposes an authenticated descriptor:

```text
id + role + declared capabilities
```

The kernel does not trust the descriptor's declared capability list as the
grant. It indexes its own policy grants by authenticated agent ID.

A proposal contains:

| Field | Meaning |
|---|---|
| `id` | Proposal identity |
| `agent_id` | Must equal the authenticated descriptor ID |
| `goal_id` | Must equal the evaluated goal |
| `based_on_revision` | Must equal the current world revision |
| `reasoning` | Audit explanation; never authorization or evidence |
| `actions` | Ordered, typed mutation plan |
| `expected_evidence` | Checks required after actions execute |

The default kernel allows at most eight actions per proposal. Action IDs must
be nonempty and unique within the proposal. Every `depends_on` ID must refer to
an earlier action that has already been simulated.

## Implemented actions

### `pull_image`

Required semantic fields: `id`, `kind`, `target`, `node`, and `image`.

Kernel rules:

- Node exists and is healthy.
- Image exactly matches the goal's pinned image.

Node behavior:

- Pull and unpack through containerd.
- Compare the pulled target digest with the requested digest.
- Return `image.present` evidence.

### `create_allocation`

Required semantic fields: `id`, `kind`, `target`, `workload`, `node`, `image`,
`replica`, and `resources`.

Kernel rules:

- Allocation does not already exist in the simulated world.
- Node is healthy and satisfies allowed-node and label constraints.
- Image is present on that node and equals the goal image.
- Workload and resource request equal the goal.
- Node has remaining capacity.
- Replica index is within the goal's desired replica count.

Node behavior:

- Accept a matching existing a4s-managed container as an idempotent repeat.
- Reject an existing container that does not have the requested ownership,
  workload, and image labels.
- Create an OCI container and snapshot with hardened defaults.
- Return `allocation.created` evidence.

### `create_volume`, `attach_volume`, `detach_volume`

Volumes are explicit durable objects with a name, a home node, an owner, and a
generation. The thing that must not be lost has to be nameable independently of
the process using it.

Kernel rules:

- A volume is created on exactly one node. Creating the same name elsewhere
  would silently produce two divergent copies of what the operator thinks is one
  volume.
- A volume may be attached to one allocation at a time. Two processes writing
  one local filesystem is corruption, not scaling, so a workload declaring
  volumes is limited to a single replica.
- A volume is attached only on the node holding its data. Local storage stays
  local.
- A volume is released only from a stopped allocation, and only by its current
  owner. Detaching a running writer would pull storage out from under a live
  process.
- An allocation holding volumes cannot be deleted until it releases them, which
  would otherwise orphan the storage.
- Destroying a stateful allocation requires a granted `destroy-stateful`
  approval. Losing durable data is the one outcome reconciliation cannot undo.

### Ownership fencing

Every ownership change increments the volume's generation, and an allocation
records the generation it attached at. Starting requires those to match.

That is what makes a partition safe. A node that loses contact still believes it
owns its volume; if ownership moves on, the generation advances, and the stale
writer is refused when it tries to start. Releasing also advances the
generation, so a writer detached while unreachable cannot resume against the
generation it remembers.

A missing heartbeat is never treated as evidence that a writer stopped. Placement
proposes nothing for a workload whose data is unreachable rather than starting a
second copy elsewhere.

### `snapshot_volume`

Snapshotting an attached volume is refused: a copy taken from a live writer may
be internally inconsistent, and an operator would later trust it for restore.
The volume must be quiesced first. No snapshot backend is implemented yet.

### `mount_secret`

Required semantic fields: `id`, `kind`, `target`, `workload`, `node`, and
`secret`.

Kernel rules:

- Allocation exists and is still in `created` phase. Credentials must be in
  place before the process starts.
- The reference must be one the goal declared, exactly: name, version, and mount
  path. An agent cannot mount material the operator did not authorize for this
  workload, and cannot substitute a different version.

Node behavior:

- Fetch node-scoped sealed material from the broker and decrypt it with the
  node's identity key.
- Write it to a tmpfs directory at mode `0400` and bind it read-only into the
  container.
- Return the existing mount as an idempotent repeat rather than re-fetching.
- Return `secret.mounted` evidence carrying name, version, and mount path.

The action carries a reference, never material, so a proposal remains safe to
log in full and to show a model. A workload cannot start until every declared
secret is mounted at the declared version; rotating a secret means changing the
goal, which registers as drift.

Releasing happens as part of `delete_allocation`, because a deleted workload
must not leave credentials readable on the node.

### `attach_network`

Required semantic fields: `id`, `kind`, `target`, `workload`, and `node`.

Kernel rules:

- Allocation exists and is still in `created` phase. Attaching after start would
  leave the container running in the wrong namespace.
- Workload equals the allocation's workload.

Node behavior:

- Invoke CNI `ADD` to create the allocation's namespace and address.
- Return the existing attachment as an idempotent repeat rather than creating a
  second namespace, which would strand the first.
- Release the reserved address if the plugin fails, so a failed attach does not
  leak one.
- Reject an empty or malformed address rather than recording it.
- Return `network.attached` evidence carrying the address and namespace.

Each node owns a local allocation subnet and assigns addresses without
coordinating with any other node. Cross-node traffic goes through service
gateways rather than depending on cluster-wide address transparency, so
assignment never needs consensus.

Detachment is not a separately proposed action. It happens as part of
`delete_allocation`, because a deleted allocation must never leave a namespace
or address behind, and an agent could forget to propose the teardown.

### `start_allocation`

Required semantic fields: `id`, `kind`, `target`, and `workload`.

Kernel rules:

- Allocation exists in `created` phase.
- A workload with a port must already have an address. Starting first would
  leave it either unreachable or, without a namespace, contending with its own
  replicas for a host port.
- Workload equals both the goal and allocation workload.
- Proposal declares an `allocation_ready` check for the target.

Node behavior:

- Return the existing running task as an idempotent repeat.
- Otherwise create and start a containerd task.
- Return `allocation.running`, which is not readiness evidence.

No executor returns readiness. The memory executor reports `allocation.running`
exactly as the real node does; an independent prober supplies
`allocation.ready`. See "Evidence and projection" below.

### `stop_allocation`

Required semantic fields: `id`, `kind`, `target`, and `workload`.

Kernel rules:

- Allocation exists and is in the `running` phase.
- Workload equals the allocation's workload.
- Stopping a ready allocation must leave at least the availability floor of
  ready replicas serving. The floor is the goal's replica count minus one, or
  zero for a single-replica workload, which cannot be updated without a gap.

Availability is counted across every replica currently serving the workload,
whatever image it runs. Counting only replicas matching the goal's new image
would read as zero availability at the start of every rollout.

The kernel applies this floor independently of the proposing agent. An agent
that respects its own budget is convenient; an agent that cannot exceed the
budget is a safety property.

Node behavior:

- Signal the task, wait up to the kill deadline, then kill it.
- Report the observed exit code and whether a kill was required.
- Clear local running intent, so the supervisor stops restarting it.
- An already-absent task is reported as `already_gone` rather than an error.

### `delete_allocation`

Required semantic fields: `id`, `kind`, `target`, and `workload`.

Kernel rules:

- Allocation exists and is not running. Stop is a required prior step, so a
  workload is never destroyed without the operator observing it stop.
- Workload equals the allocation's workload.
- Stateful allocations are refused until the volume ownership protocol exists.

Node behavior:

- Refuse to delete a container a4s does not own.
- Remove the container and its snapshot.
- Succeed when the container is already absent, so a replayed delete is safe.
- Forget local desired state for the allocation.

Deletion releases the node capacity that creation charged.

### `publish_route`

Required semantic fields: `id`, `kind`, `target`, `workload`, `port`, and
`exposure`.

Kernel rules:

- Goal requests the exact route.
- Workload equals the goal workload.
- Every desired allocation is ready.
- Public exposure has a granted `public-route` approval.
- Proposal declares a `route_reachable` check for the host.

The node's router owns this action, separately from the container runtime, so
publishing a route and starting a container are distinct capabilities with
distinct blast radius. The router hands the gateway a complete route snapshot
rather than an incremental edit, and restores the previous snapshot if the
gateway refuses the new one. No concrete gateway backend is implemented yet.

## Current capability grants

| Agent ID | Granted actions |
|---|---|
| `placement-agent` | `pull_image`, `create_allocation`, `create_volume`, `attach_volume`, `mount_secret`, `attach_network`, `start_allocation` |
| `network-agent` | `publish_route` |
| `rollout-agent` | `stop_allocation`, `delete_allocation`, `detach_volume` |

An agent cannot acquire another action by returning it in its descriptor or
proposal.

The rollout agent may retire an allocation but may not create one. Replacement
is placement's job, which keeps destruction and creation in separate capability
sets: an agent that can only destroy cannot quietly replace a workload with
something else.

## Reconciliation sequence

```text
goal accepted
    |
    v
fresh world snapshot -> agent proposes against revision R
    |
    v
kernel authenticates actor and simulates every action
    |
    +---- deny ----> denial event -> next agent or blocked goal
    |
  approve
    |
    v
persist action.dispatched -> execute -> project evidence
    |
    v
persist action.completed
    |
    v
independent probes observe -> project probe evidence
    |
    v
verify declared evidence against projected world
    |
    +---- fail ----> goal.blocked
    |
  next revision / next agent / goal.achieved
```

Placement intentionally proposes no more than one missing replica per world
revision. That keeps each mutation batch small and forces re-observation before
the next replica.

## Evidence and projection

Evidence is the only input that advances the world. An action never mutates the
projection directly: an executor performs a host mutation and reports what it
observed, and `control.Project` independently interprets that evidence. A
faulty or adversarial executor can therefore only report evidence, never assert
world state.

`Project` is pure and idempotent. It never mutates its input world, and applying
the same evidence twice yields the same result. That property is what makes
action replay safe after a node crashes between host mutation and recording the
result: the replayed action produces the same evidence, which projects to the
same state instead of double-counting capacity.

Implemented evidence kinds:

| Kind | Produced by | Effect on the world |
|---|---|---|
| `image.present` | Executor | Marks the image present on the observed node |
| `volume.created` | Volumes | Records a durable volume and its home node |
| `volume.attached` | Volumes | Assigns ownership and advances the generation |
| `volume.detached` | Volumes | Releases ownership and advances the generation |
| `volume.snapshotted` | Volumes | Records a verified snapshot id |
| `secret.mounted` | Secrets | Records the mounted secret version, never the material |
| `network.attached` | Network | Records the allocation's own address |
| `network.detached` | Network | Clears the address on teardown |
| `allocation.created` | Executor | Creates the allocation and charges node capacity once |
| `allocation.running` | Executor or supervisor | Moves the allocation to `running`; never sets readiness |
| `allocation.ready` | Prober | Sets readiness on an allocation already observed running |
| `allocation.stopped` | Executor | Moves to `stopped` and clears readiness |
| `allocation.failed` | Supervisor | Records exit code and restart count; clears readiness |
| `allocation.deleted` | Executor | Removes the allocation and releases its capacity once |
| `route.reachable` | Router or gateway | Records the route |
| `route.removed` | Router | Removes the route |

Readiness evidence carries `observed_at` and `expires_at`. An expired readiness
observation stops satisfying a goal, because a service that was healthy when
measured is not necessarily healthy now. State-change evidence such as
`allocation.created` does not expire.

The separation between `allocation.running` and `allocation.ready` is a
security boundary, not a naming detail. The component that started a container
is not permitted to declare that container healthy.

Readiness is supplied by a `MeasuredProber` over a `ReadinessObserver`. On a
node that observer performs a real process, TCP, or HTTP measurement, and it
reports readiness only from a measurement that actually succeeded. A probe that
cannot complete yields no evidence rather than a false negative, because "could
not measure" and "measured as unhealthy" are different facts.

Every readiness observation carries `observed_at` and `expires_at`. Once an
observation expires the allocation stops counting as ready, so a dead workload
cannot keep satisfying a goal on the strength of an old measurement.

An image observed serving is recorded as that workload's known-good version. A
rollout that is later observed failing surfaces that digest as the rollback
target, so a rollback always names a version this cluster actually saw working.
Applying it changes what the operator asked for, so the goal blocks with the
evidence rather than an agent quietly running a different version.

## Events

Every event contains `sequence`, UTC `at`, `type`, `actor`, `goal_id`, current
`world_revision`, and `message`. Proposal, action, and evidence fields appear
when applicable.

Implemented event types:

| Event | Meaning |
|---|---|
| `goal.accepted` | Evaluation began for an accepted goal |
| `proposal.created` | An agent produced a nonempty proposal |
| `proposal.approved` | The kernel authorized the whole proposal |
| `proposal.denied` | Agent error or deterministic authorization denial |
| `action.dispatched` | Mutation intent was durably recorded before execution |
| `action.completed` | Executor returned evidence successfully |
| `observation.recorded` | A prober produced evidence independently of the executor |
| `goal.achieved` | Current world satisfies the goal query |
| `goal.blocked` | Execution, evidence, progress, or round budget failed |

The file event store wraps each event in a record containing `previous_hash`
and `hash`. The first record has no prior hash.

## Signed node envelope

The current envelope version is integer `1` and is independent of the goal API
version.

```json
{
  "envelope": {
    "version": 1,
    "id": "envelope-123",
    "node_id": "edge-1",
    "goal_id": "web-public",
    "proposal_id": "placement-agent-r7",
    "world_revision": 7,
    "lease_id": "lease-123",
    "idempotency_key": "web-0-start-v1",
    "issued_at": "2026-07-22T12:00:00Z",
    "expires_at": "2026-07-22T12:00:30Z",
    "action": {}
  },
  "key_id": "control-1",
  "signature": "<unpadded base64>"
}
```

The signature is Ed25519 over the exact bytes of the `envelope` member as
transmitted, encoded with unpadded standard base64.

The verifier authenticates the received bytes and only then decodes them. It
never re-serializes a decoded struct to reconstruct the signed payload, because
that would authenticate the verifier's own encoding rather than the sender's:
any payload that decoded to an equivalent struct would pass, and the signature
would silently depend on encoder stability across Go versions, field additions,
and languages. Verifying the received bytes removes that dependency, so a
cross-language signer only needs to reproduce bytes, not Go's encoder.

Decoding rejects unknown fields and trailing content, so a node never executes
an envelope it did not fully understand.

Envelope validation requires:

- Version 1.
- Nonempty envelope, node, goal, proposal, lease, and idempotency identities.
- Expiry after issue time and no more than five minutes later.
- Action node either empty or equal to envelope node.
- Runtime clock no earlier than thirty seconds before issue time.
- Runtime clock strictly before expiry.
- Known local key ID and valid signature.

`lease_id` is issued by the kernel's lease manager, which claims every target a
proposal will mutate before the first mutation and releases them when the plan
finishes. Revision binding alone is not sufficient: two proposals built against
the same revision are both non-stale, and the second would still execute against
state the first has begun changing.

Leases expire, so a controller that dies mid-plan does not block its targets
permanently. Acquisition is all-or-nothing, so a proposal cannot hold part of a
plan while another holds the rest.

The node keeps a local backstop that refuses an envelope contradicting a live
lease it already accepted for the same target. That catches a controller which
lost track of its own leases; it does not replace kernel-side exclusion.

## Dispatch response

The node emits exactly one response per received envelope. A rejected or failed
action does not terminate the node.

Success:

```json
{
  "envelope_id": "envelope-123",
  "result": {
    "envelope_digest": "<hex sha256 of the authorized work>",
    "evidence": {
      "kind": "allocation.running",
      "target": "web-0",
      "observed": {
        "pid": "1234",
        "already_running": "false"
      }
    }
  }
}
```

Failure:

```json
{
  "envelope_id": "envelope-123",
  "error": "action envelope expired"
}
```

`envelope_id` on a failure response is read from unauthenticated input and is
reported only to correlate the failure. It must not be treated as trustworthy.

The only condition that still terminates the reader is an undecodable frame,
because a malformed frame desynchronizes the stream and the reader cannot
determine where the next envelope begins.

Repeating the same idempotency key for the same work returns the stored result
without invoking the runtime. Reusing the key for different work is rejected.

The ledger compares a digest of the *authorized work* — node, goal, proposal,
idempotency key, and action — not of the whole envelope. A controller that
retries after a timeout issues a fresh envelope with new issue and expiry times
and a new envelope ID, but it is requesting the same work. Comparing whole
envelopes would reject that honest retry while providing no additional
protection, since the signature already covers the envelope.

## Versioning policy

Until a stable release:

- Additive JSON changes require explicit defaults and tests.
- Removing, renaming, or changing field meaning requires a new API or envelope
  version.
- New actions require kernel policy, simulation, real executor behavior,
  evidence semantics, capability grants, security review, and tests together.
- Unknown fields remain rejected to prevent silent policy bypass.
- Do not infer authority from a newer field when an older node cannot validate
  it.

No compatibility guarantee exists for v1alpha1 yet. Record deliberate breaking
changes in `CHANGELOG.md` and an architectural decision record when they alter
the trust model.

## Node enrollment

A node connects to the server and proves its identity before receiving any
capability:

```text
node  -> hello      { version, node_id }
server -> challenge { version, nonce, server_key_id }
node  -> proof      { version, node_id, signature }
server -> enrolled  { version, accepted, server_key_id }
```

The node signs a canonical encoding of the protocol version, its node ID, and
the server's random 32-byte nonce. Binding the proof to a fresh nonce is what
makes enrollment replay-resistant: a captured proof authenticates only the one
connection it was produced for.

Rules:

- The server learns which node it is talking to by verification, never by
  assertion. A `hello` is a claim; only the proof establishes identity.
- The proof must name the same node ID as the hello, so a peer cannot claim one
  identity and prove another.
- A node absent from the server's enrolled key set cannot connect.
- Refusals are generic on the wire. An unenrolled prober cannot distinguish
  "unknown node" from "bad signature" and so cannot enumerate node identities.
  The server log records the specific cause.
- The node refuses a server that names a signing key the node does not already
  trust, so a reachable impostor cannot nominate its own key.
- A reconnecting node replaces its prior session, and the stale connection is
  closed rather than left able to receive capabilities.

Enrollment establishes *who* the peer is. It does not encrypt the channel. The
transport is intended to run over an already-encrypted tailnet; on an untrusted
network it requires TLS beneath it.

## Operator introspection

Three read-only capabilities fall out of recording reasoning and authorization
alongside mutation. None of them can change anything.

### Explanation

`Explain` reconstructs the causal chain for one target. Relevance is transitive:
an event matters if it names the target, or if it belongs to a proposal that
acted on the target. That second hop recovers the agent's reasoning and the
kernel's authorization, which name the proposal rather than the target.

A reconciliation loop that stores only desired and observed state cannot answer
"why does this exist" after the fact, because the decision was never durable.
Here it is.

Derived outcomes:

| State | Meaning |
|---|---|
| `serving` | Last observed ready or reachable |
| `pending` | Work dispatched with no completing evidence; the crash window |
| `failed` | Last decisive event was a failure or blockage |
| `removed` | Deliberately stopped or deleted |
| `unknown` | No decisive outcome recorded |

### Planning

`DryRun` reports what reconciliation would do without touching anything. It runs
the real agents and the real kernel against a cloned world, so a plan cannot
disagree with execution through drift between two code paths.

Simulation optimistically assumes a started allocation becomes ready, because
the kernel must be able to authorize a whole plan before its first mutation.
Anything planned after that assumption is marked contingent. Publishing a route
depends on a probe measuring the workload as ready, and a plan that promised the
route unconditionally would be lying about what it knows.

### Diagnosis

`Diagnoser` turns recorded history into an explanation of why a goal is not
converging. Findings are ordered most-specific first, and an action dispatched
without a completion is reported before anything else, because it is the only
finding implying the recorded world may disagree with the host.

A blockage often reports only that no agent could act, while the denial that
preceded it holds the specific reason. Diagnosis prefers the specific one.

The diagnoser is the one place where model-backed reasoning is unambiguously
safe. It reads events and produces text: no capability grants, no proposals, no
mutation. A wrong diagnosis misleads an operator; it cannot break anything.

## Service discovery

A directory maps each workload to the endpoints currently serving it. Three
conditions must all hold for an allocation to appear:

- Readiness was measured and has not expired.
- CNI has given it an address.
- An accepted goal declares the port it listens on.

The directory is derived, never asserted. There is no action by which an agent
can add an endpoint, and an allocation that stops being measured as ready falls
out on the next build. Routing traffic to an instance nobody has recently
observed serving is exactly how a rollout becomes an outage.

The workload port and the route port are deliberately different. The workload
port is what an endpoint is dialed on inside its namespace; the route port is
what the gateway listens on.

## Gateway snapshots

The gateway receives a complete configuration and replaces its own with it. It
never merges, never decides, and never learns endpoints on its own, so routing
stays exactly as authorized and cannot drift incrementally.

A route whose workload has no healthy endpoint is still included, carrying an
empty endpoint list, and the gateway answers 503 for it. Dropping the route
would make the hostname fall through to an unrelated site, turning a transient
outage into a wrong answer.

Public routes get ACME certificate automation. A tailnet route has no public
DNS, so ACME cannot issue for it; internal issuance is used instead of silently
serving no TLS.
