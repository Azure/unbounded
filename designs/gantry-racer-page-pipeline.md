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

## Phase 3 implementation handoff

Implementation commit: `2b292fb4` (`perf(racer): serve admitted pages with
pool-bounded early buffers`). This phase owns only `cmd/racer-dataplane` Rust
files and this tracked design. The SDK/Gantry work was committed concurrently
by its owner. No subagent API was available in the phase 3 harness.

### Readiness and physical ownership

- `cache.rs:2078-2125` still completes full receive/semantic/CRC validation and
  bounded slab admission before reaching `Loading::Publishing`. That state
  prefers `ReadLease::ready()` and otherwise attempts the early budget
  (`cache.rs:2299-2335`). There is no cut-through and no new payload copy.
- `buffers.rs:311-313,758-778` bounds early retention to
  `min(N / 2, N.saturating_sub(4))` **physical slots per NUMA pool**. Four-slot
  minimum pools therefore use write-before-forward; the default eight-slot pool
  allows four early slots. This conservative policy preserves four demand slots
  and at least half of larger pools. It is not a per-cache or per-worker quota.
- The slot flag and NUMA atomic counter charge a shared page once, including
  coalesced consumers, local lookups, cross-worker `ComputeRead` capabilities,
  and replica writes. Release occurs only at the final slot-reference decrement
  (`buffers.rs:352-368`). Cancellation, eviction, cache shutdown, and logical
  response completion cannot return the permit while any kernel/compute/flight
  owner remains. Slab completion alone does not release slow-reader ownership.
- Forwarding rank selection uses demand capacity after subtracting the early
  limit (`buffers.rs:412-417`, `cache.rs:1873-1877,2457-2462`). Keeping the old
  N-1 rank would let one slow reader block all new highest-rank demand. Four
  demand slots preserve the existing minimum-pool three-hop contract.
- A denied consumer records one budget fallback and waits for a file without
  repeatedly competing for permits. Local hits use the same gate. Flight
  consumers retain the charged terminal buffer, then perform their own local
  admission and file-first selection. No unbounded terminal-value directory is
  introduced. Retired unpublished fallback leases fail locally instead of
  waiting for a write that eviction prevented (`cache.rs:2311-2334`).
- A real multi-page regression exposed completed SEND_ZC notification owners
  retained by an idle body writer while the next page waited for receive space.
  `handlers.rs:1593-1598` now reaps those completions through
  `http_server.rs:1256-1260` before attempting prefetch. Terminal notifications,
  not the early send result, still govern resource retirement.
- UDS buffer bodies use ordinary SEND, not SEND_ZC
  (`http_server.rs:1727-1740`). Early UDS delivery trades the written-hit
  file/splice path for a kernel socket copy from the existing immutable buffer.
  TCP buffers use SEND_ZC; TLS retains its existing encrypted transport behavior.
  Written hits continue to return File values for splice/sendfile dispatch.
- `allocator/checkpoint.rs:125-139` sets `written` only after exact successful
  payload-write completion. A later disk error cannot retract bytes already
  delivered successfully, and cannot falsely publish an unwritten file. Existing
  shard poisoning and containment remain authoritative; checkpoint durability
  is still separate from both response readiness and payload-write completion.

### Fixed-series measurements

`metrics.rs:38-81,244-273,559-583` adds:

- `racer_dataplane_page_serve_total{decision="early_buffer|file|budget_fallback"}`:
  per-consumer decisions, not bytes successfully sent. Fallback counts once before
  waiting and can be followed by a file decision. Explicit RDMA materialization
  retains its existing behavior and is not counted as a File response.
- `racer_dataplane_page_stage_total` and
  `racer_dataplane_page_stage_seconds_total`, with fixed `stage` and `outcome`
  labels. Stages are `receive_buffer`, `fill_validation`, `admission`,
  `consumer_ready`, and `payload_write`. The first four observe producer work,
  not every joined consumer; `fill_validation` includes upstream fill plus
  checksum queue and validation. `consumer_ready` starts after admission.
  Interrupted producer stages report `incomplete`; successful transitions report
  `completed`. Payload-write timing is per admitted local extent from admission
  to exact successful write collection, including queueing, and reports only
  completed writes. Its incomplete series remains zero; storage failures use the
  existing quarantine/failure counters. No duration is checkpoint durability.

