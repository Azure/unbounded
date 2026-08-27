//! A worker's scratch memory: power-of-two size classes carved out of one mapping.
//!
//! Everything that is not a guest request buffer is staged here: metadata blocks, fabric
//! payloads, repair pages. The memory is registered with io_uring once at startup and
//! never moves, so a buffer is named by an index rather than a pointer and an IO against
//! it costs no pin and no page walk.
//!
//! A pool belongs to exactly one worker. [`PoolBuf`] is `!Send`, so a buffer cannot leave
//! the core that took it, and the class free lists need no locks. Allocation is a pop off
//! a free list; exhaustion parks the caller on that class's waiter queue rather than
//! failing, since the classes are sized for the workload and a shortfall is backpressure,
//! not an error.
//!
//! Freeing is refcounted against the kernel, not just the owner. An op submitted against
//! a buffer hands the memory to the kernel until its CQE lands, so a `PoolBuf` dropped
//! while an op is still in flight is orphaned rather than recycled, and returns to its
//! class when the last op completes. That is what makes dropping an in-flight IO future
//! safe rather than a use-after-free.

use std::cell::{Cell, RefCell};
use std::collections::VecDeque;
use std::io;
use std::marker::PhantomData;
use std::task::{Poll, Waker};

use super::limits::Limits;
use super::sys::Region;

thread_local! {
    static CURRENT: Cell<*const Pool> = const { Cell::new(std::ptr::null()) };
}

/// Bind `pool` to the calling thread, answering with whatever was bound before.
///
/// The caller must [`leave`] with that answer before dropping the pool. Restoring rather
/// than clearing is what lets one thread carry more than one worker: a worker that blocks
/// mid-turn gives the others a turn, and what it had bound has to come back.
pub(super) fn enter(pool: &Pool) -> *const Pool {
    CURRENT.with(|c| c.replace(pool))
}

pub(super) fn leave(previous: *const Pool) {
    CURRENT.with(|c| c.set(previous));
}

/// The calling worker's pool; panics off a worker thread.
fn current<R>(f: impl FnOnce(&Pool) -> R) -> R {
    let p = CURRENT.with(|c| c.get());
    assert!(!p.is_null(), "racer: no buffer pool on this thread");
    // SAFETY: non-null only between `enter` and `leave`, which bracket the life of the
    // pool the pointer names.
    f(unsafe { &*p })
}

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
    /// Address of each pool buffer; its registered index is the worker's base plus `i`.
    addrs: Vec<u64>,
    sizes: Vec<usize>,
    /// In-flight ops still reading or writing each buffer.
    users: RefCell<Vec<u16>>,
    /// Buffers whose `PoolBuf` is gone but whose last CQE has not arrived.
    orphan: RefCell<Vec<bool>>,
}

impl Pool {
    /// Carves one mapping into the size classes `limits` names.
    pub(super) fn new(limits: &Limits) -> io::Result<Pool> {
        let classes_spec = limits.pool_classes;
        let total: usize = classes_spec.iter().map(|(s, n)| s * n).sum();
        let region = Region::new(total)?;
        let mut addrs = Vec::new();
        let mut sizes = Vec::new();
        let mut classes = Vec::new();
        let mut off = 0usize;
        for &(size, count) in classes_spec {
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

    /// Take a free buffer of at least `len` bytes from this pool, if one is there.
    fn take(&self, len: usize) -> Option<PoolBuf> {
        let c = self.class_of(len);
        let idx = self.classes[c].free.borrow_mut().pop()?;
        Some(PoolBuf {
            index: idx,
            len,
            addr: self.addrs[idx as usize],
            _nosend: PhantomData,
        })
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

    #[cfg(test)]
    pub(super) fn waiter_count(&self, len: usize) -> usize {
        self.classes[self.class_of(len)].waiters.borrow().len()
    }
}

/// Registered scratch memory owned by the current worker. Readable and writable, unlike
/// the runtime's opaque `Buf`. Returns to the worker's pool on drop; `!Send`, so it never
/// escapes.
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
            current(|pool| {
                if let Some(b) = pool.take(len) {
                    return Poll::Ready(b);
                }
                let c = pool.class_of(len);
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
        current(|pool| pool.take(len))
    }

    /// This buffer's index within its pool. The runtime's registered buffer index is this
    /// plus the base it reserved the pool's indices at.
    pub(super) fn index(&self) -> u16 {
        self.index
    }

    /// Address of the first byte, for an SQE that names registered memory.
    pub(super) fn addr(&self) -> u64 {
        self.addr
    }

    /// Narrow the buffer to its first `len` bytes, so a caller that staged two records in
    /// one allocation can hand the first out alone: both the slice and the registered
    /// handle stop there. The pool class is unchanged, so this gives no memory back and
    /// the whole buffer still returns on drop. Panics if `len` is longer than the current
    /// view.
    pub(crate) fn truncate(&mut self, len: usize) {
        assert!(len <= self.len, "PoolBuf::truncate beyond the buffer");
        self.len = len;
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
        current(|pool| pool.release(self.index));
    }
}
