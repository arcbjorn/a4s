# Operations and disposable-node runbook

This runbook covers the current vertical slice. It is not a production install
guide. The only authorized target is a disposable Linux host or VM whose
containerd namespace and test workloads can be safely inspected and discarded.

The round trip described here has been verified against live containerd, but the
deployment surface around it has not been hardened. See
[project status](project-status.md) for what remains open.

## Current process model

One binary provides both daemons and the operator CLI:

- `a4s server` opens the durable event log, rebuilds the world projection from
  it, accepts enrolled node connections on `--listen`, and serves the
  authenticated operator API on `--api`.
- `a4s node` opens containerd and its durable state, connects to
  `--server`, and executes signed capabilities. With `--server` empty it reads
  signed envelopes from standard input instead, which is a harness for isolating
  the runtime adapter rather than the normal path.
- `simulate` runs a finite reconciliation in one process using memory state.

The node supervises its own desired state on `--supervise-interval`, so
workloads survive a control-plane outage. Service units for both daemons are in
`init/systemd/`; both handle SIGTERM and unwind rather than dying mid-action.

## Host prerequisites

Confirm before starting:

- Linux architecture matches the built binary.
- containerd 2.x and runc are installed and healthy.
- A snapshotter is configured and can unpack the target image.
- The test image is pinned by a real registry digest.
- The operator can access the containerd socket.
- The node ID is unique for the experiment.
- System time is synchronized closely enough for short-lived envelopes.
- No production workload uses the `a4s` containerd namespace.

CNI plugins, `nft`, and Caddy are needed only for the features that use them;
see the [support matrix](support-matrix.md).

Recommended dedicated paths:

```text
/etc/a4s/                     trusted keys, keyset, and configuration
/var/lib/a4s/                 event log, ledgers, volumes, and sealed secrets
/var/log/a4s/allocations/     allocation stdout and stderr
/usr/local/bin/a4s            binary
```

Directories should be owned by the dedicated service identity when that
identity exists. The node needs privileged containerd socket access, so isolate
the experiment at the host level.

## Build artifact

On a trusted build host at the project root:

```bash
go test -race ./...
./scripts/build-release.sh <version>
```

The release script produces stamped binaries for linux and darwin on amd64 and
arm64 with a `SHA256SUMS` file, and refuses to build from a modified working
tree. Signing the checksums is a manual step: the script prints the `gpg`
command rather than running it, because the signing key belongs to a person.

For an ad hoc build, cross-compile directly:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -o build/a4s-linux-amd64 ./cmd/a4s
```

## Key setup

Three separate identities are involved. Do not reuse one for another.

```bash
# Controller signing keyset. The private key stays on the server.
a4s keys init --keyset /etc/a4s/keyset.json --key-id control-1 --out /etc/a4s/control-1

# Node identity. The node proves possession of this during enrollment.
a4s keygen --out /etc/a4s/node-edge-1

# Operator key. Authority for API requests originates here.
a4s keygen --out ~/.a4s/ops
```

Distribute the keyset (public parts) to every node, `node-edge-1.pub` to the
server's `--node-keys` directory, and `ops.pub` to its `--operator-keys`
directory. A node absent from `--node-keys` cannot enroll.

Rotate the controller key with `a4s keys rotate` and retire the old one only
after every node holds the new keyset; see [upgrading](upgrading.md).

## Start the server

```bash
/usr/local/bin/a4s server \
  --event-log /var/lib/a4s/events.log \
  --signing-key /etc/a4s/control-1 \
  --key-id control-1 \
  --listen 0.0.0.0:8443 \
  --node-keys /etc/a4s/nodes \
  --api 127.0.0.1:8080 \
  --operator-keys /etc/a4s/operators \
  --require-encryption \
  --log-format json
