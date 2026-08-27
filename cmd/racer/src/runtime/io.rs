//! IO surface: `Buf`, `Disk`, `Export`, and the op slab of outstanding SQEs.

use std::cell::{Cell, RefCell};
use std::future::Future;
use std::marker::PhantomData;
use std::path::PathBuf;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::task::{Context, Poll, Waker};
use std::time::Duration;

use crate::kernel::ring::{Op, Sqe, Timespec};

use super::limits::Limits;
use super::pool::{Pool, PoolBuf};
use super::worker::{self};
use super::{Errno, Limiter, limits, now};

use super::ublk;

pub(super) struct ExportInner {
    pub(super) path: PathBuf,
}

/// Handle on a ublk device exported by this process.
pub(crate) struct Export {
    pub(super) inner: Arc<ExportInner>,
    pub(super) _nosend: PhantomData<*const ()>,
}

// SAFETY: `ExportInner` is immutable and `Arc`-shared; only the `!Send` marker needs this.
unsafe impl Sync for Export {}

impl Export {
    pub(super) fn from_inner(inner: Arc<ExportInner>) -> Export {
        Export {
            inner,
            _nosend: PhantomData,
        }
    }

    /// Path of the block device node, `/dev/ublkbN`.
    pub(crate) fn path(&self) -> &std::path::Path {
        &self.inner.path
    }
}

/// An opaque handle to registered memory, never readable: it may be guest bio pages, so
/// the type system enforces zero copy.
#[derive(Clone, Copy, Debug)]
pub(crate) struct Buf {
    pub(super) index: u16,
    pub(super) addr: u64,
    pub(super) len: u32,
}

impl Buf {
    #[allow(clippy::len_without_is_empty)]
    pub(crate) fn len(&self) -> usize {
        self.len as usize
    }

    /// Pool index, or `None` for a guest request buffer; only pool memory is refcounted.
    ///
    /// The split between the two ranges is a property of the worker's limits, which are
    /// installed per thread, so a `Buf` stays the two words the kernel wants it to be.
    fn pool_index(&self) -> Option<u16> {
        let base = limits().pool_buf_base();
        (self.index as u32 >= base).then(|| self.index - base as u16)
    }

    /// What an op keeps alive: pool buffer for its lifetime, request buffer only counted.
    fn holds(&self) -> (Option<u16>, Option<u16>) {
        match self.pool_index() {
            Some(h) => (Some(h), None),
            None => (None, Some(self.index)),
        }
    }

    /// A sub-range of the same registered buffer. Panics if out of bounds.
    pub(crate) fn slice(&self, off: usize, len: usize) -> Buf {
        let end = off.checked_add(len).expect("Buf::slice overflow");
        assert!(end <= self.len as usize, "Buf::slice out of bounds");
        Buf {
            index: self.index,
            addr: self.addr + off as u64,
            len: len as u32,
        }
    }
}

/// The runtime's view of a pool buffer: the registered handle naming that memory, at the
/// index the worker reserved the pool's buffers at.
impl PoolBuf {
    pub(crate) fn buf(&self) -> Buf {
        Buf {
            index: limits().pool_buf_base() as u16 + self.index(),
            addr: self.addr(),
            len: self.len() as u32,
        }
    }
}

pub(super) struct DiskInner {
    pub(super) slot: u32,
    pub(super) timeout: Option<Duration>,
    pub(super) limit: Limiter,

    /// Held for the life of the slot: the registration names a descriptor, and closing it
    /// would leave the ring pointing at nothing.
    #[allow(dead_code)]
    pub(super) file: crate::kernel::File,
}

/// A registered file or block device; holding one proves registration on every worker. The
/// slot is unregistered only after the last `Disk` drops. `!Send` keeps IO on the issuing
/// worker.
#[derive(Clone)]
pub(crate) struct Disk {
    pub(super) inner: Arc<DiskInner>,
    pub(super) over: Option<Duration>,
    pub(super) _nosend: PhantomData<*const ()>,
}

// SAFETY: every worker registers an identical file table at identical slots, so a shared
// reference means the same thing on any worker.
unsafe impl Sync for Disk {}

impl Disk {
    pub(super) fn from_inner(inner: Arc<DiskInner>) -> Disk {
        Disk {
            inner,
            over: None,
            _nosend: PhantomData,
        }
    }

    fn deadline(&self) -> Option<Duration> {
        self.over.or(self.inner.timeout)
    }

    pub(crate) async fn read(&self, off: u64, buf: Buf) -> Result<(), Errno> {
        self.pace(buf.len).await;
        let e = Sqe::new(
            Op::ReadFixed {
                file: self.inner.slot,
                buf: buf.addr as *mut u8,
                len: buf.len,
                buf_index: buf.index,
                offset: off,
            },
            0,
        );
        self.run(e, buf.len, buf.holds()).await
    }

