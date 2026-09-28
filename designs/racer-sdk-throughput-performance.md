# Racer SDK mixed-load performance integration

## Scope and conclusion

This is a repeatable **local, single-node process integration**, using the actual
release-built Rust `racer-dataplane`, the actual Go SDK client and UDS origin
server, Gantry's production mirror and origin adapter, and racer-loadgen's
deterministic synthetic OCI registry and digest-verifying consumer. There is no
fake Racer on the measured data path. These numbers are not production performance
claims or deployed Kubernetes results. The follow-up below adds a before/after
pool comparison, including an explicitly labeled current-SDK bulk-routing control.

The initial finding was that aggregate delivery remained high at 64 concurrent
bulk workers, but **small manifest GET latency degrades substantially at the
default 64-value SDK bulk limit**. HEAD continues through its separately reserved
pool. The first follow-up confirmed improved small-object latency but identified
small-object queue rejection with the then-default 16 waiters. The final revision
with **128 small-object waiters passes unpaced 32/64-worker bursts without
admission errors**, while retaining the latency improvement. The historical
failures and contended attempts are preserved below.

Owned files are new `cmd/racer-loadgen/performance_test.go`,
`cmd/racer-loadgen/performance/{Cargo.toml,Cargo.lock,control.rs,run.sh}`, and this
report. No existing e2e or production implementation file is part of this change.

## Why this harness

- The existing cross-language conformance fixture is a scripted Rust HTTP peer,
  explicitly not a Coordinator (`cmd/racer-dataplane/tests/conformance/sdk.rs:144`).
  It is useful for interoperability, not cold/warm cache throughput.
- The existing process fixture supplies actual TLS enrollment, an mTLS snapshot,
  and runtime-generated test keys
  (`cmd/racer-dataplane/tests/process/control.rs:21-187`). The new small Rust
  controller executable imports that fixture by path rather than copying it.
- The production executable assembles and runs `Application`
  (`cmd/racer-dataplane/src/main.rs:15-21`). The benchmark launches that executable,
  waits for `/readyz`, uses canonical `/run/racer/gantry` sockets, and terminates it
  with SIGTERM. A private mount namespace maps only worktree-local backing storage
  onto the child's `/run`.
- Existing loadgen virtual layers have stateless random access, avoiding an
  object-sized source allocation (`cmd/racer-loadgen/image.go:165-166`). Its puller
  verifies complete object sizes and SHA-256 digests
  (`cmd/racer-loadgen/pull.go:240-255`).

This exercises the real Coordinator, memory cache, crypto, origin acquisition,
and disk publication. It does not exercise peer distribution, RDMA, disk-hit
throughput, containerd unpacking, operator reconciliation, a real registry service,
or crash/restart recovery. The TLS controller is a fixture, and Gantry/registry/
consumer run in one Go process. The separate deployed e2e work remains independent.

## Exact reproduction

From the repository root in a fresh checkout, with Go 1.26.6 and Rust/Cargo
installed normally on `PATH`:

```sh
# Confirm the installed Go version, then compile before measuring.
go version
bash cmd/racer-loadgen/performance/run.sh build

# Each invocation creates a fresh Rust process, sockets, cache, identity, and slab.
bash cmd/racer-loadgen/performance/run.sh run 32
bash cmd/racer-loadgen/performance/run.sh run 64

# Repeat the same commands for independent process runs.
```

Requires Linux, sudo without an interactive password, private mount namespaces,
io_uring, and a filesystem supporting the dataplane's direct-I/O storage. Build
uses Cargo release mode with two compile jobs and locked dependency resolution.
The fixture's dependencies are already dataplane dependencies, isolated from Go
and the production Cargo manifest. Outputs live in `tmp/racer-performance/`.
The runner uses `${GOCMD:-go}` directly, with no ignored environment-wrapper
dependency. `GOCMD` may name an alternate Go executable. It respects supplied
`GOCACHE`, `GOPATH`, `GOMODCACHE`, `GOTMPDIR`, and `TMPDIR`; unset cache/temp values
default beneath `tmp/racer-performance/`. `build-info.txt` records tool versions
and binary hashes. The existing local wrapper was used only by this session's
outer build command to select its installed Go 1.26.6 and shared project caches:
`bash tmp/review-go-env.sh bash cmd/racer-loadgen/performance/run.sh build`.
That wrapper is not required by the runner or fresh-checkout commands above.
Each `run-*` directory contains `results.json`, `test.log`, `dataplane-config.json`,
Rust metrics snapshots, and runtime state. `build-overlap.log` samples named build
processes once per second. The runner refuses to start while those processes are
present. This is a best-effort contention check, not exclusive host reservation.

