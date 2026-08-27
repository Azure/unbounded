//! Optimized cross-core coordination.

use std::cell::RefCell;
use std::collections::VecDeque;
use std::future::Future;
use std::io;
use std::marker::PhantomData;
use std::mem::MaybeUninit;
use std::os::fd::RawFd;
use std::pin::Pin;
use std::ptr;
use std::rc::Rc;
use std::sync::atomic::{AtomicU8, AtomicU32, Ordering};
use std::task::{Context, Poll, Waker};

use super::limits::Limits;
use super::sys::Region;
use super::{CoreId, Handler};

const PAYLOAD_BYTES: usize = 96;
const TASK_BYTES: usize = 640;

/// Accepts an async task for `dst` and returns a future that observes its result.
pub(crate) fn to_async<F, Fut, T>(dst: CoreId, f: F) -> impl Future<Output = T>
where
    F: FnOnce() -> Fut + Send + 'static,
    Fut: Future<Output = T> + 'static,
    T: Send + 'static,
{
    let dst = dst.index();
    const { assert!(size_of::<F>() <= PAYLOAD_BYTES) };
    const { assert!(align_of::<F>() <= 64) };

    let (cell, src) = open_cell::<T>();
    if dst == super::core().index() {
        install_task(f, src, cell);
    } else {
        let msg = Msg::new(
            f,
            |f, cell, src| install_task::<F, Fut, T>(f, src, cell),
            cell,
            src,
        );
        super::worker::with(|l| l.send_hop(dst, msg));
    }
    Reply::waiting(cell)
}

/// Accepts an async task for `dst`, passing its current worker value to the factory.
pub(crate) fn to_async_with<H, F, Fut, T>(dst: CoreId, f: F) -> impl Future<Output = T>
where
    H: Handler,
    F: FnOnce(Rc<H::Worker>) -> Fut + Send + 'static,
    Fut: Future<Output = T> + 'static,
    T: Send + 'static,
{
    to_async(dst, move || f(super::worker::current_worker::<H>()))
}

fn install_task<F, Fut, T>(f: F, src: u16, cell: ReplyCell<T>)
where
    F: FnOnce() -> Fut,
    Fut: Future<Output = T> + 'static,
    T: Send + 'static,
{
    let job: TaskJob<Fut, T> = TaskJob {
        fut: f(),
        cell,
        src,
    };
    let id = super::worker::with(|l| l.hops.insert(job).expect("core task slab exhausted"));
    super::worker::with(|l| l.hops.poll_task(id));
}

/// Same behavior as [`to_async`] but the task is synchronous.
/// Less overhead because it doesn't need a task-slab slot.
pub(crate) fn to<H, F, T>(dst: CoreId, f: F) -> impl Future<Output = T>
where
    H: Handler,
    F: for<'a> FnOnce(CoreId, &'a H::Worker) -> T + Send + 'static,
    T: Send + 'static,
{
    let dst = dst.index();
    if dst == super::core().index() {
        return Reply::ready(with_worker::<H, F, T>(f));
    }
    const { assert!(size_of::<F>() <= PAYLOAD_BYTES) };
    const { assert!(align_of::<F>() <= 64) };
    let (cell, src) = open_cell::<T>();
    let msg = Msg::new(
        f,
        |f, cell, src| {
            reply(src, cell, with_worker::<H, F, T>(f));
        },
        cell,
        src,
    );
    super::worker::with(|l| l.send_hop(dst, msg));
    Reply::waiting(cell)
}

fn with_worker<H, F, T>(f: F) -> T
where
    H: Handler,
    F: for<'a> FnOnce(CoreId, &'a H::Worker) -> T,
{
    let worker: Rc<H::Worker> = super::worker::current_worker::<H>();
    f(super::core(), &worker)
}

fn open_cell<T>() -> (ReplyCell<T>, u16) {
    super::worker::with(|l| (l.hops.open_cell(), l.core as u16))
}

