---
title: "Distribute Container Images with Gantry"
weight: 8
description: "Run the Gantry peer-to-peer OCI distribution agent so nodes share image layers instead of every node pulling them from the upstream registry."
---

## Overview

Gantry is the peer-to-peer OCI distribution layer in Unbounded Kubernetes. It runs a small
agent on every node that presents itself to containerd as a normal registry mirror, then
resolves digest-addressed content from three sources in order: the node's own containerd
content store, a peer that already holds the content, and finally the upstream registry
through a bounded set of designated pullers.

The problem it solves is registry fan-out. Without Gantry, a rolling update of a 1,000
node cluster makes every node request the same layers from the same registry endpoint in
a short window. The registry front end, its private endpoint, and the network path to it
all see the full fleet's worth of duplicate requests for identical bytes. With Gantry, a
small cohort of nodes fetches each unique layer once and the rest of the fleet streams
those bytes from its neighbors.

Use Gantry when your clusters pull large images repeatedly, when many nodes start the
same workload at once, or when the link to your registry is expensive or rate limited.
You do not need it for a small cluster that pulls a handful of images, and you should not
use it as an image cache in front of a registry that is already local to every node.

Gantry is a pull-path component. It serves manifest and blob reads to containerd and to
other agents; it never accepts image pushes. Content integrity comes from the OCI digest,
so a peer cannot substitute different bytes for the digest it was asked for.

A Gantry deployment needs three things: Linux nodes running containerd with a socket
Gantry can read, containerd configured to consult `/etc/containerd/certs.d` for registry
configuration, and at least one upstream registry entry. Unbounded nodes provisioned by
`unbounded-agent` already meet the containerd side of that contract.

## Measured at scale

The design targets large, homogeneous fleets where many nodes need the same layer at the
same moment. Benchmarks in this project therefore measure four things: whether a cold
rollout can place content on a large number of nodes, how much upstream registry traffic
that rollout generates, how long a new pod waits before its container starts, and how
many pods complete within a fixed rollout window.

The results depend on the cluster, the registry, the network, and the image shape, so
treat any single number as a property of its test environment rather than a guarantee.
The methodology, the test harness, and the caveats around both live in
[Distribution benchmarks]({{< relref "guides/gantry-benchmark" >}}). That page is the
reference for the numbers; this page explains the machinery that produces them.

One structural caveat is worth stating here because it shapes expectations. Peers can
only serve content that some node already holds. In a cold cluster with no prior copy of
an image, the first wave of requests has no providers, and the cold-start path must
recruit origin pullers before any peer transfer is possible. Gantry bounds that first
wave deliberately, which means the first rollout of a brand new image is slower than the
second and later rollouts of the same digest.

## Design principles

Three properties drive most of the design decisions you will encounter when operating
the agent.

Workload transparency means a node decides for itself whether and how to participate in
distribution, using only local state and the content store it can see. There is no
central catalog that must be consulted before a pull can proceed, and there is no
controller in the request path. A node that has the bytes serves them; a node that lacks
them asks the swarm and then, if necessary, asks a designated chair to fetch them.

Decentralization means discovery is content addressed. Agents publish that they hold a
digest into a cluster-scoped Kademlia DHT and look up holders of that same digest. The
only shared coordination objects are a fixed set of Kubernetes Leases that elect the
nodes allowed to pull from origin. Those Leases are not a content index: they bound
concurrency, they do not describe what exists.

Storage reuse means Gantry does not maintain a second image cache. Its content store is
the node's containerd content store, so images that kubelet already pulled are available
to peers, and images Gantry fetched for a peer are available to kubelet. The lifetime of
a blob is governed by containerd garbage collection, which keeps the agent's disk
footprint bounded by the same policy that bounds the node's image footprint.

## Architecture

![Gantry agents form a DHT-backed peer network and a small chair cohort in front of the upstream registry](../../img/gantry-architecture.svg)

Each agent runs a libp2p host with a persisted identity, joins a cluster-scoped Kademlia
DHT using the `/gantry` protocol prefix, and participates in server mode. Server mode is
required because every agent is also a content provider: a client-mode peer would not
store provider records for other nodes. Provider records are keyed by a CIDv1 that wraps
the raw 32 bytes of the OCI digest, so any agent can derive the lookup key for a digest
without contacting a central service.

The persisted identity file under `/var/lib/gantry/libp2p` is what makes peer IDs stable
across restarts. Losing that file is not catastrophic, because provider records age out,
but it churns the routing table and invalidates the addresses peers recorded for the
node.

Bootstrap has two inputs. `libp2p_bootstrap_peers` accepts a static list of multiaddrs,
which is useful for a fixed set of seed nodes. In normal operation the dynamic bootstrap
pool comes from the chair Leases: each Lease records the holder's dialable addresses, so
a starting agent can connect to the current chair holders and join the DHT from there.
The connection manager trims idle connections above `libp2p_conn_manager_high` back to
`libp2p_conn_manager_low`, which keeps a large fleet from converging on a full mesh as
lookups open connections to every peer they walk.

Provider records are refreshed by a sweeping provider. `dht_provider_validity` bounds how
long a record may be served after its last publication, and `dht_reprovide_interval`
spreads refreshes across the interval with up to `dht_max_reprovide_delay` of extra delay.
Validation requires the interval plus the maximum delay to stay below the validity, so a
record cannot expire before the sweeper is due to renew it. A background advertiser also
reconciles its inventory against the local content store every
`advertise_reconcile_interval`; the fast path is an eager publish when a stream commits,
and the reconcile is the backstop that catches missed events and drives withdrawals.

The agent exposes five listeners. Each has a distinct trust level, which matters when you
write network policy. Two terms recur below. A provider is any agent that has advertised
a digest it can serve. A chair is an agent that currently holds one of a fixed set of
Kubernetes Leases; only chairs may pull from the registry during cold start, which is how
the fleet bounds origin concurrency.

| Port | Listener | Purpose |
| --- | --- | --- |
| `5000` | `mirror_listen` | The containerd-facing OCI mirror. Loopback by default; the default chart profile binds `0.0.0.0` and publishes it as a host port on `127.0.0.1`. |
| `5001` | `transfer_listen` | Peer-facing HTTP/2 cleartext endpoint that serves blobs and digest-addressed manifests from the local content store. |
| `5002` | `chair_listen` | HTTPS endpoint for cold-start `please_pull` calls between agents. |
| `4001` | `libp2p_listen` | libp2p TCP and QUIC transport for DHT traffic and coordination. |
| `9095` | `metrics_listen` | Prometheus metrics plus the `/livez`, `/healthz`, and `/readyz` probes. |

