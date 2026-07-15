# 300-Node AKS Cluster Playbook

This playbook creates an AKS cluster with 300 nodes or expands an existing
10-node cluster to 300 nodes. It uses supported AKS node-pool operations and
checks regional SKU availability, quota, and network capacity before creating
the large user pool.

The validated layout is:

| Pool | Mode | Nodes | Default VM size | Purpose |
|---|---|---:|---|---|
| `system` | System | 10 | `Standard_D8ds_v6` | Kubernetes system workloads |
| `worker` | User | 290 | `Standard_D8ds_v6` | Application workloads |

Each `Standard_D8ds_v6` node has 8 vCPUs and 32 GiB of memory. For an existing
cluster, the system pool may use a different VM family; calculate quota demand
for every pool being added or scaled.

## Design constraints

- Use Azure CNI Overlay. Each node receives a fixed `/24` pod address block and
  supports up to 250 pods.
- Use at least a `/15` pod CIDR. A `/16` contains only 256 `/24` blocks and
  cannot support 300 nodes. A `/15` contains 512 blocks, leaving room for
  upgrade surge nodes.
- Check the quota family reported for the exact VM SKU. Similar names can use
  different quota families. `Standard_D8ds_v6` uses
  `StandardDdsv6Family`.
- Scale pools through `az aks nodepool add` or `az aks nodepool scale`. Do not
  modify the AKS-managed VM scale set directly. Direct VMSS changes bypass AKS
  reconciliation and do not bypass family quota.
- Treat 300-node clusters as capacity-sensitive. A quota limit does not
  guarantee that Azure has 290 instances of a SKU available at a particular
  moment.

## Prerequisites

The operator needs:

- Azure CLI with the `aks-preview` extension removed or updated if it overrides
  stable AKS commands.
- `kubectl` and `jq`.
- Permission to create resource groups and AKS clusters, or to update the
  target AKS cluster and its node pools.
- At least 2,400 free vCPUs in the selected eight-vCPU VM family for a fresh
  cluster. Expanding an existing 10-node cluster requires 2,320 free vCPUs for
  the 290-node worker pool.
- At least 300 free entries in the regional Virtual Machines quota.
- A dedicated `KUBECONFIG` path.

Use a Standard AKS tier for sustained or production use. Free tier can support
the node count but does not provide the same control-plane service level.

## Set variables

Set values for the target subscription and cluster. The defaults reproduce the
validated 10+290 topology without embedding generated resource names.

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
WORKER_POOL="${WORKER_POOL:-worker}"
SYSTEM_NODE_COUNT=10
WORKER_NODE_COUNT=290
TARGET_NODE_COUNT=300

NODE_VM_SIZE="${NODE_VM_SIZE:-Standard_D8ds_v6}"
MAX_PODS=250
OS_DISK_SIZE_GB=128

POD_CIDR="${POD_CIDR:-10.244.0.0/15}"
SERVICE_CIDR="${SERVICE_CIDR:-10.0.0.0/16}"
DNS_SERVICE_IP="${DNS_SERVICE_IP:-10.0.0.10}"

if (( SYSTEM_NODE_COUNT + WORKER_NODE_COUNT != TARGET_NODE_COUNT )); then
  echo "Pool counts do not add up to ${TARGET_NODE_COUNT}" >&2
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

## Check SKU and quota

Resolve the quota family and vCPU count from the exact SKU. This avoids
assuming that similarly named VM sizes consume the same family quota.

For a fresh cluster, the quota check covers all 300 nodes. When expanding an
existing 10-node cluster, set `QUOTA_NODE_COUNT` to the 290 nodes being added
before running this section:

```bash
QUOTA_NODE_COUNT="${QUOTA_NODE_COUNT:-$TARGET_NODE_COUNT}"
```