Do not run builds concurrently with measurements. Other users' clusters and
system services remain possible noise sources on this shared host.

Verification commands:

```sh
go test ./cmd/racer-loadgen -count=1
golangci-lint run ./cmd/racer-loadgen/...
rustfmt --edition 2024 --config skip_children=true --check cmd/racer-loadgen/performance/control.rs
bash -n cmd/racer-loadgen/performance/run.sh
git diff --check
```

## Workload and measurement definitions

- Deterministic one-layer image: layer 50,333,696 bytes (48 MiB plus tar framing),
  image 50,334,260 bytes including manifest/config. Source seed `mixed-v1`.
- 32 or 64 persistent bulk workers. Each performs one complete cold pull, then
  20 complete warm pulls. Every successful pull is SHA-256 verified. The warm
  working set fits in memory. Cold means initially empty Racer state; concurrent
  readers of the same image share fills, so the entire cold burst is not a series
  of independent misses.
- Two additional closed-loop workers issue layer HEAD and digest-addressed
  manifest GET, sleeping 2 ms after each response. The manifest body is verified.
  Nearest-rank p95/p99 include probes **started with more than 16 live bulk
  workers**, even if a slow probe completes after bulk drains. Workers alternate
  manifest/config/layer requests; this is not a guarantee of more than 16
  simultaneous layer bodies at every probe. The active SDK bulk peak is recorded.
- Useful throughput is bytes in completed verified bulk images divided by bulk
  wall time, in MiB/s. Extra metadata probe bytes are excluded from useful bytes.
  Origin bytes count actual HTTP response body writes at the registry, excluding
  headers. Amplification is origin bytes divided by aggregate useful bytes.
  For a shared cold image, also compare origin bytes with one unique image.
- One separate, previously unseen `resume-v1` layer is requested from offset
  16,777,223 (`PageSize + 7`) to EOF, then repeated warm. The consumer hashes the
  local prefix plus received suffix against the full-object digest. Suffix useful
  bytes are counted independently of local prefix bytes. Resume elapsed time
  includes generating/hashing that local prefix and is not a pure transport time.
- Three pressure/recovery cycles hold 64 returned SDK Values unread, submit 160
  additional Gets, observe 128 waiters and 32 new queue rejections, and hold for
  250 ms. A direct SDK Stat must succeed while saturated. Cancellation joins all
  waiters and closes held Values; a complete verified Gantry pull must then work.
  These cycles exercise SDK admission and slow-reader cancellation, not upstream
  outage recovery or Rust internal queue saturation.
- Heap, Go RSS, Rust RSS, goroutines, and SDK admission gauges are sampled every
  10 ms during mixed phases. Sampling may miss shorter peaks. RSS is `/proc/PID/statm`;
  Go heap includes the consumer, registry, mirror, adapter, and SDK. It is not an
  SDK-only allocation metric. Retained Rust cache/allocator memory need not return
  to pre-fill RSS after cancellation.

SDK defaults used: 64 bulk Values/connections, 4 metadata connections, 128 queued
calls, five-second queue timeout (`pkg/racersdk/client.go:15-32,69-82`). Actual
`Get` admits to the bulk pool (`pkg/racersdk/client.go:248`); `Stat` admits to the
metadata pool (`pkg/racersdk/client.go:298`). Gantry routes HEAD to Stat and
manifest GET to Get (`internal/gantry/mirror/racer.go:53-82`). This is the code-based
explanation consistent with the observed manifest queue delay, not a CPU profile.

Rust configuration: one I/O/crypto worker pair (`RACER_MAX_THREADS=2`), 256 MiB
plaintext, 512 MiB ciphertext, 128 MiB dirty, 16 MiB request-context budgets;
128 client connections, 64 flights/pipes, 256 queue entries, two-page range window,
eight origin connections, 1 GiB slab with 64 MiB segments. Complete settings are
saved per run. Go uses `GOMAXPROCS=8`.

## Initial measurements (before separate small-object admission)

