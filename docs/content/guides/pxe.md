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
- Network access from metalman to each BMC (HTTPS/443) and to the Kubernetes API (TCP/6443).
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
| `--default-netboot-image` | release-matched `netboot` image | Configured netboot OCI artifact used when `spec.host.netboot.netbootImage` is omitted |
| `--experimental-container-isolation` | `false` | Enable the experimental `container` isolation mode: every agent configuration metalman renders opts in. Without it, a Machine that declares `spec.isolation: container` is not replaced and gets no agent configuration, but `grub.cfg` still chainloads its local disk when no install is pending. Not set by the operator; append it with a metalman `extraArgs` [workload override]({{< relref "reference/workload-overrides" >}}). See [Enabling the container mode]({{< relref "reference/agent/isolation#enabling-the-container-mode" >}}) |

When `--dhcp-interface` is set, metalman binds to the interface for broadcast DHCP, and the DHCP server requires leader election. Without it, metalman accepts relayed (unicast) DHCP packets and the DHCP server responds regardless of leader status. Leader election always runs at the manager level for the reconcilers.

## Images

Metalman uses a machine image and a netboot image for each repave.

- `spec.host.netboot.image` is the machine image. It contains
  `/disk/disk.img.gz`, a
  gzip-compressed raw disk image written to the target disk.
- `spec.host.netboot.architecture` selects the target architecture (`amd64` or
  `arm64`) used when pulling machine and netboot image platform manifests. It
  defaults to `amd64`.
- `spec.host.netboot.netbootImage` is the reusable boot environment. It contains
  bootloaders, kernel, initrd, templates, metadata, and `unbounded-agent`. If
  omitted, Metalman uses the OCI artifact configured by
  `--default-netboot-image`.
- `spec.host.netboot.bootProtocol` selects the boot trigger:

  | Protocol | Behavior and requirements |
  |----------|---------------------------|
  | `PXE` | Default. Uses DHCP/TFTP bootfile options. |
  | `HTTP` | Uses Redfish UEFI HTTP boot with a URL derived from the netboot image metadata. Requires `redfish`; the first `dhcpLeases` entry configures the Redfish UEFI HTTP boot client. |
  | `VirtualMedia` | Mounts an operation-owned bootstrap ISO through Redfish. Requires `redfish`, and both `image` and `netbootImage` must be explicit digest references in `image@sha256:...` form. It does not use DHCP/TFTP bootfile options. |

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

See the [CRD Reference]({{< relref "/reference/machina-crd" >}}) for the full Machine spec.

## Machine CRD

A Machine represents a single bare-metal host. The
`spec.host.netboot` section ties together the machine image, optional netboot
image override, network configuration, and BMC credentials. The top-level
`spec.pxe` field remains available only for legacy Machines; new Machines
should use `spec.host.netboot`.

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
      # Optional. Defaults to PXE. Other values are HTTP and VirtualMedia.
      bootProtocol: PXE
      # Optional except for VirtualMedia. Omit to use Metalman's configured default.
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

Store BMC passwords in a Secret referenced by `passwordRef`. `passwordRef.key` names the Secret key that holds the password and defaults to `password`. See the [CRD Reference]({{< relref "/reference/machina-crd" >}}) for all fields.

## Bonds and VLANs

Set `spec.host.netboot.network` to give a host bonds, VLANs, static addresses,
routes, and DNS. Metalman uses this config both while it installs the host and
on the installed OS. It needs `bootProtocol: VirtualMedia` and a `dhcpLeases`
entry. It does not support `holdLease` on that lease.

