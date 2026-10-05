//! Explicitly polled, bounded worker-local tasks independent of request lifetimes.
//!
//! The caller supplies capacity and polling budgets, handles task results, and owns
//! shutdown fencing. Selection is thread-local; queues and permits retain their
//! original owner across nested scopes. No executor or application policy is hidden
//! here. For example, a local proxy can drive detached cache writes alongside I/O.

use crate::{Error, Result};
use std::{
    cell::{Cell, RefCell},
    collections::VecDeque,
    future::Future,
    pin::Pin,
    rc::Rc,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll, Waker},
};

/// A local task. The caller must handle its outcome before returning unit.
pub type Task = Pin<Box<dyn Future<Output = ()> + 'static>>;

/// Poll an optional local task once and clear its slot before returning a result.
/// Pending tasks retain their owner; absent tasks and pending tasks return None.
/// No scope, result-handling, scheduling, or shutdown policy is added.
pub fn poll_task<F: Future + ?Sized>(
    task: &mut Option<Pin<Box<F>>>,
    cx: &mut Context<'_>,
) -> Option<F::Output> {
    match task.as_mut()?.as_mut().poll(cx) {
        Poll::Pending => None,
        Poll::Ready(result) => {
            task.take();
            Some(result)
        }
    }
}

thread_local! {
    static CURRENT: RefCell<Option<Rc<DriverQueue>>> = const { RefCell::new(None) };
}

/// A bounded queue, including reservations not yet submitted. Zero disables admission.
///
/// Queues cannot move between workers:
/// ```compile_fail
/// use uring_runtime::drivers::DriverQueue;
/// fn require_send<T: Send>() {}
/// require_send::<DriverQueue>();
/// ```
pub struct DriverQueue {
    capacity: usize,
    drivers: RefCell<VecDeque<Driver>>,
    new: RefCell<Vec<Task>>,
    count: Cell<usize>,
    owner: RefCell<Option<Rc<Waker>>>,
    polling: Cell<bool>,
}

struct Driver {
    operation: Task,
    wake: Arc<Runnable>,
}

/// Per-future readiness with a thread-safe wake path to the polling owner.
pub struct Runnable {
    ready: AtomicBool,
    owner: futures::task::AtomicWaker,
}

impl Runnable {
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            ready: AtomicBool::new(true),
            owner: futures::task::AtomicWaker::new(),
        })
    }

    /// Poll on first use or after a wake, unless the caller explicitly forces it.
    pub fn poll<T>(
        self: &Arc<Self>,
        future: Pin<&mut (impl Future<Output = T> + ?Sized)>,
        cx: &mut Context<'_>,
        force: bool,
    ) -> Poll<T> {
        self.owner.register(cx.waker());
        if !self.ready.swap(false, Ordering::AcqRel) && !force {
            return Poll::Pending;
        }
        let waker = Waker::from(self.clone());
        future.poll(&mut Context::from_waker(&waker))
    }
}

impl std::task::Wake for Runnable {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }

    fn wake_by_ref(self: &Arc<Self>) {
        self.ready.store(true, Ordering::Release);
        self.owner.wake();
    }
}

/// Restores the previous queue. Keep guards stack-nested, never across suspension.
/// ```compile_fail
/// use uring_runtime::drivers::QueueGuard;
/// fn require_send<T: Send>() {}
/// require_send::<QueueGuard>();
/// ```
pub struct QueueGuard {
    previous: Option<Rc<DriverQueue>>,
}

impl Drop for QueueGuard {
    fn drop(&mut self) {
        CURRENT.with(|current| current.replace(self.previous.take()));
    }
}

impl DriverQueue {
    pub fn new(capacity: usize) -> Self {
        Self {
            capacity,
            drivers: RefCell::new(VecDeque::new()),
            new: RefCell::new(Vec::new()),
            count: Cell::new(0),
            owner: RefCell::new(None),
            polling: Cell::new(false),
        }
    }

