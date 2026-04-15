---
title: "Daemon"
weight: 3
description: "Watch-based daemon that reconciles the agent to the Machine CR goal state."
---

The unbounded-agent daemon is a long-running process on the agent host that
watches the Machine custom resource on the control plane and reconciles the
local node to match the desired state. It runs as a systemd service alongside
the nspawn machine and is started automatically after the initial `start`
phase completes.

## Overview

After the agent provisions a node and joins it to a cluster, the daemon
registers a Machine CR for this node (if one does not already exist) and
establishes a Kubernetes watch on it (matched by `MachineName` in the agent
config). When an operation counter in the CR's spec diverges from the status,
the daemon performs the necessary operations to bring the node into the
desired state.

![Agent daemon overview: Control Plane with Machine CR watched by the daemon on the Agent Host, which persists applied config with SHA-256 integrity sidecar](../../../img/agent-daemon-overview.svg)

## Authentication

The daemon authenticates to the API server using the bootstrap token from the
applied agent config. This is a temporary arrangement - bootstrap tokens are
short-lived and not intended for long-running daemons. A proper agent
credential strategy (e.g. dedicated client certificates) needs to be defined
so the daemon remains authenticated after the bootstrap token expires.

The daemon builds a `rest.Config` directly from the config fields (API server
URL, base64-encoded CA certificate, bootstrap token), avoiding any dependency
on kubeconfig files inside the nspawn machine (which contain nspawn-internal
paths that do not resolve on the host filesystem).

The bootstrap token places the daemon in the `system:bootstrappers` group. The
`unbounded-bootstrapper-machine` ClusterRole (deployed with machina) grants the
daemon create, read, and update access to Machine CRs. A ClusterRoleBinding
binds this role to the `system:bootstrappers` group.

## Machine CR Registration

When the daemon starts, it checks whether a Machine CR with the configured
`MachineName` already exists. If the CR is missing (common in dynamic
environments such as manual bootstrap or cloud-init), the daemon creates a
minimal Machine CR from the applied config:

- `metadata.name` from `MachineName`
- `spec.kubernetes.bootstrapTokenRef` derived from the bootstrap token ID
- `spec.kubernetes.nodeLabels` and `spec.kubernetes.registerWithTaints`
  copied from the applied config

If the CR already exists (pre-created by machina or from a previous run), it
is left untouched. If the CRD is not installed, the daemon returns an error
(machina must be deployed first).

This self-registration happens before the watch loop begins, ensuring the
daemon always has a Machine CR to watch.

## Applied Config and Drift Detection

The daemon uses an `AgentConfig` as the local goal state for the node. This
config is persisted to disk as the applied config file
(`/etc/unbounded/agent/<machine>-applied-config.json`) after every successful
provisioning or update. The daemon reads it on startup to know what the node
is currently running.

### AgentConfig fields

The applied config contains the full set of parameters needed to provision an
nspawn machine:

| Field | Description |
|---|---|
| `MachineName` | Name of this node's Machine CR |
| `Cluster.Version` | Kubernetes version (e.g. `v1.33.1`) |
| `Cluster.CaCertBase64` | Base64-encoded cluster CA certificate |
| `Cluster.ClusterDNS` | ClusterIP of the kube-dns Service |
| `Kubelet.ApiServer` | API server endpoint URL |
| `Kubelet.BootstrapToken` | Bootstrap token for kubelet TLS bootstrapping |
| `Kubelet.Labels` | Node labels passed to kubelet registration |
| `Kubelet.RegisterWithTaints` | Taints applied at registration |
| `OCIImage` | OCI image reference for the nspawn rootfs |

When a Machine CR watch event arrives, the daemon builds a desired
`AgentConfig` by overlaying CR spec fields (version, image, labels, taints)
onto the current applied config. Fields not present in the CR (API server, CA
cert, cluster DNS, bootstrap token) are preserved from the applied config.