The mirror and transfer endpoints share the same OCI URL shape, `/v2/<repository>/blobs/<digest>`
and `/v2/<repository>/manifests/<digest>`, which lets the same client code talk to either
one. They differ in policy: the mirror may resolve an unknown digest through peers or
cold start, while the transfer endpoint only ever serves what is already on disk.

## How Gantry Handles a Pull

A pull passes through the mirror listener on port 5000. containerd selects that listener
because its `hosts.toml` for the registry lists it first, and it sends the registry name
as the `?ns=` query parameter whenever the file also declares a `server`. Gantry uses
`?ns=` to choose which upstream entry applies. If you configure exactly one upstream, the
parameter is optional and ignored when present. If you configure several, a request whose
`?ns=` matches none of them is answered with `404`, and the registry must be added to
`upstream_registries` before its requests can be mirrored.

### Tag resolution and digest-addressed requests

Gantry does not resolve tags. A request for `/v2/<repository>/manifests/<tag>` receives
`503` with an empty body, which is deliberate: containerd only falls through to the next
mirror entry on a 5xx response, so the `503` hands tag resolution to the upstream
registry. A `404` here would be treated as a hard error and break the pull.

Once containerd knows the manifest digest, every later request is digest addressed, and
that is the traffic Gantry serves. The same rule applies to manifests and blobs: a
digest-addressed request enters the resolution path, a tag-shaped request does not.

A `HEAD` request for a digest that is not cached returns metadata without warming the
cache. Caching a multi-gigabyte blob because a client asked for its size would recreate
the bandwidth amplification Gantry exists to remove. A subsequent `GET` for the same
digest follows the normal miss path and populates the node.

### Local and warm pulls

The first check is the node's own containerd content store. A hit is served directly from
disk and counted by `p2p_cache_hit_total`; this is the cheapest outcome and the common
one after the first pull of an image on that node.

On a miss, the agent asks the DHT for providers of the digest. A lookup can return records
that are no longer useful, so the agent filters candidates before dialing. It excludes
itself, because a node cannot fetch from its own transfer endpoint, and it excludes
providers it recently found to be stale or unavailable. A provider that answers `404` is
marked stale for three minutes; a provider that cannot be reached is avoided for thirty
seconds. Both windows are local decisions, not cluster-wide bans.

For each remaining candidate, the agent issues a peer request to the transfer endpoint
with the `Gantry-Mirrored: 1` header. That header is the loop breaker: the peer either
serves the requested bytes from its content store or returns `404`, and it never starts
its own lookup, cold start, or origin pull. Without the header the endpoint rejects the
request with `400`, which prevents two agents from recursing into each other's miss paths.

The response body is hashed as it streams. Before the content is committed to the
containerd store under the requested digest, the computed hash must equal the digest, so
a corrupted or malicious peer response cannot poison the store. A mismatch quarantines
that provider and fails the attempt.

A completed transfer is committed through containerd with a content lease, and the agent
advertises the new digest immediately. Other nodes that were waiting on the same layer
see the provider record and join the warm path without contacting the registry. During a
rollout the provider set therefore grows from the initial cohort outward, and each new
node adds capacity instead of load.

If a peer stream fails partway through, the requester can resume from the verified prefix
with an HTTP `Range` request against another provider or the same one. The transfer
endpoint answers a satisfiable range with `206 Partial Content` and a correct
`Content-Range`, so a large layer does not restart from zero because one provider
disappeared.

### Cold pulls

When a digest has no providers, the agent falls back to cold-start coordination. This is
the path that protects the registry: instead of every requester opening an origin pull,
a small cohort is elected to fetch the content and the rest of the fleet waits for the
resulting provider records.

The requester takes a snapshot of the chair Leases and ranks the selectable chairs for
the digest with highest-random-weight hashing. The ranking is deterministic for a digest,
so independent requesters tend to choose the same chairs and duplicate origin work is
suppressed without any shared lock. The cohort size comes from `chair_seed_count`, scaled
down proportionally when fewer chairs are selectable than `chair_count`.

The requester then calls `please_pull` on each chair in the cohort over HTTPS on port
5002. It pins the chair's self-signed certificate to the peer ID published in that chair's
Lease, so the call cannot be redirected to another node, and it includes the Lease
generation it saw so a chair that has been replaced during rotation can decline. A chair
can answer in four ways: it started the pull, it is already pulling that digest, it
recently failed with a class the requester can trust, or the requester's view of the chair
is stale. A `started` or `already_pulling` answer means the work is under way; a trusted
failure short-circuits the cascade so requesters do not pile onto an origin that is
failing for everyone.

The chair pulls the content from the upstream registry, respecting
`coord_max_concurrent_pulls` and the origin progress timeout, commits it to its local
containerd store, and advertises it. The waiting requester polls the DHT for the digest
while this happens. When a provider appears, the request continues on the warm path,
which is why a cold pull and a warm pull share the same client code after the wait.

Chairs also prefetch manifest children so a rollout does not serialize on discovery of
each layer. `prefetch_coordinator_replicas` limits how many consumers dispatch prefetch
requests, `prefetch_max_concurrent_groups` bounds simultaneous outbound groups per
manifest, and `prefetch_dispatch_jitter` spreads the first wave so a cold image does not
put every node on origin at the same instant. Prefetch is best effort: a failed group is
reported and retried by a later request rather than failing the pull.

If the cascade runs out of chairs without a provider, or a chair reports a transient
failure, the requester does not immediately pull from origin itself. A direct origin pull
requires the fallback gate to pass: the bootstrap window must have elapsed or the routing
table must have grown past `bootstrap_routing_table_pct`, the DHT must look healthy
enough, no deduplication collision may exist, a per-node token bucket must admit the
pull, and a jittered delay must elapse without a provider appearing. When the gate
refuses, the mirror returns `503` and containerd falls through to the registry directly,
which preserves the pull at the cost of one origin copy.

## Failure recovery