/// Runs `fut` on this core to completion, detached: no handle and no result.
pub(crate) fn spawn_local(fut: impl Future<Output = ()> + 'static) -> bool {
    let task: Detached = Box::pin(fut);
    let Some(id) = super::worker::with(|l| l.hops.insert(task)) else {
        return false;
    };
    super::worker::with(|l| l.hops.poll_task(id));
    true
}

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
    fn new<V, F, T>(value: V, f: F, cell: ReplyCell<T>, src: u16) -> Msg
    where
        V: Send + 'static,
        F: FnOnce(V, ReplyCell<T>, u16) + Send + 'static,
        T: Send + 'static,
    {
        const { assert!(size_of::<(V, F)>() <= PAYLOAD_BYTES) };
        const { assert!(align_of::<(V, F)>() <= 64) };

        let mut msg = Msg {
            payload: [0; PAYLOAD_BYTES],
            run: dispatch::<V, F, T>,
            cell: cell.id,
            src,
            _pad: [0; 2],
        };
        // SAFETY: `payload` is at offset 0 of a 64-aligned struct; const asserts bound the pair.
        unsafe { (msg.payload.as_mut_ptr() as *mut (V, F)).write((value, f)) };
        msg
    }

    fn execute(mut self) {
        // SAFETY: construction installs the dispatcher for the payload's concrete types,
        // and consuming the message ensures the payload is moved out exactly once.
        unsafe { (self.run)(&mut self) };
    }
}

unsafe fn dispatch<V, F, T>(msg: &mut Msg)
where
    F: FnOnce(V, ReplyCell<T>, u16),
{
    // SAFETY: `Msg::new::<V, F, T>` writes this pair and installs this exact dispatcher.
    let (value, f) = unsafe { (msg.payload.as_ptr() as *const (V, F)).read() };
    f(value, ReplyCell::from_id(msg.cell), msg.src);
}

/// A ring's producer and consumer cursors, on separate cache lines. Exactly one cache
/// line pair, so the message slots start at a fixed offset and the whole ring can be a
/// runtime-sized stride within the fabric's mapping rather than a fixed-length array.
#[repr(C, align(64))]
struct RingHead {
    head: AtomicU32,
    _p0: [u8; 60],
    tail: AtomicU32,
    _p1: [u8; 60],
}

const _: () = assert!(size_of::<RingHead>() == 128);
// The slots follow the head immediately, so the head's size must keep them aligned.
const _: () = assert!(size_of::<RingHead>().is_multiple_of(align_of::<Msg>()));

/// A borrowed view of one `src -> dst` ring: its cursors, its slots, and how many.
struct Ring<'a> {
    h: &'a RingHead,
    slots: *const std::cell::UnsafeCell<MaybeUninit<Msg>>,
    cap: u32,
}

impl Ring<'_> {
    /// Pushes from this ring's sole producer, returning the message if the ring is full.
    fn push(&self, msg: Msg) -> Option<Msg> {
        let tail = self.h.tail.load(Ordering::Relaxed);
        if tail.wrapping_sub(self.h.head.load(Ordering::Acquire)) >= self.cap {
            return Some(msg);
        }
        // SAFETY: the SPSC protocol gives the producer exclusive access to this free slot,
        // and `tail % cap` is within the `cap` slots the fabric carved for this ring.
        unsafe { (*(*self.slots.add((tail % self.cap) as usize)).get()).write(msg) };
        self.h.tail.store(tail.wrapping_add(1), Ordering::Release);
        None
    }

    /// Moves all currently published messages to this ring's sole consumer.
    fn drain(&self, mut consume: impl FnMut(Msg)) -> usize {
        let mut done = 0;
        let mut head = self.h.head.load(Ordering::Relaxed);
        let tail = self.h.tail.load(Ordering::Acquire);
        while head != tail {
            // SAFETY: the SPSC protocol gives the consumer exclusive access, and the
            // producer's release store published an initialized message in this slot.
            let msg = unsafe {
                (*(*self.slots.add((head % self.cap) as usize)).get()).assume_init_read()
            };
            head = head.wrapping_add(1);
            self.h.head.store(head, Ordering::Release);
            consume(msg);
            done += 1;
        }
        done
    }

    fn pending(&self) -> usize {
        self.h
            .tail
            .load(Ordering::Acquire)
            .wrapping_sub(self.h.head.load(Ordering::Relaxed)) as usize
    }
}

