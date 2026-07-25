# Documentation index

This directory is the project handbook for a4s.

## Suggested reading order

1. [Project README](../README.md) for the thesis and a five-minute tour.
2. [Project status](project-status.md) for what exists, what is simulated, and
   the exact next milestone.
3. [Getting started](getting-started.md) to build, test, and run the current
   implementation.
4. [Architecture](architecture.md) for the intended K3s replacement design.
5. [Control protocol](control-protocol.md) for objects, state transitions,
   actions, events, signatures, and versioning.
6. [Agent workloads](agent-workloads.md) for the workload kind whose cost is
   tokens, and how it differs from a control agent.
7. [Node runtime](node-runtime.md) for the Linux data-plane boundary.
8. [Codebase guide](codebase.md) to find implementation ownership and extension
   points.
9. [Security model](security.md) before changing policy, identity, execution,
   storage, networking, or secrets.
10. [Development guide](development.md) before adding code.
11. [Operations](operations.md) before touching a Linux host or containerd.
12. [Roadmap](roadmap.md) for milestone order and exit criteria.
13. [Support matrix](support-matrix.md) for supported versions and platforms.
14. [Upgrading and rolling back](upgrading.md) before moving a deployment to a
    new build.

The [glossary](glossary.md) defines project-specific terms. The
[decision records](decisions/README.md) preserve architectural choices and the
conditions under which they should be revisited.

## Document authority

When documents disagree, use this order:

1. Tests and executable code describe current behavior.
2. [Project status](project-status.md) describes current completeness.
3. [Control protocol](control-protocol.md) describes the current v1alpha1
   contract.
4. [Architecture](architecture.md) describes the intended end state, including
   components that do not exist yet.
5. [Roadmap](roadmap.md) describes proposed sequencing, not a promise.

This distinction matters because a4s is an early spike. Some architecture
sections intentionally describe components that do not exist yet, such as
multi-server consensus, canary rollout, and a Git source adapter.

## Maintainer checklist

Update these documents with code changes:

| Change | Documents to review |
|---|---|
| Goal, proposal, action, evidence, or event field | `control-protocol.md`, examples, status |
| New action kind | protocol, security, codebase, tests, roadmap |
| New control agent | architecture, protocol capability table, codebase |
| Agent-workload runtime, budget, tool, or queue behavior | `agent-workloads.md`, protocol, security |
| Node-runtime behavior | `node-runtime.md`, operations, security |
| New daemon or CLI flag | README, getting started, operations |
| Trust-boundary change | security and a decision record |
| Milestone completion | project status, roadmap, README |
| Go or containerd upgrade | `.go-version`, `go.mod`, getting started, status, `support-matrix.md` |
| Release mechanics or supported platforms | `support-matrix.md`, `upgrading.md`, development |

## License

The project is licensed under Apache-2.0; see [LICENSE](../LICENSE).
