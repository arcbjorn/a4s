# Codebase guide

## Repository layout

```text
.
|-- AGENTS.md                 local engineering instructions
|-- CHANGELOG.md              human-readable change history
|-- CONTRIBUTING.md           contribution expectations
|-- LICENSE                   Apache-2.0
|-- README.md                 project entry point
|-- cmd/a4s/                  CLI composition root
|-- control/                  trusted control vocabulary and kernel
|-- server/                   long-running control plane and operator API
|-- eventlog/                 durable controller event records
|-- node/                     signed dispatch and runtime adapters
|-- reason/                   model-backed control agents
|-- source/                   goals from a versioned repository
|-- obs/                      structured logging and metrics
|-- docs/                     handbook and decision records
|-- examples/                 safe simulation inputs
|-- scripts/                  release, doc-link, and nftables checks
|-- init/systemd/             service units
|-- .github/workflows/        CI
|-- go.mod / go.sum           pinned Go module graph
`-- .go-version               expected Go toolchain
```

The project deliberately has no framework, generated source, or hidden service.

## Package ownership

### `control`

This is the trusted control-plane kernel and should stay independent of
containerd, networking implementations, model SDKs, and transport libraries.

| File | Responsibility |
|---|---|
| `types.go` | Goal, world, agent, proposal, action, evidence, and event vocabulary |
| `validate.go` | Scenario normalization and input validation |
| `agents.go` | Deterministic placement and network reference agents |
| `policy.go` | Capability grants, proposal authorization, plan simulation |
| `executor.go` | Executor interface and simulated memory data plane |
| `engine.go` | Reconciliation coordination, event ordering, verification |
| `projection.go` | Pure, idempotent world projection from evidence |
| `durable.go` | Projection rebuilt from recorded history |
| `approval.go` | Operator grant issuance, expiry, and revocation |
| `keyset.go` | Controller keyset states and rotation rules |
| `lease.go` | Target leases with expiry, so proposals cannot interleave |
| `rollout.go` | Drift retirement, availability floor, and rollback resolution |
| `probe.go` | Readiness declarations and observation freshness |
| `directory.go` / `resolve.go` | Service discovery and cluster-wide name zones |
| `netpolicy.go` | Typed network intent compiled toward nftables |
| `attest.go` | Node evidence signing and verification |
| `schedule.go` | Cron parsing and scheduled-run evaluation |
| `canary.go` | Canary steps, hold durations, and endpoint traffic weights |
| `spread.go` | Failure domains and per-domain replica ceilings |
| `disruption.go` | Cluster disruption budget and per-target failure backoff |
| `clusterbudget.go` | Cluster-wide compute, allocation, and agent spend ceilings |
| `node_lifecycle.go` | Cordon, evacuation planning, and schedulability |
| `remediation.go` | The repair ladder: cordon, retire, evacuate |
| `provenance.go` | Image build attestations and trusted signers |
| `storage_agent.go` | Volume backup, restore verification, and handoff proposals |
| `plan.go` / `explain.go` / `diagnose.go` | Dry run, causal history, and deterministic diagnosis |
| `modelcontext.go` | Redacted model input and explanation provenance |
| `modeldecode.go` | Strict decoding of untrusted model output |
| `engine_test.go` | End-to-end kernel safety and convergence tests |
| `agent_workload_test.go` | Agent-workload budget, tool-grant, drain, and queue rules |

Dependency direction:

```text
control -> Go standard library only
```

Preserve that property unless there is a compelling, recorded reason.

### `reason`

Model-backed control agents. It is separate from `control` precisely so the
kernel keeps the property above: dependencies point inward, and nothing in
`control` imports `reason`.

| File | Responsibility |
|---|---|
| `diagnoser.go` | Model-backed diagnosis with deterministic fallback and provenance |
| `anthropic.go` | Minimal Messages API client implementing `Completer` |

```text
reason -> control -> Go standard library only
```

### `server`

The long-running control plane. It owns the durable event log, rebuilds the
world projection from it on every start, accepts enrolled node connections, and
serves the authenticated operator API.

| File | Responsibility |
|---|---|
| `server.go` | Startup, log ownership, projection rebuild, reconciliation, leases |
| `api.go` | Operator HTTP surface and the request-body limit applied before auth |
| `apiauth.go` | Signed-envelope verification and the single-use nonce ledger |
| `apibody.go` | Carries the pre-read body to handlers, since the signature covers its digest |
| `standby.go` | Follower log ingestion and the anchored promotion gate |
| `cordon_test.go` | Operator cordon durability, attribution, and safeguard counts |
| `approval_test.go` | Durable approval admission and restart survival |

```text
server -> control, eventlog, node, obs
```

### `source`

Goals from a versioned git repository. It is deliberately thin: a repository is a
transport for goal documents, and every goal it reads is admitted through the
same validation as one submitted over the operator API. Nothing here can
authorize anything, and a repository cannot carry an approval.

| File | Responsibility |
|---|---|
| `git.go` | Ref polling, goal decoding, and submission through admission |

Git execution comes from `agentic-git/pkg/gitcmd`, which passes arguments as a
slice with no shell, replaces the environment rather than inheriting it, and
times every invocation out. The mirror is bare, so a repository cannot write a
working tree onto the control plane.

```text
source -> control, agentic-git/pkg/gitcmd
```

### `obs`

Structured logging and in-process metrics for both daemons. It is separate so
neither the kernel nor the node depends on an observability library, and so
diagnostics never contaminate the protocol stream.

| File | Responsibility |
|---|---|
| `log.go` | Level and format selection, refusing an unrecognized value |
| `metrics.go` | Counters and gauges behind a closed set of outcome labels |
| `text.go` | Prometheus text exposition rendering, without a client library |

### `eventlog`

`eventlog.File` implements `control.EventSink`. It appends hash-chained records
to SQLite in WAL mode with `synchronous=FULL`, and validates the chain on
replay.

The chain is kept on top of the database rather than replaced by it: SQLite
establishes that the rows are the ones committed, and the chain establishes
that they are the ones a4s wrote.

It is an event durability primitive rather than a complete server database.
World projections are rebuilt from it in `control`; external hash anchoring and
compaction remain future work.

Dependency direction:

```text
eventlog -> control
```

The anchor is a separate append-only file, not a row in the database. Storing the
witness inside the thing it witnesses would defeat it.

### `node`

The node package owns the host mutation trust boundary.

| File | Responsibility |
|---|---|
| `envelope.go` | Signed action envelope, signing, and verification |
| `dispatcher.go` | Signature gate, idempotency gate, runtime dispatch, read-only probes |
| `file_ledger.go` | Persistent successful-dispatch results |
| `enroll.go` / `channel.go` / `x25519.go` | Authenticated enrollment and the encrypted channel |
| `listen.go` / `remote.go` | Enrolled node sessions and short-lived capability issuance |
| `desired.go` / `supervisor.go` | Durable authorized intent and local reconciliation |
| `container_runtime.go` | Runtime-neutral action-to-container contract |
| `containerd_linux.go` | Linux containerd implementation and OCI profile |
| `containerd_other.go` | Explicit unsupported-platform behavior |
| `cni_linux.go` / `network.go` | Allocation namespaces and addressing through CNI |
| `dns.go` | Node-local resolver serving only the a4s zone, never forwarding |
| `router.go` / `gateway.go` | Atomic route snapshots applied to the Caddy gateway |
| `firewall.go` | nftables installation of the compiled ruleset |
| `volume.go` / `snapshot.go` / `backup.go` / `handoff.go` | Fenced volumes, checksummed snapshots, off-host backup, ownership moves |
| `secret.go` | Node-sealed secret material and tmpfs mounts |
| `database.go` / `postgres.go` | Engine-consistent database backup and readiness |
| `agent.go` | Per-allocation tool envelopes, budget meters, tool-call gate, drain |
| `probe.go` | Readiness measurement and per-kind observer routing |
| `observe.go` | Readiness measured remotely, routed to the node holding the allocation |
| `provider.go` | Model-provider egress measurement, caching, and expiry |
| `queue.go` | Durable work queue, leased claims, and the agent claim gate |
| `runtime_api.go` | Workload-facing `a4s.agent/v1` surface and runtime credentials |
| `*_test.go` | Signature, replay, tamper, contract, and durability tests |

Dependency direction:

```text
node -> control
node/containerd_linux -> containerd v2 client
```

The generic container runtime translates a broad `control.Action` into a much
smaller `ContainerBackend` interface. This makes the authority boundary easy to
fake in tests and prevents containerd types from leaking into the kernel.

### `cmd/a4s`

The CLI is the composition root. It owns flags, file/stdin/stdout behavior, and
wiring concrete implementations together.

Commands:

| Command | Composition |
|---|---|
| `validate` | Scenario decoder and validator |
| `simulate` | Memory executor, built-in agents, kernel, optional event sink |
| `server` | Event log, projection rebuild, node listener, operator API, leases |
| `node` | Key loader, containerd runtime, ledgers, dispatcher, supervisor, enrollment |
| `keygen` / `keys` / `seal` | Key material: single keys, rotatable keyset, sealed secrets |
| `plan` / `explain` / `diagnose` | Read-only introspection over the kernel and history |
| `approve` / `history` | Signed operator grants and recorded-history queries |
| `backup` / `restore` | Verified controller-state archives |
| `submit` / `status` / `events` | Signed operator requests against a running server |
| `version` | Stamped version, commit, and build date |

Keep business and policy logic out of `main.go`.

## Main control call path

```text
cmd simulate
  -> loadScenario
  -> NewMemoryExecutor
  -> NewEngine
  -> Engine.Run
       -> Agent.Propose
       -> Kernel.Authorize
            -> validateAction
            -> applyAction on cloned world
       -> record action.dispatched
       -> Executor.Execute
            -> applyAction on real memory world
       -> record action.completed
       -> verifyChecks
       -> goalAchieved
