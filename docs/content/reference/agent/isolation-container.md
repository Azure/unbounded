---
title: "container Mode (Experimental)"
weight: 6
description: "Experimental mode that runs the Kubernetes node stack in a privileged OCI container, for Azure Container Linux."
---

`container` mode runs the node stack in a privileged OCI container on hosts that
cannot run systemd-nspawn or install host packages, such as Azure Container
Linux. See [Isolation Modes]({{< relref "reference/agent/isolation" >}}) to
compare.

{{< callout type="warning" >}}
`container` mode is experimental and has not yet been validated on real hosts or
on Azure Container Linux. Details here may change. Do not rely on it for
production hosts.
{{< /callout >}}

## Target host

Azure Container Linux is derived from Flatcar. These properties are confirmed by
Microsoft documentation: a read-only `/usr` protected by dm-verity, weekly
image-based updates, a signed unified kernel image with Secure Boot, and
enforcing SELinux. Public documentation does not cover container runtimes,
kernel modules, or mount propagation, so the requirements below are assumptions
that have not been checked on a node.

## Requirements

- Writable `/etc`, `/opt`, and `/var`. `/usr` is read-only and is not used.
- A loadable `runc`. The agent downloads it to
  `/opt/unbounded/runtime/runc` (the version and download source follow the
  configured `CRI.Runc` version and download override, and offline artifacts),
  and only in this mode. Only `runc` is supported. The host's own docker and
  containerd are not used, and the host's containerd and kubelet units are
  disabled because the container shares `/run/containerd` with the host.
- cgroup v2.
- Kernel modules the node stack needs. The agent loads them on the host and
  lists them in `/etc/modules-load.d/unbounded-kubernetes.conf`, because the
  container does not load modules at boot. `/lib/modules` is bound read-only
  into the container when it exists.
- With SELinux enforcing, support for the `spc_t` process label. Preflight checks
  it.
- Ignition provisioning (`ProvisioningFormat: ignition`). No new format is added.

Select it as described in
[Selecting a mode]({{< relref "reference/agent/isolation#selecting-a-mode" >}}).

## How it works

The agent reuses the OCI unpack step to place an `agent-*` rootfs image (with
systemd as PID 1) under `/var/lib/unbounded/containers/<slot>`, generates an OCI
runtime bundle (`config.json`) under `/var/lib/unbounded/containers/bundles/<slot>`,
and runs it with `runc`. An agent-owned unit,
`unbounded-node-<slot>.service`, supervises the container. Slots are `kube1` and
`kube2`, as in `nspawn`. The in-rootfs configuration (containerd, kubelet,
CNI, LocalDNS) is written by the same phases the `nspawn` mode uses, against the
container root, and commands inside the container run through `runc exec`.

Paths under the bind-mounted same-path directories (`/var/lib/kubelet`,
`/opt/cni`, `/etc/containerd/certs.d`, `/run/containerd`, `/sys/fs/bpf`) are
written by the agent on the host side, because a bind mount hides anything
written below it in the rootfs. Both slots share these directories.

The container is privileged and shares the host network and, as needed, PID
namespaces. It bind-mounts the same-path contract directories and uses
`rshared` propagation for `/var/lib/kubelet`. Device and cgroup rules come from
the same GPU, device, and mount grants that `nspawn` renders as `.nspawn` binds.
When SELinux is enforcing, the process label is `spc_t`.

Lifecycle reconciliation (pre-start, post-start, reconcile) runs through a hidden
generic `node-lifecycle` command, invoked by the unit from the agent's
rollback-stable helper copy. Pre-start regenerates the bundle, so SELinux or
device changes are picked up on every start. The `nspawn-lifecycle` command and
its config are untouched. Hook commands supplied through the nspawn lifecycle
configuration are not used by this mode.

## Operations

