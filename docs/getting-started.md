# Getting started

This guide starts at the repository root, where `go.mod` and `README.md` live.

## Prerequisites

For control-kernel development on any supported Go host:

- Go 1.26.3 or newer in the Go 1.26 line.
- Git for normal source-control work.
- Network access for the initial module download.

For the real node runtime:

- Linux amd64 or arm64.
- containerd 2.x and runc.
- A configured containerd snapshotter.
- Permission to access `/run/containerd/containerd.sock`, or another configured
  socket.
- Writable state and log directories.

macOS can build and test the control plane and can cross-compile the Linux
adapter. `a4s node` intentionally returns an unsupported-platform error outside
Linux.

## Bootstrap dependencies

```bash
go mod download
go mod verify
```

No generator, external database, local service, or vendored tool is required
for the current tests and simulation. SQLite is embedded through a pure-Go
driver, so no system SQLite and no CGO are needed.

## Run the fast path

```bash
go test ./...
go run ./cmd/a4s validate --file examples/web-service.json
go run ./cmd/a4s simulate --file examples/web-service.json
```

The simulation should finish with `goal.achieved` at world revision 7, with one
allocation, one published zone, and one route. It mutates only memory.

Run the race suite before handing off a change:

```bash
go test -race ./...
```

## Persist and inspect the event trail

The event-log path must be absolute. Use a new path for a clean sequence:

```bash
go run ./cmd/a4s simulate \
  --file examples/web-service.json \
  --event-log /tmp/a4s-events.log \
  --json
```

The event log is a SQLite database in WAL mode. Each record holds the event, its
sequence, the prior record hash, and its own hash. Reopening the log verifies
the entire chain before accepting another append. Read recorded history with
`a4s history` rather than by inspecting the database directly.

```bash
go run ./cmd/a4s history --event-log /tmp/a4s-events.log
```

Do not place secrets in a scenario, objective, event message, evidence map, or
event-log pathname.

## Understand the example

`examples/web-service.json` combines a `Goal` with a starting `World` for a
deterministic simulation. It asks for one public web replica on nodes labeled
`pool=base`. It includes a separately represented public-route approval.

The image digest is intentionally sixty-four zeroes. It satisfies format
validation but does not identify a real registry object. Replace it with an
actual digest before any Linux runtime experiment.

Useful edits to explore policy behavior:

- Remove the `public-route` approval and observe the route proposal denial.
- Change the required pool to a nonexistent value and observe placement block.
- Reduce node capacity below the workload request.
- Set `privileged` or `stateful` to true and observe scenario rejection.
- Increase replicas and observe one-replica-per-proposal reconciliation.

Keep experimental scenarios under `examples/` and never embed credentials.

## Build binaries

Build for the current host:

```bash
mkdir -p build
go build -trimpath -o build/a4s ./cmd/a4s
./build/a4s version
```

Cross-build the Linux node binary:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -o build/a4s-linux-amd64 ./cmd/a4s
```

Use `GOARCH=arm64` for an arm64 Linux node.

## Node command

The stdin stream harness, useful for isolating the runtime adapter:

```bash
./a4s node \
  --node-id edge-1 \
  --key-id control-1 \
  --public-key /etc/a4s/control-1.pub \
  --ledger /var/lib/a4s/node-ledger.jsonl \
  --containerd /run/containerd/containerd.sock \
  --namespace a4s \
  --log-dir /var/log/a4s/allocations
```

It reads one or more `SignedAction` JSON values from standard input and writes
one `DispatchResult` JSON value per input, including for a rejected or failed
action, so one bad envelope does not terminate the node.

Generate controller keys with `a4s keygen` for a single key, or `a4s keys init`
for a rotatable keyset the node accepts through `--keyset`:

```bash
./a4s keygen --out /etc/a4s/control-1
./a4s keys init --keyset /etc/a4s/keyset.json --key-id control-1 --out /etc/a4s/control-1
```

In normal operation the node does not read envelopes from standard input at all:
it enrolls with `a4s server --listen` and receives capabilities over the
authenticated, encrypted transport. The stdin harness remains useful for
isolating the runtime adapter.

Read the node-runtime and operations documents before running this command.

## Common failures

### Go version mismatch

If Go reports that the module requires 1.26.3, install the version listed in
`.go-version`. The required version follows the pinned containerd module.

### Module download fails

Confirm outbound access to the configured `GOPROXY`, then rerun `go mod
download`. Do not delete `go.sum` to work around checksum or proxy errors.

### Scenario rejects an image

The image must end in `@sha256:` followed by exactly sixty-four lowercase
hexadecimal characters. Tags alone are deliberately rejected.

### Public route never appears

The starting world needs a granted approval whose goal ID matches the goal and
whose scope is `public-route`.

### Event log fails on open

The path must be absolute, writable, and either empty or a valid event chain
created by this code. Corruption is treated as a hard failure. A store written by
a newer schema is also refused rather than misread: use the matching binary.

### Node command fails immediately on macOS

The real containerd adapter is Linux-only. Use the simulation and tests on
macOS, then cross-build or copy the source to a disposable Linux system.

## Where to continue

Read [project status](project-status.md), then take the first incomplete exit
criterion in [the roadmap](roadmap.md). The current recommended task is the
disposable Linux containerd smoke test, not networking or model integration.
