# Racer client subscription API (v2) and origin API (v1)

This is the approved implementation contract for the Go SDK and Rust dataplane.
The [Go SDK](../../pkg/racersdk/doc.go) implements the client and origin boundaries.
The replacement client contract is implemented in `src/client/request.rs:217-237`,
`src/client/subscription.rs:24-63,135-273`, and `src/read/serve.rs:216-267`.
This is a breaking client upgrade with no GET/window fallback or dual body-read
protocol. Origin operations are unchanged. Process, conformance, and E2E client
fixtures now exercise subscriptions, with a separate live Go SDK/Rust read-graph
harness. See the [subscription design](../../designs/racer-hot-subscriptions.md#evidence-and-validation-scope)
for code assertions and evidence scope; historical results are not new pass totals.
Paths in source citations here are relative to this directory.
See the [SDK design](../../designs/racer-sdk.md)
for public Go types, resource defaults, ownership, and implementation acceptance.
The [control API](CONTROL_API.md) provisions caches and credentials separately.

## Transport and identity

- HTTP/1.1 over Unix-domain stream sockets only. Racer owns
  `/run/racer/<cache name>/client/socket`; the adapter owns
  `/run/racer/<cache name>/origin/socket`. The directory selects the cache; no cache
  header or URL parameter exists. Canonical socket paths and cache-definition
  validation are implemented in `src/control/caches.rs:55-99`; application cache
  transitions are wired in `src/app_caches.rs`; scoped new-admission checks use
  `src/control/availability.rs`, while accepted operations drain under their leases.
- Client reads use `POST /v2/objects/<key>`; metadata-only `HEAD` accepts both
  `/v1/objects/<key>` and `/v2/objects/<key>`. The origin endpoint still uses
  `HEAD` and `GET` on `/v1/objects/<key>`. In each case `<key>` is exactly 64
  lowercase hexadecimal characters encoding the 32-byte key. Require this exact
  origin-form request target: no query (even empty), fragment, percent encoding,
  extra slash, absolute URI, or path cleaning. Cache names are lowercase DNS
  subdomains: nonempty dot-separated labels of 1-63 ASCII alphanumeric/hyphen
  characters, with alphanumeric ends, at most 253 bytes overall. Reject names
  whose complete socket path exceeds the platform limit (107 bytes on Linux).
- Send one `Host: racer` field. Host is not routing or authorization information.
  No proxy, TLS, upgrade, HTTP/2, redirect following, compression, multipart,
  trailers, or pipelining. HEAD and origin connections may be reused sequentially;
  a client subscription owns its connection until completion or cancellation.
- A subscription POST requires exactly one `Content-Length: 0`. After the HTTP
  request head, that socket carries the release frames described below, not
  another HTTP request. HEAD and origin requests have no body: omit
  `Content-Length` or send exactly one `Content-Length: 0`.
  Reject nonzero lengths, `Transfer-Encoding`, `Expect`, and `Trailer` with 400
  and close the connection without draining. On the sequential HEAD/origin path,
  unframed bytes belong to the next HTTP message and cannot reliably be identified
  as a hidden body.
- Reject `Content-Encoding` and `Transfer-Encoding` on either side, including
  `identity`; SDK requests omit `Accept-Encoding`. Subscription and origin GET bodies have exactly
  `Content-Type: application/octet-stream`. Responses always have one canonical
  decimal `Content-Length`; never use chunked or close-delimited framing.

## Headers and limits

Header names are case-insensitive. Reject repeated `Host`, `Content-Length`,
`Content-Type`, `Content-Range`, `ETag`, `If-Match`, `Range`, `Racer-Expires-At`,
`Racer-Metadata`, `Racer-Content-Type`, or `Authorization`, even when identical.
Client subscriptions additionally reject repeated `Racer-Page-Credits`,
`Racer-Byte-Credits`, and `Racer-Ordered`; response range/size fields are singletons.
Reject list-valued
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
| `Racer-Content-Type` | Optional object MIME metadata, 1-256 ASCII bytes when present. Require `type/subtype` with optional MIME parameters. Reject controls (including tabs), DEL, non-ASCII, leading/trailing spaces, duplicate fields/parameter names, lists, and malformed token/quoted-string syntax. Preserve the value; absence is unknown. Go exposes `Metadata.ContentType string`, with empty string meaning absent. |
| Object size | `0..9223372036854775807` bytes. HEAD carries total size in `Content-Length`; subscription 200 carries it in `Racer-Object-Length`; origin 206 carries it in `Content-Range`. |

All range/length numbers use the same canonical decimal syntax and are bounded by
MaxInt64. An ETag identifies one immutable size and byte sequence within a cache/key;
it is not required to be a content hash. Expiry may be refreshed without changing
ETag, but size must not change. Fresh metadata admits new unpinned reads only while
`now < ExpiresAt`. Revalidation may admit its current waiters once even with zero
TTL; it must not authorize later unpinned cache hits. Explicit pins and admitted
streams may continue after expiry. The metadata resolver implements this zero-TTL
and retained-version behavior (`src/read/metadata.rs:321-360`), with production
coverage in `tests/production_dataplane.rs`.

Object content type travels from the origin's `Racer-Content-Type` response header
through version metadata, retained pages, signed peer metadata, and client HEAD/subscription
responses. It never replaces transport `Content-Type: application/octet-stream`.
Two present content types for the same version must agree; legacy absence is not
a conflicting claim. HEAD remains metadata-only and does not retrieve a body.

### Metadata compatibility

The client body API is v2; the origin HTTP API remains v1. Content type is optional
in both. The peer envelope is v5, without support for older peer profiles.
Peers use the existing signed response shape when content type is absent. A typed
response additionally signs `racer-metadata-version: 2` and `racer-content-type`;
new decoders reject unknown versions, a missing typed value, or an unversioned
typed value. Old strict peer decoders reject this extension, so mixed-version peer
paths carrying typed metadata can fail until all participating nodes are upgraded.
There is no silent downgrade or capability negotiation.

Disk compatibility is separate from HTTP compatibility. The current record
format is v4 only (`src/store/format.rs:19-20`); the earlier content-type v1/v2
record compatibility policy is superseded. See the
[store protocol](../../designs/racer-store-protocol.md) for record/checkpoint
details. Replacement client subscriptions do not change the origin API or disk format.

## Client operations

`P = 16777216` (16 MiB, `src/model/range.rs:6`).

| Request | Success |
| --- | --- |
| `HEAD`, no Range, optional strong `If-Match` | 200, metadata, `Content-Length: <total>`, no body or Content-Range. |
| `POST /v2/objects/<key>`, optional Range and strong `If-Match` | 200, metadata and framed page slices for the selected range, followed by Complete. |

An absent pin selects fresh metadata within the POST; there is no client HEAD
preflight or page-zero bootstrap. A supplied pin selects that immutable version.
An absent Range selects the whole object. Range accepts `bytes=first-last`,
`bytes=first-`, or `bytes=-count`, without whitespace or multiple ranges. Resolve
against selected size `N`: closed end is `min(last,N-1)`, open end is `N-1`, and
suffix starts at `max(0,N-count)`. Reversed bounds, overflow, and malformed syntax
are 400. An explicit range on an empty object, `first >= N`, or zero suffix is 416.
An empty object with no Range succeeds with Complete only. The Go options API is
stricter than wire range normalization: an explicit Length extending past EOF is
rejected rather than silently shortened (`pkg/racersdk/subscription.go:152-170`,
repository-relative).

Subscription request fields:

| Field | Allowed values | Default |
| --- | --- | --- |
| `Racer-Page-Credits` | Canonical decimal 1..64 | 2 |
| `Racer-Byte-Credits` | Canonical decimal P..64*P | 2*P |
| `Racer-Ordered` | `0` or `1` | `0` |

Page membership is fixed by the selected immutable version and range, independent
of delivery order. Unordered consumers must use page numbers and absolute offsets,
not infer ascending order. Ordered delivery is opt-in on the wire. Credits bound
pending and delivered-but-unreleased slices; they are not an object-size limit.

A successful response includes ETag, expiry, optional object content type,
`Racer-Object-Length`, `Racer-Range-Start`, exclusive `Racer-Range-End`,
`Content-Type: application/octet-stream`, and `Connection: close`. It has no
Content-Range. For `B` selected bytes intersecting `Q` pages, Content-Length is
`B + 21*(Q+1)`; an empty object has length 21. All integers in frames are big-endian:

| Frame | Fields | Payload |
| --- | --- | --- |
| Page | u8 kind=1, u64 page number, u64 absolute byte offset, u32 slice length | Exact nonempty intersection of that page with the selected range |
| Complete | u8 kind=2, u64 delivered page count, u64 delivered byte count, u32 zero | None |
| Release (client to server) | u64 page number, u32 slice length | None |

Every page intersection is delivered once. The receiver validates membership,
offsets, lengths, duplicates, negotiated order, and terminal counts. A Release
must match one outstanding delivered page and its exact length; malformed,
duplicate, or premature releases terminate the connection. Release returns that
subscriber's credits and retained page ownership, not origin acquisition credits.
Complete does not wait for the final releases. Cancellation closes the socket;
post-header failures truncate it, never append another HTTP status. There is no
automatic restart, resume, or version substitution.

The Go SDK exposes `OpenPages`/`PageStream.Next` and explicit `PageLease.Release`.
`Get` forces ordered delivery and releases consumed leases behind `io.Reader`;
`DownloadTo` writes leases to `io.WriterAt` at absolute object offsets and releases
each after WriteAt returns. Every path uses one POST and one connection per
subscription, with no continuation requests. Always close abandoned streams or
Values. The SDK buffers complete page slices within credits, not entire objects.
See `pkg/racersdk/subscription.go:70-99,180-246,273-318` and
`pkg/racersdk/value.go:171-197` (repository-relative).

## Origin operations (unchanged v1)

The adapter endpoint retains these operations:

| Request | Success |
| --- | --- |
| `HEAD`, no Range, optional strong `If-Match` | 200, metadata, `Content-Length: <total>`, no body or Content-Range. |
| Bootstrap `GET`, `Range: bytes=0-16777215`, no If-Match | Nonempty: 206, page zero (possibly short). Empty: 200, metadata, `Content-Length: 0`, no Content-Range. |
| Pinned `GET`, one Range and one strong `If-Match` | 206, exact normalized range of that version; never downgrade to 200. |

GET without Range and any other unpinned GET range are invalid. Every 206 has
`Content-Range: bytes first-last/N`, nonzero
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

## Client progress and deadlines

Client subscriptions bound outstanding page/byte leases, not total object pages.
The dataplane bounds acquisition attempts and forwarded links per selection.
Idle/request-head reception and initial metadata/first-page acquisition
remain bounded by `RACER_REQUEST_TIMEOUT_MS` (default 30000). After success headers,
each newly admitted distinct page gets a fixed child acquisition deadline of that
duration and bounded attempts/links. Already admitted pages, retries, and peer
forwarding never renew their deadline or credits. Explicit aggregate-budget APIs
and peer operations retain their absolute deadlines.

Client body writes instead use `RACER_READER_STALL_TIMEOUT_MS`: positive socket
write progress resets the stall clock, allowing progressing multipage objects to
outlive the old absolute request timeout. Pending prefetch is polled during writes
without growing admitted credit usage. Full caller disconnect cancels pending acquisition.
A write-half-close is not a release and cannot sustain a subscription that needs
more credits. Submitted I/O retains
its buffers and admission until completion fences, including after cancellation.
Deadline, overload, or acquisition failure can still truncate a started stream.

The Go context covers the subscription until completion or Close. The default
60-second ResponseHeaderTimeout covers POST write plus response-head read on one
deadline; BodyReadTimeout defaults to 60 seconds per blocked read/release write,
not a total object deadline (`pkg/racersdk/subscription.go:121-126,257-292`).

Fresh HEAD/subscription and origin bootstrap missing objects return 404. A valid pin whose version no
longer exists (including object deletion) returns 412. Credential rejection remains
401/403, and transient failures remain 503, not 412. Resolve version before range
satisfiability; do not use current-version size to answer an old pin.

## Errors and stream boundaries

| Status | Meaning |
| --- | --- |
| 400 | Invalid target, request headers, method-specific fields, or range syntax |
| 401 / 403 | Origin credential rejected / access forbidden |
| 404 | Unpinned object missing |
| 405 | Unsupported method; client includes `Allow: HEAD, POST`, origin includes `Allow: HEAD, GET` |
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

There are real detection limits. Subscription frames and Complete counts validate
the selected membership and byte count, not an end-to-end object digest. Sequential
origin HTTP body readers expose only Content-Length bytes; surplus bytes may
corrupt a subsequent message. A guard can reject observed illegal
framing, but cannot prove the peer will never send additional bytes. Likewise,
disconnect after exactly the advertised bytes cannot convey a late producer error.
Draining/probing an HTTP response body does not prove transport-level absence of
extra bytes. The origin callback probe provides stronger local checking; it cannot
repair incorrect bytes with the right length and ETag. No end-to-end hash is added.

## Implementation boundary

Use bounded raw-head validation at Unix connections where
normalization would erase duplicate lengths, whitespace, or exact target syntax.
Account for stdlib read-ahead and sequential fixed-length bodies; do not scan body
bytes for header delimiters. The subscription SDK owns credit-bounded page slices;
the unchanged origin server uses stdlib HTTP with a raw-head guard. `MaxHeaderBytes` alone
is not an exact 32 KiB wire-head limit. Conformance tests must use raw Unix peers,
not only `httptest` handlers, to catch parser normalization and automatic error
body behavior. Runtime/parser-generated errors must also have empty bodies when
a response can be sent safely. The Go SDK implements the guards in
`pkg/racersdk/subscription.go`, `pkg/racersdk/response_conn.go`, and
`pkg/racersdk/origin_conn.go` (repository-relative
paths); stock `net/http` alone does not enforce every rule above.