    /// Discard simulated process tasks without polling them. Outstanding permits
    /// retain their reservations. Call only outside an active polling turn.
    #[cfg(any(test, feature = "simulation"))]
    pub fn simulation_crash(&self) {
        let drivers = std::mem::take(&mut *self.drivers.borrow_mut());
        let new = std::mem::take(&mut *self.new.borrow_mut());
        self.count.set(self.count.get() - drivers.len() - new.len());
        drop((drivers, new));
    }

    /// Select this queue only during poll/drop, never across async suspension.
    pub fn scope<F: Future>(self: &Rc<Self>, future: F) -> Queued<F> {
        Queued {
            queue: self.clone(),
            future: Some(Box::pin(future)),
        }
    }

    pub fn enter(self: &Rc<Self>) -> QueueGuard {
        QueueGuard {
            previous: CURRENT.with(|current| current.replace(Some(self.clone()))),
        }
    }

    pub fn pending(&self) -> usize {
        self.count.get()
    }

    pub fn poll(self: &Rc<Self>, cx: &mut Context<'_>, budget: usize) {
        // Reject nested turns before touching the outer owner's wake registration.
        let Ok(_turn) = crate::Busy::try_enter(&self.polling) else {
            return;
        };
        let _queue = self.enter();
        let owner = Rc::new(cx.waker().clone());
        let old = self.owner.replace(Some(owner));
        drop(old);
        let new = self.new.take();
        self.drivers
            .borrow_mut()
            .extend(new.into_iter().map(|operation| Driver {
                operation,
                wake: Runnable::new(),
            }));
        let turns = budget.min(self.drivers.borrow().len());
        for _ in 0..turns {
            let Some(driver) = self.drivers.borrow_mut().pop_front() else {
                break;
            };
            // Release capacity before destroying a completed or panicking task.
            let mut active = ActiveDriver {
                driver: Some(driver),
                count: &self.count,
            };
            let driver = active.driver.as_mut().unwrap();
            match driver.wake.poll(Pin::new(&mut driver.operation), cx, false) {
                Poll::Ready(()) => {}
                Poll::Pending => self
                    .drivers
                    .borrow_mut()
                    .push_back(active.driver.take().unwrap()),
            }
        }
        let ready = !self.new.borrow().is_empty()
            || self
                .drivers
                .borrow()
                .iter()
                .any(|driver| driver.wake.ready.load(Ordering::Acquire));
        if budget != 0 && ready {
            cx.waker().wake_by_ref();
        }
    }
}

struct ActiveDriver<'a> {
    driver: Option<Driver>,
    count: &'a Cell<usize>,
}

impl Drop for ActiveDriver<'_> {
    fn drop(&mut self) {
        if self.driver.is_some() {
            self.count.set(self.count.get() - 1);
        }
    }
}

/// A future that selects its queue while polling and destroying its inner future.
/// Even a Send inner future remains worker-local:
/// ```compile_fail
/// use uring_runtime::drivers::Queued;
/// fn require_send<T: Send>() {}
/// require_send::<Queued<std::future::Ready<()>>>();
/// ```
pub struct Queued<F> {
    queue: Rc<DriverQueue>,
    future: Option<Pin<Box<F>>>,
}

impl<F: Future> Future for Queued<F> {
    type Output = F::Output;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let this = self.get_mut();
        let _queue = this.queue.enter();
        this.future
            .as_mut()
            .expect("queued future")
            .as_mut()
            .poll(cx)
    }
}

impl<F> Drop for Queued<F> {
    fn drop(&mut self) {
        let _queue = self.queue.enter();
        self.future.take();
    }
}

fn current() -> Option<Rc<DriverQueue>> {
    CURRENT.with(|current| current.borrow().clone())
}

/// A reservation bound to its original worker, even if another queue is selected.
/// ```compile_fail
/// use uring_runtime::drivers::Permit;
/// fn require_send<T: Send>() {}
/// require_send::<Permit>();
/// ```
pub struct Permit {
    queue: Rc<DriverQueue>,
    reserved: bool,
}

