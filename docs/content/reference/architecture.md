---
title: "Architecture"
weight: 1
description: "High-level architecture of Unbounded Kube."
---

## Overview

Unbounded Kube extends a standard Kubernetes cluster so that worker Nodes can
run in any environment -- cloud, on-premises, or edge -- and join back to a
central control plane. It adds:

- **CRD-driven lifecycle management** for remote machines (`Machine`, `Image`).
- **Two provisioning paths**: SSH-based (machina) and PXE-based (metalman).
- **Cross-site networking** via WireGuard tunnels (unbounded-cni, separate repo).

```
                  ┌─────────────────────────────────┐
                  │      Control-Plane Cluster      │
                  │  ┌─────────┐    ┌──────────┐    │
                  │  │ machina │    │ metalman │    │
                  │  └────┬────┘    └────┬─────┘    │
                  │       │              │          │
                  │   Machine / Image CRDs          │
                  └───────┬──────────────┬──────────┘
                          │              │
             SSH (TCP/22) │              │ PXE (DHCP/TFTP/HTTP)
                          │              │
              ┌───────────▼──┐   ┌───────▼──────────┐
              │  Remote Node │   │ Bare-Metal Node  │
              │  (cloud/edge)│   │  (on-prem/edge)  │
              └──────────────┘   └──────────────────┘
                       ▲                  ▲
                       │   WireGuard UDP  │
                       └──────┬───────────┘
                              │
                       Gateway Nodes
                    (UDP/51820-51899)
```

## Components

### machina -- SSH Provisioning Controller

Binary `cmd/machina`, deployed as `machina-controller` in the `machina-system`
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

Binary `cmd/metalman`, deployed as `metalman-controller` in `machina-system`.

Runs three reconcilers and four network servers:

| Reconciler / Server     | Role                                                        |
|-------------------------|-------------------------------------------------------------|
| ImageReconciler         | Downloads, SHA-256-verifies, and caches files from `Image` CRs. Supports qcow2-to-raw conversion. |
| Redfish Reconciler      | BMC power control and boot order via Redfish REST. TOFU TLS cert pinning. |
| Lifecycle Reconciler    | Detects 30-min reimage timeout and triggers automatic retry. |
| DHCP server (UDP/67)    | Static IP assignment by MAC address.                        |
| TFTP server (UDP/69)    | Bootloader delivery.                                        |
| HTTP server (TCP/8880)  | Kernel, initrd, Go-templated configs, `/attest` (TPM), `/pxe/disable`. |
| Health server (TCP/8081)| Liveness and readiness probes.                              |

Pool-based scoping: the `--pool` flag and the `unbounded-kube.io/pool` label
restrict each instance to a subset of Machines. Leader election is per-pool.

### kubectl-unbounded -- CLI Plugin

Binary `cmd/kubectl-unbounded`. Provides the `kubectl unbounded site` subcommands:

| Subcommand         | Purpose |
|--------------------|---------|
| `site init`        | Initializes a new site: installs CNI, machina, creates RBAC, bootstrap token, and site resources. |
| `site add-machine` | Registers a machine to a site, creating a `Machine` CR with auto-discovery of SSH secrets and bootstrap tokens. |

### inventory -- Hardware Collector

Binary `cmd/inventory` (package `pkg/inventory`). Runs on target nodes and
collects chassis, BMC, CPU, memory, disk, NIC, GPU, LLDP, and NVLink data.
Results are stored in a local SQLite database.

## Custom Resources

API group `unbounded-kube.io`, version `v1alpha3`. CRD manifests live in
`deploy/crd/`. See the [CRD Reference]({{< ref "reference/machina-crd" >}}) for
full field documentation.

### Machine (cluster-scoped, short name: `mach`)

Represents a host and drives its lifecycle.

| Spec field            | Description |
|-----------------------|-------------|
| `spec.ssh`            | SSH connectivity (host, port, user, privateKeyRef) and optional bastion config. |
| `spec.pxe`            | PXE config: imageRef, dhcpLeases, redfish settings. |
| `spec.kubernetes`     | Kubernetes version, bootstrapTokenRef, nodeRef, nodeLabels. |
| `spec.operations`     | Reboot and reimage counters. |

