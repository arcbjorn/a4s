# Changelog

Notable changes per release. The project has not been released yet: there is no
stable version and no compatibility guarantee, so everything below is under
`Unreleased` and may still change shape.

## Unreleased

### Control kernel

- v1alpha1 vocabulary: goal, world, agent, proposal, action, policy, evidence,
  and event.
- Deterministic placement, network, rollout, and storage agents.
- Per-agent capability grants, revision-bound proposals, and whole-proposal
  simulation before mutation.
- Policy: node health, placement labels, resource capacity, digest-pinned
  images, privileged-workload rejection, and stateful single-writer limits.
- Evidence-only world advancement through a pure, idempotent projection, with
  readiness measured by probes and expiring rather than asserted.
- Target leases with expiry, so two proposals cannot interleave on one target.
- Rolling replacement with an availability floor, and operator-approved rollback
  bound to both the failed and known-good versions.
- Cluster-wide service names, typed network policy compiled to nftables, and
  route snapshots resolved to observed endpoints.
- Volumes with generation-fenced single-writer ownership, checksummed snapshots,
  off-host backup, scheduled restore verification, and evidence-gated cross-node
  handoff.
- Opaque secret references with node-sealed material and version-only evidence.
- Agent workloads as a workload kind: pinned model and runtime, mandatory
  positive budget ceilings, scoped tool envelopes granted before start,
  provider reachability and budget capacity as placement constraints, monotonic
  spend evidence, drain-before-stop retirement, and queue-depth scaling under a
  kernel-recomputed worker ceiling.
- Model-backed diagnosis in `reason`, outside the stdlib-only kernel, with
  deterministic fallback so model availability is never a dependency.

### Server and node

- `a4s server`: durable event log, world projection rebuilt on every start,
  shared lease manager, and goal admission.
- Authenticated operator HTTP API. Every request carries an Ed25519-signed
  envelope bound to method, path, and body digest, made single-use by a nonce
  ledger and bounded by a five-minute lifetime. Reads are authenticated; only
  liveness is public.
- Node enrollment proving possession of an identity key, with ephemeral X25519
  shares inside the signed handshake and ChaCha20-Poly1305 per-direction record
  encryption. `--require-encryption` refuses a peer that will not negotiate.
- Signed, node-bound action envelopes with short expiry, and a durable
  idempotency ledger keyed on a digest of the authorized work.
- Durable desired-state cache and a supervisor that restarts crashed workloads
  during a control-plane outage, bounded by a crash-loop budget.
- Node-side budget enforcement, a tool-call gate refusing ungranted
  capabilities, and the `a4s.agent/v1` workload runtime API on a Unix socket.
- Linux containerd v2 adapter: digest-verified pull, idempotent create and
  start, CPU/memory/PID limits, `noNewPrivileges`, empty capabilities, and
  namespaced cgroups.
- Allocation networking through CNI, a zone-only node-local DNS resolver, and
  Caddy gateway routes.

### Persistence

- SQLite in WAL mode with `synchronous=FULL`, so every acknowledged event is
  fsynced before the append returns. A hash chain is retained on top: SQLite
  establishes the rows are the ones committed, the chain that they are the ones
  a4s wrote.
- Chain invariants enforced by the schema, with the record and chain head
  advancing in one transaction under a conditional head update, so two writers
  cannot fork history.
- Versioned schema with transactional migrations; a store written by a newer
  build is refused rather than misread.
- Verified backup and restore. Backups use `VACUUM INTO`, are verified
  read-only, and anchor the chain head outside the store to detect truncation.
- Pure-Go driver, so `CGO_ENABLED=0` cross-builds stay static.

### Operator surface

- `validate`, `simulate`, `node`, `server`, `keygen`, `keys`, `seal`, `plan`,
  `explain`, `diagnose`, `approve`, `history`, `backup`, `restore`, `submit`,
  `status`, `events`, and `version`.
- Signed operator approvals for the gated decisions, with mandatory expiry
  checked against observation time and appended to durable history before the
  projection updates. Revocation is authenticated the same way granting is.
- Causal explanation of any allocation or route, dry-run planning that reuses
  the kernel's own simulation, and deterministic diagnosis of a stuck goal.
- Controller keyset with active, accepted, and retired states, so a fleet
  rotates without a coordinated restart; retiring the active key is refused.
- Structured logging and in-process metrics with a Prometheus endpoint.

### Verification

- Race-tested unit and contract tests, including a denial matrix driven over the
  real protocol: unknown key, tampered envelope, wrong node, expired capability,
  idempotency reuse, replay after restart, stale readiness, and unapproved
  public route.
- Fuzz targets over the untrusted-input surface: scenario validation, model
  output decoding, approval verification, evidence projection, and event-store
  opening. Each asserts an invariant rather than only checking for panics.
- Event durability verified by SIGKILLing a writer mid-append across repeated
  trials; no acknowledged event was lost and the chain verified on recovery.
- The nftables compiler's own output applied to a real Linux kernel.
- The full round trip verified against live containerd on linux/amd64 and
  linux/arm64, covering allocation networking, the gateway, and durable volumes.
- CI: race tests, vet, gofmt, `go mod tidy`, Linux and arm64 cross-builds, the
  example simulation, fuzz smoke targets, and documentation link checking.
- Apache-2.0 license, build-time version stamping, and a release script
  producing checksummed binaries for four platforms.

### Not implemented

- Multi-server consensus or high availability; one server owns the event log.
- Canary rollout and gradual traffic shifting.
- Per-node evidence signing and external anchoring of the audit chain.
- Git source adapter and Kubernetes manifest importer.
- Secret rotation without a workload restart, and a Vault-backed broker.
- Seccomp/AppArmor selection, user namespaces, and rootless containers.
- Direct node-to-node transfer streaming; volumes move through a shared store.
- IPv6 allocation addressing.