```

Recovery is the normal startup path: the world projection is rebuilt from the
event log every time. Check what a build recovered without starting the
listeners:

```bash
a4s server --event-log /var/lib/a4s/events.log --status
```

`--require-encryption` refuses a node that will not negotiate an encrypted
channel. Leave it on. The operator API has no TLS of its own, so bind it to
localhost or a trusted interface and reach it over an authenticated tunnel;
request authenticity does not depend on the transport, but confidentiality does.

## Start the node

```bash
/usr/local/bin/a4s node \
  --node-id edge-1 \
  --server control:8443 \
  --identity-key /etc/a4s/node-edge-1 \
  --keyset /etc/a4s/keyset.json \
  --containerd /run/containerd/containerd.sock \
  --namespace a4s \
  --ledger /var/lib/a4s/node-ledger.jsonl \
  --desired-state /var/lib/a4s/desired-state.jsonl \
  --log-dir /var/log/a4s/allocations \
  --cni-bin /opt/cni/bin \
  --subnet 10.42.0.0/24 \
  --log-format json
```

The node performs a five-second containerd health check before accepting work.
It refuses to start on invalid configuration, an unreadable key, unavailable
containerd, or a corrupt ledger replay.

Once running, a rejected or failed action produces a per-message result rather
than terminating the node. In the stdin harness it still exits on an undecodable
frame, because a malformed frame desynchronizes the stream.

## Submit a goal

Goals reach a running control plane through the operator API, not a scenario
file:

```bash
a4s submit --file examples/web-service.json \
  --server http://127.0.0.1:8080 --key-id ops --operator-key ~/.a4s/ops

a4s status --server http://127.0.0.1:8080 --key-id ops --operator-key ~/.a4s/ops
a4s events --server http://127.0.0.1:8080 --key-id ops --operator-key ~/.a4s/ops --limit 20
```

Replace the example's all-zero digest with a real one first; it satisfies format
validation but identifies no registry object.

Gated decisions need an explicit approval. List the scopes with
`a4s approve --scopes` and issue one against the event log directly:

```bash
a4s approve --event-log /var/lib/a4s/events.log --goal homepage-public \
  --scope public-route --operator you --key ~/.a4s/ops.key --key-id ops \
  --lifetime 1h --reason "smoke test"
