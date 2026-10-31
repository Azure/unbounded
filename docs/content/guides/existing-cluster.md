---
title: "Bring Your Own Cluster"
weight: 2
description: "Add Unbounded to an existing Kubernetes cluster and join remote nodes."
---

This guide adds Unbounded to a Kubernetes cluster you already have running.
You'll label gateway nodes, initialize a site, and join remote machines.

> Starting from scratch? See the
> [Getting Started]({{< relref "guides/getting-started" >}}) guide to create an
> AKS cluster with everything pre-configured.

## What You'll Do

1. **[Install the prerequisites](#1-install-the-prerequisites)** -- kubectl and the unbounded plugin
2. **[Prepare gateway nodes](#2-prepare-gateway-nodes)** -- label and open WireGuard ports
3. **[Initialize Sites](#3-initialize-sites)** -- install the operator, create the cluster and remote Sites, and route them through the gateways
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
curl -sL https://github.com/Azure/unbounded/releases/latest/download/kubectl-unbounded-linux-amd64.tar.gz | tar xz
sudo mv kubectl-unbounded /usr/local/bin/
```

<details>
<summary>macOS (Apple Silicon)</summary>

```bash
curl -sL https://github.com/Azure/unbounded/releases/latest/download/kubectl-unbounded-darwin-arm64.tar.gz | tar xz
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
kubectl label node <node-name> "unbounded-cloud.io/unbounded-net-gateway=true"
```

{{< callout type="important" >}}
The `GatewayPool` you create in [step 3](#3-initialize-sites) selects this label. You must ensure UDP ports 51820-51899 are open on the gateway node's firewall; Unbounded does not open these ports for you.
{{< /callout >}}

---

## 3. Initialize Sites

First, bootstrap `unbounded-operator` on the cluster. This installs the CRDs and
the operator that reconciles Unbounded components:

```bash
kubectl unbounded install
```

A **Site** represents a location where nodes run. The `site init` command
creates one Site per run. It requires the operator to already be installed (it
errors otherwise). It creates the Site, records the requested components in
`Site.spec.components`, and generates a bootstrap token. The operator deploys
unbounded-net, machina, and optional components from the Site specs.

Run it twice: once with the reserved name `cluster` for your cluster's own
nodes, then once for the remote site. The `cluster` Site gets no bootstrap
token, since no machines join through it.

```bash
kubectl unbounded site init \
    --name cluster \
    --node-cidr 10.224.0.0/16 \
    --pod-cidr 10.244.0.0/16

kubectl unbounded site init \
    --name my-site \
    --node-cidr 192.168.1.0/24 \
    --pod-cidr 10.245.0.0/16
```

| Flag | Description |
|------|-------------|
| `--name` | Name of the Site: `cluster` for the cluster's own nodes, or a name for the remote site |
| `--node-cidr` | Comma-separated CIDRs that contain the Site's node IPs. For `cluster`, the cluster's node subnet; for the remote site, the subnet of your remote machines (e.g. `192.168.1.0/24`), which must not overlap with the cluster's. |
| `--pod-cidr` | Comma-separated CIDRs for the Site's pod IPs. For `cluster`, the cluster's pod CIDR; for the remote site, a range such as `10.245.0.0/16` that must not overlap with the cluster's pod or service CIDRs. |

<details>
<summary>Optional flags</summary>

| Flag | Description |
|------|-------------|
| `--kubeconfig` | Path to kubeconfig file |
| `--manage-cni-plugin` | Set to `false` when the cluster already has a CNI (default: `true`) |
| `--enable-machina` | Enable machina in the Site spec (default: `true`) |
| `--enable-metalman` | Enable the metalman component in the Site spec |

</details>

Then route both Sites through the gateway nodes you labeled in step 2:

```bash
kubectl unbounded net create gatewaypool gw-main \
    --type External \
    --node-selector unbounded-cloud.io/unbounded-net-gateway=true
kubectl unbounded net create sitegatewaypoolassignment cluster \
    --site cluster --gateway-pool gw-main
kubectl unbounded net create sitegatewaypoolassignment my-site \
    --site my-site --gateway-pool gw-main
```

{{< callout type="tip" >}}
**AKS users:** The `aks-quickstart.sh setup` command can auto-detect your cluster's CIDRs and add a properly configured gateway node pool. See the [Getting Started]({{< relref "guides/getting-started" >}}) guide for details.
{{< /callout >}}

---

## 4. Add Machines

Register remote machines with `machine register`. Each machine must be
reachable via SSH:

```bash
kubectl unbounded machine register \
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
