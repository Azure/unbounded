---
title: "Racer Reference"
weight: 7
description: "This page is the complete reference for the Racer node-local object cache, including its ClusterCache API, dataplane settings, control plane, metrics, and hardware features."
---

Racer is the node-local read-through cache that serves object data to workloads through a Unix socket on each Kubernetes node. It is designed around immutable object versions: a key names content that may change, each version of that content has an ETag, and the bytes of one version never change. Racer splits each version into fixed-size pages, places those pages on a small set of owner nodes, and serves a read from local memory, local disk, a peer, or the origin in that order.

The mental model has four parts. A `ClusterCache` is the Kubernetes object that names a cache and defines its policy. The Racer dataplane is a privileged DaemonSet that stores and serves pages on every selected node. An elected control plane, embedded in the same `racer` process, publishes cluster membership, cache definitions, page keys, and the cluster page size. Clients reach the cache over `/run/racer/<name>/client/socket`, either through the Go SDK or through the injected client socket mount.

This page is the reference for operators who install, size, tune, and troubleshoot Racer. It documents the API fields, every accepted environment variable, the node requirements, the control plane, the diagnostics and metrics surface, and the hardware paths for RDMA and GPU reads. For a task-oriented introduction, start with [Racer concepts]({{< relref "concepts/racer" >}}) and the [vLLM guide]({{< relref "guides/racer-vllm" >}}).

## Components

Racer ships as one container image and one process. The process can run as a standalone dataplane, pointed at your own controller, or in managed mode, where the operator supplies Kubernetes identity and the process runs the embedded control plane on its own bounded thread.

| Component | Where it runs | Responsibility |
| --- | --- | --- |
| `racer` process | Dataplane DaemonSet Pod | Reads, verifies, stores, and serves pages; binds the peer and diagnostics listeners; optionally runs embedded cluster control |
| Embedded authority | Same Pod, managed mode only | Elects seats, publishes membership and cache definitions, rotates page keys, serves bootstrap and webhook endpoints |
| RDMA verbs adapter | Loaded by the `racer` process | Native zero-copy page transfer when the image is built with the `rdma` feature and a usable port exists |
| CUDA shim | Loaded on the first `gpu-read` | GPU discovery, allocation import, staging, and page decryption through `libracer_gpu_cuda.so.1` |
| Admission webhooks | Primary seat, ports 8443 and 9443 | Adds the object sidecar, mounts the client socket, and restricts client Pods to cache nodes |
| Go SDK | Your client Pods | `pkg/racersdk` client, origin server, and prefetch client over the cache's Unix sockets |

The dataplane does not require a separate controller Deployment. In managed mode the `racer` binary starts the authority first, waits for a bootstrap record that names the cluster ID, block device pattern, page size, and initial membership, and only then assembles its workers. If you run the dataplane without `RACER_NAMESPACE`, you must set `RACER_CLUSTER_ID` and `RACER_CONTROL_ENDPOINT` together so it can enroll with your controller.

