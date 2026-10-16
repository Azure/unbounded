//! Typed fault injection.
//!
//! Faults are queued on a [`World`](crate::World) with
//! [`World::inject`](crate::World::inject) and consumed, first in first out,
//! by the next operation that matches their [`FaultTarget`]. Each fault is
//! consumed exactly once.
//!
//! Asynchronous operations ([`Backend::push`](uring_runtime::Backend::push))
//! are matched by their [`OpKind`] when pushed. Synchronous calls (the
//! [`Host`](uring_runtime::Host) methods and the fixture helpers on
//! [`World`](crate::World) and [`SimFd`](crate::SimFd)) are matched by
//! [`SyncOp`] when called. The two never steal each other's faults, except
//! through [`FaultTarget::Any`] or [`FaultTarget::Either`].
//!
//! ```
//! use uring_runtime::backend::OpKind;
//! use uring_runtime_sim::{Fault, FaultTarget, World};
//!
//! let world = World::new();
//! world.inject(FaultTarget::Op(OpKind::Read), Fault::Short(1)).unwrap();
//! world.inject(FaultTarget::Any, Fault::Errno(libc::EIO)).unwrap();
//! assert_eq!(world.pending_faults(), 2);
//! ```

use std::collections::VecDeque;

use uring_runtime::backend::OpKind;
use uring_runtime::{Error, Result};

/// The maximum number of queued faults.
pub const MAX_FAULTS: usize = 16384;

/// What happens to the operation that consumes a fault.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum Fault {
    /// Fail with this errno (`1..=4095`) instead of executing.
    Errno(i32),
    /// Execute, but transfer at most this many bytes. Has no effect on
    /// operations that do not transfer bytes.
    Short(usize),
    /// Asynchronous operations only: wait this many backend turns (calls to
    /// [`submit`](uring_runtime::Backend::submit) or
    /// [`wait`](uring_runtime::Backend::wait)) before executing.
    Delay(usize),
    /// Asynchronous operations only: execute at the normal time, so effects
    /// are visible to other resources, but hold the completion back for this
    /// many backend turns. A held operation can no longer be canceled.
    HoldCompletion(usize),
}

/// A synchronous call site that can consume a [`Fault`].
///
/// [`Fault::Delay`] and [`Fault::HoldCompletion`] have no effect on
/// synchronous calls; a matching one is still consumed.
#[non_exhaustive]
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash, PartialOrd, Ord)]
pub enum SyncOp {
    /// Opening a file ([`World::open`](crate::World::open) and friends).
    Open,
    /// Reading a file synchronously.
    Read,
    /// Writing a file synchronously.
    Write,
    /// Syncing a file or directory synchronously.
    Fsync,
    /// Sending on a stream ([`Host::try_send`](uring_runtime::Host::try_send)).
    Send,
    /// Receiving from a stream ([`Host::try_recv`](uring_runtime::Host::try_recv)).
    Recv,
    /// Reading from a pipe.
    PipeRead,
    /// Writing to a pipe.
    PipeWrite,
    /// Moving bytes from a pipe to a stream.
    Splice,
    /// Sending a datagram.
    SendTo,
    /// Receiving a datagram.
    RecvFrom,
    /// Changing permission bits.
    Chmod,
    /// Renaming a path.
    Rename,
    /// Removing a path.
    Unlink,
    /// Creating a directory.
    Mkdir,
    /// Creating a socket.
    Socket,
    /// Binding a socket.
    Bind,
    /// Listening on a socket.
    Listen,
    /// Connecting a socket synchronously.
    Connect,
    /// Accepting a connection synchronously.
    Accept,
}

/// Which operations a queued [`Fault`] applies to.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum FaultTarget {
    /// The next operation of any kind, synchronous or asynchronous.
    Any,
    /// The next asynchronous operation of this kind.
    Op(OpKind),
    /// The next synchronous call of this kind.
    Sync(SyncOp),
    /// Whichever comes first: the next asynchronous operation of the
    /// [`OpKind`] or the next synchronous call of the [`SyncOp`] (for
    /// example a send issued either through the reactor or directly).
    Either(OpKind, SyncOp),
}

