# ADR 0002: Retain containerd and runc

- Status: Accepted
- Date: 2026-07-22

## Context

Replacing K3s does not require replacing the OCI image, snapshot, container,
and process runtime stack. Reimplementing these mechanisms would add a large
security and compatibility burden unrelated to agentic control.

Shelling out to Docker or `ctr` would expose an unstable and overly broad
orchestration interface, complicate structured errors, and weaken testability.

## Decision

Use containerd's native Go client for OCI content, snapshots, containers, and
tasks, with runc as the initial runtime. Keep containerd behind a narrow a4s
`ContainerBackend` contract.

The control kernel remains independent of containerd types. Node actions expose
only a4s-approved fields and apply an OCI hardening baseline in trusted code.

## Consequences

Benefits:

- Reuses a mature runtime and OCI ecosystem.
- Provides structured APIs and namespace isolation for a4s resources.
- Keeps the project focused on control semantics.
- Allows a fake backend for authority-contract tests.

Costs:

- containerd adds a significant Go dependency graph and Linux binary size.
- a4s inherits containerd and runc security and upgrade responsibilities.
- Live integration tests require a Linux host.
- Runtime-specific lifecycle and recovery behavior must still be reconciled.

## Revisit when

Add another backend only when a real target cannot run containerd or a measured
operational benefit justifies it. Preserve the narrow action contract and do not
let backend-specific configuration leak into agents.
