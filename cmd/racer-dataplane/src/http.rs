//! Strict HTTP/1.1 framing and reactor-driven TCP/Unix socket operations.
//!
//! Construct `HttpIo::with_admission` for operational use. The worker must poll
//! `Reactor::poll_budgeted` and arrange reactor wakeups; these futures do not own
//! an executor. `read_body`/`read_body_range` return partial reads. `write_body`
//! and `write_body_range` send their complete initialized range. Call
//! `ConnectionLease::finish_exchange` only after both directions are consumed;
//! dropping an unfinished lease closes it instead of recycling it.
//!
//! Heads retain duplicate fields for endpoint validation, while rejecting
//! ambiguous framing. The protocol supports fixed-length bodies, no transfer
//! coding, upgrades, informational responses, or pipelined pool reuse. Local
//! HEAD representation lengths are not subject to the body-allocation cap.
pub mod codec;
pub mod io;
pub mod pool;
