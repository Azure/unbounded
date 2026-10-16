//! Deterministic simulation backend for [`uring_runtime`].
//!
//! [`SimBackend`] implements [`uring_runtime::Backend`] and
//! [`uring_runtime::Host`] on top of an in-memory [`World`]: a simulated
//! filesystem with durable and volatile images and crash injection
//! ([`Disk`]), a simulated network with streams, listeners, datagrams, pipes
//! and partitions, typed fault injection ([`Fault`], [`FaultTarget`]), a
//! seeded manual clock ([`SimClock`]) and a seeded entropy stream
//! ([`SimEntropy`]). Nothing reads host time, randomness or the host
//! filesystem, and every table is ordered, so running the same program with
//! the same seed replays the same execution; compare [`World::trace`]s to
//! check.
//!
//! The simulator is a separate crate rather than a cargo feature, so it can
//! never change the behavior of the production backend.
//!
//! # Example
//!
//! Drive the backend directly through the [`Backend`](uring_runtime::Backend) trait (normally the
//! `uring_runtime` reactor does this):
//!
//! ```
//! use std::time::Duration;
//! use uring_runtime::backend::{BackendConfig, KernelTimespec, Op, OpKind};
//! use uring_runtime::{Backend, Clock};
//! use uring_runtime_sim::{
//!     DEFAULT_ENTROPY_DOMAIN, Fault, FaultTarget, SimBackend, SimClock, SimEntropy, World,
//! };
//!
//! let world = World::new();
//! let clock = SimClock::new(7);
//! let mut backend = SimBackend::new(&world, clock.clone(), SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, 7, 0));
//! backend.init(&BackendConfig::new(64)).unwrap();
//!
//! // A file fixture, read through an asynchronous operation with a short-read fault.
//! world.write_file("/data/greeting", b"hello world").unwrap();
//! let file = world.open("/data/greeting", libc::O_RDONLY).unwrap();
//! world.inject(FaultTarget::Op(OpKind::Read), Fault::Short(5)).unwrap();
//! let mut buf = vec![0u8; 64];
//! let read = Op::Read { fd: &file, offset: 0, buf: buf.as_mut_ptr(), len: 64 };
//! // SAFETY: `buf` and `file` outlive the completion popped below.
//! unsafe { backend.push(1, &read) }.unwrap();
//! backend.submit().unwrap();
//! let done = backend.pop().unwrap();
//! assert_eq!(done.result, 5);
//! assert_eq!(&buf[..5], b"hello");
//!
//! // Timeouts fire when the simulated clock reaches their deadline.
//! let mut timespec = KernelTimespec::default();
//! let deadline = clock.now() + Duration::from_secs(1);
//! let timeout = Op::Timeout { deadline, timespec: &mut timespec };
//! // SAFETY: `timespec` outlives the completion.
//! unsafe { backend.push(2, &timeout) }.unwrap();
//! backend.submit().unwrap();
//! assert!(backend.pop().is_none());
//! clock.advance(Duration::from_secs(1));
//! assert_eq!(backend.pop().unwrap().result, -libc::ETIME);
//! ```
//!
//! # Modules
//!
//! - [`world`] - the shared [`World`] and the [`SimFd`] handle.
//! - [`fs`] and [`disk`] - the filesystem and its durability model.
//! - [`net`] - streams, listeners, datagrams, pipes, readiness, partitions.
//! - [`fault`] - typed fault injection.
//! - [`trace`] - the bounded event trace.
//! - [`clock`] and [`entropy`] - deterministic time and randomness.
//! - [`backend`] - [`SimBackend`].

#![cfg_attr(docsrs, feature(doc_cfg))]

pub mod backend;
pub mod clock;
pub mod disk;
pub mod entropy;
pub mod fault;
pub mod fs;
pub mod net;
pub mod trace;
pub mod world;

pub use backend::SimBackend;
pub use clock::{DEFAULT_WALL_EPOCH, SimClock};
pub use disk::{Disk, DiskState};
pub use entropy::{DEFAULT_ENTROPY_DOMAIN, SimEntropy};
pub use fault::{Fault, FaultTarget, SyncOp};
pub use fs::Metadata;
pub use trace::{Event, EventKind};
pub use world::{SimFd, World};