These use worker-private counters and periodic snapshot publication. No
per-request labels, polling timers, or shared gauge summation were added.

### Deterministic coverage

- `tests/storage/early_serving.rs:33,89,138,176`: paused-storage early delivery,
  shared/local consumers, pointer identity/no copy, CRC retention, sticky budget
  fallback, demand capacity, incomplete/invalid receive and rejected admission.
- `tests/storage/early_serving.rs:210,257,386`: actual UDS delivery while slab
  submission is paused; canceled submitted send and abandoned submitted write
  retain the early budget until driver shutdown/collection proves quiescence.
- `tests/storage/early_serving.rs:300,322`: retired fallback termination and
  asynchronous checksum plus cross-cache single-flight replica admission.
- `tests/storage/buffer_lifetimes.rs:11,51,83`: tiny/default pool limits,
  cross-thread completion, terminal consumers, and concurrent worker admission.
- `tests/storage/allocator.rs:46`: injected publication-completion EIO keeps
  delivered immutable bytes valid, poisons the shard, and never sets FileReady.
- Existing persistence/recovery fixtures explicitly select write-before-forward
  so their disk assertions continue to test storage. Their independent reads now
  allow independent completion turns rather than assuming simultaneous CQEs.
  Existing identity, authorization, precondition, four-buffer multi-hop, and
  multi-page TCP tests run against the production early-serving policy.

### Verification commands and results

All paths below are worktree-relative unless absolute. Test runs use
`TMPDIR="$PWD/tmp" RUST_TEST_THREADS=2 RACER_REQUIRE_URING=1`; io_uring was
available and required, not silently skipped. Compilation reused
`CARGO_TARGET_DIR=/home/azureuser/code/unbounded/cmd/racer-dataplane/target`.
The library executable was copied to `tmp/phase3-rust-tests` before runtime
checks because concurrent main-checkout builds replace the shared-cache binary.
One apparent fast compilation after a main-checkout build returned the baseline
binary; its one-test early filter result was discarded, the owning Rust source
was updated, and the worktree was rebuilt and verified with all 14 early tests.

- `timeout -k 5s 180s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --locked --all-targets --no-run`:
  passed, library, main and both default benchmark targets compiled.
- `timeout -k 5s 60s tmp/phase3-rust-tests early_ --nocapture`:
  14 passed, no ignored (13 new contracts plus one existing RDMA state test).
- `timeout -k 5s 120s tmp/phase3-rust-tests cache::tests:: --nocapture`:
  25 passed, two child helpers ignored in the outer suite and executed/passed by
  their wrapper tests. This run preceded the final additional write-owner test,
  which passed in the 14-test filter above.
- `timeout -k 5s 60s tmp/phase3-rust-tests buffers::tests:: --nocapture`:
  15 passed. The subsequent four-demand-slot adjustment was rechecked by the
  early filter and saturated handler suite.
- `timeout -k 5s 120s tmp/phase3-rust-tests allocator::tests:: --nocapture`:
  32 passed, one throughput benchmark ignored.
- `timeout -k 5s 120s tmp/phase3-rust-tests handlers::tests:: --nocapture`:
  36 passed, five ignored (four subprocess helpers executed by wrappers, one
  production-duration rotation lane intentionally not run).
- `timeout -k 5s 60s tmp/phase3-rust-tests metrics::tests:: --nocapture`:
  nine passed, including fixed stage/decision series and periodic publication.
- `timeout -k 5s 120s tmp/phase3-rust-tests http_server::tests:: --skip encrypted_http_kernel_integration --nocapture`:
  11 passed, five child helpers ignored in the outer suite; applicable wrappers
  executed their children, including UDS slab replacement and lifecycle tests.
- `timeout -k 5s 120s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --locked --doc`:
  73 passed (four ordinary and 69 compile-fail ownership contracts).
