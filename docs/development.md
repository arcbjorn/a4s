# Development guide

## Working principles

- Keep agents, authorization, mutation, and verification as separate roles.
- Prefer a small typed capability over a generic integration surface.
- Keep the deterministic kernel understandable without containerd or model
  knowledge.
- Treat every new action as a security-sensitive protocol change.
- Make progress in vertical slices that can be failure-tested end to end.
- Document current behavior separately from intended architecture.

Repository-local instructions also live in `AGENTS.md` so they travel with a
copied project.

## Toolchain

Use the Go version in `.go-version`. The module currently tracks containerd
v2.3.2 and therefore declares Go 1.26.3.

```bash
go version
go mod download
go mod verify
```

Do not silently lower the Go directive without compiling the Linux containerd
adapter against the selected dependency version.

## Standard change loop

```bash
gofmt -w <changed-go-files>
go test ./...
go test -race ./...
go vet ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go test -c -o /tmp/a4s-node.test ./node
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -o /tmp/a4s ./cmd/a4s
git diff --check
```

## Fuzzing the kernel

`architecture.md` commits to keeping the kernel small enough to reason about and
fuzz thoroughly. Run these when changing anything that parses untrusted input —
scenario validation, model output, approvals, evidence projection, or log
replay:

```bash
go test ./control/ -run XXX -fuzz FuzzApprovalVerification -fuzztime 60s
go test ./control/ -run XXX -fuzz FuzzScenarioValidation -fuzztime 60s
go test ./control/ -run XXX -fuzz FuzzModelDiagnosisDecode -fuzztime 60s
go test ./control/ -run XXX -fuzz FuzzProjection -fuzztime 60s
go test ./eventlog/ -run XXX -fuzz FuzzReplay -fuzztime 300s
```

Each target asserts an invariant rather than only checking for panics: that no
input authorizes without a valid signature, that a decoded diagnosis cannot name
something the world lacks, that validation never admits an unpinned image or a
zero budget ceiling, and that projection never produces negative capacity or a
double-owned volume. A decoder that survived arbitrary input by accepting it
would pass a crash-only fuzz test while being exactly the bug worth finding.

`FuzzReplay` writes a file per iteration and runs far slower than the in-memory
targets; give it a longer window.

Also run the simulation when changing control behavior:

```bash
go run ./cmd/a4s validate --file examples/web-service.json
go run ./cmd/a4s simulate --file examples/web-service.json
```

Use a disposable Linux host for any test that invokes a real containerd socket,
CNI, nftables, volume tool, or gateway.

## Go style

- Run `gofmt`; do not manually align Go syntax.
- Keep packages focused and dependency direction explicit.
- Wrap errors with operation and target context using `%w`.
- Prefer injected interfaces around privileged or nondeterministic boundaries.
- Keep time injectable where expiry or reconciliation behavior is tested.
- Avoid global mutable state.
- Reject malformed or unsupported inputs explicitly.
- Preserve deterministic iteration when proposal order affects events or tests.
- Comment security invariants and non-obvious crash behavior, not ordinary
  syntax.

The `control` package should remain standard-library-only. Runtime and transport
dependencies belong in adapter packages.

## Test expectations

Every behavior change needs both its happy path and the nearest authority or
failure boundary.

Examples:

| Change | Minimum tests |
|---|---|
| Agent proposal | convergence, no-op, blocked capacity/constraint |
| Kernel policy | allowed case and explicit denial |
| Action dependency | valid order and unsatisfied dependency |
| Envelope field | signed acceptance, tamper rejection, boundary time |
| Idempotency | first execution, exact repeat, conflicting reuse, restart |
| Runtime action | fake-backend contract, invalid inputs, backend failure |
| Persistence | reopen, corruption, sequence conflict, permissions |
| Linux adapter | cross-compile plus disposable-host smoke test |

Use `t.TempDir()` for files. Tests must not depend on the developer's
containerd, home directory, parent repository, network, or wall clock unless
they are explicitly isolated integration tests.

## Dependency changes

For any direct dependency change:

1. Read the upstream release and security notes.
2. Update the smallest direct requirement.
3. Run `go mod tidy` and inspect both `go.mod` and `go.sum`.
4. Run the native race suite and Linux cross-build.
5. Compare Linux binary size and new transitive dependency categories.
6. Update project status and getting-started requirements.
7. Run a live disposable-node test when containerd behavior changes.

Do not use `go get -u ./...` as an unreviewed bulk upgrade.

## Documentation expectations

Documentation is part of the protocol. Update it in the same change when
behavior changes. Keep these labels precise:

- “Implemented” means executable code exists and unit/contract tests pass.
- “Cross-compiled” means the Linux code type-checks and links but has not run.
- “Simulated” means only the memory executor supplies the behavior.
- “Planned” means architecture only.
- “Production-ready” must not be used until the security blockers are closed
  and recovery is tested.

Use relative links inside the project so the folder remains portable.

## Commit and review conventions

- Use Conventional Commits: `feat:`, `fix:`, `docs:`, `refactor:`, `test:`,
  `build:`, or `chore:`.
- Keep the subject at 50 characters or fewer.
- One granular goal per commit.
- Never commit raw secrets, private keys, real credentials, or sensitive host
  inventory.
- Do not use emojis in communication, code, logs, documentation, or commits.

Security-sensitive changes should call out:

- Trust boundary affected.
- New authority granted.
- Failure and replay semantics.
- Evidence used to establish success.
- Tests that prove denial as well as success.

## Branch and release posture

The current version is `0.2.0-dev` and the API is v1alpha1. There is no stable
release process yet.

Before the first tagged release, add:

- A chosen open-source or private-use license.
- Automated CI for race tests, vet, Linux builds, and documentation links.
- Version injection at build time instead of a source constant.
- Release checksums and signatures.
- Supported Go, Linux, containerd, runc, and architecture matrix.
- Upgrade and rollback notes.

## Keeping the project portable

Do not introduce imports, scripts, fixtures, or docs that require files above
the project root. If a useful integration belongs to another infrastructure
repository, keep it there or represent it here as a generic example.

To test portability locally, copy the folder to a temporary directory without
its `.git` parent and run the commands in the verification section. The final
verification for this handoff follows that procedure.
