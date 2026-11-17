---
title: "L3 Connectivity (Azure VPN + Ubiquiti)"
weight: 5
description: "Connect an on-premises Ubiquiti site to an Azure AKS cluster over an IPsec VPN and route pod traffic with SitePeering and gateway pool scopes set to the None tunnel protocol."
---

## Overview

Unbounded normally carries cross-site pod traffic through an encrypted WireGuard
tunnel that it builds and maintains itself. That is the right default when the
only path between two locations is the public internet. It is not the only
option. When your network team has already built a private Layer 3 path between
an Azure virtual network and an on-premises LAN, you can tell Unbounded to use
that path directly and skip encapsulation on it.

This guide builds that arrangement with an Azure VPN Gateway and a Ubiquiti
router. The VPN provides IPsec encryption and private addressing across the WAN.
Unbounded then treats the two locations as one routed network: nodes keep their
own internal IP addresses, pods keep their assigned CIDRs, and packets cross the
VPN without a second tunnel inside it.

The result is easy to describe but has several moving parts. You will create the
Azure gateway, the local network gateway, and the connection that represents the
Ubiquiti router. You will configure the router, including the routes that carry
pod CIDRs rather than only node addresses. You will initialize the Azure and
on-premises Sites, select one gateway node at each end, join the two gateway
pools with a peering, and create a `SitePeering` whose `tunnelProtocol` is
`None`. Finally, you will verify that the data path uses internal addresses and
that pods can reach each other in both directions.

This page covers the network layer only. It assumes the Unbounded operator and
the unbounded-net controller are installed, and that the on-premises machine is
ready to join the cluster. If either is not true, start with the
[getting started guide]({{< relref "guides/getting-started" >}}) and the
[node agent guide]({{< relref "guides/agent" >}}).

## Topology and address planning

The topology has two Sites and one gateway pool per Site. The Azure Site is the
conventional `cluster` Site that represents the AKS control plane and its
nodes. The on-premises Site is a separate Site whose nodes sit behind the
Ubiquiti router. Each Site contributes one gateway node to its local pool:
`gw-main` for the cluster and `gw-onprem` for the on-premises Site. A
`GatewayPoolPeering` joins the two pools. Workers send cross-site pod traffic to
their local gateway, and the two gateways exchange it directly over the VPN.

```text
+-------------------------------+              +-------------------------------+
| Azure VNet 10.10.0.0/16       |              | On-premises LAN 10.20.0.0/16  |
|                               |              |                               |
|  AKS node subnet              |   IPsec      |  Ubiquiti router              |
|  10.10.1.0/24                 |<============>|  10.20.0.1                    |
|    gateway node 10.10.1.10    |   IKEv2      |    gateway node 10.20.1.10    |
|    pod CIDR 100.64.0.0/16     |              |    pod CIDR 100.65.0.0/16     |
|                               |              |                               |
|  GatewaySubnet 10.10.255.0/27 |              |                               |
+-------------------------------+              +-------------------------------+
```

Write the address plan down before you create anything. The examples in this
guide use the following values; replace them consistently if your network uses
different ranges.

| Purpose | Azure | On-premises | Notes |
|---------|-------|-------------|-------|
| Site node CIDR | `10.10.0.0/16` | `10.20.0.0/16` | Must not overlap. Site membership is matched on these addresses. |
| Node subnet or LAN | `10.10.1.0/24` | `10.20.1.0/24` | Holds the node internal IPs. |
| Gateway node | `10.10.1.10` | `10.20.1.10` | One per Site. Give each a stable address. |
| Pod CIDR pool | `100.64.0.0/16` | `100.65.0.0/16` | Must be unique and advertised across the VPN. |
| VPN transit subnet | `10.10.255.0/27` | Not applicable | Azure `GatewaySubnet`. Do not place workloads in it. |
| Router or VPN endpoint | Azure public IP | Ubiquiti public IP | The two IPsec peers. |

Three rules matter more than the specific numbers. First, no Site CIDR or pod
pool may overlap another Site, because the controller rejects overlapping
`nodeCidrs` and pod blocks. Second, the IPsec selectors on both ends must carry
the pod pools as well as the node CIDRs. A tunnel that carries only
`10.10.0.0/16` and `10.20.0.0/16` lets you ping nodes but drops pod traffic.
Third, every router and route table on the path must know which end owns each
pod pool. A packet that reaches the right VPN gateway but not the right node
will be dropped even though the tunnel itself looks healthy.

{{< callout type="note" >}}
The `None` protocol removes Unbounded's tunnel, not the VPN. IPsec still
encrypts traffic across the public internet. Inside the Azure VNet and the
on-premises LAN, traffic is unencrypted. Treat both networks as trusted.
{{< /callout >}}

## Prerequisites

You need an AKS cluster with a VNet you can modify, and the cluster must already
be running the Unbounded operator and unbounded-net. The Azure identity you use
must be able to create subnets, public IP addresses, virtual network gateways,
local network gateways, VPN connections, route tables, and network security
group rules in the cluster resource group. You also need permission to read and
modify resources in the AKS node resource group, which is where the node subnet,
its network security group, and the node network interfaces live.

On the on-premises side you need a Ubiquiti gateway or security gateway that
supports site-to-site IPsec in the UniFi Network application, and a static public
IP address for it. The router must be able to forward traffic between its LAN
and the VPN interface, and you must be able to add static routes and firewall
rules. At least one Linux machine on that LAN should already be joined to the
cluster, or ready to join, with an internal IP inside the planned Site CIDR.

On your workstation you need `az`, `kubectl`, and the `kubectl-unbounded` plugin.
The plugin supplies the `kubectl unbounded net` commands used later to inspect
peers and routes. Confirm the versions before you start:

```bash
az account show --query "{subscription:id, tenant:tenantId}" --output table
kubectl version --client
kubectl unbounded version
```

