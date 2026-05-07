# OriginCache - Implementation & Operations Plan

Status: draft for review (round 2 incorporating reviewer feedback)
Owner: TBD
Targets: Phase 0 walking skeleton in this repo, growing to multi-PB multi-replica cluster

> Mechanism, decisions, internal interfaces, and flow diagrams: see [design.md](./design.md).
> Terminology and component glossary: see [design.md#3-terminology](./design.md#3-terminology).

---

## 1. Goal

Ship a read-only S3-compatible blob caching layer ("OriginCache") inside an
on-prem datacenter, fronting cloud blob storage (AWS S3 + Azure Blob).
Clients issue range reads against OriginCache; OriginCache serves from a
shared in-DC store when present, otherwise fetches from the cloud origin,
stores the chunk, and returns it. There is no client-initiated write path.

This document covers deliverable scope, repo layout, configuration, auth,
observability, phasing, testing, risks, and the approval checklist. The
mechanism that delivers this behavior is described in
[design.md](./design.md).

## 2. Scope

In scope (v1):

- Read-only S3-compatible client API: `GetObject` (with `Range`),
  `HeadObject`, `ListObjectsV2`.
- Origin adapters for AWS S3 and Azure Blob (Block Blobs only - see
  [design.md#9-azure-adapter-block-blob-only](./design.md#9-azure-adapter-block-blob-only)).
- Pluggable backing store ("CacheStore"): local filesystem for development;
  in-DC S3-compatible store (e.g. VAST) for production.
- Fixed-size chunking with stampede protection (singleflight + tee +
  spool).
- ETag-based immutable-blob model with strict `If-Match` enforcement on
  every origin range read - see
  [design.md#8-stampede-protection](./design.md#8-stampede-protection).
- Sequential read-ahead.
- Single-tenant deployment, network-perimeter trust (bearer / mTLS) on the
  client edge, separate internal mTLS listener for inter-replica RPCs, no
  SigV4 verification in v1.
- Multi-replica Kubernetes Deployment from day one. All replicas share a
  single in-DC CacheStore; rendezvous hashing on `ChunkKey` selects the
  coordinator for miss-fills; the receiving replica is the assembler that
  fans out per-chunk fill RPCs.
- Observable (Prometheus), operable (health probes, manifests, container
  image), testable in CI against `minio` and `azurite`.

Out of scope (v1):

- Writes, multipart uploads, object versioning.
- Cross-DC cache peering.
- S3 SigV4 verification on the client edge.
- Multi-tenant quotas and per-tenant credentials.
- Mutable-blob invalidation / origin event subscriptions.
- Encryption at rest beyond what the underlying CacheStore provides.

## 3. Repo layout (mirrors `machina`)

```
cmd/origincache/
    main.go                         # thin wrapper -> origincache.Run()
    origincache/
        origincache.go              # cobra root, config load, wiring
        server/                     # S3-compatible HTTP handlers (client edge)
            internal/               # internal listener handlers
                                    #   GET /internal/fill?key=<encoded ChunkKey>
internal/origincache/
    types.go                        # ChunkKey, ObjectInfo, ChunkInfo, Config
    chunker/                        # range <-> chunk math (streaming iterator)
    fetch/                          # Coordinator: meta + chunk SF, semaphore,
                                    # assembler fan-out, internal RPC client
        spool/                      # bounded local-disk staging area for in-flight
                                    # fills; slow-joiner fallback regardless of
                                    # CacheStore driver
    chunkcatalog/                   # in-memory LRU fronting CacheStore.Stat
    cachestore/
        localfs/                    # dev; link()/renameat2(RENAME_NOREPLACE);
                                    # uses internal/posixcommon for staging,
                                    # link-commit, dir-fsync helpers
        posixfs/                    # prod; shared POSIX FS (NFSv4.1+ baseline,
                                    # plus Weka native, CephFS, Lustre, GPFS);
                                    # same primitive as localfs via posixcommon;
                                    # adds backend detection, NFS minimum-version
                                    # gate, Alluxio-FUSE refusal, fan-out path
                                    # layout, SelfTestAtomicCommit at startup
        s3/                         # VAST and other in-DC S3-like stores;
                                    # PutObject + If-None-Match: *;
                                    # SelfTestAtomicCommit at startup
        internal/
            posixcommon/            # shared link()/EEXIST commit primitive,
                                    # staging-dir layout, dir-fsync, optional
                                    # 2-char hex fan-out; consumed by
                                    # cachestore/localfs and cachestore/posixfs
                                    # only; not visible above the cachestore
                                    # package boundary
    origin/
        types.go                    # Origin interface, error types incl.
                                    # OriginETagChangedError, UnsupportedBlobTypeError
        s3/                         # If-Match: <etag> on every GetRange
        azureblob/                  # Block Blob only; If-Match on Get Blob
    singleflight/                   # per-key in-flight dedupe + tee
    cluster/                        # membership refresh from headless Service
                                    # DNS (default 5s); rendezvous hashing on
                                    # pod IP; per-chunk internal fill RPC
                                    # client + server helpers
    auth/                           # bearer / mTLS verification (client edge);
                                    # internal-listener mTLS + peer-IP authz
    metrics/                        # Prometheus collectors
deploy/origincache/
    01-namespace.yaml.tmpl
    02-rbac.yaml.tmpl
    03-config.yaml.tmpl
    04-deployment.yaml.tmpl         # exposes container ports 8443 (client),
                                    # 8444 (internal), 9090 (metrics)
    05-service.yaml.tmpl            # headless service for membership
    06-service-clientvip.yaml.tmpl  # ClusterIP for client traffic
    07-networkpolicy.yaml.tmpl      # restricts ingress on :8444 to pods
                                    # labelled app=origincache in-namespace
    # 08-storage-pvc.yaml.tmpl      - RESERVED for Phase 2 cachestore/posixfs
    #                               deployments that wire the shared FS in via
    #                               a PVC + CSI driver rather than a kubelet
    #                               mount or hostPath; content deferred
    embed.go
    rendered/                       # gitignored, produced by render-manifests
images/origincache/
    Containerfile
designdocs/origincache/
    plan.md                         # this file
    design.md                       # mechanism + flow diagrams
docs/origincache/                   # post-build, distilled from plan + design
    architecture.md
    operations.md
hack/origincache/
    Makefile                        # deploy / undeploy targets
    scripts/
```

`Makefile` additions: `origincache`, `origincache-build`, `origincache-image`,
`origincache-manifests`. `make` continues to build everything.

## 4. Auth (v1)

Two listeners with two distinct trust roots.

### 4.1 Client edge listener (default `:8443`)

- Bearer token middleware: HMAC token validated against a shared secret in
  a Kubernetes Secret.
- Optional mTLS: client cert validated against a configured **client CA
  bundle** (`server.tls.client_ca_file`).
- Pluggable so SigV4 verification can land later without rewriting the
  request pipeline.

### 4.2 Internal listener (default `:8444`)

Serves `GET /internal/fill?key=<encoded ChunkKey>` for per-chunk fill RPCs
between replicas. Implementation follows
[design.md#88-internal-rpc-listener](./design.md#88-internal-rpc-listener).

- Transport: HTTP/2 over mTLS.
- Server cert: per-replica cert (e.g. cert-manager-issued) chained to a
  configured **internal CA** (`cluster.internal_tls.ca_file`). The
  internal CA is **distinct** from the client mTLS CA so a leaked client
  cert cannot be used to dial the internal listener.
- Client auth: peer presents a client cert chained to the internal CA AND
  the peer's source IP must be in the current peer-IP set
  (`Cluster.Peers()`).
- NetworkPolicy (`07-networkpolicy.yaml.tmpl`) restricts ingress on `:8444`
  to pods with label `app=origincache` in the same namespace.
- Loop prevention: receiver enforces `X-Origincache-Internal: 1` and
  self-checks `Cluster.Coordinator(k) == Self()`; on disagreement returns
  `409 Conflict` and the assembler falls back to local fill (one duplicate
  fill possible during membership flux, observable via
  `origincache_origin_duplicate_fills_total{result="commit_lost"}`).

## 5. Configuration shape

```yaml
server:
  listen: 0.0.0.0:8443
  max_response_bytes: 0                           # 0 = no cap; >0 returns
                                                  # 400 RequestSizeExceedsLimit
                                                  # (S3-style XML) with header
                                                  # x-origincache-cap-exceeded: true
                                                  # before any cache lookup.
                                                  # 416 is reserved for true
                                                  # Range vs. object-size violations.
  tls:
    cert_file: /etc/origincache/tls/tls.crt
    key_file:  /etc/origincache/tls/tls.key
    client_ca_file: /etc/origincache/tls/client-ca.crt   # optional, enables mTLS
  auth:
    mode: bearer                                  # bearer | mtls | both
    bearer_secret_file: /etc/origincache/secret/token

readyz:
  errauth_consecutive_threshold: 3                # mark NotReady after this many
                                                  # consecutive CacheStore ErrAuth;
                                                  # one non-ErrAuth success resets

metadata_ttl: 5m                                  # bounded-staleness window
                                                  # (design.md#11-bounded-staleness-contract);
                                                  # default 5m. Upper bound on
                                                  # serving stale ETag if the
                                                  # immutable-origin contract
                                                  # is violated by an operator.

negative_metadata_ttl: 60s                        # negative-cache window
                                                  # (design.md#12-create-after-404-and-negative-cache-lifecycle);
                                                  # default 60s. Upper bound on
                                                  # serving stale 404 / unsupported-
                                                  # blob-type after the operator
                                                  # uploads a previously-missing
                                                  # key. Independent of metadata_ttl;
                                                  # short by design so create-after-404
                                                  # recovery is fast.

chunking:
  size: 8MiB                                      # 4-16 MiB
  prefetch:
    enabled: true
    depth: 4
    max_inflight_per_blob: 8
    max_inflight_global: 256

list_cache:                                       # per-replica TTL'd cache
                                                  # of Origin.List responses;
                                                  # sized for FUSE-`ls` workload
                                                  # (design.md s6.2 / FW3)
  enabled: true                                   # default true; toggle off
                                                  # for diagnostics
  ttl: 60s                                        # default 60s; configurable
                                                  # 5s - 30m typical range
  max_entries: 1024                               # bounded LRU
  max_response_bytes: 1MiB                        # responses larger than this
                                                  # bypass the cache entirely
  swr_enabled: false                              # stale-while-revalidate;
                                                  # off by default
  swr_threshold_ratio: 0.5                        # background refresh trigger
                                                  # when entry age > ratio * ttl;
                                                  # only meaningful when
                                                  # swr_enabled=true

chunk_catalog:                                    # in-memory chunk presence
                                                  # cache + access tracking
                                                  # (design.md s10.2 / s13.2)
  max_entries: 100000                             # default 100K (~12 MB at
                                                  # ~120B/entry); SIZE TO
                                                  # WORKING SET per s13.3
  active_eviction:
    enabled: false                                # default false; opt-in
                                                  # (preserves v1 lifecycle-
                                                  # only behavior); enable
                                                  # for posixfs deployments
                                                  # without external sweep
    interval: 10m                                 # eviction loop period
    inactive_threshold: 24h                       # entry must be older than
                                                  # this since last access
    access_threshold: 5                           # evict only if AccessCount
                                                  # < threshold
    min_age: 5m                                   # cold-start protection;
                                                  # never evict entries
                                                  # younger than this
    max_evictions_per_run: 1000                   # bound per-cycle work

metadata_refresh:                                 # opt-in bounded-freshness
                                                  # mode (design.md s11.2 /
                                                  # FW5); proactively re-Heads
                                                  # hot keys ahead of
                                                  # metadata_ttl
  enabled: false                                  # default false; preserves
                                                  # "trust the contract"
                                                  # posture
  interval: 1m                                    # refresh-loop period
  refresh_ahead_ratio: 0.7                        # eligible when entry age
                                                  # >= ratio * metadata_ttl
                                                  # (default 0.7 * 5m = 3.5m)
  access_threshold: 5                             # only refresh hot keys
                                                  # (AccessCount >= threshold)
  min_age: 75s                                    # cold-start protection;
                                                  # never refresh entries
                                                  # younger than this
                                                  # (default = metadata_ttl/4)
  max_refreshes_per_run: 100                      # bound per-cycle work
  refresh_concurrency: 8                          # parallel refresh workers

spool:
  dir: /var/lib/origincache/spool                 # bounded local-disk staging
  max_bytes: 8GiB                                 # full-spool -> 503 Slow Down
  max_inflight: 64                                # concurrent fills using spool
  tmp_max_age: 1h                                 # crash-recovery sweep age
  require_local_fs: true                          # boot statfs(2) check; refuse
                                                  # to start if spool.dir is on
                                                  # NFS/SMB/CephFS/Lustre/GPFS/
                                                  # FUSE. Defense-in-depth: the
                                                  # spool is no longer on the
                                                  # client TTFB path in v1, but
                                                  # joiner-fallback latency
                                                  # benefits materially from
                                                  # local block storage.
                                                  # Operators with unusual
                                                  # placements MAY relax to
                                                  # false; production deploys
                                                  # are expected to keep the
                                                  # default.
                                                  # See design.md#104-spool-locality-contract.

origin:                                           # leader-side pre-header
                                                  # retry budget; transient
                                                  # origin failures retry
                                                  # invisibly to the client
                                                  # before HTTP response
                                                  # headers are committed
                                                  # (design.md s8.6 / Option D)
  retry:
    attempts: 3                                   # max attempts before giving
                                                  # up and returning 502
                                                  # OriginRetryExhausted
    backoff_initial: 100ms                        # initial backoff
    backoff_max: 2s                               # capped backoff per attempt
    max_total_duration: 5s                        # absolute wall-clock cap;
                                                  # 502 if exhausted regardless
                                                  # of attempt count. Bounded
                                                  # well below typical S3 SDK
                                                  # read timeouts (aws-sdk-go
                                                  # 30s; boto3 60s) so retries
                                                  # complete before clients
                                                  # time out.

cachestore:
  driver: localfs                                 # localfs | posixfs | s3
  localfs:
    root: /var/lib/origincache/chunks
    staging_max_age: 1h                           # sweep <root>/.staging/<uuid>
                                                  # entries older than this; staging
                                                  # MUST live inside <root> to keep
                                                  # link()/renameat2 atomic on the
                                                  # same filesystem
  posixfs:                                        # shared POSIX FS backend; same
                                                  # link()/EEXIST primitive as
                                                  # localfs but mounted on every
                                                  # replica at the same path
    root: /mnt/origincache/chunks                 # mount point + base dir; MUST
                                                  # be the same on every replica
    staging_max_age: 1h                           # sweep <root>/.staging/<uuid>
                                                  # entries older than this
    fanout_chars: 2                               # 2-char hex fan-out under
                                                  # <origin_id>/ to bound dir
                                                  # sizes; 0 disables. localfs
                                                  # does NOT enable this by
                                                  # default; posixfs does.
    backend_type: ""                              # "" = auto-detect via
                                                  # statfs(2) f_type + /proc/mounts
                                                  # (nfs|wekafs|ceph|lustre|gpfs|...);
                                                  # operator override allowed for
                                                  # backends with ambiguous magic
                                                  # numbers, logged loudly.
    nfs:
      minimum_version: "4.1"                      # refuse to start if mount
                                                  # negotiates a lower NFS version;
                                                  # see design.md#1012-cachestoreposixfs
      allow_v3: false                             # opt-in NFSv3 with loud warning
                                                  # and posixfs_nfs_v3_optin_total++;
                                                  # NEVER set true in production
      mount_check: true                           # parse /proc/mounts at boot to
                                                  # confirm vers= and sync export
                                                  # options; warn (not refuse) on
                                                  # async export
    require_atomic_link_self_test: true           # SelfTestAtomicCommit at startup;
                                                  # refuse to start if backend
                                                  # does not honor link()/EEXIST,
                                                  # directory fsync, or size verify
                                                  # via re-stat. Never disabled in
                                                  # production.
  s3:
    endpoint: https://vast.dc.example.internal
    bucket: origincache-chunks
    region: us-east-1
    credentials_file: /etc/origincache/cachestore-creds
    atomic_commit_self_test: true                 # SelfTestAtomicCommit at
                                                  # startup; refuse to start if
                                                  # backend silently overwrites
                                                  # despite If-None-Match: *
    require_unversioned_bucket: true              # boot-time GetBucketVersioning
                                                  # check (design.md s10.1.3);
                                                  # refuse to start if Status:
                                                  # Enabled or Suspended;
                                                  # required because
                                                  # If-None-Match: * is not
                                                  # honored on versioned buckets
                                                  # across all S3-compatible
                                                  # backends (notably VAST)
  circuit_breaker:                                # per-process breaker around all
                                                  # CacheStore calls; trips on
                                                  # sustained ErrTransient/ErrAuth
                                                  # to prevent amplifying degradation
    enabled: true
    error_window: 30s
    error_threshold: 10                           # ErrTransient + ErrAuth count;
                                                  # ErrNotFound does NOT
    open_duration: 30s
    half_open_probes: 3

chunkcatalog:
  max_entries: 1_000_000                          # ~128 MiB at ~128 B/entry

origin:
  id: aws-us-east-1-prod                          # deployment-scoped origin
                                                  # identifier; required;
                                                  # baked into ChunkKey and the
                                                  # on-store path so two
                                                  # deployments can safely share
                                                  # one CacheStore bucket
  target_global: 192                              # desired cluster-wide cap
                                                  # on concurrent
                                                  # Origin.GetRange (design.md
                                                  # s8.4). Per-replica cap is
                                                  # floor(target_global /
                                                  # cluster.target_replicas).
                                                  # Realized cluster-wide cap
                                                  # tracks target_global only
                                                  # when actual replica count
                                                  # equals
                                                  # cluster.target_replicas.
                                                  # Coordinated cluster-wide
                                                  # limiter is deferred future
                                                  # work (design.md s15.5).
  queue_timeout: 5s                               # bounded wait when the
                                                  # per-replica bucket is
                                                  # saturated; on timeout the
                                                  # request returns 503 Slow
                                                  # Down so clients back off
  driver: s3                                      # s3 | azureblob
  s3:
    region: us-east-1
    bucket: example-data
    credentials: env                              # env | irsa | file
  azureblob:
    account: exampleacct
    container: data
    auth: managed-identity                        # managed-identity | sas | key
    enforce_block_blob_only: true                 # locked true; setting false
                                                  # is rejected at startup
    list_mode: filter                             # filter | passthrough
    metadata_ttl: 5m
    rejection_ttl: 5m

cluster:
  enabled: true
  service: origincache.origincache.svc.cluster.local
  port: 8443                                      # client edge port on peers
                                                  # (used only as a discovery
                                                  # convention; internal RPCs
                                                  # use internal_listen below)
  membership_refresh: 5s                          # headless Service DNS poll
  internal_listen: 0.0.0.0:8444                   # per-chunk fill RPC listener
  internal_tls:
    cert_file: /etc/origincache/internal-tls/tls.crt
    key_file:  /etc/origincache/internal-tls/tls.key
    ca_file:   /etc/origincache/internal-tls/ca.crt   # internal CA, distinct
                                                      # from client CA
    server_name: origincache.<ns>.svc                 # stable SAN; pinned as
                                                      # tls.Config.ServerName by
                                                      # internal-RPC dialers
                                                      # (NOT pod IPs); per-replica
                                                      # certs MUST include this SAN
  target_replicas: 3                                  # expected replica count;
                                                      # used to compute the
                                                      # per-replica origin
                                                      # concurrency cap
                                                      # (target_per_replica =
                                                      # floor(origin.target_global /
                                                      # cluster.target_replicas))
                                                      # (design.md s8.4).
                                                      # MUST be updated after
                                                      # any sustained scale
                                                      # change. Dynamic recompute
                                                      # is deferred future work
                                                      # (design.md s15.6).
```

CacheStore eviction (TTL / lifecycle) is configured separately on the
underlying storage system and is not a cache-layer concern. See
`operations.md` for recommended baselines.

## 6. Observability

- Prometheus collectors:
  - `origincache_requests_total{op,status}`
  - `origincache_request_duration_seconds{op}` (histogram)
  - `origincache_responses_aborted_total{phase,reason}` -- mid-stream
    aborts after first byte sent (HTTP/2 `RST_STREAM` or HTTP/1.1
    `Connection: close`); `phase` in `pre_first_byte|mid_stream`
  - `origincache_chunk_hits_total`, `origincache_chunk_misses_total`
  - `origincache_chunkcatalog_hits_total`, `origincache_chunkcatalog_misses_total`
  - `origincache_chunkcatalog_entries`
  - `origincache_cachestore_stat_total{result="present|absent|error"}`
  - `origincache_cachestore_stat_duration_seconds` (histogram)
  - `origincache_origin_requests_total{origin,op,status}`
  - `origincache_origin_bytes_total{origin}`
  - `origincache_origin_request_duration_seconds{origin,op}` (histogram)
  - `origincache_origin_rejected_total{origin,reason,blob_type}`
  - `origincache_origin_etag_changed_total{origin}` -- count of `412
    Precondition Failed` responses to `If-Match: <etag>` GETs;
    leading indicator of mid-flight overwrite or stale metadata cache
  - `origincache_origin_retry_total{result="success|exhausted_attempts|exhausted_duration|etag_changed"}`
    -- one increment per request that entered the pre-header retry
    loop ([design.md s8.6](./design.md#86-failure-handling-without-re-stampede)).
    `success` = origin returned a first byte after some attempts;
    `exhausted_attempts` = ran out of attempts within the time
    budget -> 502 OriginRetryExhausted;
    `exhausted_duration` = exceeded `origin.retry.max_total_duration`
    -> 502 OriginRetryExhausted;
    `etag_changed` = OriginETagChangedError (non-retryable) -> 502
    OriginETagChanged. Sustained non-zero `exhausted_*` rates
    indicate origin health issues.
  - `origincache_origin_retry_attempts` -- histogram of attempt
    count per request that entered the retry loop. p50 should be
    1 (first attempt succeeds); a long tail toward
    `origin.retry.attempts` indicates degraded origin.
  - `origincache_responses_aborted_total{phase="pre_commit|mid_stream",reason}`
    -- response abort counters. `pre_commit` covers errors before
    response headers are sent (mostly diagnostic; the request
    typically returns a clean HTTP error). `mid_stream` covers
    aborts after the commit boundary (origin disconnect after
    first byte) and is the metric to watch for the cost paid by
    the v1 streaming design. Sustained non-zero `mid_stream` rate
    is the trigger for considering mid-stream origin resume
    ([design.md s15.4](./design.md#154-mid-stream-origin-resume)).
  - `origincache_origin_duplicate_fills_total{result="commit_won|commit_lost"}`
    - increments at every CacheStore commit attempt. The `commit_lost` rate
      quantifies cross-replica fill duplication that escaped coordinator
      routing (e.g. during membership flux during rolling restart). See
      [design.md#8-stampede-protection](./design.md#8-stampede-protection)
      and [design.md#14-horizontal-scale](./design.md#14-horizontal-scale).
  - `origincache_inflight_fills`
  - `origincache_singleflight_joiners_total`
  - `origincache_spool_bytes` -- current spool footprint
  - `origincache_spool_evictions_total{reason="committed|aborted|full"}`
  - `origincache_cluster_internal_fill_requests_total{direction="sent|received|conflict"}`
    -- `conflict` increments whenever the receiver returns `409 Conflict`
    because of a coordinator-membership disagreement
  - `origincache_cluster_internal_fill_duration_seconds` (histogram)
  - `origincache_cluster_membership_size`
  - `origincache_cluster_membership_refresh_duration_seconds` (histogram)
  - `origincache_cachestore_self_test_total{result="ok|failed"}` --
    incremented once per process start by `SelfTestAtomicCommit`
  - `origincache_cachestore_errors_total{kind="not_found|transient|auth"}`
    -- typed CacheStore error counts (see
    [design.md#102-catalog-correctness-typed-errors-circuit-breaker](./design.md#102-catalog-correctness-typed-errors-circuit-breaker));
    `not_found` is normal cold-path traffic, `transient` and `auth`
    feed the breaker and (for `auth`) the `/readyz` threshold
  - `origincache_cachestore_breaker_state` -- 0=closed, 1=open,
    2=half_open
  - `origincache_cachestore_breaker_transitions_total{from,to}` --
    breaker state-transition counter
  - `origincache_origin_inflight{origin}` -- per-replica gauge of
    in-flight `Origin.GetRange` calls; cap is
    `floor(target_global / N_replicas)` per
    [design.md#84-origin-backpressure](./design.md#84-origin-backpressure)
  - `origincache_metadata_origin_heads_total{origin,result}` --
    per-replica HEAD calls that actually reached the origin (not
    served from the metadata cache); cluster-wide bound is N per
    object per `metadata_ttl` window in v1
  - `origincache_metadata_negative_entries` -- gauge of negative
    metadata-cache entries (404 / unsupported-blob-type) currently
    held by this replica. Drains as entries expire after
    `negative_metadata_ttl`. See
    [design.md#12-create-after-404-and-negative-cache-lifecycle](./design.md#12-create-after-404-and-negative-cache-lifecycle).
  - `origincache_metadata_negative_hit_total{origin_id}` -- counter
    of requests served from a negative entry. A spike following a
    known operator upload signals create-after-404 drain in
    progress.
  - `origincache_metadata_negative_age_seconds{origin_id}` --
    histogram of negative-entry age at hit time. Upper-bound
    percentiles inform `negative_metadata_ttl` tuning.
  - `origincache_list_cache_entries` -- gauge of LIST cache size
    (current LRU population). Approaches `list_cache.max_entries`
    indicate undersizing for the workload. See
    [design.md s6.2](./design.md#62-list-request-flow).
  - `origincache_list_cache_hit_total{origin_id,result="hit|miss"}`
    -- LIST cache hit rate; `result="hit"` increments on cache
    serve, `result="miss"` on origin pass-through. Hit rate is the
    primary indicator of LIST cache effectiveness for the FUSE
    workload.
  - `origincache_list_cache_evict_total{reason="size|ttl|response_too_large"}`
    -- LIST cache evictions by trigger. `size` = LRU bound;
    `ttl` = lazy expiration on lookup; `response_too_large` =
    response exceeded `list_cache.max_response_bytes` and bypassed
    cache.
  - `origincache_list_cache_origin_calls_total{origin_id,result}`
    -- LIST calls that actually reached origin (cache miss +
    singleflight collapse). With per-replica caching, cluster-wide
    bound is N origin LIST per identical query per
    `list_cache.ttl`.
  - `origincache_list_cache_swr_refresh_total{origin_id,result}`
    -- background stale-while-revalidate refreshes. Only emitted
    when `list_cache.swr_enabled=true`.
  - `origincache_chunk_catalog_entries` -- gauge of in-memory
    ChunkCatalog size. Pinned at `chunk_catalog.max_entries`
    suggests undersizing relative to the working set
    ([design.md s13.3](./design.md#133-chunkcatalog-size-awareness-load-bearing-operational-note)).
  - `origincache_chunk_catalog_hit_total{result="hit|miss"}` --
    catalog Lookup outcomes. Sustained hit_rate < 0.7 suggests
    undersizing.
  - `origincache_chunk_catalog_evict_total{reason="size|active|forget"}`
    -- catalog evictions by trigger. `size` = LRU bound (passive);
    `active` = active eviction loop deleted from CacheStore;
    `forget` = explicit Forget (ETag changed, GetChunk ErrNotFound).
  - `origincache_chunk_catalog_active_eviction_runs_total{result="ok|breaker_open|aborted"}`
    -- active eviction loop completions. `breaker_open` means the
    loop skipped this cycle because the CacheStore breaker is
    open. Only emitted when
    `chunk_catalog.active_eviction.enabled=true`.
  - `origincache_chunk_catalog_active_eviction_candidates` --
    histogram of per-run candidate count. Visibility into
    eligible-but-not-yet-evicted entries.
  - `origincache_cachestore_delete_total{result="ok|not_found|transient|auth"}`
    -- `CacheStore.Delete` outcomes (called by active eviction).
    `not_found` is treated as success by the eviction loop
    (idempotent). `transient` and `auth` count toward the
    CacheStore circuit breaker.
  - `origincache_metadata_refresh_runs_total{result="ok|aborted|breaker_open"}`
    -- bounded-freshness mode (FW5) per-loop completions. Only
    emitted when `metadata_refresh.enabled=true`. See
    [design.md s11.2](./design.md#112-bounded-freshness-mode-optional).
  - `origincache_metadata_refresh_total{result="ok|etag_changed|error|skipped_limiter_busy"}`
    -- per-key refresh outcomes. `etag_changed` indicates an
    immutable-contract violation detected proactively (the metric
    `origincache_origin_etag_changed_total` also increments).
  - `origincache_metadata_refresh_candidates` -- histogram of
    eligible candidates per refresh-loop run. Visibility into the
    hot-key set size.
  - `origincache_metadata_refresh_lag_seconds` -- histogram of
    `(now - LastEntered)` at refresh time; should cluster around
    `metadata_refresh.refresh_ahead_ratio * metadata_ttl`.
  - `origincache_s3_versioning_check_total{result="ok|refused"}` --
    once-per-boot emission from the `cachestore/s3` versioning
    gate ([design.md s10.1.3](./design.md#1013-cachestores3)).
    `refused` indicates the bucket has versioning enabled or
    suspended; the process exits non-zero immediately after.
  - `origincache_commit_after_serve_total{result="ok|failed"}` --
    asynchronous CacheStore commits that run after the client
    response is complete; `failed` means the
    client response succeeded but the chunk was NOT recorded in the
    `ChunkCatalog` (next request refills); see
    [design.md#86-failure-handling-without-re-stampede](./design.md#86-failure-handling-without-re-stampede)
  - `origincache_localfs_dir_fsync_total{result="ok|failed"}` --
    `fsync()` of the `<root>/.staging/` and final-parent directories
    on every commit, sweep, and orphaned-staging cleanup
  - `origincache_posixfs_link_total{result="commit_won|commit_lost|error"}` --
    every `link()` no-clobber commit attempt by `cachestore/posixfs`;
    the loser of a race is `commit_lost` (returned `EEXIST`); other
    failures are `error` and feed the breaker. See
    [design.md#1012-cachestoreposixfs](./design.md#1012-cachestoreposixfs).
  - `origincache_posixfs_dir_fsync_total{result="ok|failed"}` --
    `fsync()` of `<root>/.staging/` and `<final parent>` directories
    by `cachestore/posixfs`; rate matters because a network FS may
    silently degrade dir-fsync semantics under an `async` export.
  - `origincache_posixfs_backend{type,version,major,minor}` -- info
    gauge (value=1) labelled with the auto-detected (or
    operator-overridden) backend at boot, e.g.
    `type="nfs",version="4.1"`; `type="wekafs"`; `type="ceph"`;
    `type="lustre"`; `type="gpfs"`. Used to tag every other posixfs
    metric in dashboards via `group_left`.
  - `origincache_posixfs_selftest_last_success_timestamp` -- unix
    seconds of the last successful `SelfTestAtomicCommit`; absent if
    the driver never reached a green self-test.
  - `origincache_posixfs_nfs_v3_optin_total` -- count of boot-time
    NFSv3 opt-in events (operator set
    `cachestore.posixfs.nfs.allow_v3: true`); should be `0` in
    production.
  - `origincache_posixfs_alluxio_refusal_total` -- count of boot
    refusals because the detected backend was Alluxio FUSE; should be
    `0`. Operators MUST switch to `cachestore.driver: s3` against the
    Alluxio S3 gateway.
  - `origincache_spool_locality_check_total{result="ok|refused|bypassed",fs_type}` --
    boot `statfs(2)` outcome for `spool.dir`; `refused` means the FS
    is on the network-FS denylist and the process exited non-zero;
    `bypassed` means `spool.require_local_fs=false` (test-only).
    See [design.md#104-spool-locality-contract](./design.md#104-spool-locality-contract).
  - `origincache_readyz_errauth_consecutive` -- current count of
    consecutive `ErrAuth` responses from CacheStore; flips `/readyz`
    to NotReady at `readyz.errauth_consecutive_threshold` (default 3)
- Structured logs with request IDs propagated to origin SDKs.
- `/healthz` and `/readyz`. Ready when the CacheStore is reachable, the
  CacheStore startup self-test has succeeded (s10 of design.md), the
  internal listener is bound, and origin credentials are valid. There is
  no persistent local state to load.
- Admin endpoints (gated by separate listener / auth):
  dump cluster topology, lookup chunk, force-`Forget` a catalog entry,
  dump current spool inventory.
- `kubectl unbounded origincache` subcommand for inspection (later phase).

## 7. Phased delivery

| Phase | Scope | Definition of done |
|---|---|---|
| **0 - skeleton** | `cmd/origincache` boilerplate; `Origin` and `CacheStore` interfaces; `origin/s3`; `cachestore/localfs`; in-memory `chunkcatalog`; single-process Range GET; streaming chunk iterator; `make` integration; basic unit tests | One process serves a Range GET against a real S3 bucket and re-serves it from `localfs` |
| **1 - prod basics** | `fetch.Coordinator` with chunk + meta singleflight + tee; `chunkcatalog` LRU + Stat-on-miss path with **per-entry access-frequency tracking** (FW8) and bounded by `chunk_catalog.max_entries` with size-awareness operational guidance ([design.md s13.3](./design.md#133-chunkcatalog-size-awareness-load-bearing-operational-note)); atomic CacheStore writes (`localfs` `link`/`renameat2(RENAME_NOREPLACE)` with **staging inside `<root>/.staging/<uuid>` + parent-dir fsync**); metadata cache with `metadata_ttl=5m` and **`negative_metadata_ttl=60s`** (asymmetric defaults; bounds the create-after-404 unavailability window per [design.md s12](./design.md#12-create-after-404-and-negative-cache-lifecycle)) including `metadata_negative_entries` / `metadata_negative_hit_total` / `metadata_negative_age_seconds` metrics; **per-replica LIST cache** (FW3) with default `list_cache.ttl=60s`, `max_entries=1024`, sized for FUSE-`ls` workload ([design.md s6.2](./design.md#62-list-request-flow)); **active eviction** (FW8) opt-in via `chunk_catalog.active_eviction.enabled` (default off; recommended on for posixfs deployments without external sweep) including `CacheStore.Delete` interface method; **bounded-freshness mode** (FW5) opt-in via `metadata_refresh.enabled` (default off) with hot-key detection via metadata-cache access counters ([design.md s11.2](./design.md#112-bounded-freshness-mode-optional)); **distributed origin limiter** is deferred future work (see [design.md s15.5](./design.md#155-coordinated-cluster-wide-origin-limiter)); v1 ships with a per-replica token bucket sized `floor(origin.target_global / cluster.target_replicas)` (default 64 slots/replica at `target_global=192`, `target_replicas=3`), with origin throttling responses handled by the leader's pre-header retry loop ([design.md s8.4](./design.md#84-origin-backpressure)); **bounded staleness contract documented**; **strict `If-Match: <etag>` on every `Origin.GetRange` plus `OriginETagChangedError` handling**; **typed `CacheStore` errors (`ErrNotFound|ErrTransient|ErrAuth`)** with only `ErrNotFound` triggering refill; **per-replica HEAD singleflight wording** in metadata layer; **pre-header origin retry** (`origin.retry.attempts=3`, `origin.retry.max_total_duration=5s` defaults) as the cold-path commit boundary - cold-path bytes stream origin -> client directly with bounded leader-side retry handling transient origin failures invisibly before HTTP response headers are committed; spool tees in parallel for joiner support and as the asynchronous CacheStore-commit source ([design.md s8.6](./design.md#86-failure-handling-without-re-stampede)); **mid-stream abort** on post-first-byte failure (`RST_STREAM` / `Connection: close`); **`server.max_response_bytes` cap returns `400 RequestSizeExceedsLimit`** (S3-style XML; 416 reserved for Range vs. EOF); `HeadObject`; `ListObjectsV2`; `origin/azureblob` (Block Blob only); **`cachestore/s3` versioning gate** ([design.md s10.1.3](./design.md#1013-cachestores3)) refusing to start on versioned buckets; Prometheus; structured logging; health / readiness | One replica deployed in a dev K8s cluster serving traffic against both S3 and Azure (multi-replica clustering lands in Phase 3) |
| **2 - prod backend & ops** | `cachestore/s3` for VAST with `PutObject` + `If-None-Match: *` and **`SelfTestAtomicCommit` at startup** (refuse to start if backend silently overwrites); **`cachestore/posixfs` for shared POSIX FS deployments** (NFSv4.1+ baseline, plus Weka native, CephFS, Lustre, GPFS) sharing `link()`/`EEXIST` + dir-fsync helpers with `cachestore/localfs` via `internal/origincache/cachestore/internal/posixcommon/`, with **`SelfTestAtomicCommit` at startup** (refuse to start on Alluxio FUSE, on NFS below `nfs.minimum_version=4.1` unless `nfs.allow_v3` is set, or on any backend that fails the link-EEXIST + dir-fsync + size-verify self-test) and 2-char hex fan-out under `<origin_id>/`; **`internal/origincache/fetch/spool` layer** (slow-joiner fallback regardless of CacheStore driver) **with mandatory boot `statfs(2)` locality check** that refuses to start when `spool.dir` is on a network FS (NFS / SMB / CephFS / Lustre / GPFS / FUSE); **`commit_after_serve_total{ok|failed}` async-commit metric path**; **per-process CacheStore circuit breaker** (`enabled,error_window=30s,error_threshold=10,open_duration=30s,half_open_probes=3`); **per-replica origin semaphore documented** with formula `floor(target_global / N_replicas)` + `origin_inflight` gauge; **`localfs` `staging_max_age=1h` orphaned-staging sweeper** (and equivalent `posixfs.staging_max_age=1h`); **`/readyz` ErrAuth threshold (default 3 consecutive -> NotReady)**; sequential read-ahead; bearer / mTLS auth on the client edge; `deploy/origincache/` manifests (incl. `07-networkpolicy.yaml.tmpl`); `images/origincache/` Containerfile; `docs/origincache/` published with CacheStore lifecycle policy guidance and POSIX-backend support matrix | Production-shaped service running against VAST in a real DC with the self-test green, AND a parallel green run against at least one shared-POSIX backend (NFSv4.1+ baseline) |
| **3 - cluster** | `cluster/` peer discovery from headless Service DNS; rendezvous hashing on pod IP; **per-chunk internal fill RPC** (assembler fan-out); **internal mTLS listener on `:8444`** with internal CA + peer-IP authz + **stable `ServerName=origincache.<ns>.svc`** pinned by dialers (per-replica certs MUST include this SAN) + `X-Origincache-Internal` loop prevention + `409 Conflict` on coordinator disagreement; NetworkPolicy applied; `kubectl unbounded origincache` inspection subcommand | Multi-replica Deployment sustaining target throughput; `commit_lost` rate near zero in steady state |
| **4 - optional** | NVMe / HDD tiering; S3 SigV4 verification; adaptive prefetch; deferred optimizations catalogued in [design.md s15](./design.md#15-deferred-optimizations) (edge rate limiting, cluster-wide HEAD singleflight, cluster-wide LIST coordinator) if measured to be needed | As needed |

Estimated calendar: Phase 0 + 1 ~= 3-4 focused weeks. Phase 2 + 3 another
4-6 weeks depending on ops depth.

## 8. Test strategy

- `chunker` and `singleflight`: table-driven + fuzz (`go test -fuzz`).
  Iterator must never materialize the full `[]ChunkKey` for a range;
  test with `lastChunk - firstChunk = 1_000_000` and assert bounded
  allocation.
- `chunkcatalog`: LRU eviction behavior, concurrent `Lookup` /
  `Record` / `Forget`, bounded entry count.
- `cachestore/localfs`: temp-dir integration tests including:
  - crash simulation (kill mid-write, verify `*.tmp.*` cleanup and
    recovery via the periodic sweep);
  - **two-leader race**: two goroutines both call `PutChunk(k, ..)` with
    distinct payloads; assert exactly one wins (`commit_won`), the other
    sees `EEXIST` and reports `commit_lost`, and the on-disk content
    matches the winner.
- `cachestore/s3`: integration tests against `minio` covering:
  - direct `PutObject(final, body, If-None-Match: "*")` commit;
  - **`SelfTestAtomicCommit` pass** (real `minio` returns `412` on the
    second probe write);
  - **`SelfTestAtomicCommit` fail** (mock S3 server that always returns
    `200`; assert process exits with the documented error);
  - **412 commit_lost path**: two concurrent leaders, distinct payloads;
    assert exactly one `commit_won` and one `commit_lost`, and the stored
    object equals the winner's bytes;
  - idempotent re-PUT (committed key + repeated PutObject yields 412
    without data loss).
- `origin/s3`: contract tests against `minio` in CI, including:
  - **`If-Match: <etag>` header is sent on every `GetRange`** (assert via
    request capture);
  - **412 -> `OriginETagChangedError`**: overwrite the object mid-test,
    issue `GetRange` with the old etag, assert typed error and that the
    metadata cache entry for `{origin_id, bucket, key}` is invalidated.
- `origin/azureblob`: contract tests against `azurite` in CI, including:
  - One Block Blob, one Page Blob, one Append Blob.
  - GETs against Page / Append return `502 OriginUnsupported` and
    increment `origincache_origin_rejected_total`.
  - `ListObjectsV2` in `filter` mode returns only the Block Blob and
    preserves continuation tokens across pages.
  - 1000 concurrent requests for the same Page Blob produce exactly one
    upstream `HEAD`.
  - `If-Match: <etag>` sent on every Get Blob; 412 -> `OriginETagChangedError`.
- `fetch.Coordinator` stampede tests:
  - 1000 goroutines requesting the same `ChunkKey`; mock origin called
    exactly once; all readers receive identical bytes.
  - Same as above but origin returns an error after N bytes; all
    pre-first-byte joiners get a `502`; mid-stream joiners get an aborted
    response (`RST_STREAM` or `Connection: close`); a follow-up request
    triggers exactly one new origin call.
  - All joiners cancel mid-fill; chunk still lands in cache.
  - **Mid-fill `OriginETagChangedError`**: after N bytes, mock origin
    returns 412 on `If-Match`; assert (a) leader fails the fill with
    `OriginETagChangedError`, (b) metadata cache entry invalidated, (c)
    `origincache_origin_etag_changed_total` increments, (d) pre-first-byte
    joiners receive `502`, mid-stream joiners are aborted, (e) the next
    request issues a fresh `Head`, gets a new etag, derives a new
    `ChunkKey`, and successfully fills.
  - **Slow-joiner spool fallback**: leader streams from origin via
    spool + ring buffer; one joiner is artificially slowed beyond the
    ring buffer head; assert the joiner transparently switches to
    `Spool.Reader` and receives identical bytes; spool entry is released
    after refcount hits zero.
  - **Spool exhaustion**: fill `spool.max_bytes` with held-open joiners;
    assert subsequent fill requests time out on `spool.max_inflight` and
    return `503 Slow Down` to the client.
- Cold-start: a freshly started replica receives a request for a chunk
  already present in the CacheStore; assert exactly one
  `CacheStore.Stat`, no origin call, chunk served from CacheStore,
  `ChunkCatalog` populated; subsequent request hits the catalog.
- Cluster:
  - in-process 3-replica test for assembler fan-out and per-chunk
    coordinator routing against a shared CacheStore; assert
    `origincache_origin_duplicate_fills_total{result="commit_lost"}` = 0
    under steady-state membership;
  - **internal-listener authz**: peer with valid internal cert but source
    IP outside `Cluster.Peers()` is rejected; client cert chained only to
    the *client* CA is rejected;
  - **loop prevention**: replica A forwards `/internal/fill` to replica B
    with `X-Origincache-Internal: 1`; B's view of `Coordinator(k)` is C;
    assert B returns `409 Conflict` and A falls back to local fill;
  - **1000-chunk fan-out**: client requests a `Range` spanning 1000
    distinct cold chunks across 3 replicas; assert the assembler issues
    fan-out fill RPCs concurrently up to the configured cap, response
    body is byte-identical to a direct origin read, and total origin
    GETs equal exactly 1000.
- End-to-end: docker-compose with `minio` (origin) + a second `minio`
  (CacheStore) + a single `origincache` process; scripted range-read
  scenarios incl. mid-test object overwrite to exercise the `If-Match`
  path end-to-end.
- Load test: `vegeta` / `k6` against a process backed by a mock origin with
  injected latency. Confirm origin RPS stays at exactly 1 per cold chunk
  and at most semaphore-limited overall, while client RPS scales linearly.
- **T-1a metadata_ttl bound** (`metadata` package): seed metadata cache
  with `etag=v1` at t=0; at t=`metadata_ttl - jitter`, assert reads
  still see `v1` without a new HEAD; at t=`metadata_ttl + jitter`,
  overwrite origin to `etag=v2`, assert next request triggers HEAD,
  observes `v2`, and derives a new `ChunkKey`. Asserts the staleness
  cap from
  [design.md#11-bounded-staleness-contract](./design.md#11-bounded-staleness-contract).
- **T-create-after-404a stale window**
  (`metadata` + `fetch.Coordinator`): origin returns `404` for key `K`
  at t=0; assert the cache returns `404` to the client and records a
  negative metadata entry. Operator-side mock uploads `K` to origin at
  t=`negative_metadata_ttl / 2`. At t=`negative_metadata_ttl - jitter`,
  re-issue the client GET against the same replica; assert `404` is
  still returned (negative entry still valid) and that
  `metadata_negative_hit_total` was incremented. Asserts the bound in
  [design.md#12-create-after-404-and-negative-cache-lifecycle](./design.md#12-create-after-404-and-negative-cache-lifecycle).
- **T-create-after-404b recovery**
  (`metadata` + `fetch.Coordinator`): same setup as 404a, but at
  t=`negative_metadata_ttl + jitter` re-issue the GET against the same
  replica; assert the cache re-Heads, observes `200`, and serves the
  newly-uploaded bytes via the normal fill path.
- **T-create-after-404c per-replica fan-out** (multi-replica integration):
  in a 2-replica deployment, route the original `404` GET to replica A
  only; upload `K` to origin; route a follow-up GET to replica B and
  assert it serves `200` immediately (replica B never observed the
  404, so its metadata cache is fresh); route another follow-up to
  replica A and assert it still returns `404` until its own
  `negative_metadata_ttl` window expires.
- **T-list-cache-hit** (`metadata` + `fetch.Coordinator`): identical
  LIST queries within `list_cache.ttl` -> first triggers
  `Origin.List`, second served from cache; assert
  `list_cache_hit_total{result="hit"}` increments and origin LIST
  count = 1.
- **T-list-cache-ttl-expiry**: identical LIST query at `t=0` and
  `t=list_cache.ttl + jitter` -> two `Origin.List` calls; assert
  cache expired correctly.
- **T-list-cache-response-too-large**: mock `Origin.List` returning
  a response that exceeds `list_cache.max_response_bytes` -> response
  served to client but cache not populated; assert
  `list_cache_evict_total{reason="response_too_large"}` incremented.
- **T-list-cache-error-passthrough**: `Origin.List` returns 503 ->
  error passed to client; subsequent retry calls origin again (no
  negative caching).
- **T-list-cache-pagination**: continuation tokens are part of the
  cache key -> different tokens cache independently; sequential
  page-through doesn't collide.
- **T-list-cache-swr-trigger**: with `list_cache.swr_enabled=true`,
  query at `t=0`, query at `t=ttl*ratio + jitter` -> assert
  immediate cached response AND background refresh fires; assert
  origin LIST count = 2 over the window.
- **T-list-cache-fuse-pattern**: simulate FUSE `ls` workload (1 query
  / 5s for 5 minutes against same prefix at `list_cache.ttl=60s`) ->
  assert origin LIST count == 5 (one per minute); assert all client-
  observed latencies are sub-millisecond except the 5 cache-miss
  instances.
- **T-catalog-access-tracking** (`chunkcatalog`): Lookup hits
  increment `AccessCount`; `LastAccessed` updates; cold entries
  score lower than warm entries by the eviction ordering.
- **T-catalog-cold-start-protection**: entry created at t=0 not
  eligible for active eviction at `t < min_age` regardless of
  `AccessCount`.
- **T-active-eviction-cold-chunk** (`chunkcatalog` + `cachestore`):
  chunk in CacheStore + catalog entry with `AccessCount=0`,
  `LastEntered=t-25h`, `chunk_catalog.active_eviction.enabled=true`.
  Run eviction loop. Assert `CacheStore.Delete` called; catalog
  Forgets the entry; metric
  `cachestore_delete_total{result="ok"}` increments.
- **T-active-eviction-popular-chunk**: chunk with `AccessCount=10`.
  Run eviction loop. Assert NOT deleted.
- **T-active-eviction-bounded-run**: 5000 eligible candidates,
  `max_evictions_per_run=1000`. Assert exactly 1000 deleted, 4000
  remain (next cycle catches them).
- **T-active-eviction-breaker-open**: simulate `CacheStore.Delete`
  returning `ErrTransient` repeatedly until breaker opens. Assert
  subsequent eviction runs skip with
  `active_eviction_runs_total{result="breaker_open"}`.
- **T-catalog-size-undersized**: `chunk_catalog.max_entries=10`,
  working set=100 entries. Assert hit rate < 0.7; assert
  `chunk_catalog_evict_total{reason="size"}` increments steadily.
- **T-metadata-refresh-hot-key** (`metadata`): hot entry
  (`AccessCount=10`) at age `0.7 * metadata_ttl` is refreshed by the
  bounded-freshness loop; `LastEntered` updates; client sees no
  observable change. Requires `metadata_refresh.enabled=true`.
- **T-metadata-refresh-cold-key-skipped**: cold entry
  (`AccessCount=2`) NOT refreshed even when eligible by age.
- **T-metadata-refresh-cold-start-protected**: entry created at t=0,
  hot, NOT refreshed at `t < min_age`.
- **T-metadata-refresh-etag-changed**: background refresh detects
  new ETag; metadata cache updates; old `ChunkKey`s are orphaned;
  next chunk request derives new `ChunkKey`s; metric
  `metadata_refresh_total{result="etag_changed"}` increments;
  `origin_etag_changed_total` also increments.
- **T-metadata-refresh-bounded**: 500 eligible candidates,
  `max_refreshes_per_run=100` -> exactly 100 refreshed per cycle;
  remaining catch up on subsequent cycles.
- **T-metadata-refresh-disabled**: `enabled=false` -> no background
  activity; behaves like v1.
- **T-metadata-refresh-singleflight-race**: on-demand HEAD and
  background refresh fire concurrently for the same key; per-replica
  HEAD singleflight collapses to one origin HEAD; both consumers
  get the result.
- **T-metadata-refresh-negative-entries-not-refreshed**: negative
  entry (404) under `negative_metadata_ttl` is NOT refreshed;
  expires naturally.
- **T-origin-per-replica-cap** (`origin` + mock origin): with
  `cluster.target_replicas=3` and `origin.target_global=192`
  (giving per-replica cap = 64), launch 100 concurrent
  `Origin.GetRange` calls on a single replica. Assert at most 64
  hit origin concurrently; the remainder queue up to
  `origin.queue_timeout` (5s) before returning `503 Slow Down` to
  the client. Validates the simple per-replica token bucket
  (design.md s8.4).
- **T-origin-throttle-handled-by-retry** (`origin` +
  `fetch.Coordinator` + mock origin): origin returns `503 SlowDown`
  on the first attempt and `200` on the second. Assert client sees
  a clean 200 response; assert
  `origin_retry_total{result="success"}=1`. Validates that origin
  throttling does NOT require a coordinated cluster-wide cap;
  pre-header retry handles it.
- **T-s3-versioned-bucket-refusal** (`cachestore/s3`): configure
  `cachestore/s3` against a bucket with versioning enabled; assert
  process exits non-zero with the documented error message and
  metric `s3_versioning_check_total{result="refused"}=1`.
- **T-s3-unversioned-bucket-ok** (`cachestore/s3`): configure
  `cachestore/s3` against an unversioned bucket; assert
  `GetBucketVersioning` returns `Status: Disabled`; gate passes;
  metric `s3_versioning_check_total{result="ok"}=1`; driver proceeds
  to `SelfTestAtomicCommit`.
- **T-pre-header-retry-success** (`fetch.Coordinator` + mock origin):
  origin returns transient 503 on attempt 1, 200 + bytes on attempt 2;
  assert client sees clean 200 response with no observable abort;
  assert `origin_retry_total{result="success"}=1`; assert
  `origin_retry_attempts` records 2 attempts.
- **T-pre-header-retry-exhausted-attempts**: origin returns 503 on
  every attempt within the duration budget; assert client receives
  clean `502 Bad Gateway` with code `OriginRetryExhausted` after
  `origin.retry.attempts` exhaust; assert
  `origin_retry_total{result="exhausted_attempts"}=1`.
- **T-pre-header-retry-exhausted-duration**: origin slow-503 with
  hangs that push total wall-clock past
  `origin.retry.max_total_duration`; assert client receives `502`
  before all attempts complete; assert
  `origin_retry_total{result="exhausted_duration"}=1`.
- **T-pre-header-retry-etag-changed-non-retryable**: origin returns
  `OriginETagChangedError` on attempt 1; assert NO retry happens;
  assert `502` with code `OriginETagChanged`; assert
  `origin_retry_total{result="etag_changed"}=1`; assert metadata
  cache invalidated.
- **T-pre-header-retry-cold-path-ttfb** (`fetch` + mock origin):
  with origin returning bytes after 10ms first-byte latency,
  assert client TTFB < 50ms (sum of origin first-byte + small
  pre-header retry overhead); assert NO chunk-download wait on
  the TTFB path. Validates Option D's TTFB claim
  ([design.md s8.6](./design.md#86-failure-handling-without-re-stampede)).
- **T-mid-stream-abort-first-chunk-after-commit** (`fetch` +
  `spool` + mock origin): origin succeeds for first byte; cache
  commits headers + first byte; origin disconnects at 50% of
  chunk; assert client connection aborts (HTTP/2 RST_STREAM or
  HTTP/1.1 Connection: close); assert
  `responses_aborted_total{phase="mid_stream"}=1`; client SDK
  retries (validated separately via real aws-sdk-go integration
  test).
- **T-spool-tee-joiner-during-streaming** (`fetch` + `spool`):
  leader streams 8 MiB chunk to client A; joiner B arrives at
  50% point through the singleflight; B reads from ring buffer
  while on-pace; B falls behind; B switches to spool reader; both
  finish with full chunk byte-for-byte. Confirms the spool tee
  works in parallel with client streaming and joiner-fallback is
  unaffected by the drop of the spool-fsync gate.
- **T-commit-after-serve failure** (`fetch` + `spool` + `cachestore`):
  inject CacheStore commit error after the client response is
  complete; assert the client response completes successfully
  byte-for-byte; assert
  `origincache_commit_after_serve_total{result="failed"}` == 1;
  assert `ChunkCatalog.Lookup(k)` is still a miss; assert a
  follow-up request triggers exactly one new origin GET.
- **T-3 typed CacheStore errors** (`cachestore` + `fetch`): inject each
  of `ErrNotFound|ErrTransient|ErrAuth` from `CacheStore.GetChunk`:
  - `ErrNotFound` -> miss-fill path runs, eventual 200/206 to client;
  - `ErrTransient` -> client receives `503 Slow Down` with
    `Retry-After: 1s` and `cachestore_errors_total{kind="transient"}`
    increments; no refill attempted;
  - `ErrAuth` -> client receives `502 Bad Gateway`,
    `cachestore_errors_total{kind="auth"}` increments,
    `readyz_errauth_consecutive` increments.
- **T-3 circuit breaker** (`cachestore`): inject 10 `ErrTransient` over
  30s; assert breaker opens (`breaker_state=1`,
  `breaker_transitions_total{from="closed",to="open"}` == 1); subsequent
  calls short-circuit; after 30s, the next 3 probes are allowed (half-open
  state); on all-success, breaker closes; on any failure during half-open,
  breaker re-opens.
- **T-4a per-replica origin semaphore** (`fetch`): set semaphore to 4;
  drive 16 concurrent cold misses across 16 distinct chunks; assert
  in-flight `Origin.GetRange` never exceeds 4; assert
  `origincache_origin_inflight{origin}` saturates at 4; remaining 12
  fills queue and complete in 4-wide batches.
- **T-6a localfs staging-inside-root** (`cachestore/localfs`): assert
  every commit writes to `<root>/.staging/<uuid>` (NOT `/tmp` and NOT
  the spool dir); assert `link()` to final and `unlink()` of staging
  both happen on the same filesystem; inject orphaned staging entries
  older than `staging_max_age=1h`, run sweep, assert they are removed
  and `localfs_dir_fsync_total` increments. Verify parent-dir fsync is
  invoked by intercepting the syscall via a test seam (no strace
  required).
- **T-posixfs-nfs link-EEXIST race** (`cachestore/posixfs`): two
  goroutines on two simulated replicas (two open mount handles to a
  loopback `nfsd` v4.1 export in CI) call `PutChunk(k, ..)` with
  distinct payloads; assert exactly one wins (`commit_won`,
  `posixfs_link_total{result="commit_won"}` == 1), the other observes
  `EEXIST` and reports `commit_lost`
  (`posixfs_link_total{result="commit_lost"}` == 1), and the on-disk
  content visible from a third reader matches the winner. Repeat
  against `tmpfs` (treated as local) as a control.
- **T-posixfs-nfs SelfTestAtomicCommit success** (`cachestore/posixfs`):
  boot the driver against a CI loopback `nfsd` v4.1 export with `sync`;
  assert `posixfs_selftest_last_success_timestamp` is set and the
  process accepts traffic. Repeat against an `async` export and assert
  the runbook warning is logged (note: detecting server-side `async`
  is best-effort; the size-verify step still runs and may pass even
  with `async` because the kernel client cache is consistent within a
  process).
- **T-posixfs-nfs SelfTestAtomicCommit failure** (`cachestore/posixfs`):
  boot against a mock POSIX backend (FUSE shim) that
  (a) returns `0` instead of `EEXIST` from a second `link()`, OR
  (b) silently drops the size-verify check; assert the process exits
  non-zero with the documented `cachestore/posixfs: backend does not
  honor link()/EEXIST or directory fsync; refusing to start` message.
- **T-posixfs-nfs version gate** (`cachestore/posixfs`): boot against
  a loopback NFSv3 export with `cachestore.posixfs.nfs.allow_v3:
  false` (default); assert the process exits non-zero. Then set
  `allow_v3: true` and reboot; assert the process starts with a loud
  WARN log line and `posixfs_nfs_v3_optin_total` == 1. Boot against
  NFSv4.0 with the default config; assert exit non-zero (4.0 < 4.1
  minimum and 4.0 is not v3-opt-in eligible).
- **T-posixfs-nfs Alluxio refusal** (`cachestore/posixfs`): boot
  against a FUSE mount whose `/proc/mounts` source string contains
  `alluxio` (case-insensitive); assert the process exits non-zero
  with the `cachestore/posixfs: Alluxio FUSE is unsupported` message
  and `posixfs_alluxio_refusal_total` == 1. Repeat with a non-Alluxio
  FUSE mount (e.g. a test FUSE shim) and assert the process still
  refuses (because FUSE_SUPER_MAGIC also fails the spool-locality
  check when `spool.dir` is on the same FS, AND `cachestore/posixfs`
  treats a generic FUSE backend as unverified).
- **T-posixfs-fanout** (`cachestore/posixfs`): with
  `fanout_chars: 2`, assert chunk paths under
  `<root>/<origin_id>/<hash[0:2]>/<hash>/<chunk_index>`; with
  `fanout_chars: 0`, assert paths under
  `<root>/<origin_id>/<hash>/<chunk_index>`; assert `localfs` default
  (`fanout_chars: 0` for localfs) produces the flat layout. Verify
  the same `posixcommon` package powers both code paths via a unit
  test on the helper.
- **T-spool-locality refusal** (`spool` + `cmd/origincache`): boot
  with `spool.dir` on a tmpfs-backed loopback NFS mount (CI helper);
  assert the process exits non-zero with the `spool: ... is on a
  network filesystem (nfs); ... Refusing to start` message and
  `origincache_spool_locality_check_total{result="refused",fs_type="nfs"}`
  == 1. Repeat with `spool.require_local_fs: false`; assert the
  process starts, `result="bypassed"` is emitted, and the boot log
  carries the `WARN spool.require_local_fs is disabled` line.
  Separately assert a clean local-FS run emits `result="ok"`.
- **T-D3 internal mTLS ServerName** (`cluster`): boot 3 replicas with
  per-replica certs whose only SAN is `origincache.<ns>.svc`;
  rolling-restart one pod so its IP changes; assert the dialer pins
  `tls.Config.ServerName = origincache.<ns>.svc` and the handshake
  succeeds against the new pod IP without cert reissuance.
- **T-D4 readyz on ErrAuth** (`cachestore` + `server`): inject 1
  `ErrAuth` -> `/readyz` still 200; inject 3 consecutive `ErrAuth` ->
  `/readyz` returns 503 NotReady and
  `readyz_errauth_consecutive` == 3; interleave a non-auth `ErrNotFound`
  between failures and assert it does NOT reset the counter (only a
  successful CacheStore call resets); inject success after the
  threshold trips, assert counter resets to 0 and `/readyz` returns
  200 again.
- **T-edge cap-exceeded 400** (`server`): set `max_response_bytes=1MiB`;
  request `Range: bytes=0-2097151` (2 MiB); assert response is
  `400 RequestSizeExceedsLimit` (S3-style XML body) with
  `x-origincache-cap-exceeded: true`; separately, request a Range past
  EOF and assert response is `416 Requested Range Not Satisfiable`
  (cap-exceeded MUST NOT be reported as 416).

## 9. Out of scope for v1 (explicit)

Re-stated to prevent drift:

- No write path, multipart upload, or object versioning.
- No cross-DC peering.
- No SigV4 verification.
- No multi-tenant quotas or per-tenant credentials.
- No mutable-blob invalidation. ETag change is the only signal we honor,
  and it is enforced at the origin via `If-Match` on every GET (no
  opt-out).
- No encryption at rest beyond what the underlying CacheStore provides.

## 10. Open questions / risks

- **Origin immutability is an operator contract**: OriginCache trusts
  that an `(origin_id, bucket, object_key)` is immutable for the life
  of the key (replacement must use a new key); the bounded violation
  window is `metadata_ttl` (default 5m). `If-Match: <etag>` on every
  `Origin.GetRange` is defense-in-depth that catches in-flight
  overwrites only. Operators MUST surface this contract in the consumer
  API documentation. See
  [design.md#11-bounded-staleness-contract](./design.md#11-bounded-staleness-contract).
- **Commit-after-serve failure** (decision 2b): with v1 Option D
  the cold-path bytes stream origin -> client directly; the
  CacheStore commit is async and happens after the client response
  is complete. A failure there leaves the client successful but
  the chunk uncached. Repeated
  failures are visible only via
  `origincache_commit_after_serve_total{result="failed"}` and the
  CacheStore circuit breaker; operators MUST alert on a sustained
  non-zero rate (it indicates CacheStore degradation, not request
  errors).
- **Per-replica origin semaphore is approximate**: each replica
  enforces `floor(origin.target_global / cluster.target_replicas)`
  (default 64 slots/replica at `target_global=192`,
  `target_replicas=3`). Realized cluster-wide concurrency tracks
  `target_global` only when `N_actual == cluster.target_replicas`;
  scale-out without updating the knob over-allocates against
  origin (cluster-wide cap exceeds `target_global` by
  `(N_actual - target_replicas) * target_per_replica`); scale-in
  under-allocates. Mitigations: operators MUST update
  `cluster.target_replicas` after sustained scale changes; a
  coordinated cluster-wide limiter (s15.5) and dynamic recompute
  from `len(Cluster.Peers())` (s15.6) are deferred future work.
  Origin throttling responses (`503 SlowDown` / `429`) are handled
  by the leader's pre-header retry loop (s8.6) with exponential
  backoff regardless; origin self-protects against the static-cap
  overshoot.
- **VAST `If-None-Match: *` requires unversioned bucket**: the
  `cachestore/s3` driver relies on the backend honoring
  `If-None-Match: *` to enforce no-clobber atomic commit. AWS S3
  (since 2024-08), MinIO, and VAST Cluster (non-versioned buckets
  only) are verified. The driver runs a boot-time `GetBucketVersioning`
  versioning gate ([design.md s10.1.3](./design.md#1013-cachestores3))
  and refuses to start on enabled or suspended versioning. VAST KB
  citation is in design.md. The `SelfTestAtomicCommit` probe is the
  defense-in-depth backstop if any future S3-compatible backend
  reports versioning correctly but silently overwrites anyway.
- **NFS export `async` weakens dir-fsync**: `cachestore/posixfs`
  depends on directory `fsync()` being durable on the server, which
  requires the NFS export to be `sync` (not `async`). The driver
  cannot reliably detect server-side `async` from the client; Phase 2
  ships an operator runbook entry that mandates `sync` exports and a
  best-effort warning if `/proc/mounts` reveals an `async` client mount
  option. Mitigation: the boot self-test re-`stat`s through the kernel
  client cache and catches the most common misconfigurations; persistent
  silent corruption requires both server `async` AND a
  power-loss-window-sized failure, which is outside v1's correctness
  envelope. Document this loudly in `operations.md`.
- **Weka NFS `link()` / `EEXIST` semantics not docs-confirmed**: Weka's
  NFS share (`-t nfs4` to a Weka cluster) is verified up to NFSv4.1
  (`NFS4_CREATE_SESSION`, `ATOMIC_FILEOPEN`) but the `link()` no-clobber
  return of `EEXIST` is not explicitly documented. The driver treats
  this as a "must pass `SelfTestAtomicCommit` to start" case: if Weka
  NFS fails the self-test, operators MUST switch to Weka native
  (`-t wekafs`), which is a true POSIX FS and a separately-detected
  backend. This is not a code change, only a configuration / mount-time
  decision; document the matrix in `operations.md`.
- **Alluxio FUSE is a tempting misconfiguration**: Alluxio markets a
  shared filesystem mount but provides no `link(2)` and no atomic
  no-overwrite rename, which makes it unsafe for `cachestore/posixfs`.
  The driver detects Alluxio FUSE explicitly (FUSE_SUPER_MAGIC +
  `/proc/mounts` source matches `alluxio`) and refuses to start. The
  documented workaround is `cachestore.driver: s3` against the
  Alluxio S3 gateway, which is a normal in-DC S3 backend from the
  cache layer's perspective. Operators MUST be steered to this in the
  runbook to prevent Phase-2 deployments from getting stuck.
- **Spool on a network filesystem degrades joiner-fallback latency**:
  with the v1 streaming design (Option D) the spool is no longer on
  the client TTFB path, but joiner-fallback reads still benefit
  materially from local block storage. A spool placed on NFS /
  SMB / CephFS / Lustre / GPFS / FUSE pays a network round-trip
  per joiner-fallback read, converting microsecond-class
  switchover into milliseconds-class. The cache layer enforces
  local placement at boot via `statfs(2)` and refuses to start by
  default (`spool.require_local_fs=true`; see
  [design.md#104-spool-locality-contract](./design.md#104-spool-locality-contract)).
  Operators with unusual placements (e.g., RAM-disk) MAY relax to
  `spool.require_local_fs=false`; production deployments are
  expected to keep the default. Operators should also pin
  `spool.dir` to a hostPath / local-PV pointing at NVMe and avoid
  generic-default-storage-class PVCs that may bind to network volumes.
- **Spool exhaustion under sustained burst**: `spool.max_bytes` (default
  8 GiB) and `spool.max_inflight` (default 64) bound the local staging
  area. A correlated cold-access burst that exceeds these returns `503
  Slow Down` to clients, which is the intended backpressure but visible
  as user-facing errors. Operators should monitor `origincache_spool_bytes`
  and `origincache_spool_evictions_total{reason="full"}` and tune the caps
  per node disk capacity.
- **Internal cert rotation**: the internal listener uses per-replica certs
  chained to an internal CA. Rotation is delegated to the issuing system
  (e.g. cert-manager). The server hot-reloads `cluster.internal_tls.cert_file`
  / `key_file` on file change (inotify / periodic stat); the CA bundle is
  reloaded the same way. CA rotation requires both old and new CAs to
  appear in the bundle for at least one full rolling-restart window;
  document this in `operations.md`. Misconfiguration risk: dropping the
  old CA too early breaks inter-replica RPCs cluster-wide.
- **Cluster membership during rolling restart**: rendezvous hashing
  tolerates membership flux, but a pod restart with a new IP looks like a
  new member for up to one refresh interval (default 5s), shifting
  ownership for ~1/N keys until the next DNS refresh. Back-to-back
  restarts can cause repeated duplicate fills. The
  `origincache_origin_duplicate_fills_total{result="commit_lost"}` metric
  makes this visible. We accept this in v1 and revisit if it proves
  material. See
  [design.md#14-horizontal-scale](./design.md#14-horizontal-scale).
- **Create-after-404 unavailability window**: clients that hit a missing
  key before the operator uploads it will continue to see `404` for up
  to `negative_metadata_ttl` per replica that observed the original
  `404` (default 60s). Worst case across replicas: round-robin LB can
  alternate `404` / `200` during the drain. There is no event-driven
  invalidation or admin-invalidation in v1 (the immutable-origin
  contract makes them unnecessary).
  Mitigations: short default `negative_metadata_ttl=60s`,
  `metadata_negative_*` metrics expose drain progress, runbook
  instructs operators to wait `negative_metadata_ttl` after uploading
  a previously-missing key before announcing it. See
  [design.md#12-create-after-404-and-negative-cache-lifecycle](./design.md#12-create-after-404-and-negative-cache-lifecycle).
- **ChunkCatalog undersizing degrades active eviction quality**:
  the optional active eviction loop (s13.2) bases decisions on
  per-entry access counters in the ChunkCatalog. If
  `chunk_catalog.max_entries` is much smaller than the working set,
  many chunks live in the CacheStore but are not tracked; they
  cannot be considered for active eviction; they live indefinitely
  until external lifecycle (if any) cleans them up. Operators MUST
  size the catalog to roughly 1.2x the estimated working-set chunk
  count
  ([design.md s13.3](./design.md#133-chunkcatalog-size-awareness-load-bearing-operational-note));
  metrics `chunk_catalog_hit_rate` and
  `chunk_catalog_evict_total{reason="size"}` make undersizing
  visible.
- **LIST cache staleness in write-and-immediately-list workloads**:
  the per-replica LIST cache (s6.2) defaults to 60s TTL. A key
  uploaded mid-window will not appear in `Origin.List` results
  served from cache until the entry expires (up to 60s).
  Acceptable for the documented FUSE-`ls` read-mostly workload;
  operators with write-and-immediately-list patterns should tune
  `list_cache.ttl` shorter or disable the cache via
  `list_cache.enabled: false`.
- **Mid-stream client aborts on post-commit origin failure**:
  the v1 streaming design (Option D) sends response headers and
  begins streaming as soon as origin returns a first byte. If the
  origin connection breaks mid-chunk after the cache has committed,
  the response aborts (HTTP/2 `RST_STREAM` or HTTP/1.1
  `Connection: close`). S3 SDKs handle this via `Content-Length`
  mismatch retry; the operational impact is small for the
  documented workload but visible in
  `responses_aborted_total{phase="mid_stream"}`. Sustained non-
  zero rates indicate origin tail-latency issues; the trigger for
  considering mid-stream origin resume
  ([design.md s15.4](./design.md#154-mid-stream-origin-resume))
  is sustained mid-stream abort rate measurably impacting
  end-to-end client latency.
- **Cold-start Stat storm**: a freshly started replica receiving a wide
  fan-out of distinct cold keys does one `CacheStore.Stat` per `ChunkKey`.
  At in-DC latencies this is cheap but not free. If a deployment routinely
  sees wide-fan-out cold starts we may add a bulk-stat path or warm the
  `ChunkCatalog` from a CacheStore listing on startup. Defer until
  measured.
- **CacheStore lifecycle eviction of hot chunks**: age-based expiration may
  evict a chunk that is still hot, forcing a re-fetch from origin.
  Operators should tune TTL against `origincache_origin_bytes_total`. Phase
  4 may add an in-`chunkcatalog` access-tracking layer if this proves
  material.
- **Origin egress cost spikes**: cold-start fan-out can be expensive even
  with singleflight if many distinct keys are touched simultaneously.
  Origin semaphore + 503 backpressure protects us, but operators should
  monitor `origincache_origin_bytes_total` and set DC-side egress budgets.
- **Prefetch-induced waste**: sequential read-ahead can fetch chunks the
  client never reads. Default depth (4) is conservative; we expose the knob
  and the metric.
- **Mid-stream abort detection by clients**: post-first-byte failures abort
  the response; standard S3 SDKs (aws-sdk, boto3) detect via
  `Content-Length` mismatch and retry. Non-standard or hand-rolled HTTP
  clients may silently truncate. Document this in `operations.md`.

## 11. Approval checklist

Before starting Phase 0 implementation, please confirm:

- [ ] Repo layout under `cmd/origincache/`, `internal/origincache/`,
      `deploy/origincache/`, `images/origincache/`,
      `designdocs/origincache/`, `hack/origincache/` is acceptable,
      including `internal/origincache/fetch/spool/`,
      `cmd/origincache/origincache/server/internal/`, and
      `deploy/origincache/07-networkpolicy.yaml.tmpl`.
- [ ] Default chunk size of 8 MiB is acceptable.
- [ ] Bearer / mTLS auth on the client edge in v1 is acceptable; SigV4
      is deferred future work.
- [ ] **Separate internal mTLS listener (`:8444`) with an internal CA
      distinct from the client mTLS CA, peer-IP-set authorization,
      and a NetworkPolicy restricting ingress to `app=origincache` pods,
      is acceptable.**
- [ ] Azure constraint to Block Blobs only, surfaced as
      `502 OriginUnsupported`, is acceptable.
- [ ] No persistent local index in v1; in-memory `ChunkCatalog` +
      `CacheStore.Stat` on miss is sufficient.
- [ ] CacheStore lifecycle / TTL is the eviction mechanism in v1; cache
      layer ships no eviction code.
- [ ] **Strict `If-Match: <etag>` on every `Origin.GetRange` (no opt-out),
      with `412` translated to `OriginETagChangedError`, metadata cache
      invalidation, and a non-retryable fill failure, is acceptable.**
- [ ] **Local Spool layer (default 8 GiB) as the universal slow-joiner
      fallback, with `503 Slow Down` on exhaustion, is acceptable.**
- [ ] **Atomic-commit model is acceptable: `localfs` uses
      `link()` / `renameat2(RENAME_NOREPLACE)` (no plain `rename()`);
      `cachestore/s3` uses `PutObject` + `If-None-Match: *` with no
      tmp key and no copy hop; `SelfTestAtomicCommit` at startup refuses
      to start if the backend doesn't honor the precondition.**
- [ ] **Deferred response headers until first chunk in hand, plus
      mid-stream abort (HTTP/2 `RST_STREAM` / HTTP/1.1 `Connection: close`)
      on post-first-byte failure, is acceptable.**
- [ ] **Assembler-per-request + per-chunk coordinator routing via
      internal fill RPC (rather than whole-request reverse-proxy) is the
      right v1 mechanism for strongly correlated cold-access workloads.**
- [ ] Deployment (not StatefulSet) is acceptable for v1 given no per-pod
      state, faster rolling updates, and parity with other stateless
      components in this repo.
- [ ] Phase 0 deliverable definition (one process serving a Range GET
      against real S3 and re-serving from `localfs`) is the right starting
      milestone.
- [ ] No cross-cmd imports; shared code lives under `internal/origincache/`
      per the project's coding standards.
- [ ] **Bounded staleness contract published in design.md s11 with
      `metadata_ttl=5m` default; operators are expected to honor the
      immutable-origin contract.**
- [ ] **Pre-header origin retry (Option D) ships in Phase 1: the
      leader retries `Origin.GetRange` up to
      `origin.retry.attempts` (default 3) with exponential backoff
      capped by `origin.retry.max_total_duration` (default 5s)
      BEFORE response headers are sent to the client; transparent
      to the client. The commit boundary is the first byte arrival
      from origin: post-commit, bytes stream origin -> client
      directly; spool tees in parallel for joiner support and as
      the asynchronous CacheStore-commit source. Pre-commit
      failures (retry budget exhausted, `OriginETagChangedError`)
      return clean HTTP errors; post-commit failures become
      mid-stream client aborts (handled by SDK retry).
      `origin_retry_total` and `origin_retry_attempts` metrics
      exposed; T-pre-header-retry-* test group in Phase 1.
      Mid-stream origin resume is deferred future work
      ([design.md s15.4](./design.md#154-mid-stream-origin-resume)).
      CacheStore commit runs asynchronously after the client
      response completes; commit-after-serve failures are reported
      as `commit_after_serve_total{result="failed"}` and do NOT
      affect client responses.**
- [ ] **`CacheStore` returns typed errors `ErrNotFound|ErrTransient|ErrAuth`;
      only `ErrNotFound` triggers refill; `ErrTransient` -> `503 Slow Down`
      with `Retry-After`; `ErrAuth` -> `502 Bad Gateway`.**
- [ ] **Per-process CacheStore circuit breaker with defaults
      `error_window=30s, error_threshold=10, open_duration=30s,
      half_open_probes=3`; state and transitions exported as metrics.**
- [ ] **Origin backpressure is per-replica static cap:
      `target_per_replica = floor(origin.target_global /
      cluster.target_replicas)` (default 64 slots/replica at
      `target_global=192`, `target_replicas=3`); origin throttling
      responses (`503 SlowDown` / `429`) are handled by the
      pre-header retry loop (`origin.retry.*`); `origin_inflight`
      gauge exposes per-replica saturation. Coordinated
      cluster-wide limiter and dynamic per-replica recompute are
      deferred future work, see
      [design.md s15.5](./design.md#155-coordinated-cluster-wide-origin-limiter)
      and
      [design.md s15.6](./design.md#156-dynamic-per-replica-origin-cap).
      Operators MUST update `cluster.target_replicas` after any
      sustained scale change.**
- [ ] **`cachestore/localfs` stages inside `<root>/.staging/<uuid>` (NOT
      `/tmp` and NOT spool dir); parent-dir fsync after every link/unlink;
      `staging_max_age=1h` orphaned-staging sweeper.**
- [ ] **Internal mTLS dialer pins `tls.Config.ServerName` to the stable
      SAN `origincache.<ns>.svc`; per-replica certs MUST include this
      SAN; pod-IP SANs are NOT used.**
- [ ] **`/readyz` flips to NotReady after `readyz.errauth_consecutive_threshold=3`
      consecutive `ErrAuth` from CacheStore; one non-`ErrAuth` success
      resets the counter.**
- [ ] **`server.max_response_bytes` overflow returns
      `400 RequestSizeExceedsLimit` (S3-style XML body); `416` is
      reserved for true Range vs. object-size violations.**
- [ ] **`cachestore/posixfs` ships in Phase 2 alongside `cachestore/s3`,
      sharing `link()`/`EEXIST` + dir-fsync helpers with
      `cachestore/localfs` via
      `internal/origincache/cachestore/internal/posixcommon/`. Supported
      backends: NFSv4.1+ (baseline), Weka native (`-t wekafs`), CephFS,
      Lustre, GPFS / IBM Spectrum Scale.**
- [ ] **`cachestore/posixfs` runs `SelfTestAtomicCommit` at startup
      (link()/`EEXIST` + dir-fsync + size verify); refuses to start on
      any failure. Never disabled in production
      (`require_atomic_link_self_test: true`).**
- [ ] **NFS minimum version is `4.1`
      (`cachestore.posixfs.nfs.minimum_version: "4.1"`); NFSv3 is opt-in
      only (`cachestore.posixfs.nfs.allow_v3: true`) with a loud WARN
      log and `posixfs_nfs_v3_optin_total++`; `allow_v3` MUST stay
      `false` in production manifests.**
- [ ] **Backend auto-detection via `statfs(2)` `f_type` + `/proc/mounts`
      emits `posixfs_backend{type,version}` info gauge; operator
      override allowed via `cachestore.posixfs.backend_type` for
      ambiguous magic numbers; override is logged loudly.**
- [ ] **Alluxio FUSE is unsupported: `cachestore/posixfs` detects it
      (FUSE_SUPER_MAGIC + `/proc/mounts` source matches `alluxio`) and
      refuses to start with a message pointing operators to
      `cachestore.driver: s3` against the Alluxio S3 gateway;
      `posixfs_alluxio_refusal_total` exposes accidental
      misconfigurations.**
- [ ] **`cachestore/posixfs` paths use a 2-character hex fan-out under
      `<root>/<origin_id>/<hash[0:2]>/<hash>/<chunk_index>` by default
      (`fanout_chars: 2`); `cachestore/localfs` keeps the flat layout
      (`fanout_chars: 0` default) but the helper is shared.**
- [ ] **NFS export hardening is operator-runbook material: exports MUST
      be `sync` (not `async`); the driver issues a best-effort warning
      from `/proc/mounts` client-side options but does not refuse on
      `async` (it cannot reliably detect server-side `async`); document
      this in `operations.md`.**
- [ ] **Spool locality is enforced at boot: `spool.require_local_fs:
      true` (default) runs `statfs(2)` on `spool.dir` and refuses to
      start when the FS magic matches NFS / SMB / CephFS / Lustre /
      GPFS / FUSE. With Option D the spool is no longer on the
      client TTFB path, so the contract is defense-in-depth for
      joiner-fallback latency; operators with unusual placements
      (e.g., RAM-disk) MAY relax via `spool.require_local_fs: false`
      with the documented operational warning. Production deploys
      are expected to keep the default. See
      [design.md#104-spool-locality-contract](./design.md#104-spool-locality-contract).**
- [ ] **Negative-cache TTL is independent: `negative_metadata_ttl: 60s`
      (default) is distinct from `metadata_ttl: 5m`; bounds the
      create-after-404 unavailability window. The
      `metadata_negative_entries` / `metadata_negative_hit_total` /
      `metadata_negative_age_seconds` metrics are exposed; the
      `T-create-after-404a/b/c` test group is in Phase 1.
      Event-driven invalidation and admin-invalidation RPC are
      out of v1 scope (the immutable-origin contract makes them
      unnecessary). See
      [design.md#12-create-after-404-and-negative-cache-lifecycle](./design.md#12-create-after-404-and-negative-cache-lifecycle).**
- [ ] **Per-replica LIST cache (FW3) ships in Phase 1 sized for
      the FUSE-`ls` workload pattern: default `list_cache.ttl=60s`,
      `max_entries=1024`, `max_response_bytes=1MiB`, no negative
      caching, optional stale-while-revalidate (`swr_enabled: false`
      default); `list_cache_*` metrics exposed; T-list-cache-* test
      group in Phase 1; cluster-wide LIST coordinator is a
      deferred optimization
      ([design.md s15.3](./design.md#153-cluster-wide-list-coordinator)).**
- [ ] **ChunkCatalog access-frequency tracking (FW8) added in
      Phase 1: per-entry `AccessCount`, `LastAccessed`,
      `LastEntered`. Optional active eviction loop opt-in via
      `chunk_catalog.active_eviction.enabled` (default `false`)
      with `inactive_threshold=24h`, `access_threshold=5`,
      `min_age=5m`, `max_evictions_per_run=1000`. New
      `CacheStore.Delete` method on the interface;
      `cachestore_delete_total` and `chunk_catalog_*` metrics
      exposed. Operators MUST size `chunk_catalog.max_entries` to
      ~1.2x estimated working-set chunks per the load-bearing
      operational note in
      [design.md s13.3](./design.md#133-chunkcatalog-size-awareness-load-bearing-operational-note).
      `T-active-eviction-*` and `T-catalog-*` test groups in Phase 1.**
- [ ] **Bounded-freshness mode (FW5) opt-in via
      `metadata_refresh.enabled` (default `false`) with hot-key
      detection via metadata-cache access counters (parallel to
      ChunkCatalog tracking from FW8). Defaults: `interval=1m`,
      `refresh_ahead_ratio=0.7`, `access_threshold=5`,
      `min_age=metadata_ttl/4=75s`, `max_refreshes_per_run=100`,
      `refresh_concurrency=8`. Negative entries are NOT refreshed.
      `metadata_refresh_*` metrics exposed; `T-metadata-refresh-*`
      test group in Phase 1. See
      [design.md s11.2](./design.md#112-bounded-freshness-mode-optional).**
- [ ] **`cachestore/s3` versioning gate enforced at boot: drives
      `GetBucketVersioning` and refuses to start on `Status: Enabled`
      or `Status: Suspended`. Governed by
      `cachestore.s3.require_unversioned_bucket: true` (default;
      never disabled in production). Required because
      `If-None-Match: *` is not honored on versioned buckets across
      all S3-compatible backends (notably VAST). Metric
      `s3_versioning_check_total{result="ok|refused"}` emitted once
      per boot. `T-s3-versioned-bucket-refusal` and
      `T-s3-unversioned-bucket-ok` tests in Phase 1. See
      [design.md s10.1.3](./design.md#1013-cachestores3) and the
      VAST KB citation therein.**
- [ ] **Edge rate limiting documented as v1 gap in
      [design.md s15.1](./design.md#151-edge-rate-limiting). Multi-
      tenant deployments worried about single-client monopolization
      should layer rate limiting at an upstream proxy or LB until
      this lands as a future deliverable.**
