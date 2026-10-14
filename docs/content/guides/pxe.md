---
title: "Bare Metal (PXE)"
weight: 3
description: "Netboot bare metal machines into your cluster."
---

## Overview

Metalman is a controller that PXE-boots bare-metal servers and joins them to your Kubernetes cluster. It bundles DHCP, TFTP, and HTTP servers into a single binary, integrates with Redfish BMCs for remote power management, and uses TPM 2.0 attestation for secure bootstrap token delivery.

API group: `unbounded-cloud.io/v1alpha3`. CRD: **Machine** (`mach`), cluster-scoped.

## Prerequisites

- A Kubernetes cluster with access to `kubectl`.
- Bare-metal servers with UEFI PXE firmware and a BMC exposing a Redfish API.
- Layer-2 network connectivity (or a DHCP relay agent) between metalman and the PXE NICs.
- Network access from metalman to each BMC's HTTPS port (443 by default) and to the Kubernetes API (TCP/6443).
- Network access from target machines to metalman (UDP/67, UDP/69, TCP/8880) and to the Kubernetes API (TCP/6443).
- TPM 2.0 modules on target machines (required for secure attestation).

## Deploy Metalman

Metalman is a per-site component managed by the unbounded operator. First install
the operator (this also installs the CRDs):

```bash
kubectl unbounded install
```

Then enable metalman on a Site by passing `--enable-metalman` to `site init`, or by
setting the metalman entry in `Site.spec.components`:

```bash
kubectl unbounded site init --name my-edge-site --enable-metalman
```

`kubectl unbounded site init` requires the operator to be installed first (the
`kubectl unbounded install` step above), and errors otherwise. The operator then
reconciles the `unbounded-system` namespace, ServiceAccounts
(`metalman-controller`, `metalman-bootstrap`), RBAC roles, and a metalman
Deployment for the Site.

Key `serve-pxe` flags (baked into the operator-managed per-site Deployment; `--site`
scoping is inherent to the per-site component):

| Flag | Default | Description |
|------|---------|-------------|
| `--dhcp-interface` | *(none - relay mode)* | NIC for broadcast DHCP |
| `--site` | *(none)* | Scope to machines with a specific site label |
| `--http-port` | 8880 | HTTP server port |
| `--cache-dir` | `~/.unbounded/metalman/cache` | Local cache for downloaded images |
| `--health-port` | 8081 | Health/readiness probe port |
| `--serve-url` | | External URL of this metalman instance |
| `--media-tftp-server` | `serverIP` | Advertised IPv4 address that the rack manager uses to fetch virtual media over TFTP (UDP/69) |
| `--default-netboot-image` | release-matched `netboot` image | PXE boot environment used when `spec.host.netboot.netbootImage` is omitted |

When `--dhcp-interface` is set, metalman binds to the interface for broadcast DHCP, and the DHCP server requires leader election. Without it, metalman accepts relayed (unicast) DHCP packets and the DHCP server responds regardless of leader status. Leader election always runs at the manager level for the reconcilers.

## Images

Metalman uses a machine image and a netboot image for each PXE repave.

- `spec.host.netboot.image` is the machine image. It contains `/disk/disk.img.gz`, a
  gzip-compressed raw disk image written to the target disk.
- `spec.host.netboot.architecture` selects the target architecture (`amd64` or `arm64`)
  used when pulling machine and netboot image platform manifests. It defaults
  to `amd64`.
- `spec.host.netboot.netbootImage` is the reusable PXE boot environment. It contains
  bootloaders, kernel, initrd, templates, metadata, and `unbounded-agent`. If
  omitted, Metalman uses the release-matched `--default-netboot-image`, except
  for `VirtualMedia`, which requires an explicit digest-pinned reference.
- `spec.host.netboot.bootProtocol` selects the network boot trigger. `PXE` is the
  default and uses DHCP/TFTP bootfile options. `HTTP` uses Redfish UEFI HTTP
  boot and requires a Redfish block. `VirtualMedia` opts into the experimental
  rack-manager ISO workflow below.

