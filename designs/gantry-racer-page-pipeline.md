# Gantry/Racer page pipeline: measurement and verification

## Approved direction and phase boundaries

Phase 1 adds SDK measurements and deterministic test foundations. Phase 2 adds
opt-in `PageLookahead`: one current page plus one prefetched page on separate UDS
connections, sharing a bounded nonblocking speculative budget across credential
views. Keep pinned per-page pre-body retries, ordered failures, cancellation,
and bounded scratch without Go page buffers.

Phase 3 serves validated, admitted immutable buffers before slab payload write,
with a pool-derived budget whose ownership lasts through actual I/O completion.
Prefer a ready file and fall back to write-before-forward under pressure. The
user approved this changed completion semantics; preserve storage-failure
containment. Phase 4 verifies the combined path. No production throughput or
latency improvement is established by phase 1 tests.

The pre-change path waits for a file: `cache.rs:2054-2085` admits the validated
buffer and transitions to `Loading::Publishing`; `cache.rs:2240-2254` waits for
`ReadLease::ready`. `allocator.rs:558` gates that on payload `written`, set after
exact write completion in `allocator/checkpoint.rs:125-132`. This is distinct from
checkpoint durability. The user's original, untracked `designs/racer-dataplane.md`
describes validated shared buffers generally (lines 168 and 297) and separately
describes checkpoint durability (lines 386-390); it is not evidence that early
serving is already implemented. Those user files are not part of this change.
Rust paths in this document are relative to `cmd/racer-dataplane/src/` unless
prefixed with `tests/`.

## SDK measurement contract

`pkg/racersdk/stream.go` defines additive `TransferStats` fields:

| Field | Meaning |
| --- | --- |
| `PageRequests int64` | GET write attempts, including retries/failed writes; excludes HEAD and pre-write connection failures. |
| `PageRetries int64` | Subset of requests dispatched after a retryable rejected pre-body response. Canceled backoff is not a dispatched retry. |
| `PageHeaderWait time.Duration` | Consumption-path wait for page preparation, including connection setup, validation, retry sleeps and failures; includes `Prepare`. |
| `ForwardDuration time.Duration` | Active `WriteTo` time outside header preparation, including body supply, downstream backpressure, copying/splicing and cleanup. |

Byte/syscall fields keep their existing meaning. Stats are cumulative and use
the existing stream lock. They are final-operation snapshots, not live progress
polling. Empty streams stay zero. No per-page arrays, labels, target strings, or
per-splice clock calls are added. Sum the two durations for active consumption
time, including `Prepare`, not HEAD or caller idle time. The forwarding duration
must not be presented as downstream-only blocking.

Phase 2 must preserve **critical-path** header wait: measure only the consumer's
wait for the prefetched result, not speculative wall time overlapping forwarding.
Merge request/retry counters from the prefetch worker without racing Stats.
Canceled/discarded speculative attempts still count when dispatched. Keep
`ForwardDuration` subtraction local to each `WriteTo` so earlier `Prepare` time
is not subtracted twice. An extra aggregate attempt-duration field is unnecessary
unless a concrete diagnostic use appears.

## Reuse existing Racer measurements

| Existing evidence | Useful interpretation and limit |
| --- | --- |
| `metrics.rs:426-447`: cache lookups and upstream request counters | Distinguish metadata/page hits, misses, coalescing and upstream attempts. They do not measure latency. File hits can be OS page-cache hits. |
| `metrics.rs:496-533`: allocator rejection, checkpoint and pressure series | Fixed rejection reasons, checkpoint prepared/completed counts, current phase gauges and pending/publishing/charged pressure. Completed checkpoint means final sync, not first body byte. |
| `metrics.rs:535-572`: resource exhaustion, HTTP failure/abort, quarantine counters | Terminal outcomes; exhaustion is not wait duration or total backpressure events. |
| `slab_io.rs:269-303`: `racer_dataplane_slab_io_{operations,bytes,waits,wait_seconds}_total` | Shared logical limiter accounting; zero when unlimited. Token wait is not device latency, and sums may exceed wall time across concurrent operations. |
| `allocator.rs:553-556`, `allocator/checkpoint.rs:54-130` | Publication diagnostic stages `punch`, `write_admission`, `write`, `written`, stage age and ticket identity. Reuse these for storage-stall attribution. |
| `cache.rs:1037-1093`, `failure_diagnostics.rs:171-211` | Sampled fault state, buffer-wait/network-flight flags, publication evidence, deadlines. Bounded samples are failure evidence, not distributions of successful latency. |

Worker counters use private cells and periodic publication (`metrics.rs:174-189`,
`279-301`). Retain that pattern. Tests assert fixed allocator/exhaustion series
and aggregation (`tests/metrics.rs:53-123`, `153-199`). Do not introduce digest,
offset, request, credential, or raw-error labels.

