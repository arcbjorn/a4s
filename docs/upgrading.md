# Upgrading and rolling back

How to move a running a4s deployment to a new build, and how to get back if it
goes wrong. See [support matrix](support-matrix.md) for what each build
requires.

## Before upgrading

Take a verified backup of controller state. The event log is the only
authoritative state a4s holds; everything else is derived from it.

```bash
a4s backup --event-log /var/lib/a4s/events.log --out /backups/events-$(date -u +%Y%m%dT%H%M%SZ).log
a4s backup --verify /backups/events-<stamp>.log
```

The verify step is not optional. It re-derives the hash chain and checks the
recorded head, which is what distinguishes a recovery point from a file that
happens to exist.

Record what is running, so a later comparison is possible:

```bash
a4s status --server https://control:8443 --key-id ops --operator-key ~/.a4s/ops.key --json > /tmp/before.json
a4s version --json
```

## Order of operations

Upgrade the server before the nodes. The server issues capabilities and the
nodes execute them, so a server that understands fewer action kinds than its
nodes is harmless, while the reverse means a node receives work it cannot
perform.

1. **Stop the server.** Running workloads are unaffected: the node supervises
   its own desired state during a control-plane outage.
2. **Replace the binary** and start it. Recovery is the normal startup path, so
   the world projection rebuilds from the event log automatically. Confirm the
   recovered revision matches what you recorded.
3. **Upgrade one node**, confirm it re-enrolls and its workloads stay running,
   then continue with the rest.

```bash
# On the server, after replacing the binary:
a4s server --event-log /var/lib/a4s/events.log --status
```

A node is upgraded by replacing its binary and restarting it. Actions the
server already dispatched are replayed on reconnect; the durable idempotency
ledger makes that safe, and replay after a node restart is covered by the
acceptance suite.

## Rotating the controller signing key

Rotation is separate from upgrading and can be done independently. It is three
steps because a signature outlives the moment it was made:

```bash
a4s keys rotate --keyset /etc/a4s/keyset.json --key-id control-2 --out /etc/a4s/control-2
# Distribute the keyset to every node and restart the server with the new key.
a4s keys retire --keyset /etc/a4s/keyset.json --key-id control-1
```

Retiring before every node has the new keyset will make those nodes reject
capabilities. See `a4s keys list` for the current state.

## Rolling back a workload

A failed deployment is a workload concern, not a a4s upgrade concern. When a
new image fails, a4s blocks the goal and names the last known-good digest
rather than silently reverting.

```bash
a4s approve --event-log /var/lib/a4s/events.log --goal web-public \
  --scope rollback --workload web \
  --operator you --key ~/.a4s/ops.key --key-id ops --reason "bad deploy"
```

The approval records both versions, so the rollback holds until the goal itself
is changed. Fixing the goal to name the known-good image ends the compensation.

## Rolling back a4s itself

If a new a4s build misbehaves:

1. Stop the server.
2. Restore the binary you were running before.
3. If the event log has been written by the newer build and the older one
   cannot read it, restore the backup you took:

```bash
a4s restore --from /backups/events-<stamp>.log --event-log /var/lib/a4s/events.log
```

`restore` verifies the archive before touching the destination and preserves
any existing log alongside the restored one, so a bad restore does not destroy
the history it was meant to recover.

Nodes can be rolled back the same way: replace the binary and restart. A node
holds no authoritative state beyond its idempotency ledger and desired-state
cache, both of which are safe to replay against.

## After upgrading

```bash
a4s status --server https://control:8443 --key-id ops --operator-key ~/.a4s/ops.key
a4s events --server https://control:8443 --key-id ops --operator-key ~/.a4s/ops.key --limit 20
```

Compare the recovered revision, allocation count, and route count against what
you recorded. A lower allocation count after an upgrade means workloads were
lost, not that the upgrade succeeded quietly.