const STATE_RUNNING: u8 = 0;
const STATE_SLEEPING: u8 = 1;

/// Per-worker liveness and ring fd, so any thread can tell whether a doorbell is needed.
#[repr(C, align(64))]
struct WorkerState {
    state: AtomicU8,
    ring_fd: AtomicU32,
}

/// The hop transport: n^2 rings in one region plus n worker states.
pub(super) struct Fabric {
    region: Region,
    states: Vec<WorkerState>,
    n: usize,
    /// Message slots per ring. A power of two, so a cursor that wraps `u32::MAX` still
    /// indexes the right slot.
    slots: u32,
    /// Bytes from one ring's head to the next.
    stride: usize,
}

impl Fabric {
    pub(super) fn new(n: usize, limits: &Limits) -> io::Result<Fabric> {
        let slots = limits.ring_slots;
        if n == 0 || n > u16::MAX as usize + 1 || slots == 0 || !slots.is_power_of_two() {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }
        let stride = (slots as usize)
            .checked_mul(size_of::<Msg>())
            .and_then(|s| s.checked_add(size_of::<RingHead>()))
            .ok_or_else(|| io::Error::from_raw_os_error(libc::EOVERFLOW))?;
        let bytes = n
            .checked_mul(n)
            .and_then(|rings| rings.checked_mul(stride))
            .ok_or_else(|| io::Error::from_raw_os_error(libc::EOVERFLOW))?;
        let region = Region::new(bytes)?;
        let mut states = Vec::with_capacity(n);
        for _ in 0..n {
            states.push(WorkerState {
                state: AtomicU8::new(STATE_RUNNING),
                ring_fd: AtomicU32::new(u32::MAX),
            });
        }
        Ok(Fabric {
            region,
            states,
            n,
            slots,
            stride,
        })
    }

    pub(super) fn cores(&self) -> usize {
        self.n
    }

    fn ring(&self, src: usize, dst: usize) -> Ring<'_> {
        assert!(src < self.n && dst < self.n, "hop core out of range");
        // SAFETY: `src`, `dst` < n and the region is sized for exactly n*n rings of
        // `stride` bytes, each a head followed by `slots` message slots. The region is
        // page-aligned and `stride` is a multiple of the head's alignment, so every ring
        // starts aligned; anonymous mappings start zeroed, which is a valid `RingHead`.
        unsafe {
            let base = self.region.as_ptr().add((src * self.n + dst) * self.stride);
            Ring {
                h: &*(base as *const RingHead),
                slots: base.add(size_of::<RingHead>()) as *const _,
                cap: self.slots,
            }
        }
    }

    /// Pushes onto the `src -> dst` ring. Returns the message back if it is full.
    fn send(&self, src: usize, dst: usize, msg: Msg) -> Option<Msg> {
        self.ring(src, dst).push(msg)
    }

    pub(super) fn drain(&self, me: usize) -> usize {
        (0..self.n)
            .map(|src| self.ring(src, me).drain(Msg::execute))
            .sum()
    }

    pub(super) fn pending(&self, me: usize) -> usize {
        (0..self.n).map(|src| self.ring(src, me).pending()).sum()
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
        self.states[me]
            .state
            .store(STATE_SLEEPING, Ordering::SeqCst);
    }

    pub(super) fn set_running(&self, me: usize) {
        self.states[me].state.store(STATE_RUNNING, Ordering::SeqCst);
    }

    pub(super) fn is_sleeping(&self, dst: usize) -> bool {
        self.states[dst].state.load(Ordering::SeqCst) == STATE_SLEEPING
    }
}

