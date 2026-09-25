# Racer client and origin API (v1)

This is the approved implementation contract for the Go SDK and Rust dataplane.
The [Go SDK](../../pkg/racersdk/doc.go) implements the client and origin boundaries;
this does not establish live Rust interoperability. The Rust parser, origin calls,
and stream coordinator still return `Unimplemented` (`src/client/request.rs:32-34`,
`src/origin/client.rs:46-64`, `src/read/serve.rs:61-68`). Paths in source citations
here are relative to this directory. See the [SDK design](../../designs/racer-sdk.md)
for public Go types, resource defaults, ownership, and implementation acceptance.
The [control API](CONTROL_API.md) provisions caches and credentials separately.

## Transport and identity

- HTTP/1.1 over Unix-domain stream sockets only. Racer owns
  `/run/racer/<cache name>/client/socket`; the adapter owns
  `/run/racer/<cache name>/origin/socket`. The directory selects the cache; no cache
  header or URL parameter exists. These ownership boundaries follow
  `src/control/caches.rs:3-7,14-23`; lifecycle validation is still a stub at 34-35.
- Exactly `HEAD` and `GET` on `/v1/objects/<key>`, where `<key>` is exactly 64
  lowercase hexadecimal characters encoding the 32-byte key. Require this exact
  origin-form request target: no query (even empty), fragment, percent encoding,
  extra slash, absolute URI, or path cleaning. Cache names are lowercase DNS
  subdomains: nonempty dot-separated labels of 1-63 ASCII alphanumeric/hyphen
  characters, with alphanumeric ends, at most 253 bytes overall. Reject names
  whose complete socket path exceeds the platform limit (107 bytes on Linux).
- Send one `Host: racer` field. Host is not routing or authorization information.
  No proxy, TLS, upgrade, HTTP/2, redirect following, compression, multipart,
  trailers, or pipelining. Connections may be reused sequentially.
- No request body: omit `Content-Length` or send exactly one `Content-Length: 0`.
  Reject nonzero lengths, `Transfer-Encoding`, `Expect`, and `Trailer` with 400
  and close the connection without draining. Unframed bytes after a request belong
  to the next HTTP message; they cannot reliably be identified as a hidden body.
- Reject `Content-Encoding` and `Transfer-Encoding` on either side, including
  `identity`; SDK requests omit `Accept-Encoding`. Successful GETs have exactly
  `Content-Type: application/octet-stream`. Responses always have one canonical
  decimal `Content-Length`; never use chunked or close-delimited framing.

## Headers and limits

Header names are case-insensitive. Reject repeated `Host`, `Content-Length`,
`Content-Type`, `Content-Range`, `ETag`, `If-Match`, `Range`, `Racer-Expires-At`,
`Racer-Metadata`, or `Authorization`, even when identical. Reject list-valued
framing, folded lines, and duplicate framing before a library normalizes them.
Unknown non-framing fields may be ignored but count toward the limit. Unsupported
HTTP conditional fields (`If-None-Match`, date conditions, `If-Range`) are invalid.

The entire head, including start line, CRLFs, and terminating CRLF, is at most
32 KiB. Each present `Racer-Metadata` or `Authorization` field value is 1-8192
bytes. They are opaque canonical field values: no leading/trailing space, no bytes
0x00-0x1f or 0x7f, no interpretation of commas or credential schemes. Interior
spaces and bytes 0x80-0xff are allowed and preserved byte-for-byte. Absence differs
from an empty field, which is invalid. Require `Name: value\r\n` for these two
fields, with exactly one separator space; reject surplus leading/trailing
whitespace rather than silently trimming it. Raw-header validation is necessary:
Go's parsed `http.Header` alone cannot enforce these rules.

Forward key and optional context unchanged on every origin request, including
pinned continuations. Authorization is an upstream fetch credential, never Racer
authorization. Neither context field enters cache identity, metadata/page caches,
logs, metrics, or durable retries. Keep it only while the request needs it; pooled
connections must not retain request headers. This completes the intent in
`src/model/context.rs:1-7,35-54`; Go cannot promise secret zeroization.

Successful metadata consists of:

| Field | Canonical value |
| --- | --- |
| `ETag` | One strong quoted tag, 2-8192 bytes including quotes. Interior bytes are ASCII `!` (0x21) or 0x23-0x7e. Empty `""` is valid. No escaping: backslash is literal, embedded quote is invalid. Reject `W/`, wildcard, and tag lists. A comma inside quotes is literal. |
| `Racer-Expires-At` | Absolute Unix milliseconds in `0..9223372036854775807`, decimal digits, no sign, padding, whitespace, or leading zeros except `0`. |
| Object size | `0..9223372036854775807` bytes. HEAD carries total size in `Content-Length`; 206 carries it in `Content-Range`. |

All range/length numbers use the same canonical decimal syntax and are bounded by
MaxInt64. An ETag identifies one immutable size and byte sequence within a cache/key;
it is not required to be a content hash. Expiry may be refreshed without changing
ETag, but size must not change. Fresh metadata admits new unpinned reads only while
`now < ExpiresAt`. Revalidation may admit its current waiters once even with zero
TTL; it must not authorize later unpinned cache hits. Explicit pins and admitted
streams may continue after expiry. This preserves the zero-TTL intent in
`src/read/metadata.rs:3-6`, whose resolver is unimplemented at 46-54.

## Operations

`P = 16777216` (16 MiB, `src/model/range.rs:6`). Both endpoints use these operations:

