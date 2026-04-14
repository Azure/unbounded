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
establishes a Kubernetes watch on the Machine CR that corresponds to this
node (matched by `MachineName` in the agent config). When the CR's spec
diverges from the locally persisted applied config, the daemon performs the
necessary operations to bring the node into the desired state.

```
Control Plane                         Agent Host
+-----------------+                   +-------------------+
| Machine CR      |   watch/update    | unbounded-agent   |
| spec:           | <-------------+   | daemon            |
|   kubernetes:   |                |  |                   |
|     version     | +-----------+  +--+ kubelet kubeconfig|
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

The daemon uses the kubelet's TLS-bootstrapped kubeconfig to authenticate to
the API server. After the kubelet completes TLS bootstrap, it writes a client
certificate-based kubeconfig to `/var/lib/kubelet/kubeconfig` inside the
nspawn machine. The daemon reads this file from the host-side path:

```
/var/lib/machines/<nspawn-machine>/var/lib/kubelet/kubeconfig
```

A dedicated RBAC ClusterRole (`unbounded-agent-daemon`) grants the daemon
read and update access to Machine CRs. A ClusterRoleBinding binds this role
to all authenticated nodes in the `system:nodes` group.

## Drift Detection

The daemon compares the Machine CR spec against the locally persisted applied
config (`/etc/unbounded-agent/<machine>-applied-config.json`) on each watch
event. Drift is detected when any of the following fields differ:

| Machine CR field | Applied config field | Trigger |
|---|---|---|
| `spec.kubernetes.version` | `Cluster.Version` | Kubernetes version upgrade |
| `spec.agent.image` | `OCIImage` | Agent OCI image change |
| `spec.operations.rebootCounter` | (status counter) | Reboot requested |
| `spec.operations.reimageCounter` | (status counter) | Reimage requested |

For operation counters, drift follows the same convention used by the
metalman controllers: `spec.operations.<counter> > status.operations.<counter>`
indicates a pending operation.

For spec fields (version, image), the daemon compares the CR value against
the locally applied config. Empty fields in the CR are ignored (no drift).

## Reconciliation Flow

When drift is detected:

1. The daemon sets `status.phase` to `Provisioning` and updates the Machine
   CR status.
2. It constructs a new `AgentConfig` by merging the CR spec fields onto the
   current applied config.
3. It executes the node update via `nodeupdate.Execute()`, which:
   - Provisions a new nspawn machine (the alternate of the current one)
   - Gracefully stops services in the old machine
   - Stops the old nspawn machine
   - Starts the new machine and verifies kubelet health
   - Removes the old machine
   - Persists the new applied config
4. On success, the daemon updates Machine CR status:
   - Sets `status.phase` to `Joining` (the machina controller will
     transition to `Ready` once the Node object appears)
   - Sets `status.operations` counters to match spec (acknowledging the
     operations)
   - Sets a `NodeUpdated` condition to `True`
5. On failure, the daemon sets `status.phase` to `Failed` with an error
   message and sets `NodeUpdated` condition to `False`.

## Systemd Unit

The daemon runs as `unbounded-agent-daemon.service`:

```ini
[Unit]
Description=Unbounded Agent Daemon
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/unbounded-agent daemon
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

The `start` command enables and starts this service after the initial node
provisioning is complete and the applied config is persisted.

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
    verbs: ["get", "watch", "list"]
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
    name: system:nodes
```
