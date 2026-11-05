//! A scoped reactor on the thread-selected backend for filesystem tests.
use crate::rt::{
    Error, Operation, Result, Scope,
    environment::{Cancellation, Deadline},
};
use std::{num::NonZeroUsize, ops::Deref, rc::Rc, time::Instant};

/// Queue ceiling selected by each regression.
pub struct Limits {
    /// Maximum accepted operations across ordinary and reserved partitions.
    pub queue_entries: NonZeroUsize,
}

/// Reactor configuration shared by a test.
pub struct Admission {
    /// Queue ceiling used when constructing the associated reactor.
    pub limits: Limits,
}

impl Admission {
    /// Select the queue limit.
    pub fn new(limits: Limits) -> Self {
        Self { limits }
    }
}

/// Scoped reactor on the thread-selected backend.
pub struct Reactor {
    core: racer_io::scoped::Reactor<RequestScope>,
}

impl Reactor {
    /// Construct a lazy reactor using the configured queue ceiling.
    pub fn new(admission: Rc<Admission>) -> Self {
        Self {
            core: racer_io::scoped::Reactor::new(
                admission.limits.queue_entries.get(),
                &crate::rt::reactor::simulation::ambient_env(),
            ),
        }
    }

    /// Fence a snapshot of all current operations while leaving admission open.
    pub fn file_fence(&self, _: ()) -> Operation<'_, ()> {
        self.core.fence_matching(|_| true)
    }
}

impl Deref for Reactor {
    type Target = racer_io::scoped::Reactor<RequestScope>;

    fn deref(&self) -> &Self::Target {
        &self.core
    }
}

/// Deadline and cancellation policy retained independently by each operation.
#[derive(Clone)]
pub struct RequestScope {
    /// Absolute deadline in the active clock domain.
    pub deadline: Deadline,

    /// Shared cancellation state, checked before deadline expiration.
    pub cancellation: Cancellation,
}

impl Scope for RequestScope {
    type Error = Error;

    /// Give cancellation precedence over expiration in the active clock domain.
    fn check(&self) -> Result<()> {
        if self.cancellation.is_cancelled() {
            Err(Error::Cancelled)
        } else if crate::rt::environment::now() >= self.deadline.0 {
            Err(Error::DeadlineExceeded)
        } else {
            Ok(())
        }
    }
}

/// Fresh cancellation state and a five-second deadline.
pub fn scope() -> RequestScope {
    RequestScope {
        deadline: Deadline(Instant::now() + std::time::Duration::from_secs(5)),
        cancellation: Cancellation::new().unwrap(),
    }
}

impl RequestScope {
    /// Request cancellation without treating the request as a kernel fence.
    pub fn cancel(&self) -> Result<()> {
        self.cancellation.cancel()
    }
}