Both images are standard OCI container images built `FROM scratch` with
artifacts under `/disk/`. Files with a `.tmpl` suffix in the netboot image are
Go templates rendered per-machine at serve time; other files are served
verbatim. A `metadata.yaml` file in the netboot image provides image-level
configuration such as `dhcpBootImageName` and `httpBootPath`.

Images are built, tagged, and pushed using standard container tooling:

```bash
docker build -t ghcr.io/azure/host-ubuntu2404:v1 -f images/host-ubuntu2404/Containerfile .
docker build -t ghcr.io/azure/netboot:v1 -f images/netboot/Containerfile .
docker push ghcr.io/azure/host-ubuntu2404:v1
docker push ghcr.io/azure/netboot:v1
```

Template context includes `.Machine`, `.ApiserverURL`, `.ServeURL`, `.KubernetesVersion`, and `.ClusterDNS`.

### Machine images

| Image | Contents |
|---|---|
| `images/host-ubuntu2404` | Ubuntu 24.04 cloud image. |
| `images/host-azurelinux3` | Azure Linux 3.0 with the Azure Linux kernel, cloud-init, and systemd-networkd. Built for amd64 and arm64. |
| `images/host-azurelinux3-nvidia` | The Azure Linux 3.0 image plus the HWE kernel and the NVIDIA open GPU driver. It loads the driver and enables persistence mode at boot. Fabric Manager and IMEX are not included. |

The Azure Linux images boot with a fixed GRUB entry. A kernel package update
does not change the boot entry, so update these machines by repaving them with
a new image.

The installer in the netboot image runs the Azure Linux kernel, so it has
drivers for NICs such as Microsoft MANA. It still boots through the Ubuntu shim
and GRUB. Running this installer with Secure Boot on is not supported yet.

The installer writes the machine's static network settings as netplan when the
image has `/etc/netplan`, and as a systemd-networkd file otherwise.

See the [CRD Reference]({{< relref "/reference/machina-crd" >}}) for the full Machine spec.

## Machine CRD

A Machine represents a single bare-metal host. The `spec.host.netboot` section ties together the machine image, optional netboot image override, network config, and BMC credentials:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: server-01
  labels:
    unbounded-cloud.io/site: rack-a
spec:
  host:
    netboot:
      image: ghcr.io/azure/host-ubuntu2404:v1
      architecture: amd64
      # Optional. Defaults to PXE. Set to HTTP for Redfish UEFI HTTP boot.
      bootProtocol: PXE
      # Optional. Omit to use Metalman's default netboot image.
      netbootImage: ghcr.io/azure/netboot:v1
      # Optional. Recommended on hosts with multiple disks.
      targetDisk: /dev/disk/by-id/example-os-disk
      dhcpLeases:
      - ipv4: "10.10.0.50"
        mac: "aa:bb:cc:dd:ee:ff"
        subnetMask: "255.255.255.0"
        gateway: "10.10.0.1"
        dns: ["8.8.8.8"]
      redfish:
        url: "https://bmc-01.example.com"
        username: admin
        passwordRef:
          name: bmc-passwords
          namespace: unbounded-system
          key: bmc-01
```

Store BMC passwords in a Secret referenced by `passwordRef`. See the [CRD Reference]({{< relref "/reference/machina-crd" >}}) for all fields.

For a rack manager that exposes each server through a separate URL, include the
server's path prefix in `redfish.url`, such as `https://manager:8080/7` for slot 7.
Do not append `/redfish/v1`. Metalman detects supported endpoints automatically;
`deviceID` is ignored when the URL selects a single system. Use the rack manager's
credentials with permission to change system boot settings and power.

If the BMC does not answer and the rack reports the slot off, Metalman sends the
rack's slot power-on action. It does this only for power-on or when clearing the
boot override, because on tested firmware this action also powers on the host.
Metalman then waits up to ten minutes for the BMC without consuming operation
attempts. Power-off uses `ForceOff` and does not cut slot power. Use
`bootProtocol: PXE` or opt into `VirtualMedia` below; HTTP boot is not supported
on these endpoints. Tested firmware ignores `Once` and keeps every boot override
`Continuous`, so Metalman always restores `Hdd` after an install.

## Virtual media (experimental)

