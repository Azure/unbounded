use std::cell::{Cell, RefCell};
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Wake, Waker};

use super::*;
use crate::ErrorKind;

/// Counts wakes; used as an owner waker.
#[derive(Default)]
struct WakeCount(AtomicUsize);

impl WakeCount {
    fn count(&self) -> usize {
        self.0.load(Ordering::SeqCst)
    }
}

impl Wake for WakeCount {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }

    fn wake_by_ref(self: &Arc<Self>) {
        self.0.fetch_add(1, Ordering::SeqCst);
    }
}

fn noop_cx() -> Context<'static> {
    Context::from_waker(Waker::noop())
}

/// Poll a join handle once with a no-op waker.
fn try_join<T>(handle: &mut JoinHandle<T>) -> Poll<crate::Result<T>> {
    Pin::new(handle).poll(&mut noop_cx())
}

/// A task that records its polls and saves its waker, then stays pending.
fn sleeper(polls: Rc<Cell<usize>>, saved: Rc<RefCell<Option<Waker>>>) -> impl Future<Output = ()> {
    std::future::poll_fn(move |cx| {
        polls.set(polls.get() + 1);
        *saved.borrow_mut() = Some(cx.waker().clone());
        Poll::Pending
    })
}

#[test]
fn spawn_runs_and_join_returns_output_after_future_drop() {
    struct Probe(Rc<Cell<bool>>);
    impl Drop for Probe {
        fn drop(&mut self) {
            self.0.set(true);
        }
    }
    let executor = LocalExecutor::new(4);
    let dropped = Rc::new(Cell::new(false));
    let probe = Probe(dropped.clone());
    let mut handle = executor
        .spawn(async move {
            let _probe = probe;
            7
        })
        .unwrap();
    assert_eq!(executor.len(), 1);
    assert!(try_join(&mut handle).is_pending());
    assert_eq!(executor.run_ready(8), 1);
    assert!(dropped.get());
    assert!(handle.is_finished());
    assert_eq!(try_join(&mut handle).map(Result::unwrap), Poll::Ready(7));
    assert!(executor.is_empty());
}

#[test]
fn capacity_is_bounded_and_counts_permits() {
    let executor = LocalExecutor::new(2);
    let permit = executor.reserve().unwrap();
    executor
        .spawn_detached(std::future::pending::<()>())
        .unwrap();
    let error = executor.spawn(async {}).unwrap_err();
    assert_eq!(error.kind(), ErrorKind::Overloaded);
    assert_eq!(
        executor.reserve().unwrap_err().kind(),
        ErrorKind::Overloaded
    );
    assert_eq!((executor.len(), executor.reserved()), (1, 1));
    drop(permit);
    assert_eq!(executor.reserved(), 0);
    let permit = executor.reserve().unwrap();
    let mut handle = permit.spawn(async { 3 });
    assert_eq!((executor.len(), executor.reserved()), (2, 0));
    executor.run_ready(8);
    assert_eq!(try_join(&mut handle).map(Result::unwrap), Poll::Ready(3));
    assert_eq!(executor.len(), 1);

    let zero = LocalExecutor::new(0);
    assert_eq!(
        zero.spawn(async {}).unwrap_err().kind(),
        ErrorKind::Overloaded
    );
}

#[test]
fn idle_task_is_not_repolled_until_its_own_wake() {
    let executor = LocalExecutor::new(4);
    let polls = Rc::new(Cell::new(0));
    let saved = Rc::new(RefCell::new(None));
    executor
        .spawn_detached(sleeper(polls.clone(), saved.clone()))
        .unwrap();
    for _ in 0..100 {
        executor.run_ready(64);
    }
    assert_eq!(polls.get(), 1);
    assert!(!executor.has_ready());
    let waker = saved.borrow().clone().unwrap();
    waker.wake_by_ref();
    waker.wake_by_ref();
    waker.wake();
    assert!(executor.has_ready());
    assert_eq!(
        executor.run_ready(64),
        1,
        "repeated wakes queue the task once"
    );
    assert_eq!(polls.get(), 2);
    assert_eq!(executor.run_ready(64), 0);
}