Gantry treats peer failures as expected events and classifies them rather than retrying
indiscriminately. The following behaviors are the ones you will see in logs and metrics.

A peer that answers `404` is stale for that digest: its provider record outlived the
blob, usually because containerd garbage collection removed it or a node evicted the
image. Gantry skips that provider for three minutes and continues with the next
candidate. The request does not fail, and a later request may find the record gone.

A peer that answers `429` is alive but at capacity. The transfer endpoint sheds blob
serves above `transfer_max_concurrent_serves` so that one popular node does not accept
more streams than it can feed. The requester honors the `Retry-After` hint when present,
re-discovers providers, and moves to another peer rather than waiting on the busy one.

A peer that cannot be dialed is avoided for thirty seconds. Connection refused and dial
timeouts are the common cases. If the requester had already written response headers and a
verified prefix, it closes the truncated response so containerd can retry, and the retry
resumes from a verified offset.

A digest mismatch is a different class entirely. The stream is aborted before commit, the
provider is marked suspicious for five minutes, and the failure is counted. Repeated
mismatches between the same pair of nodes point at storage corruption or a bug, not at
network flakiness.

Cold-start failures are handled by the chair cascade. A chair that reports
`already_pulling` returns progress information rather than starting a second pull, so a
thundering herd of requesters for the same digest converges on one origin fetch. If a
chair's Lease expires, requesters see it as stale, rank the remaining chairs, and
continue. Chair rotation reserves a successor before an epoch boundary, so a healthy
cluster does not have a window where a digest has no eligible chair.

Finally, failure classes are shared where that is safe. Authentication failures, not
found responses, and rate limiting from origin are trusted cluster-wide for a cooldown
window, because retrying them from a different node will not help. Transient failures are
honored only locally, because a different path or a later attempt frequently succeeds.

## Security and integrity

Every peer transfer is verified against the OCI digest. The stream hash is computed while
the body is read and compared before the content is committed, so a peer cannot return
different bytes for a digest and have them accepted. containerd verifies the same digest
again when the content is ingested, which means an integrity failure surfaces even if the
agent's own check were bypassed.

The DHT and the chair RPC use different protections. libp2p traffic is encrypted with
Noise, and the chair listener serves TLS with a self-signed certificate derived from the
agent's libp2p identity key. A requester verifies that certificate by recovering the peer
ID from it and comparing that with the peer ID published in the chair's Lease, so there is
no certificate authority to operate and no way to redirect the call to another node. The
chair separately validates that it still owns the Lease generation named in the request,
so a chair that has been replaced during rotation refuses the work instead of splitting
the cohort.

The peer transfer endpoint on port 5001 is HTTP/2 cleartext by design. It assumes the
cluster network is trusted and relies on network policy, the `Gantry-Mirrored` header
gate, and digest verification rather than on transport encryption. If you need
on-the-wire confidentiality between nodes, layer a service mesh with mutual TLS above
Gantry rather than trying to terminate TLS on the transfer listener.

The mirror listener is loopback-only by default, and validation rejects a non-loopback
bind unless `mirror_bind_allow_non_loopback` is true. The shipped chart sets that opt-in
because it publishes port 5000 as a host port with `hostIP: 127.0.0.1`: the socket facing
the cluster is still reachable only from the node, and the wildcard bind inside the pod is
what makes the host port work. If you run the agent outside Kubernetes, keep the default
and leave the mirror on the loopback interface.

Registry credentials are handled per request. With requester-delegated authentication,
containerd's credential travels with the request, the agent relays the registry challenge,
and the authorization containerd supplies is used for that request and then discarded
rather than cached on the node. Shared-identity mode exists for compatibility and reads a
credential file at startup, which means the secret is present on every node that runs the
agent.

The agent's Kubernetes permissions are deliberately narrow. It can get, list, create,
and update `leases` in its own namespace, and it can get its own DaemonSet so it knows how
many holders the chair pool can have. It runs no informer and opens no watch, needs no
access to Pods or Nodes, and should never be granted a `ClusterRole`.

## Deployment options

There are two supported ways to install Gantry, and they are mutually exclusive in a
cluster.

An operator-managed installation uses the manifests embedded in the `unbounded-operator`
binary. The operator applies them with its own image, ConfigMap, and Lease ownership
semantics, installs into the `unbounded-system` namespace, and never runs Helm. Gantry is
enabled by default for a Site, so an operator-managed cluster normally has it unless a
Site explicitly disables it.

A standalone installation uses the released OCI Helm chart. Choose this when the cluster
has no operator, or when you want the chart's node-side configuration reconciler to own
the containerd mirror route. The chart installs its own DaemonSet, ConfigMap, ServiceAccount,
PriorityClass, chair Leases, and optionally the containerd host configuration.

Both paths mark ownership with `PriorityClass/gantry-low`. The chart refuses to install
when that object already belongs to the operator or to a different release, and the
operator makes the corresponding check. Clear the old installation rather than deleting
the PriorityClass by hand, because the marker is what prevents two managers from fighting
over the same DaemonSet.

## Installation

### 1. Confirm compatibility

Start by confirming that every node you intend to cover meets the runtime requirements.

- The node runs Linux with containerd, and the socket is readable at
  `/run/containerd/containerd.sock` (or at the path you set in `containerd.socketPath`).
  Gantry reads and writes content through this socket; the default backend has no
  alternative storage, and the Racer backend described below replaces it rather than
  adding to it.
- containerd is configured to read `/etc/containerd/certs.d` as its CRI registry
  `config_path`. Gantry does not edit containerd's main configuration and does not restart
  the service. If the directory is not consulted, the mirror entry has no effect.
- Either `unbounded-agent` already owns the node's containerd route, or you let the chart
  manage it. On Unbounded nodes the managed `_default/hosts.toml` mirror entry is already
  present, and the mirror activates as soon as the agent listens on `127.0.0.1:5000`.
- The namespace has no existing Gantry installation from the other deployment path. A
  `PriorityClass/gantry-low` owned by `unbounded-operator` causes the chart to fail with
  an ownership error rather than adopt it.

The DaemonSet mounts the containerd runtime directory rather than the socket file, so a
containerd restart that replaces the socket is observed by the existing pod. The agent
runs as UID 65532 with primary GID 0 because many node images expose `containerd.sock` as
`root:root` with mode `0660`. Check the ownership and mode on your node image before a
production rollout; if your runtime uses a dedicated socket group, set
`containerd.socketGroup` to it.

