# 300-Node AKS Cluster Playbook

This playbook creates an AKS cluster with 300 nodes in a single system pool, or
expands an existing system pool to 300 nodes. It uses supported AKS node-pool
operations.

This playbook assumes the target subscription already has sufficient regional
and VM-family capacity. It does not run quota preflight checks: the `az vm
list-usage` and `az vm list-skus` calls used for that are slow and frequently
stall. If a pool creation fails for capacity reasons, Azure reports it directly.

The validated layout is:

| Pool | Mode | Nodes | Default VM size | Purpose |
|---|---|---:|---|---|
| `system` | System | 300 | `Standard_D8ds_v6` | All Kubernetes and application workloads |

Each `Standard_D8ds_v6` node has 8 vCPUs and 32 GiB of memory.

## Gantry benchmark topology

The Gantry benchmark counts every Ready, schedulable `linux/amd64` node as a
target, and `enable` fails unless that count matches `BENCHMARK_NODE_COUNT`
exactly. Node taints are *not* excluded from that count, and both the benchmark
Job and the Gantry DaemonSet tolerate everything, so a dedicated pool stays
consistent across all three checks.

To keep Prometheus and Grafana off the measured worker nodes while still
totalling 300 eligible nodes, split the cluster:

| Pool | Mode | Nodes | VM size | Notes |
|---|---|---:|---|---|
| `system` | System | 298 | `Standard_D8ds_v6` | Benchmark workers |
| `bench` | User | 2 | `Standard_D16ds_v6` | Labelled and tainted `gantry-benchmark-proxy=true`; hosts monitoring |

Set `BENCHMARK_NODE_COUNT=300` for that layout.

## Design constraints

- Use Azure CNI Overlay. Each node receives a fixed `/24` pod address block and
  supports up to 250 pods.
- Use at least a `/15` pod CIDR. A `/16` contains only 256 `/24` blocks and
  cannot support 300 nodes. A `/15` contains 512 blocks, leaving room for
  upgrade surge nodes.
- Scale the system pool through `az aks nodepool scale`. Do not modify the
  AKS-managed VM scale set directly. Direct VMSS changes bypass AKS
  reconciliation.
- Treat 300-node clusters as capacity-sensitive. Available quota does not
  guarantee that Azure has 300 instances of a SKU available at a particular
  moment.

## Prerequisites

The operator needs:

- Azure CLI with the `aks-preview` extension removed or updated if it overrides
  stable AKS commands.
- `kubectl` and `jq`.
- Permission to create resource groups and AKS clusters, or to update the
  target AKS cluster and its node pools.
- Sufficient regional and VM-family capacity for the pool being created or
  scaled. This playbook does not verify it.
- A dedicated `KUBECONFIG` path.

Use a Standard AKS tier for sustained or production use. Free tier can support
the node count but does not provide the same control-plane service level.

## Set variables

Set values for the target subscription and cluster. The defaults reproduce the
validated single-pool 300-node topology without embedding generated resource
names.