impl Permit {
    /// Discard the outcome, but retain the completed future until queue capacity
    /// has been released. An async wrapper would destroy it during its final poll.
    pub fn submit_detached<F: Future + 'static>(self, driver: F) {
        self.submit(Box::pin(Detached(driver)));
    }

    pub fn submit(mut self, driver: Task) {
        self.queue.new.borrow_mut().push(driver);
        self.reserved = false;
        let waker = self.queue.owner.borrow().clone();
        if let Some(waker) = waker {
            waker.wake_by_ref();
        }
    }
}

struct Detached<F>(F);

impl<F: Future> Future for Detached<F> {
    type Output = ();

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        // SAFETY: the inner future stays pinned until Detached is destroyed.
        unsafe { self.map_unchecked_mut(|this| &mut this.0) }
            .poll(cx)
            .map(|_| ())
    }
}

impl Drop for Permit {
    fn drop(&mut self) {
        if self.reserved {
            self.queue.count.set(self.queue.count.get() - 1);
        }
    }
}

/// Reserve capacity in the selected queue. Missing selection is InvalidConfiguration;
/// exhausted capacity is Overloaded. Dropping an unused permit returns capacity.
pub fn reserve() -> Result<Permit> {
    let queue = current().ok_or(Error::InvalidConfiguration)?;
    if queue.count.get() >= queue.capacity {
        return Err(Error::Overloaded);
    }
    queue.count.set(queue.count.get() + 1);
    Ok(Permit {
        queue,
        reserved: true,
    })
}

pub fn spawn(driver: Task) -> Result<()> {
    reserve()?.submit(driver);
    Ok(())
}

pub fn poll(cx: &mut Context<'_>, budget: usize) {
    if let Some(queue) = current() {
        queue.poll(cx, budget);
    }
}

pub fn pending() -> usize {
    current().map_or(0, |queue| queue.pending())
}

#[cfg(test)]
mod tests {
    use super::*;
    use futures::{channel::oneshot, task::noop_waker};