```

## Smoke-test sequence

Expected evidence for one stateless replica reaching a published route:

```text
image.present
allocation.created
network.attached
allocation.running
allocation.ready
zone.published
route.reachable
```

`allocation.running` means the OCI task started; `allocation.ready` is a
separate measurement from a process, TCP, or HTTP probe and carries an expiry.
Do not treat running as ready.

Trace how any of it happened, or why it did not:

```bash
a4s explain --event-log /var/lib/a4s/events.log --target homepage-0
a4s diagnose --event-log /var/lib/a4s/events.log --goal homepage-public
a4s plan --file examples/web-service.json --event-log /var/lib/a4s/events.log
```

`plan` reuses the kernel's own simulation, so a dry run cannot drift from what
execution would do.

## Read-only inspection

If `ctr` is installed and configured for the same containerd socket, inspect
only the dedicated namespace:

```bash
ctr --namespace a4s containers list
ctr --namespace a4s tasks list
```

Confirm:

- Exactly one container exists for the allocation ID.
- It has `a4s.io/managed=true`, the expected workload, and exact image labels.
- The task PID matches returned evidence.
- Allocation output appears only under the configured log directory.
- The ledger is mode `0600` and contains no secret value.

Do not use broad cleanup commands from this document. Resolve exact test
container, task, and snapshot identities and use the disposable host's normal
recovery or teardown process. Orphans are discovered and reported, but their
removal is an authorized action rather than a node decision.

## Restart and replay test

1. Preserve the ledger and desired-state file.
2. Restart only `a4s node`.
3. Confirm the server's redispatched actions do not create a second container
   or task, and that orphan discovery reports anything left behind.
4. Submit a different envelope reusing a committed idempotency key and confirm
   rejection.
5. Restart the server and confirm the projection rebuilds from the event log at
   the same revision without redoing work.

Deduplication keys on a digest of the authorized work rather than the whole
envelope, so a legitimate retry returns the stored result while a key reused for
different work is refused. Expired envelopes are rejected before ledger lookup,
so an exact replay after expiry fails rather than returning historical evidence;
the server reissues instead.

Kill a container out from under the node with the server stopped, and confirm
the supervisor restarts it within the crash-loop budget.

## Data requiring backup

| Path | Purpose | Recovery behavior |
|---|---|---|
| Event log (SQLite) | Intent, decisions, evidence, audit chain | Verified on open; world projection rebuilt from it |
| Node ledger JSONL | Successful idempotency results | Replay restores deduplication |
| Desired-state JSONL | Server-authorized intent the node supervises | Replay restores local supervision |
| Volume state JSONL | Single-writer volume ownership and generation | Replay prevents a second writer |
| Volume root | Durable workload data | Snapshot, off-host backup, verified restore |
| Allocation logs | Workload diagnostics | Not control authority |

The event log is the only authoritative controller state; everything else is
derived from it. Back it up with `a4s backup`, which uses `VACUUM INTO` and is
transactionally consistent without stopping writers, and records the chain head
outside the file so truncation is detectable:

```bash
a4s backup --event-log /var/lib/a4s/events.log --out /backups/events-$(date -u +%Y%m%dT%H%M%SZ).log
a4s backup --verify /backups/events-<stamp>.log
```

The verify step is what distinguishes a recovery point from a file that happens
to exist. Do not copy the database by hand while a writer is running.

containerd metadata and snapshots are external state. The control plane
reconciles them against durable goals and observations; they are not a
replacement for the event log.

## Failure response

### Event log corruption

Stop mutation. Preserve the corrupt database and an independent copy. Do not
edit rows through `sqlite3` to make startup pass: the hash chain exists to catch
exactly that, and an edit that satisfies SQLite still breaks the chain. Determine
whether the cause is storage failure, accidental edit, or compromise. Restore a
verified backup with `a4s restore`, which verifies the archive before touching
the destination and preserves the log it supersedes, then reconcile missing
dispatched actions against node ledgers before reissuing anything.

A store written by a newer schema is refused rather than interpreted. That is a
build mismatch, not corruption; see [upgrading](upgrading.md).

### Node ledger corruption

Stop the node. Preserve the file. Without the ledger, retries may reach the
backend again; the implemented container actions are individually idempotent,
but this is not a general recovery guarantee. Inspect exact containerd state
before restoring or reconstructing ledger data.

### Dispatched event without completion

Query the node's stored result by replaying the same authorized work; an exact
repeat returns the recorded evidence. If the envelope has expired, observe
containerd state and let the server reissue rather than fabricating an envelope.
Never assume failure solely from a missing completion event.

### Controller unavailable

Existing tasks continue and the node keeps its desired state true, bounded by
the crash-loop budget and backoff. It will not accept expired or new
unauthorized work. Restart the server; recovery is the normal startup path.

### Node unavailable

A stateless workload is replaced elsewhere. A stateful workload is not: it is
pinned to the node holding its data and is never relocated on a missing
heartbeat alone, because a duplicate writer is worse than an outage. Recover it
through volume backup and an explicit ownership handoff.

## Productionization sequence

Remaining before running production traffic:

1. Turn on the confinement that is off by default: `--run-as`, `--apparmor`,
   `--read-only-root`, and `--user-namespace`. Each can break an image not
   written for it, so adopt them per workload rather than fleet-wide.
2. Give the gateway a way to verify snapshot provenance independently.
3. Rotate secrets without replacing the allocation.
4. Run sustained-failure and penetration testing beyond the verified round trip.

See the [security model](security.md) for what each of these leaves open.

## Service units

`init/systemd/a4s-server.service` runs the control plane as an unprivileged
`a4s` user with `ProtectSystem=strict`, an empty capability set, and a
`@system-service` syscall filter. It needs only its state directory and its keys.

`init/systemd/a4s-node.service` cannot be unprivileged: it drives containerd,
creates network namespaces, and installs nftables rules. Its
`CapabilityBoundingSet` names exactly the capabilities that work needs and drops
the rest, which is the meaningful confinement available to a process that must be
able to do those things.

Both stop with SIGTERM and are given 30 seconds to unwind. Stopping either is not
an outage: containers keep running, because the node holds no workload process as
a child.
