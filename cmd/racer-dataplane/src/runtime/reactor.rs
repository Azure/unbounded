//! Racer admission and request policy around the worker-local runtime reactor.
//!
//! Inline storage cannot meet the stable-buffer contract:
//! ```compile_fail
//! use racer_dataplane::{error::Result, runtime::reactor::IoBuffer};
//! struct Inline([u8; 16]);
//! impl IoBuffer for Inline {
//!     type Error = racer_dataplane::error::Error;
//!     fn bytes(&self) -> Result<&[u8]> { Ok(&self.0) }
//!     fn bytes_mut(&mut self) -> Result<&mut [u8]> { Ok(&mut self.0) }
//! }
//! ```
//! Borrowed storage does not have an independent completion lifetime:
//! ```compile_fail
//! use racer_dataplane::{error::Result, runtime::reactor::IoBuffer};
//! struct Borrowed<'a>(&'a mut [u8]);
//! unsafe impl IoBuffer for Borrowed<'_> {
//!     type Error = racer_dataplane::error::Error;
//!     fn bytes(&self) -> Result<&[u8]> { Ok(self.0) }
//!     fn bytes_mut(&mut self) -> Result<&mut [u8]> { Ok(self.0) }
//! }
//! ```
//! Audited production buffers own independent storage:
//! ```
//! use racer_dataplane::{memory::pool::PlaintextBuffer,
//!     runtime::{reactor::IoBuffer, admission::Reservation}};
//! use page_alloc::AlignedBuffer;
//! fn independent<T: 'static>() {}
//! fn completion_safe<B: IoBuffer>() { independent::<B>(); }
//! completion_safe::<PlaintextBuffer>();
//! completion_safe::<AlignedBuffer<Reservation>>();
//! ```
//! Immutable ciphertext cannot be used for receive:
//! ```compile_fail
//! use std::rc::Rc;
//! use racer_dataplane::{memory::pool::CiphertextPage,
//!     runtime::{reactor::{Reactor, Descriptor}, deadline::RequestScope}};
//! fn receive(r: &Reactor, fd: Rc<Descriptor>, page: CiphertextPage, scope: &RequestScope) {
//!     let _ = r.recv(fd, page, (), scope);
//! }
//! ```
use super::{
    admission::{Admission, ConnectionReservation, Reservation},
    deadline::RequestScope,
};
use crate::{
    error::{Error, Operation, Result},
    model::{RequestId, ResourceClass},
};
use std::{ops::Deref, rc::Rc};
pub use uring_runtime::reactor::{
    Completion, Descriptor, IoBuffer, IoId, ReactorWake, SUBMISSION_BYTES, SendBuffer,
    SocketAddress, SubmissionCapacity,
};
pub mod filesystem;
#[cfg(test)]
pub mod simulation;

pub struct AdmissionBudget(Rc<Admission>);
impl uring_runtime::Budget for AdmissionBudget {
    type Charge = Reservation;
    fn charge(&self, bytes: usize) -> uring_runtime::Result<Reservation> {
        self.0
            .reserve_completion(None, ResourceClass::RequestContext, bytes)
            .map_err(|error| match error {
                Error::InvalidConfiguration => uring_runtime::Error::InvalidConfiguration,
                Error::Unavailable => uring_runtime::Error::Unavailable,
                _ => uring_runtime::Error::Overloaded,
            })
    }
}

pub struct Reactor {
    core: uring_runtime::reactor::Reactor<RequestScope, AdmissionBudget>,
    admission: Rc<Admission>,
}
impl Deref for Reactor {
    type Target = uring_runtime::reactor::Reactor<RequestScope, AdmissionBudget>;
    fn deref(&self) -> &Self::Target {
        &self.core
    }
}
impl Reactor {
    pub fn new(admission: Rc<Admission>) -> Self {
        Self {
            core: uring_runtime::reactor::Reactor::new(
                admission.limits().queue_entries.get(),
                AdmissionBudget(admission.clone()),
            ),
            admission,
        }
    }
    pub fn init(&self) -> Result<()> {
        self.core.init().map_err(Into::into)
    }
    pub fn poll_budgeted(&self, budget: usize) -> Result<usize> {
        self.core.poll_budgeted(budget).map_err(Into::into)
    }
    pub fn wait(&self, duration: std::time::Duration) -> Result<()> {
        self.core.wait(duration).map_err(Into::into)
    }
    pub fn waker(&self) -> Result<ReactorWake> {
        self.core.waker().map_err(Into::into)
    }
    pub fn file_buffer(&self, length: usize) -> Result<filesystem::Buffer> {
        if length == 0 {
            return Err(Error::InvalidConfiguration);
        }
        self.core
            .file_buffer(length)
            .map(filesystem::Buffer)
            .map_err(Into::into)
    }
    pub fn file_bytes(&self, bytes: &[u8]) -> Result<filesystem::Buffer> {
        if bytes.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        self.core
            .file_bytes(bytes)
            .map(filesystem::Buffer)
            .map_err(Into::into)
    }
    pub fn reserve_connection(&self, role: ResourceClass) -> Result<ConnectionReservation> {
        self.admission.reserve_connection(role)
    }
    pub(crate) fn reserve_submissions(
        &self,
        slots: Reservation,
        memory: Reservation,
    ) -> Result<Rc<SubmissionCapacity>> {
        let capacity = slots.amount();
        slots.validate(ResourceClass::ControlProgress, capacity)?;
        memory.validate(
            ResourceClass::RequestContext,
            capacity
                .checked_mul(SUBMISSION_BYTES)
                .ok_or(Error::InvalidConfiguration)?,
        )?;
        if !self.admission.owns(&slots) || !self.admission.owns(&memory) {
            return Err(Error::InvalidConfiguration);
        }
        self.core
            .reserve_submissions(capacity, (slots, memory))
            .map_err(|error| match error {
                uring_runtime::Error::InvalidInput => Error::InvalidConfiguration,
                other => other.into(),
            })
    }
    pub fn file_fence(&self, request: RequestId) -> Operation<'_, ()> {
        self.core
            .fence_matching(move |scope| scope.request == request)
    }
}

#[cfg(test)]
mod tests;