```bash
SKU_JSON=$(az vm list-skus \
  --location "$LOCATION" \
  --size "$NODE_VM_SIZE" \
  --resource-type virtualMachines \
  -o json)

SKU_FAMILY=$(jq -r '.[0].family // empty' <<<"$SKU_JSON")
VCPUS_PER_NODE=$(jq -r \
  '.[0].capabilities[] | select(.name == "vCPUs") | .value' \
  <<<"$SKU_JSON")
SKU_RESTRICTIONS=$(jq -r '(.[0].restrictions // []) | length' <<<"$SKU_JSON")

if [[ -z "$SKU_FAMILY" || -z "$VCPUS_PER_NODE" ]]; then
  echo "SKU ${NODE_VM_SIZE} is not available in ${LOCATION}" >&2
  exit 1
fi

if (( SKU_RESTRICTIONS != 0 )); then
  jq '.[0].restrictions' <<<"$SKU_JSON" >&2
  echo "SKU ${NODE_VM_SIZE} has deployment restrictions in ${LOCATION}" >&2
  exit 1
fi

USAGE_JSON=$(az vm list-usage --location "$LOCATION" -o json)

FAMILY_USED=$(jq -r --arg family "$SKU_FAMILY" \
  '.[] | select(.name.value == $family) | .currentValue | tonumber' \
  <<<"$USAGE_JSON")
FAMILY_LIMIT=$(jq -r --arg family "$SKU_FAMILY" \
  '.[] | select(.name.value == $family) | .limit | tonumber' \
  <<<"$USAGE_JSON")
REGIONAL_USED=$(jq -r \
  '.[] | select(.name.value == "cores") | .currentValue | tonumber' \
  <<<"$USAGE_JSON")
REGIONAL_LIMIT=$(jq -r \
  '.[] | select(.name.value == "cores") | .limit | tonumber' \
  <<<"$USAGE_JSON")
VM_USED=$(jq -r \
  '.[] | select(.name.value == "virtualMachines") | .currentValue | tonumber' \
  <<<"$USAGE_JSON")
VM_LIMIT=$(jq -r \
  '.[] | select(.name.value == "virtualMachines") | .limit | tonumber' \
  <<<"$USAGE_JSON")

if [[ -z "$FAMILY_USED" || -z "$FAMILY_LIMIT" ]]; then
  echo "No quota record found for ${SKU_FAMILY} in ${LOCATION}" >&2
  exit 1
fi

REQUIRED_VCPUS=$((QUOTA_NODE_COUNT * VCPUS_PER_NODE))

printf '%-28s used=%-6s limit=%-6s available=%s\n' \
  "$SKU_FAMILY" "$FAMILY_USED" "$FAMILY_LIMIT" \
  "$((FAMILY_LIMIT - FAMILY_USED))"
printf '%-28s used=%-6s limit=%-6s available=%s\n' \
  "Total regional vCPUs" "$REGIONAL_USED" "$REGIONAL_LIMIT" \
  "$((REGIONAL_LIMIT - REGIONAL_USED))"
printf '%-28s used=%-6s limit=%-6s available=%s\n' \
  "Virtual Machines" "$VM_USED" "$VM_LIMIT" "$((VM_LIMIT - VM_USED))"

if (( FAMILY_LIMIT - FAMILY_USED < REQUIRED_VCPUS )); then
  echo "Insufficient ${SKU_FAMILY} quota: need ${REQUIRED_VCPUS} free vCPUs" >&2
  exit 1
fi

if (( REGIONAL_LIMIT - REGIONAL_USED < REQUIRED_VCPUS )); then
  echo "Insufficient total regional vCPU quota" >&2
  exit 1
fi

if (( VM_LIMIT - VM_USED < QUOTA_NODE_COUNT )); then
  echo "Insufficient regional Virtual Machines quota" >&2
  exit 1
fi
```

If this check fails, select another unrestricted eight-vCPU SKU with sufficient
quota or request a quota increase. Do not continue with a direct VMSS scale.

## Create a new cluster

Create the resource group and the 10-node system pool. Supplying the `/15` pod
CIDR at creation avoids a later network-profile update.

```bash
az group create \
  --name "$RESOURCE_GROUP" \
  --location "$LOCATION" \
  --only-show-errors

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
  --pod-cidr "$POD_CIDR" \
  --service-cidr "$SERVICE_CIDR" \
  --dns-service-ip "$DNS_SERVICE_IP" \
  --generate-ssh-keys \
  --only-show-errors
```

Load credentials into the dedicated kubeconfig and verify the system pool
before creating the large worker pool:

```bash
az aks get-credentials \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --file "$KUBECONFIG" \
  --overwrite-existing

kubectl get nodes -l agentpool="$SYSTEM_POOL"
```

## Add the 290-node user pool

Use AKS to create the user pool. Do not run `az vmss create`, `az vmss update`,
or `az vmss scale` against the node resource group.

```bash
az aks nodepool add \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --name "$WORKER_POOL" \
  --mode User \
  --node-count "$WORKER_NODE_COUNT" \
  --node-vm-size "$NODE_VM_SIZE" \
  --max-pods "$MAX_PODS" \
  --node-osdisk-size "$OS_DISK_SIZE_GB" \
  --node-osdisk-type Managed \
  --only-show-errors
```

The command can take a while because AKS waits for the VMSS instances and
nodes. If Azure reports regional capacity pressure, leave the system pool
intact and retry later or select another quota-rich SKU. Do not compensate by
editing the managed VMSS.

## Expand an existing 10-node cluster

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

Set the quota demand to the number of nodes being added, then re-run the
commands in [Check SKU and quota](#check-sku-and-quota):

```bash
QUOTA_NODE_COUNT="$WORKER_NODE_COUNT"
```

Then run the `az aks nodepool add` command from the previous section.

If the existing cluster hosted a node-mutating DaemonSet, clean its host state
before scaling. Gantry, for example, can write containerd registry mirror files
under `/etc/containerd/certs.d` and peer state under `/var/lib/gantry`. Deleting
its namespace removes Kubernetes objects but does not remove those host files.
Use a narrowly scoped cleanup DaemonSet against the existing nodes, verify the
owned paths are absent on every node, and delete the cleanup DaemonSet before
adding the worker pool.

## Validate the result

Wait until AKS reports both pools as succeeded:

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

- The system pool has 10 nodes.
- The worker pool has 290 nodes.
- All 300 Kubernetes nodes report `Ready`.
- Both AKS node pools and both managed VM scale sets report `Succeeded`.

## Roll back the worker pool

If the worker pool must be removed, drain or relocate its workloads and delete
the pool through AKS:

```bash
az aks nodepool delete \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --name "$WORKER_POOL" \
  --only-show-errors
```

Do not delete the AKS-managed worker VMSS directly.