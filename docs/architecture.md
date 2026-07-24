# a4s: an agentic replacement for K3s

## Scope correction

a4s is not an orchestrator specialized for AI-agent workloads. It is a general
infrastructure orchestrator whose control plane is composed of agents.

An nginx container, PostgreSQL instance, background worker, static website, or
AI runtime are all ordinary workloads. “Agentic” describes how the platform
understands goals, makes plans, reacts to evidence, and repairs the system.

### The word “agent” names two different objects

A **control agent** is control-plane machinery: a registered decision-maker that
proposes typed plans and holds `ActionKind` capability grants. This document is
about those.

An **agent workload** is scheduled cargo: a container whose cost is measured in
tokens rather than cpu-seconds, which acts on the world through granted tools.
It proposes nothing and holds no infrastructure capability. It is a workload
kind alongside service, task, and database, declared by a `runtime` block the
way a database is declared by an `engine`.

The two never share an authority path, and their grant vocabularies are
deliberately disjoint: a control agent's grants are actions the kernel executes,
an agent workload's grants are tools that mean nothing to the kernel. See
[agent workloads](agent-workloads.md).

## Why build this instead of another Kubernetes distribution

K3s packages Kubernetes into a smaller operational unit, but its conceptual
unit remains the Kubernetes object graph. Deploying one application can involve
a Namespace, Deployment, Service, Ingress, Certificate, Secret annotations,
PVC, NetworkPolicies, RBAC, an ArgoCD Application, and operator-specific custom
resources. Each controller understands only its resource slice.

The project originated inside a larger `base_infrastructure` GitOps repository.
That snapshot demonstrated both the strength and cost of the Kubernetes model:

- 17 Deployments, 2 StatefulSets, Jobs, a CronJob, and 6 PVCs represent the
  actual applications.
- 44 NetworkPolicies, 25 Kustomizations, 15 Namespaces, 12 ArgoCD Applications,
  10 Ingresses, 8 issuers, quotas, limits, RBAC, MetalLB objects, monitoring
  CRDs, and operator resources surround them.
- K3s agents run kubelet, containerd, and CNI on every node. The current
  Flannel-over-Tailscale path adds a second network overlay and has required
  restart tuning, a watchdog, and node-local DNS mitigation.

The alternative is not to remove reconciliation. It is to reconcile outcomes
through a smaller set of stable primitives and let specialized agents compose
plans across domains.

## What “agentic” means here

An a4s control agent is a registered decision-maker with:

- A role and event subscriptions.
- A bounded view of observations and prior decisions.
- An explicit set of action kinds it may propose.
- Time, token, action-count, and blast-radius budgets.
- A required explanation, expected evidence, and compensation plan.
- No direct credentials for containerd, the network, storage, or hosts.

Agents may be deterministic Go modules, WASM components, local models, remote
models, or human-operated planners. The authority contract is identical.
Model-backed reasoning is an enhancement, never a bootstrap dependency.

The built-in deterministic agents keep ordinary reconciliation working when a
model provider or the internet is unavailable. Model-backed agents are most
valuable for diagnosis, choosing among safe alternatives, planning unusual
migrations, optimizing placement, and synthesizing recovery plans.

## Non-negotiable invariants

1. **Agents propose; they do not mutate.** Only the deterministic executor has
   host capabilities.
2. **Observations are facts, not prompts.** Node daemons and probes sign or
   attest evidence; an agent cannot claim its own plan succeeded.
3. **Every proposal names a world revision.** A stale proposal is rejected.
4. **The whole plan is checked before its first action.** Capacity, policy,
   dependencies, conflicts, approvals, and compensations are simulated.
5. **Actions are typed and idempotent.** There is no general remote shell in the
   control protocol.
6. **Hard policy is code/data outside the model.** Image provenance, privilege,
   secret scope, public exposure, protected volumes, and resource ceilings are
   not negotiable through reasoning.
7. **Blast radius is bounded.** The kernel limits actions per proposal and
   simultaneous disruption per service, cell, and node.
8. **Verification is independent.** A verifier consumes fresh probes after
   execution and decides whether the goal is satisfied.
9. **All decisions are auditable.** Goals, observations, proposals, denials,
   approvals, actions, evidence, and operator interventions form an append-only
   event log. Secret values never enter it.
10. **The platform boots without the platform.** Server and node daemons are
    static binaries launched by systemd; core recovery does not require a
    healthy workload scheduler.

## Native control objects

The small stable vocabulary is:

### Goal

An outcome requested by an operator, Git source, API client, schedule, or
higher-level agent. A goal combines a service/task/database outcome, success
conditions, constraints, risk policy, and required approval scopes. Actual
approvals are separately authenticated operator events, never fields an agent
or goal may self-assert.

