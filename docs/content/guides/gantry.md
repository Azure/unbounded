---
title: "Distribute Container Images with Gantry"
weight: 8
description: "Deploy and operate Gantry as a peer-to-peer containerd registry mirror."
---

Gantry is a decentralized, peer-to-peer OCI distribution layer for Kubernetes.
It reduces duplicate origin-registry traffic during large rollouts by
coordinating each digest fetch on a small number of nodes and serving the
content from peers that already have it.

Gantry runs as a DaemonSet and uses each node's existing containerd content
store. It does not introduce a separate image cache or a central metadata
service. Peer discovery uses libp2p and a distributed hash table (DHT), and
containerd continues to verify every content digest before using it.

After the node-level mirror configuration is installed, Gantry is transparent
to workloads. Pods continue to use normal OCI image references,
`imagePullSecrets`, and kubelet credential providers.

## Architecture

![Gantry architecture: Requester node with Workload, containerd, and Gantry A; Gantry peer network containing the libp2p DHT and a Selected puller node with Gantry B and its containerd content store; Gantry A looks up providers in the DHT and coordinates pull plus streams image content with Gantry B, which fetches once from the Origin registry, while the requester containerd retains a direct fallback path to the Origin registry](../../img/gantry-architecture.svg)

The DHT contains provider records, not image data. Manifests and layers stream
directly from the selected Gantry peer, while containerd remains responsible
for digest verification and the node-local content store.

## How Gantry Handles a Pull

1. Containerd resolves an image tag at the origin registry and obtains a
   content digest.
2. Containerd requests that digest through the node-local Gantry mirror.
3. Gantry checks the local containerd content store and asks the DHT for peers
   that already provide the digest.
4. If no provider exists, Gantry uses deterministic highest-random-weight
  (HRW) selection, a consistent-hashing algorithm, to choose one of a small
  set of nodes to fetch the digest. Every agent reaches the same choice
  without a consensus service.
5. Other nodes stream the content from that peer. The requesting containerd
   verifies and commits it to its own content store.

Gantry routes content by digest. Tag resolution still reaches the origin, so
the exact reduction in registry requests depends on image composition and
rollout size. Large layers distributed to many nodes receive the greatest
benefit.

## Installation

### 1. Confirm Compatibility

The standalone chart supports Linux nodes running containerd. Before installing,
confirm that every selected node exposes `/run/containerd/containerd.sock` and
that containerd reads registry host configuration from
`/etc/containerd/certs.d`.

The chart does not modify or restart containerd. Provision the `certs.d`
setting through your node-management system. For standalone installations, the
chart runs a node-config DaemonSet that continuously reconciles the Gantry
mirror route shown below. The Unbounded agent owns that route instead on
operator-managed nodes, where the chart's node-config resources are disabled.

The default route written on each node is:

```toml
# /etc/containerd/certs.d/_default/hosts.toml
[host."http://127.0.0.1:5000"]
   capabilities = ["pull", "resolve"]
   dial_timeout = "200ms"
```

Containerd must already have its CRI registry `config_path` set to
`/etc/containerd/certs.d`. The chart mounts the runtime directory and expects
the socket at `/run/containerd/containerd.sock` by default; use the
`containerd.*` chart values for a different layout.

Do not install the chart on a cluster where the Unbounded operator manages
Gantry. Both paths check `PriorityClass/gantry-low` and reject ownership by the
other manager.

### 2. Install the OCI Chart