### 2. Install the chart

Install a released chart version and pin the agent image by digest. Using a digest rather
than a tag keeps the rollout reproducible when a tag is moved.

```bash
helm upgrade --install gantry oci://ghcr.io/azure/charts/gantry \
  --version <release-without-v> \
  --namespace gantry-system \
  --create-namespace \
  --set image.digest=sha256:<gantry-image-digest> \
  --set gantry.upstreamRegistries[0].name=registry.example.com \
  --set gantry.upstreamRegistries[0].endpoint=https://registry.example.com
```

Replace `<release-without-v>` with the chart version without its leading `v`, and
`<gantry-image-digest>` with the digest published for that release. At minimum you must
configure one upstream registry; validation rejects a configuration with none.

The release creates the agent DaemonSet, its ConfigMap, a ServiceAccount with the Lease
permissions described earlier, the `gantry-low` PriorityClass, and one Lease per chair.
It also creates a second DaemonSet for node configuration unless you disable it. That
reconciler writes the chart-owned `hosts.toml` payload, checks it every
`nodeConfig.reconcileIntervalSeconds` (five seconds by default), and atomically restores
it when it is missing or different, including after a node upgrade or a manual edit.

Upgrade with `helm upgrade` and keep using the chart. If another system owns the host
configuration, set `nodeConfig.enabled=false` and manage the registry files yourself; the
chart then does not write or remove them. Uninstalling removes only unchanged chart-owned
files, and only on nodes where the terminating pod can run. A node that was unavailable
during uninstall keeps a stale entry that points containerd at a listener that no longer
exists, so check those nodes and remove the file.

### 3. Configure upstream registries

`upstream_registries` is the allowlist of registries Gantry mirrors. Each entry has a
`name`, an `endpoint`, and optional `credentials_path` and `ns_alias` fields. The `name`
must match the host, and port when one is used, that containerd addresses for that
registry; it is also the value the mirror expects in `?ns=` when several registries are
configured.

```yaml
upstream_registries:
  - name: registry.example.com
    endpoint: https://registry.example.com
  - name: ghcr.io
    endpoint: https://ghcr.io
    credentials_path: /etc/gantry/registry/ghcr
```

Validation requires at least one entry, a non-empty unique `name`, and an `endpoint` that
starts with `http://` or `https://` and contains a host. A duplicate name, or an
`ns_alias` that collides with another name or alias, is rejected at startup rather than
silently shadowing an entry.

A registry that is not in the list is not mirrored. When containerd asks for it, the
mirror logs a warning and answers `404`, so that pull makes no use of Gantry and falls
back to the registry on its own. If you add a registry to the cluster but forget this
list, the symptom is a normal but slow pull and a repeating warning in the agent log.

The `endpoint` is the origin Gantry reads from, not the address containerd uses. In the
common case they are the same host. They differ for Artifact Streaming, where the
endpoint points at the node-local AKS mirror, and for registries fronted by a proxy.

### 4. Optional: enable ACR Artifact Streaming

ACR Artifact Streaming serves an image as a sealed OverlayBD artifact, which lets a
container mount before its full layer has been downloaded. Gantry can act as the
node-local range proxy for that format, adding peer reuse to the streaming reads ACR
already supports.

This integration requires Linux nodes that support NBD, ext4, and EROFS when the image
selects it, plus shared mount propagation. It consumes the existing OverlayBD v0.1.0
artifact contract without running the OverlayBD daemon; Gantry supplies its own
`gantry-overlaybd` proxy snapshotter and delegates ordinary OCI layers to a private
`overlayfs` snapshotter.

For a standalone Helm installation, enable the range endpoint and the host configurator
together, and point the upstream at the node-local AKS mirror. The chart switches the
agent to host networking in this mode so it can reach the loopback-only mirror.

```yaml
nodeSelector:
  gantry-streaming: "true"

gantry:
  artifactStreaming:
    enabled: true
  upstreamRegistries:
    - name: <registry>.azurecr.io
      endpoint: http://127.0.0.1:8578?ns=<registry>.azurecr.io

overlaybdConfig:
  enabled: true
  nodeSelector:
    gantry-streaming: "true"
```

The host configurator validates that `overlaybdConfig.address` is a socket directly under
`/run/gantry-overlaybd`, that `overlaybdConfig.gantryEndpoint` is a loopback HTTP `/blobs`
endpoint, and that the snapshotter root is absolute. It records the previous containerd
configuration before enabling `/etc/containerd/conf.d/*.toml`, restarting containerd only
when the effective configuration changes, and refuses to overwrite a conflicting file
written by another owner.

For an operator-managed installation, enable it through every participating `Site` with
the same selector. The operator rejects an empty selector, a Site that enables Artifact
Streaming while disabling Gantry, and conflicting selectors across Sites.

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Site
metadata:
  name: <site-name>
spec:
  components:
    gantry:
      enabled: true
      artifactStreaming:
        enabled: true
        nodeSelector:
          kubernetes.azure.com/agentpool: <streaming-node-pool>
```

The adapter DaemonSet uses `OnDelete`, because the snapshotter owns live NBD devices that
cannot be handed to a replacement pod while in use. To roll it, cordon and drain the
workloads that use the Gantry snapshotter, delete the adapter pod on that node, bootstrap
its replacement with native `overlayfs`, verify readiness, restore `gantry-overlaybd` for
CRI image unpacking, and then uncordon. To disable streaming, drain first, disable the
configurator while leaving the range endpoint enabled, wait for the configurator to
restore the host configuration, and then disable the range endpoint.

### 5. Private registry authentication

Gantry supports two authentication models. Prefer requester-delegated authentication,
which keeps the node free of long-lived registry identity.

#### Requester-delegated authentication

In this mode, containerd's own credentials are used for the origin pull. When a digest
request arrives without an `Authorization` header and the configured upstream has no
credential file, the mirror asks the registry's `/v2/` endpoint for its challenge and
relays that challenge to containerd as a `401` with a `WWW-Authenticate` header.
containerd answers with the credential kubelet already holds, typically after exchanging
it for a registry token itself, and the agent forwards that Basic or Bearer value to the
origin unchanged. The agent never swaps in a different identity when the origin rejects
the delegated one; it returns the failure instead.

The resulting authorization is attached to the request context and travels with the
`please_pull` call to the chair, so the chair can pull from the origin without holding a
registry identity of its own. It is normalized to Basic or Bearer, bounded in size, and
discarded when the request finishes; it is never written to disk or reused for another
request.

Challenge discovery and credential delegation are HTTPS-only. An anonymous HTTP registry
still works, because the agent reports no challenge and the pull proceeds anonymously,
but a private HTTP registry cannot use this mode. Use an HTTPS endpoint or shared-identity
mode for that case.

#### ACR attachment and explicit authentication on Flex nodes

For operator-managed clusters, `kubectl unbounded gantry attach-acr` adds an existing ACR
to `upstream_registries` without replacing unrelated Gantry configuration. It reads the
operator's `gantry-config` ConfigMap, so operator-managed Gantry must already be
installed.

```bash
kubectl unbounded gantry attach-acr \
  --acr-resource-id /subscriptions/<subscription>/resourceGroups/<group>/providers/Microsoft.ContainerRegistry/registries/<registry> \
  --kubeconfig <path-to-kubeconfig>
