---
title: "Architecture"
weight: 1
description: "High-level architecture of Unbounded."
---

## Overview

Unbounded extends a standard Kubernetes cluster so that worker Nodes can
run in any environment -- cloud, on-premises, or edge -- and join back to a
central control plane. It adds:

- **CRD-driven lifecycle management** for remote machines (`Machine`).
- **Two provisioning paths**: SSH-based (machina) and PXE-based (metalman).
- **Cross-site networking** via WireGuard tunnels ([unbounded-net]({{< relref "concepts/networking" >}}), separate repo).

![Architecture overview: Control-Plane Cluster with machina and metalman controllers, provisioning Remote Nodes via SSH and Bare-Metal Nodes via PXE, connected through WireGuard Gateway Nodes](../../img/architecture-overview.svg)

## Components

### machina -- SSH Provisioning Controller

Binary `cmd/machina`, deployed as `machina-controller` in the `unbounded-system`
namespace. Built on controller-runtime.

**Responsibilities:**

- Watches `Machine` and `Node` resources.
- Provisions remote hosts over SSH: TCP probe, SSH connect (direct or via
  bastion), copy and execute an install script.
- Detects the corresponding Node by the label
  `unbounded-cloud.io/machine=<name>` and transitions the Machine phase to
  Ready.

**Startup resolution:** resolves API server address from
`KUBERNETES_SERVICE_HOST`, cluster CA from the `kube-root-ca.crt` ConfigMap,
DNS from `kube-dns`, and Kubernetes version from `/version`. On AKS the
annotation `kubernetes.azure.com/set-kube-service-host-fqdn: "true"` maps the
service host to a public FQDN.

**Configuration:** ConfigMap mounted at `/etc/machina/config.yaml`
(`metricsAddr`, `probeAddr`, `enableLeaderElection`, `maxConcurrentReconciles`,
`provisioningTimeout`). The Go code defaults `maxConcurrentReconciles` to 10,
but the shipped ConfigMap (rendered from `deploy/machina/03-config.yaml.tmpl`) sets it to 50.
`provisioningTimeout` defaults to 5 minutes.

### metalman -- Bare Metal Netboot

Binary `cmd/metalman`, deployed in separate per-Site roles in
`unbounded-system`:

| Role | Responsibility |
|------|----------------|
| `metalman controller` | Leader-elected MachineOperation and Redfish reconciliation, OCI resolution, and immutable NetbootSession creation. |
| `metalman server` | Replicated, Service-backed artifact serving, callbacks, TPM attestation, and authenticated edge decisions. |
| `metalman edge` | DHCP/TFTP on a provisioning LAN and HTTP proxying. It has no controller credentials. |

The operator deploys one controller, two server replicas, a server Service and
PodDisruptionBudget, and a shared capability-signing Secret. Controller and
server pods use ordinary pod networking. `NetbootEndpoint` resources determine
edge placement; only a `ManagedL2` edge uses host networking.

Each HostReplace target receives an immutable `NetbootSession` that pins the
endpoint, OCI digests, artifact allowlist, rendered inputs, and expiry. HMAC
capability URLs identify the exact session for HTTP, TFTP, callbacks, and
attestation. Static HTTP transfers support ranges so an edge can reconnect to a
different server replica and resume an interrupted download.

### kubectl-unbounded -- CLI Plugin

Binary `cmd/kubectl-unbounded`. Provides subcommands:

| Subcommand         | Purpose |
|--------------------|---------|
| `install`          | Bootstraps CRDs and `unbounded-operator`; component workloads are reconciled from `Site.spec.components`. |
| `site init`        | Initializes a new site by bootstrapping Unbounded when needed, creating site resources, and creating the bootstrap token. |
| `site bootstrap-netboot` | Runs a temporary local netboot edge until one designated first Site Node is Ready. |
| `machine register`   | Registers a machine to a site, creating a `Machine` CR with auto-discovery of SSH secrets and bootstrap tokens. |

### inventory -- Hardware Collector

Binary `cmd/inventory` (package `pkg/inventory`). Runs on target nodes and
collects chassis, BMC, CPU, memory, disk, NIC, GPU, LLDP, and NVLink data.
Results are stored in a local SQLite database.

## Custom Resources

API group `unbounded-cloud.io`, version `v1alpha3`. CRD manifests live in
`deploy/machina/crd/`. See the [CRD Reference]({{< ref "reference/machina-crd" >}}) for
full field documentation.

### Machine (cluster-scoped, short name: `mach`)

Represents a host and drives its lifecycle.

