# ADR 0003: No Kubernetes API compatibility

- Status: Accepted
- Date: 2026-07-22

## Context

A Kubernetes-compatible API would ease migration of manifests and Helm charts,
but it would also import Kubernetes object semantics, watches, controllers,
status ownership, admission behavior, discovery, and an expanding CRD surface.
Maintaining compatibility would pull a4s toward recreating Kubernetes instead
of exploring an outcome-oriented agentic control layer.

## Decision

a4s will not implement the Kubernetes API or make Kubernetes resources its
native contract.

Migration may use a one-way importer that converts a deliberately supported
subset into a4s goals and reports unsupported semantics. Native packaging will
use recipes that compile to a4s goals. The importer is not a permanent runtime
compatibility layer.

## Consequences

Benefits:

- Goal and action semantics can remain small and outcome-oriented.
- The project can remove Kubernetes-specific control-plane components.
- Unsupported behavior remains visible instead of being emulated poorly.

Costs:

- Existing Helm charts and manifests cannot run unchanged.
- Important applications need native recipes or explicit definitions.
- Migration requires workload-by-workload semantic analysis.
- The potential user base is narrower than a drop-in K3s replacement.

## Revisit when

Revisit the importer scope when a concrete migration is blocked. Do not revisit
native API compatibility unless the project's core objective changes.
