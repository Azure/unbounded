# Client integration

`ClientListeners::new` is side-effect-free. The worker calls
`reconcile(&[CacheDefinition], &RequestScope)` to bind sockets, then
`poll_budgeted(usize)` alongside reactor polling. `stop_admission()` retires
listeners and idle keepalive connections; `drain(&RequestScope)` bounds active
response completion. Removed cache generations cannot begin another request.

Read requests use `Head`, `HeadPinned { etag }`, `Bootstrap`, and
`Pinned { etag, range }`. `ReadService::read` receives the original opaque
context. Return the complete immutable object identity in metadata. A successful
nonempty GET must include the exact resolved range and its `RangeStream`.
Client delivery calls `RangeStream::next_slice()` and
`Delivery::finish_to(ReaderLease, ConnectionLease, &RequestScope)`; it checks the
total transmitted length and closes on any post-header error.

Use `Error::UnsatisfiableRangeWithLength(selected_length)` for SDK-valid 416.
Bare `UnsatisfiableRange` cannot produce the required Content-Range and maps to
500. Existing variants cover other endpoint statuses: `MethodNotAllowed`,
`HeaderTooLarge`, `OriginRejected`, `OriginForbidden`, `NotFound`,
`VersionUnavailable`, `BadGateway`, and transient failures. Pinned NotFound is
normalized to VersionUnavailable. No new client-specific shared errors needed.

HTTP construction must provide admission (`HttpIo::with_admission`) and a body
limit that supports signed-63-bit object/range lengths. Client head reception is
independently capped at 32768 bytes. Accepted sockets transfer into
`ConnectionLease::from_accepted` and remain completion-owned throughout I/O.

Client ingress uses `HttpIo::receive_request_head_limited(connection, scope,
limit) -> Operation<HeadCompletion<Result<MessageHead>>>`. Inner protocol errors
retain a fenced, poisoned lease for zero-body 400/431 responses; outer transport
failures remain terminal. The codec preserves raw opaque header bytes and
requires exactly one separator space.

The listener passes the parser's configured cap to this raw framing operation.
`RequestParser::parse` consumes an already framed head; it cannot recover its
original wire length from decoded fields. Its decoded-input bound is separate
from the wire cap and does not assume optional whitespace on unknown headers.

Owned tests cover SDK parsing/headers, exact head limits, actual UDS permissions
and lifecycle, keepalive removal, single-budget accept fairness, nonempty range
delivery, late acquisition truncation, status mapping, and immutable metadata
validation. Run `cargo test --manifest-path cmd/racer-dataplane/Cargo.toml
client:: --lib` from the worktree root.
