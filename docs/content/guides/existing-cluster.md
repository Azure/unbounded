---
title: "Bring Your Own Cluster"
weight: 2
description: "Add Unbounded Kube to an existing Kubernetes cluster and join remote nodes."
---

This guide adds Unbounded Kube to a Kubernetes cluster you already have running.
You'll label gateway nodes, initialize a site, and join remote machines.

> Starting from scratch? See the
> [Getting Started]({{< relref "guides/getting-started" >}}) guide to create an
> AKS cluster with everything pre-configured.

## What You'll Do

1. **[Install the prerequisites](#1-install-the-prerequisites)** -- kubectl and the unbounded plugin
2. **[Prepare gateway nodes](#2-prepare-gateway-nodes)** -- label and open WireGuard ports
3. **[Initialize a site](#3-initialize-a-site)** -- install the networking stack and create site resources
4. **[Add machines](#4-add-machines)** -- register remote hosts for SSH provisioning
5. **[Watch progress](#5-watch-progress)** -- monitor the provisioning lifecycle

---

## 1. Install the Prerequisites

> **You'll need:** A Kubernetes cluster with `kubeconfig` access, one or more
> cluster nodes with UDP 51820-51899 open, and remote machines reachable via SSH.

{{< callout type="info" >}}
**Clusters with an existing CNI** (e.g. Cilium, Calico, Azure CNI): Unbounded works alongside your existing CNI. Pass `--manage-cni-plugin=false` when running `site init`, or set `manageCniPlugin: false` on the Site resource after creation. See the [manageCniPlugin reference]({{< relref "reference/networking/custom-resources#managecniplugin-behavior" >}}) for details.

**Clusters without a CNI** (created with `--network-plugin none`): No extra configuration is needed — unbounded-net will serve as the CNI.
{{< /callout >}}

Install the kubectl-unbounded plugin:

```bash
# Linux (amd64)
curl -sL https://github.com/Azure/unbounded-kube/releases/latest/download/kubectl-unbounded-linux-amd64.tar.gz | tar xz
sudo mv kubectl-unbounded /usr/local/bin/
```

<details>
<summary>macOS (Apple Silicon)</summary>

```bash
curl -sL https://github.com/Azure/unbounded-kube/releases/latest/download/kubectl-unbounded-darwin-arm64.tar.gz | tar xz
sudo mv kubectl-unbounded /usr/local/bin/
```

</details>

Verify:

```bash
kubectl unbounded --help
```

---

## 2. Prepare Gateway Nodes

At least one cluster node must be labeled as a WireGuard gateway. Open
**UDP 51820-51899** on the node's firewall, then label it:

```bash
kubectl label node <node-name> "unbounded-kube.io/unbounded-net-gateway=true"
```

{{< callout type="important" >}}
`kubectl unbounded site init` checks for the gateway label and fails if no labeled nodes are found. You must also ensure UDP ports 51820-51899 are open on the gateway node's firewall before proceeding -- the init command does not open these ports for you.
{{< /callout >}}

---

## 3. Initialize a Site

A **Site** represents a remote location where machines will run. The `site init`
command installs unbounded-net, creates site resources, generates a bootstrap
token, and deploys the machina controller -- all in one step.

```bash
kubectl unbounded site init \
    --name my-site \
    --cluster-node-cidr 10.224.0.0/16 \
    --cluster-pod-cidr 10.244.0.0/16 \
    --node-cidr 192.168.1.0/24 \
    --pod-cidr 10.245.0.0/16
```

| Flag | Description |
|------|-------------|
| `--name` | Name of the remote site |
| `--cluster-node-cidr` | CIDR used by the cluster for node IPs |
| `--cluster-pod-cidr` | CIDR used by the cluster for pod IPs |
| `--node-cidr` | Subnet of your remote machines (e.g. `192.168.1.0/24`). Must not overlap with the cluster's node CIDR. |
| `--pod-cidr` | Pod CIDR for the remote site (e.g. `10.245.0.0/16`). Must not overlap with the cluster's pod or service CIDRs. |

<details>
<summary>Optional flags</summary>

| Flag | Description |
|------|-------------|
| `--kubeconfig` | Path to kubeconfig file |
| `--cni-manifests` | Path or URL to CNI manifests (defaults to a known release) |
| `--machina-manifests` | Path or URL to machina manifests (uses embedded manifests if omitted) |
| `--cluster-service-cidr` | Service CIDR (derived from kube-dns if omitted) |
| `--manage-cni-plugin` | Set to `false` when the cluster already has a CNI (default: `true`) |

</details>

{{< callout type="tip" >}}
**AKS users:** The `aks-quickstart.sh setup` command can auto-detect your cluster's CIDRs and add a properly configured gateway node pool. See the [Getting Started]({{< relref "guides/getting-started" >}}) guide for details.
{{< /callout >}}

---

## 4. Add Machines

Register remote machines with `machine create`. Each machine must be
reachable via SSH:

```bash
kubectl unbounded machine create \
    --site my-site \
    --host 10.0.0.5 \
    --ssh-username ubuntu \
    --ssh-private-key ~/.ssh/id_rsa
```

| Flag | Description |
|------|-------------|
| `--site` | Site name (must already be initialized) |
| `--host` | Host IP, optionally with port (`10.0.0.5` or `10.0.0.5:2222`) |
| `--ssh-username` | SSH username |

<details>
<summary>Optional flags</summary>

| Flag | Description |
|------|-------------|
| `--name` | Machine name (derived from host if omitted) |
| `--ssh-private-key` | Path to SSH private key |
| `--ssh-secret-name` | K8s secret name for SSH credentials (defaults to `ssh-$site`) |
| `--bastion-host` | Bastion host and optionally port |
| `--bastion-ssh-username` | Bastion SSH username (defaults to `--ssh-username`) |
| `--bastion-ssh-private-key` | Bastion SSH key (defaults to `--ssh-private-key`) |
| `--bastion-ssh-secret-name` | Bastion SSH secret (defaults to `--ssh-secret-name`) |
| `--kubeconfig` | Path to kubeconfig file |

</details>

---

## 5. Watch Progress

Machines transition through phases as they are provisioned:

**Pending** &rarr; **Provisioning** &rarr; **Joining** &rarr; **Ready**

```bash
watch 'kubectl get machines'
```

---

## Next Steps

- **[Project Overview]({{< relref "concepts/overview" >}})** -- how the
  components fit together
- **[SSH Guide]({{< relref "guides/ssh" >}})** -- bastion hosts,
  troubleshooting, and the full provisioning lifecycle
- **[PXE Guide]({{< relref "guides/pxe" >}})** -- boot bare-metal servers
  with metalman
- **[Networking Concepts]({{< relref "concepts/networking" >}})** -- cross-site
  pod networking deep dive
- **[CLI Reference]({{< relref "reference/cli" >}})** -- all
  `kubectl unbounded` commands and flags
- **[CRD Reference]({{< relref "reference/machina-crd" >}})** -- Machine and
  Image API specification