| Operation | Behavior |
|---|---|
| Bootstrap | Host preparation, unpack the rootfs to a slot, generate the OCI bundle, write the unit, start the container, start the stack inside. |
| Start | Start the container unit, then the in-container units. |
| Stop | Stop the in-container units, then the container. |
| Soft reboot (`NodeReboot`) | Restart the container unit, which reruns the lifecycle hooks. The host does not reboot. |
| Repave and upgrade | Blue/green. The alternate slot is built beside the running one, then the old container is stopped, host networking is cleaned, the shared kubelet state is reset (pod mounts unmounted, previous kubelet credentials removed when a bootstrap token is used), and the new container is started and waited on. The kubelet, containerd, CNI and LocalDNS configuration files both slots share on the host are saved first (in `/etc/unbounded/agent/container-upgrade-config`). If the new slot does not come up it is removed, the saved files are put back, and the old container is started again; the error says whether that worked. The applied config moves to the new slot only after it is healthy, then the old slot is removed. |
| Reset and delete | Stop the containers, remove the bundle, rootfs, and unit of each slot, the shared kubelet and CNI directories and the host runtime, then the host-global cleanup in the usual order. A container that is still alive after a failed stop keeps its bundle and rootfs, and the error says so. If a pod mount below `/var/lib/kubelet` cannot be unmounted the kubelet directory is kept, so a volume mounted there is never deleted through the mount. |
| Health | The container is running, kubelet is healthy, and LocalDNS answers when enabled. |
| Exec and logs | `unbounded-agent exec` runs through `runc exec`. `unbounded-agent logs` runs `journalctl` in the container. See [Running commands and reading logs]({{< relref "reference/agent/isolation" >}}#running-commands-and-reading-logs). |
| GPU and devices | The grants become OCI mounts and devices, and the container runs with every device allowed. The `node-lifecycle` hooks wire NVIDIA. Experimental; see [NVIDIA]({{< relref "reference/gpu/nvidia#gpu-support-by-isolation-mode" >}}) and [AMD]({{< relref "reference/gpu/amd#gpu-support-by-isolation-mode" >}}). |
| LocalDNS | CoreDNS runs in the container and the LocalDNS network unit is unchanged. |
| Uninstall | The uninstall script reads the mode from the applied config and runs the `container` cleanup. It is not a reset; see [What the uninstall script removes](#what-the-uninstall-script-removes). |

## Limits

- **Experimental.** Behavior can change when the mode is validated on Azure
  Container Linux.
- **Shared directories.** Both slots share the same-path directories, so a
  repave rewrites the kubelet configuration and CNI files the previous slot also
  sees. The agent saves these files before an upgrade and puts them back if the
  new slot does not come up.
- **NVIDIA GPUs.** GPU wiring is implemented but has not run on a GPU host. A
  node provisioned without NVIDIA devices stays CPU-only, because adding a GPU
  to it needs a new rootfs. A host that reports NVIDIA devices with incomplete
  driver state fails the start, as in `nspawn`. See
  [NVIDIA]({{< relref "reference/gpu/nvidia" >}}).
- **Runtime.** The OCI runtime is `runc`. Starting a node whose runtime is missing
  fails with an error that names it.
- **Read-only binds.** A read-only bind (the kernel modules directory, a read-only
  mount grant, a library path) uses the `rro` option, which makes its submounts
  read-only too, when the installed runc is 1.1 or newer and the kernel is 5.12 or
  newer. Otherwise it uses `ro`, and submounts of the bound directory stay
  writable. The choice is made when the bundle is rendered, which happens before
  every start.
- **Bind targets.** Targets are cleaned (`..` and repeated slashes removed) before
  the conflict check, and a target at or under `/proc` or `/sys` is refused: the
  container mounts those itself.
- **Restarts.** The supervising unit sets no start limit, so a container that
  keeps failing keeps being restarted, with a growing delay on systemd 254 or
  newer and every second before that.
- **Provisioning.** Whether Azure Container Linux accepts user-supplied Ignition
  when AKS provisions the node is unverified.
- **Changing the mode.** See
  [Changing the Isolation Mode]({{< relref "reference/agent/isolation-migration" >}}).

## What the uninstall script removes

The uninstall script is not a reset; see
[Uninstall script and reset]({{< relref "reference/agent/isolation#uninstall-script-and-reset" >}}),
which also lists what it removes in every mode.

For a `container` node the script stops and removes every
`unbounded-node-*.service` unit and removes the active slot's root filesystem,
`/var/lib/unbounded/containers/<slot>`.

It leaves, for a `container` node:

- The OCI bundles under `/var/lib/unbounded/containers/bundles` and the other
  slot's root filesystem, if one exists.
- The runtime, `/opt/unbounded/runtime/runc`.
- The shared directories `/var/lib/kubelet`, `/etc/cni/net.d`, `/opt/cni/bin` and
  `/var/lib/unbounded/container-images`.
- `unbounded-agent-daemon.service` (it is not stopped) and the agent files under
  `/opt/unbounded/agent`.
- Runtime state of a container that did not stop (the script does not run
  `runc delete`).

## Troubleshooting

| Symptom | Check |
|---|---|
| Preflight fails on the runtime | An existing `/opt/unbounded/runtime/runc` cannot execute. Check that `/opt/unbounded` is writable and not mounted `noexec`. A missing runtime is not an error at preflight, since bootstrap installs it. |
| Unit `unbounded-node-<slot>.service` keeps restarting | `journalctl -u unbounded-node-kube1`. The unit restarts the container; the pre-start hook regenerates the bundle and the post-start hook waits for the runtime to report it running. |
| Preflight fails on the SELinux label | The host policy does not allow the `spc_t` label. Check `getenforce` and the audit log. |
| Container starts but kubelet does not | `sudo unbounded-agent logs --unit kubelet --lines 200`, then `sudo unbounded-agent exec -- systemctl status kubelet`. |
| Missing kernel module | Confirm the module loads on the host. The container uses the host kernel and the agent loads the module list on the host. |
