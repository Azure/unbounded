---
title: "Project Overview"
weight: 1
description: "What Project Unbounded is, the problem it solves, and how its components work together."
---

## The Problem

Kubernetes was designed for nodes that share a network. Cloud-managed clusters
(EKS, AKS, GKE) provision all worker nodes inside a single VPC, and on-prem
clusters assume a flat LAN. This model breaks when you need compute in multiple
locations:

- A second cloud region or a different provider entirely.
- GPU compute from specialized providers like Nebius, CoreWeave, or OCI.
- On-premises hardware behind a NAT or firewall.
- Edge devices at remote sites with limited connectivity.

Kubernetes itself has no opinion on how to provision nodes outside the cluster's
network, how to route pod traffic across sites, or how to PXE-boot bare-metal
servers. These are gaps that Project Unbounded fills.

## The Solution

**Project Unbounded** extends any conformant Kubernetes control plane so that
worker nodes can run anywhere and join back to the cluster over encrypted
tunnels. It supports four provisioning paths and a unified networking layer.

```
  +-----------------------------------------------+
  |           Control-Plane Cluster               |
  |  +---------+  +----------+  +-------------+   |
  |  | machina |  | metalman |  |  Karpenter  |   |
  |  +----+----+  +----+-----+  +------+------+   |
  |       |            |               |          |
  |          Machine CRDs                          |
  +-------+------------+---------------+----------+
          |            |               |
    SSH   |    PXE     |   Cloud API   |
  (TCP/22)|  (DHCP/    |  + cloud-init |
          | TFTP/HTTP) |               |
  +-------v----+  +----v-------+  +---v-----------------+
  | Remote     |  | Bare-Metal |  | Cloud Instance      |
  | Node       |  | Node       |  | (Nebius, CoreWeave, |
  | (cloud/    |  |            |  |  OCI, Azure, AWS..) |
  | edge)      |  |            |  |                     |
  +------------+  +------------+  +---------------------+
        ^               ^                    ^
        |   WireGuard / direct L3            |
        +-------------------+----------------+
                            |
                     Gateway Nodes
                  (UDP/51820-51899)
```

## Components

### machina -- SSH Provisioning Controller

**machina** is a Kubernetes controller that provisions remote Linux machines
over SSH. Given a `Machine` custom resource with SSH connection details, it:

1. Probes the target host over TCP to confirm reachability.
2. Connects via SSH (directly or through a bastion host).
3. Copies and executes an install script that installs the unbounded-kube agent
   and joins the node to the cluster using a bootstrap token.
4. Watches for the corresponding `Node` object and transitions the Machine
   through its lifecycle phases.

machina is deployed as a `Deployment` in the `machina-system` namespace and is
installed automatically by `kubectl unbounded site init`.

See the [SSH guide]({{< relref "guides/ssh" >}}) for a hands-on walkthrough
and the [Architecture reference]({{< relref "reference/architecture" >}}) for
implementation details.

### Cloud API Provisioning -- Karpenter-based Controller

The Cloud API provisioning path uses an Unbounded implementation of Karpenter
to automatically provision instances from third-party cloud providers in
response to unschedulable pods. This enables seamless scaling across GPU and
compute providers such as **Nebius**, **CoreWeave**, **OCI**, **Azure**, **AWS**,
and others.

When pods cannot be scheduled, the Karpenter controller creates `Machine` CRs
for the target provider. Machine controllers then call the provider's API to
create instances, injecting a **cloud-init user-data** script that installs the
unbounded-kube agent on first boot. The agent contacts the API server and the
node joins the cluster automatically.

See the [Cloud API guide]({{< relref "guides/cloud-api" >}}) for the full flow.

### metalman -- Bare-Metal PXE Controller

**metalman** is a controller for PXE-booting bare-metal servers into your
cluster. It bundles four servers into a single binary:

- **DHCP** -- Assigns IP addresses and points machines to the TFTP/HTTP boot
  artifacts. Supports both broadcast (interface mode) and unicast (relay mode).