| Request | Success |
| --- | --- |
| `HEAD`, no Range, optional strong `If-Match` | 200, metadata, `Content-Length: <total>`, no body or Content-Range. |
| Bootstrap `GET`, `Range: bytes=0-16777215`, no If-Match | Nonempty: 206, page zero (possibly short). Empty: 200, metadata, `Content-Length: 0`, no Content-Range. |
| Pinned `GET`, one Range and one strong `If-Match` | 206, exact normalized range of that version; never downgrade to 200. |

GET without Range and any other unpinned GET range are invalid. Client requests
accept `bytes=first-last`, `bytes=first-`, or `bytes=-count`, with no whitespace or
multiple ranges. Reversed bounds, overflow, and malformed syntax are 400. Resolve
against the pinned version's size `N`: closed end is `min(last,N-1)`, open end is
`N-1`, suffix starts at `max(0,N-count)`. An empty object, `first >= N`, or zero
suffix count is 416. Every 206 has `Content-Range: bytes first-last/N`, nonzero
`Content-Length: last-first+1`, ETag, expiry, and the GET content type. A range
covering the entire nonempty object still returns 206. Range never appears on HEAD.

The origin endpoint accepts bootstrap and HEAD as above, but a pinned GET must
request exactly one whole page: a closed range starting at `k*P`, with end either
`k*P+P-1` or the exact short-final-page end. The callback receives the requested
bounds; the SDK validates the latter against returned total size before headers.
No suffix, open, cross-page, or partial-page origin requests. For arithmetic at the
MaxInt64 boundary, the nominal end is clamped to MaxInt64. An out-of-object page
is 416; an in-object partial/cross-page request is 400. The callback must retrieve
the exact requested version or report 412, never silently return current bytes.
SDK validation also rejects an apparently successful callback with a mismatched
ETag as 502. Dataplane client ranges are split into whole-page origin fetches by
the dataplane, not by the Go client.

Fresh HEAD/bootstrap missing objects return 404. A valid pin whose version no
longer exists (including object deletion) returns 412. Credential rejection remains
401/403, and transient failures remain 503, not 412. Resolve version before range
satisfiability; do not use current-version size to answer an old pin.

## Errors and stream boundaries

| Status | Meaning |
| --- | --- |
| 400 | Invalid target, request headers, method-specific fields, or range syntax |
| 401 / 403 | Origin credential rejected / access forbidden |
| 404 | Unpinned object missing |
| 405 | Unsupported method; include `Allow: HEAD, GET` |
| 412 | Pinned version unavailable |
| 416 | Unsatisfiable range; include `Content-Range: bytes */N` for the selected version |
| 431 | Request header or opaque-field limit exceeded |
| 500 | Unexpected callback/server failure |
| 502 | Invalid upstream response, metadata, or body contract |
| 503 | Transient failure, deadline before headers, or overload |

Errors have `Content-Length: 0` and no body, ETag, or expiry. Close on malformed
framing or header-limit violations. A parser failure with no safely writable HTTP
connection may just close. Oversized or malformed *responses* are local protocol
errors in the SDK and 502 at a forwarding dataplane, not 431. Unknown statuses,
redirects, and unexpected informational responses are protocol errors. No SDK
retry loop or automatic restart on 412, 503, truncation, or cancellation.

Validate status, headers, version, total size, and normalized boundaries before
returning a Value or writing origin response headers. A body reader must terminate
at exactly the advertised length. Early EOF is `io.ErrUnexpectedEOF`, not success.
Preserve other I/O and context causes. Never append an HTTP error body or second
status after committing a success; abort the connection on a late failure.

The origin SDK owns every callback body, even one returned alongside an error.
HEAD requires nil body; close and reject an unexpected body. Nonempty GET requires
a body. Empty bootstrap permits nil or a body that immediately returns EOF. The
callback must atomically select metadata and open the body for the same version;
separate stat/open without an immutable handle or conditional read is insufficient.
The SDK streams only the requested bytes, checks short reads, and probes one byte
past the expected length for EOF. Hold the final byte until this probe succeeds so
an overlong body or terminal error can truncate the response; for empty GET, probe
before headers. The probe uses the same deadline and cancellation as the request.
Do not discard a non-EOF error returned with the last expected bytes.

There are real detection limits. An HTTP body reader exposes only Content-Length
bytes, so extra bytes beyond that frame are not reliably detectable by a Go Value;
they may instead corrupt a subsequent message. A guard can reject observed illegal
framing, but cannot prove the peer will never send additional bytes. Likewise,
disconnect after exactly the advertised bytes cannot convey a late producer error.
Draining/probing an HTTP response body does not prove transport-level absence of
extra bytes. The origin callback probe provides stronger local checking; it cannot
repair incorrect bytes with the right length and ETag. No end-to-end hash is added.

## Implementation boundary

Use stdlib HTTP plus a small bounded raw-head guard at Unix connections where
normalization would erase duplicate lengths, whitespace, or exact target syntax.
Account for stdlib read-ahead and sequential fixed-length bodies; do not scan body
bytes for header delimiters or build another page buffer. `MaxHeaderBytes` alone
is not an exact 32 KiB wire-head limit. Conformance tests must use raw Unix peers,
not only `httptest` handlers, to catch parser normalization and automatic error
body behavior. Runtime/parser-generated errors must also have empty bodies when
a response can be sent safely. The Go SDK implements the guards in
`pkg/racersdk/response_conn.go` and `pkg/racersdk/origin_conn.go` (repository-relative
paths); stock `net/http` alone does not enforce every rule above.