Set `spec.host.netboot.bootProtocol: VirtualMedia` to boot an operation-owned UEFI
ISO through a supported rack manager. Metalman detects the endpoint from
`redfish.url`, such as `https://manager:8080/7`; there is no public `RackSCM` field.
This mode is opt-in and does not change the default PXE or HTTP paths.

{{< callout type="warning" >}}
Partly tested on hardware: one arm64 server behind a rack manager running firmware
1.23.13. On that setup, staging, mounting, UEFI boot from the ISO, static GRUB
networking, kernel and initrd download, cleanup, and reboot to disk all worked.
A full disk install, amd64, Secure Boot, and other firmware are not yet tested.
{{< /callout >}}

### Requirements

- Deploy the updated Metalman image. ISO creation needs `xorriso`, `dosfstools`
  (`mkfs.vfat`), and `mtools` (`mmd`, `mcopy`) at runtime.
- Pin **both** `image` and an explicit `netbootImage` with `@sha256:` followed by
  64 lowercase hex digits. Build and publish the updated `images/netboot` artifact
  with `mediaFormat: unbounded-uefi-v1`, its EFI payloads, and the updated GRUB
  template and installer. Older netboot artifacts and the implicit default do not
  meet this contract.
- The first `dhcpLeases` entry must contain the boot NIC's canonical lowercase,
  colon-separated Ethernet MAC, static IPv4 address, subnet mask, and gateway in
  that subnet. Use a prefix from /1 through /30. These values configure GRUB, the
  installer, and the installed OS; they do not require control of a DHCP server.
  Virtual media does not use or advertise DHCP boot hints.
- Set `--serve-url` (`ServeURL`) to a host-reachable plain HTTP IPv4 root, such as
  `http://10.10.0.2:8880`. HTTPS, hostnames, path prefixes, credentials, queries,
  and fragments are not supported for this mode.
- The rack manager must reach Metalman's TFTP server on UDP/69. Set
  `--media-tftp-server=10.10.0.2` to advertise its reachable IPv4 address, not a URL
  or an address with a port. It defaults to the server IP when that is a usable
  IPv4 address. Otherwise set the flag explicitly. Check reachability from the
  rack, especially with an explicit `--serve-url` or multiple interfaces.
  The host must also reach Metalman's HTTP server and the Kubernetes API.
- Use an isolated management/provisioning network. TFTP is unauthenticated, and
  HTTP control requests must arrive from the first lease's IP without source NAT.

### Example

This illustrative YAML requires real values before use. Replace both digest
placeholders with the published images' 64-character SHA-256 digests; no real
digest is shown. Set the site, NIC, disk, and existing password Secret for your host.
A repave overwrites the selected disk.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: rack-slot-7
  labels:
    unbounded-cloud.io/site: rack-a
spec:
  host:
    netboot:
      bootProtocol: VirtualMedia
      architecture: amd64
      image: "ghcr.io/azure/host-ubuntu2404@sha256:<disk-image-digest>"
      netbootImage: "ghcr.io/azure/netboot@sha256:<updated-netboot-image-digest>"
      targetDisk: /dev/nvme0n1
      dhcpLeases:
      - mac: "02:11:22:33:44:55"
        ipv4: "10.10.0.50"
        subnetMask: "255.255.255.0"
        gateway: "10.10.0.1"
        dns: ["10.10.0.1"]
      redfish:
        url: "https://manager:8080/7"
        username: admin
        passwordRef:
          name: bmc-passwords
          namespace: unbounded-system
          key: rack-manager
