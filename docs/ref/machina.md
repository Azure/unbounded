# Machina Reference

Machina is a Kubernetes controller that manages `Machine` and `MachineModel` custom resources.
Machines represent physical or virtual hosts that Machina can reach over SSH. MachineModels
define how those machines should be provisioned.

API group: `machina.unboundedkube.io/v1alpha2`

## Installation

Apply the manifests from the `deploy/machina/` directory in order:

```bash
kubectl apply -f deploy/machina/crd/
kubectl apply -f deploy/machina/
```

This creates:

- The `machina-system` namespace.
- CRDs for `Machine` and `MachineModel`.
- RBAC (ClusterRole, ClusterRoleBinding, ServiceAccount).
- A ConfigMap (`machina-config`) with controller settings.
- The controller Deployment and Service.

### Configuration

The controller is configured via the `machina-config` ConfigMap in `machina-system`. The config
is a YAML file mounted at `/etc/machina/config.yaml`.

| Setting | Type | Default | Description |
|---|---|---|---|
| `metricsAddr` | string | `:8080` | Address to bind the metrics endpoint. |
| `probeAddr` | string | `:8081` | Address to bind the health probe endpoint. |
| `enableLeaderElection` | bool | `false` | Enable leader election for high availability. |
| `maxConcurrentReconciles` | int | `10` | Maximum number of machines to reconcile in parallel. Each worker handles both reachability probes and SSH provisioning. When all workers are busy provisioning (which involves SSH sessions), reachability probes for other machines are delayed. |

## CRD Reference

### Machine

A Machine represents a host that Machina monitors for reachability and optionally provisions.
Machine is a **cluster-scoped** resource (no namespace).

```yaml
apiVersion: machina.unboundedkube.io/v1alpha2
kind: Machine
metadata:
  name: worker-01
spec:
  ssh:
    host: "10.0.0.5"
    port: 22
  modelRef:
    name: ubuntu-k8s
```

#### `spec` fields

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `ssh` | object | yes | | SSH connection details. |
| `ssh.host` | string | yes | | Hostname or IP address of the machine. |
| `ssh.port` | int32 | no | `22` | SSH port (1-65535). |
| `modelRef` | object | no | | Reference to a MachineModel (both are cluster-scoped). When set, the controller provisions the machine once it is reachable. |
| `modelRef.name` | string | yes | | Name of the MachineModel. |

#### `status` fields

| Field | Type | Description |
|---|---|---|
| `phase` | string | Current phase: `Pending`, `Ready`, `Provisioning`, `Provisioned`, `Joined`, `Orphaned`, or `Failed`. |
| `message` | string | Human-readable status message. |
| `lastProbeTime` | timestamp | Last time the machine was probed for reachability. |
| `provisionedModelGeneration` | int64 | The `metadata.generation` of the MachineModel that was last successfully provisioned. |
| `nodeRef` | object | Reference to the Node that corresponds to this Machine. Set when the machine transitions to `Joined`. |
| `nodeRef.name` | string | Name of the Node. |
| `conditions` | array | Standard Kubernetes conditions. The `Ready` condition tracks overall health. |

#### Phase lifecycle

| Phase | Meaning |
|---|---|
| `Pending` | Machine is not reachable. The controller retries every 30 seconds. |
| `Ready` | Machine is reachable but has no `modelRef`, or has not yet been provisioned. |
| `Provisioning` | SSH session is active and the install script is running. |
| `Provisioned` | Install script completed successfully. The controller polls every 30 seconds for a Node with the label `machina.project-unbounded.io/machine=<machine-name>` to appear. |
| `Joined` | A Node with the matching label exists. `status.nodeRef` is set. The controller polls every 5 minutes to ensure the Node is still present. |
| `Orphaned` | The machine was previously `Joined` but the corresponding Node has disappeared. The controller polls every 1 minute for the Node to reappear. If the Node reappears, the machine transitions back to `Joined`. |
| `Failed` | Reachability check succeeded but provisioning failed (model not found, SSH error, script error). The controller retries every 60 seconds. |

#### Node label convention

When the agent bootstraps a Node, it must apply the label
`machina.project-unbounded.io/machine=<machine-name>`. The controller watches for Nodes with
this label and uses it to transition machines through the `Provisioned` → `Joined` → `Orphaned`
lifecycle.

The label exists because there is no reliable way to cross-reference a Machine back to the
Kubernetes Node it provisioned. Nodes join the cluster using their hostname, which may bear no
relation to the address the Machine was registered with. For example, a Machine configured as
`74.147.143.85:22` may join as a Node named `worker-3.internal`. This is especially true when
the Machine is behind a NAT — the `spec.ssh.host` is a load balancer or gateway address while
the Node's internal IP is a completely different private address that the controller never sees.
Even if the Node's labels or addresses expose the internal IP, the controller cannot map it back
because it only knows the NAT address it connected through.

The label provides this cross-reference. Without it the Machine will remain in `Provisioned`
indefinitely.

---

### MachineModel

