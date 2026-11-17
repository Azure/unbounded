---
title: "Getting Started"
description: "Learn how to set up an AKS cluster and connect a remote worker node using Unbounded."
weight: 1
---

This guide provides an end-to-end workflow to set up an Azure Kubernetes Service (AKS) cluster and connect a remote worker node over SSH. By the end of this tutorial, you'll have a working example of a remote cluster joining an Unbounded control plane over an encrypted tunnel.

## What You'll Do

- Install the [required prerequisites](#1-install-the-prerequisites)
- Build an AKS cluster with pre-configured gateways by running the `aks-quickstart.sh` script
- Add a remote worker node over SSH
- Verify the connection with cluster node networking tests

## 1. Install the Prerequisites

| Prerequisites | What you need |
| -- | -- |
| Azure account | An Azure subscription and [terminal access](#ssh-recommended) |
| Azure CLI | Installed and configured with `az login` |
| kubectl | Installed via `az aks install-cli` |
| `kubectl-unbounded` | Installed on the control-plane terminal |

```bash
# On your terminal

# Install kubectl
az aks install-cli

# Install the unbounded plugin
curl -L https://github.com/Azure/unbounded/raw/main/install/kubectl-unbounded-linux-amd64.tar.gz | sudo tar xz
sudo install -m 755 kubectl-unbounded /usr/local/bin/kubectl-unbounded

# Verify installation
kubectl version --client
kubectl unbounded version
```

### Recommended terminal

We recommend a remote terminal such as `ssh -X` or `az ssh` instead of running commands directly on the local terminal. The following prerequisites apply whether you use a local or remote terminal.

| Prerequisites | Requirement |
| -- | -- |
| Azure account | An Azure subscription and `az login` |
| terminal | SSH terminal access |
| [SSH key pair](#ssh-recommended) | Required by the SSH tunnel |
| `kubectl` | Installed via `az aks install-cli` |
| `kubectl-unbounded` | `tar` installation (Linux amd64) or `brew` installation (macOS Apple Silicon) |

Verify installations on your SSH terminal.

## SSH Recommended

We recommend using `ssh -X` for a remote terminal that supports X11 forwarding or the [Azure-native SSH solution]({{< relref "guides/ssh" >}}#az-ssh) instead of running commands directly in a local terminal. This requires:

| Prereq | Requirement |
| -- | -- |
| Azure account | Azure subscription and `az login` |
| SSH terminal | Terminal with SSH access |
| SSH key pair | Required by the SSH tunnel |
| `kubectl` | Installed via `az aks install-cli` |
| `kubectl-unbounded` | `tar` installation (Linux amd64) or `brew` installation (macOS Apple Silicon) |

SSH terminal setup guides are available in the [SSH guide]({{< relref "guides/ssh" >}}#setup).

## 2. Create the Cluster

To create an AKS cluster, download and run the `aks-quickstart.sh` script. This command uses the `create` subcommand, which sets up a cluster with pre-configured gateways for remote node connectivity.

```bash
curl -L https://github.com/Azure/unbounded/raw/main/scripts/aks-quickstart.sh > aks-quickstart.sh
chmod +x aks-quickstart.sh
./aks-quickstart.sh create \
  --name my-unbounded \
  --location us-east-1 \
  --remote-node-cidr 10.42.0.0/16 \
  --remote-pod-cidr 10.43.0.0/16
```

Make sure to use non-overlapping CIDRs for your cluster and remote node sites.

You can customize this command with the following flags:

| Flag | Description |
|--|--|
| `--name` | The name of your Kubernetes cluster |
| `--location` | Azure region for your cluster |
| `--remote-node-cidr` | The CIDR range for the remote node |
| `--remote-pod-cidr` | The CIDR range for the remote pod |
| `--resource-group` | The Azure resource group name |
| `--kubernetes-version` | Azure Kubernetes version |
| `--nodes` | The number of nodes in your pool |
| `--node-sku` | The SKU of your node pool |
| `--service-cidr` | The service CIDR range |
| `--site-name` | The name of your remote site |
| `--ssh-key` | The public key for SSH access |
| `--public-ip-strategy` | Public IP allocation strategy |

## 3. Add a Remote Node

Next, connect a remote worker node to your Kubernetes cluster. Unbounded provides built-in support to join Linux VMs over SSH.

The `create-azure-vm` subcommand is available for those who want to deploy a remote node with a ready-to-go Ubuntu 24.04 LTS instance over SSH.

```bash
./aks-quickstart.sh create-azure-vm \
  --remote-site-cidr 10.42.0.0/16 \
  --name remote-node
```

This command creates a remote site configuration in your Kubernetes cluster and deploys a remote node to a new Ubuntu virtual machine with SSH access.

Now that your node is ready to join your cluster, run the `kubectl unbounded machine manual-bootstrap` command.

This command generates a bootstrap script. When you run it on the remote node machine, the script installs the Unbounded agent, sets up a WireGuard tunnel, and registers the remote node to join your Kubernetes cluster.

Here's how to SSH in and run this script without manually copying files:

```bash
ssh user@remote-node "sudo ./bootstrap.sh" < ./bootstrap.sh
```

The bootstrap command includes built-in tests that verify the Unbounded agent is running, the WireGuard tunnel is up, and the remote node is registered in the cluster.

## 4. Verify Connectivity

Your cluster and remote node are now connected. You can verify the connection with networking tests from the cluster node.

```bash
kubectl get nodes -w
```

The remote node is now in the `Ready` state in your cluster.

### Test pod networking

Run a networking test from the cluster to verify connectivity through the tunnel.

```bash
kubectl run -it -l app=networking-test --image=alpine --node-selector="unbounded-cloud.io/site=remote" -- bash

sleep 10

curl http://<cluster-node-internal-ip>:80

# Clean up
exit
```

Replace `<cluster-node-internal-ip>` with the internal IP address of the cluster node where your test pod is running. If the request returns a success response, you're connected.

The test pod returns to the control plane over the WireGuard tunnel, then returns control to the terminal.

## What the Script Creates

The `aks-quickstart.sh` script delegates to the following commands:

- `kubectl unbounded install`: Installs the required Unbounded components.
- `kubectl unbounded site init`: Initializes a new site configuration.

### AKS Cluster (BYO CNI)

To save resources, the script uses the "Bring Your Own CNI" model, which uses the `--network-plugin none` option during AKS cluster creation.

- Use the unbounded-net CNI for local pod networking.
- Use Unbounded-managed WireGuard tunnels for cross-site connectivity between remote nodes.
- Keep the default Azure CNI provider if you need to integrate with existing CNI in your Kubernetes cluster by passing `--manage-cni-plugin=false`.

### Gateway Node Pool

The script creates a dedicated `gwmain` pool with the following configurations:

- Assign each node in the `gwmain` pool its own public IP address.
- Configure required NSG rules for the tunnels (UDP 51820-51899).
- Set the `CriticalAddonsOnly=true:NoSchedule` taint to prevent pods from scheduling on gateway nodes.
- Label gateway nodes with `unbounded-cloud.io/unbounded-net-gateway=true`.

### unbounded-net CNI

The script creates the unbounded-net CNI, with the following configurations:

- Allocate a Kubernetes controller pod CIDR.
- Set up the key management system for WireGuard tunnels.
- Configure the agent DaemonSet to manage the WireGuard tunnels.
- Create the routes for encrypted cross-site tunnels and GENEVE-based intra-site networking.

### Site Resources

The script creates the required resources for remote site connectivity:

- `GatewayPool` (`gw-main`): This resource selects the gateway nodes.
- `Site`: These resources define the CIDR ranges for the sites. These ranges are used by the CNI for address management and by WireGuard for tunnel configuration.
- `Cluster`: These resources represent the sites in your Kubernetes cluster and remote sites.
- `SiteGatewayPoolAssignment`: These resources link your gateway nodes to a specific site.

### Machina Controller and Bootstrap Token

The `manual-bootstrap` command generates an authenticated bootstrap script for an SSH-based node join:

- Authentication with the Kubernetes control plane through a `kube-system` token.
- Embed the bootstrap token in the generated `./bootstrap.sh`.

You can join nodes with existing SSH automation by using the same bootstrap token from `./bootstrap.sh`. The same token is used by the following bootstrapping systems:

- Provisioning your nodes with Machina.
- Automated node provisioning in the cloud with Karpenter.
- PXE boot node provisioning with metalman.

## Cleanup

If you're just testing Unbounded, you can delete your Azure resource group with the `--yes` flag and use the `--no-wait` flag to avoid waiting for Azure to finish deleting the resources before you return control to the terminal:

```bash
az group delete --name my-unbounded --yes --no-wait
```

> **Caution:**
> This command deletes your Azure resource group. If you plan to continue using this resource group after you finish with this quickstart guide, don't run this command.

## Next Steps

- Review the [project overview]({{< relref "concepts/overview" >}}) for an overview of the Unbounded project architecture and component relationships.
- To use existing Kubernetes clusters, see [Bring Your Own Cluster]({{< relref "guides/existing-cluster" >}}).
- To learn more about the SSH provisioning lifecycle, see [SSH]({{< relref "guides/ssh" >}}).
- For a deep dive into how Unbounded manages site-to-site connectivity, see [Networking Concepts]({{< relref "concepts/networking" >}}).
- For reference information on the commands and flags available to you, see [Reference]({{< relref "reference/cli" >}}).
