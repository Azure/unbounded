---
title: "Bare Metal (PXE)"
weight: 3
description: "Netboot bare metal machines into your cluster."
---

## Overview

Metalman is a controller that PXE-boots bare-metal servers and joins them to your Kubernetes cluster. It bundles DHCP, TFTP, and HTTP servers into a single binary, integrates with Redfish BMCs for remote power management, and uses TPM 2.0 attestation for secure bootstrap token delivery.

API group: `unbounded-kube.io/v1alpha3`. CRD: **Machine** (`mach`), cluster-scoped.

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
kubectl apply -f deploy/metalman/
```

This creates the `machina-system` namespace, ServiceAccounts (`metalman-controller`, `metalman-bootstrap`), RBAC roles, and a Deployment.

Key `serve-pxe` flags (set via the Deployment):

| Flag | Default | Description |
|------|---------|-------------|
| `--dhcp-interface` | *(none — relay mode)* | NIC for broadcast DHCP |
| `--site` | *(none)* | Scope to machines with a specific site label |
| `--http-port` | 8880 | HTTP server port |
| `--cache-dir` | `~/.unbounded/metalman/cache` | Local cache for downloaded images |
| `--apiserver-url` | | External Kubernetes API URL |
| `--health-port` | 8081 | Health/readiness probe port |
| `--serve-url` | | External URL of this metalman instance |

When `--dhcp-interface` is set, metalman binds to the interface for broadcast DHCP, and the DHCP server requires leader election. Without it, metalman accepts relayed (unicast) DHCP packets and the DHCP server responds regardless of leader status. Leader election always runs at the manager level for the reconcilers.

## Netboot Images

Netboot images are standard OCI container images built `FROM scratch` that
contain all files needed for PXE booting a machine under `/disk/`. Files with a
`.tmpl` suffix are Go templates rendered per-machine at serve time; other files
are served verbatim. A `metadata.yaml` file provides image-level configuration
(e.g. `dhcpBootImageName`).

Images are built, tagged, and pushed using standard container tooling:

```bash
docker build -t ghcr.io/azure/images/host-ubuntu2404:v1 .
docker push ghcr.io/azure/images/host-ubuntu2404:v1
```

Template context includes `.Machine`, `.ApiserverURL`, `.ServeURL`, `.KubernetesVersion`, and `.ClusterDNS`.

See the [CRD Reference]({{< relref "/reference/machina-crd" >}}) for the full Machine spec.

## Machine CRD

A Machine represents a single bare-metal host. The `spec.pxe` section ties together the OCI image, network config, and BMC credentials:

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: server-01
  labels:
    unbounded-kube.io/site: rack-a
spec:
  pxe:
    image: ghcr.io/azure/images/host-ubuntu2404:v1
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
        namespace: machina-system
        key: bmc-01
  operations:
    rebootCounter: 0
    reimageCounter: 0
```

Store BMC passwords in a Secret referenced by `passwordRef`. See the [CRD Reference]({{< relref "/reference/machina-crd" >}}) for all fields.

## Boot Flow

1. **Machine CR created.** The Redfish reconciler sets the boot device to PXE and power-cycles the server (ForceOff → On).
2. **PXE boot.** DHCP assigns the static IP by MAC. TFTP serves `shimx64.efi`, which chainloads GRUB over HTTP.
3. **GRUB decision.** A rendered `grub.cfg` (from a `.tmpl` file in the OCI image) checks `reimageCounter` against status: if counter is ahead, boot the PXE installer; otherwise chainload the local OS.
4. **Installer (initrd overlay).** An init script in the initrd:
   - Loads storage and network drivers, configures the static IP from kernel cmdline.
   - Downloads the gzip-compressed raw disk image over HTTP (retries up to 120 times).
   - Writes the image to the largest block device via `dd`.
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

Use the `--site` flag to scope a metalman instance to machines labeled `unbounded-kube.io/site=<value>`. Each site gets its own leader-election lease.

Run separate metalman instances for different racks or network segments:

```bash
# Instance for rack-a
metalman serve-pxe --site=rack-a --dhcp-interface=eth1

# Instance for rack-b
metalman serve-pxe --site=rack-b --dhcp-interface=eth2
```

## Operations

Metalman uses counter-based operations. Increment a spec counter above the corresponding status counter to trigger an action.

**Reboot** a machine by incrementing the `rebootCounter`:

**Reimage** a machine (PXE reinstall) by incrementing both counters.

The lifecycle reconciler enforces a 30-minute timeout for reimaging and automatically retries on timeout.

Edit the Machine CR directly:

```yaml
spec:
  operations:
    rebootCounter: 1   # increment above status to reboot
    reimageCounter: 1   # increment above status to reimage
```

## Troubleshooting

{{< callout type="warning" >}}
Running metalman's DHCP server on a network segment that already has an active DHCP server will cause conflicts. Ensure metalman is the only DHCP server on the PXE segment, or use relay mode to isolate DHCP traffic.
{{< /callout >}}

**Machine stuck in reimaging.** Check metalman logs for HTTP download errors. Verify the target machine can reach metalman on TCP/8880. The lifecycle reconciler will auto-retry after the 30-minute timeout.

**DHCP not responding.** Confirm `--dhcp-interface` points to the correct NIC (broadcast mode) or that your relay agent forwards to metalman's DHCP port. Check that no other DHCP server is competing on the same segment.

**BMC connection failures.** Metalman uses TLS TOFU for Redfish — the first connection captures the BMC certificate fingerprint in `status.redfish.certFingerprint`. If a BMC certificate rotates, clear the fingerprint from the Machine status. Verify HTTPS/443 connectivity from metalman to the BMC.

**TPM attestation rejected (403).** The EK public key has changed since the initial TOFU. If the TPM was legitimately replaced, clear `status.tpm.ekPublicKey` from the Machine CR to allow re-enrollment.

**Node not joining.** Verify the target machine can reach the Kubernetes API on TCP/6443. Check that the `metalman-bootstrap` ServiceAccount and RBAC are in place. Inspect kubelet logs on the target for certificate signing request errors.

## Limitations

{{< callout type="note" >}}
Only Ubuntu 24.04 images are currently supported. The reimage timeout is fixed at 30 minutes.
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
