#!/bin/sh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.


VMSS_NAME="${1}"
if [ -z "$VMSS_NAME" ]; then
    echo "Error: VMSS_NAME argument is required" >&2
    exit 1
fi

# Optional: override the NSG name (e.g. site-nsg). If not provided, resolved from cluster.
NSG_NAME_OVERRIDE="${2:-}"

# Optional: extra node labels to append (e.g. ",net.unbounded-cloud.io/gateway=true")
EXTRA_NODE_LABELS="${3:-}"

API_SERVER="$(kubectl config view --flatten --minify --template '{{ (index .clusters 0).cluster.server }}' | awk -F'[:/]+' '{print $2}')"
CA_CERT_BASE64="$(kubectl config view --flatten --minify --template '{{ index (index .clusters 0).cluster "certificate-authority-data" }}')"
CLUSTER_DNS="$(kubectl get svc -n kube-system kube-dns -o go-template --template '{{.spec.clusterIP}}')"
KUBERNETES_VERSION="$(kubectl version -o json | jq -r '.serverVersion.gitVersion')"
REPO="$(git rev-parse --show-toplevel)"

# Check for an existing bootstrap token secret for this pool
EXISTING_SECRET="$(kubectl get secrets -n kube-system \
    -l net.unbounded-cloud.io/pool-name="$VMSS_NAME" \
    --field-selector type=bootstrap.kubernetes.io/token \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"

if [ -n "$EXISTING_SECRET" ]; then
    echo "Reusing existing bootstrap token secret: $EXISTING_SECRET" >&2
    BOOTSTRAP_TOKEN_ID="$(kubectl get secret -n kube-system "$EXISTING_SECRET" -o jsonpath='{.data.token-id}' | base64 -d)"
    BOOTSTRAP_TOKEN_SECRET="$(kubectl get secret -n kube-system "$EXISTING_SECRET" -o jsonpath='{.data.token-secret}' | base64 -d)"
else
    echo "No existing bootstrap token found for pool $VMSS_NAME, creating new one" >&2
    BOOTSTRAP_TOKEN_ID="$(openssl rand -hex 16 | cut -c1-6)"
    BOOTSTRAP_TOKEN_SECRET="$(openssl rand -hex 16 | cut -c1-16)"

    kubectl create secret generic bootstrap-token-$BOOTSTRAP_TOKEN_ID \
    --namespace kube-system \
    --type 'bootstrap.kubernetes.io/token' \
    --from-literal=description="Bootstrap token for ${VMSS_NAME}" \
    --from-literal=token-id="$BOOTSTRAP_TOKEN_ID" \
    --from-literal=token-secret="$BOOTSTRAP_TOKEN_SECRET" \
    --from-literal=usage-bootstrap-authentication="true" \
    --from-literal=usage-bootstrap-signing="true" \
    -l net.unbounded-cloud.io/pool-name="$VMSS_NAME" >&2
fi

# Extract fields from the providerID of a system node:
#   azure:///subscriptions/<SUB>/resourceGroups/<RG>/providers/Microsoft.Compute/virtualMachineScaleSets/<VMSS>/virtualMachines/<ID>
PROVIDER_ID="$(kubectl get nodes -l kubernetes.azure.com/mode=system --template '{{ (index .items 0).spec.providerID }}')"
SUBSCRIPTION_ID="$(echo "$PROVIDER_ID" | awk -F'/+' '{print $3}')"
CLUSTER_RG="$(echo "$PROVIDER_ID" | awk -F'/+' '{print $5}')"
SYSTEM_VMSS="$(echo "$PROVIDER_ID" | awk -F'/+' '{print $9}')"

# Look up the system VMSS to find the subnet, VNet, NSG, and route table
SUBNET_ID="$(az vmss show -g "$CLUSTER_RG" -n "$SYSTEM_VMSS" \
    --query 'virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].ipConfigurations[0].subnet.id' -o tsv)"
VNET_NAME="$(echo "$SUBNET_ID" | awk -F'/' '{for(i=1;i<=NF;i++) if($i=="virtualNetworks") print $(i+1)}')"
VNET_RG="$(echo "$SUBNET_ID" | awk -F'/' '{for(i=1;i<=NF;i++) if($i=="resourceGroups") print $(i+1)}')"
SUBNET_NAME="$(echo "$SUBNET_ID" | awk -F'/' '{print $NF}')"

