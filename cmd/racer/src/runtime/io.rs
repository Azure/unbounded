//! IO surface: `Buf`, `PoolBuf`, `Disk`, `Export`, and the op slab of outstanding SQEs.

use std::cell::{Cell, RefCell};
use std::collections::VecDeque;
use std::future::Future;
use std::io;
use std::marker::PhantomData;
use std::os::fd::OwnedFd;
use std::path::PathBuf;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll, Waker};
use std::time::Duration;

use io_uring::{opcode, squeue, types};

use super::limit::Limiter;
use super::sys::Region;
use super::worker::{self};
use super::{Errno, POOL_BUF_BASE};

/// In-flight SQE-backed operations a worker can own at once.
#[cfg(not(feature = "sim"))]
const OPS_PER_WORKER: u32 = 4096;
/// Simulated workers share one address space, so per-worker tables shrink to the protocol
/// floor: one op per piece of a whole page.
#[cfg(feature = "sim")]
const OPS_PER_WORKER: u32 = 256;

// --- Buf ---

/// An opaque handle to registered memory, never readable: it may be guest bio pages, so
/// the type system enforces zero copy. io_uring resolves the SQE `addr` against registered
/// buffer `index`: base 0 for ublk request buffers, the real pointer for pool memory.
#[derive(Clone, Copy, Debug)]
pub(crate) struct Buf {
    pub(super) index: u16,
    pub(super) addr: u64,
    pub(super) len: u32,
}

impl Buf {
    // No `is_empty`: a zero-length request buffer cannot occur.
    #[allow(clippy::len_without_is_empty)]
    pub(crate) fn len(&self) -> usize {
        self.len as usize
    }