Host: Linux 7.0.0-30-generic, x86-64, Microsoft hypervisor, AMD EPYC 9V74,
48 logical CPUs / 24 exposed cores. Go 1.26.6, Rust 1.96.0. Worktree base commit
`42287a50dbc47b7ec7efa4fb693b9433544765d6` plus concurrent uncommitted SDK/Gantry/Rust
rewrite work. This base commit alone does not identify the measured implementation.
Release dataplane SHA-256:
`b0c66cbf635083de1d1083d7f5b956466b15c90ffafd3397c978d65183a2d2d5`.

### Primary runs: no sampled build overlap

Artifacts are relative to `tmp/racer-performance/`. Both primary runs have empty
`build-overlap.log` files. Values are individual runs, not statistical confidence
intervals. All complete images and resumed objects passed digest verification.

| Run | Bulk workers | Phase | Useful MiB/s | HEAD p95 / p99 ms | Manifest GET p95 / p99 ms | HEAD / manifest samples |
| --- | ---: | --- | ---: | ---: | ---: | ---: |
| `run-oGbrbt` | 32 | Cold | 2,127.9 | 8.149 / 17.369 | 8.414 / 17.768 | 101 / 99 |
| `run-oGbrbt` | 32 | Warm | 2,956.3 | 6.632 / 7.307 | 6.682 / 7.380 | 1,349 / 1,330 |
| `run-RiD1v0` | 64 | Cold | 2,846.5 | 15.639 / 19.829 | 1,023.567 / 1,023.567 | 107 / 8 |
| `run-RiD1v0` | 64 | Warm | 3,376.2 | 15.724 / 18.102 | 709.392 / 913.165 | 1,355 / 183 |

The 32-worker warm phase delivered 32,213,926,400 verified bytes in 10.392 s;
64 workers delivered 64,427,852,800 bytes in 18.199 s. These are aggregate bytes
delivered to all consumers, not unique origin data. At 64 workers the warm phase
added 1,508 SDK admission waits, peaked at one queued call and 64 occupied bulk
slots, and had no SDK queue timeout or rejection. At 32 workers there were no
admission waits and the bulk peak was 33, including the manifest probe.

Go executable SHA-256 for `run-oGbrbt`:
`e85e975575e505573e44f870f1f8ac34423be21edb52334bd4a1c1d363443474`.
For `run-RiD1v0`:
`8c74a938c58377a71fa118c5a386a21ec169544f412e752b371c2ef2ddba3c0f`.
The intervening harness-only change adds Rust metric capture on test failure;
the successful workload and dataplane binary are unchanged.

### Amplification

Both primary cold bursts fetched exactly one unique image: **50,334,260 origin
body bytes**, six GETs (manifest, config, four layer pages) and one HEAD. Each
layer page was fetched once. Origin/unique-image amplification was 1.0; divided
by all consumers' useful bytes it was 1/32 = 0.03125 or 1/64 = 0.015625. These
small aggregate ratios reflect shared caching, not compression. Both warm phases
made **zero origin requests and transferred zero origin bytes**.

Cold resume delivered 33,556,473 useful suffix bytes and fetched 33,556,480 origin
bytes: **1.0000002086x**, seven bytes of page-alignment overhead. It issued one
HEAD and three page GETs, starting at 16,777,216; there was no page-zero request.
Warm resume issued zero origin requests/bytes. Full reconstructed SHA-256 matched
in both phases. Primary cold/warm resume elapsed times were 113.385/45.050 ms
at 32 workers and 112.070/48.358 ms in the 64-worker run; the resume phase itself
is serial after mixed load, not a 32/64-way resume workload.

### Memory, queue bounds, and recovery

| Observation | 32-worker primary | 64-worker primary |
| --- | ---: | ---: |
| Warm peak Go heap | 8,444,848 bytes | 14,680,760 bytes |
| Warm peak Go RSS | 28,463,104 bytes | 36,986,880 bytes |
| Warm peak Rust RSS | 126,140,416 bytes | 127,045,632 bytes |
| Warm peak goroutines | 216 | 408 |
| Saturated SDK bulk / queued / new rejections, each cycle | 64 / 128 / 32 | 64 / 128 / 32 |
| SDK Stat at saturation, three cycles | 0.281 / 0.253 / 0.276 ms | 0.253 / 0.291 / 0.269 ms |
| Verified post-cancel pull, three cycles | 47.609 / 41.467 / 48.663 ms | 44.072 / 41.012 / 50.730 ms |
| Rust RSS after each recovery | 196,554,752 bytes, all three | 197,140,480 bytes, all three |