```bash
set -euo pipefail

: "${SUBSCRIPTION_ID:?Set SUBSCRIPTION_ID}"
: "${KUBECONFIG:?Set KUBECONFIG to a dedicated file}"
export KUBECONFIG

LOCATION="${LOCATION:-canadacentral}"
RESOURCE_GROUP="${RESOURCE_GROUP:-rg-aks-300}"
CLUSTER_NAME="${CLUSTER_NAME:-aks-300}"
KUBERNETES_VERSION="${KUBERNETES_VERSION:-1.35}"
AKS_TIER="${AKS_TIER:-standard}"

SYSTEM_POOL="${SYSTEM_POOL:-system}"
SYSTEM_NODE_COUNT=300
TARGET_NODE_COUNT=300

NODE_VM_SIZE="${NODE_VM_SIZE:-Standard_D8ds_v6}"
MAX_PODS=250
OS_DISK_SIZE_GB=128

POD_CIDR="${POD_CIDR:-10.244.0.0/15}"
SERVICE_CIDR="${SERVICE_CIDR:-10.0.0.0/16}"
DNS_SERVICE_IP="${DNS_SERVICE_IP:-10.0.0.10}"

VNET_NAME="${VNET_NAME:-vapa-gantry-bench-vnet}"
VNET_CIDR="${VNET_CIDR:-10.224.0.0/15}"
NODE_SUBNET="${NODE_SUBNET:-aks-nodes}"
NODE_SUBNET_CIDR="${NODE_SUBNET_CIDR:-10.224.0.0/16}"
PRIVATE_ENDPOINT_SUBNET="${PRIVATE_ENDPOINT_SUBNET:-acr-private-endpoints}"
PRIVATE_ENDPOINT_SUBNET_CIDR="${PRIVATE_ENDPOINT_SUBNET_CIDR:-10.225.0.0/27}"

if (( SYSTEM_NODE_COUNT != TARGET_NODE_COUNT )); then
  echo "System pool count must equal ${TARGET_NODE_COUNT}" >&2
  exit 1
fi

az account set --subscription "$SUBSCRIPTION_ID"
```

Before continuing, confirm that the selected Kubernetes version is offered in
the region:

```bash
az aks get-versions \
  --location "$LOCATION" \
  --query 'values[].version' \
  -o table
```

## Create a new cluster

Create the resource group and the 300-node system pool. Supplying the `/15` pod
CIDR at creation avoids a later network-profile update.

```bash
az group create \
  --name "$RESOURCE_GROUP" \
  --location "$LOCATION" \
  --only-show-errors

az network vnet create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$VNET_NAME" \
  --location "$LOCATION" \
  --address-prefixes "$VNET_CIDR" \
  --subnet-name "$NODE_SUBNET" \
  --subnet-prefixes "$NODE_SUBNET_CIDR" \
  --only-show-errors

az network vnet subnet create \
  --resource-group "$RESOURCE_GROUP" \
  --vnet-name "$VNET_NAME" \
  --name "$PRIVATE_ENDPOINT_SUBNET" \
  --address-prefixes "$PRIVATE_ENDPOINT_SUBNET_CIDR" \
  --disable-private-endpoint-network-policies true \
  --only-show-errors

NODE_SUBNET_ID=$(az network vnet subnet show \
  --resource-group "$RESOURCE_GROUP" \
  --vnet-name "$VNET_NAME" \
  --name "$NODE_SUBNET" \
  --query id \
  --output tsv)

az aks create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --location "$LOCATION" \
  --kubernetes-version "$KUBERNETES_VERSION" \
  --tier "$AKS_TIER" \
  --nodepool-name "$SYSTEM_POOL" \
  --node-count "$SYSTEM_NODE_COUNT" \
  --node-vm-size "$NODE_VM_SIZE" \
  --max-pods "$MAX_PODS" \
  --node-osdisk-size "$OS_DISK_SIZE_GB" \
  --node-osdisk-type Managed \
  --network-plugin azure \
  --network-plugin-mode overlay \
  --vnet-subnet-id "$NODE_SUBNET_ID" \
  --pod-cidr "$POD_CIDR" \
  --service-cidr "$SERVICE_CIDR" \
  --dns-service-ip "$DNS_SERVICE_IP" \
  --generate-ssh-keys \
  --only-show-errors
```

Load credentials into the dedicated kubeconfig and verify the nodes:

```bash
az aks get-credentials \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --file "$KUBECONFIG" \
  --overwrite-existing

kubectl get nodes -l agentpool="$SYSTEM_POOL"
```

Provisioning 300 nodes in one create can take a while because AKS waits for all
VMSS instances and nodes. If Azure reports regional capacity pressure, you can
create the cluster with a smaller `--node-count` and then scale the same system
pool up to 300:

```bash
az aks nodepool scale \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --name "$SYSTEM_POOL" \
  --node-count "$TARGET_NODE_COUNT" \
  --only-show-errors
```

Do not run `az vmss create`, `az vmss update`, or `az vmss scale` against the
node resource group. Direct VMSS changes bypass AKS reconciliation.

## Expand an existing cluster

Use this path only when the target cluster already exists. First verify the
current topology and network profile:

```bash
az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --query '{state:provisioningState,podCidrs:networkProfile.podCidrs,networkPlugin:networkProfile.networkPlugin,networkPluginMode:networkProfile.networkPluginMode}' \
  -o json

az aks nodepool list \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --query '[].{name:name,mode:mode,count:count,vmSize:vmSize,state:provisioningState}' \
  -o table
```

The cluster must use Azure CNI Overlay. If its pod CIDR is `/16`, expand it to
a non-overlapping `/15` before adding nodes. Pod CIDRs can be expanded but must
not be shrunk.

```bash
az aks update \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --pod-cidr "$POD_CIDR" \
  --yes \
  --only-show-errors
```

Then scale the system pool to 300 nodes. Do not run `az vmss create`,
`az vmss update`, or `az vmss scale` against the node resource group.

```bash
az aks nodepool scale \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --name "$SYSTEM_POOL" \
  --node-count "$TARGET_NODE_COUNT" \
  --only-show-errors
```

If the existing cluster hosted a node-mutating DaemonSet, clean its host state
before scaling. Gantry, for example, can write containerd registry mirror files
under `/etc/containerd/certs.d` and peer state under `/var/lib/gantry`. Deleting
its namespace removes Kubernetes objects but does not remove those host files.
Use a narrowly scoped cleanup DaemonSet against the existing nodes, verify the
owned paths are absent on every node, and delete the cleanup DaemonSet before
scaling the system pool.

## Validate the result

Wait until AKS reports the system pool as succeeded:

```bash
az aks nodepool list \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --query '[].{name:name,mode:mode,count:count,vmSize:vmSize,state:provisioningState,power:powerState.code}' \
  -o table
```

Require exactly 300 registered and Ready nodes:

```bash
NODE_STATUS=$(kubectl get nodes -o json | jq '{
  total: (.items | length),
  ready: ([.items[] | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))] | length),
  pools: ([.items[].metadata.labels.agentpool] | group_by(.) | map({pool: .[0], count: length}))
}')

jq . <<<"$NODE_STATUS"

TOTAL_NODES=$(jq -r '.total' <<<"$NODE_STATUS")
READY_NODES=$(jq -r '.ready' <<<"$NODE_STATUS")

if (( TOTAL_NODES != TARGET_NODE_COUNT || READY_NODES != TARGET_NODE_COUNT )); then
  echo "Expected ${TARGET_NODE_COUNT} Ready nodes" >&2
  exit 1
fi
```

Check cluster workloads and recent warning events:

```bash
kubectl get pods -A
kubectl get events -A --field-selector type=Warning --sort-by=.lastTimestamp
```

Finally, verify the AKS-managed scale sets without modifying them:

```bash
NODE_RESOURCE_GROUP=$(az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --query nodeResourceGroup \
  -o tsv)

az vmss list \
  --resource-group "$NODE_RESOURCE_GROUP" \
  --query '[].{name:name,sku:sku.name,capacity:sku.capacity,state:provisioningState}' \
  -o table
```

Expected result:

- The system pool has 300 nodes.
- All 300 Kubernetes nodes report `Ready`.
- The AKS node pool and its managed VM scale set report `Succeeded`.

## Roll back the system pool

A cluster must keep at least one system pool, so scale the pool down rather than
deleting it. Drain or relocate workloads first, then scale through AKS:

```bash
az aks nodepool scale \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --name "$SYSTEM_POOL" \
  --node-count 10 \
  --only-show-errors
```

Do not scale the AKS-managed VM scale set directly.