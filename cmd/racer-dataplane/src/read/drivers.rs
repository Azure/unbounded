//! Worker-owned acquisition futures survive ingress future cancellation. The worker
//! polls this bounded queue alongside reactor completions until shutdown is fenced.
use crate::error::{Error, Operation, Result};
use std::{
    cell::RefCell,
    collections::VecDeque,
    task::{Context, Poll},
};

thread_local! {
    static DRIVERS: RefCell<VecDeque<Operation<'static, ()>>> = RefCell::new(VecDeque::new());
    static NEW: RefCell<Vec<Operation<'static, ()>>> = RefCell::new(Vec::new());
    static COUNT: std::cell::Cell<usize> = const { std::cell::Cell::new(0) };
    static OWNER: RefCell<Option<std::task::Waker>> = const { RefCell::new(None) };
}
const MAX_DRIVERS: usize = 1024;

pub struct Permit(bool);
impl Permit {
    pub fn submit(mut self, driver: Operation<'static, ()>) {
        NEW.with(|new| new.borrow_mut().push(driver));
        self.0 = false;
        OWNER.with(|owner| {
            if let Some(waker) = owner.borrow().as_ref() {
                waker.wake_by_ref();
            }
        });
    }
}
impl Drop for Permit {
    fn drop(&mut self) {
        if self.0 {
            COUNT.with(|count| count.set(count.get() - 1));
        }
    }
}
pub fn reserve() -> Result<Permit> {
    COUNT.with(|count| {
        if count.get() >= MAX_DRIVERS {
            return Err(Error::Overloaded);
        }
        count.set(count.get() + 1);
        Ok(Permit(true))
    })
}

pub fn spawn(driver: Operation<'static, ()>) -> Result<()> {
    reserve()?.submit(driver);
    Ok(())
}

pub fn poll(cx: &mut Context<'_>, budget: usize) {
    OWNER.with(|owner| *owner.borrow_mut() = Some(cx.waker().clone()));
    // Futures may enqueue another owner operation while being polled. Keep the
    // inbound queue separate, and do not recursively poll a worker queue.
    DRIVERS.with(|drivers| {
        let Ok(mut drivers) = drivers.try_borrow_mut() else {
            return;
        };
        NEW.with(|new| {
            for driver in new.borrow_mut().drain(..) {
                drivers.push_back(driver);
            }
        });
        for _ in 0..budget.min(drivers.len()) {
            let Some(mut driver) = drivers.pop_front() else {
                break;
            };
            match driver.as_mut().poll(cx) {
                Poll::Ready(_) => COUNT.with(|count| count.set(count.get() - 1)),
                Poll::Pending => drivers.push_back(driver),
            }
        }
    });
}

pub fn pending() -> usize {
    COUNT.with(|count| count.get())
}

#[cfg(test)]
mod tests {
    use super::*;
    use futures::{channel::oneshot, task::noop_waker};
    use std::{cell::Cell, rc::Rc};
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
