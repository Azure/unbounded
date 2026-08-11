//! Cross-core hops.
//!
//! Workers share no mutable state: to touch another core's data you send it a closure
//! and get a value back. The transport is an n² matrix of SPSC rings of fixed 128-byte
//! messages, each carrying an inline payload plus the one function pointer that knows
//! how to interpret it.
//!
//! Calls and replies are the same message; a reply lands in the caller's rendezvous
//! cell. That cell lives in the caller's own per-core slab, so completing, abandoning
//! and polling a hop are all single-threaded on the caller's core.

use std::cell::RefCell;
use std::collections::VecDeque;
use std::future::Future;
use std::io;
use std::marker::PhantomData;
use std::os::fd::RawFd;
use std::pin::Pin;
use std::ptr;
use std::sync::atomic::{AtomicU8, AtomicU32, Ordering};
use std::task::{Context, Poll, Waker};

use super::sys::Region;
use super::worker;

/// Slots per directed ring. Sized so a burst of hops never spills into the outbox.
#[cfg(not(feature = "sim"))]
const RING_SLOTS: usize = 256;
#[cfg(feature = "sim")]
const RING_SLOTS: usize = 32;
/// Inline bytes in a message: a closure's captures or a returned value.
const PAYLOAD_BYTES: usize = 96;
/// Bytes reserved per in-flight remote future. The largest is the allocator's metadata
/// commit: a stage, a group-commit wait and the mblock write it may issue.
const HOP_TASK_BYTES: usize = 576;
/// Remote futures a worker can host at once.
#[cfg(not(feature = "sim"))]
const HOP_TASKS: u32 = 1024;
/// Outstanding hops a worker can have in flight.
#[cfg(not(feature = "sim"))]
const HOP_CELLS: u32 = 1024;

#[cfg(feature = "sim")]
const HOP_TASKS: u32 = 64;
#[cfg(feature = "sim")]
const HOP_CELLS: u32 = 64;

// ---------------------------------------------------------------------------
// messages and rings
// ---------------------------------------------------------------------------

/// One ring slot. `run` is the only thing that knows what `payload` holds, so calls
/// and replies need no discriminant.
#[repr(C, align(64))]
pub(super) struct Msg {
    payload: [u8; PAYLOAD_BYTES],
    run: unsafe fn(&mut Msg),
    cell: u32,
    src: u16,
    _pad: [u8; 2],
}

const _: () = assert!(size_of::<Msg>() == 128);

impl Msg {
    fn new(run: unsafe fn(&mut Msg), cell: u32, src: u16) -> Msg {
        Msg {
            payload: [0; PAYLOAD_BYTES],
            run,
            cell,
            src,
            _pad: [0; 2],
        }
    }

    /// Writes `v` into the inline payload. The paired `run` must read the same type
    /// back out.
    fn put<T>(&mut self, v: T) {
        const { assert!(size_of::<T>() <= PAYLOAD_BYTES) };
        const { assert!(align_of::<T>() <= 64) };
        // SAFETY: `payload` is at offset 0 of a 64-aligned struct and the const
        // asserts above bound `T`'s size and alignment.
        unsafe { (self.payload.as_mut_ptr() as *mut T).write(v) };
    }

    /// Moves the inline payload out as a `T`. Must match the `put` that wrote it.
    unsafe fn take<T>(&mut self) -> T {
        unsafe { (self.payload.as_ptr() as *const T).read() }
    }
}

#[repr(C, align(64))]
struct Ring {
    head: AtomicU32,
    _p0: [u8; 60],
    tail: AtomicU32,
    _p1: [u8; 60],
    slots: [std::cell::UnsafeCell<Msg>; RING_SLOTS],
}

const STATE_RUNNING: u8 = 0;
const STATE_SLEEPING: u8 = 1;

/// Per-worker liveness and ring fd, so any thread can tell whether a doorbell is
/// needed and which ring to ring.
#[repr(C, align(64))]
struct WorkerState {
    state: AtomicU8,
    ring_fd: AtomicU32,
}

/// The hop transport: n² rings in one region plus n worker states.
pub(super) struct Fabric {
    region: Region,
    states: Vec<WorkerState>,
    n: usize,
}

// SAFETY: every field is an atomic or is reached only through the SPSC protocol
// below, which is what makes cross-thread access sound.
unsafe impl Send for Fabric {}
unsafe impl Sync for Fabric {}