Each pressure cycle returned all 160 waiting/overflow calls as failures after
rejection or cancellation. The SDK recovered to zero active bulk, zero active
metadata, zero queue depth, and two reusable idle connections. Cumulative queue
rejections increased by exactly 32 per cycle (96 total); queue timeouts stayed
zero. Retained Go RSS after the three 64-worker recovery cycles was
37,335,040 / 37,343,232 / 37,355,520 bytes. Rust RSS plateaued across the three
cycles, but this short observation does not prove an allocator or process-memory
upper bound.

Final primary 64-worker Rust metrics show 9 origin fills and 9 disk publications,
8,209 memory hits, zero disk/peer hits, zero overloads, and zero corrupt misses.
The 192 request errors correspond to the 3 x 64 deliberately abandoned bodies;
mixed and resumed requests succeeded. Active requests, active fills, and pending
disk writes all drained to zero; readiness remained 1. See
`run-RiD1v0/final-rust.prom:1-50` for the captured counters/gauges.

### Repeats, contention, and failures

- `run-bNTNSG`, same workload before using the public Gantry request builder in
  the pressure helper: 64-worker warm 3,225.7 MiB/s, HEAD p99 18.835 ms,
  manifest p99 889.956 ms. A pre-run build check passed, but this earlier run did
  not yet monitor overlap continuously. It independently reproduced the latency
  cliff and all three recovery cycles passed.
- `run-ZT1CJM`: 64-worker warm 2,955.1 MiB/s, HEAD p99 19.513 ms, manifest p99
  1,049.251 ms. Another worktree started a Rust compilation during the run, recorded
  in `build-overlap.log`. **Contended repeat, not the primary throughput sample.**
- `run-lI5jKI`: a fresh 64-worker run failed on a cold layer HTTP 503 before a
  result document was emitted (`test.log:1-10`). Its overlap log was empty. The
  root cause is not established; the harness did not silently retry or count the
  failed image as useful throughput. A subsequent run passed as `run-RiD1v0`.
  Failure-only Rust metric capture was added afterward, so this one failure has
  no such snapshot. This is an unresolved cold-burst availability observation,
  not a claim that every repetition succeeds.
- Earlier exploration used a shorter four-round warm phase or sampled probes by
  completion instead of start. Those numbers are excluded from the primary table.
  Start-based inclusion now preserves slow calls that finish after bulk drains.

Package tests, scoped golangci-lint, Rust formatting, shell syntax, and
`git diff --check` passed. Opt-in process runs above passed except the explicitly
listed setup attempts and cold-503 repetition. No production source was changed
to make the benchmark pass.

## Follow-up: separate small-object admission

Rebuilt after the SDK added four small-object connections, its independent
16-entry queue, a separate 16-entry metadata queue, and four origin HEAD callback
slots. Gantry now sets `SmallObject: true` on manifest GETs
(`internal/gantry/mirror/racer.go:82-86`); SDK selects `smallPool`
(`pkg/racersdk/client.go:235-238`). Defaults and separate queues are constructed at
`pkg/racersdk/client.go:77-128`; origin HEAD slots default to four at
`pkg/racersdk/origin.go:76-78`.

### Repeatable cold 503: exact cause identified for the new burst failure

Unchanged synchronized-start commands were attempted first:

```sh
bash cmd/racer-loadgen/performance/run.sh build
bash cmd/racer-loadgen/performance/run.sh run 32
bash cmd/racer-loadgen/performance/run.sh run 64
```

Both overload the default small-object admission capacity: all image workers
first request the same manifest, competing for four active slots and 16 waiters.
The 32-worker attempt logged 13 rejections. The 64-worker run `run-DwCfW4`
logged **45 rejections**, including the independent manifest probe. Its exact
failed HTTP resource was:

```text
GET /v2/perf/bulk/manifests/sha256:11ce78441c5c574139faf24d2700dbb3cc64adeeb82250c75d079b740bd4dc04?ns=loadgen.invalid
SDK kind=unavailable operation=queue full status=0 cause=<nil>
ActiveSmallObjects=4 SmallObjectQueueDepth=16 BulkQueueDepth=0
```

