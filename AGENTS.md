# Project instructions

These instructions apply to the entire a4s repository.

## Communication and commits

- Never use emojis in communication, code, logs, documentation, or commits
  unless explicitly requested.
- Use Conventional Commits.
- Keep commit subjects at 50 characters or fewer.
- Keep one granular goal per commit.
- Never commit secrets, private keys, real credentials, or sensitive host
  inventory.

## Architecture invariants

- Agents propose; they never mutate infrastructure directly.
- The deterministic kernel owns authentication, grants, policy, complete-plan
  simulation, and authorization.
- Executors expose typed capabilities and must not provide a generic shell.
- Verification comes from executor or independent probe evidence, never agent
  reasoning.
- Every proposal is bound to an exact observed world revision.
- Every mutating action has explicit idempotency and crash-recovery semantics.
- The `control` package should remain standard-library-only.
- Do not implement Kubernetes API compatibility as a native interface.
- Do not add model availability as a bootstrap or steady-state dependency.

Read `docs/security.md` and `docs/control-protocol.md` before changing action,
identity, approval, persistence, node execution, storage, network, or secret
behavior.

## Development workflow

- Format Go with `gofmt`.
- Add denial and failure tests alongside happy paths.
- Run `go test -race ./...` for code changes.
- Cross-compile Linux node changes.
- Run `go vet ./...` and `git diff --check` before handoff.
- Update `docs/project-status.md` and affected protocol or operational docs in
  the same change.
- Keep all project paths relative to this repository; do not depend on a parent
  checkout.

The complete workflow and extension checklists are in
`docs/development.md` and `docs/codebase.md`.
