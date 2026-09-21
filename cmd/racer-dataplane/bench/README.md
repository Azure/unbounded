# Dataplane benchmarks

Run from `cmd/racer-dataplane/`. Install the [build prerequisites](../README.md#build)
first; native verbs development libraries are required for HTTP-only builds too.

```sh
cargo build --release --locked --bin http-bench --bin crypto-bench
./target/release/http-bench --help
```

Use a host permitting io_uring, NUMA binding/prefaulting, and locked-memory
registration. Select distinct physical cores on participating NUMA nodes;
adjacent logical CPU IDs may be SMT siblings. Bound the enclosing processes'
memory and runtime. Results depend on the exact binary, kernel, filesystem,
CPU placement, and resource limits.

## Warm page-cache HTTP throughput

`http-bench` uses the production HTTP transports. File mode publishes through
the slab and serves file-backed bodies; buffer mode uses immutable-buffer
SEND_ZC. Both transfer 4 MiB bodies over persistent HTTP/1.1, with payload
validation during warmup. File mode requires an existing ext4 slab directory
with sufficient free space; each worker uses an unlinked temporary slab.

In separate terminals, start the server and then the client. Substitute
disjoint physical-core masks appropriate to the host and an existing ext4
directory inside your workspace:

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

Stop the server with SIGINT or SIGTERM after the trial. Repeat with `--body buffer`
for comparison. The client must exit successfully and print a `RESULT` line.
Measure warm-cache transport separately from cold storage and origin fill.
File bodies avoid a payload staging slot per read, but the benchmark still
registers its setup pool. Report decimal Gbit/s, complete-body latency, errors,
CPU allocation, connection count, warmup, and every trial.

## Checksum and authentication throughput

`crypto-bench` measures bounded NUMA checksum admission and Ed25519 control
authentication. Bulk timing includes acquisition, fill, queueing, completion,
and publication. Worker counts are per NUMA node; select enough physical cores
for the requested I/O and compute pools.

```sh
timeout --signal=KILL 180s cargo run --release --locked --bin crypto-bench -- \
  --io-workers 1 --compute-workers 1,2,4 --warmup 2 --duration 5
```

Use `--bulk-only` to omit signing/verification measurement.

## Metrics correctness

Metric definitions are in [`src/metrics.rs`](../src/metrics.rs). Focused tests
cover worker aggregation, idle publication, exporter framing/deadlines/shutdown,
lookup classification through cancellation/retry, file-backed streaming, and
authenticated RDMA acknowledgment lengths and replay.

```sh
timeout --signal=KILL 120s env RUST_TEST_THREADS=1 \
  cargo test --locked --lib metrics::tests::
for module in http_client http_server cache handlers; do
  timeout --signal=KILL 120s env RACER_REQUIRE_URING=1 RUST_TEST_THREADS=1 \
    cargo test --locked --lib "$module::tests::kernel_integration" -- --exact || exit $?
done
```

Use an ext4 temporary directory via `TMPDIR`. Kernel wrappers run bounded
subprocesses; confirm each filter actually runs a test. See
[TESTING.md](../TESTING.md) for the full campaigns and hardware prerequisites.

For overhead measurements, control origin, client, payload, core allocation,
warmup, and trial order. Compare no scraping, normal scraping, and an explicit
aggressive rate. Measure process CPU per completed request, complete-body
latency, and scrape errors. Quiesce before checking exact totals because worker
publication is asynchronous. File-backed hits are `disk_hit`, even when warm in
Linux page cache; metadata and page misses are separate. Backend attempts may
exceed origin requests after idle-socket replay, and RDMA-to-HTTP fallback counts
both admitted attempts. Scrapes must not increment data traffic.
