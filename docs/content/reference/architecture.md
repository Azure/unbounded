---
title: "Architecture"
weight: 1
description: "High-level architecture of Unbounded Kube."
---

## Overview

Unbounded Kube extends a standard Kubernetes cluster so that worker Nodes can
run in any environment -- cloud, on-premises, or edge -- and join back to a
central control plane. It adds:

- **CRD-driven lifecycle management** for remote machines (`Machine`).
- **Two provisioning paths**: SSH-based (machina) and PXE-based (metalman).
- **Cross-site networking** via WireGuard tunnels ([unbounded-net]({{< relref "concepts/networking" >}}), separate repo).

![Architecture overview: Control-Plane Cluster with machina and metalman controllers, provisioning Remote Nodes via SSH and Bare-Metal Nodes via PXE, connected through WireGuard Gateway Nodes](../../img/architecture-overview.svg)

## Components

### machina -- SSH Provisioning Controller

Binary `cmd/machina`, deployed as `machina-controller` in the `unbounded-kube`
namespace. Built on controller-runtime.

**Responsibilities:**

- Watches `Machine` and `Node` resources.
- Provisions remote hosts over SSH: TCP probe, SSH connect (direct or via
  bastion), copy and execute an install script.
- Detects the corresponding Node by the label
  `unbounded-kube.io/machine=<name>` and transitions the Machine phase to
  Ready.

**Startup resolution:** resolves API server address from
`KUBERNETES_SERVICE_HOST`, cluster CA from the `kube-root-ca.crt` ConfigMap,
DNS from `kube-dns`, and Kubernetes version from `/version`. On AKS the
annotation `kubernetes.azure.com/set-kube-service-host-fqdn: "true"` maps the
service host to a public FQDN.

**Configuration:** ConfigMap mounted at `/etc/machina/config.yaml`
(`metricsAddr`, `probeAddr`, `enableLeaderElection`, `maxConcurrentReconciles`,
`provisioningTimeout`). The Go code defaults `maxConcurrentReconciles` to 10,
but the shipped ConfigMap (`deploy/machina/03-config.yaml`) sets it to 50.
`provisioningTimeout` defaults to 5 minutes.

### metalman -- Bare Metal PXE Controller

Binary `cmd/metalman`, deployed as `metalman-controller` in `unbounded-kube`.

Runs three reconcilers and four network servers:

| Reconciler / Server     | Role                                                        |
|-------------------------|-------------------------------------------------------------|
| OCIReconciler           | Pulls and caches OCI netboot images from container registries. |
| Redfish Reconciler      | BMC power control and boot order via Redfish REST. TOFU TLS cert pinning. |
| Lifecycle Reconciler    | Detects 30-min reimage timeout and triggers automatic retry. |
| DHCP server (UDP/67)    | Static IP assignment by MAC address.                        |
| TFTP server (UDP/69)    | Bootloader delivery.                                        |
| HTTP server (TCP/8880)  | Kernel, initrd, Go-templated configs, `/attest` (TPM), `/pxe/disable`. |
| Health server (TCP/8081)| Liveness and readiness probes.                              |

Site-based scoping: the `--site` flag and the `unbounded-kube.io/site` label
restrict each instance to a subset of Machines. Leader election is per-site.

### kubectl-unbounded -- CLI Plugin

Binary `cmd/kubectl-unbounded`. Provides subcommands:

| Subcommand         | Purpose |
|--------------------|---------|
| `site init`        | Initializes a new site: installs CNI, machina, creates RBAC, bootstrap token, and site resources. |
| `machine register`   | Registers a machine to a site, creating a `Machine` CR with auto-discovery of SSH secrets and bootstrap tokens. |

### inventory -- Hardware Collector

Binary `cmd/inventory` (package `pkg/inventory`). Runs on target nodes and
collects chassis, BMC, CPU, memory, disk, NIC, GPU, LLDP, and NVLink data.
Results are stored in a local SQLite database.

## Custom Resources

API group `unbounded-kube.io`, version `v1alpha3`. CRD manifests live in
`deploy/machina/crd/`. See the [CRD Reference]({{< ref "reference/machina-crd" >}}) for
full field documentation.

### Machine (cluster-scoped, short name: `mach`)

Represents a host and drives its lifecycle.

| Spec field            | Description |
|-----------------------|-------------|
| `spec.ssh`            | SSH connectivity (host, port, user, privateKeyRef) and optional bastion config. |
| `spec.pxe`            | PXE config: OCI image reference, dhcpLeases, redfish settings. |
| `spec.kubernetes`     | Kubernetes version, bootstrapTokenRef, nodeRef, nodeLabels. |
| `spec.operations`     | Reboot and reimage counters. |

Status includes phase, message, conditions, SSH fingerprint, Redfish cert
fingerprint, TPM info, and operation results. The API defines four condition
type constants: `Provisioned`, `SSHReachable`, `Provisioning`, and `Reimaged`.
Additional conditions such as `PoweredOff` and `BootOrderConfigSupported` may
be set by the metalman controller but are not defined as constants in the
Machine types.

### Netboot OCI Images

Netboot images are standard OCI container images referenced by
`Machine.spec.pxe.image`. They contain all files needed for PXE booting under
`/disk/`. Files with a `.tmpl` suffix are Go templates rendered per-machine at
serve time. A `metadata.yaml` provides image-level configuration (e.g.
`dhcpBootImageName`).

