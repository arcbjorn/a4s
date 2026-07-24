# ADR 0001: Agents propose; kernel authorizes

- Status: Accepted
- Date: 2026-07-22

## Context

The project exists to make agents first-class infrastructure planners. Giving
an agent direct host, containerd, network, storage, or secret authority would
make model errors, prompt injection, agent bugs, and stale context equivalent
to privileged operator commands.

Conventional fixed controllers avoid some model uncertainty but fragment one
outcome across many isolated resource reconcilers. a4s needs cross-domain
reasoning without turning reasoning into authority.

## Decision

Agents only produce revision-bound typed proposals. A small deterministic
kernel owned outside the agent:

- Authenticates agent identity.
- Applies kernel-owned capability grants.
- Validates structured fields against the goal and current world.
- Simulates the complete proposal before mutation.
- Requires explicit approvals and evidence.
- Issues only typed actions to trusted executors.

Deterministic and model-backed agents use the same proposal interface and have
the same lack of direct credentials.

## Consequences

Benefits:

- Model or agent quality can evolve without redefining host authority.
- Denials and approvals are deterministic and testable.
- Bootstrap and routine repair can work without a model provider.
- Agent reasoning remains useful for diagnosis and planning.

Costs:

- Every new action requires explicit protocol, policy, executor, and evidence
  work.
- Some useful operations remain impossible until a narrow capability exists.
- The kernel becomes a high-assurance component requiring extensive tests and
  review.

## Revisit when

Do not revisit the separation itself. Revisit interface shape when multiple
real agents cannot express safe plans without excessive action coupling. The
answer should remain a better typed contract, not direct mutation access.
