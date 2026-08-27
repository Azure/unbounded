//! The executor: futures stored in place, a ready FIFO, id-only wakers, no allocation.

use std::cell::RefCell;
use std::collections::VecDeque;
use std::future::Future;
use std::mem::MaybeUninit;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

use super::{Errno, Handler, Request};

struct ReqSlot<F> {
    used: bool,
    fut: MaybeUninit<F>,
}

/// The per-worker request executor. Generic over the handler's future type so futures
/// live in place in the slab; `F` is inferred from `H::handle`, so this type is built in
/// the worker loop rather than stored in the shared `Local`.
pub(super) struct Exec<H: Handler, F> {
    make: fn(Rc<H::Worker>, Request) -> F,
    slots: Box<[ReqSlot<F>]>,
    live: usize,
}

impl<H: Handler, F> Exec<H, F>
where
    F: Future<Output = Result<(), Errno>>,
{
    pub(super) fn new(make: fn(Rc<H::Worker>, Request) -> F, n: usize) -> Exec<H, F> {
        let mut v = Vec::with_capacity(n);
        for _ in 0..n {
            v.push(ReqSlot {
                used: false,
                fut: MaybeUninit::uninit(),
            });
        }
        Exec {
            make,
            slots: v.into_boxed_slice(),
            live: 0,
        }
    }

    pub(super) fn start(
        &mut self,
        id: u32,
        worker: Rc<H::Worker>,
        req: Request,
    ) -> Option<Result<(), Errno>> {
        let s = &mut self.slots[id as usize];
        debug_assert!(!s.used, "request slot reused while live");
        s.used = true;
        s.fut.write((self.make)(worker, req));
        self.live += 1;
        self.poll(id)
    }

    /// Polls one request future. `Some` means finished and the caller must commit it.
    pub(super) fn poll(&mut self, id: u32) -> Option<Result<(), Errno>> {
        let s = &mut self.slots[id as usize];
        if !s.used {
            return None;
        }

        let w = waker_for(id);
        let mut cx = Context::from_waker(&w);

        // SAFETY: `used` means the slot holds an initialized future, and the slab is
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
        // Abandoned futures own worker values and hop cells; drop while thread-local lives.
        for s in self.slots.iter_mut().filter(|s| s.used) {
            s.used = false;
            unsafe { s.fut.assume_init_drop() };
        }
    }
}

const KIND_TASK: u32 = 1 << 31;

pub(super) fn is_task(id: u32) -> bool {
    id & KIND_TASK != 0
}

pub(super) fn slot_of(id: u32) -> u32 {
    id & !KIND_TASK
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

pub(super) fn task_waker(id: u32) -> Waker {
    waker_for(KIND_TASK | id)
}

static VTABLE: RawWakerVTable = RawWakerVTable::new(clone_w, wake_w, wake_w, drop_w);

unsafe fn clone_w(p: *const ()) -> RawWaker {
    RawWaker::new(p, &VTABLE)
}

unsafe fn wake_w(p: *const ()) {
    let id = p as usize as u32;
    super::worker::with(|l| l.ready.push(id));
}

unsafe fn drop_w(_: *const ()) {}

fn waker_for(id: u32) -> Waker {
    unsafe { Waker::from_raw(RawWaker::new(id as usize as *const (), &VTABLE)) }
}

#[cfg(test)]
mod tests {
    use std::rc::Rc;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering};

    use super::*;
    use crate::runtime::{Buf, Op};

    static DROPS: AtomicUsize = AtomicUsize::new(0);

    struct TestHandler;

    impl Handler for TestHandler {
        type Config = ();
        type Worker = ();

        fn build_worker(_: super::super::CoreId, _: Arc<()>, _: Option<&()>) {}

        async fn handle(_: Rc<()>, _: Request) -> Result<(), Errno> {
            unreachable!()
        }
    }

    struct DropSpy;

    impl Drop for DropSpy {
        fn drop(&mut self) {
            DROPS.fetch_add(1, Ordering::Relaxed);
        }
    }

    async fn make(_: Rc<()>, req: Request) -> Result<(), Errno> {
        let _drop = DropSpy;
        if req.lba != 0 {
            let mut pending = true;
            std::future::poll_fn(move |_| {
                if std::mem::take(&mut pending) {
                    Poll::Pending
                } else {
                    Poll::Ready(())
                }
            })
            .await;
        }
        Ok(())
    }

    #[test]
    fn future_slots_complete_reuse_and_drop_in_place() {
        let worker = || Rc::new(());
        let req = Request {
            dev: 0,
            op: Op::Read,
            lba: 0,
            buf: Buf {
                index: 0,
                addr: 0,
                len: 1,
            },
        };
        DROPS.store(0, Ordering::Relaxed);
        let mut exec = Exec::<TestHandler, _>::new(make, 2);

        assert_eq!(exec.poll(0), None);
        assert_eq!(exec.start(0, worker(), req), Some(Ok(())));
        assert_eq!(exec.live_count(), 0);
        assert_eq!(exec.poll(0), None);
        assert_eq!(exec.start(0, worker(), Request { lba: 1, ..req }), None);
        assert_eq!(exec.live_count(), 1);
        assert_eq!(exec.poll(0), Some(Ok(())));
        assert_eq!(exec.start(1, worker(), Request { lba: 1, ..req }), None);
        drop(exec);

        assert_eq!(DROPS.load(Ordering::Relaxed), 3);
    }
}