- `timeout -k 5s 60s make racer-dataplane-fmt-check`: passed, including explicitly
  included tests. `git diff --check`: passed.
- `GOTOOLCHAIN=go1.26.6 TMPDIR="$PWD/tmp" GOTMPDIR="$PWD/tmp" timeout -k 5s 120s make fmt GO_PACKAGE_PATTERNS=./internal/version/... GO_PACKAGE_DIRS=./internal/version`:
  passed, zero issues and no Go changes. This scopes the required formatter away
  from the concurrent SDK/Gantry owner's files.

Known baseline failure: running the complete HTTP-server filter or the exact
`http_server::tests::tls_transport::encrypted_http_kernel_integration` test fails
at `tests/http/tls.rs:21` (kTLS TX counter 4 versus expected 2). Rebuilding the
unchanged main checkout at `97febafe` with its own manifest, snapshotting its
executable as `tmp/phase3-baseline-tests`, and running the same exact test under
`timeout -k 5s 60s` reproduces the identical failure. It is not counted as passed
or an unavailable prerequisite. The full hardware RDMA, strict kTLS, throughput,
and combined kind/Go pipeline lanes were not run in phase 3; phase 4 owns combined
verification. These tests establish ordering and ownership, not production
throughput improvement.

## Phase 4: combined verification

Integration commit: `a66ee991`. Phase 4 did not delegate further. The parent owns
the independent production review and parent-branch merge.

### Added acceptance coverage

- `internal/gantry/mirror/racer_pipeline_test.go:47`: real TCP Gantry mirror,
  real UDS SDK with the production `PageLookahead: true` option, and deterministic
  HTTP fixture. The current body is gated until the next GET arrives, so a
  sequential implementation cannot pass. Checks pinned authorization/encoding,
  exact cross-page range, bytes, digest/ETag/type headers, pre-body 503 retry,
  forbidden/version/truncated later responses without replay, and final stats.
  Ten disconnects exceed the eight speculative slots, with both upstream
  requests canceled before reuse and Gantry admission restricted to one transfer.
- `internal/gantry/mirror/racer_pipeline_test.go:226`: a patterned 64 MiB + 31
  byte object, full SHA-256 verification and twelve concurrent cross-page range
  consumers, exact status/length and SDK byte/request accounting. The HTTP fixture
  uses `http.ServeContent`; it does not duplicate SDK scheduling or validation.
- `cmd/racer-dataplane/tests/http/page_pipeline.rs:14`: actual origin TCP,
  production cache/handler/server and UDS client, eight physical pool slots.
  Slab token admission is paused before the request. Exact 206 headers and bytes
  arrive with one early-buffer decision, zero completed payload writes and no
  additional slab operations. After releasing storage, background polling
  completes publication and the second response records file service. Shutdown
  verifies all physical pool slots recovered. This complements the existing
  submitted-send/write cancellation ownership tests; it does not call SEND on a
  preconstructed buffer as a substitute for exercising the handler.
- `tests/storage/flight_admission.rs:261` now explicitly selects file readiness
  for its published-file/rank-capacity contract. Its former immediate File
  assertion failed under the approved early-buffer semantics.
- `internal/racer/socket_test.go:14` uses a short temporary name. Go's full test
  name in `t.TempDir` exceeded the 107-byte UDS limit even under the project-local
  `/home/azureuser/code/unbounded/tmp` directory. No production socket validation
  changed.

### Environment and bounded commands

Commands ran from this worktree. Go used `GOTOOLCHAIN=go1.26.6`,
`TMPDIR=/home/azureuser/code/unbounded/tmp`, and `GOTMPDIR="$PWD/tmp"`.
Rust runtime used the same short TMPDIR, `RACER_REQUIRE_URING=1` and
`RUST_TEST_THREADS=2`. Host kernel: `7.0.0-30-generic`; vendored OpenSSL reported
`3.6.3 9 Jun 2026`. Required io_uring tests executed, rather than skipping.

Go commands (all passed after the socket-fixture correction):