```yaml
spec:
  host:
    netboot:
      bootProtocol: VirtualMedia
      image: ghcr.io/azure/host-ubuntu2404@sha256:...
      netbootImage: ghcr.io/azure/netboot@sha256:...
      dhcpLeases:
      - ipv4: "192.0.2.30"
        mac: "02:00:00:00:00:01"
        subnetMask: "255.255.255.0"
        gateway: "192.0.2.1"
      network:
        ethernets:
        - name: nic0
          macAddress: "02:00:00:00:00:01"
        - name: nic1
          macAddress: "02:00:00:00:00:02"
        bonds:
        - name: bond0
          interfaces: [nic0, nic1]
          mode: 802.3ad
          miiMonitorInterval: 100
          transmitHashPolicy: layer3+4
          mtu: 1500
          addresses: ["192.0.2.30/24"]
          gateway: "192.0.2.1"
        vlans:
        - name: bond0.64
          id: 64
          link: bond0
          addresses: ["198.51.100.30/24"]
          routes:
          - to: "203.0.113.0/24"
            via: "198.51.100.1"
        dns: ["192.0.2.53"]
      redfish:
        url: "https://bmc-01.example.com"
        username: admin
        passwordRef:
          name: bmc-passwords
          namespace: unbounded-system
          key: bmc-01
```

Rules:

- `ethernets` match NICs by MAC address, so names do not have to match
  kernel names.
- The `dhcpLeases[0]` entry still names the host's provisioning identity.
  Its `mac` must be a NIC under the interface that carries its `ipv4`, its
  address and mask must be set on exactly one interface as the first entry
  in its `addresses`, and its `gateway` must
  match that interface's `gateway`. Metalman checks the source IP of installer
  requests against this address.
- A VLAN `mtu` must not exceed its link's MTU. A link without `mtu` stays
  at 1500, so jumbo VLANs need `mtu` on the bond or ethernet too.
- Only IPv4 static addresses are supported. DHCP and IPv6 are not.

With `network` set, Metalman builds a local installer ISO. The ISO holds the
kernel, the initrd, and the network config. It does not fetch anything over the
network before Linux starts, so it works on links that only come up with LACP.
The installer sets up the bond and VLANs, then fetches its install
instructions from `/bootmedia/<id>/install` on Metalman. Metalman serves these
only for the active `HostReplace` operation and only to the lease address.

The installer writes the same config to the installed OS. It writes
`/etc/netplan/99-unbounded-metal.yaml` when the image uses netplan. Otherwise
it writes `.netdev` and `.network` files to `/etc/systemd/network`. The
netboot image must support local install mode, and `metadata.yaml` must set
`localInstaller: unbounded-local-installer-v1`.

Without `network`, VirtualMedia works as before: the ISO's GRUB brings up one
NIC from `dhcpLeases[0]` and loads its config over HTTP.

See `deploy/machina/examples/metalman-bond-vlan.yaml` for a full example.

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
        holdLease: true # optional: keep the site DHCP lease for this address
      cloudInit:
        userDataConfigMapRef:
          name: my-cloud-init
          namespace: unbounded-system
```

The `key` field defaults to `user-data` but can be overridden to select a different key from the ConfigMap. Both `data` and `binaryData` entries are supported.

If the referenced ConfigMap does not exist, metalman falls back to the default minimal cloud-config. If the ConfigMap exists but the referenced key is not found, metalman returns an error and the machine will not receive user-data.

Metalman allows 20 minutes from the installer reboot handoff for first-boot cloud-init to complete and the Node to register. The budget includes firmware POST, which can consume most of it on large GPU servers. Set `spec.host.netboot.cloudInitTimeout` to a positive duration to override the default for a Machine:

```yaml
spec:
  host:
    netboot:
      cloudInitTimeout: 30m
