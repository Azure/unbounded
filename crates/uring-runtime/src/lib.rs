//! A small, explicitly driven, thread-per-core io_uring runtime for Linux.
//!
//! `uring-runtime` is a thin, safe layer over io_uring for applications that
//! run one event loop per core and want full control over when work happens.
//! Nothing runs in the background: the application drives the reactor and its
//! tasks from its own loop.
//!
//! # Cancellation
//!
//! Operations run under a caller-defined [`Scope`]. A [`CancelToken`] is the
//! simplest scope; applications typically implement [`Scope`] for their own
//! request or connection context.
//!
//! # Example
//!
//! ```
//! use uring_runtime::{CancelToken, ErrorKind, Scope};
//!
//! let token = CancelToken::new();
//! token.cancel();
//! assert_eq!(token.check().unwrap_err().kind(), ErrorKind::Canceled);
//! ```

#![cfg_attr(docsrs, feature(doc_cfg))]

pub mod cancel;
pub mod clock;
pub mod error;
pub mod scope;

pub use cancel::{CancelRegistration, CancelToken, Canceled};
pub use clock::{Clock, Entropy, SystemClock, SystemEntropy};
pub use error::{Error, ErrorKind, Result};
pub use scope::{Scope, Unscoped};
