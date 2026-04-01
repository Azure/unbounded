---
title: "Getting Started"
weight: 1
description: "Install Unbounded Kube and join your first remote node."
---

## Prerequisites

1. A Kubernetes cluster that you have access to via a `kubeconfig` file.
   Developers are encouraged to use the [Forge](https://github.com/project-unbounded/unbounded-kube/tree/main/hack/cmd/forge) tool for development and testing, but any cluster works.
2. One or more cluster nodes with `UDP/51820-51899` open for WireGuard connectivity, labeled as gateway nodes.
3. Remote machines reachable via SSH.

## Install kubectl-unbounded

Download the latest release for your platform from the [releases page](https://github.com/project-unbounded/unbounded-kube/releases/latest):

```bash
# Linux (amd64)
curl -L https://github.com/project-unbounded/unbounded-kube/releases/latest/download/kubectl-unbounded-linux-amd64.tar.gz | tar -xz
sudo mv kubectl-unbounded /usr/local/bin/

# macOS (Apple Silicon)
curl -L https://github.com/project-unbounded/unbounded-kube/releases/latest/download/kubectl-unbounded-darwin-arm64.tar.gz | tar -xz
sudo mv kubectl-unbounded /usr/local/bin/

# verify
kubectl unbounded --help
```

## Prepare Gateway Nodes

Before initializing a site, at least one cluster node must be labeled as a
WireGuard gateway. Open `UDP/51820-51899` on the node and then label it:

```bash
kubectl label node <node-name> "unbounded-kube.io/unbounded-net-gateway=true"
```

`kubectl unbounded site init` will verify that a labeled gateway node exists
and will fail with an error if none is found.

## Initialize a Site

A **Site** represents a remote location where external machines are hosted.
The `kubectl unbounded site init` command bootstraps everything needed to
connect a site to your cluster: it installs the unbounded-net CNI plugin,
creates site resources, generates a bootstrap token, configures kubeadm
RBAC/ConfigMaps, and installs the machina controller.

```bash
kubectl unbounded site init \
    --name my-site \
    --cluster-node-cidr 10.224.0.0/16 \
    --cluster-pod-cidr 10.244.0.0/16 \
    --node-cidr 10.225.0.0/16 \
    --pod-cidr 10.245.0.0/16
```

### Required flags

| Flag | Description |
|------|-------------|
| `--name` | Name of the site |
| `--cluster-node-cidr` | CIDR used by the cluster for node IPs |
| `--cluster-pod-cidr` | CIDR used by the cluster for pod IPs |
| `--node-cidr` | CIDR to assign to node IPs at this site |
| `--pod-cidr` | CIDR to assign to pod IPs at this site |

### Optional flags

| Flag | Description |
|------|-------------|
| `--kubeconfig` | Path to kubeconfig file |
| `--cni-manifests` | Path or HTTPS URL to CNI manifests (defaults to a known release) |
| `--machina-manifests` | Path or HTTPS URL to machina manifests (uses embedded manifests if omitted) |
| `--cluster-service-cidr` | Service CIDR of the cluster (derived from kube-dns if omitted) |

## Add Machines

Once the site is initialized, register remote machines with
`kubectl unbounded site add-machine`. Each machine must be reachable via SSH.

```bash
kubectl unbounded site add-machine \
    --site my-site \
    --host 10.0.0.5 \
    --ssh-username ubuntu \
    --ssh-private-key ~/.ssh/id_rsa

kubectl unbounded site add-machine \
    --site my-site \
    --host 10.0.0.6:2222 \
    --ssh-username ubuntu \
    --ssh-private-key ~/.ssh/id_rsa
```

### Required flags

| Flag | Description |
|------|-------------|
| `--site` | Name of the site (must already be initialized) |
| `--host` | Host IP, optionally with port (e.g. `10.0.0.5` or `10.0.0.5:2222`) |
| `--ssh-username` | SSH username for connecting to the machine |

### Optional flags

| Flag | Description |
|------|-------------|
| `--name` | Machine name (derived from host if omitted; site name is always prefixed) |
| `--ssh-private-key` | Path to SSH private key file (required if no bastion is configured) |
| `--ssh-secret-name` | Kubernetes secret name for SSH credentials (defaults to `ssh-$site`) |
| `--bastion-host` | Bastion host and optionally port (e.g. `5.6.7.8` or `5.6.7.8:2222`) |
| `--bastion-ssh-username` | SSH username for the bastion (defaults to `--ssh-username`) |
| `--bastion-ssh-private-key` | Path to SSH private key file for the bastion (defaults to `--ssh-private-key`) |
| `--bastion-ssh-secret-name` | Kubernetes secret name for bastion SSH credentials (defaults to `--ssh-secret-name`) |
| `--kubeconfig` | Path to kubeconfig file |

## Watch Progress

Machines transition through phases as they are provisioned and joined to the cluster:

**Pending** &rarr; **Provisioning** &rarr; **Joining** &rarr; **Ready**

```bash
watch 'kubectl get machines'
```
