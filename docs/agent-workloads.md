# Agent workloads

This document defines the second meaning of "agent" in a4s and the rules that
bound it. Read [the architecture](architecture.md) first: it describes control
agents, which are a different thing entirely.

## Two kinds of agent, one word

a4s uses "agent" for two objects with different hierarchies, purposes, and
authority. They never share an authority path.

| | Control agent | Agent workload |
|---|---|---|
| What it is | A registered decision-maker in the control plane | Scheduled cargo, like any other workload |
| What it holds | `ActionKind` capability grants | `ToolGrant` tool envelope |
| What it does | Proposes typed infrastructure plans | Performs work outside a4s |
| Who executes it | Never executes; the kernel does | Runs in a container on a node |
| Where it is declared | `AgentDescriptor`, policy grants | `WorkloadSpec.Runtime` |
| Can it mutate infrastructure | No, and it cannot be granted the ability | No, and it has no vocabulary for it |

The separation is deliberate and structural. A control agent's grants are
`ActionKind` values the kernel knows how to execute. An agent workload's grants
are `ToolGrant` values that mean nothing to the kernel. Sharing one vocabulary
would make it possible to grant a workload an infrastructure mutation, which is
the authority path that must not exist.

An agent workload never proposes anything. It is not registered, not consulted
during an evaluation session, and has no seat in the coordinator.

## Why a workload kind and not a new primitive

The `Engine` field set the precedent. A database is meaningfully unlike a
generic container: its files are inconsistent when copied hot, it is ready only
when it accepts connections, and it needs domain actions rather than generic
ones. That did not become a new top-level object. It became one declarative
field, and the kernel keys its behavior off it.

An agent is unlike a generic container along analogous axes:

- Its cost is tokens, not cpu-seconds.
- It is ready only when it can reach a provider with budget remaining.
- It has domain actions: grant an envelope, drain a task.
- Its blast radius is not node-local, because a granted tool acts on the world.

So `Runtime` mirrors `Engine`. It is structured rather than a bare string
because an engine's behavior is implied by its name, while an agent's budget
ceiling, tool grants, and provider are per-workload policy inputs the kernel has
to enforce.

A workload may declare `Engine` or `Runtime`, never both. One container cannot
be both a database and an agent without leaving the kernel holding two
contradictory sets of rules for backing it up and probing it.

## Kubernetes capability map

The light-version claim is about cutting capabilities, not just packaging. What
carries over, and in what shape:

| Kubernetes | a4s agent workload |
|---|---|
| Pod | Allocation running an agent runtime container |
| Container image | Pinned image digest plus pinned `runtime.model` |
| Resource limits | `Resources` for cpu/memory, `Budget` for tokens/cost/wall-clock/tool-calls |
| Liveness/readiness probe | `ProbeAgent`: provider reachable and budget remaining |
| Deployment replicas | `Replicas`, but retirement drains first |
| HorizontalPodAutoscaler | `Queue.Depth` scaling, capped by `Queue.MaxWorkers` |
| Service/DNS | Optional; an agent may declare no port at all |
| PVC | Existing volume machinery, unchanged |
| Secret | Existing broker and tmpfs mounts, unchanged |
| NetworkPolicy | `ToolGrant` scopes; the envelope is the real boundary |
| Node | Host with capacity *and* provider egress |
| Scheduler | Kernel feasibility on capacity, provider, budget; agent ranks |

Deliberately not carried over: interchangeable replicas. An agent instance
accumulates task context, so it is closer to a StatefulSet member than a
Deployment replica. That is what drain exists for.

## The Runtime block

```json
"runtime": {
  "name": "a4s.agent/v1",
  "provider": "anthropic",
  "model": "claude-opus-5",
  "queue": "intake",
  "budget": {
    "tokens": 200000,
    "cost_millis": 4000,
    "wall_seconds": 900,
    "tool_calls": 60
  },
  "tools": [
    {"name": "repo.read", "scope": "github.com/arcbjorn/a4s"}
  ]
}
```

Every budget ceiling must be present and positive. A zero ceiling is rejected
rather than treated as unlimited: unlimited is a decision an operator should
have to write down, and the common case of a forgotten field must not be the
case that grants infinite spend.

The model is pinned for the same reason the image digest is. A provider that
repoints an alias would otherwise change the workload's behavior with no goal
change and no audit trail.

Every tool must declare a scope. An unscoped tool is granted whatever its
credential allows, which makes the declared envelope a description of nothing.

## Budget as a resource dimension

`Budget` is separate from `Resources` because a cpu limit bounds how fast an
agent burns money, not how much. An agent blocked on a provider call is idle by
cgroup accounting and expensive in every way that matters.

Nodes carry `BudgetCapacity` and `BudgetUsed`. Placement commits budget the same
way it commits memory, so one node's agents cannot consume a cluster's spend
before any other node schedules one.

`Spent` comes only from `agent.spent` evidence produced by the node runtime.
Spend never decreases: a lower reading is treated as stale and ignored, because
accepting it would let an exhausted agent look affordable again and be restarted
into the same ceiling.

An exhausted agent is not a failed agent. It stopped because it hit a declared
ceiling. It stops being ready, stops counting toward goal satisfaction, and is
treated as drifted so it can be retired and replaced.

## Tool grants and the plan-checking invariant

Invariant 4 says the whole plan is checked before its first action. An agent
workload's internal plan is discovered as it runs, which appears to conflict.

It does not. The kernel checks the agent's **grant envelope** up front, not its
reasoning. `grant_tools` installs the envelope while the allocation is still in
`created` phase, and the kernel refuses to grant tools to a running allocation.
The agent then operates inside a pre-approved box it cannot widen at runtime.