impl Fabric {
    pub(super) fn new(n: usize) -> io::Result<Fabric> {
        let region = Region::new(n * n * size_of::<Ring>())?;
        let mut states = Vec::with_capacity(n);
        for _ in 0..n {
            states.push(WorkerState {
                state: AtomicU8::new(STATE_RUNNING),
                ring_fd: AtomicU32::new(u32::MAX),
            });
        }
        Ok(Fabric { region, states, n })
    }

    pub(super) fn cores(&self) -> usize {
        self.n
    }

    fn ring(&self, src: usize, dst: usize) -> &Ring {
        debug_assert!(src < self.n && dst < self.n);
        // SAFETY: `src`, `dst` < n and the region is sized for exactly n*n rings;
        // anonymous mappings start zeroed, which is a valid `Ring`.
        unsafe { &*(self.region.as_ptr() as *const Ring).add(src * self.n + dst) }
    }

    /// Pushes onto the `src -> dst` ring. Returns the message back if it is full.
    pub(super) fn send(&self, src: usize, dst: usize, msg: Msg) -> Option<Msg> {
        let r = self.ring(src, dst);
        let tail = r.tail.load(Ordering::Relaxed);
        if tail.wrapping_sub(r.head.load(Ordering::Acquire)) >= RING_SLOTS as u32 {
            return Some(msg);
        }
        // SAFETY: sole producer for this ring; the slot is free per the check above.
        unsafe { ptr::write(r.slots[tail as usize % RING_SLOTS].get(), msg) };
        r.tail.store(tail.wrapping_add(1), Ordering::Release);
        None
    }

    /// Runs every message addressed to `me`; returns how many were handled.
    pub(super) fn drain(&self, me: usize) -> usize {
        let mut done = 0;
        for src in 0..self.n {
            let r = self.ring(src, me);
            let mut head = r.head.load(Ordering::Relaxed);
            let tail = r.tail.load(Ordering::Acquire);
            while head != tail {
                // SAFETY: sole consumer; the producer released this slot.
                let mut msg = unsafe { ptr::read(r.slots[head as usize % RING_SLOTS].get()) };
                head = head.wrapping_add(1);
                r.head.store(head, Ordering::Release);
                // SAFETY: `run` was paired with `payload` by whoever built the message.
                unsafe { (msg.run)(&mut msg) };
                done += 1;
            }
        }
        done
    }

    pub(super) fn pending(&self, me: usize) -> usize {
        (0..self.n)
            .map(|src| {
                let r = self.ring(src, me);
                r.tail
                    .load(Ordering::Acquire)
                    .wrapping_sub(r.head.load(Ordering::Relaxed)) as usize
            })
            .sum()
    }

    pub(super) fn publish(&self, me: usize, fd: RawFd) {
        self.states[me].ring_fd.store(fd as u32, Ordering::Release);
    }

    pub(super) fn ring_fd(&self, dst: usize) -> Option<RawFd> {
        match self.states[dst].ring_fd.load(Ordering::Acquire) {
            u32::MAX => None,
            fd => Some(fd as RawFd),
        }
    }

    pub(super) fn set_sleeping(&self, me: usize) {
        self.states[me].state.store(STATE_SLEEPING, Ordering::SeqCst);
    }

    pub(super) fn set_running(&self, me: usize) {
        self.states[me].state.store(STATE_RUNNING, Ordering::SeqCst);
    }

    pub(super) fn is_sleeping(&self, dst: usize) -> bool {
        self.states[dst].state.load(Ordering::SeqCst) == STATE_SLEEPING
    }
}

// ---------------------------------------------------------------------------
// rendezvous cells (caller side)
// ---------------------------------------------------------------------------

const CELL_EMPTY: u8 = 0;
const CELL_FULL: u8 = 1;
const CELL_ABANDONED: u8 = 2;

/// Where a hop's result lands. Owned by the caller's core and touched only there — the
/// reply trampoline runs on the caller's own worker loop — so no atomics.
#[repr(C, align(64))]
struct CellSlot {
    data: [u8; PAYLOAD_BYTES],
    state: u8,
    used: bool,
    waker: Option<Waker>,
}

pub(super) struct Cells {
    slots: Vec<CellSlot>,
    free: Vec<u32>,
}

