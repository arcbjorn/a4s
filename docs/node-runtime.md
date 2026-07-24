# Node runtime slice

The first Linux data-plane slice is implemented. It is intentionally narrower
than a kubelet: the node accepts signed typed actions and translates
`pull_image`, `create_allocation`, `start_allocation`, `stop_allocation`, and
`delete_allocation` into containerd calls, and `publish_route` into a gateway
route snapshot. It has no generic command or shell endpoint.

The node holds a durable record of server-authorized intent and supervises it.
That is what lets workloads survive a control-plane outage: a container that
dies while the server is unreachable is restarted locally, bounded by a
crash-loop budget. The node never invents intent, changes images, or expands its
own authority; it can only keep true what the server already authorized.

## Trust boundary

Every input is an Ed25519-signed `SignedAction`. The dispatcher rejects an
action unless all of these are true:

- The signing key ID is locally trusted.
- The signature covers the exact transmitted envelope bytes and is verified
  before those bytes are decoded.
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

- Mutual node authentication and an encrypted network transport. The protocol is
  implemented and carried over a stream; only the authenticated tailnet
  connection is missing.
- Automatic cleanup of orphaned containerd resources. Orphans are discovered and
  reported, but removal stays an authorized action rather than a node decision.
- CNI network namespaces and per-allocation addressing. Probes currently reach a
  workload on loopback, which is why the endpoint resolution is deliberately
  conservative.
- Seccomp/AppArmor profile selection, user namespaces, and rootless execution.
- Image and snapshot garbage collection.
- A concrete gateway backend behind the route snapshot interface.

These are required before running a production workload. The next useful
milestone is running the existing round trip against a live containerd on a
disposable Linux host.
