//! Worker-local io_uring ownership and completion fences.
//!
//! Before submission, the reactor must own the buffer, FD, and associated leases
//! in its in-flight table, independently of the waiting future. Dropping a future
//! only abandons its result; it cannot release submitted resources. Cancellation
//! requests do not release them: original and cancellation completion accounting
//! must both finish. Shutdown must fence kernel access before dropping the table.
//! These are implementation requirements; this scaffold submits no I/O.

use super::{admission::Admission, deadline::RequestScope};
use crate::error::{Operation, Result, deferred};
use std::{any::Any, cell::RefCell, collections::HashMap, os::fd::OwnedFd, rc::Rc};

pub struct Reactor {
    admission: Rc<Admission>,
    /// Type-erased `InFlight<B, L>` owners, never borrowed from a waiting future.
    in_flight: RefCell<HashMap<IoId, Box<dyn Any>>>,
}
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct IoId(pub u64);

pub(crate) mod sealed {
    pub trait Sealed {}
}

/// An exclusively owned, stable backing allocation with an independent lifetime.
///
/// Only audited crate types may implement this trait. Moving the owner or calling
/// either accessor must not relocate or resize its backing allocation. Accessors
/// expose the same initialized region; there must be no independently accessible
/// mutable aliases. Ownership includes the allocation's quota reservation. Neither
/// the allocation nor that reservation may be freed/recycled before the final fence.
/// `'static` excludes request-scoped borrows; it does not require leaking memory.
///
/// Inline storage is address-unstable when its owner moves and cannot opt in:
/// ```compile_fail
/// use racer_dataplane::{error::Result, runtime::reactor::IoBuffer};
/// struct Inline([u8; 16]);
/// impl IoBuffer for Inline {
///     fn bytes(&self) -> Result<&[u8]> { Ok(&self.0) }
///     fn bytes_mut(&mut self) -> Result<&mut [u8]> { Ok(&mut self.0) }
/// }
/// ```
/// Borrowed storage cannot opt in either:
/// ```compile_fail
/// use racer_dataplane::{error::Result, runtime::reactor::IoBuffer};
/// struct Borrowed<'a>(&'a mut [u8]);
/// impl IoBuffer for Borrowed<'_> {
///     fn bytes(&self) -> Result<&[u8]> { Ok(self.0) }
///     fn bytes_mut(&mut self) -> Result<&mut [u8]> { Ok(self.0) }
/// }
/// ```
/// Both supported buffer types have independently owned lifetimes:
/// ```
/// use racer_dataplane::{memory::pool::PlaintextBuffer,
///     runtime::reactor::IoBuffer, store::direct::AlignedBuffer};
/// fn independent<T: 'static>() {}
/// fn completion_safe<B: IoBuffer>() { independent::<B>(); }
/// completion_safe::<PlaintextBuffer>();
/// completion_safe::<AlignedBuffer>();
/// ```
pub trait IoBuffer: sealed::Sealed + 'static {
    fn bytes(&self) -> Result<&[u8]>;
    fn bytes_mut(&mut self) -> Result<&mut [u8]>;
}

/// Reactor-owned submission state. `L` retains connection/segment/other leases.
/// Stored before the kernel can see any pointer, including across partial I/O.
struct InFlight<B: IoBuffer, L: 'static> {
    file: Rc<OwnedFd>,
    buffer: B,
    lease: L,
}

/// Resources return to the caller only after all applicable completion fences.
/// On error or an abandoned future, the reactor releases them after those fences.
pub struct Completion<B: IoBuffer, L: 'static = ()> {
    pub buffer: B,
    pub bytes: usize,
    pub lease: L,
}

impl Reactor {
    pub fn new(admission: Rc<Admission>) -> Self {
        Self {
            admission,
            in_flight: RefCell::new(HashMap::new()),
        }
    }
    /// Transfer the FD, buffer, and any reuse-preventing lease (`()` if none).
    /// Owned resources can move from one completed operation to the next:
    /// ```no_run
    /// use std::{os::fd::OwnedFd, rc::Rc};
    /// use racer_dataplane::{error::Result,
    ///     runtime::{deadline::RequestScope, reactor::{Completion, Reactor}},
    ///     store::{direct::AlignedBuffer, segment::SegmentLease}};
    /// async fn copy(reactor: &Reactor, fd: Rc<OwnedFd>, buffer: AlignedBuffer,
    ///     lease: SegmentLease, scope: &RequestScope)
    ///     -> Result<Completion<AlignedBuffer, SegmentLease>> {
    ///     let read = reactor.read_at(fd.clone(), 0, buffer, lease, scope).await?;
    ///     reactor.write_at(fd, 0, read.buffer, read.lease, scope).await
    /// }
    /// ```
    /// The retained lease cannot borrow from the waiting future's caller:
    /// ```compile_fail
    /// use std::{os::fd::OwnedFd, rc::Rc};
    /// use racer_dataplane::{memory::pool::PlaintextBuffer,
    ///     runtime::{deadline::RequestScope, reactor::Reactor},
    ///     store::segment::SegmentLease};
    /// fn borrowed(reactor: &Reactor, fd: Rc<OwnedFd>, buffer: PlaintextBuffer,
    ///     lease: &SegmentLease, scope: &RequestScope) {
    ///     let _future = reactor.read_at(fd, 0, buffer, lease, scope);
    /// }
    /// ```
    pub fn read_at<'a, B: IoBuffer, L: 'static>(
        &'a self,
        _file: Rc<OwnedFd>,
        _offset: u64,
        _buffer: B,
        _lease: L,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Completion<B, L>> {
        deferred("reactor.read_at")
    }
    pub fn write_at<'a, B: IoBuffer, L: 'static>(
        &'a self,
        _file: Rc<OwnedFd>,
        _offset: u64,
        _buffer: B,
        _lease: L,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Completion<B, L>> {
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