    /// `Durability::Durable` adds `RWF_DSYNC`: on an `O_DIRECT` block device this is
    /// per-write FUA, stable media when the CQE lands. No flush op; never buffered.
    pub(crate) async fn write(&self, off: u64, buf: Buf, d: Durability) -> Result<(), Errno> {
        self.pace(buf.len).await;
        let e = Sqe::new(
            Op::WriteFixed {
                file: self.inner.slot,
                buf: buf.addr as *const u8,
                len: buf.len,
                buf_index: buf.index,
                offset: off,
                dsync: matches!(d, Durability::Durable),
            },
            0,
        );
        self.run(e, buf.len, buf.holds()).await
    }

    /// Waits for device budget. Runs before an op slot is taken, so pacing costs a timer.
    async fn pace(&self, len: u32) {
        if let Some(d) = self.inner.limit.admit(len) {
            sleep(d).await;
        }
    }

    /// Whether the budget is committed far enough ahead that droppable work should drop.
    pub(crate) fn pressed(&self) -> bool {
        self.inner.limit.pressed()
    }

    /// Total time transfers have been held back, in microseconds.
    pub(crate) fn waited_us(&self) -> u64 {
        self.inner.limit.waited_us()
    }

    async fn run(&self, e: Sqe, len: u32, holds: (Option<u16>, Option<u16>)) -> Result<(), Errno> {
        transfer_result(submit_op(e, self.deadline(), holds.0, holds.1).await, len)
    }
}

fn transfer_result(res: i32, len: u32) -> Result<(), Errno> {
    if res < 0 {
        return Err(Errno(-res));
    }
    // A short transfer on an O_DIRECT device is a failure; we never retry the rest.
    if res as u32 != len {
        return Err(Errno::EIO);
    }
    Ok(())
}

pub(super) async fn read_request(req: Buf, off: usize, dst: Buf) -> Result<(), Errno> {
    request_copy(req, off, dst, false).await
}

pub(super) async fn write_request(req: Buf, off: usize, src: Buf) -> Result<(), Errno> {
    request_copy(req, off, src, true).await
}

/// Copies between a request's guest pages and a pool buffer, through `/dev/ublkcN`.
///
/// The pool buffer is named by address rather than by its registered index, which is the
/// one place in the node that does. `ublk_drv` will not take a registered buffer: it
/// reaches the driver as an `ITER_BVEC` iterator, and `ublk_check_and_get_req()` answers
/// `EACCES` unless `user_backed_iter()` holds. The memory is the same memory either way,
/// and it stays registered for every other submission that names it; only the addressing
/// mode of this one submission changes. The file stays a fixed-file index: a registered
/// file is not what the driver objects to.
async fn request_copy(req: Buf, off: usize, pool: Buf, store: bool) -> Result<(), Errno> {
    let hold = pool
        .pool_index()
        .expect("request copy requires a pool buffer");
    let fut = {
        let (slot, q_id, tag) = ublk::request_target(req.index as u32)?;
        let pos = ublk::buf_offset(q_id, tag, off);
        let op = if store {
            Op::Write {
                file: slot,
                buf: pool.addr as *const u8,
                len: pool.len,
                offset: pos,
            }
        } else {
            Op::Read {
                file: slot,
                buf: pool.addr as *mut u8,
                len: pool.len,
                offset: pos,
            }
        };
        let e = Sqe::new(op, 0);
        submit_op(e, None, Some(hold), Some(req.index))
    };
    transfer_result(fut.await, pool.len)
}

/// Sleeps on this worker's ring. Timers are ordinary ops: one SQE, no thread.
pub(crate) async fn sleep(d: Duration) {
    let fut = super::worker::with(|l| {
        let (idx, seq) = l
            .ops
            .acquire(&l.pool, 1, None, None)
            .expect("op slab exhausted");
        // The deadline lives in the op slot, so it outlives the SQE.
        let ts = l.ops.set_timespec(idx, d);
        l.push(Sqe::new(Op::Timeout { ts }, worker::ud_op(idx, seq)));
        OpFuture {
            idx,
            seq,
            done: false,
        }
    });
    let _ = fut.await;
}

/// Run `fut`, giving up after `d`. `None` means it was still running and has been dropped.
pub(crate) async fn deadline<F: Future>(fut: F, d: Duration) -> Option<F::Output> {
    let mut fut = std::pin::pin!(fut);
    let at = now() + d;
    let waker = Rc::new(RefCell::new(None));
    super::worker::with(|l| {
        l.deadlines.borrow_mut().push((at, Rc::downgrade(&waker)));
    });
    std::future::poll_fn(|cx| {
        if let Poll::Ready(v) = fut.as_mut().poll(cx) {
            return Poll::Ready(Some(v));
        }
        if now() >= at {
            return Poll::Ready(None);
        }
        *waker.borrow_mut() = Some(cx.waker().clone());
        Poll::Pending
    })
    .await
}

