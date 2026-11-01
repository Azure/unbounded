---
title: "Install and Operate Gantry"
weight: 8
description: "Install Gantry as a peer-to-peer containerd registry mirror and verify image distribution."
---

Gantry is a peer-to-peer OCI distribution layer for Kubernetes clusters that
run containerd. It reduces duplicate traffic to origin registries by reusing
image manifests and layers already present in the containerd content stores of
other nodes.

Gantry runs one agent on each selected Linux node. The agent acts as a local
containerd registry mirror, participates in a libp2p distributed hash table
(DHT), serves content to peers, and coordinates cold pulls through a bounded
set of Kubernetes Lease holders called chairs. Workloads continue to use
normal image references, `imagePullSecrets`, and kubelet credential providers.

Use Gantry when many nodes pull the same large images and origin bandwidth,
registry rate limits, or rollout time are concerns. Gantry is not an image
registry, an image-signing system, or a replacement for containerd's local
content store.

For the component and request flows behind this guide, see
[Gantry Architecture]({{< relref "concepts/gantry-architecture" >}}).

## Requirements

Before installing Gantry, confirm the following on every selected node:

- The node runs Linux and containerd.
- Containerd exposes its socket. The chart default is
  `/run/containerd/containerd.sock`.
- The CRI registry configuration points `config_path` at
  `/etc/containerd/certs.d`.
- Gantry pods can connect to each other on the libp2p, transfer, and chair
  ports. The chart defaults are TCP `4001`, `5001`, and `5002`.
- The cluster can create namespace-scoped Leases and the Gantry service
  account can manage the configured chair Leases.
- Each configured origin is reachable from every selected node because any
  agent may hold a chair or use the last-resort origin fallback.

Nodes provisioned by `unbounded-agent` already satisfy the containerd
requirements: the agent configures `config_path`, writes the Gantry
`_default/hosts.toml` route, and manages containerd. No additional containerd
setup is required for the operator-managed installation.

For a standalone Helm installation, the chart's node-config DaemonSet writes
and reconciles `_default/hosts.toml`, but it does not edit containerd's main
configuration file or restart containerd. Containerd must already have
`config_path` configured through the operating system or another
node-provisioning system.

If the containerd socket or `certs.d` directory is in a nonstandard location,
set the corresponding `containerd.*` chart values. If the socket uses a
dedicated group, set `containerd.socketGroup` to that group ID.

## Choose an Installation Model

Gantry has two mutually exclusive installation models:

| Model | Resource manager | Containerd mirror owner |
| --- | --- | --- |
| Standalone Helm | Helm | `DaemonSet/gantry-containerd-config` |
| Unbounded operator | `unbounded-operator` | `unbounded-agent` |

Do not install the standalone chart on a cluster where the Unbounded operator
manages Gantry. Both installers use `PriorityClass/gantry-low` to identify the
active manager and reject ownership by the other installation model.

## Standalone Helm Installation

### Install Gantry

