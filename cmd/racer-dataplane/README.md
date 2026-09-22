# RACER dataplane

Linux Rust cache dataplane imported from `racer` revision
`c9bf09848a66df58d7cde6bb09bd5c6fcc61913c`. The crate provides the
`racer-dataplane` daemon, `racer-preflight` deployment checks, and the
`http-bench` and `crypto-bench` binaries.

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

The daemon rejects buffer counts below four at startup, before opening the slab
or starting services. This tightens the previous any-positive-count contract:
canonical routes can have three peer hops, requiring three downstream progress
slots plus one receive slot. Smaller pools would otherwise start successfully
but return Busy/503 for routes they can never admit, even when idle. The minimum
applies before topology subscription because later configurations may add hops.
The managed `http-small-v1` preflight profile still requires eight buffers per
NUMA node; the daemon minimum does not change that profile or the slab layout.

Runtime needs Linux io_uring, allowed physical cores, NUMA binding/prefaulting,
and enough locked-memory allowance for registered buffers. Use an ext4 slab
filesystem with 4 KiB base pages. `racer-preflight` checks the bounded
`http-small-v1` deployment profile, including cgroup, storage, CPU, memory, and
kernel requirements; its exact environment requirements are in
[`src/preflight.rs`](src/preflight.rs).

Existing slabs retain their layout. Keep the shard count and actual total I/O
worker count fixed across restarts; affinity, NUMA topology, and automatic worker
selection affect that count. Incompatible formats and placement are rejected.
For a layout change, stop the daemon, preserve the old slab, and select a fresh
`RACER_SLAB_PATH` to refill from origin. There is no automatic slab migration or
reformatting. Current storage uses `RACERS04`/`RACERN04` inline metadata.

Management serves `/metrics`, `/readyz`, `/livez`, and `/startupz`.
Metric definitions and aggregation are in [`src/metrics.rs`](src/metrics.rs).
Payload cache hits are `disk_hit`, including Linux page-cache hits;
`memory_hit` is reserved for inline metadata.

## Protocol and verification

Peer representations use RF05 checksum descriptors, RF03 canonical routing,
and mandatory RF04 budget framing. Origins supply a strong quoted 64-character
lowercase checksum ETag. Cryptographic domain strings are protocol constants
and are independent of Kubernetes metadata prefixes.

See [TESTING.md](TESTING.md) for deterministic campaigns, real-kernel checks,
ownership tests, and compile-fail doctests, and [bench/README.md](bench/README.md)
for benchmark commands. `autotests = false` is intentional: files under `tests/`
are attached to the owning library/binary modules through `#[path]` and
test-only `include!`, preserving private access and subprocess test selectors.