`status=0` means this error was raised locally, not received as a Rust HTTP 503.
The SDK queue-full branch is `pkg/racersdk/client.go:148-155`; Gantry maps it to an
HTTP failure. `run-DwCfW4/failure-rust.prom` records zero Rust request errors and
zero Rust overloads, corroborating the local admission diagnosis. Failed pulls
are never counted as useful complete images; the run fails rather than silently
retrying or presenting surviving-reader throughput as a successful 64-way result.

This newly reproducible **manifest** admission failure does not establish the
cause of the earlier **layer** 503 in `run-lI5jKI`. The old layer failure was not
observed in the follow-up completed runs. The harness now logs SDK operation,
classification, HTTP status, object key, cause, and Stats for mirror-boundary
errors; logs origin callback errors; and saves phase results before asserting
success plus Rust metrics on failure. It does not expose a private Rust error
reason beyond what the public protocol and metrics provide.

### Comparable steady mixed workload and commands

The default synchronized mode is retained. A separately labeled paced mode
starts successive bulk workers 5 ms apart in both cold and warm phases. There
are still 32/64 persistent bulk workers, 20 warm pulls per worker, and the same
images, consumer hashing, and more-than-16-live-worker probe filter. Start spacing
is included in elapsed time. There are no retries, preloaded images, raised SDK
queue caps, or dropped useful bytes in this mode.

Rust's test pipe budget was raised from 64 to **72** to cover 64 bulk plus four
small-object body streams and metadata headroom. With 64 pipes, the expanded
pressure check stalled a small-object body behind 64 unread bulk bodies until
bulk reader timeouts, invalidating its queue assertion (`run-bRMcjp`). Changing
only this harness resource budget allowed simultaneous pool saturation/recovery.
The historical comparison therefore has both this disclosed configuration change
and the start-spacing change; do not attribute every throughput difference to
the SDK pool change.

```sh
# Actual updated Gantry/SDK behavior, unchanged SDK defaults:
bash cmd/racer-loadgen/performance/run.sh run 32 5ms
bash cmd/racer-loadgen/performance/run.sh run 64 5ms

# Explicit current-SDK control, same pacing/configuration:
# The harness clears SmallObject on mirror Get calls, restoring bulk routing.
# This control omits the new all-pool pressure phase.
bash cmd/racer-loadgen/performance/run.sh run 64 5ms 1
```

The control is a routing experiment, **not** a rebuilt historical SDK. It changes
no source implementation, protocol, registry bytes, or Rust executable.

### Follow-up results

`run-q8tfYE` (32) and `run-3nUdAh` (64) have empty build-overlap logs. The latter
was started after a quiet interval with no compiler, Docker build, or `racer.test`
process. Host services and other agents were not exclusively reserved. Repeated
external builds that began after preflight were detected and labeled below.

| Run / mode | Workers | Phase | Useful MiB/s | HEAD p95 / p99 ms | Manifest p95 / p99 ms | HEAD / manifest samples |
| --- | ---: | --- | ---: | ---: | ---: | ---: |
| `run-q8tfYE`, small-object | 32 | Cold | 1,700.9 | 12.078 / 30.453 | 12.023 / 30.932 | 87 / 86 |
| `run-q8tfYE`, small-object | 32 | Warm | 1,989.0 | 11.816 / 13.054 | 11.793 / 13.368 | 1,342 / 1,358 |
| `run-3nUdAh`, small-object | 64 | Cold | 2,334.7 | 18.858 / 28.709 | 19.673 / 28.182 | 90 / 85 |
| `run-3nUdAh`, small-object | 64 | Warm | 2,423.2 | 22.200 / 23.938 | 19.063 / 20.297 | 1,364 / 1,370 |
| `run-AO7K5a`, bulk control, build overlap | 64 | Cold | 2,348.4 | 19.499 / 27.762 | 946.653 / 946.653 | 88 / 18 |
| `run-AO7K5a`, bulk control, build overlap | 64 | Warm | 2,308.9 | 23.491 / 25.573 | 41.466 / 140.615 | 1,375 / 831 |