const CELL_EMPTY: u8 = 0;
const CELL_FULL: u8 = 1;
const CELL_ABANDONED: u8 = 2;

struct ReplyCell<T> {
    id: u32,
    _type: PhantomData<fn() -> T>,
}

impl<T> ReplyCell<T> {
    fn from_id(id: u32) -> ReplyCell<T> {
        const { assert!(size_of::<T>() <= PAYLOAD_BYTES) };
        const { assert!(align_of::<T>() <= 64) };
        ReplyCell {
            id,
            _type: PhantomData,
        }
    }
}

impl<T> Clone for ReplyCell<T> {
    fn clone(&self) -> Self {
        *self
    }
}

impl<T> Copy for ReplyCell<T> {}

/// Where a hop's result lands. Owned and touched only by the caller's core, so no atomics.
#[repr(C, align(64))]
struct CellSlot {
    data: [u8; PAYLOAD_BYTES],
    state: u8,
    used: bool,
    waker: Option<Waker>,
}

struct Cells {
    slots: Vec<CellSlot>,
    free: Vec<u32>,
}

impl Cells {
    fn new(cells: u32) -> Cells {
        let mut slots = Vec::with_capacity(cells as usize);
        for _ in 0..cells {
            slots.push(CellSlot {
                data: [0; PAYLOAD_BYTES],
                state: CELL_EMPTY,
                used: false,
                waker: None,
            });
        }
        Cells {
            slots,
            free: (0..cells).rev().collect(),
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

    fn deliver<T>(&mut self, cell: ReplyCell<T>, value: T) -> (Option<T>, Option<Waker>) {
        let mut value = Some(value);
        let slot = &mut self.slots[cell.id as usize];
        if slot.state == CELL_ABANDONED {
            self.release(cell.id);
            return (value, None);
        }
        // SAFETY: `ReplyCell<T>` establishes the slot's type and bounds its size and
        // alignment; an empty live cell has no initialized payload to overwrite.
        unsafe { (slot.data.as_mut_ptr() as *mut T).write(value.take().unwrap()) };
        slot.state = CELL_FULL;
        (None, slot.waker.take())
    }

    fn take<T>(&mut self, cell: ReplyCell<T>, cx: &mut Context) -> Option<T> {
        let slot = &mut self.slots[cell.id as usize];
        if slot.state != CELL_FULL {
            slot.waker = Some(cx.waker().clone());
            return None;
        }
        // SAFETY: this typed cell's full state means `deliver` wrote one live `T`.
        let value = unsafe { (slot.data.as_ptr() as *const T).read() };
        self.release(cell.id);
        Some(value)
    }

    fn abandon<T>(&mut self, cell: ReplyCell<T>) -> Option<T> {
        let slot = &mut self.slots[cell.id as usize];
        if slot.state == CELL_FULL {
            // SAFETY: this typed cell's full state means `deliver` wrote one live `T`.
            let value = unsafe { (slot.data.as_ptr() as *const T).read() };
            self.release(cell.id);
            Some(value)
        } else {
            slot.state = CELL_ABANDONED;
            slot.waker = None;
            None
        }
    }
}

/// Lands a reply on this core without going through its own ring.
fn deliver_reply<T>(cell: ReplyCell<T>, v: T) {
    let (v, waker) = super::worker::with(|l| l.hops.deliver_reply(cell, v));
    // Drop an abandoned result after releasing the cell slab borrow.
    drop(v);
    if let Some(w) = waker {
        w.wake();
    }
}

fn reply<T: Send + 'static>(src: u16, cell: ReplyCell<T>, v: T) {
    if src as usize == super::core().index() {
        deliver_reply(cell, v);
        return;
    }
    let msg = Msg::new(v, |v, cell, _| deliver_reply(cell, v), cell, src);
    super::worker::with(|l| l.send_hop(src as usize, msg));
}

struct TaskVtable {
    poll: unsafe fn(*mut u8, &mut Context) -> Poll<()>,
    drop: unsafe fn(*mut u8),
}

struct ActiveTask {
    vtable: &'static TaskVtable,
    data: *mut u8,
}

impl ActiveTask {
    fn poll(&self, cx: &mut Context) -> Poll<()> {
        // SAFETY: `Tasks::insert` installs this vtable alongside its concrete value.
        unsafe { (self.vtable.poll)(self.data, cx) }
    }

