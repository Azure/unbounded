//! Migration shim over [`racer_io::sim`]: a thread-local world selection for the
//! legacy reactor and synchronous syscalls. The simulated OS itself, its
//! resources, faults, and turn-based driver live in `racer-io`. Delete together
//! with this crate; new code selects a world explicitly with `Driver::Sim`.
use super::{BufferOperation, Descriptor, KernelResult, SocketAddress};
use racer_io::sim::compat::{LegacyDriver, LegacyOp, LegacyResult};
pub use racer_io::sim::{CrashDisk, EndpointEnvironment, Event, Fault, Handle, SimOs};
#[cfg(test)]
use std::io;
use std::{cell::RefCell, ffi::CString, ops::Deref, rc::Rc};

thread_local! { static CURRENT: RefCell<Option<Simulation>> = const { RefCell::new(None) }; }

/// Crash-consistent storage images.
pub mod disk {
    pub use racer_io::sim::{CrashDisk, DiskState};
}

/// Shared deterministic OS state retained by reactors and simulated descriptors.
#[derive(Clone, Debug, Default)]
pub struct Simulation(SimOs);

impl Deref for Simulation {
    type Target = SimOs;

    fn deref(&self) -> &SimOs {
        &self.0
    }
}

impl From<SimOs> for Simulation {
    /// Select an existing world.
    fn from(os: SimOs) -> Self {
        Self(os)
    }
}

/// Restores the previous thread-local simulation when its scope ends.
pub struct Environment {
    previous: Option<Simulation>,
}

impl Drop for Environment {
    /// Restore the previous thread-local backend selection.
    fn drop(&mut self) {
        CURRENT.with(|s| *s.borrow_mut() = self.previous.take());
    }
}

impl Simulation {
    /// Creates an empty world with a durable root directory.
    pub fn new() -> Self {
        Self(SimOs::new())
    }

    /// The underlying simulated OS.
    pub fn os(&self) -> &SimOs {
        &self.0
    }

    /// Selection is worker-thread scoped. Reactors and descriptors retain their
    /// environment independently, including while a nested environment is active.
    pub fn enter(&self) -> Environment {
        Environment {
            previous: CURRENT.with(|s| s.borrow_mut().replace(self.clone())),
        }
    }

    /// Returns the simulation selected for the current worker thread.
    pub fn current() -> Option<Self> {
        CURRENT.with(|s| s.borrow().clone())
    }
}

/// A submission borrowing memory pinned by the production reactor's entry.
pub(super) enum Op {
    Buffer {
        fd: Rc<Descriptor>,

        operation: BufferOperation,

        ptr: *mut u8,

        len: usize,
    },
    /// A positioned vectored write; segments are gathered at execution time.
    Writev {
        fd: Rc<Descriptor>,

        offset: u64,

        iovecs: *const libc::iovec,

        count: usize,
    },
    Poll {
        fd: Rc<Descriptor>,

        interest: u32,
    },
    Accept(Rc<Descriptor>),
    Connect {
        fd: Rc<Descriptor>,

        address: SocketAddress,
    },
    Open {
        dir: Option<Rc<Descriptor>>,

        path: CString,

        flags: i32,

        resolve: u64,
    },
    Stat {
        fd: Rc<Descriptor>,

        ptr: *mut libc::statx,
    },
    Sync(Rc<Descriptor>),
    Mkdir {
        dir: Rc<Descriptor>,

        name: CString,
    },
    Rename {
        dir: Rc<Descriptor>,

        from: CString,

        to: CString,
    },
    Unlink {
        dir: Rc<Descriptor>,

        name: CString,
    },
}

impl From<Op> for LegacyOp {
    /// Lower the reactor's submission shape onto the shared driver's.
    fn from(op: Op) -> Self {
        match op {
            Op::Buffer {
                fd,
                operation,
                ptr,
                len,
            } => match operation {
                BufferOperation::Read(offset) => Self::Read {
                    fd,
                    ptr,
                    len,
                    offset,
                },
                BufferOperation::Write(offset) => Self::Write {
                    fd,
                    ptr,
                    len,
                    offset,
                },
                BufferOperation::Recv => Self::Recv { fd, ptr, len },
                BufferOperation::Send => Self::Send { fd, ptr, len },
            },
            Op::Writev {
                fd,
                offset,
                iovecs,
                count,
            } => Self::Writev {
                fd,
                offset,
                iovecs,
                count,
            },
            Op::Poll { fd, interest } => Self::Poll { fd, interest },
            Op::Accept(fd) => Self::Accept(fd),
            Op::Connect { fd, address } => Self::Connect { fd, address },
            Op::Open {
                dir,
                path,
                flags,
                resolve,
            } => Self::Open {
                dir,
                path,
                flags,
                resolve,
            },
            Op::Stat { fd, ptr } => Self::Stat { fd, ptr },
            Op::Sync(fd) => Self::Sync(fd),
            Op::Mkdir { dir, name } => Self::Mkdir { dir, name },
            Op::Rename { dir, from, to } => Self::Rename { dir, from, to },
            Op::Unlink { dir, name } => Self::Unlink { dir, name },
        }
    }
}

/// The reactor's simulated submission and completion queue.
pub(super) struct Driver(LegacyDriver);

impl Driver {
    /// Creates an empty submission and completion queue for a world.
    pub(super) fn new(sim: Simulation) -> Self {
        let clock = super::super::environment::Environment::current()
            .worker_ctx()
            .clock;
        Self(LegacyDriver::new(sim.0, clock))
    }

    /// Publishes an operation, capturing its fault and affected disk paths once.
    pub(super) fn push(&mut self, id: u64, op: Op) -> Result<(), ()> {
        self.0.push(id, op.into())
    }

    /// Retires borrowed pointers before supplying an arbitrary original CQE.
    #[cfg(test)]
    pub(super) fn inject_completion(&mut self, id: u64, result: i32) -> io::Result<()> {
        self.0.inject_completion(id, result)
    }

    /// Swaps queued completions to exercise arbitrary original/cancel orderings.
    #[cfg(test)]
    pub(super) fn reorder_completions(&mut self, first: usize, second: usize) -> io::Result<()> {
        self.0.reorder_completions(first, second)
    }

    /// Cancels only unexecuted work, preserving held results and both CQE fences.
    pub(super) fn cancel(&mut self, id: u64) {
        self.0.cancel(id);
    }

    /// Advances every pending operation by one deterministic submission turn.
    pub(super) fn submit(&mut self) {
        self.0.submit();
    }

    /// Removes the oldest completion without advancing simulated execution.
    pub(super) fn pop(&mut self) -> Option<(u64, KernelResult)> {
        let (id, result) = self.0.pop()?;
        Some((
            id,
            match result {
                LegacyResult::Value(value) => KernelResult::Value(value),
                LegacyResult::Accepted(fd) => KernelResult::Accepted(fd),
            },
        ))
    }
}