#[test]
fn budget_limits_polls_and_turn_excludes_tasks_woken_during_it() {
    let executor = LocalExecutor::new(8);
    let order = Rc::new(RefCell::new(Vec::new()));
    for id in 0..3 {
        let order = order.clone();
        executor
            .spawn_detached(async move {
                loop {
                    order.borrow_mut().push(id);
                    yield_now().await;
                }
            })
            .unwrap();
    }
    assert_eq!(executor.run_ready(2), 2);
    assert_eq!(*order.borrow(), [0, 1]);
    // Task 2 was queued first; 0 and 1 re-queued behind it by yielding.
    assert_eq!(executor.run_ready(100), 3);
    assert_eq!(*order.borrow(), [0, 1, 2, 0, 1]);
    assert_eq!(executor.run_ready(0), 0);
    assert_eq!(executor.run_ready(100), 3);
    assert_eq!(*order.borrow(), [0, 1, 2, 0, 1, 2, 0, 1]);
}

#[test]
fn spawned_children_run_in_the_next_turn() {
    let executor = LocalExecutor::new(4);
    let spawner = executor.spawner();
    let ran = Rc::new(Cell::new(false));
    let flag = ran.clone();
    executor
        .spawn_detached(async move {
            spawner
                .spawn_detached(async move { flag.set(true) })
                .unwrap();
        })
        .unwrap();
    assert_eq!(executor.run_ready(10), 1);
    assert!(!ran.get());
    assert_eq!(executor.run_ready(10), 1);
    assert!(ran.get());
    assert!(executor.is_empty());
}

#[test]
fn poll_registers_owner_and_wakes_it_for_backlog_and_spawns() {
    let executor = LocalExecutor::new(4);
    executor.spawn_detached(async {}).unwrap();
    executor.spawn_detached(async {}).unwrap();
    let owner = Arc::new(WakeCount::default());
    let waker = Waker::from(owner.clone());
    let mut cx = Context::from_waker(&waker);
    assert_eq!(executor.poll(&mut cx, 1), 1);
    assert_eq!(owner.count(), 1, "backlog wakes the owner");
    // The owner waker was not consumed by the self-wake, only by a task wake.
    executor.spawn_detached(async {}).unwrap();
    assert_eq!(owner.count(), 2);
    executor.spawn_detached(async {}).unwrap();
    assert_eq!(
        owner.count(),
        2,
        "the owner waker is taken by the first wake"
    );
    assert_eq!(executor.poll(&mut cx, 8), 3);
    assert_eq!(owner.count(), 2);
    assert!(executor.is_empty());
    // A zero budget registers without self-waking.
    executor.spawn_detached(async {}).unwrap();
    assert_eq!(owner.count(), 3);
    assert_eq!(executor.poll(&mut cx, 0), 0);
    assert_eq!(owner.count(), 3);
}

#[test]
fn nested_poll_is_a_noop_and_keeps_the_outer_owner() {
    let executor = Rc::new(LocalExecutor::new(4));
    let nested = Arc::new(WakeCount::default());
    let nested_waker = Waker::from(nested.clone());
    let inner = Rc::downgrade(&executor);
    let results = Rc::new(RefCell::new(Vec::new()));
    let seen = results.clone();
    executor
        .spawn_detached(async move {
            let executor = inner.upgrade().unwrap();
            seen.borrow_mut()
                .push(executor.poll(&mut Context::from_waker(&nested_waker), 8));
            seen.borrow_mut().push(executor.run_ready(8));
        })
        .unwrap();
    let pending = Rc::new(RefCell::new(None));
    executor
        .spawn_detached(sleeper(Rc::new(Cell::new(0)), pending.clone()))
        .unwrap();
    let outer = Arc::new(WakeCount::default());
    let outer_waker = Waker::from(outer.clone());
    executor.poll(&mut Context::from_waker(&outer_waker), 8);
    assert_eq!(*results.borrow(), [0, 0]);
    pending.borrow().as_ref().unwrap().wake_by_ref();
    assert_eq!(outer.count(), 1);
    assert_eq!(nested.count(), 0);
}

#[test]
fn cross_thread_wake_queues_task_and_wakes_owner() {
    let executor = LocalExecutor::new(2);
    let polls = Rc::new(Cell::new(0));
    let saved = Rc::new(RefCell::new(None));
    executor
        .spawn_detached(sleeper(polls.clone(), saved.clone()))
        .unwrap();
    let first = Arc::new(WakeCount::default());
    let second = Arc::new(WakeCount::default());
    executor.poll(&mut Context::from_waker(&Waker::from(first.clone())), 8);
    // A later owner replaces the first even though the task is idle.
    executor.poll(&mut Context::from_waker(&Waker::from(second.clone())), 8);
    let waker = saved.borrow().clone().unwrap();
    std::thread::spawn(move || waker.wake()).join().unwrap();
    assert_eq!(first.count(), 0);
    assert_eq!(second.count(), 1);
    assert!(executor.has_ready());
    executor.run_ready(8);
    assert_eq!(polls.get(), 2);
}