    fn drop_value(&self) {
        // SAFETY: `Tasks::insert` installs this vtable alongside its concrete value, and
        // callers remove the slot immediately after dropping it.
        unsafe { (self.vtable.drop)(self.data) };
    }
}

trait InlineTask {
    fn poll(self: Pin<&mut Self>, cx: &mut Context) -> Poll<()>;
}

struct TaskJob<Fut, T> {
    fut: Fut,
    cell: ReplyCell<T>,
    src: u16,
}

impl<Fut, T> InlineTask for TaskJob<Fut, T>
where
    Fut: Future<Output = T>,
    T: Send + 'static,
{
    fn poll(self: Pin<&mut Self>, cx: &mut Context) -> Poll<()> {
        // SAFETY: the task job remains pinned in its slab slot until this returns ready.
        let job = unsafe { self.get_unchecked_mut() };
        let (src, cell) = (job.src, job.cell);
        // SAFETY: projecting the pinned job to `fut` does not move the future.
        let fut = unsafe { Pin::new_unchecked(&mut job.fut) };
        match fut.poll(cx) {
            Poll::Ready(v) => {
                reply(src, cell, v);
                Poll::Ready(())
            }
            Poll::Pending => Poll::Pending,
        }
    }
}

unsafe fn task_poll<T: InlineTask>(p: *mut u8, cx: &mut Context) -> Poll<()> {
    // SAFETY: the vtable is installed only beside a live `T`, which remains pinned there.
    let task = unsafe { Pin::new_unchecked(&mut *p.cast::<T>()) };
    task.poll(cx)
}

unsafe fn task_drop<T>(p: *mut u8) {
    // SAFETY: the vtable is installed only beside a live `T` and called exactly once.
    unsafe { ptr::drop_in_place(p.cast::<T>()) };
}

struct TaskType<T>(PhantomData<fn() -> T>);

impl<T: InlineTask> TaskType<T> {
    const VT: TaskVtable = TaskVtable {
        poll: task_poll::<T>,
        drop: task_drop::<T>,
    };
}

struct TaskSlot {
    vtable: Option<&'static TaskVtable>,
    data: [u64; TASK_BYTES / 8],
}

struct Tasks {
    slots: Vec<TaskSlot>,
    free: Vec<u32>,
}

impl Tasks {
    fn new(tasks: u32) -> Tasks {
        let mut slots = Vec::with_capacity(tasks as usize);
        for _ in 0..tasks {
            slots.push(TaskSlot {
                vtable: None,
                data: [0; TASK_BYTES / 8],
            });
        }
        Tasks {
            slots,
            free: (0..tasks).rev().collect(),
        }
    }

    fn insert<T: InlineTask + 'static>(&mut self, value: T) -> Option<u32> {
        const { assert!(size_of::<T>() <= TASK_BYTES) };
        const { assert!(align_of::<T>() <= 8) };

        let id = self.free.pop()?;
        let slot = &mut self.slots[id as usize];
        debug_assert!(slot.vtable.is_none());
        // SAFETY: the free slot is exclusive and the const assertions bound `T`.
        unsafe { slot.data.as_mut_ptr().cast::<T>().write(value) };
        slot.vtable = Some(&TaskType::<T>::VT);
        Some(id)
    }

    fn release(&mut self, id: u32) {
        self.slots[id as usize].vtable = None;
        self.free.push(id);
    }

