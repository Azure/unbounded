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

Apply the CRDs and controller manifests:

```bash
kubectl apply -f deploy/machina/crd/
kubectl apply -f deploy/machina/
```

This creates the `unbounded-kube` namespace, ServiceAccounts (`metalman-controller`, `metalman-bootstrap`), RBAC roles, and a Deployment.

Key `serve-pxe` flags (set via the Deployment):

| Flag | Default | Description |
|------|---------|-------------|
| `--dhcp-interface` | *(none - relay mode)* | NIC for broadcast DHCP |
| `--site` | *(none)* | Scope to machines with a specific site label |
| `--http-port` | 8880 | HTTP server port |
| `--cache-dir` | `~/.unbounded/metalman/cache` | Local cache for downloaded images |
| `--health-port` | 8081 | Health/readiness probe port |
| `--serve-url` | | External URL of this metalman instance |
| `--default-netboot-image` | release-matched `netboot` image | PXE boot environment used when `spec.pxe.netbootImage` is omitted |

When `--dhcp-interface` is set, metalman binds to the interface for broadcast DHCP, and the DHCP server requires leader election. Without it, metalman accepts relayed (unicast) DHCP packets and the DHCP server responds regardless of leader status. Leader election always runs at the manager level for the reconcilers.

## Images

Metalman uses a machine image and a netboot image for each PXE repave.

- `spec.pxe.image` is the machine image. It contains `/disk/disk.img.gz`, a
  gzip-compressed raw disk image written to the target disk.
- `spec.pxe.architecture` selects the target architecture (`amd64` or `arm64`)
  used when pulling machine and netboot image platform manifests. It defaults
  to `amd64`.
- `spec.pxe.netbootImage` is the reusable PXE boot environment. It contains
  bootloaders, kernel, initrd, templates, metadata, and `unbounded-agent`. If
  omitted, Metalman uses the release-matched `--default-netboot-image`.
- `spec.pxe.bootProtocol` selects the network boot trigger. `PXE` is the
  default and uses DHCP/TFTP bootfile options. `HTTP` uses Redfish UEFI HTTP
  boot and requires a Redfish block.

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

A Machine represents a single bare-metal host. The `spec.pxe` section ties together the machine image, optional netboot image override, network config, and BMC credentials:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: server-01
  labels:
    unbounded-cloud.io/site: rack-a
spec:
  pxe:
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
        namespace: unbounded-kube
        key: bmc-01
```

Store BMC passwords in a Secret referenced by `passwordRef`. See the [CRD Reference]({{< relref "/reference/machina-crd" >}}) for all fields.

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
  namespace: unbounded-kube
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
  pxe:
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
        namespace: unbounded-kube
```

The `key` field defaults to `user-data` but can be overridden to select a different key from the ConfigMap. Both `data` and `binaryData` entries are supported.

If the referenced ConfigMap does not exist, metalman falls back to the default minimal cloud-config. If the ConfigMap exists but the referenced key is not found, metalman returns an error and the machine will not receive user-data.

## Boot Flow

1. **Repave requested.** A `HostReplace` `MachineOperation` targets the Machine. Metalman sets the boot override and force-restarts the server. For `bootProtocol: PXE`, it selects PXE boot. For `bootProtocol: HTTP`, it sets a Redfish UEFI HTTP boot URL from the netboot image metadata.
2. **Network boot.** In PXE mode, DHCP assigns the static IP by MAC, advertises the TFTP bootfile, and TFTP serves `shimx64.efi`. In HTTP mode, Metalman configures the first lease as a static Redfish EthernetInterface and the firmware downloads the Redfish-supplied URL from Metalman's HTTP server; DHCP is not required for this path.
3. **GRUB decision.** A rendered `grub.cfg` checks for an active `HostReplace` `MachineOperation` targeting the Machine. If a repave is requested, GRUB boots the PXE installer; otherwise it chainloads the local OS. When a Machine has multiple DHCP leases, metalman renders the lease matching the request source IP and passes that lease's MAC as `unbounded.boot_mac`.
4. **Installer (initrd overlay).** An init script in the initrd:
   - Loads storage and network drivers, selects the provisioning NIC by MAC, and configures the static IP and DNS from kernel cmdline.
   - Writes matching MAC-based static netplan configuration into the installed system before reboot and disables cloud-init network rendering so fallback DHCP configuration cannot conflict with it. The default netboot image also serves the selected lease as NoCloud `network-config`.
   - Downloads the gzip-compressed raw disk image from the machine image over HTTP (retries up to 120 times).
   - Writes the image to `spec.pxe.targetDisk` when set, otherwise to an automatically selected block device.
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
