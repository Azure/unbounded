# Executable restart integration tests

`process_restart.rs` executes Cargo's actual `racer-dataplane` binary, including
environment parsing, CPU discovery, TLS enrollment, projected key loading,
application startup, readiness, UDS listeners, encrypted O_DIRECT storage, signal
handling, and shutdown. It supplies a TLS controller fixture and a raw HTTP/UDS
origin adapter. Client requests use the production UDS wire protocol.

Run from the repository root on Linux with io_uring, O_DIRECT/STATX_DIOALIGN, at
least one usable CPU, and permission to create a private mount namespace. The
tests are explicitly ignored in ordinary unprivileged Cargo runs; selecting them
fails visibly if prerequisites are missing. The `Racer Rust Suite` CI job selects
this suite explicitly in a privileged restart step after the complete Rust suite.
On x86-64, run the same bounded system service locally. Cargo builds as your normal
user and its runner executes just this suite as root:

```sh
mkdir -p tmp
sudo systemd-run --wait --pipe --collect \
  --property="User=$(id -un)" --property="WorkingDirectory=$PWD" \
  --property=MemoryMax=12G --property=MemorySwapMax=0 \
  --property=RuntimeMaxSec=5min --property=TimeoutStopSec=30s \
  --setenv="PATH=$PATH" --setenv="HOME=$HOME" --setenv="TMPDIR=$PWD/tmp" \
  --setenv="CARGO_TARGET_DIR=${CARGO_TARGET_DIR:-$PWD/bin/racer-cargo}" \
  --setenv='CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER=sudo -n' \
  bash "$PWD/hack/scripts/memory-safe-run.sh" -- \
  cargo test --locked --manifest-path cmd/racer-dataplane/Cargo.toml \
  --all-features --test process_restart -j 2 -- \
  --ignored --test-threads=1 --nocapture
```

The command verifies a 12 GiB memory limit and no swap before launching Cargo.
It reuses the CI job's `bin/racer-cargo` artifacts; export `CARGO_TARGET_DIR` to
reuse a different worktree-local build directory. On other architectures, use the
corresponding Cargo target runner variable. Only `process_restart` is selected;
other ignored suites are not enabled.

Each process has one I/O/crypto worker pair, a 256 MiB sparse slab, 64 MiB
plaintext, 160 MiB ciphertext (including write staging), and 64 MiB dirty budgets.
The restart-only request-context budget is 16 MiB to satisfy the current signed
peer envelope startup floor; the old 1 MiB fixture exited before enrollment.
Tests run serially and transfer a 32 MiB-plus-113-byte object. All temporary files,
keys, logs, and sockets are under this crate's `target/restart-*` directories and
are removed by fixture guards. A child-only private mount maps its scratch runtime
directory over `/run`; the host's `/run` is untouched. Identity and slabs persist
between incarnations, while runtime socket directories are fresh by fixture
design. The deployed DaemonSet instead uses a hostPath for `/run/racer`
(`internal/racer/workload_controller.go:167,176-177`), so persistent stale-socket
restart recovery is not covered here. This matters after SIGKILL because startup
deliberately rejects existing sockets it does not own
(`src/client/transition.rs:176-177`). This suite proves persisted identity/slab
recovery once fresh listener paths are available, not unattended recovery from a
crash with stale hostPath sockets.

## Assertions

- **Graceful SIGTERM:** bootstrap page zero, fetch a pinned remainder spanning
  another full page and a short tail, verify every byte, and require successful
  process exit and client socket cleanup. Decode the resulting checkpoint and
  require all three page mappings under the projected page key. Launch a new PID
  with the same identity/slabs, reject origin requests, and verify the complete
  pinned object, exactly three disk hits, and zero origin calls/fills. The
  persisted certificate is freshly issued after token reauthentication; its
  verified cluster and Node UID remain the same. Each process enrolls twice,
  during pre-worker bootstrap and control-worker startup, so the two incarnations
  require four enrollments. Readiness requires completed pending identity cleanup.
- **Interrupted SIGKILL:** deliver and verify page zero, wait for real slab
  allocation, then pause the origin after sending the first chunk of another
  page. Kill and reap the process without graceful shutdown. Require no published
  checkpoint, launch a fresh PID with the same identity/slabs, and verify every
  byte of the pinned object. Require safe misses and the exact three pinned origin
  page refetches. A subsequent cross-page read succeeds with origin disabled.
  The same fresh-issuance, authenticated binding, and enrollment-count checks
  apply after SIGKILL; a valid persisted certificate never bypasses startup
  token authentication.

Readiness, client I/O, origin I/O, and process-exit waits are bounded. Process
guards kill and reap children on failure and print their captured logs. This is
process-loss coverage, not power-loss durability: checkpoint publication does not
fsync files or directories, and uncheckpointed slabs are not scanned
(`designs/racer-store-protocol.md:86-93`).

## Production throughput gates

`process/throughput.rs` reuses the executable, TLS control, and UDS origin
fixtures with **unchanged production default budgets**. Only thread count and
fixture endpoints/paths are configured. This profile is separate from the
small-slab restart profile above. No custom worker factory distributes clients.

