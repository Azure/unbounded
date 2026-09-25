# Racer SDK verification and measurements

Step 5, 2026-09-25. Linux amd64, Go 1.26.6, AMD EPYC 9V74,
default GOMAXPROCS 48. Measurements are local generated-byte UDS streams, not
storage, network, or Rust dataplane benchmarks. No new dependencies were added.

## Findings and coverage

- Fixed a lifecycle bug: Transport detaches its dial context from a request.
  `Client.Close` canceled Get but left an outstanding dial alive until its dial
  timeout. `pkg/racersdk/client.go:85-106` now attaches dials to client lifetime,
  including closing a connection returned after cancellation.
  `TestIntegrationCancelBlockedDial` demonstrates the failure before the fix and
  checks both Get completion and dial exit after it. Request cancellation still
  allows Transport's bounded in-flight dial to serve another request; closing the
  Client cancels it. The dial test uses the private dial seam to block deterministically.
- Removed redundant parsing of locally serialized, validated request values in
  `pkg/racersdk/wire.go:256-327`. Direct validation preserves the operation, pin,
  context, range, and aggregate-limit checks. Network heads still pass through
  the raw guards before net/http. Small pooled reads shed about 55 allocations and
  13 KiB per operation (4 KiB object: about 246 to 191 allocations, 32 to 19 KiB).
  Fuzz round trips and malformed private-value tests cover the changed serializer.
- `integration_test.go` exercises Client and the actual origin server over UDS:
  empty, 4 KiB, one page, short second page, and three pages plus a tail. An
  offset-dependent generator/sink verifies every byte without an object buffer.
  Larger ranges traverse a sequential fake dataplane that splits origin pages.
  Every origin call checks the exact key and both opaque fields, including
  interior spaces and high bytes. Tests check the pin, refreshed-expiry snapshot,
  new-version selection on a subsequent expired bootstrap, blocked-body context
  cancellation/Value.Close, full-frame reuse, and refusal to reuse unread bodies.
- Exact 32 KiB response-head acceptance and 32769-byte rejection are tested over
  raw UDS. Request-side exact bounds already have live tests in
  `origin_test.go:686-737`. This agrees with
  `cmd/racer-dataplane/CLIENT_ORIGIN_API.md:47-55`; net/http limits alone are not
  being treated as sufficient. Existing assertions also cover callback-error,
  short/long/final-error body Close-once, blocked probes, overload, header/idle/
  request/write timeouts, immutable size/tag rejection, and continuation cancellation.
  These were inspected rather than duplicated.
- Expired bootstrap responses can represent a zero-TTL revalidation admission;
  rejecting them locally would contradict `CLIENT_ORIGIN_API.md:75-79`. Tests
  therefore check admission and snapshot behavior, not a local expiry clock.

## Results

All final checks passed:

- `GOTOOLCHAIN=go1.26.6 PATH="$PWD/tmp/bin:$PATH" make fmt`
- `make lint`: golangci-lint zero issues and actionlint passed.
- `go test -race ./pkg/racersdk -count=1`: passed, including executable examples.
- `CI=1 make test`: full repository `go test -race ./...` passed.
- Each available fuzz target ran with `-run '^$' -fuzztime=15s -parallel=4`:
  `FuzzWireHead` (939,242 executions after the serializer change), `FuzzRange`
  (501,878), and `FuzzValidatedTypes` (582,605), with no failure.

Commands used these additional environment variables to keep scratch in the worktree:

```sh
export GOTOOLCHAIN=go1.26.6
export PATH="$PWD/tmp/bin:$PATH"
export GOCACHE="$PWD/tmp/go-cache"
export GOTMPDIR="$PWD/tmp/.go-tmp" TMPDIR="$PWD/tmp/.go-tmp"
export GOLANGCI_LINT_CACHE="$PWD/tmp/lint-cache"
export GIT_CEILING_DIRECTORIES="$PWD/tmp/.go-tmp"
```

Initial full-suite attempts found build scratch under the nonhidden `tmp/go-tmp`
while other Go commands ran, and Git tests discovered the enclosing worktree from
their temporary directories. Hidden scratch, a local scratch-module boundary for
the abandoned build output, and the Git discovery ceiling resolved those failures.
No production or unrelated test changes were required. An initial benchmark filter
accidentally selected 200 one-GiB iterations and timed out; final measurements use
anchored selections or the complete bounded matrix below.

## Benchmark method and representative results

```sh
go test ./pkg/racersdk -run '^$' \
  -bench '^Benchmark(ClientStream|OriginStream|ConcurrentStream|StreamingLiveHeap)$' \
  -benchmem -benchtime=100ms -count=3
go test ./pkg/racersdk -run '^$' -bench '^BenchmarkStreamingLiveHeap$' \
  -benchmem -benchtime=1x -count=1
go test ./pkg/racersdk -run '^$' \
  -bench '^BenchmarkClientStream$/^1073741824$/^sdk$/^fresh=false$/^Read32K$' \
  -benchtime=1x -memprofile=tmp/sdk-heap.pprof -memprofilerate=1 -o tmp/sdk-bench.test
go tool pprof -top -alloc_space tmp/sdk-bench.test tmp/sdk-heap.pprof
```