```

Request installation with `kubectl unbounded machine repave rack-slot-7`.

### Install and cleanup

Metalman saves ownership and a cleanup finalizer on the `HostReplace` operation,
builds and stages the ISO, powers the host off, mounts the ISO, selects UEFI
`RemoteDrive`, then powers the host on. The ISO selects the NIC by MAC, sets static
networking, and fetches live install instructions and artifacts over HTTP.
The operation keeps its frozen netboot settings despite later Machine edits,
including the original first lease for downloads and callbacks.

After writing the disk, the installer calls its operation-scoped `/complete`
endpoint and waits for `/ready`. This is a reboot barrier: Metalman restores
`Hdd` and unmounts the remote drive before saving boot-safe cleanup. `/ready`
then saves the reboot acknowledgment before allowing reboot. Rack-file deletion
and local-media release are separate cleanup steps: a failed rack deletion keeps
ownership but does not block a safe reboot. Cleanup never changes power. If the
BMC does not answer, cleanup keeps retrying until both `Hdd` and unmount succeed.

If callbacks fail, the installer stays in RAM and keeps retrying with bounded
`curl` requests. It does not drop to a shell, reboot early, or write the disk
again. Late cleanup can therefore let the same installer reboot. Callbacks from
the original lease still work after operation failure or a deletion request while
the operation exists. New install work is no longer authorized in those states.

The default installation deadline is one hour, separate from the five-minute
power-action timeout and ten-minute BMC readiness grace. An operation timeout does
not end the installer's safe callback wait or release ownership by itself.

Metalman records `loaderStartedAt` when GRUB on the ISO first fetches its install
config. If that does not happen within ten minutes of power-on, Metalman assumes
the host did not boot the media. It restores `Hdd`, unmounts, deletes the staged
ISO, and builds a new one under a new name. Then it stages it, powers the host off,
mounts it, selects `RemoteDrive`, and powers on. A new name is needed because the
tested firmware can keep a broken drive for an image name, even after a remount.
It tries three boots in total, then fails
the target. `bootAttempts` counts the retries.

The finalizer also drives cleanup after failure or operation deletion, using saved
state across controller restarts. Pending cleanup or reboot handoff blocks another
host operation on the same machine or BMC, even when the old operation is terminal.
For started installs, ownership remains until successful first-boot cloud-init
confirms the host rebooted. This keeps `/ready` available if its response is lost.
If the host never booted the media, ownership ends after cleanup. If the installer
started but cloud-init never reports, ownership ends one install deadline after
cleanup or reboot handoff, and Metalman sets `abandoned`.
Restore connectivity and let cleanup finish; do not remove the finalizer to bypass it.

### Hardware results

Tested on one arm64 server behind a rack manager running firmware 1.23.13:

- `RemoteDriveMount.ImageName` finds files staged by `Tftp.Get`. Image names must
  be shorter than 40 characters, so Metalman uses `ub-<32 hex>.iso` and
  `up-<32 hex>.iso`.
- The host sees the mounted image as a removable USB disk, not a CD. Metalman
  builds a hybrid ISO with a GPT EFI system partition so UEFI can boot it.
- `Tftp.Get` and `RemoteDriveMount` block until done. A mount takes about 20
  seconds. Metalman allows three minutes for each rack manager request.
- Unmount can be repeated. Deleting a missing file returns "No such file or
  directory", which Metalman treats as already deleted.
- Unmount fails with "Rmedia Support is not enabled" when nothing is mounted,
  for example after a power cycle. Metalman treats this as unmounted.
- After a power cycle that follows a media boot, the first mount of a new name
  often leaves no usable drive. Linux saw the disk but reads failed, or the BMC
  USB devices were missing, and UEFI did not find a USB boot device. Mounting the
  same name again did not help. A second mount of another name always gave a
  working drive. So Metalman stages the ISO under two names, `up-<id>.iso` and
  `ub-<id>.iso`, mounts `up-` first and then `ub-`, and deletes both in cleanup.
- `ForceOff` works even though the system does not list it as an allowed reset.
- The rack power map shows host power, not slot AC power. The slot power-on
  action also powers on the host.
- GRUB found the NIC by MAC and used the static lease. GRUB needs HTTP range
  requests to load a gzip arm64 kernel. Metalman's HTTP server supports them.

### Hardware validation checklist

- Run a full disk install. Confirm the installer reaches `/complete` and `/ready`,
  and the installed host joins the cluster. Reboot again and confirm no reinstall.
- Boot the ISO on amd64 and with Secure Boot on.
- Test a server whose slot AC power is off, and other rack manager firmware.
- Restart Metalman during staging, installation, and cleanup. Test callback and
  BMC failures and operation deletion. Confirm the installer waits in RAM, late
  cleanup permits reboot without reinstalling, and pending cleanup blocks the next
  operation even if rack deletion fails after a safe reboot.

## Cloud-Init Customization

Cloud-init on PXE-booted machines uses two data sources that are merged at boot:

- **Vendor-data** (managed by Unbounded) -- Contains the agent configuration, bootstrap scripts, and system defaults required for the node to join the cluster. This is not user-editable.
- **User-data** (managed by the cluster operator) -- Optional customization such as SSH keys, additional packages, or host-level configuration.

When no user-data is configured, metalman serves a minimal `#cloud-config` document. To supply custom user-data, create a ConfigMap and reference it from the Machine spec:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: my-cloud-init
  namespace: unbounded-system