    #[test]
    fn panic_releases_capacity_before_task_drop_and_queue_remains_usable() {
        struct Panics;
        impl Future for Panics {
            type Output = ();
            fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<()> {
                panic!("injected poll panic");
            }
        }
        impl Drop for Panics {
            fn drop(&mut self) {
                assert_eq!(pending(), 0);
                reserve().unwrap().submit(Box::pin(async {}));
            }
        }
        let queue = Rc::new(DriverQueue::new(1));
        let _owner = queue.enter();
        reserve().unwrap().submit_detached(Panics);
        let mut cx = Context::from_waker(Waker::noop());
        assert!(
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| queue.poll(&mut cx, 1)))
                .is_err()
        );
        assert_eq!(pending(), 1);
        queue.poll(&mut cx, 1);
        assert_eq!(pending(), 0);
    }

    #[test]
    fn budget_backlog_wakes_outer_owner_and_nested_poll_cannot_replace_it() {
        let queue = Rc::new(DriverQueue::new(2));
        let _owner = queue.enter();
        let nested = Arc::new(WakeCount::default());
        let nested_waker = Waker::from(nested.clone());
        spawn(Box::pin(async move {
            poll(&mut Context::from_waker(&nested_waker), 1);
        }))
        .unwrap();
        spawn(Box::pin(async {})).unwrap();
        let outer = Arc::new(WakeCount::default());
        let waker = Waker::from(outer.clone());
        let mut cx = Context::from_waker(&waker);
        queue.poll(&mut cx, 1);
        assert_eq!(outer.count(), 1);
        spawn(Box::pin(async {})).unwrap();
        assert_eq!(outer.count(), 2);
        assert_eq!(nested.count(), 0);
        queue.poll(&mut cx, 2);
        assert_eq!(pending(), 0);
        assert_eq!(outer.count(), 2);
    }

    #[test]
    fn replacing_owner_drops_waker_outside_queue_borrows() {
        struct OnDrop;
        impl std::task::Wake for OnDrop {
            fn wake(self: Arc<Self>) {
                panic!("idle queue must not wake its retired owner");
            }
        }
        impl Drop for OnDrop {
            fn drop(&mut self) {
                // Submission reads the owner; nested polling must be rejected
                // without any owner or driver-table borrow held by its caller.
                spawn(Box::pin(async {})).unwrap();
                poll(&mut Context::from_waker(Waker::noop()), 1);
            }
        }
        let queue = Rc::new(DriverQueue::new(1));
        let _owner = queue.enter();
        queue.poll(&mut Context::from_waker(&Waker::from(Arc::new(OnDrop))), 0);
        queue.poll(&mut Context::from_waker(Waker::noop()), 1);
        assert_eq!(pending(), 0);
    }

    #[test]
    fn panicking_completed_task_destructor_does_not_leak_capacity() {
        struct PanicsOnDrop;
        impl Future for PanicsOnDrop {
            type Output = ();
            fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<()> {
                Poll::Ready(())
            }
        }
        impl Drop for PanicsOnDrop {
            fn drop(&mut self) {
                assert_eq!(pending(), 0);
                panic!("injected destructor panic");
            }
        }
        let queue = Rc::new(DriverQueue::new(1));
        let _owner = queue.enter();
        reserve().unwrap().submit_detached(PanicsOnDrop);
        assert!(
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                queue.poll(&mut Context::from_waker(Waker::noop()), 1);
            }))
            .is_err()
        );
        assert_eq!(pending(), 0);
        assert!(reserve().is_ok());
    }

    #[test]
    fn completed_operation_is_dropped_after_releasing_capacity() {
        struct Complete;
        impl Future for Complete {
            type Output = std::result::Result<(), &'static str>;
            fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<Self::Output> {
                Poll::Ready(Err("detached failure"))
            }
        }
        impl Drop for Complete {
            fn drop(&mut self) {
                assert_eq!(pending(), 1023);
                assert!(reserve().is_ok());
            }
        }
        let queue = Rc::new(DriverQueue::new(1024));
        let _owner = queue.enter();
        let permits: Vec<_> = (0..1023).map(|_| reserve().unwrap()).collect();
        reserve().unwrap().submit_detached(Complete);
        poll(&mut Context::from_waker(futures::task::noop_waker_ref()), 1);
        drop(permits);
        assert_eq!(pending(), 0);
    }

    #[test]
    fn blocked_driver_is_not_repolled_until_its_own_wake() {
        let queue = Rc::new(DriverQueue::new(4));
        let _owner = queue.enter();
        let polls = Rc::new(Cell::new(0));
        let observed = polls.clone();
        let wake = Rc::new(RefCell::new(None::<Waker>));
        let saved = wake.clone();
        spawn(Box::pin(std::future::poll_fn(move |cx| {
            observed.set(observed.get() + 1);
            *saved.borrow_mut() = Some(cx.waker().clone());
            Poll::Pending
        })))
        .unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..100 {
            queue.poll(&mut cx, 64);
        }
        assert_eq!(polls.get(), 1);
        wake.borrow().as_ref().unwrap().wake_by_ref();
        queue.poll(&mut cx, 64);
        assert_eq!(polls.get(), 2);
        queue.simulation_crash();
        assert_eq!(queue.pending(), 0);
    }

    #[test]
    fn workers_isolate_permits_children_and_capacity_on_one_thread() {
        let a = Rc::new(DriverQueue::new(2));
        let b = Rc::new(DriverQueue::new(3));
        let _a = a.enter();
        let permit = reserve().unwrap();
        let completed = Rc::new(Cell::new(false));
        let child = completed.clone();
        {
            let _b = b.enter();
            permit.submit(Box::pin(async move {
                spawn(Box::pin(async move {
                    child.set(true);
                }))
                .unwrap();
            }));
            let permits: Vec<_> = (0..3).map(|_| reserve().unwrap()).collect();
            assert!(matches!(reserve(), Err(Error::Overloaded)));
            assert_eq!(a.pending(), 1);
            drop(permits);
        }
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        b.poll(&mut cx, 8);
        assert!(!completed.get());
        assert_eq!(b.pending(), 0);
        a.poll(&mut cx, 8);
        assert!(!completed.get());
        assert_eq!(a.pending(), 1);
        a.poll(&mut cx, 8);
        assert!(completed.get());
        assert_eq!(a.pending(), 0);
    }

    #[test]
    fn owned_operation_progresses_after_request_receiver_disappears() {
        let queue = Rc::new(DriverQueue::new(1));
        let _owner = queue.enter();
        let complete = Rc::new(Cell::new(false));
        let observed = complete.clone();
        let (completion, fence) = oneshot::channel::<()>();
        let (reply, reader) = oneshot::channel::<()>();
        spawn(Box::pin(async move {
            fence.await.unwrap();
            observed.set(true);
            let _ = reply.send(());
        }))
        .unwrap();
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        poll(&mut cx, 8);
        drop(reader);
        assert!(!complete.get());
        assert_eq!(pending(), 1);
        completion.send(()).unwrap();
        poll(&mut cx, 8);
        assert!(complete.get());
        assert_eq!(pending(), 0);
    }

    #[test]
    fn nested_bootstrap_driver_is_polled_without_recursive_table_borrow() {
        let queue = Rc::new(DriverQueue::new(2));
        let _owner = queue.enter();
        let (send, mut result) = oneshot::channel();
        spawn(Box::pin(async move {
            let (child, receive) = oneshot::channel();
            spawn(Box::pin(async move {
                child.send(7).unwrap();
            }))
            .unwrap();
            let value = receive.await.unwrap();
            let _ = send.send(value);
        }))
        .unwrap();
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        for _ in 0..4 {
            poll(&mut cx, 8);
        }
        assert_eq!(result.try_recv().unwrap(), Some(7));
        assert_eq!(pending(), 0);
    }

    #[test]
    fn acquisition_requires_an_installed_worker_queue() {
        assert!(matches!(reserve(), Err(Error::InvalidConfiguration)));
        let queue = Rc::new(DriverQueue::new(1));
        {
            let _owner = queue.enter();
            let permit = reserve().unwrap();
            assert_eq!(queue.pending(), 1);
            drop(permit);
            assert_eq!(queue.pending(), 0);
        }
        assert!(matches!(reserve(), Err(Error::InvalidConfiguration)));
    }

    #[test]
    fn zero_capacity_budget_and_completion_accounting() {
        let disabled = Rc::new(DriverQueue::new(0));
        {
            let _owner = disabled.enter();
            assert!(matches!(reserve(), Err(Error::Overloaded)));
            assert_eq!(pending(), 0);
        }
        let queue = Rc::new(DriverQueue::new(2));
        let _owner = queue.enter();
        spawn(Box::pin(async {})).unwrap();
        spawn(Box::pin(async {})).unwrap();
        assert!(matches!(reserve(), Err(Error::Overloaded)));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        poll(&mut cx, 0);
        assert_eq!(pending(), 2);
        poll(&mut cx, 1);
        assert_eq!(pending(), 1);
        let permit = reserve().unwrap();
        poll(&mut cx, usize::MAX);
        assert_eq!(pending(), 1);
        drop(permit);
        assert_eq!(pending(), 0);
    }

    #[test]
    fn scoped_poll_and_drop_restore_previous_selection() {
        struct OnDrop(Rc<DriverQueue>);
        impl Drop for OnDrop {
            fn drop(&mut self) {
                assert!(Rc::ptr_eq(&current().unwrap(), &self.0));
                spawn(Box::pin(async {})).unwrap();
            }
        }
        let a = Rc::new(DriverQueue::new(2));
        let b = Rc::new(DriverQueue::new(2));
        let dropper = OnDrop(a.clone());
        let expected = a.clone();
        let mut scoped = Box::pin(a.scope(async move {
            let _dropper = dropper;
            assert!(Rc::ptr_eq(&current().unwrap(), &expected));
            std::future::pending::<()>().await;
        }));
        let _owner = b.enter();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(scoped.as_mut().poll(&mut cx).is_pending());
        assert!(Rc::ptr_eq(&current().unwrap(), &b));
        drop(scoped);
        assert_eq!(a.pending(), 1);
        assert!(Rc::ptr_eq(&current().unwrap(), &b));
        a.poll(&mut cx, 1);
        assert_eq!(a.pending(), 0);
    }

    #[test]
    fn recursive_poll_defers_children_and_crash_preserves_unused_permits() {
        let queue = Rc::new(DriverQueue::new(3));
        let _owner = queue.enter();
        let permit = reserve().unwrap();
        spawn(Box::pin(async {
            spawn(Box::pin(std::future::pending())).unwrap();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            poll(&mut cx, 8);
        }))
        .unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        poll(&mut cx, 8);
        assert_eq!(pending(), 2);
        queue.simulation_crash();
        assert_eq!(pending(), 1);
        drop(permit);
        assert_eq!(pending(), 0);
    }

    use crate::test_util::WakeCounter as WakeCount;

    #[test]
    fn runnable_replaces_owner_preserves_in_poll_wake_and_force() {
        let first = Arc::new(WakeCount::default());
        let second = Arc::new(WakeCount::default());
        let w1 = Waker::from(first.clone());
        let w2 = Waker::from(second.clone());
        let runnable = Runnable::new();
        let polls = Cell::new(0);
        let saved = RefCell::new(None);
        let mut work = Box::pin(std::future::poll_fn(|cx| {
            polls.set(polls.get() + 1);
            *saved.borrow_mut() = Some(cx.waker().clone());
            if polls.get() == 1 {
                cx.waker().wake_by_ref();
            }
            Poll::<()>::Pending
        }));
        assert!(
            runnable
                .poll(work.as_mut(), &mut Context::from_waker(&w1), false)
                .is_pending()
        );
        assert_eq!(first.count(), 1);
        assert!(
            runnable
                .poll(work.as_mut(), &mut Context::from_waker(&w2), false)
                .is_pending()
        );
        assert_eq!(polls.get(), 2);
        assert!(
            runnable
                .poll(work.as_mut(), &mut Context::from_waker(&w2), false)
                .is_pending()
        );
        assert_eq!(polls.get(), 2);
        saved.borrow().as_ref().unwrap().wake_by_ref();
        assert_eq!(second.count(), 1);
        assert_eq!(first.count(), 1);
        assert!(
            runnable
                .poll(work.as_mut(), &mut Context::from_waker(&w2), false)
                .is_pending()
        );
        assert!(
            runnable
                .poll(work.as_mut(), &mut Context::from_waker(&w2), true)
                .is_pending()
        );
        assert_eq!(polls.get(), 4);
    }

    #[test]
    fn submission_wakes_last_polling_owner() {
        let queue = Rc::new(DriverQueue::new(1));
        let _owner = queue.enter();
        let wake = Arc::new(WakeCount::default());
        let waker = Waker::from(wake.clone());
        queue.poll(&mut Context::from_waker(&waker), 0);
        spawn(Box::pin(async {})).unwrap();
        assert_eq!(wake.count(), 1);
    }

    #[test]
    fn task_slot_retains_pending_and_drops_ready_before_returning_result() {
        struct Local {
            ready: Rc<Cell<bool>>,
            dropped: Rc<Cell<bool>>,
            result: Result<usize>,
        }
        impl Future for Local {
            type Output = Result<usize>;
            fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<Self::Output> {
                if self.ready.get() {
                    Poll::Ready(self.result)
                } else {
                    Poll::Pending
                }
            }
        }
        impl Drop for Local {
            fn drop(&mut self) {
                self.dropped.set(true);
            }
        }
        for result in [Ok(7), Err(Error::Io)] {
            let ready = Rc::new(Cell::new(false));
            let dropped = Rc::new(Cell::new(false));
            let mut task = Some(Box::pin(Local {
                ready: ready.clone(),
                dropped: dropped.clone(),
                result,
            }));
            let mut cx = Context::from_waker(Waker::noop());
            assert_eq!(poll_task(&mut task, &mut cx), None);
            assert!(task.is_some());
            assert!(!dropped.get());
            ready.set(true);
            assert_eq!(poll_task(&mut task, &mut cx), Some(result));
            assert!(task.is_none());
            assert!(dropped.get());
            assert_eq!(poll_task(&mut task, &mut cx), None);
        }
    }
}
