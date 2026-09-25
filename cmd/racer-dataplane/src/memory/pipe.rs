//! Pooled per-reader pipes with splice where supported and safe copying otherwise.
//!
//! vmsplice requires proof the kernel released backing pages before reuse. Socket
//! acceptance, write submission, cancellation, or timeout alone are not that proof.
use crate::{
    error::{Result, pending},
    runtime::{admission::Admission, reactor::Reactor},
};
use std::{os::fd::OwnedFd, rc::Rc};
pub struct PipePool {
    admission: Rc<Admission>,
    reactor: Rc<Reactor>,
}
pub struct PipeLease {
    read: OwnedFd,
    write: OwnedFd,
}
impl PipePool {
    pub fn new(admission: Rc<Admission>, reactor: Rc<Reactor>) -> Self {
        Self { admission, reactor }
    }
    pub fn acquire(&self) -> Result<PipeLease> {
        pending("pipe.acquire")
    }
}
#[cfg(test)]
mod tests { /* Independent cursors, fallback, pool exhaustion, kernel-held pages. */
}
