# Racer Go SDK

Import `github.com/Azure/unbounded/pkg/racersdk` to read immutable objects or serve
an origin callback on Linux. The SDK does not start a Racer runtime or provision
volumes, directories, or origins. Client reads need a compatible Racer endpoint;
use `racersdktest` for local tests without one.

## Read objects

Parse a volume name with `ParseVolumeName`, then pass it to `NewClient` in
`ClientConfig.Volume`. The client uses `/run/racer/<volume>/client/socket`.
Construction validates settings without dialing, so it is not a readiness check.
Reuse the client across requests and close it at shutdown.

`ParseKey` accepts exactly 64 lowercase hex digits. Build a `Request` with that
key and, if needed, a `FetchContext` made from parsed adapter metadata and upstream
authorization. These fields carry origin context, not access control for Racer.
Use `ForOrigin` only when passing them to the backend; do not log the extracted
values or unwrapped error causes.

| API | Use and cleanup |
| --- | --- |
| `Stat` | Read fresh full-object metadata on reserved connections. |
| `Get` | Read ordered bytes with `Read`, `WriteTo`, or `io.Copy`. Close the returned `Value` on every path. |
| `OpenPages` | Receive page slices with `Next`, unordered by default. Release every `PageLease` and close the stream. Do not use `Data` after release. |
| `GetStreaming` | Forward only through `Value.WriteToHTTP`. Close the value on every path. |

Keep the request context alive through the last read, not just the opening call.
Use one consumer per value or page stream. `Close` can run concurrently and stops
the stream without draining it. Check copy/read errors: a destination may already
contain a prefix. The SDK does not restart a failed subscription.

`ReadOptions.Offset` and `Length` select bytes; zero length means through EOF.
Overlong ranges are rejected, not shortened. `Metadata().Size` is always the full
object size, not the selected length. `Pin` selects a version. A trusted
`ReadOptions.Metadata` snapshot, usually from `Stat` for the same object, pins its
ETag; any explicit pin must match.

`Get` uses one subscription and buffers at most two page slices. `OpenPages`
waits when outstanding leases use its credits, so release pages as you finish
with them. Pages are 16 MiB. Page credits accept 1 through 64; byte credits accept
16 MiB through 1 GiB. Zero selects the configured page window (default two) and
`PageCredits * PageSize` bytes. `SmallObject` uses reserved admission and requires
the full object, not just the range, to fit in one page.

For HTTP forwarding, the handler sets headers, including the selected length,
and calls `WriteToHTTP`. Streaming mode may expose an incomplete page prefix;
it holds back the final byte until the terminal frame is checked. On failure
after headers are sent, abort the response rather than write an error body.
For an empty range, do not send headers before `WriteToHTTP` succeeds.

Use `errors.As` to inspect `*racersdk.Error`, then `Kind()` and `StatusCode()`.
Use `errors.Is` for wrapped causes such as context cancellation. `Stats()` gives
client limits, queue depth, connection counts, and transfer counters without
request labels. Configuration comments list defaults. Body read timeouts bound
individual reads, not the total stream lifetime or time spent by the caller
between reads. An arbitrary blocked destination writer cannot be interrupted by
SDK cancellation.

## Serve an origin

Pass an `Origin` callback and `OriginConfig{Volume: volume}` to `ServeOrigin`.
It binds `/run/racer/<volume>/origin/socket` and blocks until cancellation or
listener failure. Treat `context.Canceled` as expected during shutdown.

Prepare and protect the origin directory first. Its ancestors must not be
symlinks or writable by untrusted users. Socket mode defaults to `0600`.
Existing paths are refused. `RecoverStaleSocket` only recovers sockets created
in that mode, using lock and witness files in a directory owned by the current
user and not writable by group or others. It does not remove arbitrary sockets.

The callback may run concurrently. Select metadata and bytes from the same
immutable version; do not stat a mutable name and then open different bytes.
Honor `Pin()` before resolving `Range()`:

- HEAD returns full-object metadata and a nil body.
- Bootstrap selects a version and its first page; an empty object may have a nil body.
- Pinned reads return exactly the page bytes selected by `Range.Resolve(size)`.
  Its bounds are inclusive, and the last page is shortened at EOF.

Metadata needs a full size, strong quoted ETag, and nonnegative Unix expiry with
millisecond precision. `time.Time{}` is not a valid expiry. Optional `ContentType`
is limited to 256 bytes. Expiry is an admission hint, not a running stream deadline.

Every nonnil body transfers to the SDK, even on error. Do not close it before
returning. Pass cancellation into backend I/O; body `Close` must promptly unblock
`Read` and be safe to call concurrently with it. Return `NewOriginError` for
classified failures. Pinned not-found becomes version-unavailable (412). A range
error (416) must include valid metadata for the selected version.

## Examples and tests

[SDK examples](example_test.go) cover client startup, partial reads, metadata
snapshots, errors, and an immutable origin. The client and origin startup examples
are compile-checked only and use separate fixture keys. They require prepared
canonical endpoints when run.

For a runnable local example, see
[`racersdktest.NewClient`](racersdktest/example_test.go). It starts a noncaching
fake with the real SDK client and origin server on private temporary Unix sockets.
Register its returned cleanup function with `t.Cleanup`; `Client.Close` alone
does not stop the servers. Use a short `TMPDIR` so socket paths fit in 107 bytes.
This helper does not test distributed caching, RDMA, or real runtime compatibility.

From the repository root, run the SDK tests and runnable examples with:

```sh
timeout --signal=TERM --kill-after=10s 300s env GOTOOLCHAIN=go1.26.8 go test -timeout=5m ./pkg/racersdk/...
```

Rust interoperability is opt-in via `RACER_SUBSCRIPTION_INTEROP=1`. It requires
the sibling dataplane work, which is not included in this SDK-only change.
The test expects its `subscription-interop` feature and `subscription_interop`
fixture at `cmd/racer-dataplane` in the same checkout; it does not discover a
separate sibling checkout. Leave the flag unset for standalone SDK tests.
The runtime connection-age load test also needs an external Rust fixture and is
skipped unless `RACER_SDK_AGE_SOCKET` is set.