```sh
timeout -k 5s 120s go test ./pkg/racersdk/... ./internal/gantry/... ./cmd/gantry/... -count=1 -timeout=90s
timeout -k 5s 120s go test -race ./pkg/racersdk/... ./internal/gantry/... ./cmd/gantry/... -count=1 -timeout=90s
timeout -k 5s 120s go test -race ./api/racer/... ./internal/racer/... ./internal/operator/components/racer/... ./cmd/racer-loadgen/... -count=1 -timeout=90s
timeout -k 5s 60s go test -race ./internal/racer/... -count=1 -timeout=45s
timeout -k 5s 60s go test -race ./internal/gantry/mirror -run '^TestRacerPipelineBoundaries$' -count=5 -timeout=45s
timeout -k 5s 60s go test -race ./internal/gantry/mirror -run '^TestRacerPipeline' -count=1 -timeout=45s
```

The broader Racer Go command initially failed only `TestPrepareSocketDirectory`;
its other packages passed. The focused full `internal/racer` race rerun passed
after the short-name correction. The final mirror rerun includes the added
status and total-byte assertions.

Both crates compiled with `timeout -k 5s 180s cargo test --manifest-path
cmd/racer-{dataplane,controlplane}/Cargo.toml --locked --all-targets --no-run`,
each using its respective `/home/azureuser/code/unbounded/cmd/racer-*/target`
as `CARGO_TARGET_DIR`. Dataplane executable snapshots are `tmp/phase4-rust-tests`,
`tmp/phase4-rust-main`, `tmp/phase4-rust-crypto`, and `tmp/phase4-rust-http`.
Snapshots avoid replacement by concurrent main-checkout builds.

The complete default dataplane library test inventory was executed in bounded
partitions. Each command below has prefix `timeout -k 5s 120s
tmp/phase4-rust-tests`; omitted tests were either run separately or are the two
explicit failures listed below. Child helpers ignored in outer suites execute
under their owning wrapper where applicable.

| Arguments | Result |
| --- | --- |
| `allocator:: buffers:: cache:: --skip creation_recovery_and_replacement_share_setup_accounting` | 104 passed, 5 ignored |
| `control:: --skip topology_reload_retains_wire_epoch_and_rejects_unknown_or_malformed_cursors --skip production_compiler_product_snapshots` | 69 passed, 2 ignored |
| `handlers:: http_client:: http_server:: --skip page_pipeline --nocapture` | 67 passed, 16 ignored |
| `--exact handlers::tests::page_pipeline::early_handler_delivers_http_before_storage_then_serves_written_file --nocapture` | 1 passed |
| `runtime:: --nocapture` | 35 passed, 3 ignored |
| `--skip allocator:: --skip buffers:: --skip cache:: --skip control:: --skip handlers:: --skip http_client:: --skip http_server:: --skip runtime:: --skip product_v1_corpus_preserves_membership_ownership_and_rejects_malformed --skip actual_rust_compiler_snapshots_prepare_and_retain_last_good` | 159 passed, 6 ignored |

Three compiler-conformance tests initially failed because direct execution did
not provide this run's compiler export. All three passed through the existing
harness with fresh production compiler receipts, after:

```sh
timeout -k 5s 180s python3 hack/scripts/racer-test.py --build-timeout 150 --test-timeout 120 compile controlplane dataplane
timeout -k 5s 120s python3 hack/scripts/racer-test.py --test-timeout 60 selected dataplane control::product_routing::tests::production_compiler_product_snapshots
timeout -k 5s 120s python3 hack/scripts/racer-test.py --test-timeout 60 selected dataplane conformance::product_v1_corpus_preserves_membership_ownership_and_rejects_malformed
timeout -k 5s 120s python3 hack/scripts/racer-test.py --test-timeout 60 selected dataplane endpoint_tests::actual_rust_compiler_snapshots_prepare_and_retain_last_good
```

These harness commands used `RACER_CARGO_TARGET_DIR` and
`RACER_CONTROLPLANE_CARGO_TARGET_DIR` pointing to the caches above. Dataplane
main: 14 passed, 3 ignored; both default benchmark executables: zero tests,
successful execution, each under `timeout -k 5s 60s`. Dataplane doctests:
`timeout -k 5s 120s cargo test --manifest-path cmd/racer-dataplane/Cargo.toml
--locked --doc`: 73 passed, including 69 compile-fail ownership contracts.

