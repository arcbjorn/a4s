# Getting started

This guide starts at the root of the copied a4s project, where `go.mod` and
`README.md` live.

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

## Copy or extract the project

The folder is self-contained. After copying it, confirm these files exist:

```text
README.md
go.mod
go.sum
cmd/a4s/main.go
control/
eventlog/
node/
docs/
examples/
```

The current module path is `github.com/arcbjorn/a4s`; it does not need to match
the local directory. See the documentation index if you intend to publish
under a different module path.

## Bootstrap dependencies

```bash
go mod download
go mod verify
```

No generator, database, local service, vendored tool, or parent repository is
required for the current tests and simulation.

## Run the fast path

```bash
go test ./...
go run ./cmd/a4s validate --file examples/web-service.json
go run ./cmd/a4s simulate --file examples/web-service.json
```

The simulation should finish with `goal.achieved`, world revision 4, one
allocation, and one route. It mutates only memory.

Run the race suite before handing off a change:

```bash
go test -race ./...
```

## Persist and inspect the event trail

The event-log path must be absolute. Use a new path for a clean sequence:

```bash
go run ./cmd/a4s simulate \
  --file examples/web-service.json \
  --event-log /tmp/a4s-events.jsonl \
  --json
```

The file contains one JSON `Record` per line. Each record includes the event,
its sequence, the prior record hash, and its own hash. Reopening the file
replays and verifies the entire chain before accepting another append.

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

The current node command is a stream harness, not a network daemon:

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

It reads one or more `SignedAction` JSON values from standard input. It writes
one `DispatchResult` JSON value per successful input and exits on the first
error. The project does not yet include a key-generation or signing CLI because
key custody belongs in the future server. Tests in `node/dispatcher_test.go`
show how to generate an ephemeral Ed25519 key and call `node.Sign` for a local
experiment.

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
created by this code. Corruption is treated as a hard failure.

### Node command fails immediately on macOS

The real containerd adapter is Linux-only. Use the simulation and tests on
macOS, then cross-build or copy the source to a disposable Linux system.

## Where to continue

Read [project status](project-status.md), then take the first incomplete exit
criterion in [the roadmap](roadmap.md). The current recommended task is the
disposable Linux containerd smoke test, not networking or model integration.