impl Cells {
    pub(super) fn new() -> Cells {
        let mut slots = Vec::with_capacity(HOP_CELLS as usize);
        for _ in 0..HOP_CELLS {
            slots.push(CellSlot {
                data: [0; PAYLOAD_BYTES],
                state: CELL_EMPTY,
                used: false,
                waker: None,
            });
        }
        Cells {
            slots,
            free: (0..HOP_CELLS).rev().collect(),
        }
    }

    fn alloc(&mut self) -> Option<u32> {
        let id = self.free.pop()?;
        let s = &mut self.slots[id as usize];
        s.state = CELL_EMPTY;
        s.used = true;
        s.waker = None;
        Some(id)
    }

    fn release(&mut self, id: u32) {
        let s = &mut self.slots[id as usize];
        debug_assert!(s.used);
        s.used = false;
        s.waker = None;
        self.free.push(id);
    }
}

/// Reply side, on the caller's core: land `T` in the caller's cell, or drop it if the
/// caller walked away.
unsafe fn reply_trampoline<T>(msg: &mut Msg) {
    let id = msg.cell;
    let waker = worker::with_local(|l| {
        let mut cells = l.cells.borrow_mut();
        let s = &mut cells.slots[id as usize];
        if s.state == CELL_ABANDONED {
            // SAFETY: the payload holds a live `T` written by `reply`.
            unsafe { ptr::drop_in_place(msg.payload.as_mut_ptr() as *mut T) };
            cells.release(id);
            return None;
        }
        // SAFETY: `data` is at offset 0 of a 64-aligned struct and `T` fit the
        // payload on the way out.
        unsafe { (s.data.as_mut_ptr() as *mut T).write(msg.take::<T>()) };
        s.state = CELL_FULL;
        s.waker.take()
    });
    if let Some(w) = waker {
        w.wake();
    }
}

/// Sends a finished remote future's value home.
fn reply<T>(src: u16, cell: u32, v: T) {
    let mut msg = Msg::new(reply_trampoline::<T>, cell, src);
    msg.put(v);
    worker::send_hop(src as usize, msg);
}

// ---------------------------------------------------------------------------
// remote futures (callee side)
// ---------------------------------------------------------------------------

/// A remote future plus where to send its value. Lives in the callee's task slab.
struct HopJob<Fut, T> {
    fut: Fut,
    cell: u32,
    src: u16,
    _t: PhantomData<fn() -> T>,
}

pub(super) struct HopVt {
    poll: unsafe fn(*mut u8, &mut Context) -> Poll<()>,
    drop: unsafe fn(*mut u8),
}

unsafe fn job_poll<Fut: Future<Output = T>, T>(p: *mut u8, cx: &mut Context) -> Poll<()> {
    let job = p as *mut HopJob<Fut, T>;
    // SAFETY: the job never moves out of its slab slot until it is dropped.
    let fut = unsafe { Pin::new_unchecked(&mut (*job).fut) };
    match fut.poll(cx) {
        Poll::Ready(v) => {
            let (src, cell) = unsafe { ((*job).src, (*job).cell) };
            reply(src, cell, v);
            Poll::Ready(())
        }
        Poll::Pending => Poll::Pending,
    }
}

unsafe fn job_drop<Fut, T>(p: *mut u8) {
    unsafe { ptr::drop_in_place(p as *mut HopJob<Fut, T>) };
}

struct Vt<Fut, T>(PhantomData<(Fut, T)>);

impl<Fut: Future<Output = T>, T> Vt<Fut, T> {
    const VT: HopVt = HopVt {
        poll: job_poll::<Fut, T>,
        drop: job_drop::<Fut, T>,
    };
}

struct HopSlot {
    used: bool,
    vt: Option<&'static HopVt>,
    data: [u64; HOP_TASK_BYTES / 8],
}

pub(super) struct HopTasks {
    slots: Vec<HopSlot>,
    free: Vec<u32>,
}

impl HopTasks {
    pub(super) fn new() -> HopTasks {
        let mut slots = Vec::with_capacity(HOP_TASKS as usize);
        for _ in 0..HOP_TASKS {
            slots.push(HopSlot {
                used: false,
                vt: None,
                data: [0; HOP_TASK_BYTES / 8],
            });
        }
        HopTasks {
            slots,
            free: (0..HOP_TASKS).rev().collect(),
        }
    }