Status includes phase, message, conditions, SSH fingerprint, Redfish cert
fingerprint, TPM info, and operation results. The API defines four condition
type constants: `Provisioned`, `SSHReachable`, `Provisioning`, and `Reimaged`.
Additional conditions such as `PoweredOff` and `BootOrderConfigSupported` may
be set by the metalman controller but are not defined as constants in the
Machine types.

### Image (cluster-scoped, short name: `img`)

Defines PXE boot artifacts served by metalman.

| Spec field                 | Description |
|----------------------------|-------------|
| `spec.dhcpBootImageName`   | Filename returned in DHCP option 67. |
| `spec.files[]`             | List of file entries: `http` (url + sha256 + convert), `template` (Go template content), or `static` (literal content + encoding). |

### Resource relationships

```
Secret (machina-system)  ◄── Machine.spec.ssh.privateKeyRef
Secret (machina-system)  ◄── Machine.spec.pxe.redfish.passwordRef
Secret (kube-system)     ◄── Machine.spec.kubernetes.bootstrapTokenRef
Image                    ◄── Machine.spec.pxe.imageRef
Node                     ◄──► Machine   (linked by label unbounded-kube.io/machine=<name>)
```

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
- Clusters are created with `NetworkPlugin: None`; unbounded-cni replaces the
  default CNI.

```
  Remote Site                         Control-Plane Cluster
 ┌──────────────┐    WireGuard UDP   ┌──────────────────────┐
 │  Node + Pods ├───────────────────►│  Gateway Node (pub)  │
 └──────────────┘    51820-51899     │  ▲                   │
                                     │  │ vxlan / routing   │
                                     │  ▼                   │
                                     │  Cluster Pods        │
                                     └──────────────────────┘
```

## Provisioning Pipelines

### SSH Path (machina)

```
kubectl unbounded site add-machine
        │
        ▼
   Machine CR created
        │
        ▼
   machina reconciles ──► TCP probe ──► SSH connect ──► copy install script
        │                                                      │
        │                                                      ▼
        │                                               execute script
        │                                        (API_SERVER, BOOTSTRAP_TOKEN,
        │                                         CA_CERT_BASE64, KUBE_VERSION)
        │                                                      │
        │                                                      ▼
        │                                           kubelet joins cluster
        │                                                      │
        ▼                                                      │
   Node appears with label ◄───────────────────────────────────┘
   unbounded-kube.io/machine=<name>
        │
        ▼
   Machine phase → Ready
```

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
| SSH keys | Ed25519, RSA, and ECDSA supported (user-provided). Stored as Secrets in `machina-system`. |
| SSH host verification | Currently disabled (`InsecureIgnoreHostKey`). The `status.ssh.fingerprint` field exists in the CRD but host key verification is not yet enforced. |
| Bootstrap tokens | Standard kubeadm tokens (`token-id` + `token-secret`). SSH path passes as env var; PXE path encrypts via TPM. |
| TPM attestation | TOFU EK pinning. AES-256-GCM encrypted service-account tokens with 1-hour expiry. |
| Redfish TLS | TOFU cert fingerprint pinning stored in `status.redfish.certFingerprint`. |
| RBAC | Separate ServiceAccounts, Roles, and ClusterRoles per controller. |
| Secret access | Via Kubernetes API only; never mounted as volumes. |

## Deployment

All components deploy into the `machina-system` namespace. Manifests are plain
numbered YAML files (no Helm or Kustomize).

| Directory | Contents |
|-----------|----------|
| `deploy/crd/` | `Machine` and `Image` CRD definitions. |
| `deploy/machina/` | Namespace, RBAC, ConfigMap, Deployment, Service. |
| `deploy/metalman/` | RBAC, Deployment. |

Resource defaults for both controllers: 100m CPU / 128Mi memory requests,
500m CPU / 256Mi memory limits. Both tolerate `CriticalAddonsOnly`.

Container images are multi-stage builds on Azure Linux 3.0, built with
`podman`. CRDs are generated with `controller-gen` v0.20.1.

**Build toolchain:** Go 1.25.7, controller-runtime v0.23.3.