The Azure commands below use the Bash shell. If you use PowerShell, adapt the
variable syntax; the `az` arguments are the same.

## Create the Azure VPN gateway

The Azure side has four resources: a `GatewaySubnet`, a static public IP, a
virtual network gateway, and a local network gateway that represents the
Ubiquiti router. The order matters, because the virtual network gateway needs
the subnet and the public IP, and the VPN connection needs both gateways.

### Set the environment variables

Export the values once so that later commands stay readable. The public IP in
`UBIQUITI_PUBLIC_IP` is the address of the router, not the address of an
individual on-premises node.

```bash
CLUSTER_RESOURCE_GROUP="rg-unbounded-eastus"
CLUSTER_NAME="unbounded-aks"
CLUSTER_VNET_NAME="aks-vnet"
LOCATION="eastus"

GATEWAY_SUBNET_PREFIX="10.10.255.0/27"
AZURE_NODE_CIDR="10.10.0.0/16"
AZURE_POD_CIDR="100.64.0.0/16"

UBIQUITI_PUBLIC_IP="203.0.113.10"
ONPREM_NODE_CIDR="10.20.0.0/16"
ONPREM_POD_CIDR="100.65.0.0/16"

VPN_GATEWAY_NAME="vpn-gw-azure"
VPN_PUBLIC_IP_NAME="pip-vpn-gw-azure"
LOCAL_GATEWAY_NAME="lgw-onprem"
VPN_CONNECTION_NAME="conn-onprem"
AZURE_GATEWAY_NODE_IP="10.10.1.10"
ONPREM_GATEWAY_NODE_IP="10.20.1.10"
```

Check that the VNet exists and confirm its address space. Adding a
`GatewaySubnet` is safe, but changing the VNet address space after the cluster
is running can break node networking.

```bash
az network vnet show \
  --resource-group "$CLUSTER_RESOURCE_GROUP" \
  --name "$CLUSTER_VNET_NAME" \
  --query "{name:name, prefixes:addressSpace.addressPrefixes, subnets:subnets[].name}" \
  --output json
```

### Create the GatewaySubnet

Azure reserves the name `GatewaySubnet` for the subnet that hosts the VPN
gateway virtual machines. The name is case-sensitive and must be exact. Use a
dedicated prefix that does not overlap any other subnet, and make it at least a
`/27`. Do not put workloads or other appliances in this subnet.

```bash
az network vnet subnet create \
  --resource-group "$CLUSTER_RESOURCE_GROUP" \
  --vnet-name "$CLUSTER_VNET_NAME" \
  --name GatewaySubnet \
  --address-prefixes "$GATEWAY_SUBNET_PREFIX"
```

If your organization requires a network security group on every subnet, attach
one that permits IKE and IPsec traffic from the Ubiquiti public IP. The gateway
itself must be able to reach Azure management endpoints, so do not block
outbound traffic from this subnet.

### Allocate a public IP for the gateway

The VPN gateway needs a Standard SKU, statically allocated public IP. A dynamic
address could change and break the router configuration, so static allocation
is the right choice here.

```bash
AZURE_VPN_IP=$(az network public-ip create \
  --resource-group "$CLUSTER_RESOURCE_GROUP" \
  --name "$VPN_PUBLIC_IP_NAME" \
  --location "$LOCATION" \
  --sku Standard \
  --allocation-method Static \
  --query ipAddress \
  --output tsv)

echo "Azure VPN public IP: $AZURE_VPN_IP"
```

Record this address. It is the remote gateway value in the Ubiquiti
configuration. If you create the public IP without retrieving it, you can always
read it later with `az network public-ip show`.

### Create the virtual network gateway

Create a route-based VPN gateway. `VpnGw1AZ` is an availability-zone-aware SKU
and is a reasonable starting point for one site-to-site connection. If your
region does not offer AZ SKUs, use `VpnGw1` instead. The gateway type is `Vpn`,
and the VPN type must be `RouteBased` because the local network gateway carries
a list of prefixes rather than a single policy selector.

```bash
az network vnet-gateway create \
  --resource-group "$CLUSTER_RESOURCE_GROUP" \
  --name "$VPN_GATEWAY_NAME" \
  --location "$LOCATION" \
  --vnet "$CLUSTER_VNET_NAME" \
  --gateway-type Vpn \
  --vpn-type RouteBased \
  --sku VpnGw1AZ \
  --public-ip-addresses "$VPN_PUBLIC_IP_NAME" \
  --no-wait
```

Provisioning is asynchronous and can take a while. The `--no-wait` flag returns
control immediately, so poll the provisioning state before you create the
connection:

```bash
until [ "$(az network vnet-gateway show \
  --resource-group "$CLUSTER_RESOURCE_GROUP" \
  --name "$VPN_GATEWAY_NAME" \
  --query provisioningState \
  --output tsv)" = "Succeeded" ]; do
  echo "Waiting for the Azure VPN gateway..."
  sleep 30
done
```

Do not create the VPN connection while the gateway is still provisioning. The
connection creation can fail or produce a connection in an error state that has
to be deleted and recreated.

## Add the on-premises network and connection

Azure needs an object that describes what sits behind the Ubiquiti router. That
object is the local network gateway. Its address prefixes become the routes that
Azure installs for the tunnel, and they also become the traffic selectors that
the VPN gateway expects to see from the on-premises side.

### Create the local network gateway

The address prefixes must include both the on-premises node CIDR and the
on-premises pod pool. If you list only the node CIDR, Azure will send traffic
for on-premises nodes into the tunnel and will send pod traffic to the internet
or drop it. The gateway address is the router's public IP.

```bash
az network local-gateway create \
  --resource-group "$CLUSTER_RESOURCE_GROUP" \
  --name "$LOCAL_GATEWAY_NAME" \
  --location "$LOCATION" \
  --gateway-address "$UBIQUITI_PUBLIC_IP" \
  --address-prefixes "$ONPREM_NODE_CIDR" "$ONPREM_POD_CIDR"
```