/// Hand-written futures keep their state until the executor drops them. (An
/// `async` block drops its locals itself when it completes or unwinds, so it
/// cannot observe this ordering.)
struct Guarded<D> {
    guard: D,
    panic: bool,
}

impl<D: Unpin> Future for Guarded<D> {
    type Output = u8;
    fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<u8> {
        let _ = &self.guard;
        if self.panic {
            panic!("injected poll panic");
        }
        Poll::Ready(5)
    }
}

#[test]
fn panicking_task_is_removed_before_panic_resumes() {
    struct Replace(Rc<LocalExecutor>);
    impl Drop for Replace {
        fn drop(&mut self) {
            assert_eq!(self.0.len(), 0, "capacity is released before drop");
            self.0.spawn_detached(async {}).unwrap();
        }
    }
    let executor = Rc::new(LocalExecutor::new(1));
    let mut handle = executor
        .spawn(Guarded {
            guard: Replace(executor.clone()),
            panic: true,
        })
        .unwrap();
    let panic = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| executor.run_ready(8)));
    assert!(panic.is_err());
    let Poll::Ready(Err(error)) = try_join(&mut handle) else {
        panic!("handle must report the panic");
    };
    assert_eq!(error.kind(), ErrorKind::Unavailable);
    assert_eq!(error.context(), Some("task panicked"));
    // The replacement spawned by the destructor runs normally.
    assert_eq!(executor.len(), 1);
    assert_eq!(executor.run_ready(8), 1);
    assert!(executor.is_empty());
}

#[test]
fn panicking_destructor_of_completed_task_does_not_leak_capacity() {
    struct PanicsOnDrop(Rc<LocalExecutor>);
    impl Drop for PanicsOnDrop {
        fn drop(&mut self) {
            assert!(self.0.is_empty());
            panic!("injected destructor panic");
        }
    }
    let executor = Rc::new(LocalExecutor::new(1));
    let mut handle = executor
        .spawn(Guarded {
            guard: PanicsOnDrop(executor.clone()),
            panic: false,
        })
        .unwrap();
    let panic = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| executor.run_ready(1)));
    let message = panic.unwrap_err();
    assert_eq!(
        message.downcast_ref::<&str>(),
        Some(&"injected destructor panic")
    );
    assert!(executor.is_empty());
    assert!(executor.reserve().is_ok());
    // The output is still delivered: the future finished before its drop panicked.
    assert_eq!(try_join(&mut handle).map(Result::unwrap), Poll::Ready(5));
}

#[test]
fn capacity_is_released_before_completed_future_drop() {
    struct Check(Rc<LocalExecutor>);
    impl Drop for Check {
        fn drop(&mut self) {
            assert_eq!(self.0.reserved(), 1023);
            assert_eq!(self.0.len(), 0);
            drop(self.0.reserve().unwrap());
        }
    }
    let executor = Rc::new(LocalExecutor::new(1024));
    let permits: Vec<_> = (0..1023).map(|_| executor.reserve().unwrap()).collect();
    executor
        .spawn_detached(Guarded {
            guard: Check(executor.clone()),
            panic: false,
        })
        .unwrap();
    assert_eq!(executor.run_ready(1), 1);
    drop(permits);
    assert_eq!(executor.reserved(), 0);
}

#[test]
fn abort_drops_idle_task_and_resolves_handle() {
    let executor = LocalExecutor::new(2);
    let dropped = Rc::new(Cell::new(false));
    struct Probe(Rc<Cell<bool>>);
    impl Drop for Probe {
        fn drop(&mut self) {
            self.0.set(true);
        }
    }
    let probe = Probe(dropped.clone());
    let mut handle = executor
        .spawn(async move {
            let _probe = probe;
            std::future::pending::<u8>().await
        })
        .unwrap();
    executor.run_ready(8);
    handle.abort();
    assert!(dropped.get());
    assert!(executor.is_empty());
    let Poll::Ready(Err(error)) = try_join(&mut handle) else {
        panic!("aborted handle must be ready");
    };
    assert!(error.is_canceled());
    // Aborting before the first poll never polls the task.
    let polls = Rc::new(Cell::new(0));
    let handle = executor
        .spawn(sleeper(polls.clone(), Rc::new(RefCell::new(None))))
        .unwrap();
    handle.abort();
    handle.abort();
    assert_eq!(executor.run_ready(8), 0);
    assert_eq!(polls.get(), 0);
    assert!(!executor.has_ready());
}

