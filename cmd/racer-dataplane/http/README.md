# http1

Private Rust 2024 package for strict fixed-length HTTP/1.1 over `uring-runtime`.
It has no application imports, executor, DNS resolver, or background tasks.

## Codec

`Codec<O: Opaque = ()>::new(header_limit)` accepts one limit, in wire bytes.
`Opaque::NAMES` is a static list of case-insensitive field names requiring exactly
one separator space and no leading/trailing value whitespace. `()` selects none.
Other values preserve spelling, order, duplicates, and opaque value bytes.
`Header` zeroizes its value on drop; raw heads intentionally have no `Debug`.
Errors are `Malformed` and `HeadTooLarge`. Body limits belong to I/O, not parsing.
Transfer coding, ambiguous framing, upgrades, and informational exchanges are
not supported. HEAD and 304 representation lengths do not allocate bodies.

## Runtime and policy

Implement `connection::Context` with associated `Error`, runtime `Scope`,
`Budget`, reactor wrapper (`Deref` to the runtime reactor), `Charge`, `Slot`,
`Opaque`, `State`, and ordered `Endpoint`. Its `charge(bytes)`, `outbound_slot()`,
and `stopped()` hooks keep admission policy with the caller. HTTP allocations
use `charge`, independently of the reactor's completion reserve.

`State` owns session and transient attachments. `admit` and `sign` consume and
return the head. `finished` records completed exchanges; `connect_failed`
observes a completed connect errno only while the scope remains valid.
`attach` installs checkout-local state before connect submission, including on
a reused session. `idle(self)` clears transient attachments while retaining the
session. Policy transformations and evicted state drops occur outside pool
borrows, allowing attachments to return other leases safely.

`Endpoint::address` resolves an already-selected `SocketAddress`. `capacity`,
`priority_headroom`, and `allocation` supply endpoint policy. Priority headroom
defaults to zero. `PoolConfig` controls endpoint/secondary caps, endpoint count,
waiter count, idle timeout, and TCP NODELAY (never applied to Unix sockets).

## Ownership

`HttpIo::new(reactor, codec, context, receive_limit, send_limit)` constructs I/O.
Head and body operations transfer `ConnectionLease` ownership into and out of
the reactor. Partial reads return byte counts; writes send the complete range.
Buffers and charges remain alive through completion/cancellation fences.
The caller drives the reactor and calls `HttpPool::poll_waiters` on bounded ticks.

Lease fields are private. Accessors expose `socket`, `slot`, `state`, `state_mut`,
`receive_remaining`, `send_remaining`, and `closing`. `consume_received` and
`consume_sent` reject underflow. `begin_io`, `next_round`, `finish_exchange`,
`is_reusable`, and `poison` control reuse. Unfinished leases close on drop.
`take_read_ahead` poisons reuse if the tail exceeds framing;
`restore_read_ahead` checks allocation bounds. Never discard a relay's excess
tail: preserve it so `finish_exchange` rejects pipelining.

The opt-in `test-util` feature exposes framing setup, pool snapshots, forced
expiry, framing inspection, and retained-buffer accounting for caller tests.
Production builds do not need it. Package tests require no application crate.