data:
  user-data: |
    #cloud-config
    ssh_authorized_keys:
      - ssh-rsa AAAA...
    packages:
      - vim
      - htop
```

Then reference the ConfigMap in the Machine:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: server-01
spec:
  host:
    netboot:
      image: ghcr.io/azure/host-ubuntu2404:v1
      dhcpLeases:
      - ipv4: "10.10.0.50"
        mac: "aa:bb:cc:dd:ee:ff"
        subnetMask: "255.255.255.0"
        gateway: "10.10.0.1"
        dns: ["8.8.8.8"]
      cloudInit:
        userDataConfigMapRef:
          name: my-cloud-init
          namespace: unbounded-system
```

The `key` field defaults to `user-data` but can be overridden to select a different key from the ConfigMap. Both `data` and `binaryData` entries are supported.

If the referenced ConfigMap does not exist, metalman falls back to the default minimal cloud-config. If the ConfigMap exists but the referenced key is not found, metalman returns an error and the machine will not receive user-data.

## Boot Flow

The following flow covers PXE and HTTP. Virtual media uses the install and cleanup
flow above.

1. **Repave requested.** A `HostReplace` `MachineOperation` targets the Machine. Metalman sets the boot override and force-restarts the server. For `bootProtocol: PXE`, it selects PXE boot. For `bootProtocol: HTTP`, it sets a Redfish UEFI HTTP boot URL from the netboot image metadata.
2. **Network boot.** In PXE mode, DHCP assigns the static IP by MAC, advertises the TFTP bootfile, and TFTP serves `shimx64.efi`. In HTTP mode, Metalman configures the first lease as a static Redfish EthernetInterface and the firmware downloads the Redfish-supplied URL from Metalman's HTTP server; DHCP is not required for this path.
3. **GRUB decision.** A rendered `grub.cfg` checks for an active `HostReplace` `MachineOperation` targeting the Machine. If a repave is requested, GRUB boots the PXE installer; otherwise it chainloads the local OS. When a Machine has multiple DHCP leases, metalman renders the lease matching the request source IP and passes that lease's MAC as `unbounded.boot_mac`.
4. **Installer (initrd overlay).** An init script in the initrd:
   - Loads storage and network drivers, selects the provisioning NIC by MAC, and configures the static IP and DNS from kernel cmdline.
   - Writes matching MAC-based static netplan configuration into the installed system before reboot and disables cloud-init network rendering so fallback DHCP configuration cannot conflict with it. The default netboot image also serves the selected lease as NoCloud `network-config`.
   - Downloads the gzip-compressed raw disk image from the machine image over HTTP (retries up to 120 times).
   - Writes the image to `spec.host.netboot.targetDisk` when set, otherwise to an automatically selected block device.
   - Mounts the root filesystem and injects cloud-init config and the agent configuration.
   - Calls `/pxe/disable` on metalman to signal completion, then reboots.
5. **First boot.** cloud-init downloads the `unbounded-agent` binary from metalman and runs `unbounded-agent start`.
6. **Node join.** The agent installs containerd and kubelet, performs TPM attestation to obtain a bootstrap token (see below), configures kubelet, and starts it. The node TLS-bootstraps and reaches `Ready`.

## TPM Attestation

Metalman uses TPM 2.0 to securely deliver a bootstrap token without embedding secrets in the image.