Every granted tool must be one the goal declared, compared exactly including
scope and the mutating flag. A grant matched by name alone would let a read-only
declaration be installed as a mutating capability.

Any envelope containing a mutating tool requires a separately authenticated
`agent-mutating-tools` approval. A mutating tool changes state outside a4s,
where no compensation and no event log reaches.

On the node, envelopes are stored strictly per allocation. A runtime asks the
node what it may call rather than deciding for itself, and there is no path from
one allocation's query to another's grants. Deleting an allocation releases its
envelope, so a later allocation reusing the identifier inherits nothing.

## Context isolation

The requirement is that hundreds of agents never leak context into each other,
with the robustness of an OCI container. These operate at different layers and
do not conflict.

The container provides lifecycle, resource limits, digest-pinned images, the
no-new-privileges and empty-capabilities baseline, and a per-allocation network
namespace. That is unchanged from any other workload.

Context leakage is not something a container boundary prevents. It happens
through shared state in a runtime: a provider client with a cached conversation,
a shared scratch directory, one credential serving every instance. So isolation
is enforced above the container:

- Each instance gets its own credential mount from the existing secret broker.
  No shared provider client, no shared key.
- Each instance gets its own workspace directory under the agent capability's
  root. Nothing is shared between them.
- Tool grants are per instance. One agent cannot call a tool that reads
  another's workspace, because the grant does not exist.

This is also why the node does not run the model loop itself. A native runtime
managing hundreds of agents in one process would hold every context in one
address space, which is precisely the leak the design has to prevent. The
container path is both more robust and better isolated.

## Drain before stop

An agent instance holds task context that a stateless replica does not. Stopping
it mid-task destroys work that has already been paid for in tokens, rather than
merely shifting load to a sibling.

Retirement therefore borrows the volume-handoff shape: an evidenced step before
the destructive one.

```text
running -> drain_allocation -> (agent.draining | allocation.drained) -> stop -> delete
```

The kernel refuses to stop an agent that holds a task unless it has been drained
and observed holding nothing. The node reports `agent.draining` while a task is
still held and `allocation.drained` only once the task slot is empty, so a drain
that never completes stalls the rollout instead of silently discarding work.

An exhausted agent is exempt. It cannot make progress on its task, so waiting
for it to finish would wait forever.

A draining instance stops counting toward goal satisfaction immediately. It is
on its way out and must not make a goal look satisfied.

## Work queues

A queue exists so agent replicas can be sized by observed demand rather than a
fixed count. It is an explicit object for the same reason a volume is: the thing
that determines how many workers are needed must be nameable independently of
the workers.

`Queue.Depth` comes from `queue.observed` evidence, never from an agent's report
of its own backlog. Depth older than 60 seconds does not drive scaling: queue
depth is the most perishable fact in the world view, because the workers are
consuming it as it is read.

`MaxWorkers` is mandatory and at least one. Demand-driven scaling without a
ceiling is how a queue spike becomes a spend incident. The goal's replica count
is the floor; `MaxWorkers` is the ceiling. The kernel recomputes that bound
itself in `authorizedReplicas` rather than trusting the placement agent's
arithmetic.

A queue serves exactly one workload, so a scaling decision has an unambiguous
subject and one workload's demand cannot scale another's replicas.

## Actions, checks, and evidence

New actions:

| Action | Meaning |
|---|---|
| `grant_tools` | Install an agent allocation's tool envelope before it starts |
| `drain_allocation` | Tell an instance to stop accepting work and finish its task |

New checks:

| Check | Satisfied when |
|---|---|
| `agent_ready` | Ready, not draining, not exhausted |
| `allocation_drained` | Draining and holding no task |

New evidence:

| Evidence | Source |
|---|---|
| `agent.tools_granted` | Node, reporting envelope size only |
| `agent.ready` | Agent probe: provider reachable, budget remaining |
| `agent.spent` | Node runtime; monotonic |
| `agent.draining` | Node; instance still holds a task |
| `allocation.drained` | Node; task slot empty |
| `queue.observed` | Queue measurement, with observation time |

Tool-grant evidence reports how many capabilities were installed, not which. The
authoritative envelope is the one the kernel authorized in the action; echoing
names back would invite trusting the node's copy over the kernel's.

## Policy grants

`grant_tools` belongs to the placement agent: installing an envelope is part of
preparing an allocation to run, alongside mounting its secrets.

`drain_allocation` belongs to the rollout agent: draining is how a rollout
retires an instance without destroying the task it holds.

Neither is granted to the storage agent, and no control agent may hold both
creation and destruction authority for agent workloads.

## Try it

```bash
go run ./cmd/a4s validate --file examples/agent-workload.json
go run ./cmd/a4s simulate --file examples/agent-workload.json
```

Expected reconciliation, per replica: `pull_image`, `create_allocation` with
budget reserved, `grant_tools`, `start_allocation`, then independent
`agent.ready` evidence before the verifier accepts the goal.

## What is not built yet

- A real agent runtime implementing `a4s.agent/v1`. The contract is defined and
  bounded; no image implements it.
- Node-side spend metering. `agent.spent` is projected correctly, but nothing
  measures real token consumption yet.
- Queue storage and delivery. `Queue` is an observed object; there is no broker
  behind it.
- Per-instance credential derivation. Agents use the existing secret broker,
  which is not yet per-allocation-scoped for provider keys.
- Agent-to-agent messaging. Deliberately absent: it would need its own authority
  model, and nothing requires it yet.
