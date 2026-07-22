---
title: "Bare Metal Netboot"
weight: 3
description: "Provision bare-metal machines with resilient PXE or UEFI HTTP boot."
---

## Overview

Metalman provisions bare-metal nodes from OCI images and joins them to a
Kubernetes cluster. Its controller, serving, and network-edge responsibilities
are separate so the normal control plane does not need host networking and a
server pod restart does not invalidate an in-flight provision.

## Architecture

The operator deploys these per-Site resources after Metalman is enabled:

- One leader-elected `metalman-controller-<site>` Deployment for operations,
  Redfish, OCI resolution, and immutable session creation.
- A two-replica `metalman-server-<site>` Deployment and ClusterIP Service for
  artifact serving, callbacks, and TPM attestation.
- A capability-signing Secret shared by the controller and servers.
- Edge workloads selected by `NetbootEndpoint` resources.

Only a `ManagedL2` edge uses host networking. `HTTP` edges are replicated
ordinary pods. `ExternalL2` endpoints run outside the cluster, including the
temporary administrator-machine bootstrap flow.

## Prerequisites

- An installed Unbounded operator and a Site with Metalman enabled.
- A target Machine with a UEFI-capable NIC and TPM 2.0.
- BMC connectivity from the controller when Redfish is configured.
- Target access to the endpoint and Kubernetes API.
- For TFTP: UDP 69 plus negotiated TFTP transfer ports.
- For DHCP: an edge on the target L2 or a DHCP relay. WireGuard does not carry
  Ethernet broadcasts.
- For internet-facing HTTP boot: HTTPS with a trusted certificate.

## Define an Endpoint

This endpoint places DHCP, TFTP, and HTTP on a selected provisioning node:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: NetbootEndpoint
metadata:
  name: rack-a-l2
spec:
  siteRef: rack-a
  type: ManagedL2
  externalURL: http://10.20.0.2:8880
  trust: TrustedLAN
  tls:
    mode: Disabled
  managedL2:
    interface: eno1
    address: 10.20.0.2
    nodeSelector:
      matchLabels:
        provisioning.unbounded-cloud.io/rack: a
```

Use an `HTTP` endpoint for a Service-backed HTTP edge. Set `trust: Public`, an
`https://` external URL, and TLS mode `Secret` or `External` for public access.

## Define a Machine

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
      netbootImage: ghcr.io/azure/netboot:v1
      architecture: amd64
      endpointRef: rack-a-l2
      transport: TFTP
      configurationSource: DHCP
      networkMode: DHCP
      targetDisk: /dev/disk/by-id/example-os-disk
      dhcpLeases:
      - mac: aa:bb:cc:dd:ee:ff
        ipv4: 10.20.0.50
        subnetMask: 255.255.255.0
        gateway: 10.20.0.1
        dns: [10.20.0.1]
      redfish:
        url: https://bmc-01.example.com
        username: admin
        passwordRef:
          name: bmc-passwords
          namespace: unbounded-system
          key: bmc-01
```

The supported axes are independent:

| Transport | Boot target source | Firmware network |
|-----------|--------------------|------------------|
| TFTP | DHCP | DHCP |
| HTTP | DHCP | DHCP |
| HTTP | Redfish | DHCP |
| HTTP | Redfish | Static |

For HTTP with Redfish and static networking, the first DHCP lease supplies the
Redfish EthernetInterface address, gateway, DNS, and NIC identity. HTTP with
DHCP receives its capability boot URL through DHCP instead.

## Provision

Create a HostReplace operation with the CLI:

```bash
kubectl unbounded machine replace server-01 --force
```

The controller creates an immutable `NetbootSession` for that operation target.
The session pins OCI digests and all rendered inputs before the BMC is powered
on. Firmware, installer, cloud-init, and attestation use capability-scoped URLs.
Callbacks update only the matching operation target, so stale boots and
multi-target operations cannot satisfy each other's milestones.

Static artifacts support HTTP ranges. If an edge loses its backend server pod,
it reconnects and resumes the missing byte range against another replica.

## Bootstrap the First Site Node

When no Site node exists for a managed L2 edge, run an edge temporarily on an
administrator machine attached to that LAN:

```bash
kubectl unbounded site bootstrap-netboot rack-a \
  --machine server-01 \
  --interface eno1 \
  --address 10.20.0.2
```

The command runs only the edge data plane locally. Controllers remain in the
cluster. It creates an ephemeral endpoint, uses a reconnecting port-forward to
the server Service, and automatically stops after the designated Node becomes
Ready. This first-node policy intentionally supports one designated Machine.

For routed BMC or downstream LAN access, add repeatable CIDRs:

```bash
kubectl unbounded site bootstrap-netboot rack-a \
  --machine server-01 \
  --interface eno1 \
  --address 10.20.0.2 \
  --routed-cidr 10.30.0.0/24 \
  --gateway-external-address 203.0.113.20
```

This starts the existing unbounded-net dataplane as an ephemeral external
gateway. It still provides L3 routing only; DHCP remains local to the LAN.

## Cloud-Init and Attestation

Optional cloud-init user-data comes from
`spec.host.netboot.cloudInit.userDataConfigMapRef`. Metalman snapshots it into
the session, while vendor-data installs and configures `unbounded-agent`.

The agent sends TPM EK/SRK material to the session's authenticated attestation
URL. Metalman uses TPM CreateCredential and AES-GCM to deliver a short-lived
bootstrap token. The TPM EK and Redfish certificate use trust on first use and
are pinned in Machine status.

## Troubleshooting

**Session remains Preparing.** Check endpoint `Ready`, controller/server
rollouts, registry reachability, and whether both OCI images resolve.

**DHCP does not respond.** Confirm the ManagedL2 or external edge owns the
correct interface. For remote subnets, use a DHCP relay; do not expect a
WireGuard tunnel to forward broadcasts.

**Firmware gets 401 or 404.** Verify it is using the current session capability
URL. Capabilities are session-bound and expire with the session.

**Transfer stops after pod deletion.** Check that another server replica is
Ready and the edge can reconnect. Immutable artifact requests are range
resumable, but firmware itself must retry if it loses its direct edge connection.

**Attestation is rejected.** If the TPM was intentionally replaced, clear the
pinned EK only after validating the hardware change.

See the [CRD reference]({{< relref "/reference/machina-crd" >}}) and
[CLI reference]({{< relref "/reference/cli" >}}) for all fields and flags.
