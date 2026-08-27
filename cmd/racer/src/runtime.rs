//! Racer async runtime: ublk+io_uring with share-nothing, thread-per-core architecture.
//! Supports deterministic simulation as a first-class citizen.

mod control;
mod exec;
mod hop;
mod io;
mod limit;
mod limits;
mod pool;
mod sys;
mod ublk;
mod worker;

use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::task::{Context, Poll};
use std::time::Instant;

use io::OpSlab;
use worker::{Ack, Local, with};

#[cfg(test)]
pub(crate) use control::exclusive;
pub use control::{ResourceBuild, Runtime, UpdateError, start};
pub(crate) use control::{broadcast_stalls, broadcast_wait_us};
pub(crate) use hop::{spawn_local, to, to_async_with};
pub(crate) use io::{Buf, Disk, Durability, Export, deadline, sleep};
pub(crate) use limit::Limiter;
pub(crate) use pool::PoolBuf;

pub(crate) use crate::kernel::now;

#[cfg(test)]
use hop::Fabric;
pub(crate) use limits::QUEUES_PER_WORKER;
pub(crate) use limits::{COMPACT, install_limits, limits};

pub trait Handler: 'static {
    type Config: Sync + 'static;
    type Worker: 'static;

    /// Build this core's complete value for an accepted configuration generation.
    fn build_worker(
        core: CoreId,
        config: Arc<Self::Config>,
        previous: Option<&Self::Worker>,
    ) -> Self::Worker;

    /// Serve one request.
    /// The returned future is stored in a preallocated slot, so it must be small.
    /// Put cold paths behind `Box::pin(..).await`.
    fn handle(
        worker: Rc<Self::Worker>,
        req: Request,
    ) -> impl Future<Output = Result<(), Errno>> + 'static;

    /// Maintenance hook: runs on every core about every millisecond, idle or not, for
    /// per-core state. Runs on the worker thread, so it must not block.
    fn tick(_worker: Rc<Self::Worker>, _now: Instant) {}
}

#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub(crate) enum Op {
    Read,
    Write,
    Discard,
}

#[derive(Copy, Clone, Debug)]
pub struct Request {
    pub(crate) dev: u64,
    pub(crate) op: Op,
    pub(crate) lba: u64,
    pub(crate) buf: Buf,
}

impl Request {
    /// Copies from the guest request buffer into a registered pool buffer.
    pub(crate) async fn read(&self, off: usize, dst: &mut PoolBuf) -> Result<(), Errno> {
        self.check_copy(off, dst.len())?;
        io::read_request(self.buf, off, dst.buf()).await
    }

    /// Copies from a registered pool buffer into the guest request buffer.
    pub(crate) async fn write(&self, off: usize, src: &PoolBuf) -> Result<(), Errno> {
        self.check_copy(off, src.len())?;
        io::write_request(self.buf, off, src.buf()).await
    }

    fn check_copy(&self, off: usize, len: usize) -> Result<(), Errno> {
        match off.checked_add(len) {
            Some(end) if end <= self.buf.len() => Ok(()),
            _ => Err(Errno::EINVAL),
        }
    }
}

#[derive(Copy, Clone, PartialEq, Eq)]
pub struct Errno(i32);

impl Errno {
    pub(crate) const EIO: Errno = Errno(libc::EIO);
    pub(crate) const ENOSPC: Errno = Errno(libc::ENOSPC);
    pub(crate) const EOPNOTSUPP: Errno = Errno(libc::EOPNOTSUPP);
    pub(crate) const EINVAL: Errno = Errno(libc::EINVAL);
    pub(crate) const ENODATA: Errno = Errno(libc::ENODATA);
    pub(crate) const EREMOTEIO: Errno = Errno(libc::EREMOTEIO);

    pub(crate) fn from_raw(e: i32) -> Errno {
        Errno(e.abs())
    }

    pub(crate) fn raw(self) -> i32 {
        self.0
    }
}

impl std::fmt::Debug for Errno {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "Errno({})", self.0)
    }
}

/// Poll all `futs` concurrently until `need` of them succeed, then abandon the rest.
/// Resolves once `need` succeed or so many fail that `need` is unreachable.
/// Slot `i` holds `futs[i]`'s outcome, or `None` if it was still running at resolution.
pub(crate) fn quorum<T, E, F, const N: usize>(
    futs: [F; N],
    need: usize,
) -> impl Future<Output = [Option<Result<T, E>>; N]>
where
    F: Future<Output = Result<T, E>>,
{
    Quorum {
        futs: futs.map(Some),
        out: [const { None }; N],
        need,
        ok: 0,
        failed: 0,
    }
}