A MachineModel defines SSH credentials, an optional jumpbox, an agent install script, and
optional Kubernetes-specific settings. Multiple Machines can reference the same MachineModel.
MachineModel is a **cluster-scoped** resource (no namespace).

**Note:** Because MachineModel is cluster-scoped, the controller looks up all referenced SSH
Secrets (both `sshPrivateKeyRef` and `jumpbox.sshPrivateKeyRef`) in the `machina-system`
namespace. Secrets must be created there for the controller to find them.

```yaml
apiVersion: machina.unboundedkube.io/v1alpha2
kind: MachineModel
metadata:
  name: ubuntu-k8s
spec:
  sshUsername: stargate
  sshPrivateKeyRef:
    name: machina-ssh
    key: ssh-privatekey
  jumpbox:
    host: bastion.example.com
    port: 22
    sshUsername: jumpbox
    sshPrivateKeyRef:
      name: machina-jumpbox
      key: jumpbox-privatekey
  agentInstallScript: |
    #!/usr/bin/env bash
    set -euo pipefail
    echo "Installing agent..."
  kubernetesProfile:
    version: "1.34.0"
    bootstrapTokenRef:
      name: bootstrap-token-abc123
```

#### `spec` fields

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `sshUsername` | string | no | `azureuser` | SSH username for target machines. |
| `sshPrivateKeyRef` | object | yes | | Reference to a Secret containing the SSH private key. Must be RSA. The Secret must reside in the `machina-system` namespace. |
| `sshPrivateKeyRef.name` | string | yes | | Name of the Secret. |
| `sshPrivateKeyRef.key` | string | no | `ssh-privatekey` | Key within the Secret's `data`. |
| `jumpbox` | object | no | | Optional bastion/jump host configuration. |
| `jumpbox.host` | string | yes | | Hostname or IP of the jumpbox. |
| `jumpbox.port` | int32 | no | `22` | SSH port on the jumpbox. |
| `jumpbox.sshUsername` | string | no | `azureuser` | SSH username for the jumpbox. |
| `jumpbox.sshPrivateKeyRef` | object | no | Same as model's `sshPrivateKeyRef` | Secret reference for the jumpbox SSH key. |
| `agentInstallScript` | string | yes | | Shell script executed on the target machine during provisioning. |
| `kubernetesProfile` | object | no | | Kubernetes-specific provisioning settings. |
| `kubernetesProfile.version` | string | no | cluster version | Kubernetes version to install (e.g. `1.34.0`). When omitted the controller falls back to the cluster's Kubernetes version. |
| `kubernetesProfile.bootstrapTokenRef` | object | yes | | Reference to a `bootstrap.kubernetes.io/token` Secret in `kube-system`. The controller reads `token-id` and `token-secret` keys and joins them as `<token-id>.<token-secret>`. |
| `kubernetesProfile.bootstrapTokenRef.name` | string | yes | | Name of the bootstrap token Secret. |

#### Provisioning flow

1. The install script is copied to `/tmp/machina-agent-install.sh` on the target machine via SSH stdin.
2. The script is executed with `sudo -E bash` and environment variables exported inline.
3. The script is removed (`rm -f /tmp/machina-agent-install.sh`) regardless of success or failure.

Environment variables available to the script:

| Variable | Source |
|---|---|
| `MACHINA_MACHINE_NAME` | `metadata.name` of the Machine |
| `API_SERVER` | Cluster API server URL (resolved at controller startup) |
| `CA_CERT_BASE64` | Cluster CA certificate (base64) |
| `CLUSTER_DNS` | Cluster DNS address |
| `CLUSTER_RG` | Cluster resource group |
| `KUBE_VERSION` | Kubernetes version (from `kubernetesProfile.version` or cluster version, prefixed with `v`) |
| `BOOTSTRAP_TOKEN` | Bootstrap token (`<token-id>.<token-secret>`) when `kubernetesProfile` is set |

## Example: minimal Machine without provisioning

```yaml
apiVersion: machina.unboundedkube.io/v1alpha2
kind: Machine
metadata:
  name: probe-only
spec:
  ssh:
    host: "192.168.1.100"
```

The controller monitors reachability and transitions the Machine between `Pending` and `Ready`.

## Example: full provisioning setup

Create the SSH key Secret (must be in `machina-system`):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: machina-ssh
  namespace: machina-system
type: Opaque
data:
  ssh-privatekey: <base64-encoded RSA private key>
```

Create the MachineModel:

```yaml
apiVersion: machina.unboundedkube.io/v1alpha2
kind: MachineModel
metadata:
  name: ubuntu-k8s
spec:
  sshUsername: stargate
  sshPrivateKeyRef:
    name: machina-ssh
  agentInstallScript: |
    #!/usr/bin/env bash
    set -euo pipefail
    curl -fsSL https://example.com/install-agent.sh | bash
```

Create a Machine referencing the model:

```yaml
apiVersion: machina.unboundedkube.io/v1alpha2
kind: Machine
metadata:
  name: worker-01
spec:
  ssh:
    host: "10.0.0.5"
    port: 22
  modelRef:
    name: ubuntu-k8s
```

Once the machine is reachable over TCP on port 22, the controller provisions it automatically.
