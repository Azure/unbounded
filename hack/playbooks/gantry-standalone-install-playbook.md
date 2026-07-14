# Standalone Gantry Install Playbook

This playbook installs Gantry on a fresh Kubernetes cluster and validates
private Azure Container Registry pulls without workload `imagePullSecrets`.
It uses the repo-owned ACR kubelet exec credential provider as a validation
fixture, proves private ACR pulls work before Gantry, then installs Gantry and
proves the same private pull path works through Gantry.

The credential provider is an independent fixture under
`hack/acr-credential-provider`. This playbook consumes it, but Gantry does not
own or depend on its source code.

This is an internal operator playbook, not a design document and not the public
Gantry guide.

## Validation Flow

1. Create or reuse an ACR.
2. Import a small private test image into ACR.
3. Grant the AKS kubelet identity `AcrPull` on the ACR.
4. Build and publish a durable public ACR credential-provider installer image.
5. Install the provider on every node with a DaemonSet.
6. Validate a plain private ACR Pod before installing Gantry.
7. Install Gantry service account, containerd node config, ConfigMap, and DaemonSet.
8. Validate the same private ACR image on worker nodes after Gantry is running.
9. Check Gantry logs, health, and node-local metrics.

## Assumptions

- The target cluster uses Linux nodes and containerd.
- The cluster nodes support kubelet exec credential providers.
- The operator can assign ACR pull permissions to the AKS kubelet identity.
- The provider installer image is in a public registry that nodes can pull
  before ACR authentication is configured.
- The provider installer image tag is durable. Do not use a short-lived tag for
  ongoing clusters, because future nodes must be able to pull the installer.
- Workload manifests do not use `imagePullSecrets` for this validation.
- Commands run from the repository root so relative `hack/` and `deploy/`
  paths resolve correctly.
- Gantry is installed from this repository's plain manifests under
  `deploy/gantry`.

## Set Variables

Set a dedicated kubeconfig and the AKS, ACR, Gantry, and provider image values.
The provider installer image must be public. Gantry itself may live in the
private ACR after the provider preflight succeeds.

```bash
set -euo pipefail

: "${KUBECONFIG:?Set KUBECONFIG to the target cluster kubeconfig}"
: "${SUBSCRIPTION_ID:?Set SUBSCRIPTION_ID}"
: "${RESOURCE_GROUP:?Set RESOURCE_GROUP}"
: "${CLUSTER_NAME:?Set CLUSTER_NAME}"
: "${ACR_NAME:?Set ACR_NAME}"
: "${PUBLIC_PROVIDER_REGISTRY:?Set PUBLIC_PROVIDER_REGISTRY, for example ghcr.io/your-org}"
: "${PROVIDER_VERSION:?Set PROVIDER_VERSION to a durable tag}"

export KUBECONFIG

LOCATION="${LOCATION:-canadacentral}"
WORK_DIR="${WORK_DIR:-tmp/gantry-standalone-live}"
ACR_PROVIDER_ROOT="${ACR_PROVIDER_ROOT:-hack/acr-credential-provider}"
ACR_PROVIDER_NAMESPACE="${ACR_PROVIDER_NAMESPACE:-acr-credential-provider-system}"
ACR_PROVIDER_INSTALLER_IMAGE="${PUBLIC_PROVIDER_REGISTRY}/acr-credential-provider-installer:${PROVIDER_VERSION}"

az account set --subscription "$SUBSCRIPTION_ID"
mkdir -p "$WORK_DIR"
```

Confirm cluster access and node readiness:

```bash
kubectl config current-context
kubectl get nodes
kubectl get nodes --no-headers | awk '{count[$2]++} END {for (condition in count) print condition, count[condition]}'
```

## Prepare ACR And Test Image

Create the registry if needed, then import a small Kubernetes test image. The
operator imports the image. Gantry and the credential-provider installer do not
push the test image.

