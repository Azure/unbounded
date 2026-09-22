# RACER dataplane

Linux Rust cache dataplane. The crate provides the `racer-dataplane` daemon
and the `http-bench` and `crypto-bench` binaries.

## Build

Run from `cmd/racer-dataplane/` in this repository. Install a current Rust
toolchain supporting edition 2024, a C compiler, `ar`, and the libibverbs
development headers/library (Ubuntu: `build-essential libibverbs-dev`). Native
RDMA code is compiled and linked even when runtime RDMA is disabled.

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

Provision its environment, configuration, and signing bundles first:

| Setting | Purpose |
| --- | --- |
| `RACER_CONTROL_PLANE_URL` | HTTP subscription URL or watched local ProtoJSON configuration file |
| `RACER_UNIVERSE`, `RACER_NODE` | Bootstrap identities, each a 32-byte hexadecimal value |
| `RACER_PEER_KEYS_DIR` | Required projected peer signing/verification `bundle.json` directory |
| `RACER_CONFIG_KEYS_DIR` | Configuration verification bundle directory, required for HTTP control |
| `RACER_CONTROL_TOKEN_FILE` | Optional HTTP control bearer-token file |
| `RACER_SLAB_PATH`, `RACER_SLAB_SIZE` | Cache file path (default `cache.slab`) and new slab size (default 10 GiB) |
| `RACER_SHARDS` | Positive shard count, default 32 |
| `RACER_IO_WORKERS`, `RACER_COMPUTE_WORKERS` | Optional positive worker counts per NUMA node |
| `RACER_BUFFERS_PER_NODE` | Transient 4 MiB buffer count per NUMA node, minimum 4, default 32 |
| `RACER_METRICS_ADDR` | Numeric management socket address; otherwise `RACER_POD_IP:9090`, falling back to `0.0.0.0:9090` |
| `RACER_RDMA_MODE` | `disabled` by default; `enabled` also requires `RACER_RDMA_RAILS` selectors |

Environment parsing and defaults are in [`src/main.rs`](src/main.rs); bootstrap,
subscription, and reload validation are in [`src/control.rs`](src/control.rs).
Remote configurations require signatures and must match the bootstrap identities.
Local files use the same configuration validation boundary but may contain an
unsigned snapshot. Signing bundle loading and rotation checks are in
[`src/signing.rs`](src/signing.rs).

The daemon requires at least four buffers per NUMA node: canonical routes can
have three peer hops, requiring three downstream progress slots plus one receive
slot. The minimum applies before topology subscription because later
configurations may add hops. The managed `http-small-v1` profile
configures eight buffers per NUMA node.

Runtime needs Linux io_uring, allowed physical cores, NUMA binding/prefaulting,
and enough locked-memory allowance for registered buffers. Use an ext4 slab
filesystem with 4 KiB base pages. The daemon initializes worker placement,
storage, buffer pools, and io_uring during startup; setup failures stop startup.

Existing slabs retain their layout. Keep the shard count and actual total I/O
worker count fixed across restarts; affinity, NUMA topology, and automatic worker
selection affect that count. Incompatible formats and placement are rejected.
For a layout change, stop the daemon, preserve the old slab, and select a fresh
`RACER_SLAB_PATH` to refill from origin. There is no automatic slab migration or
reformatting. Current storage uses `RACERS04`/`RACERN04` inline metadata.

Management serves `/metrics`, `/readyz`, `/livez`, and `/startupz`.
Readiness requires an activated configuration and healthy workers. A signed
`idle` snapshot explicitly permits readiness without volume listeners for an
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

`crypto-bench` measures NUMA checksum admission and Ed25519 authentication.
Bulk timing includes acquisition, fill, queueing, completion, and publication.
Worker counts are per NUMA node; select enough physical cores for both pools:

```sh
timeout --signal=KILL 180s cargo run --release --locked --bin crypto-bench -- \
  --io-workers 1 --compute-workers 1,2,4 --warmup 2 --duration 5
```

Use `--bulk-only` to omit signing and verification measurements.