#[test]
fn self_abort_drops_task_after_its_poll_returns() {
    let executor = LocalExecutor::new(2);
    let handle_slot: Rc<RefCell<Option<JoinHandle<()>>>> = Rc::new(RefCell::new(None));
    let slot = handle_slot.clone();
    let polls = Rc::new(Cell::new(0));
    let counter = polls.clone();
    let handle = executor
        .spawn(std::future::poll_fn(move |cx| {
            counter.set(counter.get() + 1);
            slot.borrow().as_ref().unwrap().abort();
            cx.waker().wake_by_ref();
            Poll::Pending
        }))
        .unwrap();
    // Store the handle in the task's own state; aborting from inside drops the
    // task (and with it the closure's `slot` clone) after the poll.
    *handle_slot.borrow_mut() = Some(handle);
    executor.run_ready(8);
    assert_eq!(polls.get(), 1);
    assert!(executor.is_empty());
    executor.run_ready(8);
    assert_eq!(polls.get(), 1);
    let mut handle = handle_slot.borrow_mut().take().unwrap();
    assert!(matches!(try_join(&mut handle), Poll::Ready(Err(e)) if e.is_canceled()));
}

#[test]
fn dropping_handle_detaches_task() {
    let executor = LocalExecutor::new(1);
    let ran = Rc::new(Cell::new(false));
    let flag = ran.clone();
    let handle = executor
        .spawn(async move {
            yield_now().await;
            flag.set(true);
        })
        .unwrap();
    drop(handle);
    executor.run_ready(8);
    executor.run_ready(8);
    assert!(ran.get());
    assert!(executor.is_empty());
}

#[test]
fn join_handle_wakes_awaiting_task() {
    let executor = LocalExecutor::new(4);
    let spawner = executor.spawner();
    let mut outer = executor
        .spawn(async move {
            let child = spawner
                .spawn(async {
                    yield_now().await;
                    yield_now().await;
                    11
                })
                .unwrap();
            child.await.unwrap() * 2
        })
        .unwrap();
    let mut turns = 0;
    while !outer.is_finished() {
        assert!(executor.run_ready(8) > 0, "join wakes must keep progress");
        turns += 1;
    }
    assert!(turns <= 5);
    assert_eq!(try_join(&mut outer).map(Result::unwrap), Poll::Ready(22));
}

#[test]
fn abort_all_releases_tasks_but_keeps_permits_and_survivors() {
    let executor = Rc::new(LocalExecutor::new(4));
    let permit = executor.reserve().unwrap();
    struct Respawn(Rc<LocalExecutor>);
    impl Drop for Respawn {
        fn drop(&mut self) {
            self.0.spawn_detached(async {}).unwrap();
        }
    }
    let respawn = Respawn(executor.clone());
    let mut handle = executor
        .spawn(async move {
            let _respawn = respawn;
            std::future::pending::<()>().await
        })
        .unwrap();
    executor
        .spawn_detached(std::future::pending::<()>())
        .unwrap();
    executor.run_ready(8);
    executor.abort_all();
    assert!(matches!(try_join(&mut handle), Poll::Ready(Err(e)) if e.is_canceled()));
    assert_eq!(executor.reserved(), 1);
    assert_eq!(executor.len(), 1, "the destructor's spawn survives");
    assert_eq!(executor.run_ready(8), 1);
    drop(permit);
    assert!(executor.is_empty());
}

#[test]
fn dropping_executor_aborts_tasks_and_disables_spawners() {
    let executor = LocalExecutor::new(4);
    let spawner = executor.spawner();
    let permit = executor.reserve().unwrap();
    let mut handle = executor.spawn(std::future::pending::<()>()).unwrap();
    drop(executor);
    assert!(!spawner.is_alive());
    assert!(matches!(try_join(&mut handle), Poll::Ready(Err(e)) if e.is_canceled()));
    assert_eq!(
        spawner.spawn(async {}).unwrap_err().kind(),
        ErrorKind::Unavailable
    );
    let mut late = permit.spawn(async { 1 });
    assert!(
        matches!(try_join(&mut late), Poll::Ready(Err(e)) if e.kind() == ErrorKind::Unavailable)
    );
}

#[test]
fn task_wakes_after_executor_drop_are_ignored() {
    let executor = LocalExecutor::new(1);
    let saved = Rc::new(RefCell::new(None));
    executor
        .spawn_detached(sleeper(Rc::new(Cell::new(0)), saved.clone()))
        .unwrap();
    executor.run_ready(1);
    let waker = saved.borrow_mut().take().unwrap();
    drop(executor);
    waker.wake();
}