At 64 workers, the no-build-overlap updated run's warm manifest p99 is
**20.297 ms versus historical 913.165 ms (97.8% lower)**. The same-paced control
observed 140.615 ms, approximately 6.9 times the small-object result, but an
external Rust compile overlapped that control, so this is corroboration rather
than an uncontended causal ratio. Two additional controls reproduced elevated
warm p99 (178.940 and 422.479 ms); both also recorded external compilation.
Updated-pool repeats recorded 20.420 and 25.612 ms, also with brief build overlap.
The observed improvement is specific to saturated 64-way bulk traffic: at 32
workers the historical p99 was 7.380 ms versus follow-up 13.368 ms, not an
improvement. Host load and pacing differ from the historical runs.

Useful bytes are exactly preserved: 32,213,926,400 warm bytes at 32 workers and
64,427,852,800 at 64, with all digests matching. Both warm phases again issued
zero origin requests/bytes. Cold origin bytes stayed 50,334,260, and cold resume
stayed 33,556,480 origin bytes / 33,556,473 useful bytes (seven-byte overhead),
with zero warm-resume origin traffic. **Absolute throughput was lower than the
historical primary runs**, so no historical throughput-preservation claim is
made. Against the same-paced, contended bulk control it was comparable (2,423
versus 2,309 MiB/s), not a clean performance-isolation experiment.

### Updated bounds and recovery

Harness checks now cover bulk <=64, metadata <=4, small-object <=4, bulk queue
<=128, metadata queue <=16, small-object queue <=16, and total SDK connections
<=**72**, not 64. Independent gauges come from `pkg/racersdk/stats.go:54-69`.
The metadata queue is observed but not forced to saturation by this workload.

For `run-3nUdAh`, the warm peaks were 64 bulk, 4 small-object, 1 metadata,
69 open connections, and two small-object waiters; no bulk/metadata waiters,
rejections, or timeouts occurred. Peak Go heap/RSS were 13,912,200 / 36,237,312
bytes; Rust RSS was 126,963,712 bytes. At 32 workers the corresponding heap/RSS
were 8,473,608 / 28,745,728 and Rust RSS 126,046,208 bytes.

Three expanded pressure cycles each held 64 bulk Values, filled 128 bulk waiters,
and verified a manifest through Gantry while bulk was saturated. It then held
four small-object Values and filled 16 small-object waiters. Each cycle observed
144 total waiters, 69 open connections, and exactly 40 new rejections (32 bulk
plus eight small-object). HEAD succeeded with both data queues full. All 184
submitted waiting/overflow calls returned after rejection/cancellation. All three
pool active counts and all queue depths returned to zero; three reusable idle
connections remained after the recovery pull.

In the primary 64-worker follow-up, manifest-at-bulk-saturation times were
0.684 / 0.573 / 0.632 ms; HEAD times were 0.451 / 0.358 / 0.436 ms. Verified
recovery pulls completed in 68.059 / 57.340 / 62.520 ms. Rust RSS was
196,308,992 bytes after every cycle. Final metrics show 9 fills/publications,
9,586 memory hits, zero overloads/corrupt misses, readiness 1, and zero active
requests/fills/pending disk writes. The 192 Rust errors match deliberately
abandoned bulk bodies. These remain sampled/short-run observations, not a hard
RSS bound or long-duration leak proof.

The follow-up test binary SHA-256 is
`2c809ff7aa0f771b53317462569f3fb59264909140699fa85dcbc04f3b433626`;
the Rust binary hash is unchanged from the initial report. Rebuild commands
completed before measurements. Scoped package tests/lint and shell syntax passed.
All updated-pool paced runs completed their mixed phases without a cold-layer
503. The synchronized manifest admission failures and the insufficient-pipe
pressure attempt remain recorded, not discarded as passing measurements.

The registry fixture still honors Range and returns partial content; therefore
these runs **do not independently exercise the newly accepted complete-200 small
manifest origin response**. Likewise, four origin HEAD slots are present but not
saturated here. Their implementation changes are included in the rebuilt SDK,
not separately certified by these performance observations.

## Final revision: 128 small-object waiters and rebuilt Rust reclamation

This section supersedes the 16-waiter follow-up for current defaults. The SDK now
defaults to four small-object connections and **128** small-object waiters
(`pkg/racersdk/client.go:93-99`), independently of the 128 bulk and 16 metadata
waiters. The Rust executable was rebuilt from the current worktree after the
writeback-staging reclamation changes; the writer's reclamation seam is at
`cmd/racer-dataplane/src/store/writer.rs:146-154`. The reported 803-test Rust suite
result is supplied by the implementation owner, not independently rerun here.

