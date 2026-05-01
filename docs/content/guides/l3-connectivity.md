---
title: "L3 Connectivity (Azure VPN + Ubiquiti)"
weight: 5
description: "Connect an AKS cluster to a remote Ubiquiti network over Azure VPN Gateway using direct L3 routing instead of WireGuard tunnels."
---

This guide walks through connecting an existing AKS cluster to a remote site
over an Azure VPN Gateway and a Ubiquiti router. Because the VPN provides
private L3 connectivity between the two networks, you'll configure Unbounded to route directly over that link -- no WireGuard overlay needed.

{{< callout type="info" >}}
This guide uses Azure VPN Gateway and a Ubiquiti router as a concrete example, but the same approach works with **any L3 interconnect** -- Azure ExpressRoute, AWS Direct Connect, GCP Cloud Interconnect, a hardware VPN appliance, or even a simple routed link between two networks. The Unbounded configuration in [steps 5-9](#5-prepare-gateway-nodes) is identical regardless of how the L3 path is established.
{{< /callout >}}

> New to Unbounded? Start with the
> [Getting Started]({{< relref "guides/getting-started" >}}) guide to create an
> AKS cluster from scratch, then come back here to add L3 connectivity.

## What You'll Do

1. **[Prerequisites](#1-prerequisites)** -- tools and infrastructure you'll need
2. **[Create an Azure VPN Gateway](#2-create-an-azure-vpn-gateway)** -- build the Azure side of the VPN tunnel
3. **[Configure the Ubiquiti side](#3-configure-the-ubiquiti-side)** -- establish the IPsec tunnel from your router
4. **[Verify the VPN connection](#4-verify-the-vpn-connection)** -- confirm L3 reachability between networks
5. **[Prepare gateway nodes](#5-prepare-gateway-nodes)** -- label AKS nodes for unbounded-net
6. **[Initialize the site](#6-initialize-the-site)** -- install unbounded-net and create site resources
7. **[Configure direct L3 routing](#7-configure-direct-l3-routing)** -- disable WireGuard and route over the VPN
8. **[Add remote machines](#8-add-remote-machines)** -- join machines from the Ubiquiti network
9. **[Verify connectivity](#9-verify-connectivity)** -- test cross-site pod networking

---

## 1. Prerequisites

> **You'll need:**
> - An existing AKS cluster with `kubeconfig` access
> - Azure CLI (`az`) logged in with permissions to create networking resources
> - `kubectl` and the `kubectl-unbounded` plugin installed
> - A Ubiquiti router (UniFi Security Gateway, UDM, UDM-Pro, or EdgeRouter) with a static public IP
> - Remote Linux machines behind the Ubiquiti router reachable via SSH

Install the prerequisites if you haven't already:

```bash
# kubectl-unbounded (Linux amd64)
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
az version && kubectl version --client && kubectl unbounded --help
```

---

## 2. Create an Azure VPN Gateway

This section creates an Azure VPN Gateway in the same VNet as your AKS cluster
and establishes a site-to-site IPsec connection to your Ubiquiti router.

### Set variables

Adjust these values to match your environment:

```bash
# Azure / AKS
RESOURCE_GROUP="my-aks-rg"              # Resource group containing your AKS cluster
VNET_NAME="my-aks-vnet"                 # VNet used by AKS
LOCATION="eastus"                       # Azure region

# Remote (Ubiquiti) network
UBIQUITI_PUBLIC_IP="203.0.113.1"        # Public IP of your Ubiquiti router
UBIQUITI_LAN_CIDR="192.168.1.0/24"      # LAN subnet behind the Ubiquiti router
VPN_SHARED_KEY="$(openssl rand -base64 32)"  # Pre-shared key for IPsec
```

{{< callout type="warning" >}}
Store the `VPN_SHARED_KEY` securely -- you'll need the same key when configuring the Ubiquiti side. Do not commit it to source control.
{{< /callout >}}

### Create the GatewaySubnet

Azure VPN Gateways require a dedicated subnet named `GatewaySubnet`:

```bash
az network vnet subnet create \
    --resource-group "$RESOURCE_GROUP" \
    --vnet-name "$VNET_NAME" \
    --name GatewaySubnet \
    --address-prefix 10.225.1.0/27
```

{{< callout type="note" >}}
The `GatewaySubnet` address prefix must not overlap with any existing subnets in the VNet. The example uses `10.224.255.0/27` (30 usable addresses), which is sufficient for a single VPN Gateway. Adjust the prefix if this range is already in use.
{{< /callout >}}

### Create a public IP for the VPN Gateway

```bash
az network public-ip create \
    --resource-group "$RESOURCE_GROUP" \
    --name vpn-gateway-ip \
    --allocation-method Static \
    --sku Standard \
    --zone 1 2 3
```

### Create the VPN Gateway

```bash
az network vnet-gateway create \
    --resource-group "$RESOURCE_GROUP" \
    --name aks-vpn-gateway \
    --vnet "$VNET_NAME" \
    --gateway-type Vpn \
    --vpn-type RouteBased \
    --sku VpnGw1AZ \
    --public-ip-address vpn-gateway-ip \
    --no-wait
```

{{< callout type="important" >}}
VPN Gateway provisioning takes **25-45 minutes**. The `--no-wait` flag returns immediately. Check progress with:

```bash
az network vnet-gateway show \
    --resource-group "$RESOURCE_GROUP" \
    --name aks-vpn-gateway \
    --query provisioningState -o tsv
```

Wait until the output shows `Succeeded` before continuing.
{{< /callout >}}

### Create the Local Network Gateway

The Local Network Gateway represents your Ubiquiti router in Azure:

```bash
az network local-gateway create \
    --resource-group "$RESOURCE_GROUP" \
    --name ubiquiti-local-gateway \
    --gateway-ip-address "$UBIQUITI_PUBLIC_IP" \
    --local-address-prefixes "$UBIQUITI_LAN_CIDR"
```

### Create the VPN connection

```bash
az network vpn-connection create \
    --resource-group "$RESOURCE_GROUP" \
    --name aks-to-ubiquiti \
    --vnet-gateway1 aks-vpn-gateway \
    --local-gateway2 ubiquiti-local-gateway \
    --shared-key "$VPN_SHARED_KEY"
```

### Retrieve the Azure VPN public IP

You'll need this IP when configuring the Ubiquiti side:

```bash
AZURE_VPN_IP=$(az network public-ip show \
    --resource-group "$RESOURCE_GROUP" \
    --name vpn-gateway-ip \
    --query ipAddress -o tsv)
echo "Azure VPN Gateway IP: $AZURE_VPN_IP"
```

### Allow VPN traffic through the AKS NSG

By default, AKS Network Security Groups block inbound traffic from outside the
VNet. Add a rule to allow traffic from the Ubiquiti LAN through the VPN:

```bash
# Find the NSG attached to the AKS node subnet
AKS_NSG=$(az network nsg list \
    --resource-group "$RESOURCE_GROUP" \
    --query "[0].name" -o tsv)

az network nsg rule create \
    --resource-group "$RESOURCE_GROUP" \
    --nsg-name "$AKS_NSG" \
    --name allow-vpn-inbound \
    --priority 100 \
    --direction Inbound \
    --access Allow \
    --protocol '*' \
    --source-address-prefixes "$UBIQUITI_LAN_CIDR" \
    --destination-address-prefixes '*' \
    --destination-port-ranges '*'
```

{{< callout type="note" >}}
If your AKS cluster uses a managed resource group (e.g. `MC_my-aks-rg_my-aks_eastus`), the NSG may be in that group instead. Replace `$RESOURCE_GROUP` with the managed resource group name, or find the NSG with:

```bash
az network nsg list --query "[?contains(name,'aks')].{name:name,rg:resourceGroup}" -o table
```
{{< /callout >}}

---

## 3. Configure the Ubiquiti Side

Configure your Ubiquiti router to establish an IPsec site-to-site VPN back to
the Azure VPN Gateway. The exact steps vary by device, but the parameters are
the same.

### IPsec parameters

Use these values when creating the VPN on your Ubiquiti device:

| Parameter | Value |
|-----------|-------|
| **Remote gateway** | `$AZURE_VPN_IP` (from the previous step) |
| **Pre-shared key** | Same `$VPN_SHARED_KEY` used above |
| **IKE version** | IKEv2 |
| **Phase 1 encryption** | AES-256 |
| **Phase 1 hash** | SHA-256 |
| **Phase 1 DH group** | 2 (1024-bit) |
| **Phase 2 encryption** | AES-256 |
| **Phase 2 hash** | SHA-256 |
| **Phase 2 PFS** | Enabled (DH group 2) |
| **Local subnet** | `192.168.1.0/24` (your LAN behind the Ubiquiti router) |
| **Remote subnet** | AKS VNet CIDR (e.g. `10.224.0.0/12`) |

{{< callout type="tip" >}}
**UniFi (UDM, UDM-Pro, USG):** Go to **Settings > VPN > Site-to-Site VPN > Create Site-to-Site VPN**. Select "Manual IPsec" and enter the parameters above.

**EdgeRouter:** Use the CLI or the VPN wizard at **Wizards > VPN > IPsec Site-to-Site**. The [Ubiquiti Help Center](https://help.ui.com) has model-specific guides.
{{< /callout >}}

{{< callout type="important" >}}
The remote subnet on the Ubiquiti side must include the AKS node CIDR **and** the pod CIDR you plan to use for the remote site. If your AKS VNet is `10.224.0.0/16` and cluster pods use `10.244.0.0/16`, configure the remote subnet as `10.0.0.0/8` or add both prefixes separately. Without this, return traffic from remote machines to cluster pods will not traverse the VPN.
{{< /callout >}}

### Configure routes on the Ubiquiti router

The IPsec tunnel alone does not automatically create routes for the remote
CIDRs. You must add static routes on the Ubiquiti router so that traffic
destined for the AKS node CIDR, cluster pod CIDR, and remote pod CIDR is
forwarded through the VPN interface.

The routes you need:

| Destination | Description |
|-------------|-------------|
| `10.224.0.0/16` | AKS node subnet |
| `10.244.0.0/16` | AKS cluster pod CIDR |
| `10.245.0.0/16` | Remote site pod CIDR (allocated to pods on remote nodes) |

{{< callout type="tip" >}}
**UniFi (UDM, UDM-Pro, USG):** Go to **Settings > Routes > Create New Route**. For each CIDR above, set the destination network and select the IPsec VPN interface as the next hop.

**EdgeRouter:** Add static routes via the CLI:

```bash
set protocols static interface-route 10.224.0.0/16 next-hop-interface <vpn-interface>
set protocols static interface-route 10.244.0.0/16 next-hop-interface <vpn-interface>
set protocols static interface-route 10.245.0.0/16 next-hop-interface <vpn-interface>
commit; save
```

Replace `<vpn-interface>` with the IPsec tunnel interface name (e.g. `vti0` or `ipsec0`).
{{< /callout >}}

{{< callout type="warning" >}}
Without these routes, the Ubiquiti router will not know to forward cluster-bound traffic through the VPN tunnel. Pings to AKS node IPs will fail with "Destination Host Unreachable", and pod-to-pod traffic across sites will not work.
{{< /callout >}}

---

## 4. Verify the VPN Connection

Wait for the tunnel to come up, then verify from the Azure side:

```bash
az network vpn-connection show \
    --resource-group "$RESOURCE_GROUP" \
    --name aks-to-ubiquiti \
    --query connectionStatus -o tsv
```

The output should show `Connected`. If it shows `Connecting` or `Unknown`,
check the pre-shared key and IPsec parameters on both sides.

On the Ubiquiti side, verify the IPsec tunnel status through your device's
dashboard or CLI (`show vpn ipsec sa` on EdgeRouter,
**Settings > VPN > Site-to-Site VPN** on UniFi).

Test basic L3 reachability from a machine on the Ubiquiti network:

```bash
# From a machine on the Ubiquiti LAN, ping an AKS node's internal IP
ping -c 3 10.224.0.4
```

{{< callout type="note" >}}
If the ping fails, check that NSG rules on the AKS VNet allow ICMP from the Ubiquiti LAN CIDR, and that the Ubiquiti router's VPN policy routes traffic for the AKS VNet through the IPsec tunnel.
{{< /callout >}}

---

## 5. Prepare Gateway Nodes

At least one AKS node must be labeled as a gateway for unbounded-net. Unlike the
standard WireGuard setup, you do **not** need to open UDP 51820-51899 because
this configuration uses the VPN for all cross-site traffic.

```bash
# Pick a node (or use a dedicated node pool)
kubectl label node <node-name> "unbounded-cloud.io/unbounded-net-gateway=true"
```

{{< callout type="tip" >}}
If your AKS cluster was created with the quickstart script, gateway nodes are already labeled. Verify with:

```bash
kubectl get nodes -l unbounded-cloud.io/unbounded-net-gateway=true
```
{{< /callout >}}

---

## 6. Initialize the Site

Run `kubectl unbounded site init` to install the networking stack and create
site resources. The CIDRs must match the networks reachable over the VPN:

```bash
kubectl unbounded site init \
    --name ubiquiti-site \
    --cluster-node-cidr 10.224.0.0/16 \
    --cluster-pod-cidr 10.244.0.0/16 \
    --node-cidr 192.168.1.0/24 \
    --pod-cidr 10.245.0.0/16
```

| Flag | Description |
|------|-------------|
| `--name` | Name for the remote site (used in Site and SitePeering resources) |
| `--cluster-node-cidr` | CIDR of the AKS VNet node subnet |
| `--cluster-pod-cidr` | Pod CIDR used by the AKS cluster |
| `--node-cidr` | Subnet behind the Ubiquiti router (`192.168.1.0/24`) |
| `--pod-cidr` | Pod CIDR for the remote site (must not overlap cluster CIDRs) |

{{< callout type="info" >}}
**Clusters with an existing CNI** (e.g. Azure CNI, Cilium, Calico): Add `--manage-cni-plugin=false` so unbounded-net doesn't overwrite the existing CNI configuration. See [manageCniPlugin behavior]({{< relref "reference/networking/custom-resources#managecniplugin-behavior" >}}) for details.
{{< /callout >}}

This command creates:

- The **unbounded-net CNI** controller and node agent
- A **cluster** Site for the AKS nodes
- A **ubiquiti-site** Site for the remote machines
- A **GatewayPool** (`gw-main`) selecting labeled gateway nodes
- **SiteGatewayPoolAssignments** linking both sites to the gateway pool
- A **bootstrap token** for the remote site
- The **machina** controller for SSH-based provisioning

---

## 7. Configure Direct L3 Routing

This is the key step that differentiates an L3 connectivity setup from the
default WireGuard configuration. You'll create a `SitePeering` that tells
unbounded-net to route directly over the VPN instead of building WireGuard
tunnels, and update the gateway pool to use internal IPs.

### Create the SitePeering

The `SitePeering` with `meshNodes: false` and `tunnelProtocol: None` tells
unbounded-net that these two sites are already connected at L3. No WireGuard
tunnels are created; routes point directly at internal IPs reachable over the
VPN:

```bash
kubectl apply -f - <<'EOF'
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SitePeering
metadata:
  name: aks-to-ubiquiti-peering
spec:
  sites:
    - cluster
    - ubiquiti-site
  meshNodes: false       # do not create WireGuard tunnels between nodes
  tunnelProtocol: None   # route directly over the existing L3 path
EOF
```

#### What these fields do

| Field | Value | Effect |
|-------|-------|--------|
| `meshNodes` | `false` | Disables direct node-to-node WireGuard tunnels. Traffic flows through gateways using internal IPs. |
| `tunnelProtocol` | `None` | No encapsulation overhead. Packets are routed as-is over the VPN. |

{{< callout type="tip" >}}
Both approaches can coexist in the same cluster. For example, you could have this direct L3 peering for your Ubiquiti site while other remote sites still connect over WireGuard through the public internet. See [Externally Peered Sites]({{< relref "concepts/networking#externally-peered-sites" >}}) for more details.
{{< /callout >}}

### Update the GatewayPool type to Internal

By default, `site init` creates the gateway pool with `type: External`, which
means cross-site connections resolve to gateway nodes' external (public) IPs.
Since VPN traffic uses internal IPs, change the pool type to `Internal`:

```bash
kubectl patch gatewaypool gw-main --type merge -p '{"spec":{"type":"Internal"}}'
```

| Pool Type | IP Used for Cross-Site | When to Use |
|-----------|----------------------|-------------|
| `External` (default) | Public IPs | Sites connected over the public internet (WireGuard) |
| `Internal` | Internal/private IPs | Sites connected over VPN, ExpressRoute, or Direct Connect |

After applying these changes, unbounded-net routes cross-site traffic through
the gateway nodes' internal IPs, which are reachable through the Azure VPN
tunnel.

---

## 8. Add Remote Machines

Register machines from the Ubiquiti network. These machines must be reachable
via SSH from the AKS cluster (which they are, since the VPN provides L3
connectivity):

```bash
kubectl unbounded machine register \
    --site ubiquiti-site \
    --host 192.168.1.100 \
    --ssh-username ubuntu \
    --ssh-private-key ~/.ssh/id_rsa
```

{{< callout type="note" >}}
The `--host` IP must be the machine's LAN IP on the Ubiquiti network (e.g. `192.168.1.100`), not a public IP. The VPN connection makes these internal IPs directly reachable from the AKS cluster.
{{< /callout >}}

Or use `manual-bootstrap` to pipe a bootstrap script over SSH:

```bash
kubectl unbounded machine manual-bootstrap my-node --site ubiquiti-site \
    | ssh ubuntu@192.168.1.100 sudo bash
```

Watch the machine progress through provisioning phases:

```bash
watch 'kubectl get machines'
```

**Pending** &rarr; **Provisioning** &rarr; **Joining** &rarr; **Ready**

---

## 9. Verify Connectivity

Once the remote node shows **Ready**, verify cross-site pod networking:

```bash
kubectl get nodes -w
```

### Test cross-site pod networking

```bash
# Run a pod on the remote node
kubectl run test-remote --image=busybox --restart=Never \
    --overrides='{"spec":{"nodeSelector":{"net.unbounded-cloud.io/site":"ubiquiti-site"}}}' \
    -- sleep 3600

# Get a cluster node's internal IP
CLUSTER_NODE_IP=$(kubectl get nodes -l 'net.unbounded-cloud.io/site=cluster' \
    -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')

# Ping a cluster node from the remote pod (over the VPN -- no WireGuard)
kubectl exec test-remote -- ping -c 3 "$CLUSTER_NODE_IP"

# Run a pod on a cluster node and curl it from the remote pod
kubectl run test-cluster --image=nginx --restart=Never \
    --overrides='{"spec":{"nodeSelector":{"net.unbounded-cloud.io/site":"cluster"}}}'
kubectl wait --for=condition=ready pod/test-cluster --timeout=60s
CLUSTER_POD_IP=$(kubectl get pod test-cluster -o jsonpath='{.status.podIP}')
kubectl exec test-remote -- wget -qO- "http://$CLUSTER_POD_IP"

# Clean up
kubectl delete pod test-remote test-cluster
```

If both the ping and wget succeed, pod traffic is flowing directly over the VPN
with no WireGuard encapsulation.

### Verify no WireGuard tunnels are created

You can confirm that no WireGuard interfaces were set up between the two sites:

```bash
# On a gateway node, list WireGuard interfaces
kubectl debug node/<gateway-node> -it --image=busybox -- ip link show type wireguard
```

With `tunnelProtocol: None` and `meshNodes: false`, you should see no WireGuard
interfaces for the ubiquiti-site peering.

---

## How It Works

In a standard Unbounded deployment, remote nodes establish encrypted
WireGuard tunnels to gateway nodes over the public internet. This is secure and
works anywhere, but adds encapsulation overhead and requires public IPs on
gateway nodes.

When you have private L3 connectivity (like the Azure VPN Gateway in this
guide), the WireGuard overlay is redundant -- the VPN already provides the
network path. The configuration in this guide tells unbounded-net to skip
WireGuard and route packets directly over the existing L3 path:

| Component | Standard (WireGuard) | This Guide (Direct L3) |
|-----------|---------------------|----------------------|
| Cross-site transport | WireGuard tunnels over public internet | Azure VPN (IPsec) over public internet |
| Encryption | WireGuard (built-in) | IPsec (handled by VPN) |
| Encapsulation overhead | 80 bytes (WireGuard header) | 0 bytes (direct routing) |
| Gateway pool type | `External` (public IPs) | `Internal` (private IPs) |
| SitePeering `meshNodes` | `true` (direct tunnels) | `false` (no tunnels) |
| SitePeering `tunnelProtocol` | `Auto` (WireGuard for external) | `None` (direct L3) |
| Gateway node ports | UDP 51820-51899 open | No special ports needed |

The result is lower overhead and simpler firewall rules, at the cost of managing
the VPN connection separately.

---

## Cleanup

To remove the VPN Gateway and associated Azure resources:

```bash
az network vpn-connection delete \
    --resource-group "$RESOURCE_GROUP" \
    --name aks-to-ubiquiti --yes

az network vnet-gateway delete \
    --resource-group "$RESOURCE_GROUP" \
    --name aks-vpn-gateway

az network local-gateway delete \
    --resource-group "$RESOURCE_GROUP" \
    --name ubiquiti-local-gateway

az network public-ip delete \
    --resource-group "$RESOURCE_GROUP" \
    --name vpn-gateway-ip
```

To remove the Unbounded site resources:

```bash
kubectl delete sitepeering aks-to-ubiquiti-peering
kubectl delete machines --selector unbounded-cloud.io/site=ubiquiti-site
```

---

## Next Steps

- **[Networking Concepts]({{< relref "concepts/networking" >}})** -- how
  unbounded-net routes traffic across sites
- **[Externally Peered Sites]({{< relref "concepts/networking#externally-peered-sites" >}})** --
  deep dive on direct L3 routing with `SitePeering`
- **[Custom Resources]({{< relref "reference/networking/custom-resources" >}})** --
  full specification for Site, GatewayPool, and SitePeering
- **[SSH Guide]({{< relref "guides/ssh" >}})** -- bastion hosts,
  troubleshooting, and the full provisioning lifecycle
- **[CLI Reference]({{< relref "reference/cli" >}})** -- all
  `kubectl unbounded` commands and flags