/// Waker callbacks (clone, wake, drop) that re-enter the executor must find
/// no internal borrow held. Port of the old owner-refresh re-entrancy test.
#[test]
fn waker_callbacks_may_reenter_the_executor() {
    thread_local! {
        static EXECUTOR: RefCell<Option<Rc<LocalExecutor>>> = const { RefCell::new(None) };
        static CALLS: Cell<usize> = const { Cell::new(0) };
    }
    fn reenter() {
        CALLS.with(|calls| calls.set(calls.get() + 1));
        EXECUTOR.with(|slot| {
            let Some(executor) = slot.borrow().clone() else {
                return;
            };
            let _ = executor.len();
            let _ = executor.has_ready();
            drop(executor.reserve());
            executor.run_ready(1);
            executor.poll(&mut Context::from_waker(Waker::noop()), 0);
        });
    }
    unsafe fn clone(_: *const ()) -> RawWaker {
        reenter();
        RawWaker::new(std::ptr::null(), &VTABLE)
    }
    unsafe fn callback(_: *const ()) {
        reenter();
    }
    static VTABLE: RawWakerVTable = RawWakerVTable::new(clone, callback, callback, callback);

    let executor = Rc::new(LocalExecutor::new(4));
    EXECUTOR.with(|slot| slot.replace(Some(executor.clone())));
    for _ in 0..2 {
        executor
            .spawn_detached(std::future::poll_fn(|cx| {
                cx.waker().wake_by_ref();
                Poll::<()>::Pending
            }))
            .unwrap();
    }
    // SAFETY: the vtable ignores its data pointer and owns no allocation.
    let waker = unsafe { Waker::from_raw(RawWaker::new(std::ptr::null(), &VTABLE)) };
    let mut cx = Context::from_waker(&waker);
    executor.poll(&mut cx, 1);
    executor.poll(&mut cx, 0);
    // Replacing the owner drops the old raw waker; spawning wakes the owner.
    executor.poll(&mut noop_cx(), 1);
    executor.poll(&mut cx, 0);
    executor.spawn_detached(async {}).unwrap();
    drop(waker);
    assert!(CALLS.with(Cell::get) > 0);
    EXECUTOR.with(|slot| slot.take());
}

/// A join waker that re-enters on wake and drop.
#[test]
fn join_waker_callbacks_may_reenter() {
    thread_local! {
        static EXECUTOR: RefCell<Option<Rc<LocalExecutor>>> = const { RefCell::new(None) };
    }
    struct Reenter;
    impl Wake for Reenter {
        fn wake(self: Arc<Self>) {
            EXECUTOR.with(|slot| {
                let executor = slot.borrow().clone().unwrap();
                executor.spawn_detached(async {}).unwrap();
            });
        }
    }
    let executor = Rc::new(LocalExecutor::new(4));
    EXECUTOR.with(|slot| slot.replace(Some(executor.clone())));
    let mut handle = executor.spawn(async { 1 }).unwrap();
    let waker = Waker::from(Arc::new(Reenter));
    assert!(
        Pin::new(&mut handle)
            .poll(&mut Context::from_waker(&waker))
            .is_pending()
    );
    executor.run_ready(1);
    assert_eq!(executor.len(), 1, "the join waker spawned a task");
    assert_eq!(try_join(&mut handle).map(Result::unwrap), Poll::Ready(1));
    EXECUTOR.with(|slot| slot.take());
}

#[test]
fn yield_is_pending_once_and_wakes_once() {
    let owner = Arc::new(WakeCount::default());
    let waker = Waker::from(owner.clone());
    let mut cx = Context::from_waker(&waker);
    let mut yielded = yield_now();
    assert!(Pin::new(&mut yielded).poll(&mut cx).is_pending());
    assert_eq!(owner.count(), 1);
    assert!(Pin::new(&mut yielded).poll(&mut cx).is_ready());
    assert_eq!(owner.count(), 1);
}

#[test]
fn stale_wake_does_not_reach_reused_slot() {
    let executor = LocalExecutor::new(1);
    let saved = Rc::new(RefCell::new(None));
    let handle = executor
        .spawn(sleeper(Rc::new(Cell::new(0)), saved.clone()))
        .unwrap();
    executor.run_ready(1);
    handle.abort();
    let stale = saved.borrow_mut().take().unwrap();
    let polls = Rc::new(Cell::new(0));
    executor
        .spawn_detached(sleeper(polls.clone(), Rc::new(RefCell::new(None))))
        .unwrap();
    executor.run_ready(1);
    assert_eq!(polls.get(), 1);
    stale.wake();
    assert_eq!(executor.run_ready(1), 0);
    assert_eq!(polls.get(), 1);
}