    fn alloc(&mut self) -> Option<u32> {
        let id = self.free.pop()?;
        self.slots[id as usize].used = true;
        Some(id)
    }

    pub(super) fn release(&mut self, id: u32) {
        self.slots[id as usize].used = false;
        self.slots[id as usize].vt = None;
        self.free.push(id);
    }

    pub(super) fn data_ptr(&self, id: u32) -> *mut u8 {
        self.slots[id as usize].data.as_ptr() as *mut u8
    }

    /// `None` once the slot has been handed back: a task can be woken more than once
    /// before it is polled, so a wake can outlive the task it names.
    pub(super) fn vt(&self, id: u32) -> Option<&'static HopVt> {
        self.slots[id as usize].vt
    }

    pub(super) fn live(&self) -> usize {
        self.slots.len() - self.free.len()
    }
}

impl HopVt {
    pub(super) unsafe fn poll(&self, p: *mut u8, cx: &mut Context) -> Poll<()> {
        unsafe { (self.poll)(p, cx) }
    }
    pub(super) unsafe fn drop_in_place(&self, p: *mut u8) {
        unsafe { (self.drop)(p) }
    }
}

// ---------------------------------------------------------------------------
// detached local tasks
// ---------------------------------------------------------------------------

/// A future with nowhere to send its value. Shares the remote futures' slab because it
/// needs the same thing: somewhere to live between polls that is not the caller's stack.
type Detached = Pin<Box<dyn Future<Output = ()>>>;

unsafe fn detached_poll(p: *mut u8, cx: &mut Context) -> Poll<()> {
    // SAFETY: the slot holds a live `Detached` until `detached_drop` runs.
    unsafe { (*(p as *mut Detached)).as_mut().poll(cx) }
}

unsafe fn detached_drop(p: *mut u8) {
    unsafe { ptr::drop_in_place(p as *mut Detached) };
}

static DETACHED_VT: HopVt = HopVt {
    poll: detached_poll,
    drop: detached_drop,
};

/// Runs `fut` on this core until it finishes, with no handle to await it. Boxing keeps
/// the slot size independent of what the future captures.
///
/// Returns false when the slab is full, having dropped the future unpolled. The runtime
/// counts live tasks to decide it has quiesced, so a task that cannot start must not be
/// queued for later; every caller would rather skip a round than wait.
pub(super) fn spawn(fut: impl Future<Output = ()> + 'static) -> bool {
    let job: Detached = Box::pin(fut);
    let Some(id) = worker::with_local(|l| {
        let id = l.hops.borrow_mut().alloc()?;
        let p = l.hops.borrow().data_ptr(id) as *mut Detached;
        // SAFETY: the slot is exclusively ours and a boxed future is two words.
        unsafe { p.write(job) };
        l.hops.borrow_mut().slots[id as usize].vt = Some(&DETACHED_VT);
        Some(id)
    }) else {
        return false;
    };
    worker::poll_hop_task(id);
    true
}

/// Call side, on the destination core: rebuild the closure, run it, and park the
/// resulting future in the task slab.
unsafe fn call_trampoline<F, Fut, T>(msg: &mut Msg)
where
    F: FnOnce() -> Fut,
    Fut: Future<Output = T>,
{
    const { assert!(size_of::<HopJob<Fut, T>>() <= HOP_TASK_BYTES) };
    const { assert!(align_of::<HopJob<Fut, T>>() <= 8) };

    // SAFETY: paired with the `put::<F>` in `Hop::poll`.
    let f = unsafe { msg.take::<F>() };
    let job = HopJob {
        fut: f(),
        cell: msg.cell,
        src: msg.src,
        _t: PhantomData,
    };
    let id = worker::with_local(|l| {
        let id = l.hops.borrow_mut().alloc().expect("hop task slab exhausted");
        let p = l.hops.borrow().data_ptr(id) as *mut HopJob<Fut, T>;
        // SAFETY: slot is exclusively ours and large enough per the const asserts.
        unsafe { p.write(job) };
        l.hops.borrow_mut().slots[id as usize].vt = Some(&Vt::<Fut, T>::VT);
        id
    });
    worker::poll_hop_task(id);
}

// ---------------------------------------------------------------------------
// the caller's future
// ---------------------------------------------------------------------------