1. On first boot, the `unbounded-agent` creates a TPM Endorsement Key (EK) and Storage Root Key (SRK), then POSTs them to metalman's `/attest` endpoint.
{{< callout type="important" >}}
Metalman uses trust-on-first-use (TOFU) for TPM attestation: once a machine's EK public key is stored in `status.tpm.ekPublicKey`, any attestation from a different EK is rejected (HTTP 403). If a TPM is legitimately replaced, you must clear `status.tpm.ekPublicKey` from the Machine CR before the machine can re-enroll.
{{< /callout >}}

3. Metalman wraps an AES-256 key via `tpm2.CreateCredential` (bound to the EK and SRK), then encrypts a 1-hour ServiceAccount token with AES-256-GCM and returns both to the client.
4. The agent uses the TPM `ActivateCredential` operation to recover the AES key, decrypts the token, and writes a bootstrap kubeconfig for kubelet to use during TLS bootstrapping.

The `metalman-bootstrap` ServiceAccount has RBAC for `system:node-bootstrapper` and `certificatesigningrequests:nodeclient` auto-approval.

## Site Isolation

Use the `--site` flag to scope a metalman instance to machines labeled `unbounded-cloud.io/site=<value>`. Each site gets its own leader-election lease.

Run separate metalman instances for different racks or network segments:

```bash
# Instance for rack-a
metalman serve-pxe --site=rack-a --dhcp-interface=eth1

# Instance for rack-b
metalman serve-pxe --site=rack-b --dhcp-interface=eth2
```

## Operations

Use `kubectl unbounded` to create and watch `MachineOperation` objects for day-2 actions:

```bash
kubectl unbounded machine reboot server-01
kubectl unbounded machine repave server-01
```

`machine reboot` creates a `HostReboot` operation. `machine repave` creates a
`HostReplace` operation. Metalman records progress on the operation target and
conditions, including `BootImageWritten` and `CloudInitDone`.

## Troubleshooting

{{< callout type="warning" >}}
Running metalman's DHCP server on a network segment that already has an active DHCP server will cause conflicts. Ensure metalman is the only DHCP server on the PXE segment, or use relay mode to isolate DHCP traffic.
{{< /callout >}}

**Machine stuck in repaving.** Check the active `HostReplace` `MachineOperation` target stage and conditions. Verify the target machine can reach metalman on TCP/8880 and that the BMC is reachable from metalman. Metalman retries the Redfish boot request while the operation remains active.

**DHCP not responding.** Confirm `--dhcp-interface` points to the correct NIC (broadcast mode) or that your relay agent forwards to metalman's DHCP port. Check that no other DHCP server is competing on the same segment.

**BMC connection failures.** Metalman uses TLS TOFU for Redfish - the first connection captures the BMC certificate fingerprint in `status.redfish.certFingerprint`. If a BMC certificate rotates, clear the fingerprint from the Machine status. Verify HTTPS/443 connectivity from metalman to the BMC.

**TPM attestation rejected (403).** The EK public key has changed since the initial TOFU. If the TPM was legitimately replaced, clear `status.tpm.ekPublicKey` from the Machine CR to allow re-enrollment.

**Node not joining.** Verify the target machine can reach the Kubernetes API on TCP/6443. Check that the `metalman-bootstrap` ServiceAccount and RBAC are in place. Inspect kubelet logs on the target for certificate signing request errors.

## Limitations

{{< callout type="note" >}}
Only Ubuntu 24.04 images are currently supported. The PXE/HTTP repave boot request timeout is fixed at 30 minutes. Virtual-media deadlines are listed above.
{{< /callout >}}

## See Also

- **[Bare Metal Concepts]({{< relref "concepts/bare-metal" >}})** -- Deeper
  explanation of PXE boot, DHCP modes, TPM attestation, and pool isolation.
- **[Project Overview]({{< relref "concepts/overview" >}})** -- How metalman
  fits into the broader system.
- **[CRD Reference]({{< relref "reference/machina-crd" >}})** -- Complete
  Machine API specification.
- **[Architecture]({{< relref "reference/architecture" >}})** -- Internal
  design of the PXE provisioning pipeline.
- **[SSH Guide]({{< relref "guides/ssh" >}})** -- Alternative provisioning
  path for machines with an existing OS.