If you later add a pod pool or a node subnet on the on-premises side, update
this object. Azure does not discover those prefixes from the router, and the
tunnel will not carry a prefix that the local network gateway does not list.

### Create the IPsec connection

The connection binds the Azure virtual network gateway to the local network
gateway and carries the pre-shared key. Generate a long random key and store it
somewhere safe before you run the command. You will paste the same value into
the Ubiquiti configuration.

```bash
VPN_SHARED_KEY="$(openssl rand -base64 32)"
printf '%s\n' "$VPN_SHARED_KEY" > "$HOME/vpn-shared-key.txt"
chmod 600 "$HOME/vpn-shared-key.txt"

az network vpn-connection create \
  --resource-group "$CLUSTER_RESOURCE_GROUP" \
  --name "$VPN_CONNECTION_NAME" \
  --vnet-gateway1 "$VPN_GATEWAY_NAME" \
  --local-gateway2 "$LOCAL_GATEWAY_NAME" \
  --shared-key "$VPN_SHARED_KEY"
```

Keep the key file out of source control and delete it after the router is
configured. In a production environment, place the key in your secret manager
and inject it into the router configuration rather than leaving it on a
workstation.

### Enable forwarding on the Azure gateway node

The Azure VPN gateway delivers a packet destined for an Azure pod to the
`GatewaySubnet`, not to the pod's node. You must tell Azure to hand that packet
to the Azure gateway node, and you must tell the node's network interface to
accept traffic that is not addressed to its own IP. Both settings are required.
Without the route, Azure has no path from the gateway subnet into the pod CIDR.
Without IP forwarding on the NIC, Azure drops the packet at the node.

First, find the node resource group and the network interface that holds the
gateway node's internal address:

```bash
NODE_RESOURCE_GROUP=$(az aks show \
  --resource-group "$CLUSTER_RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --query nodeResourceGroup \
  --output tsv)

NODE_NIC=$(az network nic list \
  --resource-group "$NODE_RESOURCE_GROUP" \
  --query "[?ipConfigurations[0].privateIPAddress=='$AZURE_GATEWAY_NODE_IP'].name | [0]" \
  --output tsv)

az network nic update \
  --resource-group "$NODE_RESOURCE_GROUP" \
  --name "$NODE_NIC" \
  --ip-forwarding true
```

Then create a route table with one route for the Azure pod pool and associate it
with `GatewaySubnet`. The next hop is the gateway node's internal IP. This route
is what makes an on-premises-to-Azure pod connection work when the on-premises
side initiates it.

```bash
az network route-table create \
  --resource-group "$CLUSTER_RESOURCE_GROUP" \
  --name "rt-gateway-subnet" \
  --location "$LOCATION"

az network route-table route create \
  --resource-group "$CLUSTER_RESOURCE_GROUP" \
  --route-table-name "rt-gateway-subnet" \
  --name "to-azure-pods" \
  --address-prefix "$AZURE_POD_CIDR" \
  --next-hop-type VirtualAppliance \
  --next-hop-ip-address "$AZURE_GATEWAY_NODE_IP"

az network vnet subnet update \
  --resource-group "$CLUSTER_RESOURCE_GROUP" \
  --vnet-name "$CLUSTER_VNET_NAME" \
  --name GatewaySubnet \
  --route-table "rt-gateway-subnet"
```

{{< callout type="warning" >}}
Do not add a `0.0.0.0/0` route to `GatewaySubnet`. Azure does not support a
default route with a virtual appliance next hop on that subnet, and doing so can
stop the VPN gateway from functioning. The route table should contain specific
prefixes such as the pod pool, not a default route.
{{< /callout >}}

### Allow the on-premises ranges through the AKS network security group

The AKS node subnet usually has a network security group that allows traffic
from the VNet and from Azure load balancers but not from an arbitrary private
range. Add an inbound rule that permits the on-premises node and pod CIDRs. The
rule below allows all protocols and ports between the two private networks,
which keeps the first bring-up simple. Tighten it once you know which services
the sites actually use.

```bash
NODE_NSG=$(az network nsg list \
  --resource-group "$NODE_RESOURCE_GROUP" \
  --query "[0].name" \
  --output tsv)

az network nsg rule create \
  --resource-group "$NODE_RESOURCE_GROUP" \
  --nsg-name "$NODE_NSG" \
  --name "allow-onprem-l3" \
  --priority 100 \
  --direction Inbound \
  --access Allow \
  --protocol "*" \
  --source-address-prefixes "$ONPREM_NODE_CIDR" "$ONPREM_POD_CIDR" \
  --source-port-ranges "*" \
  --destination-address-prefixes "*" \
  --destination-port-ranges "*"
```

If priority 100 is already in use, choose another unused priority below 65000.
If the node resource group contains more than one network security group,
replace `[0]` with a query that selects the one attached to the node subnet.

If the cluster uses a restrictive egress policy, add the matching outbound rule
for the same prefixes. Also check whether a firewall between the VNet and the
VPN gateway inspects the traffic. A firewall that allows the tunnel but blocks
private transit between the node subnet and `GatewaySubnet` will produce the
same symptom as a broken VPN: the tunnel is up, but no pod traffic crosses.

## Configure the Ubiquiti router

The exact labels in the UniFi Network application change between releases, so
treat this section as a description of the settings rather than a click-by-click
script. The values you enter are the important part, and they must mirror the
Azure side.

### Build the site-to-site VPN

In the UniFi Network application, open the VPN settings and create a
site-to-site VPN. Choose IPsec and the manual or advanced mode used for a
third-party peer. The automatic UniFi-to-UniFi mode assumes a UniFi console on
both ends and does not describe the Azure gateway correctly.

