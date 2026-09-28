# Real Racer SDK connection-age validation

## Result

The opt-in real Application test passed on September 28, 2026. The SDK test
binary included the candidate MaxConnAge implementation. No production SDK
changes were needed for this validation.

| Metric | Nonrotating control (1h age) | Rotation (500ms age) |
| --- | ---: | ---: |
| Elapsed seconds, including final response drain | 8.452 | 8.444 |
| Completed operations | 2,585 | 2,765 |
| Verified payload bytes | 553,796,612 | 553,806,669 |
| Verified MiB/s | 62.487 | 62.547 |
| Completed operations/s | 305.843 | 327.450 |
| Bulk / small / Stat completions | 47 / 1,269 / 1,269 | 47 / 1,358 / 1,360 |
| Bulk p50 / p99 ms | 663.118 / 1720.286 | 673.572 / 1725.085 |
| Small p50 / p99 ms | 1.474 / 10.922 | 0.892 / 3.016 |
| Stat p50 / p99 ms | 1.426 / 13.352 | 1.303 / 3.348 |
| Dials, bulk+metadata / small | 3 / 1 | 38 / 18 |
| Age retirements, bulk+metadata / small | 0 / 0 | 38 / 18 |
| Reuses, bulk+metadata / small | 1,360 / 1,268 | 1,416 / 1,340 |
| Queue waits, bulk+metadata / small | 381 / 451 | 459 / 361 |
| Cumulative queue-wait seconds, bulk+metadata / small | 14.928 / 0.172 | 14.877 / 0.126 |
| Peak sampled SDK queue / active / open connections | 4 / 4 / 4 | 4 / 4 / 4 |
| Failures, retries, queue rejection, queue timeout | 0 | 0 |

Rotating bulk+metadata retirement counts at seconds 1 through 8 were
3, 7, 12, 17, 21, 25, 30, 35; small-pool counts were
2, 4, 6, 8, 10, 13, 15, 17. Final totals include final response drain and idle
expiry. All final admission and queue gauges were zero. Sampled server request
errors, overloads, corrupt misses, and dirty discards were zero in both phases.

The existing startup assertions require four production I/O workers
(`cmd/racer-dataplane/tests/process_restart.rs:230-249`). Their measured Linux
schedstat execution-time shares (worker 0/1/2/3) were:

- Control: 23.36% / 34.32% / 14.28% / 28.04%.
- Rotation: 24.51% / 33.34% / 15.50% / 26.65%.

These are CPU activity shares, NOT request or accepted-connection shares.

## Workload and assertions

The workload is implemented in
`pkg/racersdk/runtime_connection_age_test.go`. Eight producers run for eight
seconds, with an additional ten-second context allowance for final responses.
Four bulk producers contend for two bulk slots; two Stat producers share one
reserved metadata slot; two small-object producers share one reserved
small-object slot on a second cache. Bulk objects are 16 MiB + 113 bytes. Small
objects are 113 bytes. Bulk operations mix bootstrap plus pinned continuation
with a 97-byte range crossing the page boundary. One producer holds each active
response for 1.1 seconds, exceeding the 500ms maximum age; another pauses 100ms
between requests instead of the other producers' 10ms. Production jitter, clock,
timers and dialing are used.

Every successful payload byte is independently checked against the origin
fixture's position-dependent pattern. Object size and ETag are checked. The
SDK's production response parser checks wire framing and ranges. The test fails
on errors, missing traffic classes, absence of queue pressure, retries, queue
rejections/timeouts, retained admission, or fewer than three rotating retirements
per client. The 1h control must not rotate.

## Scope and limitations

- This is the actual Rust executable, not a simulated HTTP peer. It uses four
  I/O/crypto pairs, production Application startup/listeners/coordinator/crypto/
  storage, a TLS enrollment/controller fixture, and a raw UDS origin fixture.
- The separate Go/Rust conformance harness was inspected, not redundantly run.
  It scripts responses through production HTTP/parser components, explicitly not
  the Coordinator (`cmd/racer-dataplane/tests/conformance/sdk.rs:144-146`).
- The Rust runtime exports aggregate metrics, not per-worker request counts,
  connection assignment, queue residence, or reactor lag. Worker CPU is a proxy.
  No exact request rebalancing claim or causal improvement claim is justified.
- One hot object per cache intentionally skews page ownership. This does not
  validate many-key placement, multi-node peers, RDMA, or cluster rollout.
- Baseline runs first and includes three cold page fills; rotation runs warm.
  This is a bounded correctness/load regression, not an unbiased benchmark.
- Percentiles include admission, SDK I/O, verification and deliberate delay;
  p99 is an order-statistic sample, particularly coarse for 47 bulk operations.
- SDK gauges are sampled every 100ms, server metrics/CPU every 500ms. Peaks may
  miss transients, and sampled server deltas omit activity after the last scrape.
  Zero sampled pending disk writes does not prove no transient disk queue.
