# RACER dataplane

Linux Rust cache dataplane. The crate provides the `racer-dataplane` daemon
and the `http-bench` and `crypto-bench` binaries.

## Build

Run from `cmd/racer-dataplane/` in this repository. Install a current Rust
toolchain supporting edition 2024, a C compiler, `ar`, and the libibverbs
development headers/library and OpenSSL 3 headers (Ubuntu:
`build-essential libibverbs-dev libssl-dev`). Native RDMA and TLS code is
compiled and linked even when runtime RDMA is disabled. Runtime requires the
OpenSSL 3 shared libraries.

```sh
cargo build --locked --all-targets
cargo build --release --locked --bins
cargo fmt --check
cargo test --locked --all-targets --no-run
```

`build.rs` generates Prost and ProtoJSON bindings from the authoritative
[`api/racer/control.proto`](../../api/racer/control.proto). Protoc is supplied by
the locked `protoc-bin-vendored` dependency. Generated bindings stay in Cargo's
build output. The Go bindings are package `racerconfig` in `api/racer`; shared
Go protocol helpers live in `internal/racer`.

### Build identity

The daemon supports `--version` (also `-version`) and the `version`
subcommand. It prints the same format as Go's `internal/version` and exits before
signal/lifecycle setup, runtime configuration, kernel checks, or slab access:

```sh
./target/release/racer-dataplane --version
./target/release/racer-dataplane version
# dev (commit: unknown, built: unknown)
```

Direct Cargo builds read `VERSION`, `GIT_COMMIT`, and `BUILD_TIME` from the build
environment. Unset or empty values default to `dev`, `unknown`, and `unknown`,
respectively; Cargo's package version is not the release identity. Changes to any
of these inputs invalidate Cargo's cached build. Runtime environment variables
cannot change the embedded identity.

```sh
VERSION=v1.2.3 GIT_COMMIT=abc1234 BUILD_TIME=2026-09-22T00:00:00Z \
  cargo build --release --locked --bin racer-dataplane
```

From the repository root, `make racer-dataplane-build` stamps the daemon using
the same `VERSION`, `GIT_COMMIT`, and UTC `BUILD_TIME` variables as Go builds;
each can be overridden on the Make command line. The dataplane Containerfile
accepts the same three build arguments in its builder stage. Its default build
time is the current UTC time, and its version and commit defaults match the OCI
labels (`dev` and `unknown`). Supply `--build-arg BUILD_TIME=...` for a fixed image
build timestamp.

## Running

The daemon starts directly, without a `serve` subcommand:

```sh
./target/release/racer-dataplane
```

Provision its environment, configuration, and projected TLS trust bundle first:

| Setting | Purpose |
| --- | --- |
| `RACER_CONTROL_PLANE_URL` | HTTPS mTLS subscription URL (`https://racer-controlplane.<namespace>.svc:8443/v3/<universe>/<node>`) or watched local ProtoJSON configuration file |
| `RACER_UNIVERSE`, `RACER_NODE` | Bootstrap identities, each a 32-byte hexadecimal value |
| `RACER_TLS_TRUST_DIR` | Projected `racer-trust` ConfigMap directory containing `bundle.json`, normally `/var/run/racer-trust` |
| `RACER_ENROLL_URL` | HTTPS certificate enrollment URL (`https://racer-controlplane.<namespace>.svc:8444/v3/enroll`) |
| `RACER_CONTROL_SERVER_NAME` | Expected control-plane DNS SAN, `racer-controlplane.<namespace>.svc` |
| `RACER_CONTROL_TOKEN_FILE` | Enrollment bearer-token file with audience `racer-control` |
| `RACER_POD_NAMESPACE`, `RACER_POD_NAME`, `RACER_POD_UID` | Kubernetes Pod identity used for enrollment and leaf URI validation |
| `RACER_SLAB_PATH`, `RACER_SLAB_SIZE` | Cache file path (default `cache.slab`) and new slab size (default 10 GiB) |
| `RACER_SHARDS` | Optional explicit legacy slab shard count; otherwise storage layout is automatic. The execution planner retains its default 32-worker cap. |
| `RACER_IO_WORKERS`, `RACER_COMPUTE_WORKERS` | Optional positive worker counts per NUMA node |
| `RACER_BUFFERS_PER_NODE` | Transient 4 MiB buffer count per NUMA node, minimum 4, default 32 |
| `RACER_METRICS_ADDR` | Numeric management socket address; otherwise `RACER_POD_IP:9090`, falling back to `0.0.0.0:9090` |
| `RACER_RDMA_MODE` | `disabled` by default; `enabled` also requires `RACER_RDMA_RAILS` selectors |