```

The command takes full Azure resource IDs and accepts an ACR and an AKS cluster in
different subscriptions within the same tenant. `--auth-mode` defaults to `external`,
which preserves the existing authentication path and changes nothing about how nodes
authenticate. Set `--auth-mode aks-identity-binding` together with
`--cluster-resource-id` to install the stock ACR credential provider on current and
future Linux Flex nodes. That mode prints a warning because it may restart kubelets on
all targeted nodes concurrently; nodes are not drained automatically, so you own the
impact on running workloads. The installer also refuses to proceed when kubelet already
uses a different external credential provider.

Use `--namespace` when the operator and Gantry live somewhere other than the default
system namespace, and `--timeout` to bound the node configuration wait, which defaults to
ten minutes.

#### Shared-identity authentication

Shared-identity mode exists for compatibility with clusters that cannot delegate a
credential per request. You create a Secret containing a credentials file, mount it, and
point the registry entry's `credentials_path` at the mounted file. Each file holds one
`username:password` pair, or a `_json_key:<json>` value for the well-known Google
registry pattern.

Setting `credentials_path` opts that registry into this mode for requests that arrive
without delegated authorization. The agent reads the file eagerly at startup, so a
missing or unreadable file fails the pod instead of failing the first pull. Because the
secret is present on every node, prefer requester-delegated mode unless you have a
specific reason to keep a node-local identity.

### 6. Verify distribution

Roll out the DaemonSet and confirm the probes turn green before testing a pull.

```bash
kubectl -n gantry-system rollout status daemonset/gantry
kubectl -n gantry-system get pods -l app.kubernetes.io/name=gantry
```

Then check readiness and metrics through the operations port. The `/readyz` endpoint is
the same check the kubelet uses: it fails while the containerd content store is
unreachable, while the DHT routing table is empty during bootstrap, and while no chair
Lease is selectable.

```bash
kubectl -n gantry-system port-forward pod/<gantry-pod> 9095:9095
curl -s http://127.0.0.1:9095/readyz
curl -s http://127.0.0.1:9095/metrics | grep '^p2p_dht_health_score'
```

To exercise peer reuse, pull a multi-layer image on two nodes and compare the counters.
On the first node, `p2p_origin_pull_total` should advance and `p2p_peer_fetch_total`
should not. On the second node, after the first node has advertised, a pull of the same
digest should increase `p2p_peer_fetch_total` and leave the origin counters alone. If the
second node also pulls from origin, the peers are not discovering each other, which the
troubleshooting section covers.

## Configuration reference

The agent accepts configuration from four layers, applied in order: built-in defaults, a
YAML file passed with `--config`, `GANTRY_*` environment variables, and command-line
flags. A later layer wins over an earlier one. Unknown YAML keys are an error, so a typo
in a ConfigMap fails the pod instead of silently keeping the default. Environment
variables cover scalar fields only; list fields such as `upstream_registries` and
`libp2p_bootstrap_peers` must come from the file.

The chart exposes a small set of values and writes a complete `config.yaml`; setting
`gantry.config` replaces that generated file entirely, so use it only for options that
have no dedicated value.

### Listeners and identity

| Key | Flag | Default | Purpose |
| --- | --- | --- | --- |
| `mirror_listen` | `--mirror-listen` | `127.0.0.1:5000` | containerd-facing mirror. Must be loopback unless the non-loopback opt-in is set. |
| `mirror_bind_allow_non_loopback` | `--mirror-bind-allow-non-loopback` | `false` | Permits a wildcard mirror bind, as the host-port chart profile requires. |
| `transfer_listen` | `--transfer-listen` | `0.0.0.0:5001` | Peer-facing transfer endpoint. |
| `chair_listen` | `--chair-listen` | `0.0.0.0:5002` | HTTPS `please_pull` endpoint. |
| `metrics_listen` | `--metrics-listen` | `0.0.0.0:9095` | Metrics and probes. |
| `pprof_listen` | `--pprof-listen` | empty | Optional Go profiles; must be a loopback address when set. |
| `libp2p_listen` | none | empty | Multiaddrs to advertise. The chart sets TCP and QUIC on port 4001. |
| `libp2p_identity_path` | `--libp2p-identity-path` | `/var/lib/gantry/libp2p/libp2p.key` | Persisted peer identity; the chart writes `identity.key` in the same directory. |
| `libp2p_bootstrap_peers` | none | empty | Static multiaddrs to seed the DHT. Chair Leases provide the dynamic pool. |
| `storage_mode` | none | `containerd` | Only `containerd` is accepted; other values fail validation. |
| `containerd_socket` | `--containerd-socket` | `/run/containerd/containerd.sock` | Required content-store socket. |
| `containerd_namespace` | `--containerd-namespace` | `k8s.io` | Namespace the image-event subscription watches. |
| `containerd_lease_ttl` | `--containerd-lease-ttl` | `60m` | TTL of content leases the agent attaches on ingest. |

### Chair and peer coordination

| Key | Flag | Default | Purpose |
| --- | --- | --- | --- |
| `chair_count` | `--chair-count` | `64` | Maximum active chair pool; bounded further by the DaemonSet's desired pod count. |
| `chair_seed_count` | `--chair-seed-count` | `8` | Per-digest seed cohort at a full pool. Must be between 1 and `chair_count`. |
| `chair_namespace` | `--chair-namespace` | empty | Namespace holding the chair Leases. |
| `chair_cluster_size_estimate` | `--chair-cluster-size-estimate` | `100000` | Sizes observation and fallback without pod watches. |
| `chair_lease_duration` | `--chair-lease-duration` | `5m` | Heartbeat expiry for a holder. |
| `chair_renew_period` | `--chair-renew-period` | `1m` | Heartbeat renewal, must be below the lease duration. |
| `chair_rotation_period` | `--chair-rotation-period` | `6h` | Wall-clock assignment epoch. |
| `chair_rotation_lead` | `--chair-rotation-lead` | `5m` | Time before an epoch boundary to reserve a successor. |
| `peer_fetch_timeout` | `--peer-fetch-timeout` | `15m` | Ceiling on one complete peer fetch, including body transfer and commit. |
| `peer_rediscover_budget` | `--peer-rediscover-budget` | `5m` | Wall-clock budget for re-discovery rounds; zero disables them. |
| `peer_rediscover_backoff` | `--peer-rediscover-backoff` | `1s` | Pause between re-discovery rounds. |
| `transfer_max_concurrent_serves` | `--transfer-max-concurrent-serves` | `10` | Concurrent peer blob serves before the endpoint returns `429`; zero is unlimited. |
| `transfer_max_concurrent_range_serves` | `--transfer-max-concurrent-range-serves` | `512` | Concurrent bounded-range serves, admitted separately. |

### Discovery, origin, and fallback

| Key | Flag | Default | Purpose |
| --- | --- | --- | --- |
| `dht_provider_validity` | `--dht-provider-validity` | `6h` | How long a provider record may be served after its last publication. |
| `dht_reprovide_interval` | `--dht-reprovide-interval` | `3h` | Interval over which the sweeper refreshes all local content. |
| `dht_max_reprovide_delay` | `--dht-max-reprovide-delay` | `10m` | Extra delay allowed beyond the scheduled reprovide. |
| `advertise_reconcile_interval` | `--advertise-reconcile-interval` | `1m` | Background inventory reconcile as a backstop to eager publish. |
| `coord_max_concurrent_pulls` | `--coord-max-concurrent-pulls` | `16` | Background origin pulls a chair may run from inbound `please_pull`. |
| `coord_max_digests_per_request` | `--coord-max-digests-per-request` | `256` | Digests accepted in one batch request. |
| `origin_pull_progress_timeout` | `--origin-pull-progress-timeout` | `5m` | Maximum time a detached origin body may make no progress; zero disables. |
| `nf5_jitter_base` | `--nf5-jitter-base` | `3s` | Base delay for the direct-origin fallback jitter window. |
| `nf5_per_node_rate_limit` | `--nf5-per-node-rate-limit` | `2` | Direct-origin fallbacks per node per minute. |
| `bootstrap_window` | `--bootstrap-window` | `30s` | Period after startup during which an empty DHT is not trusted as cold start. |
| `bootstrap_routing_table_pct` | `--bootstrap-routing-table-pct` | `25` | Routing-table size percentage that ends the bootstrap window. |

Two tuning traps are worth calling out. `peer_fetch_timeout` is a safety ceiling, not a
stall detector; setting it below the transfer time of your largest layer makes requesters
abandon transfers that are making progress. `chair_seed_count` trades origin copies for
cold-start speed: a larger cohort produces more initial seeds and a wider fan-out, at the
cost of that many origin pulls per digest.

## Operating and verifying a pull

A healthy pull on an established image is short and quiet. containerd asks the mirror for
a manifest digest, gets it from the local store or a peer, then asks for each layer and
gets those from peers. The agent logs the request path with registry, repository, digest,
and kind fields, and emits a trace span for each request when tracing is enabled. The
[tracing guide]({{< relref "guides/gantry-tracing" >}}) covers exporter setup and the span
hierarchy.

Start with the node-side route, because a pull that bypasses the mirror entirely looks
identical to a working pull from the application's point of view.

```bash
cat /etc/containerd/certs.d/_default/hosts.toml
grep -R "config_path" /etc/containerd/config.toml
```

The hosts file must list `http://127.0.0.1:5000` as a host and must declare the pull and
resolve capabilities. If the operator or the chart owns the file, edits are reverted on
the next reconcile, which is the intended behavior. If containerd's `config_path` does
not point at `/etc/containerd/certs.d`, the file is ignored.

