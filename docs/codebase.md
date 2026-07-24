# Codebase guide

## Repository layout

```text
.
|-- AGENTS.md                 local engineering instructions
|-- CHANGELOG.md              human-readable change history
|-- README.md                 project entry point
|-- cmd/a4s/                  CLI composition root
|-- control/                  trusted control vocabulary and kernel
|-- eventlog/                 durable controller event records
|-- node/                     signed dispatch and runtime adapters
|-- docs/                     handbook and decision records
|-- examples/                 safe simulation inputs
|-- go.mod / go.sum           pinned Go module graph
`-- .go-version               expected Go toolchain
```

The project deliberately has no framework, generated source, hidden service,
or parent-repository import.

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
| `engine_test.go` | End-to-end kernel safety and convergence tests |
| `agent_workload_test.go` | Agent-workload budget, tool-grant, drain, and queue rules |

Dependency direction:

```text
control -> Go standard library only
```

Preserve that property unless there is a compelling, recorded reason.

### `eventlog`

`eventlog.File` implements `control.EventSink`. It appends newline-delimited,
hash-chained records and validates them on replay.

It is currently an audit/event durability primitive, not a complete server
database. World projections, observation expiry, external hash anchoring,
compaction, and SQLite are future work.

Dependency direction:

```text
eventlog -> control
```

### `node`

The node package owns the host mutation trust boundary.

| File | Responsibility |
|---|---|
| `envelope.go` | Signed action envelope, signing, and verification |
| `dispatcher.go` | Signature gate, idempotency gate, runtime dispatch |
| `file_ledger.go` | Persistent successful-dispatch results |
| `container_runtime.go` | Runtime-neutral action-to-container contract |
| `containerd_linux.go` | Linux containerd implementation and OCI profile |
| `containerd_other.go` | Explicit unsupported-platform behavior |
| `agent.go` | Per-allocation tool envelopes, budget meters, tool-call gate, drain |
| `probe.go` | Readiness measurement and per-kind observer routing |
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
| `simulate` | Memory executor, built-in agents, kernel, optional file event sink |
| `node` | Public-key loader, containerd runtime, file ledger, dispatcher stream |
| `version` | Development version string |

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
  -> load trusted Ed25519 public key
  -> OpenContainerd and health check
  -> OpenFileLedger and replay
  -> decode SignedAction from stdin
  -> Dispatcher.Dispatch
       -> Verify signature, node, and time window
       -> check idempotency ledger
       -> ContainerRuntime.Execute
            -> ContainerBackend Pull/Create/Start
       -> append and sync successful result
  -> encode DispatchResult to stdout
```

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

Before replacing the file store with SQLite, define:

- Transaction boundaries for event append and materialized projections.
- Single-writer ownership.
- Recovery after an action is dispatched but completion is absent.
- Backup, restore, and corruption checks.
- How the latest audit hash is anchored outside the database.

## Test topology

| Test group | What it protects |
|---|---|
| `control/engine_test.go` | End-to-end control authorization and convergence |
| `eventlog/file_test.go` | Persistence, replay, hash chain, tamper rejection |
| `node/dispatcher_test.go` | Signature, expiry, target, deduplication |
| `node/file_ledger_test.go` | Durable idempotency replay and corruption handling |
| `node/container_runtime_test.go` | Narrow backend contract and hardened create spec |

The Linux adapter is compile-checked because unit-test hosts may not be Linux
and should not mutate a real containerd daemon. Live runtime tests belong on a
disposable node and must use dedicated containerd namespace and paths.