Enter the Azure public IP you printed earlier as the remote gateway. Use the
pre-shared key you saved. Select IKEv2 and a policy that matches
Azure's defaults: AES-256 for encryption, SHA-256 for integrity, and
Diffie-Hellman group 14. Enable perfect forward secrecy with the same group. If
your router firmware defaults to a different proposal, change it on the router
rather than weakening the Azure side.

Define the local and remote networks explicitly:

```text
Local networks:   10.20.0.0/16   100.65.0.0/16
Remote networks:  10.10.0.0/16   100.64.0.0/16
```

The pod pools are not optional. They are the reason the two sites can exchange
pod traffic at all. If the router offers a choice between policy-based and
route-based mode, use route-based mode so that the local network gateway's
prefix list and the router's routing table stay in agreement.

If the router sits behind another NAT device, enable NAT traversal. Azure
expects IPsec traffic over UDP 500 and UDP 4500, and it must be able to receive
ESP packets from the router's public address.

### Add a route for the on-premises pod pool

The on-premises router must know that the on-premises pod pool lives behind the
on-premises gateway node, not on the LAN itself. Add a static route with
destination `100.65.0.0/16` and next hop `10.20.1.10` on the router's LAN
interface. Without this route, a reply from an Azure pod arrives at the router
and is dropped or sent to the default gateway, because the router has no reason
to associate the pod address with the gateway node.

The same logic applies in the other direction, but Azure expresses it with the
route table on `GatewaySubnet` that you created earlier. If you change the
on-premises gateway node address, update both the router's static route and the
Azure route table.

### Allow forwarded traffic

UniFi gateways forward traffic between interfaces by default, but the firewall
may still block traffic that enters through a VPN. Create rules that allow the
following traffic in both directions:

- The two node CIDRs to each other, for ICMP, TCP, and UDP.
- Each pod pool to the other pod pool and to the other node CIDR.
- UDP 9997 between all node addresses, for Unbounded health checks.
- IKE and IPsec traffic from the Azure VPN public IP to the router's public IP.

If you use the automatic VPN firewall rules in UniFi, verify that they cover
pod CIDRs and not only the LAN subnet. A rule that allows `10.20.0.0/16` to
`10.10.0.0/16` does not allow `100.65.0.0/16` to `100.64.0.0/16`.

### Verify the tunnel before involving Kubernetes

Bring the tunnel up and confirm it independently of Unbounded. From a host on
the on-premises LAN, ping the Azure gateway node's internal address. From a host
in the VNet, ping the on-premises gateway node's internal address. If either
ping fails, fix the VPN, the routes, or the firewall before you create the
`SitePeering`. Unbounded cannot repair a missing underlay route, and it will
program routes that appear correct while every packet disappears.

## Initialize the Sites and gateway pools

Unbounded needs two `Site` objects before it can route anything. The `Site`
object defines which node addresses belong to a location, which pod CIDRs the
location may allocate, and which optional components run there. The controller
labels nodes by matching their internal IPs against `spec.nodeCidrs`, so the
address plan from the previous section becomes the source of truth for Site
membership.

### Run site initialization

The `kubectl unbounded site init` command creates the conventional `cluster`
Site, the gateway pool used by the cluster, a site-to-gateway-pool assignment
for the cluster, the remote Site, and a site-to-gateway-pool assignment for the
remote Site. It also creates a bootstrap token for the remote location. Run it
once for the on-premises site, after the operator is installed:

```bash
kubectl unbounded site init \
  --name site-onprem \
  --cluster-node-cidr 10.10.0.0/16 \
  --cluster-pod-cidr 100.64.0.0/16 \
  --node-cidr 10.20.0.0/16 \
  --pod-cidr 100.65.0.0/16
```

The command requires all four CIDR flags, including the cluster CIDRs, even
when the cluster Site already exists. It reapplies the cluster templates, so
existing objects are updated rather than duplicated. If the cluster uses an
external CNI such as Cilium or Calico, add `--manage-cni-plugin=false`. The pod
CIDRs are still recorded because they define the inter-site address ranges.
Those ranges must match the pod CIDRs that the external CNI actually uses, or
the routes that Unbounded programs will point at the wrong addresses. Do not
use `None` with an external CNI unless that CNI or your underlay routes pod
CIDRs between nodes. Unbounded can manage same-site pod routes through the node
agent's `manageSameSitePodRoutes` setting without taking over CNI management.
The configuration reference explains that interaction in detail.

If the command fails because the operator is not installed, install it first
with `kubectl unbounded install`. The Site init handler checks API discovery and
refuses to continue when the required kinds are not served.

Verify the two Sites and the generated resources:

```bash
kubectl get site cluster site-onprem
kubectl get gatewaypool gw-main
kubectl get sitegatewaypoolassignment cluster site-onprem
```

At this point the Sites exist, but no node has joined the on-premises Site. Join
the on-premises machine using the bootstrap token or the machine workflow that
matches your environment. The details are outside the scope of this page; the
[agent guide]({{< relref "guides/agent" >}}) covers the host-side setup. When the
node is `Ready`, confirm that its internal address falls inside
`10.20.0.0/16` and that the controller labeled it:

```bash
kubectl get nodes -o wide
kubectl get nodes -L unbounded-cloud.io/site
```

The on-premises node should carry `unbounded-cloud.io/site=site-onprem`. Azure
nodes should carry `unbounded-cloud.io/site=cluster`. If a node carries no site
label, its internal IP does not match either Site's `nodeCidrs`; fix the address
or the Site before continuing.

### Create one internal pool per Site

`site init` creates `gw-main` with type `External`, the default when
`spec.type` is omitted, and points both Site assignments at it. An external
pool requires every gateway node to have an external IP, and before the Sites
are network-peered it also forces their cross-site links to WireGuard. Neither
fits this design: the gateways are reached through private addresses over the
VPN, and the point is to avoid encapsulation.