You can probe the mirror directly from the node. A manifest or blob request needs a real
digest and, when several registries are configured, the matching `?ns=` value.

```bash
curl -s http://127.0.0.1:5000/v2/
curl -sI "http://127.0.0.1:5000/v2/<repository>/manifests/sha256:<digest>?ns=<registry>" \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json'
curl -s -o /dev/null -w '%{http_code} %{size_download}\n' \
  "http://127.0.0.1:5000/v2/<repository>/blobs/sha256:<digest>?ns=<registry>"
```

The first call returns `{}` when the mirror is accepting requests. The `HEAD` call returns
headers with the origin's content type, which is how containerd learns the media type
before it fetches a body. The blob `GET` returns `200` and the byte count when the layer
is available from any source, or a 5xx when the agent cannot obtain it and containerd
should fall through to the registry.

To watch a pull in progress, follow the agent log while a pod starts on that node. The
mirror logs the chosen source path, and the counters tell you which path won without
parsing log lines.

```bash
kubectl -n gantry-system logs daemonset/gantry -f
```

If you enable the optional profiles listener, `kubectl port-forward` to it and use
`go tool pprof` against the loopback address. Profiles are disabled by default and cannot
bind a non-loopback address.

## Metrics

Every agent serves Prometheus metrics on `9095` at `/metrics`. Scrape all agents, not one:
the interesting signals are per-node, and cluster-wide origin traffic is the sum across
nodes. Set `metricsService.enabled=true` in the chart to render a headless Service over
the agents for annotation-driven scrapers.

The first group tells you where bytes came from.

