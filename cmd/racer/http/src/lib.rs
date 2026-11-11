//! Strict fixed-length HTTP/1.1 with caller-owned scheduling and resource policy.
//!
//! # Heads and field values
//!
//! [`Codec`] limits the wire size of a message head, not its body. It accepts
//! HTTP/1.1 requests and responses, rejects transfer coding and duplicate
//! Content-Length or Host fields, and leaves body limits to [`connection`].
//! Response codes from 100 to 599 are valid at the codec boundary; the connection
//! exchange layer rejects informational responses and upgrades.
//!
//! Header names retain their spelling and order, and ordinary repeated fields
//! remain separate. By default decoding removes at most one separator space
//! after the colon and preserves all remaining value bytes, because callers
//! (for example origin numeric-field validation) deliberately reject padded
//! values. [`Codec::trim_ows`] opts in to RFC 9110 section 5.5 behavior, which
//! removes optional whitespace (SP and HTAB) at both edges of ordinary field
//! values. Interior bytes, including tabs and non-ASCII bytes, are preserved.
//! [`Opaque::NAMES`] selects case-insensitive names that instead require exactly
//! one separator space and no leading or trailing space or tab in the value.
//! That strictness is deliberate: opaque values such as credentials and signed
//! metadata are compared and signed byte for byte, so the codec refuses any
//! spelling that a peer or intermediary could normalize differently rather
//! than silently trimming it.
//!
//! The `Connection` field accepts any comma-separated token list with optional
//! whitespace and empty elements. Only `close` and `keep-alive` affect behavior;
//! tokens naming framing fields (`content-length`, `transfer-encoding`, `host`)
//! are rejected so that hop-by-hop stripping can never remove framing.
//!
//! [`small`] decodes bounded chunked, close-delimited, or fixed-length response
//! bodies into a capped buffer. Transfer coding is accepted only through that
//! explicit mode; [`MessageHead::content_length`] keeps rejecting it.
//! [`Header`] clears its value bytes on drop. Heads intentionally have no `Debug`
//! implementation, so ordinary diagnostic formatting cannot expose those values.
//!
//! [`small_exchange`] sends one serialized request and receives a response
//! through [`small`] on a pooled connection, for control-plane clients that
//! must accept chunked or close-delimited bodies.
//!
//! [`ContentType`] preserves accepted MIME bytes without normalizing them.
//! [`range`] parses single byte ranges while leaving numeric policy to the caller.
//!
//! # Connections and policy
//!
//! [`connection::Context`] supplies the reactor, scope, admission charges, outbound
//! slots, endpoint policy, and per-connection state. Charges cover selected owned
//! buffers and decoded-head storage; they are not total allocator accounting and
//! are independent of the reactor's completion reserve. There is no executor, DNS
//! resolver, or background maintenance task in this crate. The caller drives the
//! reactor and polls pool waiters. Idle maintenance budgets count endpoints, not
//! sockets; the same budget separately bounds waiter wakeups.
//!
//! [`connection::State`] hooks customize admission, signing, and session handling.
//! Its default `attach` replaces state, and its default `idle` returns state
//! unchanged. Implementors must provide any transient-attachment cleanup they
//! need. Leases retain buffers and charges through completion and cancellation
//! fences, and unfinished exchanges cannot return reusable connections to a pool.
//! Read-ahead beyond message framing must be retained so exchange completion can
//! reject pipelining. HEAD and 304 representation lengths do not allocate bodies.
//!
//! # Body transfer profiles
//!
//! [`delivery`] sends immutable backing through a staging pipe and owned sends;
//! [`relay`] transfers opaque fixed-length bodies with bounded synchronous steps.
//! Both adapt [`flow_control::PipeLease`] without selecting application
//! authorization, telemetry, quotas, or scheduling policy. Callers retain exchange
//! finalization and must keep complete transfer owners through completion fences.
//!
//! The optional `test-util` feature exposes connection framing, pool inspection,
//! and deterministic relay fallback helpers without an application dependency.

#![warn(missing_docs)]

pub mod connection;

pub mod small_exchange;

pub mod tls;

mod transfer;

pub use transfer::{delivery, relay};

pub use racer_wire::http::*;