This guide uses a separate pool for each Site and joins them with a
`GatewayPoolPeering`. That keeps the cross-site gateway hop under an explicit
peering protocol. If both gateways shared one pool, there would be no
cross-pool link for the peering to govern, and the same-pool link would follow a
different scope. A dedicated pool per Site makes the intended protocol
unambiguous.

Start by narrowing `gw-main` to the cluster Site and switching it to internal
direct routing:

```bash
kubectl patch gatewaypool gw-main --type merge -p \
  '{"spec":{"type":"Internal","tunnelProtocol":"None","nodeSelector":{"unbounded-cloud.io/unbounded-net-gateway":"true","unbounded-cloud.io/site":"cluster"}}}'
```

Create the on-premises pool with the matching selector for its own Site:

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: GatewayPool
metadata:
  name: gw-onprem
spec:
  type: Internal
  tunnelProtocol: None
  nodeSelector:
    unbounded-cloud.io/unbounded-net-gateway: "true"
    unbounded-cloud.io/site: site-onprem
```

Apply the pool, then repoint the on-premises assignment and set both assignments
to `None`. The cluster assignment keeps `gw-main`; the on-premises assignment
moves from the shared pool to `gw-onprem`.

```bash
kubectl apply -f gatewaypool-onprem.yaml

kubectl patch sitegatewaypoolassignment cluster --type merge -p \
  '{"spec":{"gatewayPools":["gw-main"],"tunnelProtocol":"None"}}'

kubectl patch sitegatewaypoolassignment site-onprem --type merge -p \
  '{"spec":{"gatewayPools":["gw-onprem"],"tunnelProtocol":"None"}}'
```

Finally, declare that the two pools can exchange traffic directly and that the
cross-pool gateway link uses no encapsulation:

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: GatewayPoolPeering
metadata:
  name: azure-onprem-pools
spec:
  gatewayPools:
    - gw-main
    - gw-onprem
  tunnelProtocol: None
```

```bash
kubectl apply -f gatewaypoolpeering.yaml
```

{{< callout type="note" >}}
`kubectl unbounded site init` applies the generated pool and assignment
templates every time it runs. If you run it again to add another Site, it can
reset the `gw-main` selector and the on-premises assignment. Reapply the pool
patches, the assignment patches, and the pool peering after adding a Site.
{{< /callout >}}

An `Internal` pool accepts nodes with internal IPs and does not require public
addresses. The `SiteGatewayPoolAssignment` protocol governs the link between a
worker and its local gateway. The `GatewayPoolPeering` protocol governs the
link between the two gateways and takes precedence over the pool and Site
settings for that cross-pool hop. With `None` on the pools, the pool peering,
and both assignments, the whole cross-site path is routed rather than
encapsulated.

Leaving `Site.spec.tunnelProtocol` unset is intentional. That field governs
node-to-node links inside a single Site, and those links can keep their default
encapsulation. Only the cross-site path needs direct routing. If you also want
unencapsulated traffic inside each Site, set the Site protocol to `None` as
well, but make sure the nodes in that Site can route to each other directly
before you do.

### Label the gateway nodes

A gateway node forwards traffic for its Site and participates in
gateway-to-gateway links. Choose a node with enough CPU and network headroom,
because it carries transit traffic for the Site. The pool selectors already
require the Site label, so the same gateway label can be used on both nodes
without making them eligible for each other's pool.

```bash
kubectl label node <azure-gateway-node> \
  unbounded-cloud.io/unbounded-net-gateway=true

kubectl label node <onprem-gateway-node> \
  unbounded-cloud.io/unbounded-net-gateway=true
```

The gateway pool controller watches nodes and selects the lexicographically
first matching pool for each node. Because each pool selector includes a
different Site label, the Azure node joins `gw-main` and the on-premises node
joins `gw-onprem`. The controller then writes the
`net.unbounded-cloud.io/gateway-pool` label with the matching pool name. The
node agent adopts the assignment, takes the gateway role, and applies the
`net.unbounded-cloud.io/gateway-node=true:NoSchedule` taint. Ordinary workloads
will no longer schedule on the node unless they tolerate that taint.

A gateway node must also have published its WireGuard public key, even when the
link uses `None`. The pool controller requires the key annotation as proof that
the node agent is running and has initialized its networking. If a labeled node
never appears in a pool, check that the node agent Pod is ready and that the
annotation `net.unbounded-cloud.io/wg-pubkey` is present on the node.

## Create the SitePeering with tunnel protocol None

The `SitePeering` is the object that tells Unbounded these two Sites already
share a private Layer 3 path. It does two things. It marks the Sites as
network-peered, so the controller stops treating their addresses as public and
stops forcing WireGuard for the link. It also supplies a protocol for direct
node-to-node links between the Sites when a full mesh is enabled. The protocol
you want is `None`.

```yaml
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SitePeering
metadata:
  name: azure-onprem-l3
spec:
  sites:
    - cluster
    - site-onprem
  meshNodes: false
  tunnelProtocol: None
```

Apply the manifest and inspect the result:

```bash
kubectl apply -f sitepeering.yaml
kubectl get sitepeering azure-onprem-l3 -o yaml
```

The status reports `peeredSiteCount: 2` and a `totalNodeCount` once both Sites
exist and the controller has reconciled the peering. The status is
informational; the data path is built by the node agents from the spec.

With `meshNodes: false`, the node agents do not create direct node-to-node peers
between the two Sites. That is why the `GatewayPoolPeering` matters: it is the
object that gives the cross-pool gateway link its `None` protocol. The
`SitePeering`'s `None` is still the correct setting because it describes the
site-to-site relationship and governs the direct links if you enable a full
mesh. Setting both keeps the intent and the data path aligned.

### Why meshNodes is false