| Metric | Type | Meaning |
| --- | --- | --- |
| `p2p_cache_hit_total` | counter | Local content-store hits on the mirror endpoint. |
| `p2p_cache_miss_total` | counter | Local misses; a miss can still be served by a peer. |
| `p2p_peer_fetch_total{outcome}` | counter | Peer fetches the mirror started, by terminal outcome. |
| `p2p_peer_serve_total` | counter | Peer requests this agent served from its own store. |
| `p2p_peer_miss_total` | counter | Peer requests that returned `404` because the blob was gone. |
| `p2p_origin_pull_total{kind}` | counter | Origin pulls started, by OCI kind. |
| `p2p_origin_pull_success_total{kind}` | counter | Origin pulls that streamed and committed. |
| `p2p_origin_pull_failure_total{kind,class}` | counter | Terminal origin pull failures by class. |
| `gantry_mirror_bytes_served_total{kind,source}` | counter | Bytes written to containerd, labeled by `cache`, `peer`, or `origin`. |
| `gantry_origin_bytes_total{kind}` | counter | Bytes read from upstream registries. |
| `gantry_peer_fetch_bytes_total{kind}` | counter | Bytes received from peers. |
| `gantry_peer_serve_bytes_total{kind}` | counter | Bytes sent to peers. |
| `gantry_active_transfers{source,kind}` | gauge | Origin or peer GETs currently in progress. |
| `gantry_transfer_bytes_total{source,kind}` | counter | Bytes received while reading, including partial and retried transfers. |

The second group describes discovery and cold start.

| Metric | Type | Meaning |
| --- | --- | --- |
| `p2p_dht_health_score` | gauge | Geometric mean of routing-table coverage, lookup latency, and self-test success, in `[0, 1]`. |
| `p2p_libp2p_conns` | gauge | Open libp2p connections, compared against the trim watermark. |
| `p2p_dht_lookup_total{outcome}` | counter | Provider lookups by outcome. |
| `p2p_dht_lookup_duration_seconds{outcome}` | histogram | Provider lookup latency. |
| `p2p_dht_provide_total` | counter | Successful provider publications. |
| `p2p_dht_provide_error_total{op}` | counter | Failed publications by call site. |
| `gantry_advertise_reconcile_total` | counter | Advertiser reconcile passes completed. |
| `p2p_cold_start_duration_seconds{digest_kind,outcome}` | histogram | Time spent in cold-start resolution. |
| `p2p_cold_start_selectable_chairs{digest_kind}` | histogram | Chairs considered selectable for a digest. |
| `p2p_cold_start_seed_chairs_accepted{digest_kind}` | histogram | Chairs that accepted a `please_pull`. |
| `p2p_in_flight_pulls` | gauge | Background pulls currently running on this agent. |

The third group covers storage, fallback, and streaming.

| Metric | Type | Meaning |
| --- | --- | --- |
| `gantry_storage_mode_info{mode}` | gauge | `1` for the active backend; expect `mode="containerd"`. |
| `gantry_containerd_lease_created_total` | counter | Content leases attached to ingested content. |
| `gantry_containerd_lease_active` | gauge | Leases currently held. |
| `p2p_origin_fallback_total` | counter | Direct-origin fallback pulls after cold-start exhaustion. |
| `p2p_origin_fallback_decline_total{reason}` | counter | Fallback requests refused by the gate, by reason. |
| `gantry_streaming_requests_total{source,outcome}` | counter | Artifact Streaming range requests by source. |
| `gantry_streaming_bytes_total{source}` | counter | Streaming bytes by source; `origin` is registry egress. |

A healthy steady state has a high cache-hit rate, peer fetches on nodes that did not
pull first, a DHT health score that stays high, and a fallback counter that stays near
zero. `p2p_origin_fallback_decline_total` growing is not a fault by itself: it means the
gate is doing its job during a cold rollout.

## Troubleshooting

Most Gantry incidents show up as a pull that works but is slower than expected, a pull
that bypasses the mirror entirely, or an agent that never becomes ready. Work from the
node outward: route, then readiness, then discovery, then origin.

Start with `kubectl -n <namespace> get pods -l app.kubernetes.io/name=gantry` and the
`/readyz` body, which names the failing condition. The reasons below are the ones the
agent emits, and each points at a specific fix.

| Readiness or log message | Likely cause | Action |
| --- | --- | --- |
| `dht routing table empty` | No peers reachable: port 4001 blocked, bootstrap peers wrong, or the cluster is still starting | Check `p2p_libp2p_conns`, network policy for TCP and UDP 4001, and `libp2p_bootstrap_peers`. |
| `containerd content store unavailable` | Socket missing, wrong path, or permission denied | Check `containerd_socket`, the mount, and the socket group. The agent cannot run without it. |
| `no selectable Lease chair` | Chair Leases absent, expired, or in another namespace | Check `kubectl -n <namespace> get leases` and `chair_namespace`. |
| `self-announce has no dialable p2p addresses` | `libp2p_listen` family does not match the pod IP family | Use `0.0.0.0` on IPv4-only, `[::]` on IPv6-only, or `:port` for dual stack. |
| `transfer listener family mismatches Pod IP` | Same mismatch on the transfer listener | Align `transfer_listen` with the pod network family. |
| `agent starting up` from the mirror | The startup gate has not been released | Fix readiness first; the gate releases only after `/readyz` passes once. |
| `ignoring request for unconfigured registry` | The registry is missing from `upstream_registries` | Add the entry and roll the agent. |
| `peer serving at capacity` | A popular provider is at `transfer_max_concurrent_serves` | Raise the cap on large nodes, or accept the requester's reselection. |
| `warm path exhausted` | No provider and the fallback gate declined | Check `p2p_origin_fallback_decline_total` by reason and DHT health. |
| Ownership error at install | `PriorityClass/gantry-low` belongs to another manager | Remove the other installation, then install. |

A pull that never reuses peers usually has one of three causes. The registry is not in
`upstream_registries`, so the mirror refuses every request for it with `404` and the pull
does not use the cache. The node's `hosts.toml` is missing or not read, so containerd
never contacts the mirror. Or port 5001 is blocked between pods, so provider records
exist but every peer dial fails; `p2p_peer_dial_failure_total` climbs while
`p2p_dht_lookup_total` returns providers. Check the three in that order.

Repeated `404` responses from a provider that used to serve a digest are expected after
containerd garbage collection. Gantry suppresses that provider for three minutes and
serves the request from another source. Investigate only if it persists for the same
digest across many nodes, which points at aggressive GC or a node that keeps advertising
content it no longer has.

