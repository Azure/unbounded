# Dataplane tests

Run commands from `cmd/racer-dataplane/` in this repository. See
[README.md](README.md#build) for native build dependencies. Set `TMPDIR` to an
existing ext4 scratch directory inside the workspace for storage fixtures.

## Test tree and module ownership

All unit-test bodies and standalone fixtures live in `tests/`. Cargo's
`autotests = false` is intentional: these are unit tests attached to their
owning library or binary modules with `#[path]` or test-only `include!`, so
private access and fully qualified test names (including subprocess `--exact`
selectors) are preserved. Production instrumentation and compile-fail API
documentation remain beside the implementation in `src/`.

| Directory | Coverage and shared fixtures |
| --- | --- |
| `tests/storage/` | Allocator geometry/pressure, checkpoint recovery, buffer ownership, cache flights and persistence, peer/HTTP metadata |
| `tests/execution/` | Worker placement, pools, sharding, lifecycle and io_uring ownership |
| `tests/http/` | Client framing/reuse, server streaming/files, deadlines, scheduling/pressure, handlers, breaker and owner health |
| `tests/security/` | Checksum ownership, TLS identities/trust/record I/O, negotiation and peer authorization |
| `tests/control/` | Configuration activation/subscription, routing and topology proofs |
| `tests/runtime/` | Real-socket cluster, cross-node scenarios, activation and listeners |
| `tests/rdma/` | Transport policy, native C lifecycle and native-device cases |
| `tests/bin/` | Dataplane startup/process lifecycle and build identity checks |
| `tests/contracts.rs`, `tests/metrics.rs` | Cross-subsystem conformance/endpoint contracts and metrics |

Substantial suites target roughly 2,000 lines, splitting at complete fixture or
scenario boundaries. Smaller suites share files when they have the same owning
scope; private nested-module and binary boundaries justify smaller files.
Keep helpers with their coverage: `runtime::tests::Cluster` owns real-socket
cluster setup, `control::tests` owns configuration fixtures, and `conformance`
owns shared kernel/origin fixtures.
Avoid widening production visibility solely to relocate tests.

Add new tests to the appropriate file in `tests/`, retaining the owning module
attachment. `cargo fmt` does not discover every `include!` file; format/check the
test tree explicitly as well:

```sh
cargo fmt --check
find tests -name '*.rs' -print0 | xargs -0 rustfmt --edition 2024 --check
cargo test --locked --all-targets --no-run
cargo test --locked --all-targets -- --list
```

When moving suites, compare the full per-target test and ignored-test inventories
before and after. Preserve subprocess selectors: a libtest `--exact` selector
that matches nothing exits successfully without exercising the child test.

## Focused checks

```sh
cargo check --locked --all-targets
cargo fmt --check
cargo test --locked --lib buffers::
cargo test --locked --lib workers::pool_tests::
cargo test --locked --lib workers::reentrant_tests::
cargo test --locked --lib cache::
cargo test --locked --lib allocator:: -- --test-threads=1
cargo test --locked --lib sharding::
cargo test --locked --lib uring::
cargo test --locked --lib slab_io
cargo test --locked --lib rdma::
cargo test --locked --lib crypto::
cargo test --locked --doc
```

`http_server::tests::tls_transport::encrypted_http_kernel_integration` also
checks limited TLS file responses and exact charged byte counts. Set
`RACER_REQUIRE_KTLS=1` on an eligible host to require actual kTLS coverage;
otherwise the encrypted software-TLS path remains covered.

Build identity/CLI tests run for the daemon without kernel or runtime
configuration prerequisites:

```sh
timeout --signal=KILL 90s cargo test --locked --bin racer-dataplane version_tests::
```

These subprocess checks exercise version output with missing and invalid runtime
settings, runtime attempts to override embedded metadata, invalid CLI arguments,
and normal no-argument configuration validation. To check the actual executable
and Cargo cache invalidation, build the daemon with explicit `VERSION`,
`GIT_COMMIT`, and `BUILD_TIME`, invoke it with `--version` and `version`, then
change each build input in turn and rebuild in the same target directory. Unset
the inputs and rebuild to verify the `dev`/`unknown`/`unknown` defaults return.

Pool tests cover final-reference recycling, private same-value staging, authority
pairing, error cleanup, cross-thread completion, backpressure, and independent flight
bounds. Transport tests cover delayed CQEs, zero-copy notifications, cancellation
acks, RDMA window ownership, and quiescence. Cache tests cover sharing, surviving
consumer deadlines/takeover, and metadata resolution with all payload slots pinned.

## Kernel and storage coverage

The `slab_io` filter covers configuration rejection, rate overflow,
interruptible setup waits, syscall retries, and creation/recovery/replacement
accounting. The
`uring::tests::kernel_integration` subprocess additionally exercises limited real
file reads/writes, timed sync admission, and cancellation before submission.
Use `RACER_REQUIRE_URING=1` with an external timeout to require kernel coverage.

`allocator::layout::tests` covers automatic planning boundaries, 2 TiB/4 TiB
sparse files with every allocator opened, exact bitmap backing accounting, and
a populated 16 GiB shard index/checkpoint without full payload storage. The
reported structural bytes are not RSS or full-device performance coverage.
`populated_two_tib_memory` additionally runs an isolated subprocess with every
2 TiB payload descriptor and admitted metadata entry populated, alternating
sorted/hashed keys and retaining three tree versions. It reports deduplicated
structural bytes and Linux RSS high-water growth, without writing payload data.
The managed-envelope test checks incremental empty preparation and checkpoint
drain headroom across automatic shard-count boundaries.
`sharding::tests` covers one-shard growth/shrink, generation/worker/plan/pool
authority, ordering and geometry.

`runtime::storage::tests` exercises the shipping setup thread and worker polling
with real ext4/io_uring: multiworker grow/shrink across shard counts, same-size
no-op, live HTTP drain/503/refill, preparation/ENOSPC/staging/drain failures,
retry/supersession, delayed worker acknowledgments, post-rename sync failure,
retained old-inode reads, and shutdown fencing. Abrupt-exit subprocesses cover
prepared, renamed, and directory-synced restart boundaries. These test process
crashes, not power-cut filesystem behavior. Native RDMA resize and full-device
multi-TiB load remain separate hardware validation.

`runtime::storage::memory_tests` covers clean file-cache credit, unreclaimable
exhaustion, malformed counters and finite ancestor limits using workspace
fixtures. Its ignored `real_buffered_file_cache_is_credited_after_sync` test
writes and syncs a 256 MiB workspace file and measures current-cgroup clean cache
growth. Run it explicitly with `--ignored --exact --nocapture` on a quiet cgroup;
it does not change cgroup limits and is not bounded-cgroup pressure coverage.
The daemon startup and sharding suites also cover a recorded 64 GiB/2,048-shard
layout with one worker, unchanged inode, and complete generation authorization,
while new automatic planning retains its ceilings.

`cmd/racer-controlplane/TestStorageRuntimeTLSResizeRestart` complements these
fault fixtures with the actual daemon executable and Go mTLS subscription server.
It applies 64MiB -> 20GiB -> 96MiB, verifies fresh inodes and shard changes without
changing the boot or topology, waits for `/status` and the controller's Node
annotation to agree, rejects a 5TiB runtime request without losing the old cache,
and checks invalid/equivalent input. It restarts controller and daemon, first with
control unavailable and an invalid creation-size environment, then requires fresh
policy acknowledgment with the same durable policy and published inode. Kubernetes
is fake and certificates are fixture-issued; filesystem, io_uring, TLS and the
process are real. This test does not exercise production enrollment or CA rotation
(`../racer-controlplane/storage_runtime_test.go:31-110`). The separate envtest suites
exercise Kubernetes validation and CAS.

From the repository root on a capable Linux host:

```sh
export TMPDIR="$PWD/tmp" # existing workspace ext4 directory
export RACER_REQUIRE_URING=1 RUST_TEST_THREADS=2 GOTOOLCHAIN=go1.26.6
# Set KUBEBUILDER_ASSETS to existing kube-apiserver/etcd assets for envtest.
timeout --signal=KILL 1800s make racer-fmt-check racer-test
timeout --signal=KILL 1800s make racer-crosslang-test
```

If a full-suite deadline expires, identify the unfinished cases and run bounded
groups separately; a timeout is not passing coverage. Cross-language tests need
enough physical cores and locked memory for the real daemon. Native RDMA and
full-device payload validation remain distinct from sparse/index coverage.

## Full verification

Run the full library under an external bound:

```sh
timeout --signal=KILL 180s env RACER_REQUIRE_URING=1 RUST_TEST_THREADS=2 cargo test --locked --lib
timeout --signal=KILL 90s env RACER_REQUIRE_URING=1 RUST_TEST_THREADS=2 cargo test --locked --bins
RUST_TEST_THREADS=2 cargo test --locked --doc
```

The shared Go protocol helpers live in `internal/racer`, and the authoritative
schema and Go bindings live in `api/racer` (package `racerconfig`). Cross-language
fixtures marked ignored require their named Go-produced export.
An unavailable environment must be reported separately from passing tests.
[README.md](README.md#benchmarks) documents the HTTP transport and checksum
benchmarks. Native RDMA and Soft-RoCE tests remain explicit opt-in checks.
