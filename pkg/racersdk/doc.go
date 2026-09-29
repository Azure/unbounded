// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racersdk provides a standard-library-only streaming Racer client and
// origin server over HTTP/1.1 Unix-domain sockets.
//
// # Reading objects
//
// Parse a CacheName and pass it to NewClient. Construction validates configuration
// without dialing; Get connects to /run/racer/<cache>/client/socket. Reuse the
// Client across requests and close it when its owner shuts down. Get takes a
// Request containing a Key and optional FetchContext, and returns an owned Value
// as soon as response headers have been validated. Always defer Value.Close.
// Read into a caller-owned buffer, or use io.Copy. Close the Value even when a
// destination write fails; io.Copy does not close its source.
// Avoid io.ReadAll for large objects: it introduces caller-side object buffering.
//
// Get is an ordered adapter over one POST /v2 subscription, including ranges and
// pins. There is no bootstrap, HEAD preflight, or continuation exchange. Racer
// schedules underlying origin page fetches. OpenPages exposes unordered page
// leases unless ReadOptions.Ordered is set. Release each lease after consuming
// its Data; release returns the page and byte credits on the same socket. Next
// waits when outstanding leases exhaust credits. Close cancels the subscription.
// DownloadTo writes unordered slices to an io.WriterAt at absolute object offsets.
// Length zero means through EOF; Pin optionally selects an existing version.
// Out-of-bounds ranges are rejected. Stat uses HEAD to obtain metadata without
// fetching any body. Pass a trusted Stat result as ReadOptions.Metadata to pin
// a resume. The snapshot is validated and copied, and a supplied Pin must match its
// ETag. Invalid snapshots fail before I/O; they never fall back to a fresh read.
// ReadOptions.SmallObject reserves separate admission for manifests and other
// objects whose total size is at most PageSize. It adds no HEAD by itself. An
// oversized object fails with ErrorInvalidArgument before any Value is returned,
// even if its selected byte range is small. There is no client page cache or
// object-sized SDK buffer. Page payload allocations are bounded by negotiated
// credits (default two pages), and duplicate tracking uses bounded intervals.
// Get receives ahead on one private goroutine, holding at most two payload
// buffers in total (current, queued and in-flight), reduced by page/byte credits.
// Released buffers are reused only within that request; final slices have their
// capacity clipped to their verified length. OpenPages.Next remains synchronous.
// Read-ahead retains admission until consumption ends or Close/cancellation joins
// the receiver and drops its buffers. A later receive failure is reported after
// earlier verified pages; the final page still requires a valid Complete frame.
// Value.Metadata is the initial total size, strong ETag, expiry and content type snapshot;
// it is not a remaining length and does not change during delivery.
// Empty objects have valid metadata and read as EOF.
//
// Client.Get and Client.Close are safe concurrently. A Value permits one consuming
// goroutine using Read or WriteTo, directly or through io.Copy; Metadata and
// Close may run concurrently with consumption. Do not copy Clients or Values.
// Close is idempotent, cancels SDK I/O, and closes bodies without draining unread
// bytes. Client.Close also cancels pending Gets and all active Values. Cancellation
// cannot interrupt arbitrary caller code blocked inside a destination Writer.
//
// # Types, credentials, and expiry
//
// ParseKey accepts exactly 64 lowercase hexadecimal characters; every Key,
// including zero, is valid. ParseETag requires a strong quoted tag; a zero ETag
// means an absent request pin and is invalid response metadata. ByteLength and
// ByteOffset distinguish sizes from positions, with wire values bounded by MaxInt64.
// ClosedRange and Range.Resolve serve whole-page origin adapters; ReadOptions
// selects arbitrary client byte ranges independently of origin page boundaries.
//
// A zero FetchContext omits both fields. ParseAdapterMetadata, ParseAuthorization,
// and NewFetchContext validate immutable opaque origin context without trimming or
// interpreting it. Authorization is an upstream fetch credential, not permission
// to use Racer or an input to cache identity. Socket access and cache provisioning
// are deployment responsibilities. Diagnostic formatting redacts context, requests,
// and credentials; ForOrigin explicitly extracts a field for upstream use. Do not
// log extracted credentials. Go strings do not provide secret zeroization.
//
// Metadata.Validate requires a nonnegative signed-64-bit Unix-millisecond expiry
// with exact millisecond precision. Use time.UnixMilli or explicitly truncate a
// locally chosen expiry with t.Truncate(time.Millisecond); the SDK never silently
// rounds metadata. Expiry is a cache admission hint, not a deadline that interrupts
// an admitted stream. The SDK neither invents a TTL nor locally caches metadata.
// Metadata.ContentType is optional ASCII MIME metadata, at most 256 bytes, with
// no control characters or duplicate parameters. Racer-Content-Type carries it;
// the transport Content-Type for object bytes remains application/octet-stream.
// Racer-Content-Type is optional. A supplied snapshot rejects conflicting MIME
// values only when both are present; Value.Metadata
// always retains the original snapshot, including an originally absent MIME type.
//
// # Cancellation, limits, and errors
//
// The context passed to Get governs capacity waits and the returned Value's entire
// lifetime. Keep it alive until consumption ends; use a deadline when completion
// must be bounded. Client defaults are 64 bulk connections/live Values, 4 reserved
// metadata connections, 4 small-object connections, independently bounded queues
// of 128 bulk, 16 metadata and 128 small-object calls, a 5-second queue and
// dial timeout, a 60-second response-header timeout, a 90-second idle timeout,
// and a 5-minute MaxConnAge. Each successful dial selects a fixed lifetime
// uniformly from 75% through 100% of MaxConnAge (whole nanoseconds rounded up).
// Expired reusable connections retire at checkout, after a response completes,
// or while idle. Active responses are never interrupted by connection age;
// retirement does not dial in the background or restart a pinned stream.
// Stats.ConnectionRotations counts age retirements separately from stale retries.
// Admission happens before allocating active stream state. A full queue returns
// ErrorUnavailable; queue timeout returns ErrorDeadline. There is no total client
// stream timeout. Zero numeric config fields select defaults; negatives are invalid.
// Read copies verified page slices into the caller's buffer; each active origin stream uses a 32 KiB
// scratch buffer. SDK buffering is independent of object size and page count;
// connection/request limits bound active framing and copying resources.
// WriteTo owns its 32 KiB loop and never delegates to destination ReaderFrom.
// Each client lazily allocates at most MaxConnections bulk copy buffers and
// SmallObjectConnections independently reserved small-object copy buffers, counting
// both cached buffers and copies blocked inside caller-owned Write. Close drops
// cached buffers; blocked writers retain theirs until they return. If canceled,
// blocked writers exhaust this separate bound, a new WriteTo returns
// ErrorUnavailable without consuming bytes; Read remains available.
//
// Use errors.As with *Error for Kind, Operation, and optional StatusCode (zero
// without a received HTTP status). Use errors.Is for preserved causes such as
// context.Canceled, context.DeadlineExceeded, or io.ErrUnexpectedEOF.
// Clean EOF remains io.EOF. Get success does not guarantee later
// reads will succeed: process n bytes even when Read returns an error, and treat
// io.Copy's count as partial on failure. A failed Value is terminal; the SDK never
// splices in a newer version or restarts a failed stream. Publish a copied object
// only after successful completion. Direct HTTP/1.1 parsing preserves strict raw
// framing and streams from the same connection reader. Subscription connections
// are never reused or retried. Only HEAD uses the idle pool. An EOF/reset/broken pipe
// on a pooled HEAD connection is retried once on a fresh connection only before any
// response bytes arrive. Timeouts, partial heads, invalid heads and body failures
// are never retried. The original request and pin are preserved on retry.
// Client.Stats provides fixed-size cumulative counters and current resource
// gauges without credentials, per-object labels, or a metrics dependency.
// Error diagnostics omit cause text; explicitly unwrapping an error can expose it.
//
// # Implementing an origin
//
// ServeOrigin calls Origin once per accepted operation, possibly concurrently.
// Select an immutable backend snapshot and obtain metadata and body from that
// same snapshot. A separate stat followed by opening a mutable name is unsafe.
// Check Pin before resolving Range, including for HEAD: return the requested
// version or NewOriginError(ErrorVersionUnavailable, cause), never current bytes
// under an old tag. Repeated read operations must be safe. Upstream implementations
// should pass ctx and the extracted fetch context to their conditional read API.
//
// HEAD returns metadata and a nil body. Bootstrap returns page zero, shortened at
// EOF; for an empty object it returns valid zero-size metadata and nil or an
// immediately-EOF body. Handle empty bootstrap before calling Range.Resolve,
// because ranges on empty objects are unsatisfiable. Pinned GET returns exactly
// the resolved whole page; the final page can be short. The SDK validates page
// shape, metadata, pin, and body bounds. Return selected-version metadata alongside
// an unsatisfiable-range error so the SDK can construct its 416 size. A missing
// unpinned object is ErrorNotFound; a missing pinned version is 412, including
// when the callback returns ErrorNotFound. Other callback errors can be classified
// with NewOriginError; unclassified errors become 500.
//
// Every nonnil returned body transfers to the SDK, even alongside an error. Do not
// close it in the callback after transfer. Its Close must promptly interrupt Read
// and be safe concurrently with it; callbacks must honor ctx. The SDK closes each
// body once, checks the expected length, and probes for EOF before releasing the
// final chunk. Late failures abort the response instead of appending an error.
// This checks callback stream bounds, not content authenticity: a Value cannot
// reliably detect extra bytes outside an HTTP Content-Length frame or wrong bytes
// carrying the expected length and ETag. No end-to-end content hash is added.
//
// ServeOrigin owns /run/racer/<cache>/origin/socket. Deployment must precreate its
// parent directories without symlinks and protect them from untrusted writers.
// Existing paths, including stale sockets, are refused; cleanup preserves a
// replacement inode. Default socket permissions are 0600. Origin defaults are 128
// accepted connections, 64 concurrent GET callbacks/bodies, 4 independently
// reserved HEAD callbacks, a 5-second header timeout,
// a 60-second request timeout (including EOF probing and final writes), and
// 30-second blocked-write and idle timeouts. Overload returns 503. See OriginConfig
// for overrides. Server cancellation closes connections/bodies and returns
// ctx.Err(); it does not wait for noncooperative callbacks, whose slots remain
// occupied until they return. Late-returned bodies are still closed.
//
// # Testing with a fake Racer
//
// NewFakeClient(origin) returns (*Client, func(), error). Register its cleanup with
// t.Cleanup or defer it in examples. The returned Client and its Values use the real
// SDK transport and validation path. Private loopback servers run the real origin
// request, metadata, pin, body-length, EOF, and cancellation checks, plus fake
// sequential page forwarding. No /run directory, Racer process, endpoint
// configuration, or testing-package dependency is required.
//
// Each Get opens one credit-controlled subscription. The fake resolves metadata
// using bootstrap or pinned HEAD, then forwards FetchContext on every origin page,
// keeps bounded streaming buffers, and never caches objects. It does not model
// Racer caching, distributed scheduling, retries, or performance, and passing fake
// tests does not establish real Racer compatibility. Callback failures before a
// subscription response starts preserve their HTTP classification; failures
// after the subscription head abort it. Both leave a partial
// full-object byte count and a terminal Value.
//
// Cleanup is idempotent and safe concurrently. It closes the Client and both
// servers, cancels active work, and releases listeners and connections. Calling
// Client.Close alone still requires cleanup to release the harness. Origin must
// honor cancellation and supply interruptible bodies just as with ServeOrigin;
// cleanup does not wait for noncooperative callbacks, and closes late-returned
// bodies. Client and origin resource defaults apply to the fake too.
//
// The examples with Output run without Rust or /run permissions. Examples marked
// deployment-only are compile-checked and require provisioned Unix endpoints to
// run. Rust interoperability is tested separately by the dataplane's opt-in SDK
// conformance fixture; the examples alone do not establish interoperability.
package racersdk