With `meshNodes: true`, every node in the peering creates a direct peer with
every node in the other Site. That is attractive for a small pair of Sites on a
fast link, because traffic bypasses gateways entirely and `SitePeering`'s
`tunnelProtocol: None` governs every cross-site link. The cost is a full mesh:
the number of peers grows with the product of the node counts, and every node
must be able to reach every other node's internal address over the VPN.

This guide uses `meshNodes: false`, which is the better default for a WAN and
for Sites of different sizes. The Sites are still network-peered, but nodes do
not mesh directly. Traffic continues to use the gateway pools, and the gateway
links resolve to internal addresses because the Sites are peered. The
`SitePeering` protocol still records the intent, and the pool peering and
assignment protocols make the actual gateway hops direct. In this gateway-based
design, the `GatewayPoolPeering` is the object that governs the WAN hop.

If you later switch to `meshNodes: true`, set `tunnelProtocol: None` on the
`SitePeering` and verify that the VPN carries every node CIDR in both Sites.
Gateway pools are bypassed for the peered Sites, so a missing underlay route
between two nodes becomes a black hole that no gateway can repair.

## Verify the data path

Verification has three levels. First confirm that the controller has accepted
the objects and selected one gateway per Site. Then confirm that the node agents
resolved the links to internal addresses with protocol `None`. Finally, send
real pod traffic in both directions. Do not skip the second level: a peering
object can exist and a gateway pool can look healthy while the agents still use
WireGuard or a public endpoint.

### Check the Unbounded objects

Start with the Sites, the peering, and the pool status. Each pool status lists
the gateway nodes that were selected, the Sites connected through that pool, and
the Sites reachable through its connections.

```bash
kubectl get site cluster site-onprem
kubectl get sitepeering azure-onprem-l3
kubectl get gatewaypool gw-main gw-onprem -o yaml
kubectl get gatewaypoolpeering azure-onprem-pools
kubectl get gatewaypoolnode
```

Each pool should report one node in its own Site. `gw-main` should contain the
Azure gateway node in `cluster`, and `gw-onprem` should contain the on-premises
gateway node in `site-onprem`. Each node should have an internal IP in its Site
CIDR, a pod CIDR, and a WireGuard public key. Because the `SitePeering` declares
all of its Sites connected for every pool referenced by their assignments, both
pools will usually list both `cluster` and `site-onprem` in `connectedSites` and
in `reachableSites`. That is expected and does not mean a node joined the wrong
pool. If a pool reports zero nodes, return to the labeling step before you debug
the tunnel.

You can print the selected gateways compactly:

```bash
kubectl get gatewaypools -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .status.nodes[*]}{.name}{"/"}{.siteName}{" "}{end}{"\n"}{end}'
```

### Inspect the peers

The `kubectl-unbounded` plugin reads the controller's status service and shows
the peers that a node agent actually built. Run it against the Azure gateway
node first.

```bash
kubectl unbounded net node show <azure-gateway-node> peers
```

The peer table includes the destination node, peer type, Site, pool, protocol,
Kubernetes status, health, endpoint, allowed IPs, and route count. You are
looking for the on-premises gateway peer. Its protocol must be `None`, its
endpoint must be an internal address such as `10.20.1.10`, and its health should
be up.

An endpoint that shows the Ubiquiti public IP is a sign that the Sites are not
being treated as network-peered, or that one of the pools is still `External`.
A protocol of `WireGuard`, `GENEVE`, or `Auto` means the protocol overrides did
not reach the scope that governs that link. Check both pools, the pool peering,
and the assignments again.

Repeat the command for the on-premises gateway. Both ends must agree. If one end
shows `None` and the other shows an encapsulated protocol, the two gateways will
not interpret the packets the same way, and traffic will fail even though each
peer reports itself as healthy.

### Inspect the routes

The route view shows each kernel route with its address family, destination,
route kind, device, gateway, distance, MTU, and whether an expected route is
present. The eBPF dataplane keeps a supernet route for each remote prefix on the
`unbounded0` dummy device. That route attracts traffic into the TC program, and
the program looks up the real next hop in the BPF map.

```bash
kubectl unbounded net node show <azure-gateway-node> routes
```

Look for the on-premises pod pool, `100.65.0.0/16`, and confirm that it is
present on `unbounded0`. Do not expect the kernel route to name the remote
gateway: with this dataplane, the peer-specific next hop lives in the BPF map,
not in the route's gateway field.

Inspect the BPF entries to see the peer and protocol that the prefix resolves
to:

```bash
kubectl unbounded net node show <azure-gateway-node> bpf
```

The BPF table has columns for the CIDR, remote endpoint, node, interface,
protocol, VNI, and MTU. For the on-premises pod pool, you want the remote
endpoint to be the on-premises gateway's internal address, such as
`10.20.1.10`, and the protocol to be `None`. The interface for a `None` entry is
the node's normal underlay interface, because the BPF program redirects the
packet there without a tunnel key. The same view on the on-premises gateway
should show `100.64.0.0/16` with remote `10.10.1.10` and protocol `None`.

You can confirm the kernel view directly. The node agent runs in the host
network namespace, so `ip route` from the agent container shows the host routes.

```bash
kubectl unbounded net node exec <azure-gateway-node> -- ip route show
```

You should see a supernet route similar to the following, with your own prefix:

```text
100.65.0.0/16 dev unbounded0 scope global
```

If the prefix is missing from both the route and BPF views, the gateway peer did
not advertise it or the peering did not build. If the BPF entry uses a tunnel
interface such as `geneve0`, the protocol was not resolved to `None`. If the
prefix and BPF entry exist but traffic still fails, the problem is below
Unbounded, in the VPN, the cloud route table, or the router.

### Test pod traffic in both directions

Create one pod in each Site. The node selector pins each pod to a Site label so
that the test does not accidentally run both pods in the same location. Use a
worker node, not a gateway node, because the gateway node is tainted.

