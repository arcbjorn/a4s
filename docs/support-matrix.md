# Support matrix

What this build is known to work against, and what it deliberately does not
support. "Supported" here means the combination is exercised by CI or has been
run by hand; it does not imply production readiness. See
[project status](project-status.md) for maturity.

## Toolchain

| Component | Version | Notes |
| --- | --- | --- |
| Go | 1.26.3 or later | Required by containerd v2.3.2, which declares it. Earlier toolchains cannot build the module. |
| containerd | 2.3.x | The node uses the official v2 Go client. v1 is not supported: the client API differs. |
| runc | 1.1 or later | Whatever containerd is configured to use. a4s does not invoke runc directly. |
| CNI plugins | 1.4 or later | The node writes a `bridge` network configuration with `host-local` IPAM and invokes the plugin binaries in `--cni-bin`. |
| nftables | 1.0 or later | Only when `--nft` is set. The compiled ruleset uses `inet` tables and `ct state` matching. |
| Caddy | 2.7 or later | Only when `--gateway-admin` is set. The node drives the admin API. |
| SQLite | embedded 3.53 | Provided by `modernc.org/sqlite`, a pure-Go implementation. No system SQLite is used and no CGO is required. |
| libSQL / Turso | compatible | The schema and every query the store issues have been applied to a real libSQL server. Moving to Turso is a driver and DSN change, not a schema rewrite. |

## Platforms

| Target | Server | Node | Notes |
| --- | --- | --- | --- |
| linux/amd64 | supported | supported | The primary target. Node features require a Linux kernel. |
| linux/arm64 | supported | cross-built | Built and unit-tested in CI; not yet exercised against a live containerd. |
| darwin/arm64 | supported | unsupported | The operator CLI and server run; the container runtime refuses to start. |
| darwin/amd64 | supported | unsupported | As above. |
| windows | unsupported | unsupported | Not built and not tested. |

The node's container, network, and volume adapters are behind a Linux build
tag. On other platforms `a4s node` reports that the containerd runtime adapter
requires Linux rather than failing obscurely later.

## Kernel features

The node needs these when the corresponding feature is enabled:

| Feature | Requirement |
| --- | --- |
| Containers | cgroup v2, overlayfs or the configured snapshotter |
| Allocation networking | network namespaces, veth, bridge, IP forwarding |
| Network policy | nftables with `inet` family support and `NET_ADMIN` |
| Volumes | a filesystem supporting the configured volume root |

## Verifying a host

```bash
# Toolchain and platform of a built binary.
a4s version --json

# Compile a representative ruleset and apply it to a real kernel.
./scripts/check-nftables.sh
```

## What is not supported

- Kubernetes API compatibility. a4s is not a Kubernetes distribution and
  serves no Kubernetes API.
- Multi-server control planes. One server owns the event log; there is no
  consensus or failover.
- IPv6 allocation addressing. The CNI configuration and policy compiler both
  assume IPv4.
- Rootless or user-namespaced containers.
- containerd v1, Docker, or Podman as the runtime.
- Sharing one event log between two servers. SQLite serializes writers and the
  chain-head guard turns a lost race into a refused append rather than a fork,
  but a4s has no leader election: one server owns its log.
