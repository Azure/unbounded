//! Worker-owned acquisition futures survive ingress future cancellation. The worker
//! polls this bounded queue alongside reactor completions until shutdown is fenced.
use crate::error::{Error, Operation, Result};
use std::{
    cell::{Cell, RefCell},
    collections::VecDeque,
    rc::Rc,
    task::{Context, Poll, Waker},
};

thread_local! {
    // Only selection is thread-local. Workers own the actual queues and permits
    // retain their original owner across nested scopes and deferred submission.
    static CURRENT: RefCell<Option<Rc<DriverQueue>>> = const { RefCell::new(None) };
    // Standalone component tests historically drive without a worker graph.
    #[cfg(test)]
    static STANDALONE: Rc<DriverQueue> = Rc::new(DriverQueue::default());
}
const MAX_DRIVERS: usize = 1024;

#[derive(Default)]
pub struct DriverQueue {
    drivers: RefCell<VecDeque<Operation<'static, ()>>>,
    new: RefCell<Vec<Operation<'static, ()>>>,
    count: Cell<usize>,
    owner: RefCell<Option<Waker>>,
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
        drivers.extend(self.new.borrow_mut().drain(..));
        for _ in 0..budget.min(drivers.len()) {
            let Some(mut driver) = drivers.pop_front() else {
                break;
            };
            match driver.as_mut().poll(cx) {
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
    let queue = CURRENT.with(|current| current.borrow().clone());
    #[cfg(test)]
    let queue = queue.or_else(|| Some(STANDALONE.with(Rc::clone)));
    queue
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
}