One build invocation produced all final measurement executables. No compilation
was initiated by this benchmark task during measurement. Versions and SHA-256
identities from `tmp/racer-performance/build-info.txt`:

```text
go1.26.6 linux/amd64
rustc 1.96.0 (ac68faa20 2026-05-25)
Go harness: 96c3c0476be92d74dba8333e6f7af3ab3a9b6d265794695e24ceeacb0704e4eb
Rust:       defd32868efc08413e494aac6820b963e815ffa29047f1625ba514eb8fa60095
```

### All final-batch attempts and conditions

All three completed attempts used the same binaries, default SDK configuration,
`start_spacing=0s`, and `manifest_bulk_control=false`. The Rust harness pipe limit
remained 72, as disclosed in the previous follow-up. There were no pacing, retry,
prewarming, or consumer-verification concessions.

1. `run-kNOTyP`, 32 workers: **passed**, cold/warm 2,037.9/2,781.0 MiB/s,
   warm manifest p99 9.477 ms. External Rust compilation and Go linking began
   after preflight and were recorded in `build-overlap.log`. Retained as a
   contended attempt, not selected as the primary sample.
2. `run-UrQjpi`, 64 workers: **passed**, empty build-overlap log.
3. `run-txlYvB`, 32 workers: **passed**, empty build-overlap log.

Between attempts, several invocations waited for ongoing compiler/e2e processes;
some waits expired or the runner refused a newly starting build before launching
the workload. Those are preflight deferrals, not failed benchmark trials. No
completed final-batch trial failed, and no cold manifest or layer 503 occurred.
The empty logs are one-second sampled build checks, not exclusive-host isolation;
background clusters and services still existed. No failed historical attempt was
deleted or reclassified.

Exact measurement commands, after the single build:

```sh
bash cmd/racer-loadgen/performance/run.sh run 32
bash cmd/racer-loadgen/performance/run.sh run 64
# Repeat 32 after the first attempt recorded external compilation:
bash cmd/racer-loadgen/performance/run.sh run 32
```

### Final unpaced measurements

| Run | Workers | Phase | Useful MiB/s | HEAD p95 / p99 ms | Manifest p95 / p99 ms | HEAD / manifest samples |
| --- | ---: | --- | ---: | ---: | ---: | ---: |
| `run-txlYvB` | 32 | Cold | 2,179.0 | 7.810 / 18.085 | 7.960 / 33.980 | 101 / 99 |
| `run-txlYvB` | 32 | Warm | 2,951.1 | 7.097 / 7.781 | 7.260 / 8.482 | 1,348 / 1,339 |
| `run-UrQjpi` | 64 | Cold | 2,450.4 | 18.726 / 21.868 | 17.792 / 38.130 | 105 / 95 |
| `run-UrQjpi` | 64 | Warm | 2,653.2 | 20.713 / 21.760 | 19.217 / 25.244 | 1,359 / 1,314 |

Both phases of both primary runs explicitly passed checks for **zero admission
rejection and timeout deltas**, zero errors, and the exact requested verified
image-byte count. Cold/warm useful bytes were 1,610,696,320 / 32,213,926,400 at
32 workers and 3,221,392,640 / 64,427,852,800 at 64. Warm durations were 10.410 s
and 23.159 s respectively.

Compared with the original unpaced 64-worker primary (`run-RiD1v0`), warm manifest
p99 decreased **913.165 -> 25.244 ms, 97.2% lower**. This is now an unpaced-to-
unpaced workload comparison, though the SDK, Rust implementation, and pipe budget
changed together and the shared host was not exclusively controlled. Warm useful
throughput decreased 3,376.2 -> 2,653.2 MiB/s (21.4%); do not claim throughput
improvement or unchanged performance. At 32 workers it was essentially unchanged
(2,956.3 -> 2,951.1 MiB/s), while manifest p99 was 7.380 -> 8.482 ms. The result
establishes the 64-worker latency improvement and successful bursts, not universal
latency improvement or a production capacity claim.

Origin accounting remained identical: one 50,334,260-byte unique image fetched
per cold burst, seven origin requests, and zero warm requests/bytes. Cold resume
delivered 33,556,473 bytes with 33,556,480 origin bytes, or 1.0000002086x; warm
resume again fetched nothing from origin. All resumed full-object digests matched.