```

## Boot Flow

1. **Repave requested.** A `HostReplace` `MachineOperation` targets the Machine. Metalman sets the boot override and force-restarts the server. For `bootProtocol: PXE`, it selects PXE boot. For `bootProtocol: HTTP`, it sets a Redfish UEFI HTTP boot URL from the netboot image metadata. For `bootProtocol: VirtualMedia`, it prepares an operation-owned bootstrap ISO and mounts it through Redfish.
2. **Boot handoff.** In PXE mode, DHCP assigns the static IP by MAC, advertises the TFTP bootfile, and TFTP serves `shimx64.efi`. In HTTP mode, Metalman configures the first lease as a static Redfish EthernetInterface and the firmware downloads the Redfish-supplied URL from Metalman's HTTP server; DHCP is not required for this path. In VirtualMedia mode, the firmware boots the mounted ISO; Metalman suppresses DHCP/TFTP boot hints, although the BMC must still retrieve the staged media.
3. **GRUB decision.** A rendered `grub.cfg` checks for an active `HostReplace` `MachineOperation` targeting the Machine. If a repave is requested, GRUB boots the PXE installer; otherwise it chainloads the local OS. VirtualMedia carries install intent in its operation-owned ISO. When a Machine has multiple DHCP leases, metalman renders the lease matching the request source IP and passes that lease's MAC as `unbounded.boot_mac`.
   With `network` set, the ISO's GRUB does not use the network. It boots the installer from the ISO. The installer sets up the bond and VLANs, then gets its install instructions from `/bootmedia/<id>/install`.
4. **Installer (initrd overlay).** An init script in the initrd:
   - Loads storage and network drivers, selects the provisioning NIC by MAC, and configures the static IP and DNS from kernel cmdline.
   - Writes matching MAC-based static netplan configuration into the installed system before reboot and disables cloud-init network rendering so fallback DHCP configuration cannot conflict with it. Set `holdLease: true` on a `dhcpLeases` entry to also run a DHCPv4 client on the provisioning NIC so the site DHCP server keeps the lease for the static address and does not hand it to another host; routes, DNS, NTP, hostname, and domains from DHCP are ignored. The client identifies with the NIC MAC so reservations keyed by MAC match. Metalman answers only PXE and HTTP boot firmware for a held lease, not the installed OS, so the OS holds the lease with the site DHCP server. Reserve the address for its MAC on the site DHCP server. Leave `holdLease` unset where there is no DHCP server. The default netboot image also serves the selected lease as NoCloud `network-config`.
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

Before it returns the token, metalman stores the token's credential ID (`JTI=<jti>`) in the Machine's `status.bootstrap.credentialID` (with `issuedAt`), in the same status update that pins the EK. Attesting again replaces it. This needs Kubernetes 1.30 or later, where ServiceAccount tokens carry a `jti`. On older clusters attestation fails.

Metalman also sets the agent's node name to the Machine's node name (`spec.kubernetes.nodeRef.name`, or the Machine name if unset).

The `metalman-bootstrap` ServiceAccount token is limited:

- Kubelet client CSRs from this ServiceAccount are not auto-approved by kube-controller-manager. Metalman approves a CSR only when its `authentication.kubernetes.io/credential-id` extra matches the `status.bootstrap.credentialID` of exactly one Machine, and the CN is `system:node:<status.bootstrap.nodeName of that Machine>`, the node name recorded when the token was issued. Otherwise it denies the CSR. Each metalman instance decides only CSRs whose credential is recorded on a Machine in its own Site, and leaves other CSRs pending.
- A ValidatingAdmissionPolicy lets the ServiceAccount update only the status of its own Machine (matched by credential ID), and never `status.tpm` or `status.bootstrap`. It cannot create Machines.

Metalman identifies clients only by the TCP peer address. It ignores `X-Forwarded-For`, `X-Real-IP`, and `Forwarded` headers. Running metalman behind an HTTP proxy is not supported.

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

`machine reboot` creates a `HostHardReboot` operation. `machine repave` creates a
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

**Node not joining.** Verify the target machine can reach the Kubernetes API on TCP/6443. Check that the `metalman-bootstrap` ServiceAccount and RBAC are in place, that the Machine has `status.bootstrap.credentialID` set, and that the metalman controller for the Machine's Site is running to approve the kubelet CSR. Inspect kubelet logs on the target for certificate signing request errors.

## Limitations

{{< callout type="note" >}}
Only Ubuntu 24.04 images are currently supported. The repave boot request timeout is fixed at 30 minutes.
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
