//! Worker-owned acquisition futures survive ingress future cancellation. The worker
//! polls this bounded queue alongside reactor completions until shutdown is fenced.
use crate::error::{Error, Operation, Result};
use std::{
    cell::{Cell, RefCell},
    collections::VecDeque,
    rc::Rc,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll, Waker},
};

thread_local! {
    // Only selection is thread-local. Workers own the actual queues and permits
    // retain their original owner across nested scopes and deferred submission.
    static CURRENT: RefCell<Option<Rc<DriverQueue>>> = const { RefCell::new(None) };
}
const MAX_DRIVERS: usize = 1024;

#[derive(Default)]
pub struct DriverQueue {
    drivers: RefCell<VecDeque<Driver>>,
    new: RefCell<Vec<Operation<'static, ()>>>,
    count: Cell<usize>,
    owner: RefCell<Option<Waker>>,
}
struct Driver {
    operation: Operation<'static, ()>,
    wake: Arc<Runnable>,
}
pub(crate) struct Runnable {
    ready: AtomicBool,
    owner: futures::task::AtomicWaker,
}
impl Runnable {
    pub(crate) fn new() -> Arc<Self> {
        Arc::new(Self {
            ready: AtomicBool::new(true),
            owner: futures::task::AtomicWaker::new(),
        })
    }
    pub(crate) fn poll<T>(
        self: &Arc<Self>,
        future: std::pin::Pin<&mut impl std::future::Future<Output = T>>,
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

pub struct QueueGuard {
    previous: Option<Rc<DriverQueue>>,
}

impl Drop for QueueGuard {
    fn drop(&mut self) {
        CURRENT.with(|current| current.replace(self.previous.take()));
    }
}

impl DriverQueue {
    /// Discard simulated process tasks without polling acquisition or write work.
    #[cfg(test)]
    pub(crate) fn simulation_crash(&self) {
        let drivers = std::mem::take(&mut *self.drivers.borrow_mut());
        let new = std::mem::take(&mut *self.new.borrow_mut());
        self.count.set(self.count.get() - drivers.len() - new.len());
        drop((drivers, new));
    }
    /// Select this queue only during poll/drop, never across an async suspension.
    pub fn scope<F: std::future::Future>(self: &Rc<Self>, future: F) -> Queued<F> {
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
        let _queue = self.enter();
        *self.owner.borrow_mut() = Some(cx.waker().clone());
        // Child operations use a separate inbox. Never recursively poll futures
        // already borrowed by this worker's outer polling turn.
        let Ok(mut drivers) = self.drivers.try_borrow_mut() else {
            return;
        };
        drivers.extend(self.new.borrow_mut().drain(..).map(|operation| Driver {
            operation,
            wake: Runnable::new(),
        }));
        for _ in 0..budget.min(drivers.len()) {
            let Some(mut driver) = drivers.pop_front() else {
                break;
            };
            match driver
                .wake
                .poll(std::pin::Pin::new(&mut driver.operation), cx, false)
            {
                Poll::Ready(_) => self.count.set(self.count.get() - 1),
                Poll::Pending => drivers.push_back(driver),
            }
        }
    }
}

pub struct Queued<F> {
    queue: Rc<DriverQueue>,
    future: Option<std::pin::Pin<Box<F>>>,
}

impl<F: std::future::Future> std::future::Future for Queued<F> {
    type Output = F::Output;
    fn poll(self: std::pin::Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
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

pub struct Permit {
    queue: Rc<DriverQueue>,
    reserved: bool,
}
impl Permit {
    pub fn submit(mut self, driver: Operation<'static, ()>) {
        self.queue.new.borrow_mut().push(driver);
        self.reserved = false;
        let waker = self.queue.owner.borrow().clone();
        if let Some(waker) = waker {
            waker.wake();
        }
    }
}
impl Drop for Permit {
    fn drop(&mut self) {
        if self.reserved {
            self.queue.count.set(self.queue.count.get() - 1);
        }
    }
}
pub fn reserve() -> Result<Permit> {
    let queue = current().ok_or(Error::InvalidConfiguration)?;
    if queue.count.get() >= MAX_DRIVERS {
        return Err(Error::Overloaded);
    }
    queue.count.set(queue.count.get() + 1);
    Ok(Permit {
        queue,
        reserved: true,
    })
}

pub fn spawn(driver: Operation<'static, ()>) -> Result<()> {
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
    use std::{cell::Cell, rc::Rc};
    #[test]
    fn blocked_driver_is_not_repolled_until_its_own_wake() {
        let queue = Rc::new(DriverQueue::default());
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
        let a = Rc::new(DriverQueue::default());
        let b = Rc::new(DriverQueue::default());
        let _a = a.enter();
        let permit = reserve().unwrap();
        let completed = Rc::new(Cell::new(false));
        let child = completed.clone();
        {
            let _b = b.enter();
            permit.submit(Box::pin(async move {
                spawn(Box::pin(async move {
                    child.set(true);
                    Ok(())
                }))?;
                Ok(())
            }));
            let permits: Vec<_> = (0..MAX_DRIVERS).map(|_| reserve().unwrap()).collect();
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
        let queue = Rc::new(DriverQueue::default());
        let _owner = queue.enter();
        let complete = Rc::new(Cell::new(false));
        let observed = complete.clone();
        let (completion, fence) = oneshot::channel::<()>();
        let (reply, reader) = oneshot::channel::<()>();
        spawn(Box::pin(async move {
            fence.await.map_err(|_| Error::Io)?;
            observed.set(true);
            let _ = reply.send(());
            Ok(())
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
        let queue = Rc::new(DriverQueue::default());
        let _owner = queue.enter();
        let (send, mut result) = oneshot::channel();
        spawn(Box::pin(async move {
            let (child, receive) = oneshot::channel();
            spawn(Box::pin(async move {
                child.send(7).map_err(|_| Error::Cancelled)?;
                Ok(())
            }))?;
            let value = receive.await.map_err(|_| Error::Unavailable)?;
            let _ = send.send(value);
            Ok(())
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
        let queue = Rc::new(DriverQueue::default());
        {
            let _owner = queue.enter();
            let permit = reserve().unwrap();
            assert_eq!(queue.pending(), 1);
            drop(permit);
            assert_eq!(queue.pending(), 0);
        }
        assert!(matches!(reserve(), Err(Error::InvalidConfiguration)));
    }
}