Find the release on the
[Unbounded releases page](https://github.com/Azure/unbounded/releases). The
chart version omits the release tag's leading `v`:

```bash
export GANTRY_VERSION="<release-without-v>"
export GANTRY_IMAGE_DIGEST="sha256:<digest-from-release-bom>"

helm upgrade --install gantry oci://ghcr.io/azure/charts/gantry \
   --version "$GANTRY_VERSION" \
   --namespace gantry-system \
   --create-namespace \
   --set image.digest="$GANTRY_IMAGE_DIGEST" \
   --set gantry.upstreamRegistries[0].name=registry.example.com \
   --set gantry.upstreamRegistries[0].endpoint=https://registry.example.com \
   --wait \
   --timeout 15m
```

The chart owns the Gantry ConfigMap, image, RBAC, PriorityClass, chair Leases,
agent DaemonSet, and node-config DaemonSet. The node-config process checks its
`_default/hosts.toml` every five seconds and atomically restores it after drift
or a node upgrade reset. Upgrade those resources through `helm upgrade`, not
direct edits. Set `nodeConfig.enabled=false` only when another node-management
system owns mirror routing.

During graceful disable or uninstall, the node-config pod removes
`hosts.toml` only when it still matches the chart payload. Nodes unavailable
during uninstall may retain the file and should be checked before the Gantry
endpoint is considered fully removed.

### 3. Configure Upstream Registries

Set `gantry.upstreamRegistries` in your values file. This list is an opt-in allowlist: add only the
origin registries whose image pulls you want Gantry to accelerate. Registries
that are not configured here continue using containerd's normal direct-origin
path and are not distributed through Gantry.

```yaml
gantry:
   upstreamRegistries:
      - name: registry.example.com
         endpoint: https://registry.example.com
```

The `name` must match the registry host in the image reference and in
containerd's `certs.d` directory. Include the port when the image reference
uses a non-default port. Gantry requires at least one accelerated upstream
registry.



### 4. Private Registry Authentication

#### Requester-Delegated Authentication

The workload supplies credentials in the normal Kubernetes way, such as an
`imagePullSecret` or a kubelet credential provider. For a private HTTPS
registry, the flow is:

1. Containerd sends an unauthenticated digest request to its local Gantry
   agent.
2. Gantry probes the registry's verified HTTPS `/v2/` endpoint and relays its
   Basic or Bearer `WWW-Authenticate` challenge.
3. Containerd answers the challenge using the credential supplied by kubelet
   or CRI. For Bearer authentication, containerd exchanges that credential at
   the registry's HTTPS token service and sends Gantry the scoped token.
4. Gantry carries the request-scoped authorization to the selected origin
   puller. The selected node does not need its own registry identity or access
   to the requester's `imagePullSecret`.
5. Gantry discards the authorization after the request. It does not persist
   the credential or fall back to the puller's identity if the registry
   rejects it.

Requester-delegated private-registry authentication requires an HTTPS origin.
Private plaintext HTTP registries are not supported by this mode.

#### Shared-Identity Authentication

Shared identity is a compatibility mode for environments where kubelet or CRI
cannot provide a usable request credential. It gives every Gantry agent the
same registry identity.

To enable it, create `Secret/gantry-registry-credentials` in the release
namespace and set the matching registry's `credentialsPath` chart value.
The file contains a `username:password` pair keyed by the registry `name`.

Gantry reads configured credential files eagerly during startup. A missing
file causes the pod to fail, so do not add `credentials_path` unless the Secret
is present. Prefer requester-delegated authentication when possible because it
avoids distributing a shared registry credential to every node.

### 5. Verify Distribution

First verify one agent directly. Select a pod and forward its health and
metrics listener:

```bash
GANTRY_POD=$(kubectl -n gantry-system get pods \
    -l app.kubernetes.io/name=gantry \
    -o jsonpath='{.items[0].metadata.name}')

kubectl -n gantry-system port-forward "pod/${GANTRY_POD}" 9095:9095
```

In another shell:

```bash
curl -fsS http://127.0.0.1:9095/readyz
curl -fsS http://127.0.0.1:9095/metrics
```

Roll out the same multi-layer image to at least two nodes, then inspect these
metrics:

| Signal | Expected result |
| --- | --- |
| `p2p_dht_health_score` | Reaches at least `0.7` after the routing table forms. |
| `gantry_storage_mode_info{mode="containerd"}` | Equals `1` on every agent. |
| `p2p_cache_hit_total` | Increases when the local containerd store satisfies mirror requests. |
| `p2p_peer_fetch_total{outcome="hit"}` | Increases when a node receives content from a peer. |
| `p2p_origin_pull_total` | Increases only on designated origin pullers during a cold rollout. |
| `gantry_origin_bytes_total{kind}` | Counts bytes read from upstream registries, including partial transfers and retries. |
| `gantry_peer_fetch_bytes_total{kind}` | Counts bytes received from peer Gantry agents. |
| `gantry_peer_serve_bytes_total{kind}` | Counts bytes transmitted to peers; range requests count only the transmitted range. |
| `gantry_mirror_bytes_served_total{kind,source}` | Counts bytes served to local containerd, split by `cache`, `peer`, or `origin`. |
| `p2p_origin_fallback_total` | Remains near zero during healthy operation. |
| `gantry_advertise_reconcile_total` | Continues increasing as Gantry reconciles containerd content with the DHT. |
| `gantry_containerd_lease_created_total` | Increases when coordinated background pulls ingest new content. |

## Use Racer as the Backend

The installation and distribution flow above describe Gantry's default containerd
backend. Racer mode instead uses Racer for object caching and peer reads, with
Gantry supplying the registry origin adapter. Deploy the Racer controller and
dataplane separately, then create this cluster-scoped cache:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: gantry
```

`ClusterCache` has no `spec` field. Gantry uses the fixed SDK `Cache` name
`gantry`, which selects `/run/racer/gantry/client/socket` and
`/run/racer/gantry/origin/socket`.

Racer must run on the same nodes as Gantry. Run one Gantry origin owner on every
eligible node that can supply origin reads for this cache, with consistent
upstream registry configuration. Match placement and tolerations; installing
Gantry on only the requesting nodes is not sufficient.

Racer mode uses requester-delegated registry authentication, not the shared
registry credentials described above. Keep workload `imagePullSecrets` or
kubelet credential providers configured for private HTTPS registries.

**Warning:** Credentials authorize origin reads, not cached-object access. Anyone
who can reach Gantry and knows an object's digest may read cached private content
with different or missing credentials. Run Racer-backed Gantry only within a
single trusted domain, restrict access to Gantry, and do not share it across
mutually untrusted tenants.

### Helm-Managed Gantry

Add `--set racer.enabled=true` to the OCI chart installation command above,
retaining its release version, image digest, upstream registries, and your other
settings. For upgrades, keep those settings in your existing values file:

```bash
helm upgrade --install gantry oci://ghcr.io/azure/charts/gantry \
  --version "$GANTRY_VERSION" --namespace gantry-system \
  -f gantry-values.yaml --set racer.enabled=true --wait --timeout 5m
```

Here `gantry-values.yaml` contains the existing image and registry settings. The
chart configures **Gantry only**; it does not deploy Racer or create the cache.
The setting runs Gantry as UID/GID 0 and mounts `/run/racer/gantry` read-write for
its client and origin sockets. Authorize that hostPath and root workload narrowly.
The container still drops all capabilities, disables privilege escalation, and
uses a read-only root filesystem and the runtime's default seccomp profile.

### Operator-Managed Gantry

Do not install a competing Helm release. Merge the `data.gantry-racer.yaml` entry
from [racer-operator-overrides.yaml](https://github.com/Azure/unbounded/blob/main/deploy/gantry/examples/racer-operator-overrides.yaml)
into your existing `unbounded-component-overrides` ConfigMap in the operator
namespace. Preserve all other keys and reconcile any existing Gantry patches;
do not replace the ConfigMap with the example. If no override ConfigMap exists,
use the example to create it. See
[workload overrides]({{< relref "reference/workload-overrides" >}}).

The example enables `GANTRY_RACER_ENABLED`, selects UID/GID 0, and mounts
`/run/racer/gantry` read-write. It does not provision Racer. Keep existing
`GANTRY_RACER_*` tuning and registry configuration as appropriate for your release.
Unlike the Helm Racer profile, this additive override retains the legacy mounts,
ports, and libp2p init container. The init container does not mount Racer's directory.

### Verify Racer Mode

Check Gantry's rollout with `kubectl -n NAMESPACE rollout status daemonset/gantry --timeout=5m`,
then its `/readyz` endpoint and actual image pulls. Inspect
`gantry_racer_mirror_requests_total`, `gantry_racer_origin_bytes_total`, and Racer
diagnostics instead of expecting the default backend's DHT/containerd metrics.
Gantry does not fall back to its legacy backend or bypass Racer directly on Racer
failure. This does not remove containerd's independently configured registry routes.