Choose a version from the
[Unbounded releases](https://github.com/Azure/unbounded/releases) and omit the
leading `v` when setting `GANTRY_VERSION`.

The example below accelerates images from `registry.k8s.io` and uses the
shipped defaults for everything else. To use your own registry, replace
`registry.k8s.io` in both settings with its hostname, including the port if
needed. Keep the `https://` prefix on the endpoint.

```bash
export GANTRY_VERSION="<release-without-v>"

helm upgrade --install gantry \
  "oci://ghcr.io/azure/charts/gantry" \
  --version "$GANTRY_VERSION" \
  --namespace gantry-system \
  --create-namespace \
  --set-string gantry.upstreamRegistries[0].name=registry.k8s.io \
  --set-string gantry.upstreamRegistries[0].endpoint=https://registry.k8s.io \
  --wait \
  --timeout 15m
```

No per-image configuration is needed. Workloads keep their existing image
references, and Gantry accelerates pulls from the configured registry.

The chart deploys Gantry and a node-config DaemonSet that installs and
maintains the containerd mirror route. You do not need to write `hosts.toml`
yourself. For nonstandard environments, see
[When the Defaults Do Not Fit](#when-the-defaults-do-not-fit).

### Confirm the Rollout

The install command waits for readiness. To check the rollout again:

```bash
kubectl -n gantry-system rollout status \
  daemonset/gantry-containerd-config --timeout=15m

kubectl -n gantry-system rollout status \
  daemonset/gantry --timeout=15m
```

Once both DaemonSets are ready, [verify image distribution](#verify-the-installation).

## Operator-Managed Installation

Gantry is enabled by default when the Unbounded operator manages a `Site`.
There is no separate Helm installation or Gantry image to choose. The operator
deploys Gantry, and `unbounded-agent` configures containerd on the nodes it
provisions.

If you have not installed Unbounded yet, follow
[Getting Started]({{< relref "guides/getting-started" >}}) or
[Bring Your Own Cluster]({{< relref "guides/existing-cluster" >}}) first.

### Choose the Registries to Accelerate

Edit the Gantry ConfigMap in the operator's namespace, normally
`unbounded-system`:

```bash
kubectl -n unbounded-system edit configmap gantry-config
```

Inside `data.config.yaml`, replace the example `upstream_registries` list with
your registries. For example:

```yaml
upstream_registries:
  - name: registry.k8s.io
    endpoint: https://registry.k8s.io
```

Leave the other settings unchanged. The operator preserves your configuration
and rolls out Gantry when it changes.

### Confirm the Rollout

```bash
kubectl -n unbounded-system rollout status daemonset/gantry --timeout=15m
```

There is no separate node-config DaemonSet in this installation model. Once
Gantry is ready, [verify image distribution](#verify-the-installation), using
`unbounded-system` instead of `gantry-system` in those commands. If you installed
the operator in a different namespace, use that namespace instead.

## Private Registries

Gantry supports requester-delegated credentials and an optional shared
identity.

### Requester-Delegated Credentials

Requester delegation is the preferred model:

1. Containerd requests a digest through the local Gantry mirror.
2. Gantry discovers and relays the origin's authentication challenge.
3. Containerd responds with credentials obtained from an `imagePullSecret` or
   kubelet credential provider.
4. Gantry carries that request-scoped authorization to the selected origin
   puller.
5. Gantry discards the authorization after the request.

The selected puller does not need the requester's Kubernetes Secret. Gantry
does not persist delegated credentials or replace rejected credentials with
the puller's identity.

Requester delegation requires an HTTPS origin. Validate that a private image
pull succeeds without Gantry before enabling the mirror. This separates
kubelet credential-provider or RBAC failures from Gantry failures.

### Shared Registry Identity

Use a shared identity only when containerd cannot provide a usable delegated
credential. Create `Secret/gantry-registry-credentials` in the Gantry
namespace and configure the matching upstream's `credentialsPath`.

The mounted credential file contains a `username:password` pair. Gantry reads
configured files during startup; a missing or unreadable file prevents the
agent from starting. Distributing a shared registry identity to every Gantry
pod expands its credential exposure, so prefer requester delegation.

## Verify the Installation

### Check Rollout and Health

```bash
kubectl -n gantry-system get daemonsets,pods -o wide

GANTRY_POD=$(kubectl -n gantry-system get pods \
  -l app.kubernetes.io/name=gantry \
  -o custom-columns='NAME:.metadata.name,READY:.status.containerStatuses[0].ready' \
  --no-headers |
  awk '$2 == "true" {print $1; exit}')

test -n "$GANTRY_POD"

kubectl -n gantry-system port-forward \
  "pod/${GANTRY_POD}" 9095:9095
```

In another shell:

```bash
curl -fsS http://127.0.0.1:9095/livez
curl -fsS http://127.0.0.1:9095/readyz
curl -fsS http://127.0.0.1:9095/metrics |
  grep -E 'gantry_storage_mode_info|p2p_dht_health_score'
```

`/livez` reports process health. `/readyz` also requires the local containerd
store and peer-discovery prerequisites. A live agent may therefore be NotReady
while the DHT is empty or the containerd socket is unavailable.

### Verify Peer Reuse

Roll out the same multi-layer image on at least two nodes. The first pull may
fetch content from the origin. Subsequent pulls should use local or peer
content.

Useful signals include:

| Signal | Interpretation |
| --- | --- |
| `gantry_storage_mode_info{mode="containerd"}` | The supported storage backend is active |
| `p2p_dht_health_score` | Peer-discovery health |
| `p2p_cache_hit_total` | Mirror requests served from the local containerd store |
| `p2p_peer_fetch_total{outcome="hit"}` | Content received from another Gantry agent |
| `p2p_origin_pull_total` | Origin pulls performed by selected pullers |
| `p2p_origin_fallback_total` | Last-resort direct-origin fallback |
| `gantry_advertise_reconcile_total` | Local-content reconciliation with the DHT |
| `gantry_containerd_lease_created_total` | Background ingests protected by containerd leases |

A healthy rollout does not guarantee that one arbitrary pod reports every
counter. Inspect the Gantry pod on the node that performed the image pull when
the sampled pod shows no activity.

## Upgrade

Upgrade the standalone installation with `helm upgrade` and wait for both
DaemonSets. Do not edit chart-owned resources directly.

Chair-count upgrades from releases that understand only the original 64-chair
set require two stages:

1. Upgrade all Gantry pods while keeping `gantry.chairCount=64`.
2. After the rollout completes, raise `gantry.chairCount` and
   `gantry.chairSeedCount`.

Combining these stages can let a new pod create Lease names that an old pod
does not understand.

## Uninstall

```bash
helm uninstall gantry --namespace gantry-system
```

During graceful shutdown, the node-config pod removes
`_default/hosts.toml` only when its content still matches the chart-managed
payload. Nodes that were unavailable during uninstall may retain the mirror
route. Check those nodes before considering Gantry fully removed.

Chair Leases carry Helm's keep policy and remain after uninstall. Delete them
only after every Gantry pod has stopped:

```bash
kubectl -n gantry-system delete lease \
  -l app.kubernetes.io/name=gantry
```

## Security Guidance

- Keep the mirror reachable from containerd through node loopback. The chart's
  host port binds to `127.0.0.1`, but the listener inside the pod still binds
  to the pod network.
- Restrict the mirror, transfer, chair, metrics, and libp2p ports with a
  site-specific NetworkPolicy. The example overlay is not installed by
  default because node and monitoring CIDRs vary by cluster.
- Peer transfer uses plaintext HTTP/2 within the cluster. Use a service mesh
  or another network encryption layer when the cluster network is not trusted.
- Do not expose the optional pprof endpoint. Gantry requires it to bind to
  loopback.
- Continue using admission policy and signature-verification tooling for image
  provenance. Gantry verifies content digests but does not establish publisher
  identity.

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| Gantry pod remains Pending | Node selectors, taints, tolerations, resources, and containerd socket host path |
| `/readyz` reports containerd unavailable | Socket path, socket group, mount, and containerd namespace |
| `/readyz` reports an empty routing table | Pod-to-pod connectivity on TCP `4001`, `5001`, and `5002` |
| Image pull bypasses Gantry | Containerd `config_path`, `_default/hosts.toml`, node-config rollout, and registry allowlist |
| Private image returns `401` | Verify the same image pulls without Gantry, then inspect the relayed challenge and credential provider |
| Agents restart repeatedly | Logs for startup timeout, invalid configuration, missing credentials, or inaccessible containerd |
| Origin fallback rises | Peer reachability, DHT health, stale providers, chair availability, and origin errors |
| Uninstall leaves a dead mirror route | Remove stale chart-owned `hosts.toml` from nodes unavailable during uninstall |

When a specific pod is NotReady, port-forward that pod and request `/readyz`
without curl's `--fail` option so its response body remains visible:

```bash
kubectl -n gantry-system port-forward \
  "pod/<not-ready-pod>" 9096:9095

curl -i http://127.0.0.1:9096/livez
curl -i http://127.0.0.1:9096/readyz
```

## When the Defaults Do Not Fit

Most installations should not change the Gantry runtime defaults. Override
chart values only for a concrete environment requirement:

| Scenario | Required change |
| --- | --- |
| The Unbounded operator manages Gantry | Do not install the standalone chart; use the operator-managed path |
| Containerd does not read `/etc/containerd/certs.d` | Configure containerd before installing Gantry |
| The containerd socket is not under `/run/containerd` | Set `containerd.hostPath`, `containerd.mountPath`, and `containerd.socketPath` |
| Containerd uses a namespace other than `k8s.io` | Set `containerd.namespace` |
| The socket is not accessible through group `0` | Set `containerd.socketGroup` to the socket's group ID |
| Another system owns `_default/hosts.toml` | Set `nodeConfig.enabled=false` and configure the Gantry mirror route there |
| You need to accelerate more than one registry | Add another `gantry.upstreamRegistries` entry for each registry |
| Requester-delegated private-registry authentication is unavailable | Configure a [shared registry identity](#shared-registry-identity) |
| Cluster policy requires different scheduling or resource limits | Override `nodeSelector`, `tolerations`, or `resources` |

Do not change chair counts, timeouts, concurrency limits, or libp2p settings
without an observed scaling or reliability reason.

For operator-managed installations, use
[component workload overrides]({{< relref "reference/workload-overrides" >}})
for scheduling and resource changes. To opt a Site out of Gantry, set
`spec.components.gantry.enabled: false`. All Sites must opt out to prevent
installation; opting out does not remove an existing operator-managed Gantry
installation.