| Spec field            | Description |
|-----------------------|-------------|
| `spec.ssh`            | SSH connectivity (host, port, user, privateKeyRef) and optional bastion config. |
| `spec.host.netboot`   | Netboot image, endpoint, independent transport/configuration/network axes, DHCP leases, and Redfish settings. |
| `spec.kubernetes`     | Kubernetes version, bootstrapTokenRef, nodeRef, nodeLabels. |

Status includes phase, message, conditions, SSH fingerprint, Redfish cert
fingerprint, and TPM info. The API defines condition type constants including
`Provisioned`, `SSHReachable`, `Provisioning`, `CloudInitDone`, and
`RepavePending`. Day-2 reboot and repave progress is tracked on
`MachineOperation` objects rather than on the Machine itself.

### Netboot OCI Images

Metalman uses two OCI images for netboot repaves.
`Machine.spec.host.netboot.image` references the machine image containing
`/disk/disk.img.gz`. `Machine.spec.host.netboot.netbootImage` optionally
references the reusable boot environment; when omitted, Metalman uses its
configured default netboot image. `Machine.spec.host.netboot.architecture`
selects the OCI platform manifest for both images and defaults to `amd64`.

Netboot images contain all files needed for network booting under `/disk/`.
Files with a `.tmpl` suffix are Go templates rendered from the immutable session
snapshot. A `metadata.yaml` provides image-level configuration such as the TFTP
or HTTP firmware artifact path.

### Resource relationships

![Resource relationships: Secrets and OCI Images referenced by Machine spec fields, with bidirectional Node-Machine link via label](../../img/architecture-resource-relationships.svg)

## Network Architecture

Cross-site networking is provided by **unbounded-net**, a WireGuard-based CNI
plugin.

- **Gateway nodes** are labeled `unbounded-cloud.io/unbounded-net-gateway=true`
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

1. A `Machine` selects a `NetbootEndpoint` through `spec.host.netboot`.
2. A `HostReplace` operation creates an immutable session and waits for its
   endpoint and digest-addressed artifacts to become ready.
3. Metalman configures TFTP or UEFI HTTP boot through DHCP or Redfish, according
   to the Machine's independent boot axes, and restarts the host.
4. Firmware and the installer fetch capability-scoped artifacts through an edge.
5. The installer writes the disk image and posts the exact target's
   `BootImageWritten` callback before rebooting.
6. Cloud-init installs the agent and reports target-scoped progress.
7. TPM attestation uses TOFU Endorsement Key pinning,
   `MakeCredential`/`ActivateCredential` exchange, and an AES-256-GCM encrypted
   bootstrap token delivered through the authenticated session route.
8. kubelet TLS-bootstraps into the cluster; subsequent boots chainload the local OS.

For a walkthrough, see the [PXE Provisioning Guide]({{< ref "guides/pxe" >}}).

## Security Model

| Area | Mechanism |
|------|-----------|
| SSH keys | Ed25519, RSA, and ECDSA supported (user-provided). Stored as Secrets in `unbounded-system`. |
| SSH host verification | Currently disabled (`InsecureIgnoreHostKey`). The `status.ssh.fingerprint` field exists in the CRD but host key verification is not yet enforced. |
| Bootstrap tokens | Standard kubeadm tokens (`token-id` + `token-secret`). SSH path passes as env var; PXE path encrypts via TPM. |
| TPM attestation | TOFU EK pinning. AES-256-GCM encrypted service-account tokens with 1-hour expiry. |
| Redfish TLS | TOFU cert fingerprint pinning stored in `status.redfish.certFingerprint`. |
| Netboot requests | Expiring HMAC capabilities bound to one immutable session; public endpoints require HTTPS. |
| RBAC | Separate ServiceAccounts, Roles, and ClusterRoles per controller. |
| Secret access | Scoped API reads; capability and TLS keys mount only into roles that need them. |

## Deployment

All components deploy into the `unbounded-system` namespace. Installation is driven
by the unbounded operator: `kubectl unbounded install` bootstraps the CRDs and the
operator, and `site init` creates or updates `Site` resources (and writes the
bootstrap token). The operator then reconciles component workloads from each
`Site.spec.components`. The manifests below are plain numbered YAML files (no Helm
or Kustomize) that the operator renders and applies; they are not meant to be
applied by hand.

| Directory | Contents |
|-----------|----------|
| `deploy/machina/crd/` | `Machine` CRD definition. |
| `deploy/machina/` | Namespace, RBAC (machina + metalman), ConfigMap, Deployment, Service. |

Metalman controller and server workloads request 100m CPU and 128Mi memory and
limit themselves to 2 CPU and 2Gi memory. Server replicas use topology spread,
readiness/liveness probes, zero-unavailable rollouts, and a PodDisruptionBudget.

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