Environment parsing and defaults are in [`src/main.rs`](src/main.rs); bootstrap,
subscription, and reload validation are in [`src/control.rs`](src/control.rs).
Configurations must match the bootstrap identities. Remote commands are raw
protobuf over authenticated TLS; local files use the same snapshot validation
boundary. Application-level detached signatures and signing-key bundles are no
longer part of the protocol.

The daemon requires at least four buffers per NUMA node: canonical routes can
have three peer hops, requiring three downstream progress slots plus one receive
slot. The minimum applies before topology subscription because later
configurations may add hops. The managed `http-small-v1` profile
configures eight buffers per NUMA node.

Runtime needs Linux io_uring, allowed physical cores, NUMA binding/prefaulting,
and enough locked-memory allowance for registered buffers. Use an ext4 slab
filesystem with 4 KiB base pages. The daemon initializes worker placement,
storage, buffer pools, and io_uring during startup; setup failures stop startup.

Existing slabs retain their recorded layout. Startup reads the shard count from
the locked inode's placement xattr, including after a runtime resize.
`RACER_SLAB_SIZE` is read only when creating a missing slab; it cannot override a
persisted runtime-resized capacity, even before control becomes available.
Explicit `RACER_SHARDS` controls initial creation and the execution worker cap.
Keep the actual total I/O worker count fixed across restarts; affinity, NUMA
topology, and automatic worker selection affect that
count. Incompatible formats and placement are rejected.
Storage policies delivered over mTLS can resize the cache in the same process,
discarding all cached content. Incompatible legacy formats still require a fresh
slab path. Current storage uses `RACERS04`/`RACERN04` inline metadata.

New automatic layouts accept 32 MiB through 4 TiB in 4 MiB increments, with at
least 32 MiB per existing I/O worker. They use at least one shard per worker and
target at most 16 GiB per shard, below the format's approximately 62 GiB limit.
Thus a 2 TiB layout uses at least 128 shards and 4 TiB uses at least 256, without
adding workers or transient buffers. Internal object-metadata admission remains
bounded per shard; payload indexing scales with actual admitted extents.

