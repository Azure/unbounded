---
title: "container Mode (Experimental)"
weight: 6
description: "Experimental mode that runs the Kubernetes node stack in a privileged OCI container, for image-based hosts such as Azure Container Linux."
---

`container` mode runs the node stack in a privileged OCI container on hosts that
cannot run systemd-nspawn or install host packages, such as Azure Container
Linux. See [Isolation Modes]({{< relref "reference/agent/isolation" >}}) to
compare.

{{< callout type="warning" >}}
`container` mode is experimental and has not yet been validated on real hosts or
on Azure Container Linux. Details here may change. Do not rely on it for
production hosts. It is off unless you enable it: see
[Enabling the container mode]({{< relref "reference/agent/isolation#enabling-the-container-mode" >}}).
{{< /callout >}}

## Target host

Azure Container Linux is derived from Flatcar. These properties are confirmed by
Microsoft documentation: a read-only `/usr` protected by dm-verity, weekly
image-based updates, a signed unified kernel image with Secure Boot, and
enforcing SELinux. Public documentation does not cover container runtimes,
kernel modules, or mount propagation, so the requirements below are assumptions
that have not been checked on a node.

## Requirements

- The agent configuration sets `"Experimental": {"ContainerIsolation": true}`.
- Writable `/etc`, `/opt`, and `/var`. `/usr` is read-only and is not used.
- A loadable `runc`. The agent downloads it to
  `/opt/unbounded/runtime/runc` (the version and download source follow the
  configured `CRI.Runc` version and download override, and offline artifacts),
  and only in this mode. Only `runc` is supported. The host's own docker and
  containerd are not used, and the host's containerd and kubelet units are
  disabled because the container shares `/run/containerd` with the host.
- cgroup v2, and systemd 254 or newer for `DelegateSubgroup=`.
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
container root, and commands inside the container run through `runc exec`. The
applied config is recorded as `container-<slot>`, which agents that predate the
mode do not read.

Paths under the bind-mounted same-path directories (`/var/lib/kubelet`,
`/opt/cni`, `/etc/containerd/certs.d`, `/run/containerd`) are written by the
agent on the host side, because a bind mount hides anything written below it in
the rootfs. Both slots share these directories.

The container shares the host network namespace and has its own PID, mount, IPC
and cgroup namespaces. Its process is privileged, but the kernel interfaces it
has no use for are protected:

- `/sys` is read-only, and so is `/proc/sys` except `/proc/sys/net`, which CNI
  plugins and kube-proxy need, as in the `nspawn` machine.
- The kernel interfaces runtimes mask by default (`/proc/kcore`, `/proc/keys`,
  `/sys/firmware` and others) are masked, and `/proc/sysrq-trigger`, `/proc/irq`,
  `/proc/bus` and `/proc/fs` are read-only.
- udev is masked in the container's rootfs; the host's udev owns the devices.
- Each slot gets its own bpffs, mounted on the host at `/run/bpffs/<slot>` and
  seen at `/sys/fs/bpf` in the container, as each `nspawn` machine does.

Pod volume mounts made under `/var/lib/kubelet` in the container are visible on
the host: the container's root propagation is `rshared`, and before every start
the unit makes the host's `/var/lib/kubelet` a shared mount (binding the
directory on itself when nothing is mounted there). A dedicated disk mounted at
`/var/lib/kubelet` stays in place.

The container runs in the cgroup of its unit (`runc` in the unit's `supervisor`
subgroup, the container in `payload`), so stopping the unit ends the container.
The two slots' units conflict, and the agent never starts one slot while the
runtime still reports the other slot's container alive. Device and cgroup rules
come from the same GPU, device, and mount grants that `nspawn` renders as
`.nspawn` binds. When SELinux is enforcing, the process label is `spc_t`.

Lifecycle reconciliation (pre-start, post-start, reconcile) runs through a hidden
generic `node-lifecycle` command, invoked by the unit from the agent's
rollback-stable helper copy. Pre-start refuses to start while the other slot's
container is alive, then regenerates the bundle, so SELinux or device changes
are picked up on every start. The `nspawn-lifecycle` command and its config are
untouched. Hook commands supplied through the nspawn lifecycle configuration
are not used by this mode.

## Operations