impl FaultTarget {
    fn matches(self, site: FaultTarget) -> bool {
        match self {
            FaultTarget::Any => true,
            FaultTarget::Either(op, sync) => {
                site == FaultTarget::Op(op) || site == FaultTarget::Sync(sync)
            }
            target => target == site,
        }
    }
}

/// A first-in, first-out queue of faults.
#[derive(Debug, Default)]
pub(crate) struct FaultQueue(VecDeque<(FaultTarget, Fault)>);

impl FaultQueue {
    pub(crate) fn push(&mut self, target: FaultTarget, fault: Fault) -> Result<()> {
        if let Fault::Errno(errno) = fault
            && !(1..=4095).contains(&errno)
        {
            return Err(Error::from_errno(libc::EINVAL).with_context("Fault::Errno out of range"));
        }
        if self.0.len() >= MAX_FAULTS {
            return Err(Error::from_errno(libc::ENOSPC).with_context("fault queue full"));
        }
        self.0.push_back((target, fault));
        Ok(())
    }

    /// Remove and return the first fault matching `site`, which must be
    /// [`FaultTarget::Op`] or [`FaultTarget::Sync`].
    pub(crate) fn take(&mut self, site: FaultTarget) -> Option<Fault> {
        let index = self.0.iter().position(|(target, _)| target.matches(site))?;
        self.0.remove(index).map(|(_, fault)| fault)
    }

    pub(crate) fn len(&self) -> usize {
        self.0.len()
    }

    pub(crate) fn clear(&mut self) {
        self.0.clear();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn faults_are_fifo_single_use_and_typed() {
        let mut queue = FaultQueue::default();
        queue
            .push(FaultTarget::Op(OpKind::Write), Fault::Short(3))
            .unwrap();
        queue
            .push(FaultTarget::Sync(SyncOp::Recv), Fault::Errno(libc::EINTR))
            .unwrap();
        queue.push(FaultTarget::Any, Fault::Short(0)).unwrap();
        // Asynchronous recv does not steal the synchronous fault.
        assert_eq!(
            queue.take(FaultTarget::Op(OpKind::Recv)),
            Some(Fault::Short(0))
        );
        assert_eq!(
            queue.take(FaultTarget::Sync(SyncOp::Recv)),
            Some(Fault::Errno(libc::EINTR))
        );
        assert_eq!(queue.take(FaultTarget::Sync(SyncOp::Write)), None);
        assert_eq!(
            queue.take(FaultTarget::Op(OpKind::Write)),
            Some(Fault::Short(3))
        );
        assert_eq!(queue.len(), 0);
    }

    #[test]
    fn either_targets_match_the_first_async_or_sync_site() {
        let mut queue = FaultQueue::default();
        let either = FaultTarget::Either(OpKind::Send, SyncOp::Send);
        queue.push(either, Fault::Short(1)).unwrap();
        queue.push(either, Fault::Short(2)).unwrap();
        assert_eq!(queue.take(FaultTarget::Op(OpKind::Recv)), None);
        assert_eq!(queue.take(FaultTarget::Sync(SyncOp::Recv)), None);
        assert_eq!(
            queue.take(FaultTarget::Sync(SyncOp::Send)),
            Some(Fault::Short(1))
        );
        assert_eq!(
            queue.take(FaultTarget::Op(OpKind::Send)),
            Some(Fault::Short(2))
        );
        assert_eq!(queue.len(), 0);
    }

    #[test]
    fn invalid_and_excess_faults_are_rejected_atomically() {
        let mut queue = FaultQueue::default();
        for errno in [i32::MIN, -1, 0, 4096, i32::MAX] {
            let error = queue
                .push(FaultTarget::Any, Fault::Errno(errno))
                .unwrap_err();
            assert_eq!(error.raw_os_error(), Some(libc::EINVAL));
        }
        assert_eq!(queue.len(), 0);
        for _ in 0..MAX_FAULTS {
            queue.push(FaultTarget::Any, Fault::Errno(4095)).unwrap();
        }
        let error = queue.push(FaultTarget::Any, Fault::Short(1)).unwrap_err();
        assert_eq!(error.raw_os_error(), Some(libc::ENOSPC));
        assert_eq!(queue.len(), MAX_FAULTS);
        queue.clear();
        assert_eq!(queue.len(), 0);
    }
}