/// Whether a write must be on stable media before it completes.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum Durability {
    Buffered,
    Durable,
}

const OP_FREE: u8 = 0;
const OP_ARMED: u8 = 1;
const OP_DONE: u8 = 2;
const OP_DETACHED: u8 = 3;

struct OpSlot {
    pub(super) state: u8,
    pub(super) seq: u16,
    pub(super) pending: u8,
    pub(super) res: i32,
    pub(super) timed_out: bool,
    pub(super) waker: Option<Waker>,
    pub(super) ts: Timespec,
    pub(super) hold: Option<u16>,
    pub(super) tag: Option<u16>,
}

/// Fixed-size slab of in-flight ops, one per worker.
pub(super) struct OpSlab {
    slots: RefCell<Vec<OpSlot>>,
    free: RefCell<Vec<u32>>,
    inflight: Cell<u32>,
    users: RefCell<Vec<u16>>,
    /// Slot count, kept for `utilization`, which is the throttle's only input.
    capacity: u32,
}

impl OpSlab {
    pub(super) fn new(limits: &Limits) -> OpSlab {
        let n = limits.ops_per_worker as usize;
        let mut slots = Vec::with_capacity(n);
        for _ in 0..n {
            slots.push(OpSlot {
                state: OP_FREE,
                seq: 0,
                pending: 0,
                res: 0,
                timed_out: false,
                waker: None,
                ts: Timespec::default(),
                hold: None,
                tag: None,
            });
        }
        OpSlab {
            slots: RefCell::new(slots),
            free: RefCell::new((0..n as u32).rev().collect()),
            inflight: Cell::new(0),
            users: RefCell::new(vec![0u16; limits.pool_buf_base() as usize]),
            capacity: limits.ops_per_worker,
        }
    }

    pub(super) fn inflight(&self) -> u32 {
        self.inflight.get()
    }

    pub(super) fn utilization(&self) -> f32 {
        self.inflight.get() as f32 / self.capacity as f32
    }

    /// Whether any live op names the request buffer of `tag`.
    pub(super) fn tag_busy(&self, tag: u32) -> bool {
        self.users.borrow()[tag as usize] != 0
    }

    fn acquire(
        &self,
        pool: &Pool,
        pending: u8,
        hold: Option<u16>,
        tag: Option<u16>,
    ) -> Option<(u32, u16)> {
        let idx = self.free.borrow_mut().pop()?;
        let mut slots = self.slots.borrow_mut();
        let s = &mut slots[idx as usize];
        s.state = OP_ARMED;
        s.seq = s.seq.wrapping_add(1);
        s.pending = pending;
        s.res = 0;
        s.timed_out = false;
        s.waker = None;
        s.hold = hold;
        s.tag = tag;
        if let Some(b) = s.hold {
            pool.hold(b);
        }
        if let Some(t) = s.tag {
            self.users.borrow_mut()[t as usize] += 1;
        }
        self.inflight.set(self.inflight.get() + 1);
        Some((idx, s.seq))
    }

    fn release(&self, pool: &Pool, idx: u32) {
        let mut slots = self.slots.borrow_mut();
        slots[idx as usize].state = OP_FREE;
        slots[idx as usize].waker = None;
        let hold = slots[idx as usize].hold.take();
        let tag = slots[idx as usize].tag.take();
        drop(slots);
        self.drop_buf(pool, hold, tag);
        self.free.borrow_mut().push(idx);
        self.inflight.set(self.inflight.get() - 1);
    }

    fn drop_buf(&self, pool: &Pool, hold: Option<u16>, tag: Option<u16>) {
        if let Some(b) = hold {
            pool.unhold(b);
        }
        if let Some(t) = tag {
            self.users.borrow_mut()[t as usize] -= 1;
        }
    }

    /// Records one CQE. Wakes the owner once every CQE it is owed has arrived.
    pub(super) fn complete(&self, pool: &Pool, idx: u32, seq: u16, res: i32, is_timeout: bool) {
        let mut slots = self.slots.borrow_mut();
        let s = &mut slots[idx as usize];
        if s.state == OP_FREE || s.seq != seq {
            return; // stale completion for a recycled slot
        }
        if is_timeout {
            // -ETIME on the link CQE means the deadline fired and canceled the op.
            if res == -libc::ETIME {
                s.timed_out = true;
            }
        } else {
            s.res = res;
        }
        s.pending = s.pending.saturating_sub(1);
        if s.pending != 0 {
            return;
        }
        if s.state == OP_DETACHED {
            drop(slots);
            self.release(pool, idx);
            return;
        }
        s.state = OP_DONE;
        if s.timed_out {
            s.res = -libc::ETIME;
        }
        // All CQEs have landed, so the kernel is done with the buffer though the slot lives
        // until the waiter polls. Release outside the borrow: `unhold` and `wake` re-enter.
        let hold = s.hold.take();
        let tag = s.tag.take();
        let waker = s.waker.take();
        drop(slots);
        self.drop_buf(pool, hold, tag);
        if let Some(w) = waker {
            w.wake();
        }
    }

