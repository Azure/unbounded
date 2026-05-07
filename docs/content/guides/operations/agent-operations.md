---
title: "Agent Operations"
weight: 3
description: "Upgrading the agent binary and resetting hosts."
---

Agent operations manage the host-resident `unbounded-agent` binary. They are
handled by the agent itself.

## AgentUpgrade

Replaces the host agent binary using blue-green staging with automatic rollback.
The operation requires a `downloadURL` parameter pointing to an agent release
tarball.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: upgrade-agent-worker-01
spec:
  machineRef: worker-01
  operationKind: AgentUpgrade
  parameters:
    downloadURL: https://example.com/releases/unbounded-agent-linux-amd64.tar.gz
```

```bash
kubectl apply -f upgrade-agent-worker-01.yaml
kubectl get mop upgrade-agent-worker-01 -w
```

```
NAME                       KIND            MACHINE      PHASE        AGE
upgrade-agent-worker-01    AgentUpgrade    worker-01    Pending      0s
upgrade-agent-worker-01    AgentUpgrade    worker-01    InProgress   2s
upgrade-agent-worker-01    AgentUpgrade    worker-01    Complete     15s
```

### How It Works

The agent uses a blue-green binary layout with two slots. Only one slot is
active at a time.

**Staging:**

1. Downloads the release tarball from `downloadURL`.
2. Extracts the agent binary into the inactive slot.
3. Runs `unbounded-agent version` against the staged binary as a preflight
   check. If this fails, the operation is marked `Failed` and the current
   binary is unchanged.

**Switching:**

4. Updates the `unbounded-agent-current` symlink to point to the newly staged
   binary.
5. Preserves the previous binary as `unbounded-agent-last-good`.
6. Restarts `unbounded-agent-daemon.service`.

**Rollback:**

If the upgraded daemon fails to stay healthy after the restart,
`unbounded-agent-daemon-recovery.service` triggers automatically:

1. Switches `unbounded-agent-current` back to `unbounded-agent-last-good`.
2. Restarts the daemon with the previous binary.
3. Records a rollback failure signal.
4. The recovered daemon marks the `MachineOperation` as `Failed` with reason
   `DaemonFailed`.

This ensures the agent always recovers to a known-good binary, even if the new
version crashes on startup.

### Failure Modes

| Failure | Result |
|---------|--------|
| Invalid or missing `downloadURL` | Operation marked `Failed` immediately. |
| Download or extraction error | Operation marked `Failed`. Current binary unchanged. |
| Preflight (`unbounded-agent version`) fails | Operation marked `Failed`. Current binary unchanged. |
| Upgraded daemon crashes after restart | Systemd recovery rolls back to last-good binary. Operation marked `Failed` with reason `DaemonFailed`. |

### Verifying the Upgrade

After a successful operation, confirm the agent version on the host:

```bash
# From the host
unbounded-agent version

# From the cluster, check the machine status
kubectl describe machine worker-01
```

## AgentReset

Removes the agent and all managed resources from a host, restoring it to its
pre-bootstrap state. This can be triggered remotely through a MachineOperation
or locally with the `unbounded-agent reset` command.

### Remote Reset via MachineOperation

Use this to reset agents remotely without SSH access to the host.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: reset-worker-01
spec:
  machineRef: worker-01
  operationKind: AgentReset
```

```bash
kubectl apply -f reset-worker-01.yaml
kubectl get mop reset-worker-01 -w
```

```
NAME               KIND          MACHINE      PHASE        AGE
reset-worker-01    AgentReset    worker-01    Pending      0s
reset-worker-01    AgentReset    worker-01    InProgress   1s
reset-worker-01    AgentReset    worker-01    Complete     10s
```

The daemon marks the operation complete before stopping its own running unit.

### Local Reset via CLI

If you have SSH or console access to the host, you can reset directly:

```bash
sudo unbounded-agent reset
```

This is the inverse of `unbounded-agent start` and performs the same cleanup as
the MachineOperation path.

### What Reset Does

The reset process performs these steps in order:

1. **Stops the nspawn machines** - gracefully stops `kube1` and `kube2`, then
   force-terminates if needed.
2. **Removes network interfaces** - WireGuard (`wg*`), tunnel (`geneve0`,
   `vxlan0`, `ipip0`), and overlay (`unbounded0`, `cbr0`) interfaces.
3. **Removes WireGuard keys** - cleans up `/etc/wireguard/server.priv` and
   `server.pub`.
4. **Removes nspawn configuration** - deletes `.nspawn` configs and systemd
   overrides for both machines.
5. **Removes the machine rootfs** - deletes `/var/lib/machines/kube1` and
   `/var/lib/machines/kube2`.
6. **Cleans up routing** - removes policy routing rules and flushes routing
   tables.
7. **Removes agent binaries** - deletes the agent binary and config artifacts.
8. **Reloads systemd** - picks up all configuration changes.

The reset is **idempotent** and safe to run multiple times. It unconditionally
cleans up both possible nspawn machine names (`kube1` and `kube2`) so it works
regardless of which upgrade cycle the node is in.

### When to Reset

- **Decommissioning** - removing a node from the cluster permanently.
- **Troubleshooting** - starting fresh after a failed bootstrap.
- **Testing or development** - iterating on the bootstrap process.

### After Reset

After resetting the host, delete the Kubernetes Node object from the cluster:

```bash
kubectl delete node worker-01
```

You may also want to reboot the host to ensure all kernel modules and network
state are fully cleared:

```bash
sudo reboot
```

## See Also

- [Machine Operations Overview]({{< relref "guides/operations" >}})
- [Node Operations]({{< relref "guides/operations/node-operations" >}})
- [MachineOperation CRD Reference]({{< relref "reference/machina-crd" >}})
