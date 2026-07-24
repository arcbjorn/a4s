# Node runtime slice

The first Linux data-plane slice is implemented. It is intentionally narrower
than a kubelet: the node accepts signed typed actions and translates only
`pull_image`, `create_allocation`, and `start_allocation` into containerd calls.
It has no generic command or shell endpoint.

## Trust boundary

Every input is an Ed25519-signed `SignedAction`. The dispatcher rejects an
action unless all of these are true:

- The signing key ID is locally trusted.
- The signature covers the complete canonical JSON envelope.
- The envelope targets this exact node.
- Its issue and expiry window is valid and no longer than five minutes.
- The action's node agrees with the envelope node.
- Its idempotency key is either new or maps to the same envelope digest.

The durable ledger is appended and synced after successful execution. There is
an unavoidable crash window between a host mutation and that append, so each
containerd operation is independently idempotent as well: pulls may repeat,
create recognizes only matching a4s-managed containers, and start returns the
existing running task.

```text
signed envelope stream
        |
        v
 signature / target / expiry
        |
        v
 durable idempotency ledger
        |
        v
 narrow ContainerBackend contract
        |
        v
 containerd -> OCI spec -> runc
```

## Container safety defaults

The adapter requires immutable `sha256` image references and verifies the
resolved manifest digest after pull. Create applies the goal's CPU and memory
limits, a PID limit of 256, `noNewPrivileges`, an empty Linux capability set,
and a namespaced cgroup. Allocation logs are written under the configured log
directory. A container that already exists is accepted only when its managed,
workload, and image labels match the requested allocation.

`allocation.running` means the OCI task started. It does not mean the service
is ready. Readiness must come from a separate process, TCP, or HTTP probe and
be returned as independent evidence before a route or rollout is authorized.

## Build and run on a disposable Linux node

Requirements are containerd 2.x with runc and an installed snapshotter. The
node process needs permission to access the containerd socket and its own state
and log directories.

```bash
go build -o a4s ./cmd/a4s

./a4s node \
  --node-id edge-1 \
  --key-id control-1 \
  --public-key /etc/a4s/control-1.pub \
  --ledger /var/lib/a4s/node-ledger.jsonl \
  --containerd /run/containerd/containerd.sock \
  --namespace a4s \
  --log-dir /var/log/a4s/allocations
```

The public-key file contains the raw 32-byte Ed25519 public key encoded as
standard or unpadded base64. Signed-action JSON values are read from standard
input and dispatch-result JSON values are written to standard output. This
stream is a temporary harness, not the final node transport.

## Verification

The contract tests use a fake backend and assert that only hardened container
specs cross the runtime boundary. Linux cross-compilation checks the real
containerd API adapter. A live smoke test remains necessary on a disposable
node when the development host does not expose a Linux containerd socket.

```bash
go test -race ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/a4s-node.test ./node
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/a4s ./cmd/a4s
```

## Deliberately missing

- Controller-to-node transport and mutual node authentication.
- Stop/delete actions and restart supervision.
- Process and application readiness probes.
- CNI network namespaces and service routing.
- Seccomp/AppArmor profile selection, user namespaces, and rootless execution.
- Reconciliation of orphaned containerd resources after a node restart.

These are required before running a production workload. The next useful
milestone is a server-issued envelope reaching a disposable node, followed by
independent readiness evidence returning to the server.
