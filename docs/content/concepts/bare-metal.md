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

PXE (Preboot Execution Environment) is a firmware feature that lets a machine
boot from the network instead of a local disk. metalman acts as the PXE
infrastructure:

```
┌──────────────┐        ┌──────────────────────────────────┐
│  Bare-Metal  │        │         metalman                 │
│   Machine    │        │  ┌──────┐ ┌──────┐ ┌──────┐     │
│              │◄─DHCP──│  │ DHCP │ │ TFTP │ │ HTTP │     │
│   PXE ROM    │──────► │  └──────┘ └──────┘ └──────┘     │
│              │◄─TFTP──│                                  │
│   bootloader │        │  Machine CR    Image CR          │
│              │◄─HTTP──│  (NIC specs)   (boot artifacts)  │
│   kernel +   │        └──────────────────────────────────┘
│   initramfs  │
│              │──────► Kubernetes API (bootstrap token)
│   kubelet    │
└──────────────┘
```

The boot flow in detail:

1. **DHCP Discovery** -- The machine's PXE firmware broadcasts a DHCP request.
   metalman responds with an IP address and the location of the bootloader.

2. **TFTP Boot** -- The firmware downloads the bootloader via TFTP.

3. **HTTP Artifacts** -- The bootloader fetches the kernel, initramfs, and
   configuration files from metalman's HTTP server. These are defined by the
   **Image** CRD.

4. **Kernel Boot** -- The machine boots into the downloaded kernel and
   initramfs.

5. **Token Retrieval** -- The init process contacts metalman's health endpoint
   to retrieve a bootstrap token. If TPM 2.0 is available, the token is
   encrypted to the machine's TPM.

6. **Cluster Join** -- kubelet uses the bootstrap token to join the Kubernetes
   cluster, just like an SSH-provisioned node.

7. **Node Ready** -- machina detects the new Node object and transitions the
   Machine to the **Ready** phase.

## Key Concepts

### DHCP Modes

metalman supports two DHCP modes depending on your network topology:

- **Interface mode** (`--dhcp-interface eth0`) -- metalman listens for
  broadcast DHCP on a specific NIC. Use this when metalman runs on the same
  L2 segment as the bare-metal machines.

- **Relay mode** (default, no `--dhcp-interface`) -- metalman listens for
  unicast DHCP forwarded by a DHCP relay agent. Use this when metalman and
  the machines are on different subnets.

### Image CRD

The `Image` custom resource defines the boot artifacts that metalman serves.
Each file in an Image can come from one of three sources:

| Source | Description |
|--------|-------------|
| **http** | Download from a URL (cached locally) |
| **template** | Rendered from a Go template with per-machine context (e.g., kernel command line) |
| **static** | Inline content embedded directly in the CR |

Images are cluster-scoped and referenced by Machine resources via
`spec.pxe.imageRef`.

### Machine CRD (PXE Fields)

For PXE-provisioned machines, the `Machine` resource includes:

- **`spec.pxe.imageRef`** -- Reference to the Image CR defining boot artifacts.
- **`spec.pxe.dhcpLeases`** -- NIC specifications: MAC address and IP
  assignment for each interface.
- **`spec.pxe.redfish`** -- Optional BMC connection details (endpoint, username,
  password secret) for remote power management.

### Pool Isolation

In environments with multiple metalman instances (e.g., different racks or
sites), the `--pool` flag scopes each instance to machines labeled with
`unbounded-kube.io/pool=<name>`. This prevents one metalman from interfering
with another's machines.

### TPM 2.0 Attestation

metalman uses TPM 2.0 for secure bootstrap token delivery:

1. **Endorsement Key (EK) TOFU** -- On first boot, the machine presents its
   TPM Endorsement Key. metalman records it using a Trust-On-First-Use model.

2. **MakeCredential / ActivateCredential** -- metalman encrypts the bootstrap
   token using the TPM's EK. Only the machine with the matching TPM can decrypt
   it via `ActivateCredential`.

3. **AES-256-GCM** -- The actual token payload is encrypted with AES-256-GCM,
   with the key wrapped by the TPM credential.

This ensures that bootstrap tokens cannot be intercepted by other machines on
the network.

### Counter-Based Operations

metalman supports two counter-based operations for day-2 management:

- **Reboot** -- Increment `spec.operations.rebootCounter` to trigger a
  reboot via Redfish.
- **Reimage** -- Increment `spec.operations.reimageCounter` to wipe and
  re-provision the machine from scratch.

The controller compares the spec counter against the status counter and acts
when they differ.

## Next Steps

- **[PXE Guide]({{< relref "guides/pxe" >}})** -- Step-by-step walkthrough
  for deploying metalman and booting your first bare-metal node.
- **[CRD Reference]({{< relref "reference/machina-crd" >}})** -- Full API
  specification for Machine and Image resources.
- **[Architecture Reference]({{< relref "reference/architecture" >}})** --
  How metalman fits into the broader system.