From `cmd/racer-dataplane`, run:

```sh
cargo test --locked --test process_restart measurement::tests -j 2
CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n' \
  cargo test --locked --test process_restart -j 2 -- \
  --ignored --test-threads=1
```

The second command explicitly runs all eleven privileged tests in this target,
including the two restart tests. It requires mount namespaces, io_uring,
O_DIRECT, and enough allowed physical cores/quota for four worker pairs. Missing
prerequisites fail visibly. Ordinary Cargo runs ignore these tests and are not
evidence of throughput acceptance. Kernel `iou-wrk-*` tasks are reported but
excluded from the exact userspace worker-pair count.

### Coverage and result interpretation

- `production_progress_matrix`: 1/2/4 pairs; cold distinct origin and disk reads
  of 113-byte objects and 16 MiB pages; 32 MiB-plus-113-byte origin/memory reads
  and recovered cold-to-warm disk streaming; two-cache traffic; shared fan-in;
  80-byte ranges; and slow full-page readers. Every acceptance response must
  finish with correct status, ETag, range, framing, length, and content.
- `production_peer_and_failed_neighbor_progress`: two actual Applications,
  authenticated TCP peer copy followed by memory reuse, then SIGKILL of the
  preferred peer with unchanged membership and verified origin fallback. Runs
  at 1/2/4 pairs. This is a two-node HTTP path, not RDMA or a multi-hop cluster.
- `production_blocked_control_progress` holds a real mTLS snapshot response
  while client traffic completes. `production_blocked_listener_publication`
  blocks a new socket with a regular file, verifies last-good-cache traffic,
  removes the obstacle, and verifies the newly published cache serves requests.
- `production_multicache_disk_baseline` expects exactly one HTTP 503 and an
  overload counter increase for a cold full page with two active caches at four
  pairs. A small tail remains readable. Set `RACER_THROUGHPUT_STRICT_BASELINE=1`
  in the **runner environment** to require full-page success instead.
- `production_ingress_baseline` holds 32 partial client heads at four pairs,
  observes acceptance via process socket FDs, proves excess ingress stalls,
  checks readiness, releases the clients, and verifies recovery. Its 200 ms
  probe timeout is a diagnostic bound, not a production timeout change.
- `production_churn_diagnostic` reads 512 distinct small objects across two
  caches with concurrency two at 1/2/4 pairs. It allows only HTTP 503 failures,
  records exact successes and failures, and checks server-counter consistency.
  Set the strict-baseline flag to require all 512 responses. Do not cite a
  diagnostic pass as zero-failure acceptance.

`THROUGHPUT` JSON reports completed verified payload bytes/second, attempt and
completion counts, categorized failures including truncation, complete-response
p50/p99, aggregate production counter deltas, before/after RSS and socket FDs,
and per-thread execution time from Linux `schedstat`. Results append to
`target/throughput-results.jsonl` and survive fixture cleanup. `BASELINE` records
are printed to test output. Failed partial bodies contribute zero goodput.
Slow-reader delays and full content verification are inside timing. The origin
adapter generates bytes on the same host. These are end-to-end fixture results,
not a hardware maximum. CPU observation brackets include small setup/sampling
overhead; RSS/FD samples are not peaks. Reports enumerate unavailable counters:
per-worker reservation failures, queue residence, reactor lag, allocation/copy
bytes, and ranking misses. No new production counters were added.

### Configurable measurements for later phases

```sh
CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n env RACER_THROUGHPUT_MODE=memory RACER_THROUGHPUT_PAIRS=4 RACER_THROUGHPUT_REQUESTS=128 RACER_THROUGHPUT_CONCURRENCY=4' \
  cargo test --locked --release --test process_restart \
  production_configured_measurement -j 2 -- --ignored --test-threads=1 --nocapture
```

Parameters are `MODE` (memory/disk/origin), `PAIRS` (1/2/4), `BYTES` (113 through
64 MiB), `CACHES` (1 through 4), `REQUESTS` (1 through 4096), `CONCURRENCY`,
`SHARED`, `SLOW`, and `SHORT`, each prefixed `RACER_THROUGHPUT_`. Boolean values
are 0/1. Defaults are four pairs, 16 MiB, one cache, 32 requests, concurrency one,
and distinct origin reads; memory defaults to shared reads. The configurable
test always requires zero failures. Resource-heavy configurations can fail
preload persistence or admission at current budgets; they are useful gates,
not automatically supported acceptance points. Disk preload waits for each
page's publication, then restarts to remove the memory cache. Long distinct
disk preloads can still expose missing persistence; they are not silently
refetched from origin.

Strict expected-baseline example (currently exits nonzero):

```sh
CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n env RACER_THROUGHPUT_STRICT_BASELINE=1' \
  cargo test --locked --test process_restart production_multicache_disk_baseline \
  -j 2 -- --ignored --test-threads=1 --nocapture
```

Use the appropriate Cargo runner variable on non-x86-64 systems. `sudo` normally
filters ambient variables, so put throughput parameters after `env` in the
runner as shown. Child dataplanes clear inherited `RACER_*` values and receive
only the fixture's explicitly selected production configuration.
