---
title: "Cloud Bootstrapping (SSH)"
weight: 2
description: "Join Linux machines to your cluster using SSH."
---

This guide walks through joining remote Linux machines to your Kubernetes cluster
using the `kubectl unbounded` plugin and the machina controller. The controller
connects to each target over SSH, runs an install script, and monitors the
resulting Node.

## Prerequisites

**Target machines** must have:

- Linux (x86_64 or aarch64) with `bash`, `curl`, `tar`, and `sudo`
- A user account with passwordless sudo
- SSH server listening (port 22 by default)
- Outbound internet access to download the AKS Flex Node binary and Kubernetes binaries
- Outbound HTTPS to the Kubernetes API server

**Cluster requirements:**

- A working kubeconfig for the cluster
- The `kubectl-unbounded` plugin built and on your `PATH`
- At least one cluster node labeled `unbounded-kube.io/unbounded-net-gateway=true` for WireGuard gateway traffic
- UDP ports 51820-51899 open on gateway nodes for WireGuard

## Cluster Setup

Run `kubectl unbounded site init` to prepare the cluster and create a new site.
This single command handles:

- Validating that a gateway node exists (label `unbounded-kube.io/unbounded-net-gateway=true`)
- Installing the unbounded-net CNI plugin
- Creating site resources for both the cluster and the new site
- Creating a **bootstrap token** Secret in `kube-system` (labeled `unbounded-kube.io/site=<name>`)
- Setting up kubeadm-compatible RBAC and ConfigMaps
- Installing the **machina controller** in the `machina-system` namespace

```bash
kubectl unbounded site init \
  --name mysite \
  --cluster-node-cidr 10.240.0.0/16 \
  --cluster-pod-cidr 10.244.0.0/16 \
  --node-cidr 10.1.0.0/24 \
  --pod-cidr 10.2.0.0/24
```

All five flags above are required. Optional flags:

| Flag | Description |
|---|---|
| `--cni-manifests` | Path or HTTPS URL to CNI plugin manifests (defaults to the bundled release) |
| `--machina-manifests` | Path or HTTPS URL to machina manifests (uses embedded manifests if omitted) |
| `--kubeconfig` | Path to kubeconfig file |
| `--cluster-service-cidr` | The cluster Service CIDR (e.g. `10.0.0.0/16`); derived from the kube-dns ClusterIP if omitted |

## Creating Machines

Use `kubectl unbounded site add-machine` to register a machine with the site.
The command creates an SSH key Secret in `machina-system` and applies a Machine
CR to the cluster:

```bash
kubectl unbounded site add-machine \
  --site mysite \
  --host 10.0.0.5 \
  --ssh-username ubuntu \
  --ssh-private-key ~/.ssh/id_ed25519
```

The three flags `--site`, `--host`, and `--ssh-username` are required.
`--ssh-private-key` is also required unless bastion flags are provided.

The SSH private key is read from disk and stored as a Kubernetes Secret named
`ssh-<site>` (e.g. `ssh-mysite`) in the `machina-system` namespace. If the
Secret already exists, it is updated.

The machine name is automatically prefixed with the site name. For example,
`--name worker-01` with `--site mysite` produces a Machine named
`mysite-worker-01`. When `--name` is omitted, it is derived from the host
(e.g. host `10.0.0.5` becomes `mysite-10-0-0-5`).

Optional flags:

| Flag | Description |
|---|---|
| `--name` | Machine name (derived from `--host` if omitted, always prefixed with site name) |
| `--ssh-secret-name` | Override the SSH Secret name (defaults to `ssh-$site`) |
| `--kubeconfig` | Path to kubeconfig file |

