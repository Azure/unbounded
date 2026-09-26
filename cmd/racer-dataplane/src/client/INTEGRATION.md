# Client integration

## Prepared publication and cache retirement

Exact worker-local API:

```rust
ClientListeners::prepare<'a>(&'a self, caches: &'a [CacheDefinition], scope: &'a RequestScope)
    -> Operation<'a, PreparedListeners>;
PreparedListeners::definitions(&self) -> &[CacheDefinition];
PreparedListeners::commit(self); // infallible, no filesystem/network I/O
ClientListeners::stop_cache(&self, cache: &CacheId);
ClientListeners::cancel_cache(&self, cache: &CacheId) -> Result<()>;
ClientListeners::active_connections_for(&self, cache: &CacheId) -> usize;
ClientListeners::drain_cache<'a>(&'a self, cache: &'a CacheId, scope: &'a RequestScope)
    -> Operation<'a, ()>;
```

`PreparedListeners` is exported from `client::listener`, owns its worker-local
resources without borrowing `ClientListeners`, and implements
`control::caches::CacheTransition`. Await `prepare` before control publication;
store the result in the application lifecycle adapter. Synchronous
`CacheLifecycle::stage(definitions)` compares the complete definitions with
`prepared.definitions()`, takes the matching prepared value, and returns
`Box::new(prepared)`. It performs no I/O. Reject absent/mismatched prepared state.
Prepare all workers before publication; only the designated socket-owning worker
prepares these filesystem listeners. Other workers prepare their own read/cache
resources. Keep prepare/commit/drop serialized with stop/cancel/drain lifecycle
operations. Only one listener transition may be outstanding (`Overloaded` otherwise).

Preparation binds randomly named sockets in pinned no-symlink client directories,
sets permissions before publishing any changed path, then uses `renameat2` with
`RENAME_EXCHANGE` for owned replacements or `RENAME_NOREPLACE` for additions.
Old accepted connections and listener owners remain alive. **During preparation,
changed canonical paths already route new connections to the prepared socket's
backlog; requests are not accepted there until commit.** This is the necessary
publication boundary for a commit that performs no fallible filesystem I/O.
Dropping preparation restores exchanged owned inodes and removes its additions.
Rollback never overwrites an externally substituted inode; if external code
replaces a prepared pathname, the last-good descriptor remains owned but pathname
restoration cannot be guaranteed without overwriting that foreign entry.

Commit swaps listener maps and retires old generations entirely in memory.
Worker `poll_budgeted` subsequently cleans up old owned socket names. Unchanged
definitions preserve descriptors. Permission changes and UID/name reuse create a
new listener generation rather than chmod an active socket. `reconcile` remains
compatible as prepare + commit + immediate cleanup.

`stop_cache` retires admission and idle keepalives but lets active responses finish.
`cancel_cache` additionally signals all accepted connections for that UID, across
generations; it retains completion owners. `drain_cache` stops admission and polls
until those connection futures finish, canceling them on its deadline. Continue
driving the shared reactor while draining. This is a client-future drain, **not a
kernel completion fence**: application retirement must subsequently await the
runtime's accepted-operation fence before releasing other cache resources. Other
cache UIDs remain serving. Global shutdown cannot be reversed by a prepared commit.

`ClientListeners::new` is side-effect-free. The worker calls
`reconcile(&[CacheDefinition], &RequestScope)` to bind sockets, then
`poll_budgeted(&mut Context, usize)` with the retained worker driver waker alongside
reactor polling. Cooperative continuations and completion notifications wake the
worker; its bounded 1 ms fallback still drives deadlines, nonblocking accepts, and
round-robin passes over blocked connections. `stop_admission()` retires
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
