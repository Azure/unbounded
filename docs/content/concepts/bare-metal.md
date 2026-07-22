---
title: "Bare Metal Provisioning"
weight: 3
description: "How metalman PXE-boots bare-metal servers and joins them to your cluster."
---

## When to Use Bare Metal Provisioning

Use metalman when you have physical servers that need to be:

- **Netbooted** from bare metal (no pre-installed OS).
- **Reimaged** on demand without physical access.
- **Power-managed** remotely via Redfish BMC APIs.
- **Securely bootstrapped** using TPM 2.0 hardware attestation.

If your machines already have Linux installed and are reachable via SSH, use the
[SSH provisioning path]({{< relref "guides/ssh" >}}) instead.

## How PXE Boot Works

PXE and UEFI HTTP boot let a machine start from the network instead of a local
disk. Metalman separates the Kubernetes control plane, replicated artifact
servers, and network-facing edges:

![PXE boot flow: Bare-Metal Machine boots via DHCP, TFTP, and HTTP from metalman, then joins the Kubernetes API with a bootstrap token](../../img/bare-metal-pxe-boot.svg)

The boot flow in detail:

1. **Session creation** -- A HostReplace operation creates an immutable
   NetbootSession containing the endpoint, resolved OCI digests, boot settings,
   rendered inputs, and expiry.

2. **Firmware configuration** -- DHCP or Redfish supplies a capability-scoped
   TFTP path or HTTP URL and, when selected, network configuration.

3. **Boot artifacts** -- The edge obtains the bootloader, kernel, initramfs, and
   configuration from replicated servers. Files come from the Machine's
   `spec.host.netboot.netbootImage`, or Metalman's default netboot image.

4. **Kernel Boot** -- The machine boots into the downloaded kernel and
   initramfs.

5. **Token retrieval** -- The agent uses the authenticated session attestation
   route. If TPM 2.0 is available, the token is encrypted to that machine's TPM.

6. **Cluster Join** -- kubelet uses the bootstrap token to join the Kubernetes
   cluster, just like an SSH-provisioned node.

7. **Node Ready** -- machina detects the new Node object and transitions the
   Machine to the **Ready** phase.

## Key Concepts

### Endpoints

`NetbootEndpoint` declares the stable client-facing address and edge placement:

- **ManagedL2** -- The operator places a host-network edge on a selected node
  attached to the provisioning LAN.
- **HTTP** -- The operator creates replicated HTTP edge pods and a Service.
  Public endpoints require HTTPS.
- **ExternalL2** -- An edge outside the cluster owns DHCP/TFTP/private HTTP.
  The first-node bootstrap command uses this type temporarily.

DHCP broadcasts require an edge on the target L2 or a local relay. WireGuard is
L3 only and does not extend the broadcast domain.

### OCI Images

Metalman uses two OCI images during PXE provisioning:

- The machine image, referenced by `spec.host.netboot.image`, contains `/disk/disk.img.gz`.
- The netboot image, referenced by `spec.host.netboot.netbootImage` or by Metalman's
  default, contains the reusable PXE boot environment.

Netboot images are built `FROM scratch` and contain all files needed for PXE
booting a machine under `/disk/`. Files with a `.tmpl` suffix are Go templates
rendered from immutable session data; other files are served verbatim. A
`metadata.yaml` file provides image-level configuration such as
`dhcpBootImageName`.

| Aspect | Description |
|--------|-------------|
| **Binary artifacts** | Kernel, initramfs, bootloader - served verbatim from the OCI image |
| **Templates** | Files with `.tmpl` suffix - rendered from Go templates with per-machine context (e.g., kernel command line) |
| **Configuration** | `metadata.yaml` - image-level settings such as the DHCP boot filename |

Machine and netboot image tags are resolved to digests before the host is
powered on. Server caches are disposable; the durable session records the exact
digest and allowlisted files.

### Machine CRD (PXE Fields)

For PXE-provisioned machines, the `Machine` resource includes:

- **`spec.host.netboot.image`** -- OCI machine image reference containing `/disk/disk.img.gz`
  (e.g. `"ghcr.io/azure/host-ubuntu2404:v1"`).
- **`spec.host.netboot.architecture`** -- Optional target CPU architecture for boot
  artifacts and machine images. Defaults to `amd64`; allowed values are `amd64`
  and `arm64`.
- **`spec.host.netboot.netbootImage`** -- Optional OCI netboot image reference containing
  PXE boot artifacts. When omitted, Metalman uses its configured default
  `netboot` image.
- **`spec.host.netboot.endpointRef`** -- Required NetbootEndpoint name.
- **`spec.host.netboot.transport`**, **`configurationSource`**, and
  **`networkMode`** -- Independent firmware transport, boot configuration
  source, and network configuration mode.
- **`spec.host.netboot.dhcpLeases`** -- NIC specifications: MAC address and IP
  assignment for each interface. During install, the default netboot template
  passes the matching lease MAC to the initrd so it can select the provisioning
  NIC without relying on names such as `eth0`.
- **`spec.host.netboot.targetDisk`** -- Optional block device path for the disk that
  receives the machine image. Set this on hosts with multiple disks; when
  omitted, the installer selects a disk automatically.
- **`spec.host.netboot.redfish`** -- Optional BMC connection details (endpoint, username,
  password secret) for remote power management.
- **`spec.host.netboot.cloudInit`** -- Optional cloud-init customization. References a
  ConfigMap containing user-data that is merged with the vendor-data managed by
  Unbounded.

Supported combinations are TFTP+DHCP+DHCP, HTTP+DHCP+DHCP,
HTTP+Redfish+DHCP, and HTTP+Redfish+Static. Invalid combinations are rejected by
the API.

### TPM 2.0 Attestation

metalman uses TPM 2.0 for secure bootstrap token delivery:

1. **Endorsement Key (EK) TOFU** -- On first boot, the machine presents its
   TPM Endorsement Key. metalman records it using a Trust-On-First-Use model.

2. **MakeCredential / ActivateCredential** -- metalman encrypts the bootstrap
   token using the TPM's EK. Only the machine with the matching TPM can decrypt
   it via `ActivateCredential`.

3. **AES-256-GCM** -- The actual token payload is encrypted with AES-256-GCM,
   with the key wrapped by the TPM credential.

The attestation request is also bound to an expiring session capability. Public
HTTP boot endpoints require HTTPS so artifact and callback capabilities are not
exposed in transit.

### MachineOperation-Based Operations

metalman supports day-2 host operations through `MachineOperation` resources:

- **HostReboot** -- Restarts the machine through Redfish.
- **HostReplace** -- Sets the PXE or HTTP boot override, force-restarts the
  machine, writes the selected image, and waits for first-boot cloud-init and
  the Kubernetes Node object.

Progress is recorded on the `MachineOperation` target state and conditions such
as `BootImageWritten` and `CloudInitDone`.

## Next Steps

- **[PXE Guide]({{< relref "guides/pxe" >}})** -- Step-by-step walkthrough
  for deploying metalman and booting your first bare-metal node.
- **[CRD Reference]({{< relref "reference/machina-crd" >}})** -- Full API
  specification for the Machine resource.
- **[Architecture Reference]({{< relref "reference/architecture" >}})** --
  How metalman fits into the broader system.
