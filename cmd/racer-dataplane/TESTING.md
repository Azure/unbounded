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
| `tests/security/` | Crypto ownership, handshake/negotiation, HTTP authentication/replay and signing rotation |
| `tests/control/` | Configuration activation/subscription, routing and topology proofs |
| `tests/runtime/` | Production-driver cluster, cross-node scenarios, activation and listeners |
| `tests/rdma/` | Transport policy, completion/renewal ownership and native-device cases |
| `tests/support/` | Deterministic I/O simulator, simulator contracts, workload corpus and independent oracles |
| `tests/bin/` | Dataplane startup/process lifecycle and build identity checks |
| `tests/contracts.rs`, `tests/metrics.rs` | Cross-subsystem conformance/endpoint contracts and metrics |

Substantial suites target roughly 2,000 lines, splitting at complete fixture or
scenario boundaries. Smaller suites share files when they have the same owning
scope; private nested-module and binary boundaries justify smaller files.
Keep helpers with their coverage: `runtime::dst::Cluster` owns cluster setup,
`runtime::tests::dst` exposes targeted scenario helpers, `control::tests` owns
configuration fixtures, and `conformance` owns shared kernel/origin fixtures.
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
cargo test --locked --lib rdma::
cargo test --locked --lib crypto::
cargo test --locked --doc
```

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

## Deterministic campaigns

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
authority, ordering and geometry. The allocator model additionally holds two
checkpoint preparations across slabs and verifies deferred preparation resumes
after release and completes its durable root.

`runtime::storage::tests` exercises the shipping setup thread and worker polling
with real ext4/io_uring: multiworker grow/shrink across shard counts, same-size
no-op, live HTTP drain/503/refill, preparation/ENOSPC/staging/drain failures,
retry/supersession, delayed worker acknowledgments, post-rename sync failure,
retained old-inode reads, and shutdown fencing. Abrupt-exit subprocesses cover
prepared, renamed, and directory-synced restart boundaries. These test process
crashes, not power-cut filesystem behavior. Cache DST tests additionally cover
delayed scrub/read completion ownership and shared-NUMA consumer deadlines;
the cluster DST harness checks admitted RDMA work drains and the same registered
transport resumes. Native RDMA resize and full-device multi-TiB load remain
separate hardware validation.

`cmd/racer-controlplane/TestStorageRuntimeSignedResizeRestart` complements these
fault fixtures with the actual daemon executable and Go signed-policy server.
It applies 64MiB -> 20GiB -> 96MiB, verifies fresh inodes and shard changes without
changing the boot or topology, waits for `/status` and the controller's Node
annotation to agree, rejects a 5TiB runtime request without losing the old cache,
and checks invalid/equivalent input. It restarts controller and daemon, first with
control unavailable and an invalid creation-size environment, then requires fresh
policy acknowledgment with the same durable policy and published inode. Kubernetes
and TokenReview are simulated; filesystem, io_uring, signed HTTP and the process
are real. The separate envtest suites exercise Kubernetes validation and CAS.

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

The bounded lifecycle campaigns share assertions across generated transitions:

| Campaign | Properties |
| --- | --- |
| `runtime::dst::generated_request_lifecycle` | Independent exact-byte oracle; HEAD/GET, empty and page-boundary objects, ranges/416, shared-key requests, HTTP/RDMA, cancellation/refusal, restart and namespace/topology reload |
| `generated_activation_retry_and_supersession` | Per-worker preparation/activation model, last-good authority, retry deadlines, retained successful stages, stale acknowledgments |
| `generated_listener_churn_preserves_ownership_and_deadlines` | One socket owner, bounded draining generations, original deadlines, release after expiry |
| `generated_namespace_lifecycle` | Authority/universe/volume/generation isolation despite equal versions; topology and optional URL slash preserve reuse |
| `generated_consumer_lifecycle` | Metadata/page fan-in, producer/joiner cancellation or expiry, surviving deadlines, terminal error fanout without reacquisition |
| `http_client::tests::idle_close::b11_dst_idle_close_strict_replay` | Generated reuse/close/fault transitions with mandatory GET/HEAD fault cases, exact wire requests, bounded reconnect and healthy breaker |
| `generated_pinned_version_lifecycle` | Metadata/payload versions remain pinned across replacement, reordered write execution/completion, eviction and checkpoint rotations |

From this directory, run the generated campaigns with an external hard deadline:

```sh
timeout --signal=KILL 90s cargo test --locked --lib generated_ -- --test-threads=2
timeout --signal=KILL 30s cargo test --locked --lib b11_dst_idle_close_strict_replay
```

The cluster campaign accepts `RACER_DST_SEED`, `RACER_DST_SCHEDULER_SEED`, and
`RACER_DST_STEPS` (minimum/default 6). Workload and scheduling entropy are separate.
Failures print the actions and causal decisions, then require strict replay of
the same failure. Dedicated replay tests also check successful schedules.

## Shared cluster harness and verification

`runtime::dst::Cluster` runs both generated campaigns and targeted scenarios
through the production driver, readiness scheduler, and RDMA sources. Boot,
restart, SQ effects/CQ delivery, resource checks, and teardown are shared.
`runtime::tests::dst` supplies scenario helpers and assertions, with no separate
simulation engine. Its small pools, routing algorithms, co-located slots, and
shared-worker pools remain explicit. Mixed transport scenarios have one QP;
multi-edge scenarios use two rails with alternating worker indices to isolate
renewal from healthy sessions.

The retained scenario families cover:

| Family | Properties |
| --- | --- |
| `conformance::` | Aligned two-page range reads and cache reuse; canonical relay convergence and shared failures |
| `runtime::tests::dst::` | Torn persistence, cancellation, crossing metadata/payload flights, co-location, signed admission, shared-NUMA worker takeover, strict replay |
| `http_auth::attribution::` | Candidate bounds, final-hop evidence, owner/probe recovery, backend pressure, cancellation phases, RDMA renewal and healthy-session reuse |
| `control::tests::dst_` | Signed controller updates/key overlap and routing-algorithm rollover with fresh RDMA reads |

```sh
timeout --signal=KILL 90s cargo test --locked --lib runtime::tests::dst:: -- --test-threads=2
timeout --signal=KILL 90s cargo test --locked --lib http_auth::attribution:: -- --test-threads=2
timeout --signal=KILL 90s cargo test --locked --lib runtime::dst:: -- --test-threads=2
timeout --signal=KILL 30s cargo test --locked --lib conformance::
timeout --signal=KILL 30s cargo test --locked --lib control::tests::dst_
```

Use SIGKILL-bounded groups for the full library suite. The real-socket
`b16_production_subscriber_full_server_wait` checks a 300 ms held response with
`Prefer: wait=0`; its historical name does not establish 60-second long-poll
coverage. Binary and documentation tests also need separate runs.
Go/Rust coordination fixtures require matching snapshots
from the integrated controller tests; give each named test its own timeout.

Kernel, real-thread, malformed-input, permanent storage fault, and native RDMA
ownership checks retain their focused fixtures.
Ignored large-cluster and native-device stress cases are opt-in; read each test's
ignore reason before selecting it with `--ignored`.

## Full verification

Run the full library under an external bound. These include the generated
lifecycle/DST campaigns above:

```sh
timeout --signal=KILL 180s env RACER_REQUIRE_URING=1 RUST_TEST_THREADS=2 cargo test --locked --lib
timeout --signal=KILL 90s env RACER_REQUIRE_URING=1 RUST_TEST_THREADS=2 cargo test --locked --bins
RUST_TEST_THREADS=2 cargo test --locked --doc
```

The shared Go protocol helpers live in `internal/racer`, and the authoritative
schema and Go bindings live in `api/racer` (package `racerconfig`). Cross-language
fixtures marked ignored require their named Go-produced export or coordinator.
An unavailable environment must be reported separately from passing tests.
[README.md](README.md#benchmarks) documents the HTTP transport and checksum
benchmarks. Native RDMA and Soft-RoCE tests remain explicit opt-in checks.
