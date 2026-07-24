# Architectural decision records

Decision records capture choices that constrain future implementation. They
explain why a decision exists, its cost, and when it should be revisited.

| ADR | Status | Decision |
|---|---|---|
| [0001](0001-agents-propose-kernel-authorizes.md) | Accepted | Agents propose; deterministic kernel authorizes |
| [0002](0002-retain-containerd-and-runc.md) | Accepted | Retain containerd and runc as the container data plane |
| [0003](0003-no-kubernetes-api-compatibility.md) | Accepted | Do not implement Kubernetes API compatibility |
| [0004](0004-transport-after-action-contract.md) | Accepted | Stabilize signed action semantics before transport |
| [0005](0005-single-server-event-log-first.md) | Accepted | Prove single-server event recovery before consensus |

## Status values

- **Proposed:** under active evaluation.
- **Accepted:** guides current work.
- **Superseded:** replaced by a newer ADR, which must be linked.
- **Rejected:** evaluated but deliberately not adopted.

Add an ADR for changes to trust boundaries, protocol ownership, data-plane
selection, compatibility promises, durable-state architecture, or model
authority. Ordinary implementation details do not need one.