### Resource relationships

![Resource relationships: Secrets and OCI Images referenced by Machine spec fields, with bidirectional Node-Machine link via label](../../img/architecture-resource-relationships.svg)

## Network Architecture

Cross-site networking is provided by **unbounded-cni** (separate repository), a
WireGuard-based CNI plugin.

- **Gateway nodes** are labeled `unbounded-kube.io/unbounded-net-gateway=true`
  and expose public IPs with UDP ports 51820-51899.
- Remote nodes establish WireGuard tunnels directly to gateway public IPs (no
  STUN/TURN).
- Pods and Services are routable across sites.
- CRDs: `GatewayPool`, `Site` (nodeCidrs, podCidrAssignments),
  `SiteGatewayPoolAssignment`.
- Clusters without an existing CNI are created with `NetworkPlugin: None` and
  unbounded-net serves as the CNI. Clusters with an existing CNI (e.g. Cilium,
  Calico) set `manageCniPlugin: false` on the Site resource.

![Network architecture: Remote Site node connects via WireGuard UDP tunnel to Gateway Node in Control-Plane Cluster, which routes to Cluster Pods via vxlan](../../img/architecture-network.svg)

## Provisioning Pipelines

### SSH Path (machina)

![SSH provisioning pipeline: kubectl machine register creates Machine CR, machina reconciles with TCP probe, SSH connect, script execution, kubelet joins, Node appears, Machine becomes Ready](../../img/architecture-ssh-provisioning.svg)

Requeue intervals: Pending 30s, Failed 60s, Joining 30s, Ready 5m.

Two install scripts exist in `internal/provision/assets/`:

- `aks-flex-node-install.sh` (AKS Flex Node path): runs `ConfigureBaseOS`,
  containerd install, kubelet/kubeadm install, and `kubeadm join`.
- `unbounded-agent-install.sh`: installs and runs the unbounded agent.

For a walkthrough, see the [SSH Provisioning Guide]({{< ref "guides/ssh" >}}).

### PXE Path (metalman)

1. `Machine` CR created with `spec.pxe`.
2. Redfish reconciler sets boot device to PXE and power-cycles the host.
3. Host PXE-boots: DHCP (IP + boot filename) -> TFTP (bootloader) -> HTTP
   (kernel, initrd, configs).
4. Init script: writes disk image, injects configs, calls `/pxe/disable`,
   reboots.
5. Cloud-init: installs containerd, kubelet, tpm2-tools.
6. TPM attestation: TOFU Endorsement Key pinning,
   `MakeCredential`/`ActivateCredential` exchange, AES-256-GCM encrypted
   bootstrap token delivered via `/attest`.
7. kubelet TLS-bootstraps into the cluster.
8. Subsequent reboots: GRUB chainloads the local OS (no PXE).

For a walkthrough, see the [PXE Provisioning Guide]({{< ref "guides/pxe" >}}).

## Security Model

| Area | Mechanism |
|------|-----------|
| SSH keys | Ed25519, RSA, and ECDSA supported (user-provided). Stored as Secrets in `unbounded-kube`. |
| SSH host verification | Currently disabled (`InsecureIgnoreHostKey`). The `status.ssh.fingerprint` field exists in the CRD but host key verification is not yet enforced. |
| Bootstrap tokens | Standard kubeadm tokens (`token-id` + `token-secret`). SSH path passes as env var; PXE path encrypts via TPM. |
| TPM attestation | TOFU EK pinning. AES-256-GCM encrypted service-account tokens with 1-hour expiry. |
| Redfish TLS | TOFU cert fingerprint pinning stored in `status.redfish.certFingerprint`. |
| RBAC | Separate ServiceAccounts, Roles, and ClusterRoles per controller. |
| Secret access | Via Kubernetes API only; never mounted as volumes. |

## Deployment

All components deploy into the `unbounded-kube` namespace. Manifests are plain
numbered YAML files (no Helm or Kustomize).

| Directory | Contents |
|-----------|----------|
| `deploy/machina/crd/` | `Machine` CRD definition. |
| `deploy/machina/` | Namespace, RBAC (machina + metalman), ConfigMap, Deployment, Service. |

Resource defaults for both controllers: 100m CPU / 128Mi memory requests,
500m CPU / 256Mi memory limits. Both tolerate `CriticalAddonsOnly`.

Container images are multi-stage builds on Azure Linux 3.0, built with
`podman`. CRDs are generated with `controller-gen` v0.20.1.

**Build toolchain:** Go 1.25.7, controller-runtime v0.23.3.

## See Also

- **[Project Overview]({{< relref "concepts/overview" >}})** -- Conceptual
  introduction to the system components.
- **[Networking Concepts]({{< relref "concepts/networking" >}})** -- How
  unbounded-net provides cross-site pod connectivity.
- **[Networking Reference]({{< relref "reference/networking" >}})** -- Full
  unbounded-net CRDs, configuration, routing flows, and operations.
- **[Bare Metal Concepts]({{< relref "concepts/bare-metal" >}})** -- PXE boot,
  TPM attestation, and metalman internals.
- **[CLI Reference]({{< relref "reference/cli" >}})** -- `kubectl unbounded`
  command and flag reference.
- **[CRD Reference]({{< relref "reference/machina-crd" >}})** -- Machine
  API specification.