```bash
az group create \
  --name "$RESOURCE_GROUP" \
  --location "$LOCATION" \
  --only-show-errors

az acr create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$ACR_NAME" \
  --sku Premium \
  --only-show-errors || true

ACR_LOGIN_SERVER=$(az acr show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$ACR_NAME" \
  --query loginServer \
  -o tsv)

az acr import \
  --name "$ACR_NAME" \
  --source registry.k8s.io/e2e-test-images/agnhost:2.39 \
  --image gantry-private-test:latest \
  --only-show-errors

PRIVATE_TEST_IMAGE="${ACR_LOGIN_SERVER}/gantry-private-test:latest"
```

Grant the AKS kubelet identity pull access to the registry. For ABAC-enabled
registries, use the repository-reader role your registry requires instead of
`AcrPull`.

```bash
ACR_ID=$(az acr show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$ACR_NAME" \
  --query id \
  -o tsv)

KUBELET_OBJECT_ID=$(az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --query identityProfile.kubeletidentity.objectId \
  -o tsv)

az role assignment create \
  --assignee-object-id "$KUBELET_OBJECT_ID" \
  --assignee-principal-type ServicePrincipal \
  --role AcrPull \
  --scope "$ACR_ID" \
  --only-show-errors || true
```

## Build And Publish The Provider Installer

Build the repo-owned kubelet exec provider and publish its public installer
image. Use a durable public registry tag for real cluster use. A short-lived
test registry can prove the flow once, but it will not satisfy the new-node
DaemonSet requirement later.

```bash
GOTOOLCHAIN=auto make acr-credential-provider-build

GOTOOLCHAIN=auto make image-acr-credential-provider-installer-push \
  CONTAINER_REGISTRY="$PUBLIC_PROVIDER_REGISTRY" \
  VERSION="$PROVIDER_VERSION"
```

Render the installer DaemonSet with the published image. The example manifest
is multi-document and includes the Namespace, so use direct rendering instead
of `kubectl set image --local`.

```bash
sed \
  -e "s#image: ghcr.io/azure/acr-credential-provider-installer:latest#image: ${ACR_PROVIDER_INSTALLER_IMAGE}#" \
  -e "s#acr-credential-provider-system#${ACR_PROVIDER_NAMESPACE}#g" \
  "$ACR_PROVIDER_ROOT/installer/daemonset.yaml" \
  > "$WORK_DIR/acr-credential-provider-installer.yaml"

kubectl apply --dry-run=client \
  -f "$WORK_DIR/acr-credential-provider-installer.yaml" \
  -o name
```

## Install The ACR Credential Provider

The installer DaemonSet writes host kubelet credential-provider state. It
installs the `acr-credential-provider` binary, writes a
`CredentialProviderConfig` matching `*.azurecr.io`, appends managed kubelet
flags, and restarts kubelet only when it changed those flags.

The default first-install restart jitter is 180 seconds, controlled by
`ACR_PROVIDER_RESTART_JITTER_SECONDS`, so large clusters do not restart kubelet
everywhere at once.

```bash
kubectl apply -f "$WORK_DIR/acr-credential-provider-installer.yaml"
kubectl -n "$ACR_PROVIDER_NAMESPACE" rollout status daemonset/acr-credential-provider-installer --timeout=15m
kubectl -n "$ACR_PROVIDER_NAMESPACE" get daemonset/acr-credential-provider-installer -o wide
kubectl -n "$ACR_PROVIDER_NAMESPACE" get pods -o wide
kubectl -n "$ACR_PROVIDER_NAMESPACE" logs daemonset/acr-credential-provider-installer --tail=20
kubectl wait nodes --all --for=condition=Ready --timeout=10m
```

The DaemonSet targets Linux nodes and tolerates all taints. Existing nodes and
future Linux nodes should converge automatically as long as the installer image
remains pullable.

## Validate Private ACR Pull Before Gantry

Schedule a plain Pod with the private ACR image. Do not add pull secrets. If
this fails, stop and fix the provider before installing Gantry.

```bash
cat > "$WORK_DIR/acr-provider-preflight.yaml" <<ACR_PREFLIGHT
apiVersion: v1
kind: Pod
metadata:
  name: acr-provider-preflight
  namespace: default
  labels:
    app.kubernetes.io/name: acr-provider-preflight
spec:
  restartPolicy: Never
  containers:
    - name: agnhost
      image: ${PRIVATE_TEST_IMAGE}
      command: ["/agnhost"]
      args: ["pause"]
ACR_PREFLIGHT

kubectl delete pod acr-provider-preflight --ignore-not-found
kubectl apply -f "$WORK_DIR/acr-provider-preflight.yaml"
kubectl wait pod/acr-provider-preflight --for=condition=Ready --timeout=10m
kubectl get pod acr-provider-preflight -o wide
```

