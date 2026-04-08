---
title: "Resetting a Node"
weight: 6
description: "Remove the unbounded-kube agent and restore a host to its original state."
---

This guide explains how to reset a node that was bootstrapped with unbounded-kube,
removing the agent, nspawn machine, network interfaces, and configuration files
to restore the host to its pre-bootstrap state.

## When to Reset

You may want to reset a node when:

- **Testing or development** -- you want to iterate on the bootstrap process
- **Decommissioning** -- you're removing a node from the cluster
- **Troubleshooting** -- you want a clean slate after a failed bootstrap

## Prerequisites

- Root access on the target host (or `sudo`)
- The `unbounded-agent` binary must still be installed on the host

## Resetting with `unbounded-agent reset`

The `unbounded-agent reset` command fully reverses the bootstrap process. It is
the inverse of `unbounded-agent start`.

### If the agent config file is available

When the agent config file is still on disk (e.g. `/etc/unbounded-agent/config.json`
from a cloud-init bootstrap, or a temporary file from a script bootstrap), the
reset command reads the machine name from it automatically:

```bash
# Cloud-init bootstrapped nodes (config at /etc/unbounded-agent/config.json):
sudo unbounded-agent reset

# Script bootstrapped nodes (config in a temp file):
sudo UNBOUNDED_AGENT_CONFIG_FILE=/tmp/unbounded-agent-config.XXXXXX.json unbounded-agent reset
```

### Specifying the machine name explicitly

If the config file is no longer available, provide the machine name directly:

```bash
sudo unbounded-agent reset --machine-name my-node
```

## What Reset Does

The reset command performs the following steps in order:

1. **Stops the nspawn machine** -- gracefully stops, then force-terminates if needed
2. **Removes network interfaces** -- WireGuard (`wg*`), tunnel (`geneve0`, `vxlan0`, `ipip0`), and overlay (`unbounded0`, `cbr0`) interfaces
3. **Removes WireGuard keys** -- cleans up `/etc/wireguard/server.priv` and `server.pub`
4. **Removes nspawn configuration** -- deletes `.nspawn` config and systemd overrides
5. **Removes the machine rootfs** -- deletes `/var/lib/machines/<name>`
6. **Cleans up routing** -- removes policy routing rules and flushes routing tables
7. **Removes agent binaries** -- deletes the agent binary and config artifacts
8. **Reloads systemd** -- picks up all configuration changes

The command is **idempotent** -- it is safe to run multiple times.

## After Reset

After resetting the host, you must separately delete the Kubernetes Node object
from the cluster:

```bash
kubectl delete node <machine-name>
```

You may also want to reboot the host to ensure all kernel modules and network
state are fully cleared:

```bash
sudo reboot
```

## See Also

- **[Bring Your Own Cluster]({{< relref "guides/existing-cluster" >}})** --
  set up a cluster and join nodes
- **[SSH Guide]({{< relref "guides/ssh" >}})** -- full provisioning lifecycle
  with SSH