Example: “Keep two healthy API replicas on the `sumi` cell, expose HTTPS at
`api.sumi.finance`, never move the PostgreSQL volume automatically, and keep
one replica available during rollout.”

### Observation

An immutable fact with source, time, expiry, and world revision: node capacity,
container state, image presence, probe result, certificate expiry, route
reachability, snapshot checksum, or provider availability.

### Agent

A control-plane actor registered with a role, subscriptions, proposal schema,
capability grants, budgets, and runtime. Agents do not own infrastructure state;
they produce proposals against observed state.

### Proposal

A revision-bound plan containing reasoning, typed actions, dependencies,
preconditions, expected evidence, risk, and compensating actions. Multiple
agents can submit competing proposals. The coordinator may rank them, but the
kernel independently validates the selected one.

### Action

A narrow capability such as `pull_image`, `create_allocation`,
`attach_network`, `mount_volume`, `start_allocation`, `publish_route`,
`rotate_certificate`, `snapshot_volume`, or `restore_snapshot`.

### Policy

Deterministic constraints and approval gates. Policy decides what is possible;
agents decide what is useful within that space.

### Evidence

Executor- or probe-produced proof of the observed result. Goal completion is a
query over evidence, not a status string written by a controller.

## Architecture

```text
                         operator / Git / API
                                  |
                                  v
                    +---------------------------+
                    |         a4s server        |
                    |                           |
                    | goal + event log (SQLite) |
                    | materialized world view   |
                    | agent coordinator         |
                    | deterministic policy      |
                    | plan simulation + leases  |
                    +-------------+-------------+
                                  |
                    signed typed action stream
                                  |
              Tailscale transport + node identity
                  +---------------+---------------+
                  |                               |
          +-------v--------+              +-------v--------+
          | a4s node: base |              | a4s node: nova |
          |                |              |                |
          | action executor|              | action executor|
          | containerd/runc|              | containerd/runc|
          | CNI + nftables |              | CNI + nftables |
          | volume manager |              | volume manager |
          | probes + logs  |              | probes + logs  |
          | local gateway  |              | local gateway  |
          +-------+--------+              +-------+--------+
                  |                               |
                  +---------- evidence -----------+
```

### a4s server

The first real deployment is a single server on `base` with an embedded SQLite
WAL database. The database stores the append-only event log and rebuildable
materialized views. That matches the current single-server control plane and
keeps backup and recovery understandable.

High availability is not a v1 requirement. If the workload proves that three
control-plane servers are justified, the event log can move behind Raft. Do not
introduce consensus merely to claim cluster semantics: node daemons continue
running their last accepted allocations when the server is unavailable.

### a4s node

One static daemon per Linux node:

- Registers node identity, labels, installed capabilities, and capacity.
- Maintains an outbound authenticated stream to the server over Tailscale.
- Executes only signed, leased, typed actions.
- Uses containerd for OCI content, snapshots, containers, and tasks.
- Invokes standard CNI plugins for network namespace lifecycle.
- Owns local volume mounts, health probes, allocation logs, and garbage
  collection.
- Reports evidence and heartbeats; it does not make cluster-wide decisions.

The server can fail without killing workloads. The node refuses new actions
whose leases or signatures are invalid and enters an observable disconnected
mode.

### Agent coordinator

The coordinator turns state changes into evaluation sessions. It sends the
same bounded context to relevant agents, accepts proposals, removes structurally
invalid candidates, and selects one or asks for operator approval. It is not an
executor and cannot bypass the policy kernel.

### Deterministic kernel

The kernel is the trusted core. It:

- Checks agent capability grants.
- Rejects stale world revisions.
- Acquires target leases and detects conflicting plans.
- Simulates every action and dependency against a cloned world.
- Recalculates resource fit and placement constraints.
- Enforces image, privilege, network, secret, and volume policy.
- Requires approvals for public exposure, destructive storage actions, and
  changes above the configured blast radius.
- Issues short-lived signed action envelopes to node daemons.
- Verifies evidence and triggers compensation or escalation on failure.

This core should remain small enough to reason about and fuzz thoroughly.

## Data plane choices

The goal is to replace K3s, not rewrite mature Linux plumbing.

### Containers: keep containerd and runc

containerd already separates persistent container metadata from live tasks,
manages OCI images and snapshots, and exposes a supported Go API. a4s should use
its native client rather than implement an OCI runtime or pretend Docker CLI
calls are an orchestration API.

Initial action mapping:

| a4s action | data-plane operation |
|---|---|
| `pull_image` | containerd content fetch by digest |
| `create_allocation` | snapshot + OCI spec + container metadata |
| `start_allocation` | containerd task start |
| `stop_allocation` | task signal, wait, kill deadline |
| `delete_allocation` | task/container/snapshot cleanup after policy |

### Networking: keep CNI, remove the cluster overlay

CNI is a small runtime-to-plugin contract with lifecycle operations including
ADD, DEL, CHECK, and GC. The first node implementation can use the reference
`bridge`, `host-local`, `portmap`, and `firewall` plugins.

Each node receives a local allocation subnet. Cross-node traffic does not need
pod-IP transparency. A named service resolves to healthy node gateway
endpoints; gateways communicate over Tailscale. This avoids Flannel VXLAN over
the Tailscale WireGuard mesh and removes the failure mode currently documented
in the repository.

```text
allocation -> node bridge -> local service gateway
                              |
                         Tailscale node IP
                              |
remote allocation <- remote service gateway
```

The network agent proposes service endpoints and policy. The kernel compiles
approved intent into gateway routes and nftables rules. Workloads receive DNS
names, not stable cluster IP assumptions.

This trades Kubernetes-transparent pod routing for a materially simpler and
more observable service boundary. Direct allocation-to-allocation routing can
be added later if a real workload requires it.

### Public ingress and TLS

Run one local gateway per public node, initially Caddy or Envoy. The gateway is
not a control plane; it consumes an atomic, signed route snapshot. It handles
HTTP/TCP routing and ACME certificates. A network agent can reason about route
placement and certificate renewal, while the kernel requires explicit approval
for new public exposure.

This replaces NGINX Ingress, MetalLB, and cert-manager for the current
single-public-IP-per-cell layout.

### Service discovery

The server maintains a directory of service names to healthy gateway
endpoints. Node-local DNS serves that directory with a last-known-good cache.
Discovery derives only from verified allocations; agents cannot directly write
healthy endpoints.

### Storage

Volumes are explicit durable objects with an owner, node affinity, filesystem,
backup policy, and last verified snapshot. Local storage stays local by
default.

- A stateful allocation is never automatically duplicated or relocated merely
  because a heartbeat disappeared.
- Movement requires quiescence evidence, a verified snapshot/checkpoint,
  transfer, restore verification, and an explicit ownership handoff.
- A storage agent may propose the sequence, but destructive steps require a
  kernel lease and usually operator approval.
- Restic/object storage or filesystem-native snapshots can back the first
  implementation.

PostgreSQL deserves a database-specific control agent eventually: backup,
restore, replication health, promotion, and schema migration are domain actions,
not generic container restarts. Until that agent exists, PostgreSQL migration
remains late in the replacement plan.

### Secrets

Secrets never enter goals, agent context, proposals, events, or logs.

The node receives opaque, node-scoped encrypted material from a secret broker,
decrypts it into a tmpfs or runtime credential mount, and reports only secret
version evidence. Initial backends can be encrypted files keyed to node
identity or the existing Vault. The interface must allow replacing Vault
without changing workload goals.

### Observability

Node daemons emit allocation logs, metrics, traces, events, and probe evidence.
OpenTelemetry collectors, VictoriaMetrics, Grafana, and log storage can run as
ordinary workloads. Kubernetes-specific kube-state metrics disappear; the a4s
event log and world view are the platform-state source.

## Scheduling without an omnipotent scheduler agent

Placement has two phases:

1. The kernel computes the feasible set from hard facts: healthy nodes,
   architecture, labels, resources, volume affinity, ports, isolation, and
   policy.
2. A placement agent ranks only that feasible set using soft goals: bin packing,
   failure spread, locality, energy/cost, recent instability, or operator
   preference.

The kernel then recomputes feasibility before accepting the proposal. A model
can explain or improve ranking but cannot place onto an infeasible node.

## Failure behavior

| Failure | Required behavior |
|---|---|
| Model/provider unavailable | Deterministic agents continue known reconciliation; novel decisions wait. |
| a4s server unavailable | Existing allocations and last route snapshot continue; nodes reject expired new actions. |
| Node disconnected | Mark observations stale; do not duplicate stateful workloads automatically. |
| Action fails | Record evidence, stop dependent actions, run authorized compensation, then re-observe. |
| Agent loops or thrashes | Action and round budgets trip; goal becomes blocked with evidence. |
| Conflicting proposals | Target leases and revision checks reject the loser. |
| Bad rollout | Availability policy stops blast radius; rollout agent proposes rollback to prior digest. |
| Secret backend unavailable | Existing mounted version continues until policy expiry; no secret is exposed to an agent. |

## K3s replacement map