## Preflight Gantry Node Requirements

Gantry's standalone install depends on containerd using `certs.d` host
configuration and on the containerd socket being accessible to the Gantry pod.
Validate these requirements before applying Gantry.

```bash
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.nodeInfo.operatingSystem}{"\t"}{.status.nodeInfo.containerRuntimeVersion}{"\n"}{end}'
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .spec.taints[*]}{.key}{"="}{.value}{":"}{.effect}{","}{end}{"\n"}{end}'
```

If host access is available, verify on representative nodes:

```bash
grep 'config_path.*certs.d' /etc/containerd/config.toml
stat /run/containerd/containerd.sock
```

If an unmanaged `/etc/containerd/certs.d/_default/hosts.toml` exists on any
node, stop and resolve it. The Gantry node-config DaemonSet refuses to
overwrite unrelated host configuration.

## Build And Publish Gantry

After the provider preflight succeeds, Gantry can be pulled from the private
ACR. Build and push a Gantry image there, or set `GANTRY_IMAGE` to another
image that every node can pull.

```bash
GANTRY_VERSION="${GANTRY_VERSION:-gantry-auth-live}"
GANTRY_IMAGE="${GANTRY_IMAGE:-${ACR_LOGIN_SERVER}/gantry:${GANTRY_VERSION}}"

token=$(az acr login \
  --name "$ACR_NAME" \
  --expose-token \
  --query accessToken \
  -o tsv)

podman login "$ACR_LOGIN_SERVER" \
  --username 00000000-0000-0000-0000-000000000000 \
  --password-stdin <<< "$token"
unset token

GOTOOLCHAIN=auto make image-gantry-push \
  VERSION="$GANTRY_VERSION" \
  CONTAINER_REGISTRY="$ACR_LOGIN_SERVER"
```

## Prepare Gantry Manifests

Render Gantry manifests into the working directory. The ConfigMap must list the
ACR login server as an upstream registry, and the DaemonSet image must be the
selected Gantry image.

```bash
sed \
  -e "s/registry.example.com/${ACR_LOGIN_SERVER}/g" \
  -e "s#https://registry.example.com#https://${ACR_LOGIN_SERVER}#g" \
  deploy/gantry/configmap.yaml > "$WORK_DIR/gantry-configmap.yaml"

kubectl set image --local \
  -f deploy/gantry/daemonset.yaml \
  gantry="$GANTRY_IMAGE" \
  -o yaml > "$WORK_DIR/gantry-daemonset.yaml"

kubectl apply --dry-run=client -f deploy/gantry/serviceaccount.yaml -f deploy/gantry/node-config.yaml -o name
kubectl apply --dry-run=client -f "$WORK_DIR/gantry-configmap.yaml" -f "$WORK_DIR/gantry-daemonset.yaml" -o name
```

Do not add `credentials_path` for this validation. The image pull credential
comes from kubelet through the ACR exec credential provider.

## Install Gantry

Apply Gantry in dependency order. The `gantry-containerd-config` DaemonSet is
required for standalone installs because it writes containerd's default
`hosts.toml` mirror entry.

```bash
kubectl apply -f deploy/gantry/serviceaccount.yaml

kubectl apply -f deploy/gantry/node-config.yaml
kubectl -n gantry-system rollout status daemonset/gantry-containerd-config --timeout=15m

kubectl apply -f "$WORK_DIR/gantry-configmap.yaml"
kubectl apply -f "$WORK_DIR/gantry-daemonset.yaml"
kubectl -n gantry-system rollout status daemonset/gantry --timeout=15m

kubectl -n gantry-system get daemonsets -o wide
kubectl get nodes --no-headers | awk '{count[$2]++} END {for (condition in count) print condition, count[condition]}'
```