A pull that hangs before any bytes arrive is usually a cold start without an eligible
chair. Look at `p2p_cold_start_selectable_chairs`: a value well below `chair_count` means
Leases are missing, expired, or filtered by epoch, and every requester is competing for
fewer pullers. `p2p_cold_start_chair_dispatch_total{reason="rpc_error"}` climbing means
chairs are reachable but slow to answer, not that they refuse the work.

Configuration errors fail the pod at startup rather than degrading silently. The message
names the field, and the common ones are an empty `upstream_registries`, a non-loopback
`mirror_listen` without the opt-in, a `chair_seed_count` above `chair_count`, a reprovide
interval plus delay that exceeds provider validity, and Artifact Streaming combined with
Racer mode. Fix the ConfigMap and restart the DaemonSet.

## Use Racer as the Backend

Gantry can serve its mirror from Racer instead of the containerd content store. Racer is
a cluster-wide page cache that stores objects in fixed 16 MiB pages, assigns each page an
owner node, and fetches a missing page from the origin once on behalf of every reader.
The Gantry cache takes a share of each node's Racer capacity, weighted by the cache's
`spec.priority` alongside any other caches on the node.

In this mode the agent does not read containerd, does not join the DHT, and does not serve
the peer transfer endpoint. The mirror forwards OCI requests to the Racer client, Racer
finds or fetches the pages, and Gantry acts as the origin for the `gantry` cache so Racer
can read from the upstream registry. Because there is no containerd store, every cached
byte is reconstructible from origin, which is why Racer treats cached data as disposable
and why Gantry's Racer path is pre-release.

Use this backend when you want a shared cache that covers more than the OCI image path, or
when you want page-level sharing without giving Gantry access to the node's containerd
store. Do not use it if you need Artifact Streaming: the two are mutually exclusive, and
validation rejects a configuration that enables both.

### Turn on Racer mode

Racer mode is controlled by the operator, not by a Gantry chart value. A standalone Helm
installation cannot select it.

The switch is the existence of a `ClusterCache` named `gantry`. Create it without
`spec.origin`, because Gantry is the origin for this cache and Racer allows only one
owner per origin socket.

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: gantry
spec:
  priority: 500
```

When the operator sees that object, it waits for Racer's dataplane DaemonSet to exist,
then rewrites the Gantry DaemonSet to set `GANTRY_RACER_ENABLED=true`, mount the Racer
client and origin socket directories, and copy the dataplane's node placement so Gantry
runs on exactly the nodes that run Racer. While it waits, the Gantry component reports
`RacerPending` and requeues. If the cache sets `spec.origin`, the operator reports
`RacerOriginConflict` and leaves Gantry in its default mode rather than fighting Racer
for the origin socket.

### Configure upstream registries

In Racer mode the operator creates `ConfigMap/gantry-config` if it is absent and never
overwrites an existing payload, so edits to `upstream_registries` survive reconciles. Each
`name` is a host, or a host and port, with no scheme, user information, path, query, or
fragment.

The allowlist has the same meaning as in containerd mode: a request for a registry that
is not listed is refused, and Racer does not read it. `credentials_path` is ignored in
this mode. Requesters must delegate credentials, and the origin pull uses the
authorization that arrived with the request. HTTPS registries support that negotiation;
anonymous HTTP registries work without credentials.

The containerd socket and the transfer listener are unused here, because there is no
containerd store and no peer network. Keep the configuration otherwise consistent across
nodes: the cache is cluster-wide, and a node with a different registry set produces
errors for requests that its peers would have served.

### Verify Racer mode

Confirm the environment variable reached the container, then watch the rollout.

```bash
kubectl -n <namespace> get daemonset gantry \
  -o jsonpath='{.spec.template.spec.containers[0].env}'
kubectl -n <namespace> rollout status daemonset/gantry
```

`/readyz` returns `503` until the Racer sockets are usable or while the agent drains. A
`200` means the client connected and the origin service is running. On the metrics side,
`gantry_racer_mirror_requests_total` counts mirror responses by method and status,
`gantry_racer_mirror_bytes_total` counts body bytes written downstream, and
`gantry_racer_origin_requests_total` plus `gantry_racer_origin_bytes_total` describe the
upstream reads Racer performed.

### Troubleshooting Racer mode

The mirror stamps a `Gantry-Racer-Request-ID` header on responses and logs the same value,
so you can correlate one failed client request with the agent log and with Racer's own
records.

A `503` from the mirror means the Racer client could not serve the request, for example
because a socket is missing or the cache is not ready. containerd falls through to the
next host entry, so the pull may still succeed from origin; the `503` is a signal that the
cache path was skipped, not necessarily that the pod failed. A `404` without a request ID
was rejected before it reached Racer, usually because the registry is not in the
allowlist.

If the Gantry pods stay pending, check that `/run/racer/gantry/client` and
`/run/racer/gantry/origin` exist on the node. The DaemonSet mounts them as host
directories, which means a pod scheduled before Racer created them waits rather than
creating root-owned directories. If the component reports `RacerPending`, the dataplane
DaemonSet does not exist yet; if it reports `RacerOriginConflict`, the cache sets
`spec.origin` and must be recreated without it.

### Turn off Racer mode

Delete `ClusterCache/gantry`. The operator removes the environment variable, the socket
mounts, and the copied placement, and the DaemonSet returns to the containerd backend on
the next reconcile. Racer itself keeps running and keeps any other caches on the node;
only Gantry's use of the `gantry` cache stops.

## Related

The benchmark methodology and measured rollout behavior are documented in
[Distribution benchmarks]({{< relref "guides/gantry-benchmark" >}}). For request-level
visibility, see [Trace Gantry pulls]({{< relref "guides/gantry-tracing" >}}). If you need
a cache that survives beyond the image path, the Racer guides start at
[Racer reference]({{< relref "reference/racer" >}}), and the cluster cache object is
described in [Racer objects]({{< relref "reference/racer-object" >}}).

For the registry that Gantry pulls from, the [CLI reference]({{< relref "reference/cli" >}})
covers `kubectl unbounded gantry attach-acr` in the context of the other plugin commands.
For node-level operations around containerd and kubelet, see
[Node operations]({{< relref "guides/operations/node-operations" >}}).