| Operation | Behavior |
|---|---|
| Bootstrap | Host preparation (the first boot APT guards are removed, the distro containerd and kubelet are disabled), unpack the rootfs to a slot, generate the OCI bundle, write the unit, start the container, start the stack inside. |
| Start | Disable the other slot's unit, then enable and start this slot's unit, then the in-container units. |
| Stop | Stop the in-container units, then stop and disable the container unit, and wait for `runc` to report the container gone. A container that outlives the stop is killed; one that survives that is an error. |
| Soft reboot (`NodeReboot`) | Restart the container unit, which reruns the lifecycle hooks. The host does not reboot. |
| Repave and upgrade | Blue/green. The alternate slot is built beside the running one, then the old container is stopped, host networking is cleaned, the pod mounts below `/var/lib/kubelet` are unmounted and the previous kubelet credentials removed when a bootstrap token is used, and the new container is started and waited on. The kubelet, containerd, CNI (including `/opt/cni/bin`) and LocalDNS files both slots share on the host are saved first (in `/etc/unbounded/agent/container-upgrade-config`). If the new slot does not come up it is removed and, once its container is confirmed gone, the saved files are put back and the old container is started again; the error says whether that worked. If the new container cannot be removed the old one is left stopped and the error says the node needs attention. The applied config moves to the new slot only after it is healthy, then the old slot is removed. |
| Mode change | To and from `nspawn` in place; to and from `none` by a reprovision. See [Changing the mode]({{< relref "reference/agent/isolation#changing-the-mode" >}}). |
| Reset and delete | Runs only on a host that has container traces (the layout directory or a slot's unit). Stop the containers, remove the bundle, rootfs, unit and bpffs of each slot, then, once every slot is gone, unmount the pod mounts below `/var/lib/kubelet` and remove the shared kubelet, CNI and image directories and the host runtime, then the host-global cleanup in the usual order. A container that is still alive keeps its slot and the shared directories, and the error says so. If a pod mount below `/var/lib/kubelet` cannot be unmounted the kubelet directory is kept, so a volume mounted there is never deleted through the mount. |
| Health | The container is running, kubelet is healthy, and LocalDNS answers when enabled. |
| Exec and logs | `unbounded-agent exec` runs through `runc exec`. `unbounded-agent logs` runs `journalctl` in the container. See [Running commands and reading logs]({{< relref "reference/agent/isolation#running-commands-and-reading-logs" >}}). |
| GPU and devices | The grants become OCI mounts and devices, and the container runs with every device allowed. The `node-lifecycle` hooks wire NVIDIA. Experimental; see [NVIDIA]({{< relref "reference/gpu/nvidia" >}}) and [AMD]({{< relref "reference/gpu/amd" >}}). |
| LocalDNS | CoreDNS runs in the container. The LocalDNS network unit is the same as in the other modes; the container unit requires it, so it is restarted only when its files change. |

## Limits

- **Experimental.** Behavior can change when the mode is validated on Azure
  Container Linux.
- **Host devices.** The container's `/dev` is a private tmpfs holding the
  devices `runc` creates, `/dev/kmsg`, and the granted devices, as they were
  when the bundle was rendered. Devices that appear on the host later, and
  host devices that were not granted, are not in it. The exception is a
  device directory mounted whole, such as `/dev/infiniband` (bound
  automatically when it exists, as in
  [nspawn]({{< relref "reference/agent/nspawn" >}})) or an
  `AdditionalHostMounts` entry with `DeviceAllow`: it is a bind of the host
  directory, so nodes created in it later appear in the container, and the
  container's allow-all device policy makes them usable. CSI drivers and device
  plugins that expect the host's `/dev` (block devices, `/dev/disk/by-*`
  links, hot-plugged devices) do not work until the container restarts, and
  some not at all.
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
- **Restarts.** The supervising unit restarts the container with a growing delay
  (on systemd 254 or newer). After 10 failed starts within an hour it stays
  failed instead of restarting forever; the agent clears the failed state when
  it starts the slot again. Its start timeout covers the post-start hook's
  waits (6 minutes).
- **Provisioning.** Whether Azure Container Linux accepts user-supplied Ignition
  when AKS provisions the node is unverified.

## Troubleshooting

| Symptom | Check |
|---|---|
| Config or bootstrap refused as experimental | Set `"Experimental": {"ContainerIsolation": true}` in the agent config, or enable the mode in machina or `kubectl`; see [Enabling the container mode]({{< relref "reference/agent/isolation#enabling-the-container-mode" >}}). |
| Preflight fails on the runtime | An existing `/opt/unbounded/runtime/runc` cannot execute. Check that `/opt/unbounded` is writable and not mounted `noexec`. A missing runtime is not an error at preflight, since bootstrap installs it. |
| Unit `unbounded-node-<slot>.service` keeps restarting, or is failed | `journalctl -u unbounded-node-kube1`. The pre-start hook regenerates the bundle and the post-start hook waits for the runtime to report it running. A unit that hit its start limit stays failed until the agent starts it again, or `systemctl reset-failed`. |
| Pre-start refuses: the other slot is still running | `runc state unbounded-kube2` (or `kube1`). A container that outlived its unit holds the shared paths; stop it with `runc kill` and `runc delete --force`. |
| Preflight fails on the SELinux label | The host policy does not allow the `spc_t` label. Check `getenforce` and the audit log. |
| Container starts but kubelet does not | `sudo unbounded-agent logs --unit kubelet --lines 200`, then `sudo unbounded-agent exec -- systemctl status kubelet`. |
| Pod volumes not visible on the host | `findmnt -o TARGET,PROPAGATION /var/lib/kubelet` should report `shared`. |
| Missing kernel module | Confirm the module loads on the host. The container uses the host kernel and the agent loads the module list on the host. |
