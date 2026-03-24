# kubectl unbounded

A kubectl plugin for joining remote machines to a Kubernetes cluster as worker
nodes. Machines are bootstrapped over SSH using kubeadm.

## Prerequisites

- A running Kubernetes cluster with the machina controller deployed
  (see `deploy/machina/`).
- The Machine CRD installed (`kubectl apply -f deploy/machina/crd/`).
- SSH access from the controller pod to the target machines (directly or
  through a bastion host).

## Quick Start

The typical workflow has two steps: **setup** the cluster, then **create
Machines**.

```bash
# 1. Prepare the cluster (RBAC, kubeadm configs, bootstrap token, SSH key)
kubectl unbounded setup

# 2. Add machines — each one will be bootstrapped as a node via SSH
kubectl unbounded create worker-01 --host 10.0.0.5
kubectl unbounded create worker-02 --host 10.0.0.6
```

After step 2 the machina controller takes over: it probes each machine over
SSH, runs the install script, and waits for the node to join the cluster.

## Commands

### `kubectl unbounded setup`

Prepares the cluster for kubeadm-based node joins. This command reads the
current kubeconfig to extract the cluster CA and API server address, discovers
the Kubernetes version, generates a bootstrap token, and produces a set of
resources that enable `kubeadm join` on remote machines.

By default, the command also generates an Ed25519 SSH key pair and stores it as
a Secret in the `machina-system` namespace. The secret is labeled for
auto-discovery by `kubectl unbounded create`. Use `--no-ssh-key` to skip SSH
key generation entirely, or `--ssh-private-key` to import an existing private
key instead of generating a new one.

Resources created:

- RBAC roles and bindings granting the bootstrap group access to `kubeadm-config`,
  `kubelet-config`, and node discovery.
- ConfigMaps `cluster-info` (kube-public), `kubeadm-config` and `kubelet-config`
  (kube-system).
- A `bootstrap.kubernetes.io/token` Secret in kube-system, labeled
   `unbounded-kube.io/default-bootstrap-token=true` for
  auto-discovery by subsequent commands.
- An SSH key Secret `machina-ssh` in `machina-system`, labeled
   `unbounded-kube.io/default-ssh-secret=true` for auto-discovery
  (unless `--no-ssh-key` is specified).

```bash
kubectl unbounded setup
kubectl unbounded setup --print-only
kubectl unbounded setup --service-subnet 10.96.0.0/12
kubectl unbounded setup --no-ssh-key
kubectl unbounded setup --ssh-private-key ~/.ssh/id_ed25519
```

Use `kubectl unbounded setup --help` for the full list of flags.

---

### `kubectl unbounded create NAME --host HOST`

Creates a Machine resource with all SSH credentials, bastion configuration,
and Kubernetes settings inlined. The machina controller then takes over:

1. **Pending** — Probes the machine over SSH until it is reachable.
2. **Provisioning** — The controller SSHs in (optionally through the bastion),
   copies the install script, and executes it with `sudo -E bash`.
3. **Joining** — Script succeeded. The controller waits for a Node with
   `unbounded-kube.io/machine=<name>` to appear.
4. **Ready** — Node exists. The machine is now a cluster member.

The command auto-discovers two secrets by well-known labels so you don't have
to specify them explicitly:

- **SSH key secret** — looked up in `machina-system` by label
   `unbounded-kube.io/default-ssh-secret=true` (created
  automatically by `kubectl unbounded setup`).
- **Bootstrap token secret** — looked up in `kube-system` by label
   `unbounded-kube.io/default-bootstrap-token=true` (created
  automatically by `kubectl unbounded setup`).

If neither secret is found, the command prints a warning and continues.

```bash
kubectl unbounded create worker-01 --host 10.0.0.5
kubectl unbounded create worker-02 --host 10.0.0.6 --port 2222
kubectl unbounded create worker-03 --host 10.0.0.7 --ssh-username ubuntu
kubectl unbounded create worker-01 --host 10.0.0.5 --print-only
```

