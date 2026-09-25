# HTTP integration contract

Use `HttpIo::with_admission(reactor, codec, admission)` for operational heads.
`HttpIo::new` remains composition-compatible but returns InvalidConfiguration
when staging is needed. All constructors are side-effect-free.

The worker must drive the same reactor's `poll_budgeted` and wait/wakeup loop.
`recv` and `send` must retain the full owned buffer and ConnectionLease through
original/cancellation completion, independently of waiting future lifetime.
`connect` uses owned SocketAddress::Inet/Unix. Peer endpoints are numeric socket
addresses; this component never performs blocking DNS resolution.

## Endpoint APIs

- `ConnectionLease::from_accepted(OwnedFd, &Admission)` owns an accepted TCP/UDS
  socket, sets NONBLOCK/CLOEXEC, and reserves one connection.
- `HttpIo::capped(header_limit)` shares reactor/admission with a smaller head cap.
- `receive_head_limited(connection, scope, header_limit)` enforces a smaller cap
  before allocation. Codec limits never exceed 32 KiB. SDK semantic field/ETag
  limits remain the endpoint adapter's responsibility.
- `read_body` and `read_body_range(connection, buffer, Range<usize>, scope)` return
  partial reads and never consume bytes beyond the framed Content-Length. Callers
  loop over Completion.bytes to fill an exact range. Empty destinations for an
  unfinished body are errors; EOF before the fixed length is Io.
- `write_body_range` sends the entire specified initialized subrange, retaining
  the backing allocation through each partial completion.
- `BufferRange<B: IoBuffer>` owns a fixed range of an owned stable allocation.
- `buffer(length)` allocates admitted, zeroizing owned staging.
- `exchange_head(connection, request, scope)` sends a bodyless request and returns
  its response head/connection. `collect_body(connection, maximum, scope)` returns
  a bounded OwnedBuffer and its connection; it does not finish the exchange.
- `finish_exchange()` requires both framed bodies fully consumed and no unread
  read-ahead. Dropping that lease returns a healthy connection to its pool.
  Dropping any unfinished, failed, or poisoned exchange closes it.

Framing supports fixed lengths, HEAD, 204 and 304 bodyless responses. It rejects
transfer coding, informational responses/upgrades, duplicate Content-Length and
ambiguous Connection nominations. Generic duplicates remain available in order
for semantic validation. Opaque Authorization and Racer-Metadata require exactly
one separator SP, no leading SP/HTAB in their value, and no trailing SP/HTAB.
Bytes above ASCII remain unchanged. Raw Header values, encoded send scratch, and
staging allocations are zeroized. Consumed head bytes are scrubbed before keeping
body read-ahead. `Codec::encode_head` retains its existing Vec return signature;
direct users must wrap its result in zeroize::Zeroizing when it contains secrets.

## Pool lifecycle and remaining runtime handoff

Pool capacity includes connecting/leased sockets; healthy idle sockets consume
connection quota too. Exhaustion returns Overloaded immediately, without hidden
waiters or automatic request retries. `with_limits` configures endpoint count and
idle timeout. Call `invalidate(endpoint)` on endpoint-generation changes and
`expire_idle()` from lifecycle polling. `close()` stops checkout and closes idle
sockets, while active operations retain their owners.

The current runtime `connect(fd,address,scope)` owns the FD/address but no generic
lease. A dropped connect quarantines its admission/slot until the retained FD
dies; `expire_idle()` reaps it. Normal shutdown drains the reactor, then reaps the
pool. If pool destruction precedes that fence, bounded quarantined accounting
owners are deliberately retained rather than released early. The precise missing
runtime API is `connect_with_lease<L: 'static>(fd, address, lease, scope) ->
Operation<L>`; adopting it removes that exceptional retention. Send/receive
already have the required lease API. No successful connection or I/O is mocked.

## Component verification

Normal suite: `cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --lib http::`.
During concurrent cross-component compilation failures, run
`bash cmd/racer-dataplane/src/http/check-component.sh --nocapture` after dependencies
have built. The standalone crate includes production HTTP/admission/deadline/
reactor sources and real kernel socket tests; only configuration fixtures are
local. Compile-fail ownership examples remain in the normal Cargo doctest suite.