- Ages are short to exercise several cycles. This does not validate five-minute
  real-time cycles or long-duration leak behavior. The 1h age is a nonrotating
  control for this bounded run, not a feature-disable setting.

## Reproduction and evidence interpretation

The Rust entry point is `real_sdk_connection_age_sustained` in
`cmd/racer-dataplane/tests/process/sdk_connection_age.rs`, registered by
`cmd/racer-dataplane/tests/process_restart.rs`. It runs the prebuilt Go SDK test
binary without modifying SDK sources.

Run from the repository/worktree root. Set `ROOT` to that root's absolute path
and `CARGO_TARGET_DIR` to a caller-chosen absolute build directory within the
repository, optionally reusing existing artifacts. Ensure `tmp/` and the build
directory exist. All commands are bounded; no personal paths or executable hash
are required:

```sh
timeout --signal=TERM --kill-after=10s 300s env TMPDIR="$ROOT/tmp" \
  go test -timeout=5m -c -o "$ROOT/tmp/sdk-age.test" ./pkg/racersdk
timeout --signal=TERM --kill-after=10s 300s env TMPDIR="$ROOT/tmp" \
  CARGO_TARGET_DIR="$CARGO_TARGET_DIR" \
  cargo test --locked --release --manifest-path "$ROOT/cmd/racer-dataplane/Cargo.toml" \
  --test process_restart -j 2 --no-run
timeout --signal=TERM --kill-after=10s 300s env TMPDIR="$ROOT/tmp" \
  CARGO_TARGET_DIR="$CARGO_TARGET_DIR" \
  CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER="sudo -n env RACER_SDK_AGE_BINARY=$ROOT/tmp/sdk-age.test" \
  cargo test --locked --release --manifest-path "$ROOT/cmd/racer-dataplane/Cargo.toml" \
  --test process_restart -j 2 real_sdk_connection_age_sustained -- \
  --ignored --test-threads=1 --nocapture
```

Use the corresponding Cargo runner variable on other architectures. For paths
containing spaces, use Cargo's emitted test executable path directly with
`sudo -n env RACER_SDK_AGE_BINARY="$ROOT/tmp/sdk-age.test"` under the same external
timeout, rather than the whitespace-split runner string.

The fixture requires root/mount namespace permission, io_uring, O_DIRECT, and
enough CPU for four pairs. The Go test skips without the fixture's environment.
Its `RACER_SDK_AGE_DURATION` accepts 4s through 30s; `RACER_SDK_AGE` is a positive
Go duration. The Rust driver sets 1h and 500ms ages, each under an external 60s
TERM/10s kill-after bound, and the entire invocation is externally bounded.

Each run writes `cmd/racer-dataplane/target/sdk-age-{1h,500ms}.{json,log}`. These
local artifacts are overwritten on subsequent runs and are not checked in. The
compact historical results above are preserved independently of those artifacts.
No temporary summary script is required to interpret new results:

- Each log's `SDK_AGE` JSON includes verified bytes, elapsed seconds, throughput,
  per-class latency, final SDK counters and per-second SDK samples.
- Each JSON file contains `cpu_before_ns`, `cpu_after_ns`, and server samples.
  For each I/O worker (main `racer-dataplane` thread is worker zero), subtract
  its before execution time from its after time. Divide by the sum of these
  four differences to obtain the CPU activity shares above.
- Subtract first from last sampled cumulative server counters for sampled
  deltas. Take the maximum sampled gauge for sampled peaks. Neither operation
  measures events outside the sample window or unsampled transient peaks.

Fixture secret material and runtime scratch directories are removed by guards.

## Cleanup review follow-up

The workload measurements predate a cleanup-only review fix; they were not rerun
because load behavior did not change. `SDKProcess::drop` now sends group TERM,
allows a bounded grace period without killing/reaping the supervisor, then sends
group KILL before bounded supervisor reaping. The fallback deadline allows the
timeout supervisor's full 60s + 10s escalation window. A focused regression,
`sdk_process_cleanup_terminates_term_ignoring_child`, starts a timeout-supervised
child with TERM ignored and requires cleanup to reap the supervisor and leave
the child absent or nonexecuting (a zombie awaiting init reaping).

The cleanup regression passed (one test, zero failures); compiling that focused
target also compiled the real load harness. Only the changed Rust file was
formatted, and `git diff --check` passed. No Go formatting or load rerun was
performed during this follow-up.

Run the focused cleanup regression without root or a dataplane startup:

```sh
timeout --signal=TERM --kill-after=10s 300s env TMPDIR="$ROOT/tmp" \
  CARGO_TARGET_DIR="$CARGO_TARGET_DIR" \
  cargo test --locked --release --manifest-path "$ROOT/cmd/racer-dataplane/Cargo.toml" \
  --test process_restart -j 2 sdk_process_cleanup_terminates_term_ignoring_child -- \
  --test-threads=1 --nocapture
```