The matrix covers 0, 4 KiB, 16 MiB, and 1 GiB; fresh connections and warmed reuse;
4/32/256 KiB Read buffers and WriteTo; and concurrency 1/16. Both client paths use
the same stdlib server, generated bytes, response fields, and one or two requests.
The stdlib client deliberately omits SDK semantic validation. Reader/Writer fast
paths are disabled for Read-buffer comparisons. WriteTo uses bounded scratch on
both sides (stdlib allocates it per response, SDK once per Value). Fresh means
closing idle connections before each object, not rebuilding the Client. Origin
benchmarks independently compare the SDK origin to that stdlib streaming handler.
Concurrency uses a shared client/transport, fixed workers, and one page per read;
initial pool expansion is included. Setup and warmup are excluded from timing.

Representative medians from three runs, 32 KiB Read buffer; MB/s is decimal:

| Case | stdlib | SDK | SDK allocated B/op / allocs/op |
| --- | ---: | ---: | ---: |
| Empty, pooled | 49.7 us | 75.0 us | 18,389 / 164 |
| 4 KiB, pooled | 68.9 us | 93.3 us | 19,531 / 191 |
| 4 KiB, fresh | 142.8 us | 214.0 us | 46,239 / 270 |
| 16 MiB, pooled | 1,737 MB/s | 1,770 MB/s | 25,976 / 193 |
| 16 MiB, fresh | 1,746 MB/s | 1,607 MB/s | 48,001 / 272 |
| 1 GiB, pooled | 1,811 MB/s | 1,730 MB/s | 69,336 / 380 |
| 1 GiB, fresh | 1,738 MB/s | 1,769 MB/s | 88,872 / 456 |
| 16 MiB, concurrency 16 | 11,830 MB/s | 11,488 MB/s | 54,949 / 217 |
| Origin 4 KiB, pooled | 72.0 us | 119.0 us | 50,926 / 165 |
| Origin 16 MiB, pooled | 1,659 MB/s | 1,639 MB/s | 50,388 / 166 |

The small-object regression exceeds the design's 10% review threshold: pooled
4 KiB throughput is about 26% lower, fresh about 33% lower, and the origin about
40% lower. The serializer fix removes one concrete source of waste. Remaining
fixed overhead includes raw validation plus net/http parsing, Value/context
tracking, and origin cancellation/EOF-probe ownership. The page and GiB medians
are within 10% for same-buffer comparisons in this run. No blanket performance
claim or further parser rewrite is justified by these measurements.

The one-GiB allocation profile, including setup and warmup, totals only 865 KiB:
it contains no 16 MiB allocation. Allocations occur per request/connection, not per
Read call or transferred page. A multi-page object has a second request, so its
allocation count is higher than a one-page object; it does not grow with the
remaining page count. Existing source bounds and the live sampling support this,
rather than an assertion that benchmark allocation counters are exact SDK-only costs.

Separate one-GiB live-heap sampling forces GC approximately every 64 MiB, with a
second mode sleeping 1 ms per MiB to exercise backpressure. Absolute process heap
(client, server, harness, and runtime) in the final isolated sample:

| Path | First sample | Last sample | Sampled peak |
| --- | ---: | ---: | ---: |
| stdlib | 1,179,368 B | 1,147,248 B | 1,215,432 B |
| SDK | 1,191,640 B | 1,158,024 B | 1,226,272 B |
| stdlib, slow destination | 1,182,848 B | 1,149,280 B | 1,217,464 B |
| SDK, slow destination | 1,191,512 B | 1,163,984 B | 1,192,104 B |

This is a plateau across the stream, not a process RSS or instantaneous peak
guarantee. GC sampling perturbs timing and excludes kernel socket buffers. No
concurrency-16 heap profile was collected. Timing runs share a virtualized host,
have no CPU affinity, and one-GiB cases execute once per sample; allocator pools
and scheduling explain substantial B/op and timing variation. The generator also
costs CPU. Longer isolated runs are necessary for capacity planning.

## Residual boundaries

The Rust dataplane remains unimplemented; the fake page forwarder proves SDK
integration only. HTTP Content-Length hides overlong peer bytes from Value;
origin callback probing supplies stronger local checking but cannot verify a
correct-length payload's identity (`CLIENT_ORIGIN_API.md:156-163`). Cancellation
cannot forcibly stop a callback that ignores its context/Close contract. The
new dial test checks the deterministic private seam rather than filling a kernel
UDS backlog. No semantic policy changes were needed during this review.
