---
title: "Changing the Isolation Mode"
weight: 7
description: "Declare a new isolation mode in spec.isolation and apply it with the node's next repave."
---

Setting `spec.isolation` on a Machine declares the mode you want. It does not
move the node. The change takes effect at the node's next repave, the same
repave that applies a new Kubernetes version or node labels: when the Node object
is deleted, the agent rebuilds the node, and if the declared mode differs from
the one that runs, it builds the node in the declared mode.

Until then, Machina reports the difference in the `IsolationDrift` condition and
nothing else happens. An empty `spec.isolation` declares nothing: a repave
leaves the node in whatever mode it runs.

{{< callout type="important" >}}
Changing the mode is experimental and has been validated only on QEMU test hosts.
Any change that involves `container` is experimental because that mode is. The
agent checks the target mode's host requirements before it touches the node. It
does not cordon or drain the node; see [Change the mode](#change-the-mode).
{{< /callout >}}

## Change the mode

1. Declare the mode:

   ```shell
   kubectl patch machine worker-01 --type=merge \
     -p '{"spec":{"isolation":"container"}}'
   kubectl get machine worker-01 \
     -o jsonpath='{.status.conditions[?(@.type=="IsolationDrift")]}'
   ```

   `IsolationDrift` turns `True` with reason `AppliedModeDiffers`. The node keeps
   running in its current mode. You can withdraw the change at any time before
   step 3 by setting `spec.isolation` back to the mode that runs.

2. Cordon and drain the node. The agent never touches Node objects for a mode
   change; draining is your job, as it is for any repave:

   ```shell
   kubectl cordon worker-01
   kubectl drain worker-01 --ignore-daemonsets --delete-emptydir-data
   ```

3. Delete the Node. This triggers the repave that applies the declared mode:

   ```shell
   kubectl delete node worker-01
   ```

4. Watch the result. The kubelet registers the Node again once the new mode
   runs:

   ```shell
   kubectl get machine worker-01 -o jsonpath='{.status.isolationChange}'
   kubectl get node worker-01
   kubectl uncordon worker-01
   ```

If the Machine also has a newer machine configuration assigned, the same repave
applies it. The new mode and the new Kubernetes version are built together.

## Example: nspawn to none

This is the full sequence for moving an existing `nspawn` node, `worker-01`, to
`none`. Any change involving `none` is a reprovision, so the host is reset and
bootstrapped again; read [What happens](#what-happens-from-nspawn-to-none) and
[Caveats](#caveats) before you start. Run the commands from a machine with
`kubectl` access to the cluster.

**1. Check the starting point.** The node runs in `nspawn`, nothing is declared
that differs, and there is no change in flight:

```shell
kubectl get machine worker-01 \
  -o jsonpath='{.status.isolation}{"\n"}{.status.isolationChange}{"\n"}'
# nspawn
```

**2. Check the credential the new bootstrap will use.** The reset removes the
agent, so the bootstrap that follows needs a way to authenticate. The host
either bootstrapped with attestation (nothing to do) or the bootstrap token in
its applied config must still be valid. If you are unsure, renew the token
before you continue. An expired token is caught before anything is reset, but it
costs you an attempt.

**3. Declare the mode.** Nothing moves yet:

```shell
kubectl patch machine worker-01 --type=merge -p '{"spec":{"isolation":"none"}}'
kubectl get machine worker-01 \
  -o jsonpath='{.status.conditions[?(@.type=="IsolationDrift")].reason}{"\n"}'
# AppliedModeDiffers
```

To abandon the change, patch `spec.isolation` back to `nspawn` before step 5.

**4. Drain.** The agent never drains. Cordon and drain the way you do for any
repave, and make sure workloads with local state have been moved or saved:

```shell
kubectl cordon worker-01
kubectl drain worker-01 --ignore-daemonsets --delete-emptydir-data
```

**5. Delete the Node.** This is the trigger:

```shell
kubectl delete node worker-01
```

**6. Wait and verify.** The node is offline while the host resets and
bootstraps. Poll until the Node is back and the change reports `Succeeded`:

```shell
kubectl get machine worker-01 -o jsonpath='{.status.isolationChange}{"\n"}'
# {"phase":"Succeeded","strategy":"Reprovision","targetMode":"none", ...}
kubectl get node worker-01
kubectl get machine worker-01 -o jsonpath='{.status.isolation}{"\n"}'
# none
```

**7. Return the node to service:**

```shell
kubectl uncordon worker-01
```

To do several nodes, repeat steps 1 to 7 for one node at a time and confirm the
first one is healthy before you continue. Nothing coordinates nodes for you.

### What happens from nspawn to none

After you delete the Node, the daemon on `worker-01`:

1. Runs the static `none` host checks and confirms the bootstrap will have a
   credential. A failure here is recorded as `Failed` and the node keeps running
   in `nspawn`.
2. Stages a copy of the agent, the `none` configuration and a journal, then
   starts `unbounded-agent-reprovision.service` and records `InProgress`.
3. The unit resets the host: it stops the `nspawn` machine, removes the
   machine image, the agent and the daemon. The kubelet and containerd inside
   the `nspawn` machine go away with it.
4. The unit bootstraps the host again in `none` mode. kubelet, containerd and
   LocalDNS now run directly on the host as `unbounded-containerd`,
   `unbounded-kubelet` and `unbounded-localdns` units, installed under
   `/opt/unbounded/node/<version>`.
5. The kubelet registers the Node with the same name, labels and taints. The new
   daemon records `Succeeded` and removes the staging directory.

### Caveats

- **There is no rollback for a reprovision.** An in-place change restores the old
  stack on failure; a reprovision resets the host first. A failure before the
  reset leaves the node in `nspawn`; after it, the only remedy is to fix the
  cause and let the unit retry, or to bootstrap the host again.
- **The unit retries five times,** 30 seconds apart. If the bootstrap never
  succeeds, the host stays reset with no agent, and `status.isolationChange`
  remains `InProgress` until an agent runs on the host again. Watch the host's
  `journalctl -u unbounded-agent-reprovision`, and see
  [When the bootstrap never succeeds](#when-the-bootstrap-never-succeeds).
- **Downtime.** The node is out of the cluster from the Node deletion until the
  bootstrap finishes, including the download of the Kubernetes components.
- **State is not carried over.** Pod data and the image store inside the
  `nspawn` machine are lost, and images are pulled again. Host-global state
  outside the node stack is untouched.
- **The credential must still be valid** (see step 2). A host bootstrapped by an
  earlier agent has no recorded attestation and uses the token.
- **You are responsible for draining,** and for ordering a fleet. Do not delete
  the Nodes of several machines in a way that removes capacity your workloads
  need.
- **Going back is another reprovision.** Moving from `none` to `nspawn` is the
  same procedure with `spec.isolation` set to `nspawn`; the target needs
  `systemd-container`, which the bootstrap installs.
- **Old agents ignore the key.** An agent from before isolation modes does not
  read `spec.isolation`; upgrade the agent first.
- **`container` is experimental,** and any change that involves it inherits that.

For a change between `nspawn` and `container`, use the same steps with
`spec.isolation` set to the other mode. The change is made in place: the new
stack is built beside the old one and the old one is restored if anything fails,
so the caveats about rollback and credentials do not apply, but the node is
still down while the stacks switch and the Node is still recreated.

## What the status says

The agent records its attempt in `status.isolationChange`; the fields are listed
under [status]({{< relref "reference/machina-crd#status" >}}). `phase` moves from
`InProgress` to `Succeeded` or `Failed`, `strategy` says how the change was made,
and `message` says why a failed attempt failed and whether the source was
restored. `status.isolation` is the mode the agent reports as applied.
The `IsolationDrift` condition compares it with `spec.isolation`; its reasons are
listed under [Drift]({{< relref "reference/agent/isolation#drift" >}}).

A failed change is not retried on its own. The node keeps running in the mode it
had, and deleting the Node again starts a new attempt. Fix the cause, or set
`spec.isolation` back to the mode that runs.

You deleted the Node to start the repave, and a running kubelet does not create
a deleted Node object by itself. After a failed change the agent therefore waits
a short time for the Node to return, which it does when a rolled-back node stack
restarts. If the Node has not come back, the agent restarts the node stack so the
kubelet registers it again.

## Supported changes

The agent chooses the strategy from the pair of modes. You do not request one.

| From \ To | `nspawn` | `container` | `none` |
|---|---|---|---|
| `nspawn` | n/a | in place | reprovision |
| `container` | in place | n/a | reprovision |
| `none` | reprovision | reprovision | n/a |

`nspawn` and `container` can run side by side in different slots, so a change
between them prepares the target while the source still runs. `none` has no slot
to prepare beside the running node, so any change involving it resets the host
and bootstraps it again.

Declaring the mode the node already runs does nothing.

## Before the node is touched

A change that cannot succeed is refused before it changes anything. The attempt
is recorded as `Failed` with the reason, the node keeps its mode, and the Node
registers itself again. The checks include:

- The target mode's host requirements. A change to `nspawn` needs
  `systemd-container` (`machinectl`, `systemd-nspawn`) on the host. The agent
  does not install it for an in-place change; a reprovision does.
- For a reprovision, that the bootstrap after the reset will have a credential
  (see [Reprovision](#reprovision)).

## In-place steps

The target is built in the alternate slot (`kube1` and `kube2`), from the
repave's desired configuration with `Isolation` set to the declared mode.

1. Preflight the target backend. Failure here changes nothing.
2. Download the target's artifacts. Failure here changes nothing.
3. Save the host configuration files that both stacks write at the same paths:
   the kubelet configuration, kubeconfig and client credentials, the containerd
   configuration, the CNI configuration and the LocalDNS files. They are copied
   byte for byte, with their modes and owners, to
   `/etc/unbounded/agent/isolation-migration-config`, next to the journal. Failure
   here changes nothing.
4. Prepare the target in the alternate slot. Leftovers of an earlier attempt in
   that slot are removed first.
5. Stop the source node stack.
6. Clean up host networking, as a repave does.
7. Start the target. Kubelet gets fresh credentials through the normal
   bootstrap flow: the bootstrap token in the configuration (the one a repave
   uses) is exchanged for a new kubelet client certificate. No kubelet
   credentials are copied. The bootstrap token is not renewed, so a change to a
   node whose token has expired is no different from a repave of it. The Node
   name, labels, and taints are kept.
8. Record the new mode and slot in the applied config.
9. Wait for the target to come up, for up to five minutes, with the check a
   repave makes (kubelet active on the host, with its credentials).
10. On success, destroy the source slot, remove its applied config and the saved
    configuration. `status.isolation` is updated from the new applied
    config and `status.isolationChange` becomes `Succeeded`.

### Rollback

If anything fails after the target exists, the agent stops the target, removes
its applied config and restores the source's, destroys the target, puts the
saved host configuration back (so files the target rewrote or removed, such as
the kubelet credentials, are the source's again, and files it added are gone),
and restarts the source. `status.isolationChange` then reports `Failed` with the
failure and `rolled back to <mode>`. If the source could not be restored the
message says so and the node needs attention. If the configuration could not be
put back the message says so and the saved copy is kept in
`/etc/unbounded/agent/isolation-migration-config`. The source slot and its
applied config are kept until step 9 passes.

### Interruption

Before each step that changes the host, the daemon writes the step to
`/etc/unbounded/agent/isolation-migration.json` (atomically, next to the applied
configs). A daemon that restarts mid-change resolves the journal itself when it
starts, before it takes any work, and records the outcome in
`status.isolationChange`:

| Last step recorded | At the next daemon start |
|---|---|
| `preparing` | The source never stopped. The partial target is removed and the saved host configuration is put back. The change is recorded as `Failed` with "interrupted". |
| `stopping-source`, `starting-target`, `persisting` | Roll back as above: stop and remove the target, restore the source's applied config and the saved host configuration, restart the source. The change is recorded as `Failed` with "interrupted". |
| `awaiting-ready` | Wait for the target again. On success finish the change; on timeout roll back. |
| `ready` | Finish the cleanup of the source and remove the saved configuration. The change is recorded as `Succeeded`. |
| `complete`, `rolled-back`, `failed` | Record the outcome and remove the journal. |

A step that crashed the daemon is rolled back, not repeated, so a step that
cannot succeed does not loop. While both slots hold an applied config, the
journal decides which one describes the running node.

## Reprovision

A reprovision resets the host and bootstraps again in the new mode, with the
repave's desired configuration and `Isolation` set to the declared mode. It is
the only strategy for any change involving `none`, and the Node name, labels, and
taints are kept. The reset is the existing agent reset, in its existing order.

The reset removes the daemon and the agent binary, so the daemon cannot finish
the job. Instead:

1. The daemon checks the target's host requirements. For a `none` target it
   runs the static half of the `none` preflight: the cgroup filesystem,
   `systemctl`, and writable `/etc`, `/opt/unbounded/node` and `/var`. The other
   half refuses a host that runs a kubelet or containerd the agent does not
   own, and this one does until it is reset, so that half runs in the bootstrap
   after the reset, on the clean host.
2. It checks that the bootstrap after the reset will have a credential. If the
   host was bootstrapped with attestation (the agent recorded the attestation
   URL in `/etc/unbounded/agent/attest.json`), the staged configuration carries
   it and the bootstrap attests again for a new token. Otherwise the bootstrap
   token recorded in the applied config is used, and the daemon asks the API
   server whether it still accepts it. If the server answers `401 Unauthorized`
   (the token expired or was revoked), the change fails here, before anything is
   reset, with a message to renew the token or bootstrap with attestation and try
   again. If the server cannot be asked, the change goes ahead. A host that
   authenticates with an exec credential has no token to check. A host
   bootstrapped by an earlier agent has no recorded attestation URL and takes the
   token path.
3. It stages `/var/lib/unbounded-agent-reprovision` with a copy of the agent
   binary, the target configuration, and a journal, and asks the copy to
   validate the configuration and the bootstrap plan for the target mode. A mode
   with no plan fails here, before anything changes.
4. It records the journal step `reprovisioning` and starts the unit
   `unbounded-agent-reprovision.service`, then returns. `status.isolationChange`
   stays `InProgress`.
5. The unit runs the hidden `isolation-reprovision` command from the copy. It
   waits for the daemon to release installation ownership, runs the agent reset,
   then runs the bootstrap for the target mode. A run that fails is retried by
   systemd every 30 seconds, and the unit is enabled so a reboot continues it.
   A reset that finished is not repeated. The journal counts the runs.
6. The daemon that bootstrap installs finds the journal when it starts. Once the
   applied config records the target mode, it records `Succeeded` in
   `status.isolationChange` and removes the unit and the staging directory.

A failure before step 4 undoes the staging and is recorded as `Failed`.

### When the bootstrap never succeeds

After five runs have failed, the command stops retrying. It records the journal
step `failed` with the last error, removes the unit and the staged agent binary
and configuration (which holds the bootstrap token), and exits successfully so
systemd does not run it again. Only the journal stays in
`/var/lib/unbounded-agent-reprovision`.

The host is then reset and has no daemon, so nothing can update
`status.isolationChange` yet: it stays `InProgress` until an agent runs on the
host again. Bootstrap the host again, for example with the install script. The
daemon it installs reads the journal, records `Failed` with the recorded message,
and removes the directory. Until then the journal
(`/var/lib/unbounded-agent-reprovision/journal.json`) and the unit's log
(`journalctl -u unbounded-agent-reprovision`) say why.

`unbounded-agent reset` removes the reprovision unit, its directory and its
journal along with the rest of the agent, so a reset after a failed reprovision
leaves nothing behind. The reprovision's own reset keeps them.

## State

- Pod data and the image store are not carried over. Images are pulled again.
- Host-global state is untouched; see
  [Operations]({{< relref "reference/agent/isolation#operations" >}}).
- A repave that does not change the mode is unchanged.

## Verify

```bash
kubectl get machine worker-01 -o jsonpath='{.status.isolationChange}{"\n"}{.status.isolation}{"\n"}'
kubectl get node worker-01
sudo unbounded-agent exec -- crictl ps
```

On success `status.isolationChange.phase` is `Succeeded`,
`status.isolation` is the declared mode, and `IsolationDrift` is `False`
with reason `AppliedModeMatches`.

## In-place upgrades without deleting the Node

A change to the mode, like any repave, replaces the node stack and needs the Node
object deleted. Swapping only the versions of containerd and kubelet on the host,
without deleting the Node, is not built. The `none` backend is the extension
point: it installs each version in its own directory under
`/opt/unbounded/node/<version>` and switches an atomic `current` link, so a
future trigger could flip the link without a repave. Nothing triggers that today.

## See also

- [Isolation Modes]({{< relref "reference/agent/isolation" >}})
- [Node and host operations]({{< relref "guides/operations" >}})
