//! The executor: request futures stored in place, a ready FIFO, and wakers that are
//! nothing but a task id. Serving a request allocates nothing.

use std::cell::RefCell;
use std::collections::VecDeque;
use std::future::Future;
use std::mem::MaybeUninit;
use std::pin::Pin;
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

use super::worker;
use super::{Cfg, Errno, Handler, Request};

/// Task ids are `slot | kind`. Two kinds share one FIFO: ublk request futures, and the
/// hop slab's tasks (remote jobs and detached spawns).
pub(super) const KIND_HOP: u32 = 1 << 31;

pub(super) fn is_hop(id: u32) -> bool {
    id & KIND_HOP != 0
}

pub(super) fn slot_of(id: u32) -> u32 {
    id & !KIND_HOP
}

pub(super) struct Ready {
    q: RefCell<VecDeque<u32>>,
}

impl Ready {
    pub(super) fn new(cap: usize) -> Ready {
        Ready {
            q: RefCell::new(VecDeque::with_capacity(cap)),
        }
    }

    pub(super) fn push(&self, id: u32) {
        self.q.borrow_mut().push_back(id);
    }

    pub(super) fn pop(&self) -> Option<u32> {
        self.q.borrow_mut().pop_front()
    }

    pub(super) fn len(&self) -> usize {
        self.q.borrow().len()
    }
}

// ---------------------------------------------------------------------------
// wakers
// ---------------------------------------------------------------------------

static VTABLE: RawWakerVTable = RawWakerVTable::new(clone_w, wake_w, wake_w, drop_w);

unsafe fn clone_w(p: *const ()) -> RawWaker {
    RawWaker::new(p, &VTABLE)
}

unsafe fn wake_w(p: *const ()) {
    let id = p as usize as u32;
    worker::with_local(|l| l.ready.push(id));
}

unsafe fn drop_w(_: *const ()) {}

/// A waker is nothing but the task id: every future in this runtime is polled by the
/// worker that created it, so there is no state to share and nothing to refcount.
pub(super) fn waker_for(id: u32) -> Waker {
    // SAFETY: the vtable never dereferences its data pointer — it is the id — so clone
    // and drop are no-ops and the `RawWaker` contract holds.
    unsafe { Waker::from_raw(RawWaker::new(id as usize as *const (), &VTABLE)) }
}

// ---------------------------------------------------------------------------
// request slab
// ---------------------------------------------------------------------------

struct ReqSlot<F> {
    used: bool,
    fut: MaybeUninit<F>,
}

/// The per-worker request executor.
///
/// Generic over the handler's future type so each future lives in place in the slab;
/// `F` is inferred from the `H::handle` function item, which is why this type is
/// constructed in the worker loop rather than stored in the shared `Local`.
pub(super) struct Exec<H: Handler, F> {
    handler: &'static H,
    make: fn(&'static H, Cfg<H::Config>, Request) -> F,
    slots: Box<[ReqSlot<F>]>,
    live: usize,
}

impl<H: Handler, F> Exec<H, F>
where
    F: Future<Output = Result<(), Errno>>,
{
    pub(super) fn new(
        handler: &'static H,
        make: fn(&'static H, Cfg<H::Config>, Request) -> F,
        n: usize,
    ) -> Exec<H, F> {
        let mut v = Vec::with_capacity(n);
        for _ in 0..n {
            v.push(ReqSlot {
                used: false,
                fut: MaybeUninit::uninit(),
            });
        }
        Exec {
            handler,
            make,
            slots: v.into_boxed_slice(),
            live: 0,
        }
    }

    pub(super) fn handler(&self) -> &'static H {
        self.handler
    }

    /// Starts a request. The slot index is fixed by (device, queue, tag), so there is
    /// no allocation and no free list.
    pub(super) fn start(
        &mut self,
        id: u32,
        cfg: Cfg<H::Config>,
        req: Request,
    ) -> Option<Result<(), Errno>> {
        let s = &mut self.slots[id as usize];
        debug_assert!(!s.used, "request slot reused while live");
        s.used = true;
        s.fut.write((self.make)(self.handler, cfg, req));
        self.live += 1;
        self.poll(id)
    }

    /// Polls one request future. `Some(result)` means the request is finished and the
    /// caller must commit it.
    pub(super) fn poll(&mut self, id: u32) -> Option<Result<(), Errno>> {
        let s = &mut self.slots[id as usize];
        if !s.used {
            return None;
        }
        let w = waker_for(id);
        let mut cx = Context::from_waker(&w);
        // SAFETY: `used` means the slot holds an initialised future, and the slab is
        // boxed, so the future never moves before `assume_init_drop` below.
        let fut = unsafe { Pin::new_unchecked(&mut *s.fut.as_mut_ptr()) };
        match fut.poll(&mut cx) {
            Poll::Pending => None,
            Poll::Ready(r) => {
                unsafe { s.fut.assume_init_drop() };
                s.used = false;
                self.live -= 1;
                Some(r)
            }
        }
    }

    pub(super) fn live_count(&self) -> usize {
        self.live
    }

    /// Size of one request future, logged at boot so slab growth is visible.
    pub(super) fn future_size(&self) -> usize {
        size_of::<F>()
    }
}

impl<H: Handler, F> Drop for Exec<H, F> {
    fn drop(&mut self) {
        // Abandoned requests still own `Cfg` guards and hop cells; drop them in place
        // while the worker's thread-local is still live.
        for s in self.slots.iter_mut().filter(|s| s.used) {
            s.used = false;
            unsafe { s.fut.assume_init_drop() };
        }
    }
}
