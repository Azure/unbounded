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

```
Control Plane                         Agent Host
+-----------------+                   +-------------------+
| Machine CR      |   watch/update    | unbounded-agent   |
| spec:           | <-------------+   | daemon            |
|   kubernetes:   |                |  |                   |
|     version     | +-----------+  +--+ bootstrap token   |
|   agent:        | |           |     |                   |
|     image       | | API Server|     | applied config    |
|   operations:   | |           |     | /etc/unbounded-   |
|     reboot: 2   | +-----------+     |   agent/kube1-    |
|     reimage: 1  |                   |   applied-config  |
| status:         |                   |   .json           |
|   operations:   |                   +-------------------+
|     reboot: 2   |
|     reimage: 1  |
+-----------------+
```

## Authentication

The daemon authenticates to the API server using the bootstrap token from the
applied agent config. It builds a `rest.Config` directly from the config
fields (API server URL, base64-encoded CA certificate, bootstrap token),
avoiding any dependency on kubeconfig files inside the nspawn machine (which
contain nspawn-internal paths that do not resolve on the host filesystem).

The bootstrap token places the daemon in the `system:bootstrappers` group. A
dedicated RBAC ClusterRole (`unbounded-agent-daemon`) grants the daemon
create, read, and update access to Machine CRs. A ClusterRoleBinding binds
this role to the `system:bootstrappers` group.

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

## Drift Detection

The daemon detects drift by comparing operation counters in the Machine CR:

| Machine CR field | Condition | Trigger |
|---|---|---|
| `spec.operations.reimageCounter` | `> status.operations.reimageCounter` | Reimage requested |
| `spec.operations.rebootCounter` | `> status.operations.rebootCounter` | Reboot requested |

Only operation counter drift triggers reconciliation. Inside the update flow,
`hasDrift` (unexported) additionally checks whether the desired config
actually differs from the applied config before performing the expensive
rootfs reprovision.

After successful reconciliation, only the reimage counter is acknowledged
(copied from spec to status). The reboot counter is not acknowledged by the
daemon.

## Reconciliation Flow

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

The daemon runs as `unbounded-agent-daemon.service`:

```ini
[Unit]
Description=Unbounded Agent Daemon
After=network-online.target machines.target
Wants=network-online.target machines.target

[Service]
Type=simple
ExecStart=/usr/local/bin/unbounded-agent daemon
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

The `start` command enables and starts this service as part of the serial
task list after the initial node provisioning is complete and the applied
config is persisted. The `machines.target` dependency ensures the daemon
starts after systemd-nspawn machines are available.

## RBAC

The following cluster-scoped RBAC resources are required:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: unbounded-agent-daemon
rules:
  - apiGroups: ["unbounded-kube.io"]
    resources: ["machines"]
    verbs: ["create", "get", "list", "watch"]
  - apiGroups: ["unbounded-kube.io"]
    resources: ["machines/status"]
    verbs: ["get", "update", "patch"]

---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: unbounded-agent-daemon
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: unbounded-agent-daemon
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: system:bootstrappers
```