### Updated queue/memory/recovery evidence

The harness now checks small-object queue <=128 separately from metadata queue
<=16. The full configured queue bound is 128 + 128 + 16 = **272**; the body-pool
pressure workload deliberately saturates 256 of those entries, leaving metadata
admission available. Total SDK connection bound remains **72** (64 + 4 + 4).

In each of three pressure cycles per run, 64 bulk and four small-object Values
were held; 160 bulk and 136 small-object calls were submitted. Exactly **40**
overflow rejections occurred (32 bulk + eight small-object), with **256 queued**
calls and **69 open connections** observed. After cancellation all **296**
submitted calls returned; every active-pool and queue gauge returned to zero,
with three reusable idle connections after the verified recovery pull. Queue
timeouts stayed zero. Deliberate pressure rejections are separate from the zero
mixed-load rejection counts.

| Observation | Final 32 | Final 64 |
| --- | ---: | ---: |
| Warm peak Go heap | 8,560,272 bytes | 14,090,568 bytes |
| Warm peak Go RSS | 28,590,080 bytes | 36,241,408 bytes |
| Warm peak Rust RSS | 126,291,968 bytes | 127,377,408 bytes |
| Mixed peak small-object queue (cold / warm) | 9 / 8 | 44 / 41 |
| Mixed peak total connections | 37 | 69 |
| Manifest at bulk saturation, three cycles | 0.528 / 5.613 / 0.697 ms | 4.101 / 5.601 / 1.442 ms |
| HEAD at bulk saturation, three cycles | 0.255 / 0.353 / 0.248 ms | 0.324 / 0.265 / 0.290 ms |
| Verified recovery pull, three cycles | 44.275 / 43.627 / 42.292 ms | 51.065 / 46.631 / 62.518 ms |
| Rust RSS after recovery | 196,739,072 bytes each cycle | 196,952,064 bytes each cycle |

HEAD also succeeded with both body queues full. Final 64-worker Rust metrics
record nine fills and nine disk publications, zero overloads/corrupt misses, and
readiness 1. Active requests/fills/pending writes drained to zero. The 192 request
errors remain the intentionally abandoned 3 x 64 bulk bodies, not mixed-load
errors (`run-UrQjpi/final-rust.prom:1-50`). Sampled peaks and three recovery cycles
are not proof of a hard RSS ceiling or long-duration leak freedom.

The final runner no longer references the ignored Go wrapper. Fresh-checkout
instructions in **Exact reproduction** use standard Go 1.26.6 on PATH; the
optional `GOCMD` executable and caller-supplied caches are supported. Shell syntax,
scoped golangci-lint, loadgen package tests, and `git diff --check` passed. All
changes remain in owned new performance files and this report; no commit made.

## Interpretation and remaining evidence gaps

The existing SDK copy-benchmark result supplied by the task (90% at all sizes) is
separate evidence. This harness does not rerun it or establish a production
throughput ratio. It adds a real process/cache/mirror workload and exposes small
GET queue latency that a copy benchmark cannot measure.

Memory evidence is observational: compare repeated cycles and memory/resource
gauges, not a claim of a hard process RSS bound. Rust exposes active requests,
fills, and pending writes but not internal queue high-water marks in the sampled
metrics (`cmd/racer-dataplane/src/telemetry/metrics.rs:81-107`). The harness requires
those three gauges to drain and checks SDK admission recovery. A many-key working
set exceeding cache capacity, randomized large-object sizes, long-duration leak
testing, multiple metadata workers, fixed-rate arrivals, and independent remote
machines are needed for broader performance conclusions.

Metadata is closed-loop, so these percentiles describe this offered workload,
not open-loop tail latency at a specified request rate. Cold bursts have few
manifest samples at 64 workers; their p99 can simply be the maximum. No latency
SLO was supplied, so the integration enforces correctness and recovery, not an
arbitrary throughput or latency threshold.

Early setup attempts failed readiness because scratch directories inherited
group-write permissions and initially belonged to the invoking user. A file-only
strace showed repeated checks of the `0775` client directory; the production
endpoint owner rejects group/world-writable directories and wrong ownership
(`cmd/racer-dataplane/src/client/ownership.rs:16-18`). The runner now creates secure
root-owned runtime directories in its private namespace. No measured run uses
strace, and no dataplane source change was required.