Use `kubectl unbounded create --help` for the full list of flags.

## End-to-End Example

This walks through joining two Ubuntu VMs to an existing AKS cluster.

```bash
# --- Step 1: Prepare the cluster (generates RBAC, configs, bootstrap token, SSH key) ---
kubectl unbounded setup

# --- Step 2: Copy the generated public key to target machines ---
kubectl get secret machina-ssh -n machina-system -o jsonpath='{.data.ssh-publickey}' | base64 -d

# --- Step 3: Create machines ---
kubectl unbounded create vm-east-1 --host 10.0.1.10
kubectl unbounded create vm-west-1 --host 10.0.2.20

# --- Step 4: Watch progress ---
kubectl get machines -w
# NAME        HOST             PHASE          AGE
# vm-east-1   10.0.1.10        Provisioning   2m
# vm-west-1   10.0.2.20        Pending        1m
# vm-east-1   10.0.1.10        Ready          5m
# vm-west-1   10.0.2.20        Ready          6m
```

## `--print-only` Output Reference

Every command supports `--print-only` to dump the generated YAML to stdout
without applying it. This is useful for reviewing resources, piping into
`kubectl apply`, or storing in version control.

<details>
<summary><code>kubectl unbounded setup --print-only</code></summary>

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: kube-system
  name: kubeadm:nodes-kubeadm-config
rules:
- verbs: ["get"]
  apiGroups: [""]
  resources: ["configmaps"]
  resourceNames: ["kubeadm-config"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: kube-system
  name: kubeadm:nodes-kubeadm-config
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kubeadm:nodes-kubeadm-config
subjects:
- kind: Group
  name: system:bootstrappers:kubeadm:default-node-token
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: kube-system
  name: kubeadm:kubelet-config
rules:
- verbs: ["get"]
  apiGroups: [""]
  resources: ["configmaps"]
  resourceNames: ["kubelet-config"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: kube-system
  name: kubeadm:kubelet-config
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kubeadm:kubelet-config
subjects:
- kind: Group
  name: system:bootstrappers:kubeadm:default-node-token
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeadm:get-nodes
rules:
- verbs: ["get"]
  apiGroups: [""]
  resources: ["nodes"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeadm:get-nodes
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeadm:get-nodes
subjects:
- kind: Group
  name: system:bootstrappers:kubeadm:default-node-token
---
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: kube-public
  name: cluster-info
data:
  kubeconfig: |
    apiVersion: v1
    kind: Config
    clusters:
    - cluster:
        certificate-authority-data: LS0tLS1CRUdJTi...
        server: https://mycluster.example.com:6443
---
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: kube-system
  name: kubeadm-config
data:
  ClusterConfiguration: |
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: ClusterConfiguration
    kubernetesVersion: v1.34.0
    networking:
      serviceSubnet: 10.0.0.0/16
---
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: kube-system
  name: kubelet-config
data:
  kubelet: |
    apiVersion: kubelet.config.k8s.io/v1beta1
    kind: KubeletConfiguration
---
apiVersion: v1
kind: Secret
metadata:
  namespace: kube-system
  name: bootstrap-token-a1b2c3
  labels:
    unbounded-kube.io/default-bootstrap-token: "true"
type: bootstrap.kubernetes.io/token
stringData:
  auth-extra-groups: system:bootstrappers:kubeadm:default-node-token
  token-id: "a1b2c3"
  token-secret: "0z9y8x7w6v5u4t3s"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
```

</details>

<details>
<summary><code>kubectl unbounded create worker-01 --host 10.0.0.5 --print-only</code></summary>

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: worker-01
spec:
  ssh:
    host: "10.0.0.5:22"
    username: azureuser
    privateKeyRef:
      name: machina-ssh
      key: ssh-privatekey
  kubernetes:
    version: v1.34.0
    bootstrapTokenRef:
      name: bootstrap-token-a1b2c3
```

</details>
