# Roadmap

The roadmap is ordered by risk reduction. A later phase must not compensate for
an unproven earlier trust boundary. Each milestone has an observable exit
criterion.

## Product direction

a4s aims to replace the Kubernetes/K3s control plane for a small multi-node
Linux environment while retaining mature data-plane components such as
containerd, runc, CNI plugins, Tailscale, filesystems, and an HTTP gateway.

The differentiator is not workload type. It is an agent-first control layer in
which specialized agents propose cross-domain plans and a deterministic kernel
owns authorization, typed capability issuance, bounded mutation, and
verification.

## Current milestone: M0 control and node boundary

Status: complete except for hardware validation, which is the only remaining
exit criterion and the gate on every later milestone's claim to be finished.

Complete:

- Goal/world/proposal/action/evidence vocabulary.
- Deterministic placement and network agents.
- Whole-proposal authorization and simulation.
- Memory reconciliation and hash-chained events.
- Signed node envelopes and durable idempotency ledger.
- Containerd pull/create/start adapter and contract tests.
- Linux cross-build.

Also complete since the original plan:

- Stop and delete actions with capacity release.
- Real process, TCP, and HTTP readiness observation with expiry.
- Durable world projection rebuilt from recorded evidence.
- Node desired-state supervision with a crash-loop budget.
- Controller-to-node protocol and an end-to-end acceptance suite.
- Action replay across node-process restart, proven in test.

Remaining exit work:

- Run against a real disposable Linux containerd.
- Document measured failure behavior on real hardware.

Nothing below can be called complete until this is done. The later milestones
list what is built and tested; none of it has driven a real container runtime.

Exit criterion: a real digest-pinned stateless container reaches independently
verified readiness, and duplicate signed actions after node restart do not
duplicate runtime state.

## M1 server-to-node round trip

Build:

- Long-running single-server process.
- Durable goal, approval, observation, proposal, action, and evidence event API.
- SQLite WAL event log and rebuildable world projection.
- Controller signing-key custody.
- Node enrollment and mutual authenticated transport over the tailnet.
- Short-lived envelope issuance with enforced target leases.
- Node heartbeats, capability inventory, and signed observations.
- Per-message result/error protocol.
- Recovery for dispatched actions without completion events.

Status: built and tested, pending hardware validation. The event log is a
hash-chained file rather than SQLite, which meets the durability and rebuild
requirement the milestone actually stated. Enrollment now agrees session keys
inside the signed handshake, so the transport is encrypted rather than assuming
a private tailnet beneath it. Controller keys rotate through an
active/accepted/retired keyset without a fleet restart.

Keep deterministic built-in agents in process for this milestone. Do not add a
model provider yet.

Exit criterion: from an empty server projection and a joined disposable node,
one submitted goal converges to independently verified container readiness;
server and node can each restart without duplication or loss of authoritative
history.

## M2 complete stateless lifecycle

Build:

- Stop, signal, kill deadline, delete, and snapshot cleanup actions.
- Restart policy and crash-loop budgets.
- Orphan container/task/snapshot discovery.
- Process, TCP, and HTTP probes with expiry.
- Compensation and rollback execution.
- Rolling replacement agent with availability and disruption limits.
- Garbage-collection policy with dry-run evidence.

Status: built and tested, pending hardware validation. Rollback is executed
rather than only detected, gated on an operator approval that records both the
failed and known-good versions. Garbage collection reclaims unreferenced image
storage with a protected set the kernel computes and checks. Canary rollout is
deliberately still absent.

Exit criterion: deploy, update, fail, restart, roll back, and delete a stateless
service without manual containerd mutation, including recovery from server and
node crash points.

## M3 node-local network and gateway

Build:

- CNI ADD, DEL, CHECK, and GC adapter.
- Bridge, host-local IPAM, port mapping, and firewall configuration.
- Allocation network namespace ownership.
- Node-local service directory and DNS cache.
- Local gateway consuming atomic signed route snapshots.
- Tailnet and public exposure approvals.
- Independent route and TLS evidence.
- nftables policy compilation from typed intent.