### Missing measurements for phase 3

Add only bounded aggregate stage count/time measurements needed to separate:

1. Receive-buffer acquisition wait.
2. Upstream fill and checksum queue/validation completion.
3. Payload admission wait.
4. Validated/admitted-to-consumer-ready versus validated/admitted-to-payload-write
   completion. Keep durable checkpoint completion separate using existing metrics.

Use a fixed enum of stages and terminal outcomes, measured at transitions rather
than each poll. Count producer work once, not once per coalesced consumer. Reuse
existing publication-stage diagnostics instead of creating a second diagnostic
pipeline. Early-buffer/file/fallback decision counts and a pool-budget occupancy
gauge can explain whether early serving actually ran. Successful stage timing is
currently missing; do not infer it from exhaustion counters or sampled failures.

## Deterministic controls and verification handoff

- `pkg/racersdk/stream_stats_test.go`: `scriptedPage.beforeHeaders` and
  `beforeBody` can block on channels or sleep under `testing/synctest`.
  `scriptedStream` uses `net.Pipe`, production HTTP parsing/validation, and a
  16-byte interval crossing a real 64 MiB boundary. `delayedWriter` independently
  delays or rejects writes. Exact fake-clock assertions cover implicit/explicit
  preparation, repeated Prepare, two pages, upstream body delay, downstream delay,
  header rejection, cancellation, and snapshot stability. The fixture is for
  portable forwarding; real UDS/TCP splice tests validate production sockets.
- `pkg/racersdk/page_retry_test.go` asserts request/retry totals and backoff
  attribution for first/later-page retries and terminal failures.
- Reuse Rust `tests/storage/cache_persistence.rs:812-831,964-974`: `Fake`,
  `Reply::Hold`, and `release` gate upstream completion without sleeping. Its
  recorded starts, validations, and retained destinations support ordering and
  lifetime checks.
- Reuse `slab_io::Io::testing` and `exhaust_and_pause_refill`
  (`slab_io.rs:101-139`): the RAII guard freezes exhausted tokens; dropping it
  resumes refill without accumulating credit for the paused interval. This gates
  **submission**, not completion of already-submitted I/O. Do not describe it as
  a delayed completion fixture.
- `tests/http/uds_slab.rs:171-204` already proves frozen shared storage admission,
  no operation-count growth, a rate-queued file splice, and no sent payload.
  `tests/storage/slab_io.rs:83-132` covers failure/retry accounting and stop during
  token wait. Reuse these controls for phase 3 early-response-before-write tests.
- For actual completion ownership, separately retain driver tickets/buffer owners
  after consumer cancellation and assert no pool-slot reuse until completion.
  Phase 3 must add that assertion for its new shared early-serving budget. A
  token gate alone cannot establish the lifetime of already-submitted writes or
  zero-copy send notifications.

For phase 2 compare sequential and lookahead modes with header and downstream
gates, exact bytes/order, short final pages, rejected later pages, cancellation,
credential-view shared capacity, and unavailable speculative budget. For phase 3
gate slab submission after validation/admission, prove early delivery, then
release storage and verify subsequent file service. Repeat with saturated budget,
validation/admission failure, storage error/quarantine, and an abandoned consumer.

Use external `timeout -k 5s 60s` (at most 120s for tests) and Go `-timeout`.
Keep temporary paths inside the worktree with `TMPDIR`/`GOTMPDIR`; a short socket
directory is required by the Unix address limit. Real-kernel tests must distinguish
unavailable io_uring prerequisites from passing coverage.

## Phase 1 verification

Run from this worktree with `GOTOOLCHAIN=go1.26.6 TMPDIR="$PWD/tmp"`
and `GOTMPDIR="$PWD/tmp"`:

- `timeout -k 5s 60s go test ./pkg/racersdk -count=1 -timeout=45s`: passed.
- `timeout -k 5s 120s go test -race ./pkg/racersdk -run '^TestTransferStats|^TestPageRetry|^TestSpliceContentAndReuse' -count=1 -timeout=60s`: passed.
- After adding canceled-backoff accounting coverage,
  `timeout -k 5s 60s go test -race ./pkg/racersdk -run '^TestTransferStats' -count=1 -timeout=45s`: passed.
- `timeout -k 5s 120s make fmt GO_PACKAGE_PATTERNS=./pkg/racersdk/... GO_PACKAGE_DIRS=./pkg/racersdk`: passed, zero lint issues.

The initial formatter attempt under the host's Go 1.27.1 failed because installed
golangci-lint 2.11.4 was built with Go 1.26.5. Selecting the module's Go 1.26.6
resolved it. Rust instrumentation and test controls above were inspected, not
changed or executed in this phase; these results do not claim real-kernel Rust
or combined pipeline coverage.