On large clusters, a small number of Gantry pods may restart once during initial
membership convergence. In the 300-node validation run, restarted pods exited
with member-sync timeout, then recovered and stayed Ready. Treat repeated
restarts, not a single recovered startup restart, as the failure signal.

## Validate Gantry Health And Logs

Port-forward one Gantry pod and check health plus core metrics.

```bash
GANTRY_POD=$(kubectl -n gantry-system get pods \
  -l app.kubernetes.io/name=gantry \
  -o jsonpath='{.items[0].metadata.name}')

kubectl -n gantry-system port-forward "pod/${GANTRY_POD}" 9095:9095
```

In another shell:

```bash
curl -fsS http://127.0.0.1:9095/readyz
curl -fsS http://127.0.0.1:9095/livez
curl -fsS http://127.0.0.1:9095/metrics | grep 'gantry_storage_mode_info'
curl -fsS http://127.0.0.1:9095/metrics | grep 'p2p_dht_health_score'
```

Check recent logs for startup and obvious failures:

```bash
kubectl -n gantry-system logs "pod/${GANTRY_POD}" --tail=200 | \
  grep -Ei 'gantry starting|connected to containerd|storage backend|transfer endpoint listening|error|warn|fallback' || true

kubectl -n gantry-system get pods \
  -l app.kubernetes.io/name=gantry \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.containerStatuses[0].restartCount}{"\t"}{.status.containerStatuses[0].ready}{"\n"}{end}' | \
  awk '$2 != 0 || $3 != "true" {print}'
```

The log line `NF5 origin-fallback wired` is normal startup wiring. It does not
mean an origin fallback pull happened. Use `p2p_origin_fallback_total` to detect
actual fallback pulls.

## Validate Private ACR Pull Through Gantry

Schedule the private ACR image on at least two worker nodes, still with no
workload pull secrets. Prefer nodes different from the preflight Pod's node.

```bash
PREFLIGHT_NODE=$(kubectl get pod acr-provider-preflight \
  -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)

kubectl get nodes -l agentpool=worker \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | \
  awk -v skip="$PREFLIGHT_NODE" '$0 != skip {print; count++} count == 2 {exit}' \
  > "$WORK_DIR/test-nodes.txt"

NODE_A=$(sed -n '1p' "$WORK_DIR/test-nodes.txt")
NODE_B=$(sed -n '2p' "$WORK_DIR/test-nodes.txt")

if [ -z "$NODE_A" ] || [ -z "$NODE_B" ]; then
  kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | \
    awk -v skip="$PREFLIGHT_NODE" '$0 != skip {print; count++} count == 2 {exit}' \
    > "$WORK_DIR/test-nodes.txt"
  NODE_A=$(sed -n '1p' "$WORK_DIR/test-nodes.txt")
  NODE_B=$(sed -n '2p' "$WORK_DIR/test-nodes.txt")
fi

cat > "$WORK_DIR/acr-gantry-pull-a.yaml" <<ACR_PULL_A
apiVersion: v1
kind: Pod
metadata:
  name: acr-gantry-pull-a
  namespace: default
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  containers:
    - name: agnhost
      image: ${PRIVATE_TEST_IMAGE}
      command: ["/agnhost"]
      args: ["pause"]
ACR_PULL_A

cat > "$WORK_DIR/acr-gantry-pull-b.yaml" <<ACR_PULL_B
apiVersion: v1
kind: Pod
metadata:
  name: acr-gantry-pull-b
  namespace: default
spec:
  nodeName: ${NODE_B}
  restartPolicy: Never
  containers:
    - name: agnhost
      image: ${PRIVATE_TEST_IMAGE}
      command: ["/agnhost"]
      args: ["pause"]
ACR_PULL_B

kubectl delete pod acr-gantry-pull-a acr-gantry-pull-b --ignore-not-found
kubectl apply -f "$WORK_DIR/acr-gantry-pull-a.yaml"
kubectl apply -f "$WORK_DIR/acr-gantry-pull-b.yaml"
kubectl wait pod/acr-gantry-pull-a --for=condition=Ready --timeout=10m
kubectl wait pod/acr-gantry-pull-b --for=condition=Ready --timeout=10m
kubectl get pods acr-provider-preflight acr-gantry-pull-a acr-gantry-pull-b -o wide
```

