//! Monotonic request deadlines and cancellation propagated through every attempt.

use crate::{
    error::{Result, pending},
    model::identity::RequestId,
};
use std::time::Instant;

#[derive(Clone, Copy, Debug)]
pub struct Deadline(pub Instant);

/// Cancellation is shared within a request but never resets its original deadline.
#[derive(Clone)]
pub struct Cancellation {
    state: std::sync::Arc<std::sync::atomic::AtomicBool>,
}
impl Cancellation {
    pub fn new() -> Result<Self> {
        pending("deadline.cancellation")
    }
}

#[derive(Clone)]
pub struct RequestScope {
    pub request: RequestId,
    pub deadline: Deadline,
    pub cancellation: Cancellation,
}

impl RequestScope {
    pub fn check(&self) -> Result<()> {
        pending("deadline.check")
    }
    pub fn cancel(&self) -> Result<()> {
        pending("deadline.cancel")
    }
}

#[cfg(test)]
mod tests {
    // Exercise original-deadline preservation, cancellation, and timer ordering.
}