| K3s/Kubernetes responsibility | a4s replacement |
|---|---|
| Kubernetes API objects | Goals, policies, observations, events, materialized world |
| kube-apiserver | Small goal/event API on a4s server |
| controller manager/operators | Registered domain control agents |
| scheduler | Kernel feasibility + placement agent ranking |
| kubelet | a4s node daemon |
| containerd/runc | Retained directly |
| Flannel and kube-proxy | Per-node CNI bridge + service gateways over Tailscale |
| CoreDNS | Node-local DNS backed by verified service directory |
| Service/Ingress | Service and route goals, gateway snapshots |
| cert-manager | Gateway ACME plus certificate agent/evidence |
| local-path provisioner | Explicit node-affine volume manager |
| Secrets/Vault injector | Opaque secret broker and tmpfs credential mounts |
| ArgoCD | Git source agent submitting versioned goals |
| Helm/Kustomize | Recipes that compile high-level goals; no Kubernetes API compatibility layer |
| NetworkPolicy | Policy intent compiled to gateway/nftables rules |
| probes and restart policy | Node supervisor plus external evidence |
| etcd/Kine/SQLite object store | Append-only SQLite event log; optional Raft only when justified |

## Compatibility stance

Do not implement the Kubernetes API. That path ends with recreating Kubernetes.

Build a one-way importer that converts the required subset from the originating
GitOps repository into a4s goals and flags unsupported semantics. Helm charts
cannot remain the native package format; important infrastructure components
need a4s recipes or direct OCI/service definitions. The importer is a migration
tool, not a permanent compatibility promise.

## Reference migration for the originating setup

The names and workloads below preserve the environment that motivated a4s.
They are migration context, not dependencies of this standalone folder.

### Phase 0: control-kernel spike

Status: implemented in this directory.

- Generic web-service goal and observed two-node world.
- Placement and networking agents.
- Capability grants, full-plan simulation, revision checks, capacity and label
  policy, public-route approval, evidence, and independent verification.
- In-memory executor for deterministic control-loop simulation.
- Hash-chained durable event file, signed node envelopes, and durable node
  idempotency ledger.

### Phase 1: one-node container runtime

Status: implementation in progress; live Linux validation remains.

- `a4s node` stream harness and containerd native API adapter implemented.
- Pull/create/start implemented with an OCI-hardening baseline.
- Stop/delete, restart supervision, and independent process probes remain.
- Persist the server event log in SQLite.
- Use systemd only to supervise `a4s server`, `a4s node`, containerd, and
  tailscaled.

Exit criterion: the example service survives daemon restart and converges from
an empty node using only typed actions and verified evidence.

### Phase 2: local network and gateway

- Invoke bridge/host-local/portmap/firewall CNI plugins.
- Add allocation DNS and local service gateway.
- Route cross-node services through Tailscale node endpoints.
- Add Caddy/Envoy route snapshot and ACME support.
- Compile basic ingress/egress policy to nftables.

Exit criterion: a stateless copy of `homepage` or another low-risk project runs
in parallel with K3s and serves an alternate domain end to end.

### Phase 3: secrets, Git, rollout, and schedules

- Add opaque secret mounts and secret-version evidence.
- Add Git source agent and signed goal revisions.
- Add rolling update, canary, rollback, batch task, and schedule agents.
- Add logs/metrics export and operator approval flow.

Exit criterion: one existing stateless application moves fully off K3s with
equivalent security, health, deployment, TLS, and rollback behavior.

### Phase 4: durable workloads

- Explicit volumes, snapshot ledger, off-host backup, restore verification.
- Stateful ownership leases and no-duplicate guarantees.
- Redis and simple SQLite-backed tools first.
- PostgreSQL/database agent only after repeatable restore and promotion tests.

### Phase 5: replace platform services

- Move ingress/certificates, monitoring, Git hosting, and secrets in an order
  that preserves an independent recovery path.
- Replace ArgoCD after the Git source agent is proven.
- Replace Vault only after every secret consumer and rotation path is covered.
- Remove K3s from one cell before touching the base node.

### Phase 6: remove K3s

The replacement is complete only when a blank Linux node can join over
Tailscale, restore its authorized volumes/secrets, converge all goals, serve
public and private traffic, and recover from server/node failure without any
Kubernetes component.

## What not to build yet

- A Kubernetes-compatible API or CRD system.
- A generic shell capability for control agents.
- Multi-server consensus.
- Transparent cross-node allocation IPs.
- Automatic relocation of stateful workloads.
- A model dependency in bootstrap or steady-state health repair.
- A general plugin marketplace before the trusted action vocabulary settles.

The immediate engineering target is deliberately narrower: make the typed
control loop real against containerd on one disposable Linux node. Everything
else should earn its place from that experiment.