Check mirror and peer metrics through the existing port-forward:

```bash
curl -fsS http://127.0.0.1:9095/metrics | grep -E \
  'gantry_storage_mode_info|p2p_origin_pull_total|p2p_peer_fetch_total|p2p_cache_hit_total|p2p_origin_fallback_total'
```

Expected result:

- The first private pull may increase origin-pull metrics.
- Later pulls should show cache or peer reuse.
- `p2p_origin_fallback_total` should stay near zero during healthy operation.

If the first sampled Gantry pod does not show pull activity, inspect the Gantry
pod on one of the validation nodes:

```bash
GANTRY_NODE_POD=$(kubectl -n gantry-system get pods \
  -l app.kubernetes.io/name=gantry \
  -o wide | awk -v node="$NODE_A" '$7 == node {print $1; exit}')

kubectl -n gantry-system logs "pod/${GANTRY_NODE_POD}" --tail=200 | \
  grep -Ei 'error|warn|origin|peer|fetch|pull|cache|fallback' || true

kubectl -n gantry-system port-forward "pod/${GANTRY_NODE_POD}" 9096:9095
```

In another shell:

```bash
curl -fsS http://127.0.0.1:9096/readyz
curl -fsS http://127.0.0.1:9096/metrics | grep -E \
  'gantry_storage_mode_info|gantry_containerd_hit_total|p2p_peer_fetch_total|p2p_cache_hit_total|p2p_origin_fallback_total'
```

## Optional NetworkPolicy Hardening

Apply NetworkPolicy only after baseline success. Copy
`deploy/gantry/examples/networkpolicy.yaml` into an overlay, replace every
operator-required CIDR, and ensure mirror TCP/5000 allows the node CIDR rather
than only `127.0.0.1/32`. Re-run Gantry health and private ACR pull validation
after applying the policy.

## Rollback

Remove Gantry and provider installer Kubernetes objects:

```bash
kubectl delete -f "$WORK_DIR/gantry-daemonset.yaml" --ignore-not-found
kubectl delete -f "$WORK_DIR/gantry-configmap.yaml" --ignore-not-found
kubectl delete -f deploy/gantry/node-config.yaml --ignore-not-found
kubectl delete -f deploy/gantry/serviceaccount.yaml --ignore-not-found
kubectl delete -f "$WORK_DIR/acr-credential-provider-installer.yaml" --ignore-not-found
kubectl delete pod acr-provider-preflight acr-gantry-pull-a acr-gantry-pull-b --ignore-not-found
```

Deleting Kubernetes objects does not remove host state. If host cleanup is
required, run a narrowly scoped cleanup DaemonSet that removes only files with
our managed markers:

- ACR provider binary and `CredentialProviderConfig` installed by the provider
  installer.
- The managed kubelet arg block in `/etc/default/kubelet`.
- Gantry-managed `/etc/containerd/certs.d/_default/hosts.toml`.
- Optional Gantry peer identity state under `/var/lib/gantry/libp2p`.

## Troubleshooting

| Symptom | Likely cause | Check |
| --- | --- | --- |
| Private ACR Pod fails before Gantry | Provider installer, node identity, or ACR role assignment is wrong | `kubectl describe pod acr-provider-preflight` and installer pod logs |
| Installer DaemonSet is not Ready | Host kubelet config path differs or privileged host access is blocked | `kubectl -n "$ACR_PROVIDER_NAMESPACE" logs ds/acr-credential-provider-installer` |
| Gantry pod is not Ready | containerd socket or namespace mismatch | Gantry pod logs and `/readyz` |
| Pulls bypass Gantry | containerd is not reading `certs.d` or `_default/hosts.toml` was not installed | Node `config_path` and `gantry-containerd-config` readiness |
| Auth works before Gantry but fails after | ACR is not listed in Gantry `upstream_registries` or auth challenge relay failed | Gantry logs and ConfigMap |
| NetworkPolicy breaks pulls | Mirror TCP/5000 does not allow node-source DNAT traffic | Temporarily remove policy, then fix node CIDR |
