//! Racer worker-local I/O: one concrete io_uring reactor, one local executor,
//! cross-thread queues, CPU pinning, and secure filesystem primitives.
//!
//! The reactor drives either the Linux kernel (`Driver::Uring`) or a
//! deterministic simulated operating system (`Driver::Sim`), chosen at
//! construction. Simulation is ordinary code, not a cargo feature: time and
//! randomness arrive through an explicit [`WorkerCtx`], never thread-local state.
#![deny(unsafe_op_in_unsafe_fn)]

pub mod cancel;
pub mod ctx;
pub mod error;
pub mod exec;
pub mod fs;
pub mod pin;
pub mod ring;
pub mod scope;
pub mod scoped;
pub mod signal;
pub mod sim;
pub mod xthread;

pub use cancel::{CancelToken, Deadline};
pub use ctx::{Clock, Rng, SimClock, WorkerCtx};
pub use error::{Error, Result};
pub use ring::{Driver, Env, IoClass, OpCtx, Reactor};
pub use scope::{CancelWait, Operation, Scope};
