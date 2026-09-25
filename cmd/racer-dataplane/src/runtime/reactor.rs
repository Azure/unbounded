//! Worker-local io_uring ownership and completion fences.
//!
//! Submission transfers the buffer lease to the reactor. Cancellation requests do
//! not release it: both original and cancellation completion accounting must finish.

use super::{admission::Admission, deadline::RequestScope};
use crate::error::{Operation, Result, deferred};
use std::{os::fd::OwnedFd, rc::Rc};

pub struct Reactor {
    admission: Rc<Admission>,
}
#[derive(Clone, Copy, Debug)]
pub struct IoId(pub u64);

/// Buffers remain owned until completion. Implementations must preserve backing
/// allocation addresses while submitted, including direct-I/O aligned allocations.
pub trait IoBuffer {
    fn bytes(&self) -> Result<&[u8]>;
    fn bytes_mut(&mut self) -> Result<&mut [u8]>;
}
pub struct Completion<B> {
    pub buffer: B,
    pub bytes: usize,
}

impl Reactor {
    pub fn new(admission: Rc<Admission>) -> Self {
        Self { admission }
    }
    pub fn read_at<'a, B: IoBuffer + 'a>(
        &'a self,
        _file: Rc<OwnedFd>,
        _offset: u64,
        _buffer: B,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Completion<B>> {
        deferred("reactor.read_at")
    }
    pub fn write_at<'a, B: IoBuffer + 'a>(
        &'a self,
        _file: Rc<OwnedFd>,
        _offset: u64,
        _buffer: B,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Completion<B>> {
        deferred("reactor.write_at")
    }
    pub fn cancel_and_fence(&self, _id: IoId) -> Operation<'_, ()> {
        deferred("reactor.cancel_and_fence")
    }
    pub fn drain(&self) -> Operation<'_, ()> {
        deferred("reactor.drain")
    }
}

#[cfg(test)]
mod tests {
    // Inject delayed and reordered CQEs, short I/O, cancellation, and shutdown.
}