### Operation counter triggers

Reconciliation is triggered by operation counter drift in the Machine CR, not
by config field changes alone:

| Machine CR field | Condition | Trigger |
|---|---|---|
| `spec.operations.reimageCounter` | `> status.operations.reimageCounter` | Reimage requested |

Once triggered, the update flow compares the desired config against the
applied config to decide whether a rootfs reprovision is actually needed.

After successful reconciliation, only the reimage counter is acknowledged
(copied from spec to status). The reboot counter is not acknowledged by the
daemon.

Example steps for triggering a node update:

1. Update `spec.kubernetes.version` to the desired Kubernetes version.
2. Increment `spec.operations.reimageCounter`.
3. Wait for `status.phase` to reach `Joining` (or `Ready` once the machina
   controller observes the Node).

### Persistence and integrity

The applied config is persisted to disk after every successful provisioning
or update. A SHA-256 sidecar checksum file
(`<machine>-applied-config.json.sha256`) is written alongside it to protect
against bitflips.

On the **write path**, `PersistAppliedConfig` writes the config JSON first,
then computes the SHA-256 digest and writes the sidecar. A crash between the
two writes leaves a missing sidecar, which the read path handles gracefully.

On the **read path**, `findActiveMachine` verifies the sidecar checksum
before trusting the config data:

| Sidecar state | Behavior |
|---|---|
| Present, digest matches | Config is trusted, proceed normally |
| Missing | Log a warning and proceed - the config may have been written by an older agent that did not produce checksums, or the agent may have crashed between the two writes |
| Present, digest mismatch | Return `ErrChecksumMismatch` - indicates on-disk corruption; the daemon will not start |

On **reset**, `RemoveAppliedConfig` removes both the config JSON and the
`.sha256` sidecar file.

## Reconciliation Flow

The daemon uses a rate-limited workqueue to handle watch events. The watch
loop enqueues events and the queue deduplicates and rate-limits so at most
one reconciliation runs at a time.

When operation counter drift is detected:

1. The daemon re-reads the Machine CR from the API server (to avoid stale
   watch events from earlier status updates) and re-discovers the active
   nspawn machine.
2. It sets `status.phase` to `Provisioning` and updates the Machine CR
   status.
3. It constructs a desired `AgentConfig` by overlaying CR spec fields
   (version, image, labels, taints) onto the current applied config.
4. It executes `updateNode`, which:
   - Checks whether actual config drift exists (version, image changes)
   - Provisions a new nspawn machine (the alternate of the current one)
   - Gracefully stops services in the old machine
   - Stops the old nspawn machine
   - Starts the new machine and verifies kubelet health
   - Removes the old machine
   - Persists the new applied config
5. On success, the daemon updates Machine CR status:
   - Sets `status.phase` to `Joining` (the machina controller transitions
     to `Ready` once the Node object appears)
   - Copies the reimage counter from spec to status (acknowledging the
     operation)
   - Sets a `NodeUpdated` condition to `True` / `Succeeded`
6. On failure, the daemon sets `status.phase` to `Failed` with an error
   message and sets `NodeUpdated` condition to `False` / `Failed`.

## Systemd Unit

The daemon runs as `unbounded-agent-daemon.service`. It depends on
`network-online.target` and `machines.target` so it starts after networking
is up and systemd-nspawn machines are available.

The `start` command enables and starts this service after the initial node
provisioning is complete and the applied config is persisted.

During a `reset`, the daemon is stopped first (before any nspawn machines are
torn down) by the `StopDaemon` task, which stops and disables the service and
removes the unit file.

## RBAC

The daemon needs create, get, list, and watch access to Machine CRs, plus
get, update, and patch access to Machine status subresources. These
permissions are granted by the `unbounded-bootstrapper-machine` ClusterRole,
deployed with machina, and bound to the `system:bootstrappers` group via a
ClusterRoleBinding.