Bastion-related flags are covered in the [Bastion Hosts](#bastion-hosts) section below.

### Example Machine manifest

The `add-machine` command produces and applies a manifest like this:

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: mysite-worker-01
spec:
  ssh:
    host: "10.0.0.5"
    username: ubuntu
    privateKeyRef:
      name: ssh-mysite
      namespace: machina-system
      key: ssh-private-key
  kubernetes:
    version: "v1.34.0"
    bootstrapTokenRef:
      name: bootstrap-token-abc123
```

The `kubernetes.version` is resolved automatically from the cluster's API
server version. The `bootstrapTokenRef` references the bootstrap token created
by `site init` for the site; the controller reads it from `kube-system`.

See the [CRD Reference]({{< relref "/reference" >}}) for the full list of fields.

## Bastion Hosts

To reach machines behind a jump host, add bastion flags. The controller dials
the bastion first, tunnels TCP to the target, then performs the SSH handshake
over the tunnel.

```bash
kubectl unbounded site add-machine \
  --site mysite \
  --host 10.0.1.50 \
  --ssh-username ubuntu \
  --ssh-private-key ~/.ssh/id_ed25519 \
  --bastion-host bastion.example.com \
  --bastion-ssh-username azureuser
```

| Flag | Description |
|---|---|
| `--bastion-host` | Host and optionally port of the bastion (e.g. `5.6.7.8` or `5.6.7.8:2222`) |
| `--bastion-ssh-username` | SSH username for the bastion (defaults to `--ssh-username`) |
| `--bastion-ssh-private-key` | Path to SSH private key for the bastion (defaults to `--ssh-private-key`) |
| `--bastion-ssh-secret-name` | Kubernetes Secret name for bastion SSH credentials (defaults to `--ssh-secret-name`) |

When `--bastion-ssh-secret-name` is omitted (or is the same as `--ssh-secret-name`),
the machine's SSH key is reused for the bastion hop. A separate bastion Secret
is only created when the bastion uses a different key file and a different secret
name.

```yaml
apiVersion: unbounded-kube.io/v1alpha3
kind: Machine
metadata:
  name: mysite-worker-behind-bastion
spec:
  ssh:
    host: "10.0.1.50"
    username: ubuntu
    privateKeyRef:
      name: ssh-mysite
      namespace: machina-system
      key: ssh-private-key
    bastion:
      host: "bastion.example.com"
      username: azureuser
  kubernetes:
    version: "v1.34.0"
    bootstrapTokenRef:
      name: bootstrap-token-abc123
```

When a separate bastion key is used, the bastion block includes its own
`privateKeyRef` pointing to the distinct Secret.

## Monitoring Progress

Machines move through phases: **Pending** &rarr; **Provisioning** &rarr; **Joining** &rarr; **Ready** (or **Failed**).

| Phase | Description |
|---|---|
| **Pending** | SSH unreachable; retries every 30 s. |
| **Provisioning** | SSH session active, install script running on target. |
| **Joining** | Script succeeded; waiting for the Node to register with label `unbounded-kube.io/machine=<name>` (polls every 30 s). |
| **Ready** | Node exists and is tracked (re-checked every 5 min; reverts to Joining if the Node disappears). |
| **Failed** | SSH or script error; retries every 60 s with no limit. |
| **Rebooting** | Machine is undergoing a reboot operation (used by the metalman bare-metal controller). |

Watch progress with:

```bash
watch 'kubectl get mach'
```

## Troubleshooting

**Machine stuck in Pending** -- Verify the target is reachable from the
controller pod on the configured SSH port. Check firewall rules and security
groups.

**Machine stuck in Failed** -- Inspect the machina controller logs for the SSH
or script error:

```bash
kubectl logs -n machina-system deploy/machina-controller
```

Common causes: missing or incorrect SSH key Secret, wrong username, target
missing `sudo` or `curl`.

**Machine stuck in Joining** -- The install script completed but the Node
hasn't registered. SSH into the target and check `journalctl -u kubelet` for
join errors. Verify the bootstrap token hasn't expired and that the machine has
HTTPS connectivity to the API server.

**Security considerations** -- SSH host key verification is currently disabled.
SSH keys are stored as Kubernetes Secrets in the `machina-system` namespace.
The install script runs as root via `sudo -E bash`. All binary downloads use
HTTPS. Supported key types: Ed25519, RSA, ECDSA.