Avoid transparent cross-node allocation IP routing initially. Route named
services through node gateways over Tailscale.

Status: built and tested, pending hardware validation. A service resolves under
`a4s.internal` from any node, locally to its allocation address and elsewhere
through the owning node's gateway. Typed network intent compiles to nftables,
and the compiler's own output is verified by applying it to a real Linux
kernel. TLS issuance is delegated to Caddy.

Exit criterion: a stateless service is reachable by stable internal name and an
approved alternate public domain, with route removal and certificate recovery
tested.

## M4 sources, schedules, secrets, and observability

Build:

- Git source adapter that submits versioned goals.
- Recipe format for native a4s services and one-way Kubernetes importer.
- Schedule and batch agents.
- Secret broker with opaque node-scoped mounts and version-only evidence.
- Metrics, traces, structured node logs, and event-log query API.
- Operator approval UI or CLI with strong authentication.
- Canary and automatic rollback agents.

Status: partially complete. The secret broker with node-scoped mounts and
version-only evidence, structured daemon logs, metrics, an authenticated
event-log query API, and a strongly authenticated operator CLI are all built.
Rollback is operator-approved rather than automatic, which is deliberate.
Still missing: the Git source adapter, the recipe format and Kubernetes
importer, schedule and batch agents, tracing, and canary rollout.

Exit criterion: one real stateless service moves from K3s to a4s with equivalent
Git deployment, secret handling, monitoring, TLS, health checks, and rollback.

The Git adapter is the remaining blocker for this criterion: goals arrive
through the operator API today, not from a versioned repository.

## M5 durable workload safety

Build:

- Volume identity, node affinity, and ownership leases.
- Snapshot, transfer, restore, and checksum actions.
- Off-host backup policy and scheduled restore tests.
- Quiescence evidence and explicit ownership handoff.
- No-duplicate fencing under network partition.
- Database-specific agent only after generic volume recovery works.

Exit criterion: a low-risk stateful service survives tested backup, host loss,
restore, and ownership handoff without duplicate writers.

## M6 platform migration

Migrate in reversible order:

1. Low-risk stateless application on an alternate endpoint.
2. Additional stateless services.
3. Gateway and certificates while retaining an external recovery path.
4. Git source and deployment automation.
5. Monitoring and logs.
6. Simple stateful services with verified restore.
7. Git hosting and secret infrastructure late.
8. PostgreSQL only after database-specific promotion and restore tests.

Exit criterion: one complete cell can bootstrap, converge, serve traffic, and
recover without Kubernetes components while the old environment remains a
tested rollback path.

## M7 remove K3s

Exit criterion:

- A blank Linux node joins with authenticated identity.
- Authorized goals converge after server and node restart.
- Networking, TLS, secrets, volumes, schedules, and observability recover.
- Public and private services remain within declared policy.
- Backup restore and controller-loss procedures are rehearsed.
- K3s removal from one cell does not remove the independent recovery path.

Only then should the remaining K3s control plane be retired.

## Explicit non-goals

- Kubernetes API compatibility as a permanent feature.
- Reimplementation of OCI runtime, container image, filesystem, VPN, or TLS
  primitives already provided by mature tools.
- Arbitrary shell or host-command actions.
- Model dependency for bootstrap, routine convergence, or basic repair.
- Automatic stateful relocation based only on missing heartbeat.
- Multi-server consensus before single-server recovery is proven.
- Transparent cluster-wide allocation IPs without a demonstrated workload need.
- General plugin marketplace before the action vocabulary stabilizes.
- Supporting every Kubernetes workload or Helm chart.

## Decision gates

Before starting a milestone, answer:

1. What exact failure or workload requires it?
2. What new authority enters the system?
3. Can the kernel validate it deterministically?
4. What evidence proves success independently?
5. What is the idempotency identity and crash window?
6. What is the compensation or safe blocked state?
7. How will it be tested on a disposable target?
8. Does it preserve the ability to operate without a model provider?

If those answers are unclear, refine the protocol before implementing the
adapter.
