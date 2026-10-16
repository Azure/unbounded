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
//! # Cancellation
//!
//! Operations run under a caller-defined [`Scope`]. A [`CancelToken`] is the
//! simplest scope; applications typically implement [`Scope`] for their own
//! request or connection context.
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

pub mod backend;
pub mod buf;
pub mod cancel;
pub mod clock;
pub mod error;
pub mod fd;
pub mod net;
pub mod scope;

pub use backend::{Backend, Host};
pub use buf::{BufResult, IoBuf, IoBufMut};
pub use cancel::{CancelRegistration, CancelToken, Canceled};
pub use clock::{Clock, Entropy, SystemClock, SystemEntropy};
pub use error::{Error, ErrorKind, Result};
pub use fd::Fd;
pub use net::{SockAddr, SocketAddress};
pub use scope::{Scope, Unscoped};