```bash
kubectl run nettest-azure --image=busybox:1.36 --restart=Never \
  --overrides='{"spec":{"nodeSelector":{"unbounded-cloud.io/site":"cluster"}}}' \
  -- sleep 3600

kubectl run nettest-onprem --image=busybox:1.36 --restart=Never \
  --overrides='{"spec":{"nodeSelector":{"unbounded-cloud.io/site":"site-onprem"}}}' \
  -- sleep 3600
```

Wait for both pods to be running, then read their addresses and ping in each
direction:

```bash
AZ_POD=$(kubectl get pod nettest-azure -o jsonpath='{.status.podIP}')
ONPREM_POD=$(kubectl get pod nettest-onprem -o jsonpath='{.status.podIP}')

kubectl exec nettest-azure -- ping -c 3 -w 5 "$ONPREM_POD"
kubectl exec nettest-onprem -- ping -c 3 -w 5 "$AZ_POD"
```

Both commands must succeed. A test that passes in one direction and fails in the
other points to an asymmetric route, which is almost always a missing route
table entry or a missing static route rather than a broken peering. The next
section walks through that case.

Clean up the test pods when you are done:

```bash
kubectl delete pod nettest-azure nettest-onprem
```

## Troubleshooting

Most failures in this design fall into one of a few layers: the IPsec tunnel,
the Azure route table, the on-premises static route, the gateway pool selection,
or the tunnel protocol resolution. Work from the outside in. Confirm the tunnel,
then the node-to-node path, then the gateway pools, then the pod path. Changing
several layers at once makes it hard to tell which change fixed or broke the
system.

### The tunnel is up but nodes cannot reach each other

Ping the gateway nodes' internal addresses from the opposite side before you
look at Unbounded. If that fails, the underlay is not carrying private traffic.
Check the IPsec status on the router, the traffic selectors on both ends, and the
Azure local network gateway prefixes. The selectors must include both node CIDRs
and both pod CIDRs, in both directions.

On Azure, confirm that the `GatewaySubnet` route table contains the route for
the Azure pod pool and that the gateway node's NIC has IP forwarding enabled.
Without IP forwarding, Azure delivers the packet to the node's NIC and drops it
because the destination is not the NIC's own address. On the gateway node
itself, confirm that the guest forwards IPv4. The node agent checks
`/proc/sys/net/ipv4/ip_forward` and reports a `configIPv4ForwardingDisabled`
node error when it is off; enable forwarding on the host if needed. On the
on-premises side, confirm that the router has a static route for the
on-premises pod pool through the gateway node, and that the firewall permits
forwarded traffic between the VPN interface and the LAN.

Also check for NAT. If the router translates the on-premises node addresses
before they enter the tunnel, Azure sees a different source address than the
one configured in the local network gateway, and the return path may not exist.
Site-to-site IPsec should carry the private addresses without source NAT.

### A labeled node never appears in the gateway pool

Start with the node agent. The pool controller only admits a node that matches
the `nodeSelector`, has the WireGuard public key annotation, and satisfies the
pool type's address requirement. An `External` pool requires an external IP; an
`Internal` pool requires an internal or external IP. If a pool is still
`External`, an AKS node with only an internal address will never be selected.
A missing or incorrect Site label has the same effect, because each pool
selector requires the label for its own Site.

Check the node's labels and annotations directly:

```bash
kubectl get node <node> --show-labels
kubectl get node <node> -o jsonpath='{.metadata.annotations.net\.unbounded-cloud\.io/wg-pubkey}{"\n"}'
```

If the annotation is empty, the node agent has not finished initializing. Check
the agent Pod on that node and its logs before you change any pool setting. If
the label is present but the pool still shows no node, look at the controller
logs for a selector or eligibility message.

### Peers use the wrong protocol or a public endpoint

The protocol resolution hierarchy is specific, and a setting at the wrong scope
has no effect. For the cross-site gateway hop in this guide, the governing
scopes are `GatewayPoolPeering.spec.tunnelProtocol` for the link between the two
pools, `GatewayPool.spec.tunnelProtocol` for same-pool gateways, and
`SiteGatewayPoolAssignment.spec.tunnelProtocol` for worker-to-gateway links. The
`SitePeering` marks the Sites as network-peered, which is what allows internal
addresses to be used, but with `meshNodes: false` it does not by itself set the
protocol on the gateway hop.

Verify the relevant objects:

```bash
kubectl get sitepeering azure-onprem-l3 -o yaml
kubectl get gatewaypool gw-main gw-onprem -o yaml
kubectl get gatewaypoolpeering azure-onprem-pools -o yaml
kubectl get sitegatewaypoolassignment cluster site-onprem -o yaml
```

The Site names in the peering must match the actual `Site` object names exactly.
A typo produces a peering that references a nonexistent Site, and the Sites are
not network-peered. Both pools must be `Internal`, the pool peering must name
both pools, and the protocol fields must be `None` rather than `Auto` or an
empty value. Set these fields explicitly, because an unset or `Auto` value
resolves from the link's addresses and may select an encapsulation you did not
intend.

### Pod traffic works in one direction only

Asymmetric routing is the most common cause. The forward packet takes one path
and the reply takes another, or the reply has no route at all. Trace both
directions explicitly.

For Azure-to-on-premises traffic, the Azure gateway node needs a route to the
on-premises pod pool through the on-premises gateway node, and the on-premises
router needs a route for that same pod pool through the on-premises gateway
node. For on-premises-to-Azure traffic, the on-premises gateway node needs a
route to the Azure pod pool through the Azure gateway node, and Azure needs the
`GatewaySubnet` route table entry that sends the Azure pod pool to the Azure
gateway node.

Use `ip route get <pod-ip>` on a gateway node to confirm that the destination
resolves into the dataplane, then inspect the BPF table to confirm the peer and
protocol. The kernel route points at `unbounded0`; the BPF program performs the
per-destination lookup, so the remote gateway does not appear in the route's
next-hop field.

