---
title: "Racer Reference"
weight: 7
description: "Racer resources, configuration, diagnostics, Go SDK, and RDMA requirements."
---

See [Racer concepts]({{< relref "concepts/racer" >}}) and the [deployment/integration guide]({{< relref "guides/racer" >}}).

## ClusterCache

| Property | Contract |
|----------|----------|
| API | `racer.unbounded-cloud.io/v1alpha1`, kind `ClusterCache` |
| Scope / short name | Cluster-scoped / `rcache` |
| Fields | Kubernetes metadata only; no `spec` or `status` |
| Name | DNS subdomain, at most 82 characters total and 63 per dot-separated label |
| Identity | Kubernetes UID, not name; deleting and recreating a cache creates a new identity |

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: artifacts
```

The first ClusterCache triggers installation by the Unbounded operator, not a Site
component flag. Cache data is disposable; this resource does not configure an origin.

| Path on the node | Owner and purpose |
|------------------|-------------------|
| `/run/racer/<name>/client/socket` | Dataplane; SDK clients connect here |
| `/run/racer/<name>/origin/socket` | Application origin adapter; dataplane calls it for upstream reads |

Pod mounts grant access: expose only the needed cache/role. Origins need existing trusted
parent directories. Clients do not automatically bypass Racer to connect directly to origins.

## Node labels and annotations

Keys below use the `racer.unbounded-cloud.io/` prefix unless shown in full.

| Key | Meaning |
|-----|---------|
| Label `exclude` | Presence excludes the node from membership and dataplane placement, regardless of value |
| Label `unbounded-cloud.io/site` | Canonical Site boundary for RDMA; absent or empty means HTTP-only |
| Annotation `shares` | Positive decimal `uint32` placement weight; overrides enrolled shares; default 4 |
| Annotation `rdma-nics` | Administrator NIC/rail policy; see [RDMA](#rdma); explicit `[]` disables eligibility |
| Annotations `enrolled-shares`, `enrolled-rdma-nics` | Controller-managed authenticated dataplane reports; do not edit |
| Annotation `last-admitted-member` | Controller-managed, Node-UID-bound retained membership; do not edit |
| Annotations `rails`, `aligned-rails` | Obsolete; ignored with diagnostics |

Malformed annotations retain admitted attributes; invalid new nodes are omitted. Site uses current labels.

## Dataplane configuration

Tune `racer-dataplane-config` in the installation namespace (normally `unbounded-system`).
The operator preserves administrator data and hashes edits into managed rollouts.
Explicit workload environment wiring takes precedence over ConfigMap imports.
Numeric values are unsigned decimal strings: `268435456`, not `256Mi`.

| Setting | Default | Scope / constraint |
|---------|---------|--------------------|
| `RACER_MAX_THREADS` | Unset (automatic) | Total userspace I/O and crypto threads; explicit value at least 2 |
| `RACER_ALLOW_SMT` | `false` | Use eligible physical cores by default; `true` opts into logical CPUs |
| `RACER_ENABLE_RDMA` | `auto` | `auto`, `true`, or `false`; see [modes](#rdma) |
| `RACER_SHARES` | `4` | Enrollment proposal; explicit Node `shares` wins |
| `RACER_PLAINTEXT_BYTES` / `RACER_CIPHERTEXT_BYTES` | Each `268435456` (256 MiB) | Independent node-wide plaintext/ciphertext admission budgets |
| `RACER_DIRTY_BYTES` | `134217728` (128 MiB) | Node-wide dirty-data budget |
| `RACER_REGISTERED_BYTES` | `134217728` (128 MiB) | Node-wide native RDMA memory budget when enabled |
| `RACER_REQUEST_CONTEXT_BYTES` | `67108864` (64 MiB) | Node-wide request-context budget |
| `RACER_METADATA_ENTRIES` | `4096` | Node-wide catalog entries; at most 1,048,576 |
| `RACER_DISK_PAGE_ENTRIES` | `65536` | Node-wide disk page-index budget; at most 1,048,576 |
| `RACER_ADMISSION_MODE` | `second-sight` | Disk retention policy: `disabled` or `second-sight`; does not disable request resource admission |
| `RACER_ADMISSION_HISTORY_BYTES` | `4194304` (4 MiB) | Node-wide Bloom history budget; 32 bytes..512 MiB, divided among final I/O workers, requiring at least 32 bytes per worker |
| `RACER_ADMISSION_PERIOD_SECS` | `60` | History rotation period; 1..86,400 seconds |
| `RACER_CHECKPOINT_BYTES` | `67108864` (64 MiB) | Node-wide checkpoint working-set budget; at most 512 MiB |
| `RACER_PLACEMENT_CACHE_BYTES` | `16777216` (16 MiB) | Node-wide placement capacity estimate, divided among I/O workers; 1,024 bytes..512 MiB, with at least 1,024 bytes per worker |
| `RACER_CACHED_PATHS` | `128` | Node-wide retained path-query entry budget, divided among I/O workers; 1..1,048,576 |
| `RACER_PATH_CACHE_BYTES` | `8388608` (8 MiB) | Node-wide accounted path-cache allocation budget, divided among I/O workers; 1 byte..512 MiB, independent of entry count |
| `RACER_ACTIVE_PATH_SEARCHES` | `8` | **Per I/O worker** distinct unfinished path-search limit; 1..64, independent of cache size |
| `RACER_SLAB_BYTES` | `1073741824` (1 GiB) | **Per I/O worker** slab geometry; positive multiple of segment size |
| `RACER_SEGMENT_BYTES` | `67108864` (64 MiB) | Per-worker segment size; must fit a page plus storage overhead |
| `RACER_FREE_SEGMENT_RESERVE` | `2` | Positive reserved segment count, less than total segments |
| `RACER_CLIENT_CONNECTIONS` | `128` | Node-wide connection admission budget, partitioned among workers |
| `RACER_CONNECTIONS_PER_NEIGHBOR` | `2` | Per-worker neighbor connection cap; at most 1024 |
| `RACER_ORIGIN_CONNECTIONS_PER_CACHE` | `8` | Per-cache, per-worker origin cap, independent of peer cap; at most 1024 and configured client connections, further clamped to the worker's connection budget |
| `RACER_PEER_INFLIGHT_MAX` / `RACER_PEER_PER_NEIGHBOR_MAX` | `256` / `32` | Node-wide peer-exchange admission caps |
| `RACER_REQUEST_TIMEOUT_MS` / `RACER_PEER_ATTEMPT_TIMEOUT_MS` | Each `30000` | Independent request / local peer-exchange deadlines; 1..86,400,000 ms |
| `RACER_READER_STALL_TIMEOUT_MS` | `10000` | At least 1 ms and no greater than request timeout |
| `RACER_SHUTDOWN_TIMEOUT_MS` | `30000` | 1..3,600,000 ms |

Automatic sizing considers CPU affinity, quota, NUMA locality, and progress reserves.
Node-wide budgets divide by final I/O worker count, not crypto thread count; tight budgets
can reduce workers. Storage grows with I/O workers. Admission budgets are not RSS limits:
allocator, TLS, and filesystem cache are additional. Invalid combinations fail validation.

### Second-sight disk retention

The default policy allows a currently owned page to persist on its first logical
read. A non-owned page becomes eligible after an independent reader observes it
within the recent history window. Retries and copy-only probes do not create
independent demand. Eligibility is determined before inserting the observation;
concurrent followers can qualify without promoting the first reader's own attempt.
Persistence remains best effort and only uses verified ciphertext. A skipped or
failed optional disk enqueue does not fail an otherwise valid read.

Each I/O worker has four rotating Bloom generations for page history. Reader
accounting uses exact operation-local interest tokens, retained across retries
and handoffs, rather than an approximate reader-history filter.
At the default period, observations remain for approximately 180-240 seconds.
History allocations round down to 32-byte units and do not exceed the node budget.
A budget too small for the final worker count fails startup rather than growing
implicitly. Bloom false positives can retain extra pages but cannot suppress
independent reader detection; history is never authorization or proof of integrity.

Bounded exact resident heat entries use the worker's disk page-index capacity plus
its pending-write queue capacity. Heat saturates at three and decays lazily by
one per 60 seconds, independently of history rotation. Current ownership adds a
soft eviction preference, not a pin; owned pages remain evictable. History and
heat are process-local hints, not checkpoint state. `disabled` restores owned-only
disk admission and neutral eviction scores; resource budgets, integrity checks,
and authorization still apply. Configuration values are validated even when the
policy is disabled.

Ownership hints use completed rankings for the current accepted placement.
Missing or obsolete hints provide no bonus until refreshed. Each worker also
refreshes indexed residents, including recovered idle pages, without requiring
foreground reads. A background step visits at most one resident, scores at most
256 members, and visits at most 64 ranking-cache CLOCK entries. Steps are spaced
at least 1 ms apart; completed passes and errors retry after 100 ms. Refresh is
independent of other placement maintenance and stops during shutdown. These
bounds limit background work, not the time to refresh every resident.

Low-cardinality `racer_retention_*` metrics aggregate all I/O workers without page,
cache, or reader labels. Counters report logical observations, qualified
observations, persistence attempts, and accepted persistence attempts. Acceptance
does not prove a completed disk publication; use `racer_disk_publications_total`
for that. Gauges report set/total Bloom bits and tracked heat entries. Samples
refresh on each worker's health tick (normally every 100 ms), so a stalled worker
leaves its last sample visible. The aggregate is not an atomic node-wide snapshot.
Storage metrics use the same health-tick snapshots. `classification` has exactly
two values, `owned` and `nonowned`, sampled from the current cache-only ownership
hint at each event. Missing or stale rankings classify as `nonowned`; the event
does not initiate a ranking. This is an event-time classification, not the class
at insertion, proof of nonownership, or an exact current resident-class aggregate.
Ownership refresh affects subsequent events only, never rewrites old counters.

| Metric | Meaning |
| --- | --- |
| `racer_disk_class_publications_total{classification}` | Completed writer publications; excludes checkpoint restoration and deduplicated enqueue |
| `racer_disk_class_published_payload_bytes_total{classification}` | Logical page payload bytes in those publications |
| `racer_disk_class_index_evicted_pages_total{classification}` | Victim mappings removed to reserve page-index capacity |
| `racer_disk_class_index_evicted_payload_bytes_total{classification}` | Logical payload bytes of those index victims |
| `racer_disk_class_segment_evicted_pages_total{classification}` | Victim mappings removed by bounded segment reclamation, including index-only segment reclamation |
| `racer_disk_class_segment_evicted_payload_bytes_total{classification}` | Logical payload bytes of those segment victims, not physical reclaimed disk space |
| `racer_disk_class_read_payload_bytes_total{classification}` | Logical page payload bytes returned successfully by StoreReader after framing and mapping checks |
| `racer_disk_pending_payload_bytes` | Total logical payload bytes owned by pending writes, including submitted writes until cleanup |
| `racer_disk_indexed_payload_bytes` | Total logical payload bytes in retained index mappings, including restored mappings |

All payload byte metrics exclude AEAD tags, record headers, and alignment padding.
Victim counters exclude invalidation, replacement, cache removal, and pending-write
discards. A mapping is counted only by the removal path that actually evicts it.
Read bytes count repeated full-page reads, including internal/copy-only consumers;
CRC and AEAD validation happen later in fill. They measure disk-to-reader payload,
not authenticated client-delivered bytes, requested-range bytes, or device I/O.
Use their rates to compare disk reuse in benchmarks, not as client throughput.
The two byte gauges are exact unclassified worker totals at sampling time, maintained
on lifecycle events without scanning all pages. A page can briefly count in both
while a published write is still pending cleanup. They are not physical allocation
or unique resident-byte totals, nor ownership-split gauges. Tracked heat entries
remain a separate measurement; persistence acceptance does not measure eviction.

### Topology capacity and version compatibility

Placement capacity uses a conservative 1,024-byte structural estimate per cached
ranking, rounding each worker's byte share down to whole entries. It is not an
allocator hard bound. Path-cache accounting includes retained buffer capacities
and reference-count headers, but excludes active search scratch, caller-held
results, membership graphs, and allocator overhead. A result larger than the
worker's path-cache budget is returned without retention. Cache entry and byte
limits do not determine search concurrency: identical canonical queries share
one active search, while distinct searches can report overload. The node's total
distinct-search concurrency can reach I/O worker count times
`RACER_ACTIVE_PATH_SEARCHES`.

The overlay is now the symmetric union of 32 independently hashed rings over
stable node IDs, replacing the position-based radix overlay. It has at most 64
neighbors per member and is independent of shares. Building a new graph costs
O(32 N log N) for bounded-size IDs and retains approximately 64N adjacency
`usize` slots plus N vector headers. On a 64-bit target at 100,000 members,
adjacency slots alone are about 48.8 MiB, in addition to membership records,
construction scratch, and any retained old snapshots. These allocations are
not covered by the path-cache byte limit.

Publications with unchanged validated membership version and content reuse the
whole membership. New versions with identical node IDs share immutable graph
storage, including shares-only or metadata-only updates; changed IDs rebuild
the graph. Membership byte estimates are cached at construction and read in
constant time, but each snapshot's estimate includes the full shared graph.
Summing them, as the publication grace budget does, is conservative rather than
deduplicated allocation accounting.

Live control updates prepare membership off the I/O polling thread. The
publication store admits one unfinished preparation job with no queued backlog;
additional preparation is rejected as overloaded until it finishes. Canceling
the waiter or reaching its deadline does not interrupt a running CPU job or
free its admission slot. Preparation does not publish by itself. Shutdown joins
the owned job rather than detaching it, so cleanup may wait beyond the request
deadline: the join has no independent timeout. Bounded input and job count are
not a hard wall-clock shutdown guarantee.

Routing selection uses `/next-hop/v5`, which independently length-prefixes the
seed, source ID, and destination ID. Both ring edges and next-hop selection
change routing compatibility. **Coordinate the cluster version transition and
quiesce traffic until all participating dataplanes use the same routing
implementation.** Matching membership version numbers do not negotiate the
algorithm, and no automatic safe mixed-version routing is assumed. The
placement slot, weighted rendezvous algorithm, and placement hash compatibility
are unchanged for identical application keys, node IDs, and shares; that does
not make mixed routing safe.

Remove obsolete `RACER_ROUTING_ALGORITHM` and `RACER_CACHED_RANKINGS` settings:
even empty values fail startup. Use the independent cache settings above, not
an algorithm selector or the old ranking-entry setting.

## Controller and workload wiring

`racer-config` holds controller settings and operator workload inputs. Listener changes
also require matching Services, probes, network policy, and monitoring configuration.

| Setting | Default | Purpose |
|---------|---------|---------|
| `RACER_CONTROL_ADDRESS` | `:8443` | Controller HTTPS listener |
| `RACER_METRICS_ADDRESS` / `RACER_PROBE_ADDRESS` | `:8080` / `:8081` | Controller metrics / health and readiness listeners |
| `RACER_PEER_PORT` | `8082` | Managed dataplane peer listener and membership publication; 1024..65535 |
| `RACER_DIAGNOSTICS_PORT` | Unset | Managed dataplane defaults to 9090, or 9091 if peer port is 9090; explicit port must be 1024..65535 and distinct |
| `RACER_HOST_NETWORK` | `false` | Explicit Racer host-network opt-in; not implied by RDMA |
| `RACER_POD_NETWORK_NODES` | Unset | JSON array of node names kept on pod networking when host networking is enabled; operator drains before moving between workloads |
| `RACER_SNAPSHOT_MAX_AGE` | `30s` | Maximum replicated snapshot age |
| `RACER_CERTIFICATE_LIFETIME` | `24h` | Dataplane leaf lifetime; 2 minutes..24 hours |
| `RACER_ROTATION_INTERVAL` / `RACER_ROTATION_PREPARE_FOR` / `RACER_ROTATION_RETAIN_FOR` | `24h` / `1h` / `48h` | Issuer/key rotation policy; interval must cover preparation and retention must cover leaf lifetime |

Duration settings use Go syntax and positive whole seconds. Quiesce traffic for peer-port
transitions; do not independently override the dataplane listener and published port.

Controller HTTPS admission is per process, including replication traffic. The Go
`Config.Limits` defaults allow `2 * wire.MaxMembers + 128` open connections (room
for independent snapshot and keyring polls), 32 concurrent TLS handshakes, 32
concurrent bearer-authenticated operations, and 128 concurrent response writes.
These are programmatic limits, not environment tuning keys. A connection retains
its slot until its socket closes, including silent TCP peers, TLS peers, idle
keep-alives, and long polls. Handshakes retain a separate slot until success or
failure and have a 30-second deadline. Excess connections or handshakes are
closed before HTTP without a queued waiter or overload response. Completed TLS
connections do not retain handshake slots. Request authentication and poll
admission remain independent; shutdown forcibly closes all admitted sockets.
Readiness also requires the cached serving certificate's DNS SANs to match the
configured replication server name. A valid certificate for another service is
not ready; reloading a correctly named certificate restores this readiness gate.

Go wire codec byte limits (64 KiB for bootstrap, 512 KiB for keyring bundles,
and 64 MiB for publications) bound encoded documents, not total heap usage.
The decoder buffers the bounded document, validates tokens against the schema,
then decodes typed state. It rejects unknown or duplicate fields and wrong shapes
without traversing their contents, and rejects excess members or RDMA NICs before
consuming the next element. Validation does not retain a generic JSON tree.
Buffers, individual tokens, base64 validation, typed collections, and subsequent
semantic validation still allocate memory; the byte cap is not a process memory
budget or a substitute for concurrency limits.

The operator owns identity/wiring: cluster UUID, URL, images, service accounts, trust,
replication identity, and durable resource names. These are not tuning keys. Node identity
comes from verified enrollment/local recovery, not a supplied Node name/UID. Do not reset
markers, credentials, or version counters to repair an existing identity.

Before any manager runnable starts, controller startup recovery has a 30-second
total deadline covering authoritative installation reads, initialization writes,
and final version validation. Caller cancellation or an earlier caller deadline
still applies. The competing-installer wait retains its own five-second limit
within that total budget. Recovery failure aborts startup without granting serving
authority; a timeout does not authorize resetting durable state.

The **bare binary** defaults `RACER_PEER_LISTEN` to `0.0.0.0:7443` and
`RACER_DIAGNOSTICS_LISTEN` to `127.0.0.1:9090`. Managed workloads bind both to the Pod IP
with the ports above. The operator also wires trust/token paths and
`/var/lib/racer/identity/private`; standalone defaults do not replace managed wiring.

### Membership and credential reconciliation

Endpoint selection uses the newest eligible Pod controlled by the current managed
DaemonSet, breaking creation-time ties by Pod UID. Failed, Succeeded, terminating,
and IP-less Pods are not eligible. Readiness does not gate membership. Pod phase
changes trigger reconciliation; readiness-only changes do not. A previously admitted
Node retains its last endpoint across discovery gaps, while a never-admitted Node
with no eligible endpoint is omitted.

A custom `RACER_DAEMONSET_NAME` must be a valid DNS subdomain and fit a Kubernetes
label value (at most 63 characters), because it also identifies the workload's
immutable instance selector.

Issued leaves start validity one minute before issuance to tolerate modest clock
skew, matching issuer roots. Expiration remains issuance time plus
`RACER_CERTIFICATE_LIFETIME`; the skew allowance does not extend expiration or grant
server-auth usage. Clocks must still be synchronized. A staged issuer must cover
its actual activation, the full interval until its replacement activates, and the
last issued leaf's lifetime. If that horizon no longer fits, the controller stages
a fresh issuer and waits a complete preparation period instead of activating the
stale issuer.

Topology and keyring reconciliation retry dependency-local timeouts or cancellation
errors while their reconcile context remains live. Cancellation or expiration of
the reconcile context itself is terminal for that operation.

## Diagnostics and metrics

| Dataplane HTTP path | Use |
|---------------------|-----|
| `/healthz` | Process liveness |
| `/readyz` | Serving readiness, including usable admission |
| `/metrics` | Prometheus metrics, including `racer_ready` and `racer_live` |
| `/debug/membership` | Applied membership state |
| `/debug/failures` | Bounded failure diagnostics |

Monitor `racer_requests_total`, `racer_request_errors_total`, `racer_overloads_total`,
and per-tier `racer_*_lookup_hits_total` / `racer_*_lookup_misses_total`. Page-source events
are not request counts or byte rates. A scrape is not readiness. Restrict diagnostics
network access, especially with host networking.

The [Racer Performance dashboard and provisioning notes](https://github.com/Azure/unbounded/blob/main/deploy/racer/grafana-README.md)
describe Prometheus labels and Grafana setup. The example port `19090` is installation-specific.
Persist scrape configuration on workload templates or supported overrides, not only live Pods.

## Go SDK

Import `github.com/Azure/unbounded/pkg/racersdk`; the SDK does not provision caches or origins.

| API | Purpose / ownership |
|-----|---------------------|
| `ParseCacheName`, `ParseKey`, `ParseETag` | Validate cache name, 32-byte key encoded as 64 lowercase hex digits, and a strong quoted entity tag |
| `NewClient(ClientConfig)` | Validate configuration without dialing; close the client when done |
| `Client.Stat(ctx, Request)` | Fresh full-object metadata on a separately reserved connection pool |
| `Client.Get(ctx, Request, ...ReadOptions)` | Ordered `Value` supporting `Read`, `WriteTo`, and `WriteToHTTP`; always close it |
| `Client.OpenPages(ctx, Request, ...ReadOptions)` | Page delivery, unordered unless requested; close the stream and release each `PageLease` before reusing its credit |
| `Client.GetStreaming` | Forward through `Value.WriteToHTTP` only; may expose an incomplete page prefix on failure |
| `Client.Stats()` | SDK connection, admission, queue, and transfer counters |
| `NewFetchContext` | Combine parsed adapter metadata and upstream authorization for `Request.Context` |
| `ServeOrigin(ctx, OriginConfig, Origin)` | Serve the application's origin callback until cancellation or listener failure |
| `NewOriginError` | Return a classified origin failure without exposing upstream details in diagnostics |
| `racersdktest.NewClient(Origin)` | Noncaching local test helper; import `github.com/Azure/unbounded/pkg/racersdk/racersdktest` and always call its returned cleanup function, not just `Client.Close` |

`ReadOptions.Offset` and `Length` select bytes; zero length means through EOF. Overlong
ranges are rejected, not truncated. Sizes/offsets must fit signed 64-bit wire values.
`Pin` selects an immutable version; a `Metadata` snapshot pins its ETag and must agree
with any explicit pin. Page size is 16 MiB; `PageCredits` accepts 1..64 and `ByteCredits`
accepts 1..64 pages of bytes. Zero selects the configured window and `PageCredits * PageSize`.
`SmallObject` reserves admission for objects no larger than one page.

Zero numeric fields choose defaults; negatives are invalid. `Cache` is required. Limits are per SDK instance.

| `ClientConfig` field | Default |
|----------------------|---------|
| `MaxConnections` / `MaxQueuedRequests` | 64 / 128 |
| `MetadataConnections` / `MetadataQueuedRequests` | 4 / 16 |
| `SmallObjectConnections` / `SmallObjectQueuedRequests` | 4 / 128 |
| `PageWindow` | 2; explicit 1..64 |
| `QueueTimeout` / `DialTimeout` | 5s / 5s |
| `ResponseHeaderTimeout` / `BodyReadTimeout` | 60s / 60s |
| `IdleConnTimeout` / `MaxConnAge` | 90s / 5m |

Contexts bound stream lifetime; body timeout bounds individual reads, not total lifetime
or caller think time. Reuse age is 75..100% of `MaxConnAge`; expiry does not interrupt responses.

| `OriginConfig` field | Default |
|----------------------|---------|
| `MaxConnections` | 128, including idle accepted connections |
| `MaxConcurrentRequests` / `MaxConcurrentHeadRequests` | 64 GET callbacks/bodies / 4 HEAD callbacks |
| `ReadHeaderTimeout` / `RequestTimeout` | 5s / 60s |
| `WriteTimeout` / `IdleTimeout` | 30s / 30s |
| `SocketMode` | `0600` |
| `RecoverStaleSocket` | `false`; opt-in recovery only for sockets created in owned-endpoint mode |

Origin callbacks may run concurrently and must honor cancellation. Return metadata/immutable
bytes atomically; HEAD returns no body. Every nonnil body transfers to the SDK even on error;
`Close` must interrupt `Read`. Recovery uses persistent lock/witness files in a user-owned,
non-group/world-writable directory, not permission to remove arbitrary preexisting sockets.

`Metadata` requires a size, strong quoted ETag, and nonnegative Unix expiration time
with millisecond precision. Optional `ContentType` is limited to 256 bytes. Origin
requests expose `Key()`, `Context()`, `Operation()`, `Pin()`, and `Range()`; honor the
pin and return exactly the resolved range from that immutable version.

Use `errors.As(err, &sdkError)` for `*racersdk.Error`, then `Kind()` and `StatusCode()`.
Kinds distinguish not found (404), version unavailable (412), range (416), unavailable/overload
(503), protocol, cancellation, deadline, and I/O failures. Pinned origin not-found becomes
version unavailable; 416 requires valid metadata. The fake does not test distributed/RDMA behavior.

## RDMA

RDMA is optional per hop. Peers need the same **nonempty Site**, the page's selected rail
in both published NIC lists, enabled native resources, and compatible local devices.
Different/missing Sites or unavailable rails retain HTTP, without remapping the page.
Site uses only `unbounded-cloud.io/site`, not `net.unbounded-cloud.io/site`. It does not
change placement; in-flight snapshots mean label changes are not immediate revocation.

| `RACER_ENABLE_RDMA` | Behavior |
|---------------------|----------|
| `auto` (default) | Reserve native resources only with usable startup hardware; otherwise HTTP-only until restart |
| `true` | Reserve native capacity even without startup hardware, allowing later discovery to activate devices |
| `false` | Disable native RDMA |

Release images include native verbs support; discovery/provider failures retain HTTP.
The Node annotation `racer.unbounded-cloud.io/rdma-nics` accepts at most 64 entries:

```json
[{"device":"mlx5_0","port":1,"rail":0}]
```

`port` is 1..255; `rail` is 0..65535. Optional `gid` is 32 lowercase hex digits;
optional `numa_node` is `uint32`. Device/port pairs must be unique; NICs may share a rail.
Workers prefer same-rail local, then unknown, then remote NUMA, spreading within the best tier.

Absent administrator policy uses active MW2B-capable ports in PCI BDF/port order, reported
during authenticated enrollment/renewal and published from `enrolled-rdma-nics`. Explicit
`[]` disables eligibility; a list overrides automatic rails. Discovery cannot infer cabling
or correct asymmetric initial inventories: use explicit mappings when ordinals do not match.

The private identity directory's `rdma-rails.json` preserves physical-port reservations
across renewal/restart: missing ports retain rails; new ports append. Do not delete it
independently of coordinated topology changes. Corruption fails enrollment instead of renumbering rails.

Dataplanes run privileged as root with escalation allowed and a read-only root filesystem.
HostPaths `/var/lib/racer/identity`, `/var/lib/racer/slabs`, and `/run/racer` are writable;
`/dev/infiniband` is read-only for directory entries, not device-I/O isolation. This is
hostPath access, not device-plugin/CDI allocation or a host security boundary. Kubelet may
create an empty device directory on HTTP-only nodes; Racer does not provision drivers/devices.

Verify from the actual Pod namespaces:

- Compatible drivers/firmware/providers, active MW2B ports, and sufficient pinned-memory/resources.
- Visible `/sys/class/infiniband`, `/sys/class/infiniband_verbs`, and `/sys/devices`
  targets; no host `/sys` mount is automatically added.
- RDMA namespace mode allows device access. Privilege/mounts do not override exclusive
  namespace ownership; explicitly enable host networking if needed, reviewing port exposure/conflicts.
- RoCE has the correct namespace-local Ethernet device, address, routing, and GID.
  The adapter uses **GID index 0**. Annotation `gid` is a matching constraint, not an index
  selector or RoCE configuration; retain HTTP when another index is required.

Coordinate controller/dataplane upgrades and quiesce traffic. The `rails`/`alignment_enabled`
to `rdma_nics` change breaks compatibility despite wire schema version 1; rolling updates
do not make mixed versions safe. Remove `RACER_RAILS`, `RACER_ALIGNED_RAILS`,
`RACER_FABRIC_PORTS`, and `RACER_FABRIC_PORTS_FILE`: even empty values fail startup.
Validate transfers, HTTP fallback, and recovery on target hardware before relying on RDMA.