The control-plane all-target command under 120 seconds passed lib (30, 2
ignored), main (0), core (14), placement (7, 1 ignored), placement_export (1),
runtime (17), and security (17), then hit the external deadline in service.
Service alone also exceeded 120 seconds, so it was partitioned using the emitted
`target/debug/deps/service-661ceece5d83a0d9` executable:

- `timeout -k 5s 120s <service> --skip canceled_committed_route_publication_retries --skip canceled_route_publication_retries_and_repairs_removed_label`: 15 passed.
- `timeout -k 5s 120s <service> canceled_committed_route_publication_retries canceled_route_publication_retries_and_repairs_removed_label`: 2 passed.
- `timeout -k 5s 120s cargo test --manifest-path cmd/racer-controlplane/Cargo.toml --locked --doc`: passed, zero doctests.

Thus every default control-plane target completed across bounded runs. The
existing unused `Publication::published` test-support warning remains.

### TLS artifact diagnosis and remaining failures

The previously reported `tests/http/tls.rs:21` failure was **not valid baseline
source evidence**. Both saved phase-3 executables linked a stale native shim
from the shared build cache. `objdump -d --disassemble=racer_tls_configure
tmp/phase3-rust-tests` shows unconditional `SSL_OP_ENABLE_KTLS` and no use of the
second argument. Worktree source `src/tls_native.c:62-66` instead clears the bit
and enables it conditionally. The freshly rebuilt executable's disassembly
contains that clear and branch; no TLS source was changed. `OPENSSL_CONF=/dev/null`
did not fix the old executable. Rebuilding the native artifact did.

Both commands now pass with real offload required:

```sh
RACER_REQUIRE_KTLS=1 timeout -k 5s 60s tmp/phase4-rust-tests --exact http_server::tests::tls_transport::encrypted_http_kernel_integration --nocapture
RACER_REQUIRE_KTLS=1 timeout -k 5s 60s tmp/phase4-rust-tests --exact tls::tests::tls13_key_update_with_actual_ktls --nocapture
```

Two unrelated default dataplane tests still fail, including when rerun together
under `timeout -k 5s 60s tmp/phase3-baseline-tests`:

- `allocator::setup_tests::slab_io_setup::creation_recovery_and_replacement_share_setup_accounting`: `tests/storage/slab_io_setup.rs:41`, actual `(25, 90112)`, expected `(13, 40960)`.
- `control::tests::topology_reload_retains_wire_epoch_and_rejects_unknown_or_malformed_cursors`: `tests/control/configuration.rs:68`, actual `503`, expected `502`.

Their identical failures in the saved baseline are recorded, with the caveat
above about that executable's native TLS artifact. They are not counted as
passing or suppressed in source. Default dataplane aggregate: 438 passed,
2 failed, 32 ignored across partitions, plus 14 main tests and 73 doctests.

### Formatting and scope of evidence

Passed `timeout -k 5s 120s make fmt` separately with
`GO_PACKAGE_PATTERNS=./internal/gantry/mirror/... GO_PACKAGE_DIRS=./internal/gantry/mirror`
and `GO_PACKAGE_PATTERNS=./internal/racer/... GO_PACKAGE_DIRS=./internal/racer`.
Both reported zero lint issues. Passed `timeout -k 5s 60s make racer-fmt-check`
and `git diff --check`.

The full live Gantry + Go SDK + Rust dataplane + control-plane deployment was
not run. The accepted local minimum is covered by two real-socket integrations:
Gantry/SDK with a deterministic Racer-protocol fixture, and the production Rust
origin/cache/HTTP/UDS path with storage paused. These tests do not establish
production throughput, end-to-end deployed latency, hardware RDMA behavior,
opt-in NUMA placement, scale campaigns, or the ignored production-duration
timing lanes. No throughput improvement is claimed. Phase-4 core acceptance is
implemented; parent merge readiness is conditional on the independent review
and explicit acknowledgment of the two remaining baseline-suite failures.