SECURITY_GROUP_NAME="$NSG_NAME_OVERRIDE"
if [ -z "$SECURITY_GROUP_NAME" ]; then
    SECURITY_GROUP_NAME="$(az vmss show -g "$CLUSTER_RG" -n "$SYSTEM_VMSS" \
        --query 'virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].networkSecurityGroup.id' -o tsv | awk -F'/' '{print $NF}')"
fi
ROUTE_TABLE_NAME="$(az network vnet subnet show --ids "$SUBNET_ID" --query 'routeTable.id' -o tsv | awk -F'/' '{print $NF}')"
TENANT_ID="$(az account show --query tenantId -o tsv)"

# Resolve the client ID of the kubelet managed identity
IDENTITY_ID="$(az vmss show -g "$CLUSTER_RG" -n "$SYSTEM_VMSS" \
    --query 'identity.userAssignedIdentities | keys(@) | [0]' -o tsv)"
KUBELET_CLIENT_ID="$(az identity show --ids "$IDENTITY_ID" --query clientId -o tsv)"

SCALE_SET_NAME="$VMSS_NAME"

echo "Resolved values:" >&2
echo "  API_SERVER=$API_SERVER" >&2
echo "  CLUSTER_RG=$CLUSTER_RG" >&2
echo "  SUBSCRIPTION_ID=$SUBSCRIPTION_ID" >&2
echo "  TENANT_ID=$TENANT_ID" >&2
echo "  VNET_NAME=$VNET_NAME" >&2
echo "  VNET_RG=$VNET_RG" >&2
echo "  SECURITY_GROUP_NAME=$SECURITY_GROUP_NAME" >&2
echo "  ROUTE_TABLE_NAME=$ROUTE_TABLE_NAME" >&2
echo "  SCALE_SET_NAME=$SCALE_SET_NAME" >&2

# Skip comment lines (^[[:space:]]*#) to avoid replacing tokens in documentation
sed -e '/^[[:space:]]*#/!s|__API_SERVER__|'"$API_SERVER"'|g' \
    -e '/^[[:space:]]*#/!s|__BOOTSTRAP_TOKEN__|'"$BOOTSTRAP_TOKEN_ID.$BOOTSTRAP_TOKEN_SECRET"'|g' \
    -e '/^[[:space:]]*#/!s|__CLUSTER_DNS__|'"$CLUSTER_DNS"'|g' \
    -e '/^[[:space:]]*#/!s|__CLUSTER_RG__|'"$CLUSTER_RG"'|g' \
    -e '/^[[:space:]]*#/!s|__KUBERNETES_VERSION__|'"$KUBERNETES_VERSION"'|g' \
    -e '/^[[:space:]]*#/!s|__CA_CERT_BASE64__|'"$CA_CERT_BASE64"'|g' \
    -e '/^[[:space:]]*#/!s|__TENANT_ID__|'"$TENANT_ID"'|g' \
    -e '/^[[:space:]]*#/!s|__SUBSCRIPTION_ID__|'"$SUBSCRIPTION_ID"'|g' \
    -e '/^[[:space:]]*#/!s|__SECURITY_GROUP_NAME__|'"$SECURITY_GROUP_NAME"'|g' \
    -e '/^[[:space:]]*#/!s|__VNET_NAME__|'"$VNET_NAME"'|g' \
    -e '/^[[:space:]]*#/!s|__VNET_RG__|'"$VNET_RG"'|g' \
    -e '/^[[:space:]]*#/!s|__SUBNET_NAME__|'"$SUBNET_NAME"'|g' \
    -e '/^[[:space:]]*#/!s|__ROUTE_TABLE_NAME__|'"$ROUTE_TABLE_NAME"'|g' \
    -e '/^[[:space:]]*#/!s|__SCALE_SET_NAME__|'"$SCALE_SET_NAME"'|g' \
    -e '/^[[:space:]]*#/!s|__EXTRA_NODE_LABELS__|'"$EXTRA_NODE_LABELS"'|g' \
    -e '/^[[:space:]]*#/!s|__KUBELET_CLIENT_ID__|'"$KUBELET_CLIENT_ID"'|g' \
    -e '/^[[:space:]]*#/!s|__CUSTOMDATA_GENERATED__|'"$(date -u +%Y%m%d%H%M%SZ)"'|g' \
    < "${REPO}/scripts/userdata-bootstrap.yaml"