```bash
kubectl unbounded net node exec <gateway-node> -- ip route get <remote-pod-ip>
kubectl unbounded net node show <gateway-node> bpf
```

If the address does not resolve to `unbounded0`, the supernet route is missing
or a more specific route is sending it elsewhere. If the BPF entry has the wrong
remote endpoint or protocol, fix the peering or the pool protocol. If the entry
is correct but packets never appear on the router, capture packets on both
gateway nodes and on the router. A packet that appears on the sending gateway
and on the router but not on the receiving gateway is being dropped by the
tunnel policy or the firewall.

### Nodes respond but pods do not

If pinging a node's internal IP works and pinging a pod does not, the problem is
in the pod CIDR advertisement or in the CNI. Check that both Sites declare the
pod pools you expect, that the gateway nodes advertise them, and that the IPsec
selectors include them.

```bash
kubectl get site cluster site-onprem -o yaml
kubectl get gatewaypoolnode -o yaml
```

The `GatewayPoolNode.status.routes` map should contain entries for the pod
prefixes, not only for node prefixes. If it does not, the Site's
`podCidrAssignments` may not include the pool, or the gateway node has not
published its route advertisement yet.

If you use an external CNI with `manageCniPlugin: false`, Unbounded does not
install same-site pod routes for you. The CNI must route pod traffic to the local
node, or you must set `node.manageSameSitePodRoutes: true` in the node agent
configuration. Without that, a packet can reach the correct gateway and still
fail to reach the pod because the final hop inside the Site is missing.

### Large packets stall while small packets work

IPsec adds overhead, and a path that passes a 64-byte ping can still fail for a
1500-byte packet. Test with a payload size that approaches the path MTU and with
the do-not-fragment bit set:

```bash
# On a Linux host with iputils ping, using an address the host can reach:
ping -M do -s 1400 <azure-pod-ip>
```

If this fails while the default ping succeeds, reduce the MTU on the pod
interface or enable TCP MSS clamping on the routers. BusyBox `ping` does not
support `-M`, so run this test from a Linux host or a pod image that includes
the full `iputils` version. `spec.tunnelMTU` on the Unbounded objects controls
routes that use a tunnel; it does not raise the MTU of the IPsec underlay. With
`None`, the underlay MTU and the CNI MTU are the values that matter. The
networking configuration reference covers the MTU settings and the recommended
headroom.

### Health checks flap or stay down

Unbounded health checks use UDP probes and default to a 15-second transmit and
receive interval with a detect multiplier of 3, so a peer can take about 45
seconds to be marked down after the path fails. A peer that flaps every few
minutes usually has a firewall that drops the probes or a path that is dropping
a fraction of UDP traffic.

Allow UDP 9997 between all node addresses in both Sites. Confirm that the return
path for the probe is symmetric. Do not disable health checks as a permanent
fix; disabling them hides a broken underlay and prevents route failover from
working correctly.

### The plugin cannot load node details

The `kubectl unbounded net node show` command reads the controller status
service. If it reports that details are unavailable or expired, check that the
aggregated API service is healthy and that the controller Pod is running:

```bash
kubectl get apiservice v1alpha1.status.net.unbounded-cloud.io
kubectl -n unbounded-system get pods -l app.kubernetes.io/name=unbounded-net-controller
```

The plugin also has a `--refresh` option on `node show` that requests fresh data
from a node instead of returning the cached snapshot. Use it when a node has just
changed state and the summary still shows the old value.

## Security and operational notes

`None` means no confidentiality at the Unbounded layer. The IPsec tunnel
protects traffic on the public WAN, but traffic inside the Azure VNet and inside
the on-premises LAN is plain IP. Restrict the network security group rules and
the router firewall to the specific node and pod CIDRs. Do not open the Azure
node subnet to all private addresses just because the VPN is private.

The pre-shared key is a long-lived secret that authenticates both ends of the
tunnel. Store it in a secret manager, restrict who can read the router
configuration, and rotate it on a schedule. Rotating it requires updating the
Azure VPN connection and the Ubiquiti configuration together; plan for a brief
tunnel interruption rather than changing only one side.

Gateway nodes carry transit traffic for their Site and are tainted so that
ordinary workloads do not compete with forwarding. Monitor their CPU, network
throughput, and conntrack usage. A single gateway is a single point of failure
for cross-site traffic; if you need redundancy, add a second gateway node to the
pool in each Site and confirm that the pool advertises both. Unbounded builds
ECMP nexthop groups for healthy gateways, so a second gateway can carry traffic
when the first fails.

When you change the address plan, update every layer in the same order. Update
the Site objects, the Azure local network gateway prefixes for on-premises
pools, the Ubiquiti IPsec selectors and static routes, the `GatewaySubnet` route
table for Azure pools, and the Azure node network security group. Pods that
receive addresses from a pool before the underlay knows about it will fail to
communicate even though every Kubernetes object looks correct.

## Related

- [Networking concepts]({{< relref "concepts/networking" >}}) explains the
  peering model, the protocol hierarchy, and the difference between overlay and
  direct paths.
- [Networking custom resources]({{< relref "reference/networking/custom-resources" >}})
  is the field reference for `Site`, `SitePeering`, `GatewayPool`, and
  `SiteGatewayPoolAssignment`.
- [Networking configuration]({{< relref "reference/networking/configuration" >}})
  covers MTU guidance, health check tuning, and node agent settings.
- [Networking operations]({{< relref "reference/networking/operations" >}})
  covers day-two monitoring, gateway replacement, and diagnostic commands.
- [Networking architecture]({{< relref "reference/networking/architecture" >}})
  describes the dataplane and how routes are programmed.
- [Agent guide]({{< relref "guides/agent" >}}) covers joining the on-premises
  machine to the cluster.