- **TFTP** -- Serves the initial PXE bootloader.
- **HTTP** -- Serves kernel, initramfs, and configuration files.
- **Health** -- Exposes readiness and liveness probes.

metalman integrates with **Redfish BMCs** for remote power management and
**TPM 2.0** for secure, attestation-based bootstrap token delivery.

See the [Bare Metal concepts]({{< relref "concepts/bare-metal" >}}) for a
deeper explanation and the [PXE guide]({{< relref "guides/pxe" >}}) for the
hands-on walkthrough.

### unbounded-net -- Multi-Site Networking

**[unbounded-net](https://github.com/project-unbounded/unbounded-net)** is a
CNI plugin and multi-site networking system. It provides transparent pod-to-pod
connectivity across sites by:

- Allocating pod CIDRs per site and per node.
- Grouping nodes into **Sites** based on their internal IPs.
- Routing cross-site traffic through **Gateway** nodes using configurable
  tunnels (WireGuard, GENEVE, VXLAN, IPIP, or direct routing).
- Running an eBPF or netlink dataplane on each node to program routes.

See the [Networking concepts]({{< relref "concepts/networking" >}}) for an
introduction and the
[Networking reference]({{< relref "reference/networking" >}}) for full
configuration and CRD details.

## Key Custom Resources

The system is driven by Kubernetes custom resources:

| CRD | API Group | Scope | Purpose |
|-----|-----------|-------|---------|
| **Machine** | `unbounded-kube.io` | Cluster | Represents a remote host to be provisioned (SSH, cloud API, or PXE) |
| **Site** | `net.unbounded-kube.io` | Cluster | Groups nodes by internal IP range; allocates pod CIDRs |
| **GatewayPool** | `net.unbounded-kube.io` | Cluster | Defines a set of gateway nodes for inter-site routing |
| **SitePeering** | `net.unbounded-kube.io` | Cluster | Enables direct node-to-node tunnels between sites |

For full API specifications, see the
[CRD Reference]({{< relref "reference/machina-crd" >}}) (Machine) and
[Networking CRDs]({{< relref "reference/networking/custom-resources" >}})
(Site/GatewayPool/SitePeering and related resources).

## What Happens When You Add a Node

The flow varies by provisioning path, but all paths share the same final steps:

**SSH path:**

1. **`kubectl unbounded site init`** prepares the cluster: installs
   unbounded-net, creates Site and GatewayPool resources, generates a bootstrap
   token, and deploys the machina controller.
2. **`kubectl unbounded site add-machine`** creates a `Machine` resource with
   SSH connection details.
3. **machina** SSHs into the host, runs the install script, and waits for the
   node to register.

**Cloud API path:**

1. An unschedulable pod is detected by the Karpenter controller.
2. A `Machine` CR is created for the target cloud provider.
3. The Machine controller calls the provider API and launches an instance with
   cloud-init user-data that installs the unbounded-kube agent.

**PXE path:**

1. A `Machine` CR with PXE configuration is created.
2. metalman responds to the server's DHCP/TFTP/HTTP boot requests and serves
   the OS image.
3. The installed OS runs the unbounded-kube agent on first boot.

**All paths converge here:**

4. **The unbounded-kube agent** on the node uses a bootstrap token to join the
   cluster and gets assigned a pod CIDR by unbounded-net.
5. **unbounded-net** establishes tunnels (or routes over an existing private
   link) between the new node's site and the gateway nodes.
6. The Machine transitions to **Ready**.

## Next Steps

- **[Getting Started]({{< relref "guides/getting-started" >}})** -- Install
  the plugin and join your first node in under 10 minutes.
- **[Cloud API Guide]({{< relref "guides/cloud-api" >}})** -- Provision GPU
  instances from third-party clouds automatically.
- **[Networking Concepts]({{< relref "concepts/networking" >}})** -- Understand
  sites, gateways, and tunnel types.
- **[Bare Metal Concepts]({{< relref "concepts/bare-metal" >}})** -- Learn how
  PXE boot and TPM attestation work.
- **[Architecture Reference]({{< relref "reference/architecture" >}})** -- Deep
  dive into component internals.