Capacity is logical file length, including the approximately one-eighth index
reservation and layout overhead, not usable payload bytes or RAM. Kubernetes
inputs require whole bytes of at least 32 MiB and round up to 4 MiB; they allow
values above 4 TiB within signed file-offset bounds. Such a policy reaches
the runtime but fails automatic layout planning and retains the previous cache.
See the [operator guide](../../docs/content/guides/racer.md#set-cache-capacity)
for Site defaults, Node overrides, removing overrides, and status commands.

`allocator::LayoutPlan` exposes capacity, shard geometry, unused aligned tail,
and diagnostic structural resource estimates. The 4 TiB planner
ceiling is the tested sparse/bitmap envelope: 917,504 payload extents and up to
2,097,152 inline metadata entries with 256 target-sized shards. Tests initialize
all allocators at 2 TiB and 4 TiB and compare actual bitmap backing to accounting
(8,578,048 and 17,156,096 bytes). They also populate a target-sized shard's index
without payload data: on x86-64 its live structural footprint is 4,126,016 bytes
(excluding map control/slack), with 7,421,952 encoded checkpoint page bytes.
These are not full-device load tests or measured RSS. The resource API uses a
deliberately conservative sparse-tree bound, concrete Rust type sizes, retained
CoW versions and container slack; it excludes malloc overhead, external holders,
pools, transport state and kernel page cache. Its estimates are not allocations
or a user-configured memory budget. A larger envelope needs additional validation.

### Storage generation integration

Execution placement is immutable. Retain a thread-local `WorkerContext::clone`
in the runtime driver; clones share pool binding and initial assignment issuance.
`LayoutPlan::authorize` (or `Placement::storage_generation` for an existing
layout) creates a unique plan-bound `StorageGeneration`. Each worker calls
`take_assignments(&context)` once. Activate those capabilities with
`ShardState::activate` and build `Cache::for_generation` from the complete,
ascending local shard set. Foreign plans/workers, mixed generations, wrong
geometry, foreign pools and missing/reordered shards are rejected. A new cache
can use the existing ring, pool and transports; old faults/metadata belong to
their original cache and must be drained there.

Share one `CheckpointBudget` across active and replacement slabs, installing it
before issuing shards (`LayoutPlan::create` accepts it). At most two checkpoint
preparations remain in flight through final sync collection; deferred shards
remain runnable. `resources().replacement_peak_bytes(next)` describes the
diagnostic worst case for two populated generations, not resize admission. The
runtime transaction must enforce the
two-generation lifecycle, retire before preparing another replacement, and
preserve outstanding file/buffer ownership. These APIs are driven by
`runtime::StorageCoordinator` and worker-local `Volumes::with_storage`.

The process consumes `Updates::desired_storage` independently of topology and
reports pending, failed, or applied with the actual capacity through
`Updates::report_storage`. Same-capacity requests are no-ops. A single setup
thread validates automatic layout and incremental empty-candidate allocation
against Linux available memory and finite cgroup-v2 headroom, then creates and
syncs a fresh sparse inode. Cgroup headroom includes conservatively estimated
clean reclaimable file cache, excluding shmem, dirty/writeback and unevictable
pages, and is capped independently at every visible ancestor and by Linux
available memory. Admission includes old-generation concurrent
checkpoint scratch and an operational margin, not another charge for the
already-resident old cache or a hypothetically populated new cache. Startup
checks bitmap backing and per-worker recovery scratch. These checks are
operational headroom checks, not RSS guarantees or configurable memory budgets.
The populated-tree worst-case estimates remain diagnostic only. Filesystem
exhaustion can still reject fills.

The managed profile reserves 3 CPUs and 4 GiB memory, independent of capacity.
An isolated 2 TiB index fixture fills every payload descriptor and metadata slot,
retains three tree versions, and measures under 1 GiB RSS growth. Extrapolating
to 4 TiB allows 2 GiB for indexes, under 512 MiB incremental replacement/drain
headroom, and 1.5 GiB for pools, process/network state and allocator variation.
This is representative index coverage, not full-device payload/page-cache load
or a guarantee for every mutation history. No additional workers or buffer pools
are provisioned when capacity grows.

Workers stage empty allocators incrementally on their existing pools and rings.
Resumable maintenance returns Busy/503 for new HTTP/RDMA cache work while
admitted requests keep their original deadline semantics. All-worker drain
includes cache faults, streaming metadata owners, shared-NUMA consumers and
allocator reads/writes. Completion sources and worker heartbeats continue.
Local topology activation pauses during the storage fence. This does not add a
universe-wide storage barrier or rebuild RDMA registrations.

The daemon holds `<slab>.lock` across replacement. Never delete this sidecar while
the process runs. `<slab>.resize` is the private candidate; startup removes an
interrupted candidate and opens the authoritative slab's recorded layout. Rename
followed by directory sync commits the replacement. Precommit failure resumes
the old cache; after rename, directory-sync failure stays fenced and retries,
and restart opens a complete old or new inode. Every worker must acknowledge
install before resume. Old file leases and kernel operations retain their inode;
retirement must finish before another candidate is prepared. Repeated failures
back off, and newer desired requests coalesce. SIGTERM stops further precommit
work and retains the normal supervised process exit deadline.

The containing directory must permit creating, deleting, renaming, and syncing
these files. Reserve physical disk headroom for old/candidate overlap and future
fills: sparse logical capacity is not disk reservation. Resize is a cache flush,
including on shrink, without a Pod rollout. Invalid Kubernetes input retains the
last-good desired policy; runtime rejection retains actual old capacity. An older
client without the storage capability keeps topology service and is reported as
`unsupported` by the controller.

Management serves `/metrics`, `/readyz`, `/livez`, and `/startupz`.
`/status` (also the `/readyz` response body) includes a separate `storage` object:
`policyIdentity`, `policyVersion`, `effectiveBytes` (last accepted policy or null),
`appliedBytes` (actual process-local capacity), `appliedVersion`, `shards`, `phase`
(`unmanaged`, `pending`, `applied`, `failed`), `error`, `validationError`,
`selectedPodUID`, `boot`, `controlAgeSeconds`, and `controlFresh` (under 15 seconds).
Startup geometry is observable before a policy arrives; it never acknowledges a
policy by itself. A storage failure retains the usable old cache's readiness and
does not overwrite the topology `lastError`. Kubernetes source/requested input
and invalid desired values live in the controller-owned Node `cache-status`
annotation; they are not part of the byte policy delivered over mTLS.

The fixed eight storage metric series use prefix
`racer_dataplane_cache_storage_`: `effective_bytes`, `applied_bytes`, `shards`,
`validation_error`, and one-hot `phase{phase="unmanaged|pending|applied|failed"}`.
All are gauges; no identities, versions, quantities, paths, or errors are labels.
Readiness requires an activated configuration and healthy workers. An explicit
`idle` snapshot permits readiness without volume listeners for an
eligible managed Site member, including before the first volume and after the
last volume is deleted. Empty removal snapshots do not grant idle readiness.
Adding a volume requires its listener to activate before readiness succeeds.
Metric definitions and aggregation are in [`src/metrics.rs`](src/metrics.rs).
Payload cache hits are `disk_hit`, including Linux page-cache hits;
`memory_hit` is reserved for inline metadata.

Payload storage replenishes reusable extents before the shard becomes full.
The per-shard target is the smaller of one quarter of payload capacity, 64
extents, and the pending-value limit; replenishment begins at half that target
after pending payload writes drain. A 10 GiB, one-shard default slab starts at
32 free 4 MiB extents and targets 64 free-or-retiring extents. This earlier
eviction trades up to 256 MiB of resident payload capacity for admission
headroom. One-extent targets retain reactive reclamation. Only checkpointed,
unpinned victims qualify; both durable roots and outstanding holders must
release an extent before reuse. Already-retiring extents count toward the
target, so slow holders do not cause repeated batch eviction. This does not
guarantee admission under arbitrary bursts, slow storage, or pinned capacity;
request deadlines and bounded Busy responses still apply.

`racer_dataplane_disk_cache_evictions_total` counts payload items evicted to make
room for new cache fills. Each item in a reclamation batch counts once when
removed, even if the triggering fill later fails or is canceled. Metadata
eviction, same-key replacement, invalidation, and corruption cleanup do not
contribute. Use `rate(racer_dataplane_disk_cache_evictions_total[5m])` to monitor
disk-cache churn in items per second.

## Peer TLS

Peer traffic uses a dedicated mutual-TLS listener on port 9443. Volume ingress
listeners and origin connections continue to use ordinary HTTP. Node certificate
URI SANs bind universe, node, and Pod UID; control-plane certificates bind both
`spiffe://racer/controlplane` and the configured DNS name. The node generates its
private key locally and enrolls using its projected ServiceAccount token.

The projected trust bundle is JSON with `version: 1`, a monotonically increasing
`generation`, `active` (the lowercase SHA-256 digest of the active root DER), and
`certificates` (concatenated PEM roots). Reloads retain the last valid state on
malformed input, rollback, or same-generation equivocation. Trust acknowledgments
wait for every worker to install the corresponding context. The fresh trust proof
endpoint is derived from the enrollment URL using port 8446 and `/v3/proof`;
`RACER_TRUST_PROOF_URL` overrides it.

OpenSSL handles TLS records through its native socket BIO. kTLS is automatic on
supported systems, with encrypted software TLS otherwise. The current rekey-safe
baseline is OpenSSL 3.5 and Linux 6.14. Older stacks use software TLS even if they
can offload an initial connection, because their TLS 1.3 KeyUpdate behavior may
leave kernel traffic keys stale. TX and RX offload are measured independently;
`SSL_sendfile` is used only when actual TX offload is active.

## Protocol and verification

Peer representations use RF05 checksum descriptors, RF03 canonical routing,
and mandatory RF04 budget framing. Origins supply a strong quoted 64-character
lowercase checksum ETag. Cryptographic domain strings are protocol constants
and are independent of Kubernetes metadata prefixes.

See [TESTING.md](TESTING.md) for deterministic campaigns, real-kernel checks,
ownership tests, and compile-fail doctests. `autotests = false` is intentional:
files under `tests/` are attached to the owning library/binary modules through `#[path]` and
test-only `include!`, preserving private access and subprocess test selectors.

## Benchmarks

Build both benchmarks from this directory:

```sh
cargo build --release --locked --bin http-bench --bin crypto-bench
./target/release/http-bench --help
```

Use a host permitting io_uring, NUMA binding/prefaulting, and locked-memory
registration. Select distinct physical cores; adjacent logical CPU IDs may be
SMT siblings. Bound process memory and runtime.

`http-bench` transfers 4 MiB bodies over persistent HTTP/1.1 using the production
transports. File mode publishes through a temporary, unlinked slab and serves
file-backed bodies; buffer mode uses immutable-buffer SEND_ZC. Payload validation
runs during warmup. In separate terminals, start the server and then the client:

```sh
export RACER_BENCH_DIR=/absolute/ext4/workspace/bench-results
test -d "$RACER_BENCH_DIR"
taskset -c 0-3 ./target/release/http-bench server \
  --listen 127.0.0.1:8080 --body file --slab-dir "$RACER_BENCH_DIR"
```

```sh
timeout --signal=KILL 60s taskset -c 4-7 ./target/release/http-bench client \
  --connect 127.0.0.1:8080 --connections-per-worker 8 --warmup 3 --duration 15
```

Substitute disjoint physical-core masks and an existing workspace ext4 directory
with sufficient space. Stop the server with SIGINT or SIGTERM after each trial;
repeat with `--body buffer` for comparison. Require a successful client exit and
a `RESULT` line. Record kernel, filesystem, CPU placement, resource limits,
connection count, warmup, throughput, complete-body latency, and errors. Measure
warm-cache transport separately from cold storage and origin fill.

To benchmark mutual TLS, add all four options to each process:
`--tls-trust-dir DIR --tls-cert LEAF_CHAIN_PEM --tls-key PRIVATE_KEY_PEM
--tls-peer EXPECTED_REMOTE_SPIFFE_URI`. Use distinct node identities and keys
for server and client. The trust directory contains the same `bundle.json`
format as production. Both file and buffer modes report actual TLS offload
counters; throughput alone does not establish whether kTLS was active.

`crypto-bench` measures NUMA checksum admission.
Bulk timing includes acquisition, fill, queueing, completion, and publication.
Worker counts are per NUMA node; select enough physical cores for both pools:

```sh
timeout --signal=KILL 180s cargo run --release --locked --bin crypto-bench -- \
  --io-workers 1 --compute-workers 1,2,4 --warmup 2 --duration 5
```

The legacy `--bulk-only` option remains accepted. Checksum execution is now the
only benchmark in this binary.
