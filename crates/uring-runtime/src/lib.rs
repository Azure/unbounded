//! A small, explicitly driven, thread-per-core io_uring runtime for Linux.
//!
//! `uring-runtime` is a thin, safe layer over io_uring for applications that
//! run one event loop per core and want full control over when work happens.
//! Nothing runs in the background: the application drives the reactor and its
//! tasks from its own loop.
//!
//! # Ownership model
//!
//! io_uring reads and writes memory after the submitting call returns, so
//! every buffer, address and path handed to an operation is moved into the
//! runtime and returned with the result (see [`BufResult`]), on success and on
//! failure. Memory the kernel may still touch is never freed: if an operation
//! cannot be fenced it is leaked rather than released. Buffer types opt in
//! through the unsafe [`IoBuf`] and [`IoBufMut`] traits.
//!
//! # Backend seam
//!
//! The reactor talks to the kernel through the [`Backend`] trait, which
//! queues raw [`Op`](backend::Op) descriptions and reaps completions, and
//! the [`Host`] trait for synchronous resource creation. A deterministic
//! simulator (the `uring-runtime-sim` crate) implements the same traits, so
//! application code is identical under simulation. Time and randomness also
//! come from the backend ([`Clock`], [`Entropy`]); the runtime never reads
//! ambient time itself.
//!
//! # Reactor
//!
//! A [`Reactor`] owns the in-flight operations of one thread. Its typed
//! operation methods ([`read_at`](Reactor::read_at), [`recv`](Reactor::recv),
//! [`accept`](Reactor::accept), [`sleep_until`](Reactor::sleep_until),
//! [`open_at2`](Reactor::open_at2), ...) return lazy futures; the application
//! drives completions with [`Reactor::poll`] and blocks with
//! [`Reactor::wait`]. Operations are admitted against a bounded in-flight
//! partition or a [`Reservation`], and every operation is *fenced* (its
//! original completion and any cancel request completion have arrived)
//! before its memory is released. [`Uring`] is the io_uring backend. See the
//! [`reactor`] module for the full rules.
//!
//! # Cancellation
//!
//! Operations run under a caller-defined [`Scope`]. A [`CancelToken`] is the
//! simplest scope; applications typically implement [`Scope`] for their own
//! request or connection context.
//!
//! # Tasks and time
//!
//! [`LocalExecutor`] runs a bounded set of `!Send` tasks on the current
//! thread with a wake-driven ready queue, and [`Timers`] is a deadline queue
//! that fires [`Sleep`] futures when the application passes it the current
//! time. Both are driven explicitly from the application's loop; see the
//! [`task`] module for how they fit together with a reactor.
//!
//! # Thread groups and CPU affinity
//!
//! [`group`] runs a fixed set of pinned threads ("lanes" and optional
//! "helpers"), each building its services on its own thread and taking them
//! through start, steady-state polling, drain, two ownership fences and
//! shutdown. Panics are caught and reported with their message; a service
//! whose fence cannot be confirmed is leaked rather than freed. [`affinity`]
//! discovers the CPUs, cgroup limits and device NUMA locality visible to the
//! calling thread and pins threads; choosing a placement is left to the
//! application.
//!
//! # Kernel requirements
//!
//! Linux 5.6 or newer for the core operations, and 5.15 for
//! [`Op::MkDirAt`](backend::Op::MkDirAt). Use [`Backend::supports`] to probe.
//!
//! # Example
//!
//! ```
//! use uring_runtime::{BufResult, CancelToken, Error, ErrorKind, IoBufMut, Scope};
//!
//! // Scopes decide whether work may proceed.
//! let token = CancelToken::new();
//! assert!(token.check().is_ok());
//! token.cancel();
//! assert_eq!(token.check().unwrap_err().kind(), ErrorKind::Canceled);
//!
//! // Operations hand buffers back with their result.
//! let mut buf: Vec<u8> = Vec::with_capacity(4);
//! let total = buf.bytes_total();
//! let outcome: BufResult<usize, Vec<u8>> = BufResult::new(Err(Error::new(ErrorKind::Canceled)), buf);
//! assert!(outcome.result.unwrap_err().is_canceled());
//! assert_eq!(outcome.buf.capacity(), total);
//! ```

#![cfg_attr(docsrs, feature(doc_cfg))]

pub mod affinity;
pub mod backend;
pub mod buf;
pub mod cancel;
pub mod clock;
pub mod error;
pub mod fd;
pub mod group;
pub mod net;
pub mod reactor;
pub mod scope;
pub mod task;
pub mod time;
pub mod uring;

pub use backend::{Backend, Host};
pub use buf::{BufResult, IoBuf, IoBufMut, IoBufs};
pub use cancel::{CancelRegistration, CancelToken, Canceled};
pub use clock::{Clock, Entropy, SystemClock, SystemEntropy};
pub use error::{Error, ErrorKind, Result};
pub use fd::Fd;
pub use net::{SockAddr, SocketAddress};
pub use reactor::{Reactor, ReactorConfig, Reservation};
pub use scope::{Scope, Unscoped};
pub use task::{JoinHandle, LocalExecutor, Spawner};
pub use time::{Sleep, Timers};
pub use uring::Uring;
