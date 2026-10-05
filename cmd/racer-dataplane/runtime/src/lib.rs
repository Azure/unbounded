//! Explicitly driven worker-local I/O and pinned thread groups.
//!
//! Scheduling and resource policy belong to the caller. Simulation is opt-in.
#![deny(unsafe_op_in_unsafe_fn)]

pub mod affinity;
pub mod channel;
pub mod clock_observer;
mod cooperative;
pub use cooperative::{Busy, drive_local_with, poll_scoped, thread_waker, yield_now};
pub mod deadline;
pub mod deadline_registry;
pub mod drivers;
pub mod environment;
pub mod group;
pub mod hedge;
pub mod mailbox;
pub mod offload;
pub mod reactor;
mod retry;
pub use retry::retry_listener;
#[cfg(any(test, feature = "test-util"))]
pub mod test_util;

use std::{future::Future, pin::Pin};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    Cancelled,
    DeadlineExceeded,
    Overloaded,
    Unavailable,
    InvalidInput,
    InvalidConfiguration,
    NotFound,
    AlreadyExists,
    Io,
}

pub type Result<T, E = Error> = std::result::Result<T, E>;
/// Worker-local future; no hidden executor and no Send requirement.
pub type Operation<'a, T, E = Error> = Pin<Box<dyn Future<Output = Result<T, E>> + 'a>>;

/// Caller-owned operation deadline and cancellation policy.
pub trait Scope: Clone + 'static {
    type Error: Copy + Send + 'static + From<Error>;

    fn check(&self) -> Result<(), Self::Error>;

    fn cancellation(&self) -> Option<&deadline::Cancellation> {
        None
    }
}

/// Accounting hook. Charges stay alive until their resources are fenced.
pub trait Budget: 'static {
    type Charge: 'static;

    fn charge(&self, bytes: usize) -> Result<Self::Charge>;
}

impl Budget for () {
    type Charge = ();

    fn charge(&self, _bytes: usize) -> Result<()> {
        Ok(())
    }
}