```

World revision increments once for every successfully executed memory action.
Events observe the current revision at the instant they are recorded.

## Main node call path

```text
cmd node
  -> load trusted controller keyset or public key
  -> OpenContainerd and health check
  -> OpenFileLedger and replay
  -> enroll with the server and agree session keys   (or read stdin)
  -> receive SignedAction over the encrypted channel
  -> Dispatcher.Dispatch
       -> Verify signature, node, and time window
       -> check idempotency ledger
       -> ContainerRuntime.Execute
            -> ContainerBackend Pull/Create/Start
       -> append and sync successful result
  -> return DispatchResult per message
```

Alongside that, the supervisor keeps durable desired state true on its own
interval, which is what lets workloads survive a control-plane outage.

The dispatcher records only successful operations. An execution error remains
retryable. The backend must therefore make every mutation safe to repeat.

## Adding an action kind

An action is a cross-cutting security feature, not only a new switch case.
Change it in this order:

1. Define the action and fields in `control/types.go`.
2. State preconditions, idempotency identity, evidence, timeout, and
   compensation behavior in `docs/control-protocol.md`.
3. Add deterministic validation in `control/policy.go`.
4. Add simulation behavior in `control/executor.go`.
5. Grant it only to the minimum agent IDs in `DefaultPolicy`.
6. Require independent evidence where the mutation alone cannot establish the
   outcome.
7. Add proposal generation to the responsible agent.
8. Add a narrow executor interface and real implementation.
9. Add denial, stale-world, capacity/policy, idempotency, and failure tests.
10. Update the security model and project status.

Do not add a generic `exec`, `shell`, arbitrary OCI spec, arbitrary file-write,
or host-command action. A typed action must expose less authority than the
underlying system API.

## Adding an agent

1. Give the agent one domain role and a stable ID.
2. Implement `Descriptor` and `Propose` without mutation side effects.
3. Add only its required actions to kernel-owned grants.
4. Bind every proposal to the supplied world revision and goal ID.
5. Keep proposals bounded and ordered with explicit dependencies.
6. Declare expected evidence.
7. Test malicious, stale, excessive, and unsupported proposals independently
   of the agent's happy path.

A model-backed agent must implement the same interface and receive no more
authority than a deterministic agent. Model output is untrusted proposal data.

## Adding a persistence backend

`control.EventSink` is intentionally small: `Append` and `NextSequence`. A new
backend must preserve append-before-dispatch ordering, atomic sequence
assignment, durable writes, replay validation, and exclusive-writer behavior.

The current backend is SQLite. Anything replacing it must define:

- Transaction boundaries for event append and materialized projections.
- Single-writer ownership, and what a lost race does. Here a conditional
  chain-head update turns it into a refused append rather than a forked history.
- Recovery after an action is dispatched but completion is absent.
- Backup, restore, and corruption checks.

External anchoring of the latest audit hash remains unimplemented in every
backend. The chain detects edits and reordering; only an outside anchor detects
wholesale replacement.

## Test topology

| Test group | What it protects |
|---|---|
| `control/engine_test.go` | End-to-end control authorization and convergence |
| `eventlog/store_test.go` | Persistence, replay, hash chain, tamper rejection, crash durability |
| `eventlog/backup_test.go` | Archive consistency and read-only verification |
| `node/dispatcher_test.go` | Signature, expiry, target, deduplication |
| `node/acceptance_denial_test.go` | The denial matrix driven over the real protocol |
| `node/file_ledger_test.go` | Durable idempotency replay and corruption handling |
| `node/container_runtime_test.go` | Narrow backend contract and hardened create spec |
| `server/apiauth_test.go` | Envelope binding, nonce single-use, and read authentication |

The Linux adapter is compile-checked because unit-test hosts may not be Linux
and should not mutate a real containerd daemon. Live runtime tests belong on a
disposable node and must use dedicated containerd namespace and paths.
