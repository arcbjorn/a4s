# Glossary

## Action

A typed, bounded capability proposed by an agent, authorized by the kernel, and
executed by a trusted adapter. Examples are `pull_image` and
`create_allocation`. An action is not an arbitrary command.

## Agent

A decision-making component that observes a bounded context and returns a
proposal. It may be deterministic, model-backed, or human-operated. It has no
direct mutation credentials.

## Agentic control plane

A control plane where agents are first-class planners across infrastructure
domains, while deterministic policy and typed executors retain authority. It
does not mean the platform only schedules AI-agent workloads.

## Allocation

One placed replica of a workload on a node. In the current runtime, it maps to
a containerd container and potentially a live task.

## Approval

A separately authenticated operator decision granting a specific risk scope to
a goal. It cannot be asserted by an agent or embedded as trusted goal state.

## Capability grant

Kernel-owned permission allowing one authenticated agent ID to propose one
action kind. A grant permits proposal, not direct execution.

## Compensation

A bounded action sequence intended to return the system to a safe state after
partial failure. It is planned but not implemented.

## Control kernel

The small deterministic trusted core that authenticates agent identity, checks
grants and policy, rejects stale state, simulates complete proposals, and
authorizes action issuance.

## Data plane

The mechanisms that perform work: containerd, runc, CNI, nftables, volumes,
gateways, and probes. a4s intends to orchestrate these tools, not replace them.

## Dispatch result

The node's successful response containing the envelope digest and action
evidence. It is stored under the action's idempotency key.

## Evidence

Structured observations produced by an executor or independent probe. Evidence
is used to decide whether an action or goal succeeded. Agent reasoning is not
evidence.

## Goal

A structured desired outcome containing workload intent, optional route,
constraints, and a human-readable objective.

## Idempotency key

A stable identity for one semantic action. Repeating the same key and envelope
returns the prior result; using the key for different content is rejected.

## Lease

Exclusive, time-bounded ownership of a mutation target. Envelopes contain a
lease ID, but lease acquisition and node enforcement are not implemented yet.

## Materialized world

The current state projection built from observations, approvals, and accepted
events. The simulation receives it directly; the future server rebuilds it from
durable history.

## Node

A Linux host running the a4s node executor and data-plane tools. The node
reports facts and executes signed actions but does not make cluster-wide
decisions.

## Observation

An immutable, sourced, time-bounded fact such as node capacity, container state,
probe result, or image presence. The full observation protocol is planned.

## Policy

Deterministic constraints outside agent reasoning. Policy covers capabilities,
capacity, image identity, privilege, exposure, approval, and future storage and
secret rules.

## Proposal

An agent's revision-bound ordered plan containing reasoning, actions,
dependencies, and expected evidence.

## Readiness

Independent evidence that an application can perform its intended service.
Starting a process establishes running state but not readiness.

## Recipe

A planned package that compiles higher-level service intent into native a4s
goals. It is not implemented and is not a Helm compatibility layer.

## Reconciliation

Repeatedly observing the world, proposing bounded changes, authorizing,
executing, and verifying until the goal is achieved or safely blocked.

## Revision

A monotonically changing world version. Proposals are valid only against the
exact revision they observed.

## Stream harness

The current `a4s node` stdin/stdout JSON interface used to exercise signed
dispatch before a real authenticated network transport exists.

## World

The revisioned snapshot of nodes, allocations, routes, and approvals supplied
to control agents and the kernel.