    /// `None` once the slot is released: a wake can outlive the task it names.
    fn active(&mut self, id: u32) -> Option<ActiveTask> {
        let slot = &mut self.slots[id as usize];
        Some(ActiveTask {
            vtable: slot.vtable?,
            data: slot.data.as_mut_ptr().cast(),
        })
    }

    fn live(&self) -> usize {
        self.slots.len() - self.free.len()
    }
}

/// Hop-owned state for one worker.
pub(super) struct State {
    cells: RefCell<Cells>,
    tasks: RefCell<Tasks>,
    outbox: RefCell<VecDeque<(u16, Msg)>>,
}

impl State {
    pub(super) fn new(limits: &Limits) -> State {
        State {
            cells: RefCell::new(Cells::new(limits.hop_cells)),
            tasks: RefCell::new(Tasks::new(limits.tasks)),
            outbox: RefCell::new(VecDeque::new()),
        }
    }

    fn insert<T: InlineTask + 'static>(&self, value: T) -> Option<u32> {
        self.tasks.borrow_mut().insert(value)
    }

    fn open_cell<T>(&self) -> ReplyCell<T> {
        ReplyCell::from_id(
            self.cells
                .borrow_mut()
                .alloc()
                .expect("hop cell slab exhausted"),
        )
    }

    fn deliver_reply<T>(&self, cell: ReplyCell<T>, value: T) -> (Option<T>, Option<Waker>) {
        self.cells.borrow_mut().deliver(cell, value)
    }

    fn take_reply<T>(&self, cell: ReplyCell<T>, cx: &mut Context) -> Option<T> {
        self.cells.borrow_mut().take(cell, cx)
    }

    fn abandon_reply<T>(&self, cell: ReplyCell<T>) -> Option<T> {
        self.cells.borrow_mut().abandon(cell)
    }

    pub(super) fn poll_task(&self, id: u32) {
        // The slab is preallocated, so these pointers stay valid while the job runs.
        let Some(task) = self.tasks.borrow_mut().active(id) else {
            // A wake can outlive the task it names.
            return;
        };
        let waker = super::exec::task_waker(id);
        let mut cx = Context::from_waker(&waker);
        if task.poll(&mut cx).is_ready() {
            // The reply, if any, is already posted.
            task.drop_value();
            self.tasks.borrow_mut().release(id);
        }
    }

    /// Sends immediately unless an older message is queued or the ring is full.
    pub(super) fn send(&self, fabric: &Fabric, src: usize, dst: usize, msg: Msg) -> bool {
        assert!(dst < fabric.cores(), "hop core out of range");
        if !self.outbox.borrow().is_empty() {
            self.outbox.borrow_mut().push_back((dst as u16, msg));
            return false;
        }
        match fabric.send(src, dst, msg) {
            None => true,
            Some(msg) => {
                self.outbox.borrow_mut().push_back((dst as u16, msg));
                false
            }
        }
    }

    /// Retries the oldest queued message, returning its destination when sent.
    pub(super) fn flush_one(&self, fabric: &Fabric, src: usize) -> Option<usize> {
        let (dst, msg) = self.outbox.borrow_mut().pop_front()?;
        if let Some(msg) = fabric.send(src, dst as usize, msg) {
            self.outbox.borrow_mut().push_front((dst, msg));
            return None;
        }
        Some(dst as usize)
    }

    pub(super) fn is_idle(&self) -> bool {
        self.tasks.borrow().live() == 0 && self.outbox.borrow().is_empty()
    }
}

type Detached = Pin<Box<dyn Future<Output = ()>>>;

impl InlineTask for Detached {
    fn poll(self: Pin<&mut Self>, cx: &mut Context) -> Poll<()> {
        self.get_mut().as_mut().poll(cx)
    }
}

enum ReplyState<T> {
    Ready(Option<T>),
    Waiting(ReplyCell<T>),
    Done,
}