struct Quorum<T, E, F, const N: usize> {
    futs: [Option<F>; N],
    out: [Option<Result<T, E>>; N],
    need: usize,
    ok: usize,
    failed: usize,
}

impl<T, E, F: Future<Output = Result<T, E>>, const N: usize> Future for Quorum<T, E, F, N> {
    type Output = [Option<Result<T, E>>; N];

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let this = unsafe { self.get_unchecked_mut() };
        if this.need == 0 || this.need > N {
            for f in this.futs.iter_mut() {
                *f = None;
            }
            return Poll::Ready(std::array::from_fn(|i| this.out[i].take()));
        }
        for i in 0..N {
            let Some(f) = this.futs[i].as_mut() else {
                continue;
            };
            if let Poll::Ready(v) = unsafe { Pin::new_unchecked(f) }.poll(cx) {
                if v.is_ok() {
                    this.ok += 1;
                } else {
                    this.failed += 1;
                }
                this.out[i] = Some(v);
                this.futs[i] = None;
            }
        }
        if this.ok < this.need && N - this.failed >= this.need {
            return Poll::Pending;
        }
        // Settled: drop the losers in place, which abandons any hop still in flight.
        for f in this.futs.iter_mut() {
            *f = None;
        }
        Poll::Ready(std::array::from_fn(|i| this.out[i].take()))
    }
}

#[cfg(test)]
#[path = "../tests/runtime/int.rs"]
mod int;

/// ID of the CPU core a worker is running on.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Debug)]
pub struct CoreId(u16);

impl CoreId {
    pub(crate) fn new(i: usize) -> Option<CoreId> {
        (i < cores()).then_some(CoreId(i as u16))
    }

    pub(crate) fn of(i: usize) -> CoreId {
        debug_assert!(i <= u16::MAX as usize);
        CoreId(i as u16)
    }

    pub(crate) fn index(self) -> usize {
        self.0 as usize
    }
}

/// Worker running this code.
pub(crate) fn core() -> CoreId {
    worker::with(|l| CoreId::of(l.core))
}

/// Workers in this runtime.
pub(crate) fn cores() -> usize {
    worker::with(|l| l.fabric.cores())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn outcome(mut value: Option<Result<u8, ()>>) -> impl Future<Output = Result<u8, ()>> {
        std::future::poll_fn(move |_| value.take().map_or(Poll::Pending, Poll::Ready))
    }

    fn poll<F: Future>(future: Pin<&mut F>) -> Poll<F::Output> {
        future.poll(&mut Context::from_waker(std::task::Waker::noop()))
    }

    #[test]
    fn quorum_settles_when_reached_or_impossible() {
        let mut future = std::pin::pin!(quorum(
            [outcome(Some(Ok(1))), outcome(None), outcome(Some(Ok(3)))],
            2,
        ));
        assert_eq!(
            poll(future.as_mut()),
            Poll::Ready([Some(Ok(1)), None, Some(Ok(3))])
        );

        let mut future = std::pin::pin!(quorum(
            [
                outcome(Some(Err(()))),
                outcome(None),
                outcome(Some(Err(())))
            ],
            2,
        ));
        assert_eq!(
            poll(future.as_mut()),
            Poll::Ready([Some(Err(())), None, Some(Err(()))])
        );
    }

    #[test]
    fn quorum_waits_while_reachable() {
        let mut future = std::pin::pin!(quorum(
            [outcome(Some(Ok(1))), outcome(None), outcome(Some(Err(())))],
            2,
        ));
        assert_eq!(poll(future.as_mut()), Poll::Pending);
    }

    fn request(len: u32) -> Request {
        Request {
            dev: 0,
            op: Op::Read,
            lba: 0,
            buf: Buf {
                index: 0,
                addr: 0,
                len,
            },
        }
    }

    #[test]
    fn request_copy_bounds_are_checked() {
        let req = request(8192);
        assert_eq!(req.check_copy(4096, 4096), Ok(()));
        assert_eq!(req.check_copy(4097, 4096), Err(Errno::EINVAL));
        assert_eq!(req.check_copy(usize::MAX, 1), Err(Errno::EINVAL));
    }
}