enum Stage<F, Fut> {
    Init(F),
    /// Destination is this core: no message, run it inline.
    Here(Fut),
    Sent,
    Done,
}

/// Awaiting this runs `f` on `dst` and brings its value back. Dropping it before it
/// resolves abandons the rendezvous: the remote future still runs to completion, but
/// its value is discarded when it arrives. That is what makes quorum fan-in cheap.
pub(super) struct Hop<F, Fut, T> {
    dst: usize,
    cell: u32,
    stage: Stage<F, Fut>,
    _m: PhantomData<fn() -> T>,
}

impl<F, Fut, T> Hop<F, Fut, T> {
    pub(super) fn new(dst: usize, f: F) -> Hop<F, Fut, T> {
        Hop {
            dst,
            cell: u32::MAX,
            stage: Stage::Init(f),
            _m: PhantomData,
        }
    }
}

impl<F, Fut, T> Future for Hop<F, Fut, T>
where
    F: FnOnce() -> Fut,
    Fut: Future<Output = T>,
{
    type Output = T;

    fn poll(self: Pin<&mut Self>, cx: &mut Context) -> Poll<T> {
        // SAFETY: `stage` is never moved once it holds a future.
        let me = unsafe { self.get_unchecked_mut() };
        loop {
            match &mut me.stage {
                Stage::Init(_) => {
                    let Stage::Init(f) = std::mem::replace(&mut me.stage, Stage::Done) else {
                        unreachable!()
                    };
                    if me.dst == worker::core() {
                        me.stage = Stage::Here(f());
                        continue;
                    }
                    const { assert!(size_of::<F>() <= PAYLOAD_BYTES) };
                    const { assert!(align_of::<F>() <= 64) };
                    let (cell, src) = worker::with_local(|l| {
                        (
                            l.cells.borrow_mut().alloc().expect("hop cell slab exhausted"),
                            l.core as u16,
                        )
                    });
                    me.cell = cell;
                    let mut msg = Msg::new(call_trampoline::<F, Fut, T>, cell, src);
                    msg.put(f);
                    worker::send_hop(me.dst, msg);
                    me.stage = Stage::Sent;
                }
                Stage::Here(fut) => {
                    // SAFETY: `me` is pinned and `stage` is not moved out of here.
                    let p = unsafe { Pin::new_unchecked(fut) };
                    return match p.poll(cx) {
                        Poll::Ready(v) => {
                            me.stage = Stage::Done;
                            Poll::Ready(v)
                        }
                        Poll::Pending => Poll::Pending,
                    };
                }
                Stage::Sent => {
                    let id = me.cell;
                    let out = worker::with_local(|l| {
                        let mut cells = l.cells.borrow_mut();
                        let s = &mut cells.slots[id as usize];
                        if s.state != CELL_FULL {
                            s.waker = Some(cx.waker().clone());
                            return None;
                        }
                        // SAFETY: the reply trampoline wrote a live `T` here.
                        let v = unsafe { (s.data.as_ptr() as *const T).read() };
                        cells.release(id);
                        Some(v)
                    });
                    return match out {
                        Some(v) => {
                            me.stage = Stage::Done;
                            Poll::Ready(v)
                        }
                        None => Poll::Pending,
                    };
                }
                Stage::Done => panic!("hop polled after completion"),
            }
        }
    }
}

impl<F, Fut, T> Drop for Hop<F, Fut, T> {
    fn drop(&mut self) {
        if !matches!(self.stage, Stage::Sent) {
            return;
        }
        // Abandon: if the reply already landed, drop it here; otherwise the cell
        // outlives us and the reply trampoline frees it.
        let id = self.cell;
        worker::with_local(|l| {
            let mut cells = l.cells.borrow_mut();
            let s = &mut cells.slots[id as usize];
            if s.state == CELL_FULL {
                // SAFETY: a live `T` landed before we were dropped.
                unsafe { ptr::drop_in_place(s.data.as_mut_ptr() as *mut T) };
                cells.release(id);
            } else {
                s.state = CELL_ABANDONED;
                s.waker = None;
            }
        });
    }
}

/// Messages a worker could not push because the destination ring was full. Drained by
/// the worker loop so no send path can fail or block.
pub(super) type Outbox = RefCell<VecDeque<(u16, Msg)>>;