/// The result of accepted work. Dropping it abandons only the result.
struct Reply<T> {
    state: ReplyState<T>,
    /// The cell, the ring and the ambient worker are all this core's.
    _nosend: PhantomData<*const ()>,
}

impl<T> Unpin for Reply<T> {}

impl<T> Reply<T> {
    fn ready(v: T) -> Reply<T> {
        Reply {
            state: ReplyState::Ready(Some(v)),
            _nosend: PhantomData,
        }
    }

    fn waiting(cell: ReplyCell<T>) -> Reply<T> {
        Reply {
            state: ReplyState::Waiting(cell),
            _nosend: PhantomData,
        }
    }
}

impl<T> Future for Reply<T> {
    type Output = T;

    fn poll(self: Pin<&mut Self>, cx: &mut Context) -> Poll<T> {
        let me = self.get_mut();
        match &mut me.state {
            ReplyState::Ready(v) => {
                let v = v.take().expect("reply polled after completion");
                me.state = ReplyState::Done;
                Poll::Ready(v)
            }
            ReplyState::Waiting(cell) => match take_reply(*cell, cx) {
                Some(v) => {
                    me.state = ReplyState::Done;
                    Poll::Ready(v)
                }
                None => Poll::Pending,
            },
            ReplyState::Done => panic!("reply polled after completion"),
        }
    }
}

impl<T> Drop for Reply<T> {
    fn drop(&mut self) {
        if let ReplyState::Waiting(cell) = self.state {
            abandon_reply(cell);
        }
    }
}

fn take_reply<T>(cell: ReplyCell<T>, cx: &mut Context) -> Option<T> {
    super::worker::with(|l| l.hops.take_reply(cell, cx))
}

fn abandon_reply<T>(cell: ReplyCell<T>) {
    let value = super::worker::with(|l| l.hops.abandon_reply(cell));
    // The result may re-enter the runtime when dropped, so release the slab borrow first.
    drop(value);
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::{Arc, Mutex};

    use super::*;

    struct Payload {
        seq: usize,
        drops: Arc<AtomicUsize>,
    }

    impl Drop for Payload {
        fn drop(&mut self) {
            self.drops.fetch_add(1, Ordering::Relaxed);
        }
    }

    #[test]
    fn backpressure_preserves_fifo_and_ownership() {
        let limits = crate::runtime::limits();
        let slots = limits.ring_slots as usize;
        let fabric = Fabric::new(2, limits).unwrap();
        let state = State::new(limits);
        let ring = fabric.ring(0, 1);
        // Start the cursors just below the wrap so the run crosses `u32::MAX`.
        let start = u32::MAX - slots as u32 / 2;
        ring.h.head.store(start, Ordering::Relaxed);
        ring.h.tail.store(start, Ordering::Relaxed);
        let seen = Arc::new(Mutex::new(Vec::new()));
        let drops = Arc::new(AtomicUsize::new(0));

        for seq in 0..slots + 2 {
            let seen = seen.clone();
            let msg = Msg::new(
                Payload {
                    seq,
                    drops: drops.clone(),
                },
                move |payload: Payload, _, _| seen.lock().unwrap().push(payload.seq),
                ReplyCell::<()>::from_id(0),
                0,
            );
            assert_eq!(state.send(&fabric, 0, 1, msg), seq < slots);
        }
        assert_eq!(fabric.pending(1), slots);
        assert_eq!(state.flush_one(&fabric, 0), None);
        assert_eq!(fabric.drain(1), slots);
        assert_eq!(state.flush_one(&fabric, 0), Some(1));
        assert_eq!(state.flush_one(&fabric, 0), Some(1));
        assert_eq!(fabric.drain(1), 2);

        assert_eq!(*seen.lock().unwrap(), (0..slots + 2).collect::<Vec<_>>());
        assert_eq!(drops.load(Ordering::Relaxed), slots + 2);
        assert_eq!(fabric.pending(1), 0);
        assert!(state.is_idle());
    }
}