    /// Give up on an in-flight op. The slot stays reserved until every CQE it is owed has
    /// arrived, so a late completion can never land on a recycled index.
    fn detach(&self, pool: &Pool, idx: u32, seq: u16) {
        let mut slots = self.slots.borrow_mut();
        let s = &mut slots[idx as usize];
        if s.seq != seq {
            return;
        }
        match s.state {
            OP_ARMED => {
                s.state = OP_DETACHED;
                s.waker = None;
            }
            OP_DONE => {
                drop(slots);
                self.release(pool, idx);
            }
            _ => {}
        }
    }

    fn poll(&self, idx: u32, seq: u16, cx: &mut Context<'_>) -> Poll<i32> {
        let mut slots = self.slots.borrow_mut();
        let s = &mut slots[idx as usize];
        debug_assert_eq!(s.seq, seq);
        if s.state == OP_DONE {
            return Poll::Ready(s.res);
        }
        s.waker = Some(cx.waker().clone());
        Poll::Pending
    }

    fn set_timespec(&self, idx: u32, d: Duration) -> *const Timespec {
        let mut slots = self.slots.borrow_mut();
        slots[idx as usize].ts = Timespec::from_duration(d);
        &slots[idx as usize].ts as *const Timespec
    }
}

/// Awaits one operation. Dropping it detaches the slot rather than cancelling, so
/// abandoning a quorum leg is free; the slot holds its buffer or tag until the last CQE.
struct OpFuture {
    idx: u32,
    seq: u16,
    done: bool,
}

impl Future for OpFuture {
    type Output = i32;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<i32> {
        let me = self.get_mut();
        let r = super::worker::with(|l| l.ops.poll(me.idx, me.seq, cx));
        if let Poll::Ready(v) = r {
            me.done = true;
            super::worker::with(|l| l.ops.release(&l.pool, me.idx));
            return Poll::Ready(v);
        }
        Poll::Pending
    }
}

impl Drop for OpFuture {
    fn drop(&mut self) {
        if !self.done {
            super::worker::with(|l| l.ops.detach(&l.pool, self.idx, self.seq));
        }
    }
}

/// Submits one operation, optionally guarded by a linked timeout, and awaits it.
async fn submit_op(e: Sqe, timeout: Option<Duration>, hold: Option<u16>, tag: Option<u16>) -> i32 {
    let fut = super::worker::with(|l| {
        let pending = if timeout.is_some() { 2 } else { 1 };
        let (idx, seq) = l
            .ops
            .acquire(&l.pool, pending, hold, tag)
            .expect("op slab exhausted");
        match timeout {
            None => {
                let mut e = e;
                e.user_data = worker::ud_op(idx, seq);
                l.push(e);
            }
            Some(d) => {
                // The pair must land in the SQ together or the link is broken.
                let ts = l.ops.set_timespec(idx, d);
                let mut a = e;
                a.user_data = worker::ud_op(idx, seq);
                let b = Sqe::new(Op::LinkTimeout { ts }, worker::ud_link(idx, seq));
                l.push_linked(a.linked(), b);
            }
        }
        OpFuture {
            idx,
            seq,
            done: false,
        }
    });
    fut.await
}

#[cfg(test)]
mod tests {
    use super::*;

    fn buf(index: u16) -> Buf {
        Buf {
            index,
            addr: 100,
            len: 20,
        }
    }

    #[test]
    fn buf_slices_and_classifies_owners() {
        let request = buf(7);
        let pool = buf(limits().pool_buf_base() as u16 + 3);

        let slice = request.slice(4, 8);
        assert_eq!((slice.index, slice.addr, slice.len), (7, 104, 8));
        assert_eq!(request.holds(), (None, Some(7)));
        assert_eq!(pool.holds(), (Some(3), None));
    }

    #[test]
    #[should_panic(expected = "Buf::slice out of bounds")]
    fn buf_slice_rejects_out_of_bounds_range() {
        buf(0).slice(15, 6);
    }

    #[test]
    fn transfer_requires_exact_length() {
        assert_eq!(transfer_result(20, 20), Ok(()));
        assert_eq!(transfer_result(19, 20), Err(Errno::EIO));
        assert_eq!(transfer_result(-libc::ENOSPC, 20), Err(Errno::ENOSPC));
    }
}
