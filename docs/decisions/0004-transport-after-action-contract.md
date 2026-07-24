# ADR 0004: Stabilize actions before transport

- Status: Accepted
- Date: 2026-07-22

## Context

Selecting HTTP, gRPC, QUIC, NATS, or another transport too early would create
authentication, retry, streaming, and deployment work before the project knew
which action, evidence, idempotency, lease, and crash semantics had to cross the
boundary.

The node trust contract can be exercised without committing to a network
protocol.

## Decision

Implement signed action envelopes, node verification, durable idempotency, and
runtime dispatch first. Use a JSON stdin/stdout stream harness for the initial
Linux experiment. Choose the real transport only after restart and evidence
behavior is measured.

Transport must not change action authority. It will carry the same or a
versioned successor to the signed envelope and result semantics.

## Consequences

Benefits:

- Security and crash semantics can be tested in isolation.
- The transport decision will be informed by actual message patterns.
- The current node code remains easy to invoke in tests and harnesses.

Costs:

- There is no remote or long-running node daemon yet.
- The harness fails the whole process on one message error.
- Key enrollment and mutual node identity remain unresolved.

## Revisit when

Choose a transport after the disposable Linux smoke test proves pull, create,
start, replay, restart, and independent readiness evidence. Record that choice
in a new ADR including authentication, flow control, retry, and upgrade
semantics.
