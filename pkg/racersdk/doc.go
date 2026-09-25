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
// Get opens a fresh full-object stream with a page-zero bootstrap. After consuming
// the first 16 MiB, reading lazily opens one pinned continuation for all remaining
// bytes. Racer, rather than this SDK, schedules its whole-page origin fetches.
// There is no HEAD preflight, client page cache, or object-sized SDK buffer.
// Value.Metadata is the initial total size, strong ETag, and expiry snapshot;
// it is not a remaining length and does not change if continuation expiry changes.
// Empty objects have valid metadata and read as EOF.
//
// Client.Get and Client.Close are safe concurrently. A Value permits one consuming
// goroutine using Read, directly or through io.Copy; Metadata and
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
// ClosedRange and Range.Resolve serve whole-page origin adapters; Request deliberately
// exposes no caller-selectable range or pin.
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
//
// # Cancellation, limits, and errors
//
// The context passed to Get governs capacity waits and the returned Value's entire
// lifetime. Keep it alive until consumption ends; use a deadline when completion
// must be bounded. Client defaults are 16 connections/live Values, a 5-second dial
// timeout, a 10-second response-header timeout, and a 90-second idle timeout.
// There is no total client stream timeout. Zero numeric config fields select
// defaults, not unlimited operation; negative values are invalid. Bound caller
// concurrency too: pending Get callers still consume application resources.
// Read uses the caller's buffer; each active origin stream uses a 32 KiB
// scratch buffer. SDK buffering is independent of object size and page count;
// connection/request limits bound active framing and copying resources.
//
// Use errors.As with *Error for Kind, Operation, and optional StatusCode (zero
// without a received HTTP status). Use errors.Is for preserved causes such as
// context.Canceled, context.DeadlineExceeded, or io.ErrUnexpectedEOF.
// Clean EOF remains io.EOF. Get success does not guarantee later
// reads will succeed: process n bytes even when Read returns an error, and treat
// io.Copy's count as partial on failure. A failed Value is terminal; the SDK never
// splices in a newer version or restarts a failed stream. Publish a copied object
// only after successful completion. There is no SDK retry loop, although Go's
// HTTP transport may replay an eligible read on a failed reused connection.
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
// accepted connections, 64 concurrent callbacks/bodies, a 5-second header timeout,
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
// request, metadata, pin, body-length, EOF, and cancellation checks, plus a fake
// sequential page scheduler for continuations larger than 16 MiB. No /run directory,
// Racer process, endpoint configuration, or testing-package dependency is required.
//
// Each Get fetches a fresh bootstrap. The fake forwards FetchContext on every page,
// keeps bounded streaming buffers, and never caches objects. It does not model
// Racer caching, distributed scheduling, retries, or performance, and passing fake
// tests does not establish real Racer compatibility. Callback failures before a
// response starts preserve their HTTP classification; failures on later pages of
// an already-started continuation abort the stream and leave a partial byte count.
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