    /// Pool index, or `None` for a guest request buffer; only pool memory is refcounted.
    fn pool_index(&self) -> Option<u16> {
        (self.index as u32 >= POOL_BUF_BASE).then(|| self.index - POOL_BUF_BASE as u16)
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

// --- pool ---

/// Power-of-two size classes; class 0 (4 KiB) is the metadata block, so it gets the most.
#[cfg(not(feature = "sim"))]
const CLASSES: [(usize, usize); 7] = [
    (4 << 10, 512),
    // A fabric accept is a page plus a 4 KiB trailer, so 8 KiB is as hot as the page class.
    (8 << 10, 512),
    (16 << 10, 64),
    (64 << 10, 16),
    (256 << 10, 4),
    (1 << 20, 4),
    (4 << 20, 2),
];

/// The same classes for a simulated worker; its lone huge buffer stages huge-page repair.
#[cfg(feature = "sim")]
const CLASSES: [(usize, usize); 4] = [(4 << 10, 32), (8 << 10, 32), (16 << 10, 8), (4 << 20, 1)];

pub(super) const POOL_BUFS: u32 = {
    let mut n = 0;
    let mut i = 0;
    while i < CLASSES.len() {
        n += CLASSES[i].1;
        i += 1;
    }
    n as u32
};

struct Class {
    size: usize,
    free: RefCell<Vec<u16>>,
    waiters: RefCell<VecDeque<Waker>>,
}

/// Per-worker registered DRAM for everything that is not a guest request buffer.
pub(super) struct Pool {
    /// Never read: owns the mapping the registered buffers point into.
    #[allow(dead_code)]
    region: Region,
    classes: Vec<Class>,
    /// Address of each pool buffer; its registered index is `POOL_BUF_BASE + i`.
    addrs: Vec<u64>,
    sizes: Vec<usize>,
    /// In-flight ops still reading or writing each buffer.
    users: RefCell<Vec<u16>>,
    /// Buffers whose `PoolBuf` is gone but whose last CQE has not arrived.
    orphan: RefCell<Vec<bool>>,
}

impl Pool {
    pub(super) fn new() -> io::Result<Pool> {
        let total: usize = CLASSES.iter().map(|(s, n)| s * n).sum();
        let region = Region::new(total)?;
        let mut addrs = Vec::new();
        let mut sizes = Vec::new();
        let mut classes = Vec::new();
        let mut off = 0usize;
        for (size, count) in CLASSES {
            let mut free = Vec::with_capacity(count);
            for _ in 0..count {
                let idx = addrs.len() as u16;
                addrs.push(region.as_ptr() as u64 + off as u64);
                sizes.push(size);
                free.push(idx);
                off += size;
            }
            free.reverse();
            classes.push(Class {
                size,
                free: RefCell::new(free),
                waiters: RefCell::new(VecDeque::new()),
            });
        }
        let n = addrs.len();
        Ok(Pool {
            region,
            classes,
            addrs,
            sizes,
            users: RefCell::new(vec![0; n]),
            orphan: RefCell::new(vec![false; n]),
        })
    }

    /// iovecs for `register_buffers_update`, one per pool buffer.
    pub(super) fn iovecs(&self) -> Vec<libc::iovec> {
        self.addrs
            .iter()
            .zip(&self.sizes)
            .map(|(&a, &l)| libc::iovec {
                iov_base: a as *mut libc::c_void,
                iov_len: l,
            })
            .collect()
    }

    fn class_of(&self, len: usize) -> usize {
        self.classes
            .iter()
            .position(|c| c.size >= len)
            .expect("requested buffer larger than the largest pool class")
    }

    fn class_index_of(&self, idx: u16) -> usize {
        let size = self.sizes[idx as usize];
        self.classes.iter().position(|c| c.size == size).unwrap()
    }

    /// An op was submitted against `idx`; the kernel owns the memory until its CQE lands.
    pub(super) fn hold(&self, idx: u16) {
        self.users.borrow_mut()[idx as usize] += 1;
    }

    /// One op against `idx` is done. Frees the buffer if its owner dropped it in flight.
    pub(super) fn unhold(&self, idx: u16) {
        let mut users = self.users.borrow_mut();
        users[idx as usize] -= 1;
        if users[idx as usize] != 0 {
            return;
        }
        drop(users);
        if std::mem::replace(&mut self.orphan.borrow_mut()[idx as usize], false) {
            self.free(idx);
        }
    }

    /// Frees `idx`, or orphans it if ops still read it; this is what makes dropping an
    /// `OpFuture` safe rather than a use-after-free.
    fn release(&self, idx: u16) {
        if self.users.borrow()[idx as usize] != 0 {
            self.orphan.borrow_mut()[idx as usize] = true;
            return;
        }
        self.free(idx);
    }

    fn free(&self, idx: u16) {
        let c = self.class_index_of(idx);
        self.classes[c].free.borrow_mut().push(idx);
        // Wake all waiters, not just the first: a since-dropped one swallows the wakeup
        // and strands the queue; losers re-queue. Pop singly: `wake` re-enters the pool.
        loop {
            let next = self.classes[c].waiters.borrow_mut().pop_front();
            match next {
                Some(w) => w.wake(),
                None => break,
            }
        }
    }
}

/// Registered scratch memory owned by the current worker. Readable and writable, unlike
/// [`Buf`]. Returns to the worker's pool on drop; `!Send`, so it never escapes.
pub(crate) struct PoolBuf {
    index: u16,
    len: usize,
    addr: u64,
    _nosend: PhantomData<*const ()>,
}

impl PoolBuf {
    /// Waits for a buffer of at least `len` bytes. Never fails; parks under starvation.
    pub(crate) async fn alloc(len: usize) -> PoolBuf {
        std::future::poll_fn(|cx| {
            worker::with_local(|l| {
                let pool = &l.pool;
                let c = pool.class_of(len);
                if let Some(idx) = pool.classes[c].free.borrow_mut().pop() {
                    return Poll::Ready(PoolBuf {
                        index: idx,
                        len,
                        addr: pool.addrs[idx as usize],
                        _nosend: PhantomData,
                    });
                }
                let mut waiters = pool.classes[c].waiters.borrow_mut();
                // Once per waiting task, not once per poll. A starved class is polled
                // again by anything that wakes its task, and a queue that took a fresh
                // waker each time would grow without bound while the class stayed empty:
                // nothing removes a registration but the next [`Pool::free`].
                if !waiters.iter().any(|w| w.will_wake(cx.waker())) {
                    waiters.push_back(cx.waker().clone());
                }
                Poll::Pending
            })
        })
        .await
    }

    /// A buffer of at least `len` bytes if one is free right now, for callers that cannot
    /// await (`Handler::tick` takes mblock staging buffers this way, held across flushes).
    pub(crate) fn try_alloc(len: usize) -> Option<PoolBuf> {
        worker::with_local(|l| {
            let pool = &l.pool;
            let c = pool.class_of(len);
            let idx = pool.classes[c].free.borrow_mut().pop()?;
            Some(PoolBuf {
                index: idx,
                len,
                addr: pool.addrs[idx as usize],
                _nosend: PhantomData,
            })
        })
    }

    /// The registered handle for this memory, for passing to `Disk` IO.
    pub(crate) fn buf(&self) -> Buf {
        Buf {
            index: POOL_BUF_BASE as u16 + self.index,
            addr: self.addr,
            len: self.len as u32,
        }
    }
}

impl std::ops::Deref for PoolBuf {
    type Target = [u8];
    fn deref(&self) -> &[u8] {
        // SAFETY: `addr`/`len` name this buffer's own pool region, mapped for the life of
        // the worker and held exclusively by this `PoolBuf`.
        unsafe { std::slice::from_raw_parts(self.addr as *const u8, self.len) }
    }
}

impl std::ops::DerefMut for PoolBuf {
    fn deref_mut(&mut self) -> &mut [u8] {
        // SAFETY: as `deref`; `&mut self` makes this the only reference to the region.
        unsafe { std::slice::from_raw_parts_mut(self.addr as *mut u8, self.len) }
    }
}

impl Drop for PoolBuf {
    fn drop(&mut self) {
        worker::with_local(|l| l.pool.release(self.index));
    }
}

// --- op slab ---

const OP_FREE: u8 = 0;
const OP_ARMED: u8 = 1;
const OP_DONE: u8 = 2;
/// Nobody is waiting any more, but the kernel still owes us CQEs.
const OP_DETACHED: u8 = 3;

struct OpSlot {
    pub(super) state: u8,
    pub(super) seq: u16,
    /// CQEs still owed to us. A linked op+timeout pair owes two.
    pub(super) pending: u8,
    pub(super) res: i32,
    pub(super) timed_out: bool,
    pub(super) waker: Option<Waker>,
    /// Stable storage for a timeout's deadline; the kernel copies it at submit.
    pub(super) ts: types::Timespec,
    /// Pool buffer this op reads or writes, held alive until its last CQE.
    pub(super) hold: Option<u16>,
    /// Request buffer this op names. Owned by the kernel for one ublk request, so it is
    /// only counted; the count forbids answering that request while an op still names it.
    pub(super) tag: Option<u16>,
}

/// Fixed-size slab of in-flight ops, one per worker.
pub(super) struct OpSlab {
    slots: RefCell<Vec<OpSlot>>,
    free: RefCell<Vec<u32>>,
    inflight: Cell<u32>,
    /// Live ops naming each ublk request buffer, indexed by tag.
    users: RefCell<Vec<u16>>,
}

impl OpSlab {
    pub(super) fn new() -> OpSlab {
        let n = OPS_PER_WORKER as usize;
        let mut slots = Vec::with_capacity(n);
        for _ in 0..n {
            slots.push(OpSlot {
                state: OP_FREE,
                seq: 0,
                pending: 0,
                res: 0,
                timed_out: false,
                waker: None,
                ts: types::Timespec::new(),
                hold: None,
                tag: None,
            });
        }
        OpSlab {
            slots: RefCell::new(slots),
            free: RefCell::new((0..n as u32).rev().collect()),
            inflight: Cell::new(0),
            users: RefCell::new(vec![0u16; POOL_BUF_BASE as usize]),
        }
    }

    pub(super) fn inflight(&self) -> u32 {
        self.inflight.get()
    }

    pub(super) fn utilization(&self) -> f32 {
        self.inflight.get() as f32 / OPS_PER_WORKER as f32
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
            // -ETIME on the link CQE means the deadline fired and cancelled the op.
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
    fn detach(&self, idx: u32, seq: u16) {
        let mut slots = self.slots.borrow_mut();
        let s = &mut slots[idx as usize];
        if s.seq != seq || s.state != OP_ARMED {
            return;
        }
        s.state = OP_DETACHED;
        s.waker = None;
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

    fn set_timespec(&self, idx: u32, d: Duration) -> *const types::Timespec {
        let mut slots = self.slots.borrow_mut();
        slots[idx as usize].ts = types::Timespec::new()
            .sec(d.as_secs())
            .nsec(d.subsec_nanos());
        &slots[idx as usize].ts as *const types::Timespec
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
        let r = worker::with_local(|l| l.ops.poll(me.idx, me.seq, cx));
        if let Poll::Ready(v) = r {
            me.done = true;
            worker::with_local(|l| l.ops.release(&l.pool, me.idx));
            return Poll::Ready(v);
        }
        Poll::Pending
    }
}

impl Drop for OpFuture {
    fn drop(&mut self) {
        if !self.done {
            worker::with_local(|l| l.ops.detach(self.idx, self.seq));
        }
    }
}

// --- Disk / Export ---

pub(super) struct DiskInner {
    pub(super) slot: u32,
    pub(super) timeout: Option<Duration>,
    /// Device rate budget, shared by every worker rather than sliced per core.
    pub(super) limit: Limiter,
    /// Never read: keeps the description alive until every worker unregisters the slot.
    #[cfg(not(feature = "sim"))]
    #[allow(dead_code)]
    pub(super) fd: OwnedFd,
}

/// A registered file or block device; holding one proves registration on every worker. The
/// slot is unregistered only after the last `Disk` drops. `!Send` keeps IO on the issuing
/// worker (cross-core via `on_core`) and bars stashing a clone in a `static`.
#[derive(Clone)]
pub(crate) struct Disk {
    pub(super) inner: Arc<DiskInner>,
    /// Per-handle deadline overriding the device's; on the handle so op futures stay small.
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

    /// The same device on a caller-chosen deadline, used where a late transfer is a
    /// failed path, or where the device's own deadline pins something too long.
    pub(crate) fn by(&self, d: Duration) -> Disk {
        Disk {
            inner: self.inner.clone(),
            over: Some(d),
            _nosend: PhantomData,
        }
    }

    fn deadline(&self) -> Option<Duration> {
        self.over.or(self.inner.timeout)
    }

    pub(crate) async fn read(&self, off: u64, buf: Buf) -> Result<(), Errno> {
        self.pace(buf.len).await;
        #[cfg(feature = "sim")]
        return self.sim(off, buf, crate::sim::Kind::Read).await;
        #[cfg(not(feature = "sim"))]
        let e = opcode::ReadFixed::new(
            types::Fixed(self.inner.slot),
            buf.addr as *mut u8,
            buf.len,
            buf.index,
        )
        .offset(off)
        .build();
        #[cfg(not(feature = "sim"))]
        self.run(e, buf.len, buf.holds()).await
    }

    /// `Durability::Durable` adds `RWF_DSYNC`: on an `O_DIRECT` block device this is
    /// per-write FUA, stable media when the CQE lands. No flush op; never buffered.
    pub(crate) async fn write(&self, off: u64, buf: Buf, d: Durability) -> Result<(), Errno> {
        self.pace(buf.len).await;
        #[cfg(feature = "sim")]
        {
            // The model has no write-back cache, so acknowledged writes are already stable.
            let _ = d;
            return self.sim(off, buf, crate::sim::Kind::Write).await;
        }
        #[cfg(not(feature = "sim"))]
        let e = opcode::WriteFixed::new(
            types::Fixed(self.inner.slot),
            buf.addr as *const u8,
            buf.len,
            buf.index,
        )
        .offset(off)
        .rw_flags(match d {
            Durability::Buffered => 0,
            Durability::Durable => RWF_DSYNC,
        })
        .build();
        #[cfg(not(feature = "sim"))]
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

    /// The simulated device. Same contract as the real one (one completion, short transfer
    /// is `EIO`), but the transfer happens at completion, so overlapping IO interleaves.
    #[cfg(feature = "sim")]
    async fn sim(&self, off: u64, buf: Buf, kind: crate::sim::Kind) -> Result<(), Errno> {
        let len = buf.len;
        let (hold, tag) = buf.holds();
        let fut = worker::with_local(|l| {
            let (idx, seq) = l
                .ops
                .acquire(&l.pool, 1, hold, tag)
                .expect("op slab exhausted");
            crate::sim::submit(
                self.inner.slot,
                kind,
                off,
                buf.addr,
                len,
                self.deadline(),
                (idx, seq),
            );
            OpFuture {
                idx,
                seq,
                done: false,
            }
        });
        let res = fut.await;
        if res < 0 {
            return Err(Errno(-res));
        }
        if res as u32 != len {
            return Err(Errno::EIO);
        }
        Ok(())
    }

    #[cfg(not(feature = "sim"))]
    async fn run(
        &self,
        e: squeue::Entry,
        len: u32,
        holds: (Option<u16>, Option<u16>),
    ) -> Result<(), Errno> {
        let res = submit_op(e, self.deadline(), holds.0, holds.1).await;
        if res < 0 {
            return Err(Errno(-res));
        }
        // A short transfer on an O_DIRECT device is a failure; we never retry the rest.
        if res as u32 != len {
            return Err(Errno::EIO);
        }
        Ok(())
    }
}

/// `RWF_DSYNC`, from `include/uapi/linux/fs.h`.
const RWF_DSYNC: i32 = 0x00000002;

/// Whether a write must be on stable media before it completes.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum Durability {
    Buffered,
    Durable,
}

/// Submits one operation, optionally guarded by a linked timeout, and awaits it.
#[cfg(not(feature = "sim"))]
async fn submit_op(
    e: squeue::Entry,
    timeout: Option<Duration>,
    hold: Option<u16>,
    tag: Option<u16>,
) -> i32 {
    let fut = worker::with_local(|l| {
        let pending = if timeout.is_some() { 2 } else { 1 };
        let (idx, seq) = l
            .ops
            .acquire(&l.pool, pending, hold, tag)
            .expect("op slab exhausted");
        match timeout {
            None => {
                let e = e.user_data(worker::ud_op(idx, seq));
                l.push(e);
            }
            Some(d) => {
                // The pair must land in the SQ together or the link is broken.
                let ts = l.ops.set_timespec(idx, d);
                let a = e
                    .user_data(worker::ud_op(idx, seq))
                    .flags(squeue::Flags::IO_LINK);
                let b = opcode::LinkTimeout::new(ts)
                    .build()
                    .user_data(worker::ud_link(idx, seq));
                l.push_linked(a, b);
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

pub(super) struct ExportInner {
    pub(super) path: PathBuf,
}

/// A live ublk block device. Holding one keeps the device attached.
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

/// Sleeps on this worker's ring. Timers are ordinary ops: one SQE, no thread.
pub(crate) async fn sleep(d: Duration) {
    let fut = worker::with_local(|l| {
        let (idx, seq) = l
            .ops
            .acquire(&l.pool, 1, None, None)
            .expect("op slab exhausted");
        #[cfg(feature = "sim")]
        crate::sim::sleep(d, idx, seq);
        #[cfg(not(feature = "sim"))]
        {
            // The deadline lives in the op slot, so it outlives the SQE.
            let ts = l.ops.set_timespec(idx, d);
            l.push(
                opcode::Timeout::new(ts)
                    .build()
                    .user_data(worker::ud_op(idx, seq)),
            );
        }
        OpFuture {
            idx,
            seq,
            done: false,
        }
    });
    let _ = fut.await;
}

/// Delivers a simulated completion to the worker the thread-local currently names.
#[cfg(feature = "sim")]
pub(crate) fn sim_complete(idx: u32, seq: u16, res: i32) {
    worker::with_local(|l| l.ops.complete(&l.pool, idx, seq, res, false));
}

/// Builds a `Buf` naming simulator-owned memory, standing in for guest ublk request pages.
#[cfg(feature = "sim")]
pub(crate) fn sim_buf(index: u16, addr: u64, len: u32) -> Buf {
    Buf { index, addr, len }
}

/// The address a `Buf` names. Simulator only: buffers there are ordinary process memory.
#[cfg(feature = "sim")]
pub(crate) fn sim_addr(b: Buf) -> u64 {
    b.addr
}

/// A `Buf` for a ublk request buffer, whose registered base address is zero.
pub(super) fn req_buf(index: u16, len: u32) -> Buf {
    Buf {
        index,
        addr: 0,
        len,
    }
}