The Go SDK is a client library, not a provisioner. It does not create caches, origins, or Kubernetes objects; those come from the `ClusterCache` API and the operator. See [Go SDK](#go-sdk) for its surface.

## Pages, keys, and versions

Racer reads and stores whole pages. `RACER_PAGE_BYTES` is a cluster-wide setting and accepts only `16777216` (16 MiB), because the client SDKs and the disk index do not yet support another size. The value is fixed when the cluster is created: the primary records the value it first published in the `racer-state` Secret and ignores later changes with a warning. A running node rejects a publication that changes the page size, and nodes refuse stored data cut at another size and recover cold rather than reinterpret it.

Every page, fill, stored record, and checkpoint carries the page size it was cut at. The last page of an object may be shorter than the page size; every other page is exactly one page.

A cache key is a canonical 32-byte value, represented by the SDK as 64 lowercase hexadecimal characters. The key is independent of a version. A strong ETag is an opaque, quoted ASCII version identifier, not a content hash. The wire identity of an object version combines three values:

| Field | Meaning |
| --- | --- |
| `cache` | The `ClusterCache` UID, a UUID string, used as the wire `CacheID` |
| `key` | The 32-byte canonical object key |
| `etag` | The strong ETag that pins one immutable version |

A page identity adds a zero-based page number to the version identity. Placement hashes the cache UID, key, and page number, so page ownership excludes the ETag, while page cache and in-flight fetch identity include it. One read pins one version for all of its pages, even when it fetches pages at different times. Two separate reads of the same key may see different versions unless the caller passes a pinned ETag.

Each page has up to three owners, chosen by weighted rendezvous hashing over the cache UID, key, and page number. A node's `shares` value is its weight, so a node with more shares is ranked first for proportionally more pages. A reader prefers a direct owner, then a member ranked just below the owners that may still hold a copy, then the origin. Ownership is per page and per cache pool, not per object, so the pages of one object can be spread across owners.

The disk store keeps an indexed page per version and uses a checkpoint to rebuild its page index after a restart. Checkpoints are bound to the cluster ID and the slab layout; a checkpoint from another cluster or an incompatible layout is skipped, and the cache starts cold. Cached bytes are disposable by design: a cache has relative capacity shares but no durability promise.

## ClusterCache

`ClusterCache` is a cluster-scoped custom resource in the `racer.unbounded-cloud.io/v1alpha1` API group. The plural resource name is `clustercaches` and the short name is `ccache`. It is the only custom resource in this group. The operator installs and manages a Racer dataplane for every `ClusterCache`, and a cache is not a `PersistentVolume` or `PersistentVolumeClaim`; it has no volume lifecycle, no `status` field, and no per-cache workload to schedule.

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: models
spec:
  priority: 500
```

The schema has three optional fields plus `origin`:

| Field | Type | Effect |
| --- | --- | --- |
| `spec.origin` | `ObjectOrigin` | Reads objects directly from Azure Blob Storage instead of a self-hosted origin socket; fixed at creation |
| `spec.priority` | integer, 0 to 1000 | Relative share of node cache capacity; unset means 500 |
| `spec.passthrough` | `Passthrough` | Proxies reads to the origin without caching |
| `spec.nodeSelector` | `metav1.LabelSelector` | Restricts which Racer nodes store, serve, and read the cache |

Names are limited to 82 characters so that `/run/racer/<name>/origin/socket` fits in a Linux `sockaddr_un`, and each DNS label is limited to 63 characters. The limits are enforced by CEL rules on the resource, and Kubernetes metadata validation enforces DNS subdomain spelling.

The Kubernetes UID is the cache's wire identity. It is the `CacheID` in publications and the identity used by page keys and disk checkpoints. Socket paths use `metadata.name`, which is why the name has a length limit. Because entries are keyed by UID and not by source, changing the object source in place would serve entries fetched from a different source. The API therefore makes the presence of `spec.origin` immutable: you can neither add nor remove it after creation. A different origin requires a new `ClusterCache`, which has a new UID and therefore an empty cache.

Each cache has two socket endpoints under `/run/racer/<name>/`:

| Endpoint | Path | Purpose |
| --- | --- | --- |
| Client | `/run/racer/<name>/client/socket` | Reads, metadata, prefetch, host reads, and GPU reads |
| Origin | `/run/racer/<name>/origin/socket` | A self-hosted origin registers here and supplies cache misses |

The client directory is mounted into client Pods, usually by the admission webhook, and the origin directory is owned by the unprivileged origin user (UID and GID 65532). The dataplane creates the origin directory and assigns it to that user so an origin never needs root or `mkdir` rights under `/run/racer`. Origin directories are separate from client directories, so a Pod receives only the endpoint its mount grants. A client that can reach an origin socket talks to the origin directly; Racer does not automatically bypass the cache for such clients, so any direct origin access must be configured explicitly.

### Priority

`spec.priority` is a relative weight from 0 to 1000. When unset, the effective priority is 500. You may change it at any time; the new weight applies without a restart and does not invalidate cached pages by itself.

Disk shares are weighted across published caches, plus any unpublished caches that still hold data on the node, which count at the default weight. Memory shares are weighted across caches that are actively using memory, and each cache also receives a bounded working allowance from shared memory. Request-serving resources that are split among caches, such as pending queues and in-flight budgets, use an equal split rather than the priority weight.

A priority of 0 receives no weighted share while caches with a positive priority compete, but it keeps its working allowance. That allowance lets a zero-priority cache make progress and hold a small working set. If every counted cache has priority 0, they share equally.

Eviction prefers data from caches that use more than their share, within bounded scans. It is a preference, not a guarantee: an over-share cache can keep a page when the scan does not reach it, and an under-share cache does not reserve capacity it is not using. Idle capacity is available to other caches. Memory entries follow the same idea: a cache at its allowance retains entries while memory pressure permits, and releases them when pressure rises.

{{< callout type="note" >}}
Priority is a share of node cache capacity, not a quota and not a request admission limit. Raising it changes which pages an eviction scan prefers; it does not promise that a given byte count stays resident.
{{< /callout >}}

### Passthrough

`spec.passthrough` turns off caching for a cache. Racer proxies each read to the origin and keeps no data. Pages are not written to disk, and a page is held in memory only while concurrent readers share an in-flight origin read. Object metadata such as the ETag, size, and expiry is not reused across reads, so each read revalidates with the origin. A single read still pins one object version for all of its pages.

`spec.passthrough.route` selects which node's origin serves a read:

| Route | Behavior |
| --- | --- |
| `Peers` | Routes each page to its owner nodes through the mesh, as for a cached cache. The owner calls its local origin, concurrent reads of the same page anywhere in the cluster share one origin read, and many pages can be in flight at once. Nothing is retained after the readers finish. This is the default when the route is unset. |
| `Local` | The reading node calls its own local origin directly and never asks peers. Concurrent reads of the same page on one node still share one origin read. With this route, every reading node needs an origin; a managed `spec.origin` is served by every dataplane node. |

Passthrough affects both managed and self-hosted origins. The `Peers` route preserves cluster-wide single-flight behavior and pipelining, while `Local` removes the peer hop and the need to route pages to owners.

Prefetch requests for a passthrough cache are refused with `409` and the code `passthrough`. The `spec.priority` weight still applies to in-flight memory use, so a passthrough cache competes for memory while its origin reads are active.

You may add, change, or remove `spec.passthrough` at any time. When a cache switches to passthrough, data previously cached for it is dropped. The metric `racer_passthrough_local_origin_fills_total` counts origin fills served under the `Local` route.

### Node selector

`spec.nodeSelector` selects, by Node labels, the Racer nodes that store, serve, and read the cache. When it is unset or empty, every Racer node participates. The selector is a standard `metav1.LabelSelector`, so it supports `matchLabels` and `matchExpressions`.

The admission webhook restricts client Pods that use the cache to matching nodes. It adds the selector to the Pod's required node affinity, and when a Pod already has `spec.nodeName` set it looks up that Node and rejects the Pod if the Node does not match. That check matters because node affinity does not constrain a Pod that bypasses the scheduler. A missing ClusterCache or an invalid selector is denied with `403`; a failed lookup is denied with `503` so the client retries rather than admitting a Pod without its cache's affinity.

A managed `spec.origin` is placed with the same selector: the dataplane reads the origin only on the selected nodes. Changing the selector moves the cache to a new pool of nodes with no migration. Pages are not copied between pools; they are refilled from the origin on demand, and prefetch tickets canceled by the move are reported with the reason `cache_removed`.

The controller and the dataplane must run matching versions. The publication schema and the node's accepted cache definitions are versioned together, and a node rejects a publication it cannot interpret instead of guessing.

### Azure origin

`spec.origin` makes the Racer dataplane read a cache's objects directly from object storage. There is no separate origin process and no origin socket for that cache: the dataplane opens the upstream reads itself, on the nodes selected by `spec.nodeSelector`. Only Azure Blob Storage is supported today, and `spec.origin.azure` is required when `spec.origin` is present.

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: models
spec:
  origin:
    metadataTTL: 30s
    requestTimeout: 1m
    maxConcurrentRequests: 512
    azure:
      containerURL: https://account.blob.core.windows.net/models
      credential:
        workloadIdentity:
          clientID: 00000000-0000-4000-8000-000000000001
```

| Field | Default | Rules |
| --- | --- | --- |
| `spec.origin.metadataTTL` | `30s` | Must not be negative. Bounds how long object metadata is reused before revalidation. |
| `spec.origin.requestTimeout` | `1m` | Must be positive. Bounds each upstream request. |
| `spec.origin.maxConcurrentRequests` | `512` | 1 to 65536. Bounds concurrent upstream requests for the cache on each dataplane node. |
| `spec.origin.azure.containerURL` | required | Must be an `http` or `https` URL, with no credentials, query, or fragment. Fixed at creation. |
| `spec.origin.azure.credential` | unset | When omitted, requests are anonymous. Otherwise exactly one of `default` or `workloadIdentity`. |

`containerURL` names one container. The last path segment is the container name, which must be 3 to 63 lowercase alphanumeric or hyphen characters. An Azurite-style URL may carry one account path segment before the container, for example `http://127.0.0.1:10000/account/models`. When a credential is configured, the URL must use `https`.

The container URL is immutable. Cache entries do not record the storage account or container, so a different source needs a new `ClusterCache`, which has a new UID and therefore an empty cache. Admission rejects an update that changes the URL, and the operator reports `OriginInvalid` if a stored object bypassed admission and no longer satisfies the rules.

`spec.origin.azure.credential.default` uses the node's managed identity through the Azure Instance Metadata Service. The optional `clientID` selects a user-assigned identity; without it, Racer uses the system-assigned identity or the only user-assigned identity. Every Pod attached to the cache sees the container's whole keyspace.

`spec.origin.azure.credential.workloadIdentity` uses Microsoft Entra Workload ID. Racer exchanges a projected token for its own ServiceAccount, with audience `api://AzureADTokenExchange`, so the identity needs a federated credential whose subject is the Racer dataplane ServiceAccount in its namespace. The dataplane uses the ServiceAccount named by `RACER_SERVICE_ACCOUNT`, which defaults to `racer`, in `RACER_NAMESPACE`; the federated credential subject therefore has the form `system:serviceaccount:<namespace>:<service-account>`. No webhook is needed. `clientID` is required, and `tenantID` is optional; when unset, the tenant is discovered from the storage account's bearer challenge.

The dataplane Pod carries one credential scope for all caches it serves. Any identity that can read a container is therefore an identity trusted by every creator of a cache that uses it. Grant the dataplane identity read access only to the containers that caches on those nodes should reach, and remember that a user who can create a `ClusterCache` can name any container that identity can read.

Nodes need network access to the storage endpoint and, for the `default` credential, to the Instance Metadata Service. A node that cannot reach IMDS cannot use a managed identity, and a node that cannot reach the storage endpoint reports origin failures rather than serving stale data.

`spec.origin.azure.credential.default.secretRef` is deprecated and unsupported. The dataplane cannot read Secret credentials, so a cache that sets it is rejected. The field remains in the schema only so that a stored cache which still sets it is not pruned into a managed identity credential; it fails closed instead.

The object sidecar serves one cache. It is injected on a Pod with the label `racer.unbounded-cloud.io/object-sidecar: enabled` and the annotation `racer.unbounded-cloud.io/object-cache: <cache>`. The dataplane enforces the cache's configured container, so a sidecar request for another container is refused with `403`. A Pod that needs a second cache uses the client socket mount for that cache, or an application that opens both client sockets.

#### Upgrading from per-cache origin DaemonSets

Earlier operator versions ran one `racer-object` origin DaemonSet and ServiceAccount per cache, named `racer-origin-<cache>`. The dataplane now reads `spec.origin` in process, and the operator prunes the legacy DaemonSets only after every dataplane Pod runs a template marked with the native origin annotation.

The upgrade has a credential step for workload identity. Before you upgrade, add a federated credential for the Racer dataplane ServiceAccount to the identity, with audience `api://AzureADTokenExchange` and the subject named above. The identity needs this credential in addition to the one used by the old per-cache origin ServiceAccount. Once the rollout completes and the legacy DaemonSets are gone, remove the old federated credential and the old ServiceAccount.

Caches that use `credential.default` attach the node's managed identity instead of a per-cache ServiceAccount. Remove `secretRef` before the upgrade if any cache still sets it; the catalog rejects the stale field, and the dataplane cannot read a Secret credential. If you changed a credential, finish the credential change and delete the old Secret before the rollout, so no cache points at a credential that no longer exists.

## Node requirements

Racer runs only on Linux nodes with a working `io_uring`. The dataplane container runs as privileged UID 0 because it opens raw block devices, raises file descriptor limits, manages RDMA resources, and creates the origin directories owned by the unprivileged origin user. Your kernel and container security policy must allow those operations. The container mounts these host paths:

| Host path | Mount | Purpose |
| --- | --- | --- |
| `/var/lib/racer/identity` | writable | Node-private persistent keys; must not overlap another configured path |
| `/var/lib/racer/slabs` | writable | Slab files and checkpoints; still required and writable in raw-device mode |
| `/run/racer` | writable | Client and origin sockets |
| `/dev` | read-only at `/host/dev` | Raw device discovery under `disk/by-id` and device nodes |

The socket directory is shared, so protect it with Kubernetes admission policy: a Pod that can mount `/run/racer` can reach every cache socket on the node, not only the cache its sidecar names. The client socket mount feature narrows this by mounting only the per-cache client directory.

Startup computes the file descriptor budget from the final node-wide limits before any worker opens a socket. The budget is 1024 base descriptors, plus `RACER_CLIENT_CONNECTIONS`, `RACER_PEER_CONNECTIONS`, `RACER_OUTBOUND_CONNECTIONS`, twice `RACER_PIPES`, and 64 descriptors per I/O worker for its ring, eventfds, listeners, and cache files. Racer raises the soft `RLIMIT_NOFILE` toward the hard limit when needed and refuses to start when the hard limit is too low. The failure is logged as `stage=fd-limit` and names the variables that set the budget.

Connection and pipe budgets are admission limits, not descriptor reservations. A node that admits more sockets than `RLIMIT_NOFILE` allows would fail with `EMFILE` under load instead of answering `Overloaded`, which is why startup checks the budget rather than waiting for the first refusal. Raise the container runtime's nofile ulimit or lower the connection and pipe budgets.

Origins run as UID and GID 65532. The dataplane creates `/run/racer/<cache>/origin` with mode `0700` and that owner, so only the origin user may list, create, or remove endpoint entries. The origin socket is created with mode `0600`. One origin owns each cache's origin socket: a second origin that tries to bind the same path fails rather than sharing it. An origin must return identical bytes for the same key and version; Racer verifies page tags and treats divergent content as corruption.

HTTPS origins use rustls for the handshake and then move the record layer into kernel TLS when the `tls` module is available, so response bodies decrypt directly into page buffers. If `setsockopt(TCP_ULP, "tls")` fails, Racer logs that kernel TLS is unavailable and uses userspace TLS on the worker's reactor, which costs more CPU per byte but no extra threads. Load the module on the node (`modprobe tls`) or list it in `/etc/modules-load.d` to get kernel TLS.

`RACER_MEMORY_PRESSURE` enables cgroup v2 memory-pressure monitoring and reclaim. Keep it enabled on nodes with a cgroup v2 hierarchy; disabling it removes an early warning path and leaves memory admission as the main protection.

## Node labels and annotations

Racer uses the `racer.unbounded-cloud.io/` prefix for its own labels and annotations, plus `unbounded-cloud.io/site` for the site label it shares with the rest of Unbounded. The control plane reads Node labels and annotations when it builds membership.

| Key | Object | Meaning |
| --- | --- | --- |
| `racer.unbounded-cloud.io/exclude` | Node label | Any value removes the Node from membership and from enrollment eligibility |
| `unbounded-cloud.io/site` | Node label | Site name; RDMA requires both peers to share a nonempty site |
| `racer.unbounded-cloud.io/shares` | Node annotation | Positive decimal placement weight; overrides the Pod report |
| `racer.unbounded-cloud.io/rdma-nics` | Node annotation | JSON NIC array; overrides the Pod report and the discovered inventory |
| `racer.unbounded-cloud.io/block-devices` | Node annotation | Startup-only regex over `disk/by-id` basenames for raw cache devices |
| `racer.unbounded-cloud.io/report` | Pod annotation | JSON `{"shares": N, "rdma_nics": [...]}` reported by the Racer Pod itself |

Shares default to 4. A Node annotation wins over the Pod report, and `RACER_SHARES` supplies the value a Pod reports when the Node annotation is absent. A shares value must be a nonempty ASCII decimal without a leading `+`, and it must be nonzero; zero shares are invalid. Shares weight page ownership, so a node with twice the shares owns roughly twice as many page slots as a peer.

The `report` annotation is limited to 32 KiB of UTF-8, which is the inventory bound rounded up and well below the Kubernetes annotation limit. The admission policy enforces the same byte limit. A malformed report, an invalid shares value, or an unparsable `rdma-nics` value is reported as a diagnostic and the node keeps its previous attributes. A new node with no previous attributes does not join until its annotations are valid.

`rdma-nics` accepts a JSON array of at most 64 entries. Each entry names a device, port, and rail, and may include a GID and a NUMA node. An explicit empty array `[]` disables RDMA eligibility for that node. When the annotation is absent, the dataplane reports the NICs it discovered in its Pod report.

`block-devices` is a Rust regular expression over the basenames under `/host/dev/disk/by-id`. The selector is limited to 1 KiB, is not implicitly anchored, and is compiled at startup only. Anchor it with `^` and `$` when you mean an exact name. A selector that does not compile, or that matches no usable device, falls back to file-backed slabs; the annotation takes effect only after a dataplane restart.

Annotations from earlier releases that scoped one cache by namespace or bucket are no longer read. A cache now maps to one container, and a Pod that needs a second cache uses the client socket mount for that cache. Invalid or retired runtime annotations are ignored or reported, never interpreted with old semantics.

## Dataplane configuration

The operator stores dataplane settings in the `racer-settings` ConfigMap in the Racer namespace. The ConfigMap seeds only when it is missing; after that, the operator preserves administrator data and does not overwrite edits. It hashes the ConfigMap payload into the `unbounded-cloud.io/racer-settings-hash` annotation on the DaemonSet Pod template, so an edit that changes the payload causes a managed rollout. A change that does not change the hash does not restart the dataplane.

The Pod imports the ConfigMap with `envFrom`, so every valid key becomes an environment variable before the process starts. This wiring happens before the dataplane reads its configuration: only `main` reads the process environment, and tests and simulations build a configuration from an explicit map. A value that fails validation prevents the rollout rather than starting a partially configured Pod.

Numeric values are unsigned decimal ASCII digits only. A leading `+`, a sign, whitespace, an exponent, or a unit suffix is rejected with an error that names the variable, its value, and the rule it broke, for example `RACER_RANGE_WINDOW_PAGES="65": must be from 1 to 64`. Booleans are exactly `true` or `false`, and enumerations accept only their listed words.

The operator validates each key against the table exported to `internal/operator/components/racer/dataplane-settings.json`. A key the dataplane does not recognize fails the operator plan and dataplane startup. When validation fails, the operator keeps the existing DaemonSet instead of rolling out an invalid configuration, so a running node continues with the last good settings while the error is reported. Some limits depend on the node, such as the number of workers or the available devices; those are checked at startup, and an unsatisfiable combination fails with a named stage rather than silently reducing counts.

### Environment variables

Every accepted variable is one entry in `cmd/racer/src/config/table.rs`. The same table drives parsing, defaults, bounds, the unknown-variable check, and the generated reference below, so a setting cannot drift between documentation, defaults, and code. The table is regenerated with `BLESS=1 cargo test -p racer config::tests::generated_reference_is_current`, and the test fails when the page or the operator's JSON is stale.

Any `RACER_*` variable that the table does not list fails startup. This catches typos and stale names; test harness knobs use other prefixes, such as `DST_*`. A value longer than 4096 bytes or containing a control character is rejected before its kind is checked.

Budgets that I/O workers share come in two forms. The `*_PER_WORKER` form carries the default and is multiplied by the final I/O worker count, so automatic sizing works on any core count. The plain node-wide form is divided among the workers. Set at most one form of each budget; setting both is an error. The worker count is resolved before the division, so a per-worker value follows the final count, not the number of CPUs on the node.

| Node-wide variable | Per-worker variable | Per-worker default |
| --- | --- | --- |
| `RACER_PLAINTEXT_BYTES` | `RACER_PLAINTEXT_BYTES_PER_WORKER` | 128 MiB |
| `RACER_CIPHERTEXT_BYTES` | `RACER_CIPHERTEXT_BYTES_PER_WORKER` | 128 MiB |
| `RACER_DIRTY_BYTES` | `RACER_DIRTY_BYTES_PER_WORKER` | 160 MiB |
| `RACER_REGISTERED_BYTES` | `RACER_REGISTERED_BYTES_PER_WORKER` | 384 MiB |
| `RACER_REQUEST_CONTEXT_BYTES` | `RACER_REQUEST_CONTEXT_BYTES_PER_WORKER` | 32 MiB |
| `RACER_FLIGHTS` | `RACER_FLIGHTS_PER_WORKER` | 64 |
| `RACER_QUEUE_ENTRIES` | `RACER_QUEUE_ENTRIES_PER_WORKER` | 256 |
| `RACER_CLIENT_CONNECTIONS` | `RACER_CLIENT_CONNECTIONS_PER_WORKER` | 64 |
| `RACER_PEER_CONNECTIONS` | `RACER_PEER_CONNECTIONS_PER_WORKER` | 2048 |
| `RACER_OUTBOUND_CONNECTIONS` | `RACER_OUTBOUND_CONNECTIONS_PER_WORKER` | 64 |
| `RACER_PIPES` | `RACER_PIPES_PER_WORKER` | 16 |
| `RACER_PLACEMENT_CACHE_BYTES` | `RACER_PLACEMENT_CACHE_BYTES_PER_WORKER` | 16 MiB |
| `RACER_CACHED_PATHS` | `RACER_CACHED_PATHS_PER_WORKER` | 128 |
| `RACER_PATH_CACHE_BYTES` | `RACER_PATH_CACHE_BYTES_PER_WORKER` | 8 MiB |
| `RACER_METADATA_ENTRIES` | `RACER_METADATA_ENTRIES_PER_WORKER` | 4096 |
| `RACER_RELAY_TRANSFERS` | `RACER_RELAY_TRANSFERS_PER_WORKER` | 16 |

Automatic thread sizing uses every eligible physical core unless a CPU quota or a resource budget limits it. `RACER_ALLOW_SMT=true` sizes from logical CPUs instead. A node gets one control plane thread in addition to I/O and crypto threads. Without an explicit cap, eight eligible cores on one NUMA node give five I/O workers and three crypto helpers when the budgets permit, plus the control thread. I/O workers share NUMA-local crypto helpers at approximately a 2:1 ratio.

`RACER_MAX_THREADS` caps I/O, crypto, RDMA owner, and managed control threads; kernel io_uring workers are not counted. Managed deployments need at least 4 threads, or 3 when `RACER_ENABLE_RDMA` is `false`. `RACER_IO_WORKERS` and `RACER_CRYPTO_HELPERS` set an exact split and must be set together. Their sum, plus one RDMA owner thread when RDMA is enabled, must fit `RACER_MAX_THREADS`. Workers and helpers are spread over the NUMA nodes of the RDMA NICs in proportion to their usable CPUs, with each helper on the node of the workers it serves. Insufficient local CPUs, CPU quota, or per-I/O budget fails startup instead of reducing the requested counts.

Budgets are admission ceilings, not memory reservations. Resident memory grows toward them under load. Each worker needs roughly 210 MiB of byte budgets at the default range window: about 80 MiB of plaintext, 96 MiB of ciphertext, 16 MiB of dirty pages, plus request context. To bound memory, cap total threads, set an exact worker split, or set node-wide totals below the per-worker defaults. Crypto helper count affects decrypt throughput and the crypto queue and execution metrics, not the byte budget division, which follows the I/O worker count.

The generated table follows. It lists every accepted variable, its default, whether it is node-wide or per I/O worker, the accepted values, and its description. The scope column says `Operator` for the one setting the operator reads and the dataplane ignores, `Node` for a node-wide setting, and `Per I/O worker` for a value multiplied by the worker count.

<!-- BEGIN GENERATED: racer configuration table -->

#### Control and identity

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_CLUSTER_ID` | Unset: managed mode adopts the elected cluster | Node | UUID | Standalone cluster UUID. Set together with `RACER_CONTROL_ENDPOINT`; managed deployments set neither. |
| `RACER_CONTROL_ENDPOINT` | Unset: managed mode | Node | `https://` URL | Standalone controller URL. Set together with `RACER_CLUSTER_ID`. |
| `RACER_CONTROL_TRUST_BUNDLE` | `/etc/racer/trust/ca.crt` | Node | absolute path | CA bundle that verifies the standalone controller. |
| `RACER_CONTROL_TOKEN` | Unset: `/var/run/racer-token/token` in managed mode, `/var/run/secrets/racer-control/token` in standalone mode | Node | absolute path | Projected token with the `racer-control` audience, presented to the controller and to control seats. |
| `RACER_IDENTITY_DIRECTORY` | `/var/lib/racer/identity` | Node | absolute path | Node-private persistent keys. Must not overlap any other configured path. |
| `RACER_SHARES` | `4` | Node | 1..4,294,967,295 | Reported shares; an explicit Node `shares` value wins. |

#### Threads

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_MAX_THREADS` | Unset: no cap | Node | 2..2^64-1 | Total I/O, crypto, RDMA owner (when RDMA is enabled), and managed control threads. Managed deployments need at least 4, or 3 when `RACER_ENABLE_RDMA` is `false`. Kernel io_uring workers are not counted. |
| `RACER_IO_WORKERS` | Unset: automatic | Node | 1..65,536 | Exact I/O worker count. Requires `RACER_CRYPTO_HELPERS`; their sum, plus one RDMA owner thread when RDMA is enabled, must fit `RACER_MAX_THREADS`. Workers and helpers are spread over the NUMA nodes of the RDMA NICs in proportion to their usable CPUs, with each helper on the node of the workers it serves. |
| `RACER_CRYPTO_HELPERS` | Unset: automatic | Node | 1..65,536 | Exact crypto helper count, at most `RACER_IO_WORKERS`. Requires `RACER_IO_WORKERS`. |
| `RACER_ALLOW_SMT` | `false` | Node | `true`, `false` | `true` sizes from logical CPUs instead of one eligible CPU per physical core. |

#### RDMA and CPU

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_ENABLE_RDMA` | `auto` | Node | `auto`, `true`, `false` | `auto` uses native RDMA when built in and ports exist at startup; `true` reserves RDMA resources even without ports, for hotplug; `false` turns it off. |
| `RACER_RDMA_BREAKER` | `false` | Node | `true`, `false` | Per peer and rail circuit breaker, off by default: after a hard RDMA failure with a peer (link break, setup failure, grant or write error), or 3 page timeouts in a row with no RDMA success between them, pages to and from it use HTTP at once until a cooldown passes and one probe page succeeds over RDMA. Load (no grant slot or flight room) never trips it. |
| `RACER_RDMA_BREAKER_COOLDOWN_MS` | `2000` | Node | 1..3,600,000 ms | First breaker cooldown; it doubles for each consecutive trip, up to `RACER_RDMA_BREAKER_MAX_COOLDOWN_MS`, less up to a fifth as jitter. |
| `RACER_RDMA_BREAKER_MAX_COOLDOWN_MS` | `30000` | Node | 1..3,600,000 ms | Longest breaker cooldown; at least `RACER_RDMA_BREAKER_COOLDOWN_MS`. |
| `RACER_RDMA_PAGE_TIMEOUT_MS` | `5000` | Node | 1..60,000 ms | Longest one page's RDMA grant or write may take before the page goes over HTTP in the same exchange; counts a soft breaker failure. |
| `RACER_RDMA_CM_PORT` | `18515` | Node | 1..65,535 | TCP port each dataplane's RDMA connection manager listens on; must match on every node. |
| `RACER_MEMORY_PRESSURE` | `true` | Node | `true`, `false` | cgroup v2 memory-pressure monitoring and reclaim. |

#### GPU reads

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_ENABLE_GPU` | `auto` | Node | `auto`, `true`, `false` | `auto` turns GPU reads on at the first `gpu-read` if a usable GPU is found; `true` sets GPUs up at startup and fails without one; `false` turns them off. |
| `RACER_GPU_STAGING_BYTES` | `268435456` (256 MiB) | Node | 2,097,152..2^64-1 bytes, multiple of 2,097,152 | Ciphertext staging memory per GPU. Below about 34 MiB the GPU does not decrypt and the CPU does. |
| `RACER_GPU_LANDING_BYTES` | `268435456` (256 MiB) | Node | 0..2^64-1 bytes, multiple of 2,097,152 | GPU memory per GPU that peers' NICs write GPU-read pages into over RDMA; `0` turns landing off. |
| `RACER_GPU_PCI_P2PDMA` | `false` | Node | `true`, `false` | `true` asks cuFile to read local NVMe into GPU memory over PCI P2PDMA instead of `nvidia-fs`. Needs cuFile 1.13 (CUDA 12.8) or later. |
| `RACER_GPU_PEER` | `auto` | Node | `auto`, `off` | `auto` lets one GPU copy another GPU's encrypted page over NVLink or PCIe peer-to-peer where the driver allows it; `off` moves such pages through host memory. |

#### Storage

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_SLAB_DIRECTORY` | `/var/lib/racer/slabs` | Node | absolute path | Slab files and checkpoints; still required and writable in device mode. |
| `RACER_DEVICE_DIRECTORY` | `/host/dev` | Node | absolute path | Device root whose `disk/by-id` directory is scanned for raw devices. |
| `RACER_DEVICE_BUSY_WAIT_SECS` | `120` | Node | 0..3,600 s | Startup bound for retrying selected devices whose exclusive open is busy (`EBUSY`); `0` skips busy devices immediately. |
| `RACER_SLAB_BYTES_PER_WORKER` | `17179869184` (16 GiB) | Per I/O worker | 1..9,223,372,036,854,775,807 bytes | File-backed slab size of each I/O worker; a multiple of `RACER_SEGMENT_BYTES`. Does not limit device-mode capacity. |
| `RACER_SEGMENT_BYTES` | `1073741824` (1 GiB) | Node | 16,797,712..9,223,372,036,854,775,807 bytes | Segment size; must fit one page plus storage overhead. |
| `RACER_FREE_SEGMENT_RESERVE_PER_WORKER` | `2` | Per I/O worker | 1..1,048,575 | Reserved free segments in each I/O worker, fewer than that worker's segments. |
| `RACER_DISK_PAGE_ENTRIES` | `65536` | Node | 1..1,048,576 | Disk page-index budget in file mode; device mode can raise it. |
| `RACER_CHECKPOINT_BYTES` | `67108864` (64 MiB) | Node | 1..536,870,912 bytes | Checkpoint working-set budget; device mode can raise it. |
| `RACER_RETAINED_SEGMENT_PERCENT` | `50` | Per I/O worker | 1..100 % | Spill-hold cap of each I/O worker, as a percentage of its segments and page-index capacity. |
| `RACER_RANGE_SPILL_PAGES` | `0` | Node | 0..1,024 | Pages past the read window that a read may fetch to local disk under a short hold; `0` disables spill. |

#### Disk retention

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_ADMISSION_MODE` | `second-sight` | Node | `disabled`, `second-sight` | Disk retention policy. Does not change request admission. |
| `RACER_ADMISSION_HISTORY_BYTES` | `4194304` (4 MiB) | Node | 32..536,870,912 bytes | Bloom history, divided among I/O workers; each needs at least 32 bytes. |
| `RACER_ADMISSION_PERIOD_SECS` | `60` | Node | 1..86,400 s | History rotation period. |

#### Admission budgets

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_WAITERS_PER_FLIGHT` | `64` | Node | 1..4,096 | Readers that may wait on one single-flight fetch. Flights times waiters is at most 1,048,576. |
| `RACER_NEIGHBOR_CONNECTIONS_PER_WORKER` | `8` | Per I/O worker | 1..1,024 | Busy connections each I/O worker keeps to one neighbor; at most the worker's outbound connections. |
| `RACER_RANGE_WINDOW_PAGES` | `4` | Node | 1..64 | Pages each read fetches ahead of delivery, in any order. |
| `RACER_HEADER_BYTES` | `32768` (32 KiB) | Node | 1,024..32,768 bytes | Largest accepted HTTP header block. |
| `RACER_ACTIVE_PATH_SEARCHES_PER_WORKER` | `8` | Per I/O worker | 1..64 | Distinct unfinished path searches in each I/O worker. |
| `RACER_RETAINED_SNAPSHOTS` | `2` | Node | 2..64 | Retained topology snapshots. |
| `RACER_PLAINTEXT_BYTES` | Unset: `RACER_PLAINTEXT_BYTES_PER_WORKER` applies | Node | 1..68,719,476,736 bytes | Plaintext admission budget for in-flight decrypts and pages held by range reads. Each worker's share must fit one page more than `RACER_RANGE_WINDOW_PAGES`. Node-wide total, divided among I/O workers. |
| `RACER_PLAINTEXT_BYTES_PER_WORKER` | `134217728` (128 MiB) | Per I/O worker | 1..68,719,476,736 bytes | Plaintext admission budget for in-flight decrypts and pages held by range reads. Each worker's share must fit one page more than `RACER_RANGE_WINDOW_PAGES`. Multiplied by the final I/O worker count. |
| `RACER_CIPHERTEXT_BYTES` | Unset: `RACER_CIPHERTEXT_BYTES_PER_WORKER` applies | Node | 1..68,719,476,736 bytes | Ciphertext admission budget, including the memory cache. Each worker's share must fit the range window plus one stored record. Node-wide total, divided among I/O workers. |
| `RACER_CIPHERTEXT_BYTES_PER_WORKER` | `134217728` (128 MiB) | Per I/O worker | 1..68,719,476,736 bytes | Ciphertext admission budget, including the memory cache. Each worker's share must fit the range window plus one stored record. Multiplied by the final I/O worker count. |
| `RACER_DIRTY_BYTES` | Unset: `RACER_DIRTY_BYTES_PER_WORKER` applies | Node | 16,777,232..68,719,476,736 bytes | Budget for pages waiting to be written to disk. Node-wide total, divided among I/O workers. |
| `RACER_DIRTY_BYTES_PER_WORKER` | `167772160` (160 MiB) | Per I/O worker | 16,777,232..68,719,476,736 bytes | Budget for pages waiting to be written to disk. Multiplied by the final I/O worker count. |
| `RACER_REGISTERED_BYTES` | Unset: `RACER_REGISTERED_BYTES_PER_WORKER` applies | Node | 1..68,719,476,736 bytes | Native RDMA page-buffer pool, used only when RDMA is enabled; then at least one page plus 16 bytes. One native thread owns the whole pool, so it is never divided. When neither variable is set, the scaled default is capped at 8 GiB node-wide. The pool is split evenly over RDMA rails, and a rail without a whole buffer is not used. Each grant holds one 18 MiB buffer until its page is consumed; `racer_rdma_free_buffers` shows what is left. Node-wide total, divided among I/O workers. |
| `RACER_REGISTERED_BYTES_PER_WORKER` | `402653184` (384 MiB) | Per I/O worker | 1..68,719,476,736 bytes | Native RDMA page-buffer pool, used only when RDMA is enabled; then at least one page plus 16 bytes. One native thread owns the whole pool, so it is never divided. When neither variable is set, the scaled default is capped at 8 GiB node-wide. The pool is split evenly over RDMA rails, and a rail without a whole buffer is not used. Each grant holds one 18 MiB buffer until its page is consumed; `racer_rdma_free_buffers` shows what is left. Multiplied by the final I/O worker count. |
| `RACER_REQUEST_CONTEXT_BYTES` | Unset: `RACER_REQUEST_CONTEXT_BYTES_PER_WORKER` applies | Node | 131,072..68,719,476,736 bytes | Request-context budget. Node-wide total, divided among I/O workers. |
| `RACER_REQUEST_CONTEXT_BYTES_PER_WORKER` | `33554432` (32 MiB) | Per I/O worker | 131,072..68,719,476,736 bytes | Request-context budget. Multiplied by the final I/O worker count. |
| `RACER_FLIGHTS` | Unset: `RACER_FLIGHTS_PER_WORKER` applies | Node | 1..65,536 | Concurrent single-flight page fetches. Node-wide total, divided among I/O workers. |
| `RACER_FLIGHTS_PER_WORKER` | `64` | Per I/O worker | 1..65,536 | Concurrent single-flight page fetches. Multiplied by the final I/O worker count. |
| `RACER_QUEUE_ENTRIES` | Unset: `RACER_QUEUE_ENTRIES_PER_WORKER` applies | Node | 2..65,536 | Queued I/O operations. Node-wide total, divided among I/O workers. |
| `RACER_QUEUE_ENTRIES_PER_WORKER` | `256` | Per I/O worker | 2..65,536 | Queued I/O operations. Multiplied by the final I/O worker count. |
| `RACER_CLIENT_CONNECTIONS` | Unset: `RACER_CLIENT_CONNECTIONS_PER_WORKER` applies | Node | 1..65,536 | Client and control connections. A quarter of each worker's share, at most three, is kept for control connections; the rest accept client connections. Peer and outbound connections have their own limits. Node-wide total, divided among I/O workers. |
| `RACER_CLIENT_CONNECTIONS_PER_WORKER` | `64` | Per I/O worker | 1..65,536 | Client and control connections. A quarter of each worker's share, at most three, is kept for control connections; the rest accept client connections. Peer and outbound connections have their own limits. Multiplied by the final I/O worker count. |
| `RACER_PEER_CONNECTIONS` | Unset: `RACER_PEER_CONNECTIONS_PER_WORKER` applies | Node | 1..65,536 | Inbound peer connections. A peer connection runs one exchange at a time and stays open between them; when every worker's share is in use the listener answers new connections with the unsigned overload head and closes them, so the sender asks elsewhere. Each connection is a file descriptor. Node-wide total, divided among I/O workers. |
| `RACER_PEER_CONNECTIONS_PER_WORKER` | `2048` | Per I/O worker | 1..65,536 | Inbound peer connections. A peer connection runs one exchange at a time and stays open between them; when every worker's share is in use the listener answers new connections with the unsigned overload head and closes them, so the sender asks elsewhere. Each connection is a file descriptor. Multiplied by the final I/O worker count. |
| `RACER_OUTBOUND_CONNECTIONS` | Unset: `RACER_OUTBOUND_CONNECTIONS_PER_WORKER` applies | Node | 1..65,536 | Outbound peer and origin connections, busy or idle. One slot per worker is kept for origin metadata when the share is at least two. Raise it when many concurrent page reads to different peers are refused as `Overloaded`. Node-wide total, divided among I/O workers. |
| `RACER_OUTBOUND_CONNECTIONS_PER_WORKER` | `64` | Per I/O worker | 1..65,536 | Outbound peer and origin connections, busy or idle. One slot per worker is kept for origin metadata when the share is at least two. Raise it when many concurrent page reads to different peers are refused as `Overloaded`. Multiplied by the final I/O worker count. |
| `RACER_PIPES` | Unset: `RACER_PIPES_PER_WORKER` applies | Node | 1..65,536 | Splice pipes. Node-wide total, divided among I/O workers. |
| `RACER_PIPES_PER_WORKER` | `16` | Per I/O worker | 1..65,536 | Splice pipes. Multiplied by the final I/O worker count. |
| `RACER_PLACEMENT_CACHE_BYTES` | Unset: `RACER_PLACEMENT_CACHE_BYTES_PER_WORKER` applies | Node | 1,024..536,870,912 bytes | Cached page placement rankings. Node-wide total, divided among I/O workers. |
| `RACER_PLACEMENT_CACHE_BYTES_PER_WORKER` | `16777216` (16 MiB) | Per I/O worker | 1,024..536,870,912 bytes | Cached page placement rankings. Multiplied by the final I/O worker count. |
| `RACER_CACHED_PATHS` | Unset: `RACER_CACHED_PATHS_PER_WORKER` applies | Node | 1..1,048,576 | Cached topology paths. Node-wide total, divided among I/O workers. |
| `RACER_CACHED_PATHS_PER_WORKER` | `128` | Per I/O worker | 1..1,048,576 | Cached topology paths. Multiplied by the final I/O worker count. |
| `RACER_PATH_CACHE_BYTES` | Unset: `RACER_PATH_CACHE_BYTES_PER_WORKER` applies | Node | 1..536,870,912 bytes | Memory for cached topology paths. Node-wide total, divided among I/O workers. |
| `RACER_PATH_CACHE_BYTES_PER_WORKER` | `8388608` (8 MiB) | Per I/O worker | 1..536,870,912 bytes | Memory for cached topology paths. Multiplied by the final I/O worker count. |
| `RACER_METADATA_ENTRIES` | Unset: `RACER_METADATA_ENTRIES_PER_WORKER` applies | Node | 1..1,048,576 | Object metadata catalog entries. Node-wide total, divided among I/O workers. |
| `RACER_METADATA_ENTRIES_PER_WORKER` | `4096` | Per I/O worker | 1..1,048,576 | Object metadata catalog entries. Multiplied by the final I/O worker count. |
| `RACER_RELAY_TRANSFERS` | Unset: `RACER_RELAY_TRANSFERS_PER_WORKER` applies | Node | 1..65,536 | Concurrent relayed transfers. Node-wide total, divided among I/O workers. |
| `RACER_RELAY_TRANSFERS_PER_WORKER` | `16` | Per I/O worker | 1..65,536 | Concurrent relayed transfers. Multiplied by the final I/O worker count. |

#### Listeners and peers

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_PEER_LISTEN` | `0.0.0.0:7443` | Node | `ip:port` | Peer listener. A bracketed IPv4 address, as Kubernetes expands `[$(POD_IP)]`, is accepted. Its port is the peer port published to members. |
| `RACER_DIAGNOSTICS_LISTEN` | `127.0.0.1:9090` | Node | `ip:port` | Diagnostics listener; must differ from the peer listener. |
| `RACER_PEER_TCP_NODELAY` | `true` | Node | `true`, `false` | TCP_NODELAY on peer sockets. `false` only reproduces the old socket behavior. |
| `RACER_OPAQUE_RELAY` | `false` | Node | `true`, `false` | Experimental opaque HTTP transit instead of materialized relay. |
| `RACER_PEER_INFLIGHT_MAX` | `4096` | Node | 1..65,536 | Peer exchanges in flight. |
| `RACER_PEER_PER_NEIGHBOR_MAX` | `512` | Node | 1..65,536 | Peer exchanges in flight to one neighbor; at most `RACER_PEER_INFLIGHT_MAX`. |
| `RACER_PEER_ADMISSION_RECOVERY_MS` | `1000` | Node | 1..60,000 ms | Longest time between one-slot recoveries of a reduced peer-exchange limit; it also recovers one slot per `limit` verified completions. |
| `RACER_PEER_RECEIVE_MAX` | `0` | Node | 0..65,536 | Concurrently served peer requests; `0` turns the receive gate off. |
| `RACER_PEER_RECEIVE_QUEUE_MAX` | `256` | Node | 1..65,536 | Peer requests queued at the receive gate. |
| `RACER_PEER_RECEIVE_WAIT_MS` | `1000` | Node | 1..30,000 ms | Longest wait at the receive gate. |
| `RACER_PAGE_HEDGE_DELAY_MS` | `100` | Node | 1..30,000 ms | Delay before a hedged second page request. |
| `RACER_PAGE_HEDGE_SLOTS` | `0` | Node | 0..32 | Concurrent hedged page requests; `0` turns hedging off. |
| `RACER_PAGE_HEDGE_BYTES` | `33554448` | Node | 0..1,073,742,336 bytes | Memory for hedged duplicates; with hedging on, at least one duplicate (two pages plus 16 bytes). |
| `RACER_REPLICA_SELECTION` | `spread` | Node | `primary`, `spread` | Which owner a non-owner asks first. `spread`: direct neighbors first, then a per-node hash spreads pages across owners. `primary`: the first-ranked owner. |

#### Origins

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER` | `32` | Per I/O worker | 1..1,024 | Origin connections per cache in each I/O worker, independent of peer connections; at most the client connections. When unset it is lowered to fit them. Also caps connections to each Azure Blob endpoint, which caches in one storage account share. |
| `RACER_ORIGIN_TRUST_BUNDLE` | `/etc/ssl/certs/ca-certificates.crt` | Node | absolute path | CA bundle for HTTPS origins and the Entra authority, loaded once at startup; if it cannot be loaded, HTTPS origins fail until restart. Empty selects the default. |
| `RACER_AZURE_AUTHORITY_HOST` | `https://login.microsoftonline.com/` | Node | `https://` URL | Microsoft Entra authority, ending in `/`. Empty selects the default. |
| `RACER_AZURE_FEDERATED_TOKEN_FILE` | Unset: workload identity is unavailable | Node | absolute path | Projected token exchanged for Entra tokens when an origin uses `workloadIdentity`. |

#### Prefetch

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_PREFETCH_MAX_BODY_BYTES` | `4194304` (4 MiB) | Node | 4,096..67,108,864 bytes | `PUT` body limit. |
| `RACER_PREFETCH_MAX_REFS` | `16384` | Node | 1..1,048,576 | Refs per `PUT`. |
| `RACER_PREFETCH_MAX_TICKETS` | `1024` | Node | 1..65,536 | Live and finished tickets, all caches. |
| `RACER_PREFETCH_MAX_TTL_SECS` | `3600` | Node | 1..86,400 s | TTL limit. |
| `RACER_PREFETCH_TICKET_RETENTION_SECS` | `300` | Node | 0..86,400 s | How long finished tickets stay visible. |
| `RACER_PREFETCH_MAX_WAIT_SECS` | `60` | Node | 1..600 s | `wait` limit. |
| `RACER_PREFETCH_DEFAULT_PENDING_BYTES` | `1073741824` (1 GiB) | Node | 16,777,216..1,125,899,906,842,624 bytes | Per-ticket in-flight bound when `max_pending_bytes` is 0; at most `RACER_PREFETCH_MAX_PENDING_BYTES`. |
| `RACER_PREFETCH_MAX_PENDING_BYTES` | `17179869184` (16 GiB) | Node | 16,777,216..1,125,899,906,842,624 bytes | Limit on `max_pending_bytes`. |
| `RACER_PREFETCH_NODE_PENDING_BYTES` | `68719476736` (64 GiB) | Node | 16,777,216..4,503,599,627,370,496 bytes | Queued and in-flight prefetch bytes; checked on each `PUT` that creates or changes a ticket; over it is `429`. |
| `RACER_PREFETCH_CONCURRENT_FILLS` | `16` | Node | 1..1,024 | Concurrent prefetch fills per owner. |
| `RACER_PREFETCH_ORIGIN_BYTES_PER_SEC` | `0` | Node | 0..1,099,511,627,776 bytes/s | Prefetch origin read rate per owner; `0` turns the limit off. |
| `RACER_PREFETCH_MAX_ATTEMPTS` | `5` | Node | 1..32 | Tries per page. |

#### Timeouts

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_REQUEST_TIMEOUT_MS` | `30000` | Node | 1..86,400,000 ms | Data request deadline. |
| `RACER_PEER_ATTEMPT_TIMEOUT_MS` | `30000` | Node | 1..86,400,000 ms | Local cap on one peer exchange; never sent to peers. |
| `RACER_READER_STALL_TIMEOUT_MS` | `10000` | Node | 1..86,400,000 ms | Longest a reader may stall; at most `RACER_REQUEST_TIMEOUT_MS`. |
| `RACER_SHUTDOWN_TIMEOUT_MS` | `30000` | Node | 1..3,600,000 ms | Drain deadline at shutdown. |

#### Diagnostics

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_PPROF_ENABLED` | `false` | Node | `true`, `false` | Serve CPU profiles on the diagnostics listener. |
| `RACER_SEND_TAG_PAIR` | Unset: off | Node | text | `sender,receiver` node UUIDs whose peer sends get a sampled AEAD tag check for diagnosis. |

#### Cluster control (managed mode)

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_NAMESPACE` | Unset: standalone mode | Node | DNS label | Pod namespace. Setting it selects managed mode and starts embedded cluster control. |
| `RACER_POD_NAME` | Unset: required in managed mode | Node | DNS label | Pod name. |
| `RACER_POD_UID` | Unset: required in managed mode | Node | UUID | Pod UID. |
| `RACER_NODE_NAME` | Unset: required in managed mode | Node | text | Kubernetes Node name. |
| `RACER_POD_IP` | Unset: required in managed mode | Node | IP address | Primary Pod IP (`status.podIP`). |
| `RACER_POD_IPS` | Unset: single stack | Node | comma-separated IP addresses | Every Pod IP (`status.podIPs`), led by `RACER_POD_IP`, at most one per family. The webhook also serves the other family. |
| `RACER_CONTROL_SEATS` | `auto` | Node | text | `auto` picks 1 to 16 seats from the cluster size; a decimal from 1 to 16 fixes the count. |
| `RACER_CONTROL_LISTEN` | `:8443` | Node | `ip:port` | Control seat listener. `:port` binds every IPv4 address. |
| `RACER_WEBHOOK_LISTEN` | `:9443` | Node | `ip:port` | Object sidecar admission webhook listener. `:port` binds every IPv4 address. |
| `RACER_OBJECT_IMAGE` | Unset: webhook serving off | Node | image reference | Image injected as the object sidecar. |
| `RACER_RETAINED_PAGE_KEYS` | `7` | Node | 1..16 | Readable page keys per cache: the active key plus older decrypt keys. Read by the authority. Lowering it never drops keys at once: extra decrypt keys are dropped at the next key rotation. |
| `RACER_PAGE_BYTES` | `16777216` (16 MiB) | Node | 16,777,216..16,777,216 bytes | Cluster-wide logical page size. Only 16777216 (16 MiB) is accepted: the client SDKs and the disk index do not yet support other sizes. Fixed at cluster creation: the authority publishes the value of the primary that made the first publication, records it in the `racer-state` Secret, and ignores later changes with a warning in the primary's log. Running nodes reject a publication that changes it. |
| `RACER_KUBE_API_CA` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | Node | absolute path | Kubernetes API CA bundle. |
| `RACER_KUBE_API_TOKEN` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | Node | absolute path | Default-audience token, used only for the Kubernetes API. |
| `RACER_DAEMONSET` | `racer` | Node | DNS label | Dataplane DaemonSet name. |
| `RACER_SERVICE_ACCOUNT` | `racer` | Node | DNS label | Dataplane ServiceAccount name. |
| `KUBERNETES_SERVICE_HOST` | Unset: required in managed mode | Node | text | Kubernetes API host, set by the kubelet. |
| `KUBERNETES_SERVICE_PORT` | `443` | Node | 1..65,535 | Kubernetes API port, set by the kubelet. |

#### Operator

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_NVIDIA` | Unset: `env` | Operator | `off`, `env`, `cdi` | How the operator asks the container runtime for NVIDIA driver libraries. Read by the operator from `racer-settings`; the dataplane ignores it. |

<!-- END GENERATED: racer configuration table -->

### Raw-device storage

By default Racer stores pages in slab files under `RACER_SLAB_DIRECTORY`. On a node with spare block devices, the `racer.unbounded-cloud.io/block-devices` Node annotation selects raw devices instead. The value is a Rust regular expression over the basenames in `/host/dev/disk/by-id`. Racer also matches the corresponding name under `/sys/block` when it resolves the device.

The selector is limited to 1 KiB and is not implicitly anchored. A pattern such as `^md0$` matches one name; a pattern such as `nvme` matches every basename containing that substring, including partitions and unrelated devices. Use anchors unless a broad match is deliberate.

Discovery happens only at startup. A change to the annotation requires a dataplane restart, because the store binds its segments to the selected devices when it opens them. If the selector does not compile, matches nothing, or yields no device that passes the safety checks, Racer falls back to file-backed slabs. `RACER_SLAB_BYTES_PER_WORKER` remains valid in device mode even though it does not limit device capacity; it is the fallback size.

{{< callout type="warning" >}}
Selected devices are overwritten without a signature or filesystem check. A partition is eligible too, including an unmounted OS partition if it matches. The safety checks do not prove that a device is disposable. Select devices by an exact `by-id` name that you know belongs to the cache.
{{< /callout >}}

Racer skips a device when it has child partitions, holders, a read-only state, or a mount, and when an exclusive open fails. It opens the selected devices exclusively. A busy device (`EBUSY`) is retried until `RACER_DEVICE_BUSY_WAIT_SECS` expires; the default is 120 seconds, and `0` skips a busy device immediately. All selected devices share one startup deadline, so a single slow device cannot extend startup indefinitely.

Racer validates the first and last 1 MiB of each selected device as guards. A guard region that is not zero makes the device ineligible and is reported rather than overwritten; the guard regions are excluded from usable capacity. The host `/dev` mount is read-only at `/host/dev`, but raw writes are intentional and use the device node itself; the read-only mount does not protect the disk.

Checkpoints live in `RACER_SLAB_DIRECTORY` even in device mode, so that directory must exist and remain writable. A checkpoint records the device layout and the segment geometry. Recovery accepts a checkpoint only when it matches the live layout and cluster; an older format, a different cluster ID, a changed segment size, or a different device set is rejected and the cache starts cold.

Device capacity is split into segments of `RACER_SEGMENT_BYTES`, which defaults to 1 GiB. The last partial segment on a device is unusable, so a 1 GiB segment with 16 MiB pages keeps tail waste small. Whole segments are distributed across I/O workers. Each worker's share is subject to hard caps, and a worker may receive a different number of segments on heterogeneous devices. Subsegment tails, reserves, headers, AEAD tags, and alignment all reduce usable capacity.

`RACER_DISK_PAGE_ENTRIES` and `RACER_CHECKPOINT_BYTES` can be raised in device mode: the index may grow to fit full pages after reserves, up to 1,000,000 entries per worker, and the checkpoint working memory may grow to 512 MiB. Encoded checkpoints remain capped at 64 MiB, and an oversized cut is skipped rather than written.

### Range reads

A read delivers bytes in order but fetches pages out of order. The range stream starts at the requested offset, checks local memory for the page that contains it, and fills a missing page through the normal flight path. While the client consumes the current page, Racer fetches the next pages in the window ahead of delivery. A page that is missing locally is requested from its owners over the mesh; if the local node is an owner, it fills from its origin.

Pages ahead of the reader are admitted into the window and may wait for earlier pages before delivery. This keeps the socket fed without requiring the client to ask for each page. `RACER_RANGE_WINDOW_PAGES` sets the read-ahead window, from 1 to 64 pages; the default is 4. The plaintext budget for each worker must fit one page more than the window, and the ciphertext budget must fit the window plus one stored record.

A client can also pace a subscription read with `Racer-Page-Credits` and `Racer-Byte-Credits`. Credits limit how many pages and bytes the stream may have outstanding. The parser accepts byte credits down to the smallest page any cluster may publish, and the stream requires at least one page of the cluster's actual size. Direct reads ignore credits and are paced by the dataplane with a minimum window of four pages.

Pages past the delivery window are not fetched unless `RACER_RANGE_SPILL_PAGES` is nonzero. When spill is enabled, a read may fetch that many pages beyond the window to local disk under a short hold. The default is 0, which disables spill. Spill holds are described in [Spill holds](#spill-holds).

Each page fetch is shared per object version and page. Concurrent readers of the same version join one in-flight fetch instead of issuing duplicate origin or peer requests. A read that pins an ETag always fetches the pinned version; a read without a pin resolves the current version once and keeps it for the whole read.

The retired `Racer-Ordered` request header is ignored. Older peers that do not understand a newer request fall back to a single-page request for compatibility, and the requesting node treats that fallback as a normal page response. A peer that cannot serve a page reports a typed failure, and the requester moves to the next owner or the origin.

### Host reads

A host read writes object plaintext into a file descriptor owned by the client instead of into the socket. The request is `POST /v1/objects/<key>/host-read` with exactly one `SCM_RIGHTS` descriptor attached to the request head. Any other descriptor count, a descriptor on another request, or truncated ancillary data is a `400`.

| Header | Required | Meaning |
| --- | --- | --- |
| `Racer-Destination-Offset` | yes | Byte offset in the destination file |
| `Racer-Destination-Length` | yes, nonzero | Length of the destination window |
| `Range` | no | Byte range of the object to read |
| `If-Match` | no | Strong ETag that pins a version |
| `Racer-Metadata` | no | Opaque origin metadata, validated like an object read |
| `Authorization` | no | Opaque credential passed to the origin |

The destination window is the half-open interval from the offset for the given length. Each delivered byte lands at the destination offset that corresponds to its position in the requested object range, so the destination and object ranges may start at different offsets. A request is rejected with `400` when the window overflows, when the length is zero, when the descriptor count is wrong, or when a required header is missing.

A successful response uses `Racer-Delivery: host` frames. Racer writes each page through positioned `io_uring` writes and only acknowledges a page once its bytes are visible to the client. A failure after the response head ends the frame stream; a client must treat a short delivery as an error rather than a truncated success.

When the destination is a regular file, normally a sealed `memfd`, Racer can decrypt sealed pages directly into the destination with the crypto client instead of copying through a plaintext buffer. The metrics `racer_host_read_direct_decrypt_pages_total` and `racer_host_read_direct_decrypt_bytes_total` count that path.

A host read can also fuse the RDMA receive buffer with the decrypt: when the page arrives over RDMA into a registered buffer, the owner decrypts in place and keeps the only copy. The metric `racer_host_read_fused_skipped_total` explains why fusion did not happen, with reasons `not_registered`, `retained`, `reservation`, and `pool_low`. A page ahead of its head is deferred while its rail's pool is low, so the pool is not exhausted by speculative work.

Decrypt buffers are placed on the NUMA node of the destination when that node is known, so the decrypt does not cross a NUMA boundary. `racer_host_read_fused_numa_pages_total` labels the buffer as `registered`, `store`, or `destination` and the locality as `local`, `remote`, or `unknown`, which shows whether placement matched.

The mapping cache for `memfd` destinations is bounded. `racer_host_read_mappings_cached`, `racer_host_read_mappings_cached_bytes`, `racer_host_read_mappings_created_total`, `racer_host_read_mappings_evicted_total`, and `racer_host_read_mapping_reuses_total` describe its size and reuse. Concurrent mapped page decrypts proceed per page, and request and first-page durations are exported as `racer_host_read_request_nanoseconds_{sum,count}` and `racer_host_read_first_page_nanoseconds_{sum,count}`.

`POST /v1/objects/<key>/gpu-read` follows the same shape but delivers into a GPU allocation. It requires `Racer-Allocation-Size`, which must cover the destination window, and `Racer-Gpu-Uuid`, which names the target GPU. The descriptor must be a shareable handle produced by `cuMemExportToShareableHandle`. A node without a GPU backend answers `501`; a malformed allocation, UUID, or window answers `400`. A successful response uses `Racer-Delivery: gpu`.

### Prefetch

Prefetch loads object ranges in the background without returning their contents. It is best effort: nothing is pinned, loaded pages are evicted like any other page, and a ticket can finish `PARTIAL` when some refs fail. Tickets live in memory on one node and are lost when the dataplane restarts. Resubmitting the same ticket ID after a restart is idempotent and loads only the pages that are still missing.

The API is JSON over the cache's client socket:

| Method and path | Purpose |
| --- | --- |
| `PUT /v1/prefetches/{id}` | Create or update a ticket |
| `GET /v1/prefetches/{id}` | Read status, optionally long-polling |
| `DELETE /v1/prefetches/{id}` | Cancel a ticket |
| `GET /v1/prefetches` | List this cache's tickets |

An unsupported method receives `405` with an `Allow` header: `GET, PUT, DELETE` on an item and `GET` on the collection. A ticket ID is 1 to 128 characters from `[A-Za-z0-9._-]` and may not be `.` or `..`. Query strings are accepted only on `GET`; `PUT` and `DELETE` reject an unexpected query.

A `PUT` requires `Host: racer`, `Content-Type: application/json`, and a nonzero `Content-Length` within `RACER_PREFETCH_MAX_BODY_BYTES`. It rejects transfer encodings, `Expect`, content encodings, trailers, upgrades, duplicate `Host`, `Content-Length`, `Content-Type`, or `Authorization`, and an `Authorization` value that is empty or has leading or trailing spaces. The parser accepts `Authorization` on `PUT` only, and only its presence is hashed into the ticket hash. A request head larger than `RACER_HEADER_BYTES` receives `413`.

Every response carries `Connection: close`. A long-poll `GET` ends when the ticket changes or its `wait` expires, and a client disconnect cancels the wait.

#### Request body

The body is a strict JSON object. Unknown fields are rejected, and `refs` must be a nonempty array.

| Field | Default | Rules |
| --- | --- | --- |
| `cache_uid` | none | Optional ClusterCache UID, up to 64 characters. A mismatch with the socket's cache is `409 cache_uid_mismatch`. |
| `priority` | `normal` | One of `high`, `normal`, `low`. |
| `generation` | 0 | At most `2^53-1`. |
| `sequence` | 0 | At most `2^53-1`. |
| `ttl_seconds` | 600 | At least 1, capped at `RACER_PREFETCH_MAX_TTL_SECS`. |
| `max_pending_bytes` | `RACER_PREFETCH_DEFAULT_PENDING_BYTES` | `0` selects the default, then the value is capped at `RACER_PREFETCH_MAX_PENDING_BYTES`. |
| `refs[].key` | required | Exactly 64 hexadecimal characters. |
| `refs[].etag` | none | Strong ETag, which pins a version. |
| `refs[].offset` | 0 | Start offset in the object. |
| `refs[].length` | to end of object | When present, at least 1. |
| `refs[].metadata` | none | Opaque origin metadata, validated like `Racer-Metadata`. |

Duplicate refs are allowed; the ticket deduplicates them to page units in ref order. An empty object ref, a malformed key, an invalid ETag, a zero length, or invalid metadata rejects the whole `PUT` with `400`. A version that cannot be resolved marks that ref failed with `version_changed` rather than failing the whole ticket.

Each ref reports an outcome of `succeeded`, `failed`, or `pending`. A failed ref carries one error from `version_changed`, `not_found`, `forbidden`, `origin_error`, `peer_unsupported`, `budget_timeout`, or `invalid_range`.

#### Generation and sequence

`generation` and `sequence` order updates to one ticket ID. A new ticket records the pair from its body. A later `PUT` is compared with the stored pair before any version is resolved:

| Stored position | Submitted pair | Result |
| --- | --- | --- |
| `(g, s)` | `g' < g` | `409 stale_generation` |
| `(g, s)` | `g' == g`, `s' < s` | `409 stale_sequence` |
| `(g, s)` | `g' == g`, `s' == s`, different canonical body or `Authorization` presence | `409 payload_mismatch` |
| `(g, s)` | `g' == g`, `s' == s`, same payload | The current status; the TTL is renewed |
| `(g, s)` | `g' == g`, `s' > s` | A window update |
| `(g, s)` | `g' > g` | A new generation, which replaces the wanted set |

The body hash is the SHA-256 of the canonical re-serialized body, with fields in fixed order, absent fields omitted, whitespace removed, plus one bit for the presence of `Authorization`. Two bodies that differ only in whitespace or field order have the same hash and are treated as the same payload. A rotated credential is accepted when the body and the credential presence bit match; the ticket is applied again with the new credential.

A same-generation `PUT` is a rolling window update: refs not already in the ticket's wanted set are added, and refs omitted from the update are dropped from the wanted set. Work that already finished stays counted, so an update does not restart the ticket's totals. A new generation starts a one-shot batch: the wanted set is replaced and the accumulated counters start over. Both forms renew the TTL unless the ticket has already settled.

A settled ticket keeps its final outcomes until `RACER_PREFETCH_TICKET_RETENTION_SECS` expires, which defaults to 300 seconds. During retention, `GET` returns the frozen result and a `DELETE` still cancels it. After retention the ticket is removed and a `GET` returns `404 not_found`. A ticket that expires while it still has open work is canceled with reason `expired`; a settled window that expires only releases its interest.

#### Status

A status response describes one ticket:

| Field | Meaning |
| --- | --- |
| `id`, `cache_uid` | Ticket ID and the cache's UID |
| `state` | `QUEUED`, `RUNNING`, `SUCCEEDED`, `PARTIAL`, `FAILED`, or `CANCELLED` |
| `reason` | Cancellation reason: `client`, `expired`, `superseded`, `cache_removed`, or `passthrough`; absent when not canceled |
| `priority`, `generation`, `sequence` | Current scheduling priority and position |
| `settled` | True when no unit is queued or in flight, or the ticket was released |
| `ttl_seconds`, `expires_at_ms`, `created_at_ms`, `updated_at_ms` | Timing information |
| `refs_total`, `refs_succeeded`, `refs_failed`, `refs_pending` | Ref counts |
| `bytes` | Byte accounting, described below |
| `coverage` | Optional resident coverage, present only when requested |
| `refs`, `next_cursor` | Optional ref page and cursor |

`QUEUED` means no unit has started and no ref has an error. `RUNNING` means at least one unit has started or a ref has already failed. `SUCCEEDED` means every ref succeeded. `PARTIAL` means at least one succeeded and at least one failed. `FAILED` means every ref failed. `CANCELLED` is set when a cancellation was recorded; the `reason` field says why.

A ref succeeds when every page it needs is resident or was fetched successfully. A ref that joins an in-flight fetch counts its bytes as joined, not fetched. `settled` describes the current window, not a promise that the data will stay resident: pages can be evicted afterward, and the ticket does not pin them.

The `bytes` object separates the requested amount from the work:

| Field | Meaning |
| --- | --- |
| `logical_requested` | Bytes the refs asked for, without page alignment |
| `aligned_requested` | Bytes after rounding each ref to whole pages |
| `already_resident` | Aligned bytes already present when the ticket started |
| `queued` | Bytes waiting in the queue |
| `in_flight` | Bytes being fetched |
| `joined` | Bytes served by sharing another ticket's fetch |
| `fetched_from_origin` | Bytes fetched from an origin |
| `admitted` | Bytes admitted to the cache |
| `failed` | Bytes that failed |

`GET /v1/prefetches/{id}?wait=<duration>` long-polls until the status changes or the wait expires. The wait is capped by `RACER_PREFETCH_MAX_WAIT_SECS`, and the request gets a small slack past that wait before its own deadline. `coverage=1` asks for resident coverage and adds the `coverage` object with `resident_bytes` and `observed_at_ms`; coverage probes use a share of the request budget and are skipped when the budget is too small.

`refs=failed` lists only failed refs, and `refs=all` lists every ref. Ref lists are paginated at 256 entries. A `cursor` is accepted only with `refs`, is bound to one `(generation, sequence)`, and is rejected with `400` when it belongs to another pair. The response's `next_cursor` is present only when more refs remain; a null value means the last page.

`GET /v1/prefetches` returns `{"prefetches": [...]}` with at most 1024 summaries for the socket's cache, sorted by ID. Each summary has the ID, state, reason, priority, generation, sequence, settled flag, expiry, and byte counts, but not refs.

`DELETE /v1/prefetches/{id}` cancels the ticket with reason `client` and returns the resulting status with state `CANCELLED`. Deleting an unknown ID returns `404`.

#### Errors

Every prefetch rejection is JSON with a `code` and a `message`, and the connection closes after the response. Object read errors are different: they have empty bodies, while prefetch errors always carry this JSON shape.

| Status | Codes | Meaning |
| --- | --- | --- |
| `400` | `invalid_request` | Malformed request line, query, header, JSON body, ref, or ID |
| `404` | `not_found` | No ticket with that ID on this cache |
| `405` | `method_not_allowed` | Unsupported method; the `Allow` header names the accepted methods |
| `409` | `cache_uid_mismatch`, `payload_mismatch`, `stale_sequence`, `stale_generation`, `passthrough` | The ticket conflicts with the socket's cache, an earlier body, an earlier position, or caching is disabled |
| `413` | `too_large` | Body, ref count, or request head exceeds its limit |
| `429` | `ticket_limit`, `pending_limit` | Too many tickets or too many pending bytes; `Retry-After` is 1 to 60 seconds |
| `503` | `unavailable` | The node is draining, the cache is not ready, or the request cannot be served now |

A `405` names the allowed methods, so a client does not have to guess. A `429` tells the client when to retry; the prefetch driver also retries work internally with backoff, but a client that keeps submitting at the limit receives `429` rather than an unbounded queue.

#### How prefetch runs

The scheduler dispatches strict priority: `high`, then `normal`, then `low`. Within one priority it round-robins across tickets, and within a ticket it follows ref order. A ticket dispatches only while its in-flight bytes are under its `max_pending_bytes` and the node's pending budget is under `RACER_PREFETCH_NODE_PENDING_BYTES`.

Missing pages are scheduled to their rank-0 owners. Owners fill them through the normal flight table and persist them with normal eviction, so a prefetch fill competes with demand reads through the same admission budgets. Demand reads take precedence: a prefetch fetch that can join a demand fetch does so, and a prefetch that would displace demand is deferred. A ticket on a node that is not an owner routes its fills to owners over the mesh, and concurrent prefetch and demand reads of the same page share one fill.

Owner selection uses the current membership. If the cache's `spec.nodeSelector` changes, the ticket's owners change with it, and work that is no longer wanted is canceled with reason `cache_removed`. If the cache switches to passthrough or is deleted, its tickets are canceled with reason `passthrough` or `cache_removed`.

Each owner runs at most `RACER_PREFETCH_CONCURRENT_FILLS` fills at once, 16 by default, and may be limited to `RACER_PREFETCH_ORIGIN_BYTES_PER_SEC` origin read bytes per second. A page is attempted at most `RACER_PREFETCH_MAX_ATTEMPTS` times, 5 by default, with backoff between attempts. Exhausted retries become `budget_timeout` for a deferred or overloaded unit, `origin_error` for an unavailable owner, and `peer_unsupported` when an owner cannot decode or answer the operation.

#### Prefetch settings

Prefetch settings are ordinary `racer-settings` keys. Startup rejects a combination outside the documented bounds, and every bound is listed in the generated table above.

| Setting | Default | Purpose |
| --- | --- | --- |
| `RACER_PREFETCH_MAX_BODY_BYTES` | 4 MiB | `PUT` body limit |
| `RACER_PREFETCH_MAX_REFS` | 16384 | Refs per `PUT` |
| `RACER_PREFETCH_MAX_TICKETS` | 1024 | Live and finished tickets across all caches |
| `RACER_PREFETCH_MAX_TTL_SECS` | 3600 | TTL cap |
| `RACER_PREFETCH_TICKET_RETENTION_SECS` | 300 | How long finished tickets stay visible |
| `RACER_PREFETCH_MAX_WAIT_SECS` | 60 | Long-poll `wait` cap |
| `RACER_PREFETCH_DEFAULT_PENDING_BYTES` | 1 GiB | Per-ticket in-flight bound when `max_pending_bytes` is 0 |
| `RACER_PREFETCH_MAX_PENDING_BYTES` | 16 GiB | Cap on `max_pending_bytes` |
| `RACER_PREFETCH_NODE_PENDING_BYTES` | 64 GiB | Queued and in-flight bytes on the node |
| `RACER_PREFETCH_CONCURRENT_FILLS` | 16 | Concurrent fills per owner |
| `RACER_PREFETCH_ORIGIN_BYTES_PER_SEC` | 0 (off) | Origin read rate per owner |
| `RACER_PREFETCH_MAX_ATTEMPTS` | 5 | Tries per page |

The retired `RACER_WARM_*` keys never had an effect and are removed by the operator. They are not accepted by the dataplane; a stale key in `racer-settings` is reported during validation.

#### Prefetch metrics

Prefetch metrics carry only a `priority` label, except ticket outcomes, which also carry an `outcome` label. Requested and aligned byte counters record the logical and page-aligned demand. Resident, queued, fetched, and joined bytes separate the work that was already present, still waiting, fetched from an origin, or shared.

Admitted, failed, demand-join, retry, deferral, and throttle counters describe scheduling. Window update and dropped byte counters describe rolling updates, and `racer_prefetch_tickets` and `racer_prefetch_queued_bytes` are gauges. `racer_prefetch_tickets_finished_total` counts outcomes, `racer_prefetch_throttle_seconds_total` counts time spent throttled, and `racer_prefetch_demand_wait_seconds` is a histogram of demand-read waits.

### Spill holds

`RACER_RANGE_SPILL_PAGES` lets a read fetch pages past its delivery window to local disk under a short hold. A hold bypasses normal eviction preference: the held page is not a candidate for eviction while the hold is active, so the read can find it when its window advances. Each held page charges one whole segment, which is conservative under fragmentation and keeps the accounting safe.

The number of held pages is bounded per I/O worker by `RACER_RETAINED_SEGMENT_PERCENT`, which defaults to 50 percent of that worker's segments and page-index capacity. Holds are released when the read advances, fails, or is canceled, and the segments become reclaimable.

The held set is refreshed on the health tick. `racer_held_pages`, `racer_held_payload_bytes`, and `racer_held_segments` are gauges of the current holds, and they can lag a burst of short reads until the next tick. A hold does not pin data beyond the read that requested it, and spill is disabled when `RACER_RANGE_SPILL_PAGES` is 0.

### Second-sight disk retention

Disk retention uses the second-sight policy by default (`RACER_ADMISSION_MODE=second-sight`). The policy does not change request admission or authorization; it decides which payloads are worth keeping on disk after they have been read.

Each page has a reader token and an interest token. A reader token records that a read consumed the page, and an interest token records that something may read it again. Both are hashed into a small Bloom history window whose size is `RACER_ADMISSION_HISTORY_BYTES`, divided among I/O workers, with a rotation period of `RACER_ADMISSION_PERIOD_SECS` (60 seconds by default). The history window bounds how much evidence the filter keeps; it is not an exact access log.

Resident pages accumulate heat, and heat decays over time. A page that is read again while it is still resident gains rank; a page that is never read again loses it. Eviction ranks candidates by the cache's share and the page's score, so a cache that is over its share is more likely to lose pages, and a low-score page is more likely to lose than a hot one.

Ownership also affects retention. A node that owns a page is more likely to keep it than a node that merely fetched it while passing it to a peer. When `RACER_ADMISSION_MODE=disabled`, retention is off and pages are evicted by the ordinary policy without the second-sight score.

The disk utilization metrics label each disk and each classification. `racer_disk_class_publications_total`, `racer_disk_class_published_payload_bytes_total`, `racer_disk_class_segment_evicted_pages_total`, `racer_disk_class_segment_evicted_payload_bytes_total`, and `racer_disk_class_read_payload_bytes_total` describe publication, eviction, and reads by class. `racer_retention_observations_total`, `racer_retention_qualified_total`, `racer_retention_persistence_attempts_total`, `racer_retention_persistence_accepted_total`, and `racer_retention_share_rejected_total` describe the policy's decisions. `racer_retention_filter_set_bits` and `racer_retention_filter_bits` report filter occupancy.

Background ownership-hint refreshes are bounded, so a large membership cannot make a retention scan unbounded. Pending and indexed payload gauges report bytes waiting for and present in the index, and the checkpoint gauges report encode work and free segments. Writer in-flight and checkpoint deferral gauges show when writes are waiting for a checkpoint fence. `racer_dirty_discard_reasons_total` explains discarded dirty pages with reasons such as `displaced`, `memory_pressure`, `owned_reclaim`, `write_failed`, `write_failed_no_space`, `write_failed_index_full`, `write_failed_io`, `superseded`, `canceled`, and `cache_removed`.

Raw-device segment tails are counted separately from file-backed slabs, because a partial segment at the end of a device is unusable. A per-disk utilization metric carries the disk label, so a node with several devices shows which one is full.

### Topology capacity and version compatibility

Placement keeps a structural estimate of 1024 bytes per cached ranking. `RACER_PLACEMENT_CACHE_BYTES` bounds the ranking cache per worker, split evenly among the pools of the newest membership. A page is ranked only over the members of its cache's pool, so a cache with a small pool does not pay for the full cluster.

The path cache stores route search results and charges the key and the route vectors it holds; it does not duplicate the shared neighbor graph, which is kept once per membership at roughly 230 bytes per member. `RACER_CACHED_PATHS` bounds entries and `RACER_PATH_CACHE_BYTES` bounds bytes. An entry larger than the byte budget is not cached, and the cache evicts least-recently-used entries to stay under both limits.

A worker allows `RACER_ACTIVE_PATH_SEARCHES_PER_WORKER` distinct unfinished searches, 8 by default, and that count is independent of the path cache size. A request that cannot start a search because all slots are busy is refused as overloaded instead of queueing without a bound. `RACER_RETAINED_SNAPSHOTS` keeps at least two topology snapshots so in-flight reads finish on the membership they started with while a new membership is adopted.

A pool of up to 256 members is a full mesh. A larger pool is split into columns, mostly by site, where every member of a column links to every other member and each member links to one delegate in every other column. Any two members of a pool are at most two links apart, and a member of a pool of `k` members has about `2.83 * sqrt(k)` neighbors. This keeps a 100,000 member pool at about 940 neighbors per member instead of a full mesh's 100,000.

A new membership reuses the old pool meshes when IDs, sites, and pool members are unchanged, so a change in endpoints or NICs does not rebuild the graph. Preparation of bounded incremental ranking hints happens outside the publication lock, and a larger change falls back to exact cooperative cold computation on demand. A publication is adopted only after its structures are ready, and shutdown joins the workers that hold them rather than freeing a graph they still use.

Routing compatibility is keyed by the `/next-hop/v1` hash domain. A node that cannot interpret a publication's schema version rejects it instead of routing on a partial graph. Quiesce traffic before a cross-version upgrade, because a mixed cluster can have nodes that understand different publication features even when the wire schema is the same.

## Elected control plane

The `racer` binary is both the cache daemon and the control binary. There is no `racer-controller` Deployment. In managed mode, every dataplane Pod runs embedded cluster control on a bounded thread, and one seat is the primary. The operator creates 17 Leases: `racer-seat-0` through `racer-seat-15`, plus `racer-primary`. The runtime never creates or deletes them.

Seats are claimed by Pods through the seat Leases. A Pod claims the lowest vacant seat below the desired count, with a jittered claim delay spread by Pod UID so a whole DaemonSet does not contend at once. A seat renews its Lease every 10 seconds with a 30 second duration, stops writing and serving after 20 seconds without a fresh renewal, and only a live seat holder may contend for the primary Lease. The primary holds `racer-primary` and is the single writer of durable state.

`RACER_CONTROL_SEATS` selects the desired count. `auto` uses one seat per 2,048 members, clamped to 3 through 16, and never more than the number of eligible Pods; a decimal from 1 to 16 fixes the count. Automatic sizing therefore gives a small cluster three seats for availability and grows to 16 at large scale.

The primary publishes durable state into the `racer-state` Secret, whose UID is the cluster ID. The Secret is created empty by the operator. The first commit initializes it and records the cluster ID; after that, a missing, wiped, or corrupt state fails closed rather than starting a new identity. Seat followers reread the Secret at least every 5 seconds, and a committed state older than 30 seconds by the local clock is not served. The primary Lease carries the committed sequence, so a follower that watches the Lease sees a new record immediately instead of waiting for its next poll.

The cluster ID binds checkpoints, page keys, and enrollment. Losing the Secret means losing the cluster identity: a node cannot recover its old cache contents, and a new identity requires re-enrollment. Back up the Secret with the rest of the installation state. The operator refuses to proceed when the DaemonSet or ServiceAccount records a different `racer-state` UID, and tells you to restore the Secret or deliberately delete both objects to start a new identity.

Page keys rotate. `RACER_RETAINED_PAGE_KEYS` controls how many readable keys a cache keeps: the active encrypt key plus older decrypt keys, 7 by default and at most 16. Lowering it never drops keys at once; extra decrypt keys are dropped at the next rotation, so a read that started before the change can still decrypt. The authority publishes the keyring to seats, and a node that lacks a key for a page reports a decrypt failure rather than returning unverified bytes.

The control API listens on port 8443 and the object sidecar webhook on port 9443. The `racer-webhook` Service has no selector; the primary lists ready seat endpoints in the `racer-webhook` EndpointSlice, and the API server reaches a seat through that slice. The operator owns the Service and the EndpointSlice, and admission policy restricts the runtime to writing only that slice. Add a NetworkPolicy in your own overlay if your cluster requires one; the manifests do not ship one.

Every Racer Pod runs control under the same ServiceAccount, so every Pod holds the same permissions. Seat and primary roles are runtime state, not a security boundary. RBAC grants Nodes read-only access, ClusterCaches read-only access, TokenReview creation, patch access to the single webhook configuration, get, list, watch, and update on the 17 Leases, get and update on `racer-state`, Pod get/list/watch/patch, get on the `racer` DaemonSet and ServiceAccount, and update on the `racer-webhook` EndpointSlice. The operator binds these grants only after the admission policies are installed and revokes the bindings if a policy is missing or altered. A compromised dataplane Pod can therefore read membership and cache definitions and can patch Pods within the policy limits, but cannot create or delete Leases, Secrets, or workloads.

The control protocol has fixed bounds. A seat accepts at most 16,384 concurrent control connections, 256 concurrent TLS handshakes, 64 concurrent webhook connections, and 32 concurrent bootstrap requests. Responses with a body are limited to 64 concurrent writes, and a queue past that waits up to 5 seconds before answering `429`. A request head is at most 16 KiB, a watch event line at most 4 MiB, a bearer token at most 16 KiB, and a Kubernetes API response other than a watch stream at most 256 MiB. The effective connection cap is lowered when `RLIMIT_NOFILE` is smaller than the fixed bounds require.

Dataplane Pods discover control seats from the endpoints published in the primary Lease and from their bootstrap response, so a seat change or primary failover does not require a dataplane restart. Each seat publishes its serving certificate and issuer root in its Lease. A client pins the endpoint and certificate it was given, and a seat that cannot present a certificate from the pinned issuer is rejected rather than trusted. A new primary reads the committed state and sequence before serving, and a seat that cannot prove its state is fresh stops serving instead of answering from a stale image.

### Operator-managed dataplane

The operator installs Racer only when a `ClusterCache` exists. With no caches, it retains an existing installation and keeps admission guards enforced, so removing the last cache does not tear down the dataplane or silently drop its Pods. The first cache creates the control objects, the `racer-settings` ConfigMap, the DaemonSet, its ServiceAccount, and the admission policies.

`racer-settings` is seeded once and then owned by the administrator. The operator preserves its data, validates every key against the exported table, and hashes the payload into the DaemonSet annotation `unbounded-cloud.io/racer-settings-hash`. A changed hash rolls the DaemonSet; an invalid value stops the rollout and keeps the previous Pods. An existing object that the operator does not manage is not adopted: it fails with `not managed by the unbounded operator; refusing adoption`, so you must remove or relabel an object created outside the operator before it can take over.

Earlier releases named the component `racer-dataplane`. The operator creates the new `racer` DaemonSet and its RBAC, waits until the new DaemonSet is observed live, and only then deletes the old DaemonSet, RoleBinding, ClusterRoleBinding, ServiceAccount, ConfigMap, and admission policies in stages. A pre-rename `RACER_DATAPLANE_NVIDIA` key becomes `RACER_NVIDIA`, and a prefixed key wins over an unprefixed one so the result does not depend on map order. Old objects are removed only after the new installation is healthy, so the old identity never keeps write access without admission limits.

`RACER_CONTROL_SEATS` and `RACER_NVIDIA` are settings the operator reads. `RACER_NVIDIA` accepts `off`, `env`, and `cdi`. `env` sets `NVIDIA_VISIBLE_DEVICES=all` and `NVIDIA_DRIVER_CAPABILITIES=compute,utility`, and mounts `/run/udev` and a cuFile log volume; runc ignores the variables, so the mode is harmless on nodes without NVIDIA. `cdi` also adds the Pod annotation `cdi.k8s.io/racer: nvidia.com/gpu=all`; containerd then fails the Pod on any node without an NVIDIA CDI spec, so use it only when every dataplane node has one. The dataplane accepts `RACER_NVIDIA` in its environment and ignores it.

Workload overrides may change scheduling, rollout, and resource fields, add new environment variables, and set host networking. Existing environment values, `envFrom`, volume mounts, volumes, the ServiceAccount, and automatic token mounting are operator-owned wiring and cannot be changed. `RACER_CONTROL_SEATS` must be set in `racer-settings`, not injected into the container, because an environment value would shadow the ConfigMap. An override that tries to change an unsupported field is reported rather than merged.

The managed dataplane binds its listeners to the Pod IP, not to every address: the peer listener uses `[$RACER_POD_IP]:8082` by default, diagnostics uses `[$RACER_POD_IP]:9090` or `[$RACER_POD_IP]:9091` when the peer port is 9090, control uses `[$RACER_POD_IP]:8443`, and webhook uses `[$RACER_POD_IP]:9443`. A standalone dataplane defaults to `0.0.0.0:7443` for peers and `127.0.0.1:9090` for diagnostics. The peer port is the port published to members, so every node must use the same value.

The managed Pod projects a `racer-control` audience token at `/var/run/racer-token/token` and an Azure federated token at `/var/run/secrets/azure/tokens/azure-identity-token`. Its identity directory is `/var/lib/racer/identity/private`, beneath the hostPath mount, so the kubelet-created `0755` directory stays untouched while the dataplane creates its private `0700` child.

### Membership and credential reconciliation

Membership combines Node objects with live Racer Pods. A Node is eligible when it has a name, a valid UID, no deletion timestamp, and no `racer.unbounded-cloud.io/exclude` label. For each eligible Node, the controller selects a Pod on that Node, preferring the operator-owned dataplane Pod, and uses the Pod's IP and peer port for the member endpoint.

The Pod report takes effect through the Node it runs on. A Node annotation for `shares` or `rdma-nics` overrides the Pod report, so an administrator can correct a node without editing the Pod template. The Pod report is a JSON object with `shares` and `rdma_nics` and is limited to 32 KiB. A missing report leaves the default shares and no rails; a malformed report or annotation is a diagnostic, and the node keeps its previous attributes when it had any.

A Node that is not Ready for longer than the grace period is left out of membership even if it was a member. Pod readiness is not used for membership, because `/readyz` depends on membership and would keep a new Pod out forever. A new Node joins only after it reports and serves, which is why a Pod that has not yet bound its peer listener is not published.

The ClusterCache UID is the cache identity, and the socket names come from `metadata.name`. A cache deletion removes its definition from the publication, and nodes stop accepting reads for the old UID and cancel its prefetch tickets. A recreated cache with the same name has a new UID and therefore an empty cache; the socket path is the same name, but the new UID means old pages are never served.

Enrollment authenticates a dataplane Pod with a TokenReview against the `racer-control` audience token. The CSR carries the Node's requested shares and NICs, and the authority issues a node certificate only after the token, the cluster ID, and the enrollment request are consistent. Each Node keeps its private key locally under `RACER_IDENTITY_DIRECTORY` and never sends it to the authority.

Key rotation issues new node and page keys without resetting counters. Retired roots are retained until their NotAfter so a stale Lease cannot break a seat that still trusts the old root. Clock synchronization matters: Lease freshness, committed-state freshness, certificate validity, and token expiry all compare wall-clock times with bounded skew, so keep nodes on a time source.

## Diagnostics and metrics

The diagnostics listener defaults to `127.0.0.1:9090` and is managed at `[$RACER_POD_IP]:9090`, or `[$RACER_POD_IP]:9091` when the peer port is 9090. Keep it on the Pod network or a loopback-only port; the endpoints expose membership, failure detail, and CPU profiles. Use `kubectl exec` or a Pod proxy with the RBAC that your cluster requires.

| Endpoint | Response | Purpose |
| --- | --- | --- |
| `/healthz` | Text | Process liveness |
| `/readyz` | Text | Admission is running and health has reached ready |
| `/metrics` | Prometheus text | Counters, gauges, and histograms |
| `/debug/membership` | Text | Published members, pools, caches, and the local view |
| `/debug/failures` | Text | Recent request, origin, peer, and disk failures |
| `/debug/aead` | Text | AEAD verification failure detail |
| `/debug/decrypt-failures` | Text | Decrypt failures by cause |
| `/debug/send-tag` | Text | Samples for the configured send-tag pair |
| `/debug/terminal` | Text | Terminal request states |
| `/debug/admission-final` | Text | Final admission outcomes |
| `/debug/candidate-final` | Text | Final candidate selection outcomes |
| `/debug/gate-events` | Text | Peer receive-gate events |
| `/debug/flight-expiry` | Text | Single-flight expiry events |
| `/debug/hardware` | JSON | Block-device and RDMA topology report |
| `/debug/pprof/profile` | Binary profile | CPU profile for 1 to 60 seconds |

`/metrics` is streamed in resumable pieces: the registry exposition first, then health. A large scrape therefore does not require one contiguous buffer. Counters use wrapping arithmetic, and Prometheus reads a wrap as a counter reset; rate queries handle that as they would a process restart.

`/debug/hardware` runs discovery on a helper thread with its own memory charge and a single in-flight slot. A second request while a discovery or its response is alive receives `409 busy`. A response that would exceed the working bound stops early with `503 response_too_large`. The report does not depend on pprof being enabled, and a hardware request does not make a running profile busy.

CPU profiles are opt-in with `RACER_PPROF_ENABLED=true`. `/debug/pprof/profile` accepts an optional `seconds` query from 1 to 60 and defaults to 30. A profile reserves a process-wide budget of 192 MiB before it opens rings or allocates, and only one profile may run at a time; a second request receives `409 busy`. A profile is refused with `404` when profiling is disabled, `400` for an invalid duration, and `503` when the process is stopping or the platform does not support sampling. The profile format and symbols are suitable for Parca. A profile contains stack samples from the whole process, so anyone who can reach the diagnostics listener can request one; keep that listener off untrusted networks and grant Pod proxy access accordingly.

Racer does not export distributed trace spans and has no OpenTelemetry exporter. Use the Prometheus metrics for rates and saturation, the debug endpoints for bounded forensic detail, and CPU profiles for where time goes. `RACER_SEND_TAG_PAIR` names a `sender,receiver` pair of node UUIDs whose peer sends get a sampled AEAD tag check, which appears in `/debug/send-tag`; it is a diagnostic switch, not a tracing system.

The metrics families cover the read path:

| Area | Representative metrics |
| --- | --- |
| Requests | `racer_requests_total`, `racer_request_errors_total`, and `_overloaded`, `_unavailable`, `_deadline`, `_cancelled`, `_other` variants |
| Hits and fills | `racer_memory_hits_total`, `racer_disk_hits_total`, `racer_peer_hits_total`, `racer_origin_fills_total`, `racer_corrupt_misses_total` |
| Delivery | `racer_delivery_bytes_total`, `racer_delivery_direct_bytes_total`, `racer_delivery_pipe_drains_total` |
| Lookup | `racer_plaintext_lookup_*`, `racer_ciphertext_lookup_*`, `racer_pending_lookup_*`, `racer_disk_index_lookup_*` with `hits`, `misses`, and `errors` |
| Buffer pool | `racer_buffer_pool_bytes`, `racer_buffer_pool_misses_total`, `racer_buffer_pool_rejects_total`, `racer_buffer_pool_unpooled_total` |
| Crypto | `racer_crypto_encrypt_*`, `racer_crypto_decrypt_*`, `racer_crypto_stolen_jobs_total`, `racer_crypto_numa_forwarded_jobs_total` |
| Peers | `racer_peer_receive_*`, `racer_peer_admission_*`, `racer_peer_probes_total`, `racer_peer_link_failures_total`, `racer_peer_connection_closes_total` |
| Origin | `racer_origin_fills_total`, `racer_origin_walk_reasons_total`, `racer_fill_origin_nanoseconds_{sum,count}` |
| Disk | `racer_disk_publications_total`, `racer_disk_indexed_payload_bytes`, `racer_dirty_discards_total`, `racer_dirty_discard_reasons_total` |
| Prefetch | `racer_prefetch_*`, with `priority` and ticket `outcome` labels |

Request error counters separate `Overloaded`, `Unavailable`, `Deadline`, `Cancelled`, and `Other`, so a dashboard can distinguish a client cancellation from a node that cannot admit work. Delivery byte metrics separate data written through a pipe from data written directly into a client descriptor.

Peer connection close metrics carry a `reason` label: `abandoned`, `canceled`, `deadline`, `overloaded`, `peer_close`, or `error`. A rise in `overloaded` means the peer connection budget is too small for the peer fan-out; a rise in `deadline` means exchanges are not completing within their attempt timeout. Origin walk reasons explain why a read went to the origin instead of a peer, including rank-0 self, miss, overdue, failed, shed, unreachable, unusable, stale, and not asked.

Buffer pool misses and rejects show allocation pressure: a miss allocates outside the pool, and a reject means the pool could not serve the request at all. `racer_buffer_pool_bytes` reports pooled bytes, not entry count. The old `racer_recycler_*` metrics are gone; see the rename table in `cmd/racer/README.md` if you are updating an older dashboard.

### GPU metrics

`racer_gpu_read_requests_total` carries an `outcome` label with values `gpu`, `not_enabled`, `wrong_device`, `busy`, `invalid`, and `failed`. `racer_gpu_enabled` and `racer_gpu_standby` are gauges that are written even without a GPU helper, so a dashboard can distinguish "GPU reads off" from "GPU helper not started".

`racer_gpu_decrypt_available` is labeled by `ordinal` and `uuid`, and `racer_gpu_staging_slots_free` and `racer_gpu_executor_queued` are labeled by `ordinal`. `racer_gpu_cufile_enabled` and `racer_gpu_cufile_disabled_total{reason}` report whether direct disk reads through cuFile are active. `racer_gpu_read_pages_total{decrypt}` separates pages decrypted by the GPU from pages decrypted by the CPU.

The counters `racer_gpu_pages_rejected_total`, `racer_gpu_cufile_staged_pages_total`, `racer_gpu_cufile_failures_total`, `racer_gpu_staging_exhausted_total`, `racer_gpu_cufile_unsupported_total`, `racer_gpu_landed_pages_decrypted_total`, `racer_gpu_peer_copies_total`, `racer_gpu_peer_copy_bytes_total`, `racer_gpu_peer_failures_total`, `racer_gpu_peer_bounces_total`, and `racer_gpu_executor_queue_full_total` describe staging, errors, and peer copies. `racer_gpu_open_requests` and `racer_gpu_leaked_resources` are gauges; a leaked resource count that does not return to zero after traffic stops is a bug.

Landing metrics are per rail and per GPU: `racer_gpu_landing_enabled{rail}`, `racer_gpu_landing_slots{rail,ordinal}`, and `racer_gpu_landing_exhausted_total{rail,ordinal}`. Landing slots are the GPUDirect RDMA buffers that peers write into; exhaustion means peers are falling back or waiting for a slot.

## Upgrade and recovery

All Racer components must run matching versions. The `racer` image, the operator, and the deployed CRD schema are versioned together, and a dataplane rejects a publication it cannot interpret. Upgrade the operator and the image together, then let the DaemonSet roll; do not run an old dataplane against a new authority for longer than a quiesced drain.

Durable installation state lives in the `racer-state` Secret and the 17 Leases. Cached bytes are disposable. A restart loses in-memory pages and in-flight reads, and it loses prefetch tickets, which clients resubmit. A node that finds a valid checkpoint restores its page index and completed-page holds, but not jobs, credentials, or in-flight reads.

Earlier releases ran control in a `racer-controller` Deployment. The current operator runs seats inside the dataplane Pods and migrates the installation: it creates the new control objects, waits for the new DaemonSet and seats, and then removes the old controller workload and its RBAC. A pre-rename installation keeps its administrator settings through the `RACER_DATAPLANE_` to `RACER_` translation and its NVIDIA mode through the `RACER_DATAPLANE_NVIDIA` to `RACER_NVIDIA` translation.

A checkpoint is bound to the cluster ID, the page size, the segment geometry, and the slab layout. Recovery refuses a checkpoint from another cluster, a different page size, a different segment size, a different device set, or an older format, and starts cold instead. A cold start is safe: it refills from the origin and peers, and the cache's priority only affects how quickly it regains its share.

The operator rewrites renamed `racer-settings` keys in place with the same value and removes the `RACER_WARM_*` keys, which never had an effect. It reports `RACER_PEER_PORT` for a manual change because the port now comes from `RACER_PEER_LISTEN`; the old variable is not accepted. The rename table in `cmd/racer/README.md` lists every old and new name, and the dataplane has no compatibility aliases.

The dataplane defaults are 16 MiB pages, 1 GiB segments, 16 GiB of file-backed slab per worker, and a free reserve of 2 segments per worker. A changed segment size or a changed page size makes existing data unreadable and starts the cache cold. Plan a layout change as a cache rebuild: drain traffic, change the geometry, and expect origin and peer fills during warmup.

## Go SDK

`pkg/racersdk` is the Go client library for Racer. It reads objects through a node-local cache and serves cache misses from your own storage. It does not provision caches or origins and does not create Kubernetes objects; those remain the job of the operator and the `ClusterCache` API.

Create one `Client` per cache and share it. `NewClient` with `ClientConfig{Cache: "names"}` connects to `/run/racer/names/client/socket`. `Client.Get` returns once Racer has accepted the read, with metadata available immediately, and `Client.Stat` returns metadata without reading content. `ReadOptions` selects a byte range or pins a version by ETag.

An `Object` is consumed in exactly one of two ways. `Object.Read` copies bytes into your buffer, which you use to hash, parse, decompress, or transform content. `Object.WriteTo` forwards the object without copying; when the writer is an `*os.File`, a `net.Conn`, or a plain-text HTTP/1 `http.ResponseWriter`, the kernel moves data from the socket with `splice(2)` and it never enters process memory. Other writers get a single reused 256 KiB buffer. `io.Copy` uses `WriteTo` automatically when the source is an `Object`, so `io.Copy(dst, object)` is the no-copy path; wrapping the object in another reader switches to `Read`.

Both paths withhold the last byte until Racer confirms the whole range was delivered intact. A read that is cut short therefore ends in an error rather than a silently truncated result, even when the destination already received the rest of the data.

An `Origin` registered with `ServeOrigin` supplies objects that no Racer in the cluster has cached yet. It binds `/run/racer/<cache>/origin/socket`, which must already exist and be owned by UID and GID 65532. One process may read through a `Client`, serve an `Origin`, or do both.

### Prefetch

The SDK exposes the prefetch API through `Client.Prefetch`, `Client.PrefetchStatus`, `Client.CancelPrefetch`, `Client.ListPrefetches`, and `SplitPrefetch`. `PrefetchWindow` wraps one ticket ID and calls `Update` or `Reset` with the next generation or sequence, and `Position` reports the current pair.

`PrefetchSpec` carries the cache UID, priority, generation, sequence, TTL, pending byte bound, authorization, and refs. `PrefetchRef` carries a key, optional ETag, offset, length, and metadata. `PrefetchStatus` mirrors the wire status, including state, reason, priority, generation, sequence, settled, TTL and timestamps, ref counts, bytes, coverage, refs, and next cursor.

`PrefetchError` carries the HTTP status and the Racer error code, and its `Is` method matches errors by code, so you can test for `not_found`, `passthrough`, `stale_generation`, or `pending_limit` without parsing strings. A window update keeps the ticket's achieved work, while `Reset` starts a new generation. Resubmitting an identical spec is idempotent.

`pkg/racersdk/racersdktest` provides a fake client for tests. The Python client in `clients/python/racer-vllm` uses the same socket paths and respects `RACER_SOCKET`; its behavior mirrors the Go client for client socket selection.

## Client socket mount

The admission webhook can mount a cache's client socket into a Pod without the object sidecar. Opt in with the Pod label `racer.unbounded-cloud.io/client-socket: enabled` and the annotation `racer.unbounded-cloud.io/client-cache: <cache>`. The mount is read-only, but a read-only mount still permits `connect(2)`: the kernel checks write permission on the socket inode, not the mount flag.

| Key | Required | Meaning |
| --- | --- | --- |
| `racer.unbounded-cloud.io/client-socket` | yes, label | `enabled` opts the Pod into the mount |
| `racer.unbounded-cloud.io/client-cache` | yes, annotation | Names the ClusterCache whose client socket is mounted |
| `racer.unbounded-cloud.io/client-containers` | no, annotation | Comma-separated init or app container names; default is every init and app container except `racer-object` |
| `racer.unbounded-cloud.io/client-socket-injected` | set by webhook | Records the mounted cache; do not edit |

The webhook adds a volume named `racer-client-socket` with a `hostPath` of type `Directory` at `/run/racer/<cache>/client`, unless the Pod already mounts that directory. It mounts the directory read-only into each selected container and sets `RACER_SOCKET=/run/racer/<cache>/client/socket`. An init container is mounted too, so it can fetch through the socket before the app starts. The Pod gets `kubernetes.io/os=linux` in its node selector, the cache's node affinity, and the exclusion label's anti-affinity.

The mount and the `racer-object` sidecar are independent opt-ins and can be combined. When both target the same cache, the webhook reuses the sidecar's existing volume instead of adding a second one.

The webhook denies a Pod with `403` when the label is set but the client cache annotation is missing or names an invalid cache, when the named ClusterCache does not exist, when `spec.nodeName` names a Node that does not match the cache selector, when a selected container already mounts the client directory or already sets `RACER_SOCKET`, when the Pod already defines a volume named `racer-client-socket`, when the Pod or node selector conflicts with Linux, and when the container list is not a set of distinct existing init or app container names. A failed cache or Node lookup is denied with `503` so the client retries instead of admitting a Pod without its cache affinity.

## GPU reads

GPU reads copy object plaintext directly into a GPU allocation. They require the CUDA dataplane image, which ships the CUDA shim `libracer_gpu_cuda.so.1`, the cuFile runtime, and `/etc/racer/cufile.json`. An image built without CUDA accepts the request but has no GPU backend and answers `501`. NVIDIA nodes need the driver libraries and, for `RACER_NVIDIA=cdi`, a CDI spec for every dataplane node. The dataplane does not request `nvidia.com/gpu`; the privileged container sees the device nodes.

`RACER_ENABLE_GPU` selects the mode:

| Mode | Behavior |
| --- | --- |
| `auto` | Touches no GPU until the first `gpu-read`, then turns GPU mode on if a usable GPU is found |
| `true` | Sets up GPUs at startup and fails startup when none is found |
| `false` | Turns GPU reads off; `gpu-read` reports `not_enabled` |

Discovery logs the GPUs it finds and the mode it selected. A node with no usable GPU keeps `auto` in standby: `racer_gpu_enabled` is 0, `racer_gpu_standby` is 1, and a `gpu-read` that arrives after standby gets `501` at once instead of paying the probe cost on every request.

The backend has a coordinator thread and one executor thread per GPU, plus cuFile reader threads. I/O threads talk to them through queues and one-shot channels, so a worker never blocks on CUDA.

A GPU read can be served from a plaintext-cached page by copying it into the destination. A page fetched from the origin is decrypted on the CPU before it is copied. A page that is still encrypted and needs a GPU decrypt is staged, verified, and decrypted by the executor. A page already on local disk can be read with cuFile directly into GPU memory; `RACER_GPU_PCI_P2PDMA=true` asks cuFile to use PCI P2PDMA instead of `nvidia-fs` and requires cuFile 1.13 (CUDA 12.8) or later. A page that fails verification is not written to the destination; it is reported as damaged and the request fails rather than returning unverified data.

A shared GPU fetch is reused: concurrent `gpu-read` requests for the same page and version join one fill, and the page is copied to each destination. With RDMA enabled, peers can write GPU-read pages into landing buffers in GPU memory over GPUDirect RDMA. Landing uses `RACER_GPU_LANDING_BYTES` per GPU and is disabled with `0`. A landing buffer that is exhausted is counted, and the page falls back to another path.

`RACER_GPU_STAGING_BYTES` sizes the ciphertext staging memory per GPU in 2 MiB slots, 256 MiB by default. One slot holds one sealed page with its tag. Below about 34 MiB the GPU does not decrypt and the CPU does, so do not shrink staging below that unless you intend CPU decrypt. A larger staging area allows more concurrent streams and holds more pages in flight. The window (`RACER_RANGE_WINDOW_PAGES`) and the staging slots should be tuned together: a window larger than the staging slots makes the executor queue, while a much larger staging area than the window adds memory without more concurrency.

`RACER_GPU_PEER` controls GPU-to-GPU copies of encrypted pages. `auto` uses NVLink or PCIe peer-to-peer where the driver allows it, and `off` moves such pages through host memory. The default is `auto`.

### Testing on GPUs

`make racer-gpu-test` builds the CUDA shim with its kernels and runs the GPU hardware tests. It needs `nvcc` and a GPU. `RACER_GPU_TESTS` selects the hardware-test policy; the default `require` fails when the required hardware is absent. cuFile overrides can point the test at a different library path.

The end-to-end Python tests in `clients/python/racer-vllm` use `RACER_SOCKET` and the same socket paths as the Go client. A GPU change can be exercised through the `racer gpu` workflow trigger, which runs the hardware suite on a GPU runner rather than on a CPU-only CI node.

## RDMA

RDMA is a per-hop optimization. Two nodes use it only when both are eligible for the same transfer: both publish rails, both share a nonempty Site, and each side has a rail that the other also publishes. A node that is missing a rail, a Site, or a usable port uses HTTP for that hop. HTTP fallback does not remap pages; the same page index and delivery path apply, only the transport changes.

`RACER_ENABLE_RDMA` accepts `auto`, `true`, and `false`. `auto` uses native RDMA when the image is built with the RDMA feature and ports exist at startup. `true` reserves RDMA resources even without ports, for hotplug. `false` turns RDMA off. Native RDMA is included in release images; the verbs adapter is loaded separately.

Placement is computed over pages, and a page's owners are chosen without regard to rails. A snapshot or placement decision can therefore name an owner that shares no rail with the reader, and that hop falls back to HTTP. The `RACER_REPLICA_SELECTION` setting chooses whether a non-owner asks direct neighbors first and then spreads by a per-node hash, or asks the first-ranked owner; `spread` is the default.

`rdma-nics` entries name a device, a port, a rail, and optionally a GID and a NUMA node. The control plane normalizes rails, validates duplicates and bounds, and publishes them in the membership record. A port that is down, lacks an MW2B-capable verbs port, or has no usable GID is not eligible.

### RDMA circuit breaker

`RACER_RDMA_BREAKER` enables a per-peer and per-rail circuit breaker, off by default. It trips on a hard failure: a link break, a setup failure, or a grant or send error. It also trips after three page timeouts in a row with no RDMA success between them. Load alone never trips it: a missing grant slot or flight room is backpressure, not failure.

When the breaker opens, pages to and from that peer use HTTP at once. The first cooldown is `RACER_RDMA_BREAKER_COOLDOWN_MS`, 2000 by default. It doubles for each consecutive trip, up to `RACER_RDMA_BREAKER_MAX_COOLDOWN_MS`, 30000 by default, less up to a fifth as jitter. After the cooldown the breaker allows one probe page; a successful probe over RDMA closes it, and a failed probe keeps it open for the next, longer cooldown. `RACER_RDMA_PAGE_TIMEOUT_MS` bounds one page's grant or write before the page goes over HTTP in the same exchange and counts as a soft failure.

`racer_rdma_breaker_trips_total{reason}` counts trips with reasons `link_down`, `grant`, `send`, `timeout`, `slow_fallback`, and `setup`. `racer_rdma_breaker_soft_failures_total{reason}` counts soft failures with `timeout`, `slow_fallback`, and `load`, and `load` never opens the breaker. `racer_rdma_breaker_open`, `racer_rdma_breaker_closes_total`, and `racer_rdma_breaker_http_total` show the current state and the work moved to HTTP.

Activation registers each rail's page-buffer pool. The pool is split evenly over rails, and a rail without a whole buffer is not used. `racer_rdma_free_buffers` shows what is left, and each grant holds one 18 MiB buffer until its page is consumed. `racer_rdma_receive_buffer_hold_nanoseconds_{sum,count}` measures how long a receive buffer is held.

Grants are per link and bounded by the rail's pool. `racer_rdma_grant_waits_total`, `racer_rdma_grant_wait_granted_total`, `racer_rdma_grant_backpressure_total`, `racer_rdma_grant_owner_queue_full_total`, and `racer_rdma_grant_ahead_deferred_total` distinguish waiting, granting, and backpressure. `racer_rdma_owner_stalled` and `racer_rdma_grant_waiters` show a stalled owner thread or a queue of waiters.

Plan and send metrics explain fallback: `racer_rdma_plan_http_total{reason}` uses `no_rail`, `ineligible`, `overflow`, and `direct_http`, and `racer_rdma_send_declined_total{reason}` uses `not_page` and `plan_mismatch`. Link health is reported by `racer_rdma_links_ready`, `racer_rdma_links_connecting`, `racer_rdma_links_backoff`, and `racer_rdma_links_breaking`, and link breaks carry reasons such as `error`, `timeout`, `peer_closed`, `closed`, `abandoned`, `declined`, `grant_expired`, and `device_lost`.

A device that disappears is detected and counted in `racer_rdma_device_losses_total`; a replacement is counted in `racer_rdma_device_reactivations_total`. Racer refreshes its view of `/dev/char` so rdma-core can resolve a renumbered verbs device by major and minor number, and it retries activation after a restart. A node waiting for a device or a drain reports `racer_rdma_reactivation_waiting_device` or `racer_rdma_reactivation_waiting_drain`. Teardown reaper threads finish cleanup after a link is abandoned or a device is removed.

### Registration mode

Racer registers page buffers ahead of a transfer and exposes them to the sender through a grant. A grant carries a length, an address, a remote key, and a nonzero 32-bit tag plus a 16-byte transfer ID. The registration key is valid for one transfer; a zero tag, an unknown tag, a duplicate tag, or a credit overrun is rejected. This is zero-copy: the sender writes into the receiver's registered buffer, and the receiver verifies and consumes the page in place.

Each dataplane's RDMA connection manager listens on `RACER_RDMA_CM_PORT`, 18515 by default, and the port must match on every node. The peer listener and the CM port must be reachable between nodes, so use host networking or a Pod network that routes those ports. The image builds rdma-core 54 with the patches needed for RC queue pairs on Azure MANA; a different rdma-core may not support the same devices.

Rail identity is durable. The identity directory holds a versioned rail reservation journal that maps a stable hardware key, the PCI address plus port, to a rail number. A renamed device at the same PCI address keeps its rail. A withdrawn port keeps its reservation until a new port needs a rail, and the new port then takes the lowest rail whose port is absent, so a replaced device reuses its predecessor's rail instead of growing the rail space. On a node with no journal, rails follow PCI and port order, so homogeneous nodes agree on what rail N means. A malformed journal is reported as a corrupt record and is never silently renumbered.

The dataplane container must be privileged to open verbs devices, pin memory, and load the native adapter. Mount the host device tree read-only at `/host/dev`, and make sure the container runtime passes through the RDMA character devices and `/dev/infiniband`. Check `/debug/hardware` for the devices and ports Racer sees, confirm the `rdma-nics` annotation or Pod report, and watch `racer_rdma_links_ready` before expecting RDMA traffic. A node with `racer_rdma_active` 0 but `racer_rdma_links_ready` 0 is not failing; it has no eligible port.

Coordinate upgrades across both ends of a link. Restart one side at a time, expect the old links to break with `closed` or `peer_closed`, and wait for reactivation before draining the next node. Keep Site labels and rail numbers stable across the upgrade; a changed Site or rail makes the pair ineligible, moves that traffic to HTTP, and shows up in `racer_rdma_plan_http_total{reason="ineligible"}`.

## Related

- [Racer concepts]({{< relref "concepts/racer" >}}) for the cache model and the read path.
- [Racer object sidecar]({{< relref "reference/racer-object" >}}) for the injected `racer-object` sidecar.
- [Serving vLLM with Racer]({{< relref "guides/racer-vllm" >}}) for a complete client deployment.
- [GPU reference]({{< relref "reference/gpu" >}}) for NVIDIA and AMD node setup.
- [Networking reference]({{< relref "reference/networking" >}}) for Sites, gateways, and node connectivity.
