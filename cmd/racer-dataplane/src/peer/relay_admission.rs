//! Bounded FIFO admission for already authenticated relay requests.
use crate::{
    error::{Error, Operation},
    model::limits::ResourceClass,
    runtime::{
        admission::{Admission, Reservation},
        deadline::RequestScope,
    },
};
use std::{
    cell::RefCell,
    rc::{Rc, Weak},
    task::{Poll, Waker},
};

type Queue = Rc<RefCell<Vec<Weak<Waiter>>>>;

struct Waiter {
    scope: RequestScope,
    wake: RefCell<Option<Waker>>,
    queue: Weak<RefCell<Vec<Weak<Waiter>>>>,
    _count: Reservation,
    _context: Reservation,
}

fn wake(queue: &Queue) {
    let wakes: Vec<_> = queue
        .borrow()
        .iter()
        .filter_map(Weak::upgrade)
        .filter_map(|waiter| waiter.wake.borrow().clone())
        .collect();
    for wake in wakes {
        wake.wake();
    }
}

impl Drop for Waiter {
    fn drop(&mut self) {
        if let Some(queue) = self.queue.upgrade() {
            wake(&queue);
        }
    }
}

pub(super) struct Permit {
    charge: Option<Reservation>,
    queue: Queue,
}

impl Drop for Permit {
    fn drop(&mut self) {
        drop(self.charge.take());
        wake(&self.queue);
    }
}

pub(super) struct RelayAdmission {
    admission: Rc<Admission>,
    queue: Queue,
    #[cfg(test)]
    pub(super) waits: std::cell::Cell<usize>,
}

impl RelayAdmission {
    pub(super) fn new(admission: Rc<Admission>) -> Self {
        Self {
            admission,
            queue: Rc::new(RefCell::new(Vec::new())),
            #[cfg(test)]
            waits: std::cell::Cell::new(0),
        }
    }

    /// The owning worker calls this on its existing bounded timer tick. A queued
    /// request owns no kernel operation whose deadline could otherwise wake it.
    pub(super) fn poll_deadlines(&self) {
        let wakes: Vec<_> = self
            .queue
            .borrow()
            .iter()
            .filter_map(Weak::upgrade)
            .filter(|waiter| {
                waiter.scope.check().is_err()
                    || self.admission.is_stopped()
                    || self.admission.used(ResourceClass::Relay)
                        < self.admission.limit(ResourceClass::Relay)
            })
            .filter_map(|waiter| waiter.wake.borrow().clone())
            .collect();
        for wake in wakes {
            wake.wake();
        }
    }

    pub(super) fn acquire<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, Permit> {
        Box::pin(async move {
            scope.check()?;
            let cancellation = scope.cancellation.subscribe()?;
            let mut waiter: Option<Rc<Waiter>> = None;
            std::future::poll_fn(|cx| {
                cancellation.register(cx.waker());
                scope.check()?;
                if self.admission.is_stopped() {
                    return Poll::Ready(Err(Error::Unavailable));
                }
                let first = {
                    let mut queue = self.queue.borrow_mut();
                    queue.retain(|entry| entry.strong_count() != 0);
                    queue.first().and_then(Weak::upgrade)
                };
                let turn = first
                    .as_ref()
                    .is_none_or(|first| waiter.as_ref().is_some_and(|me| Rc::ptr_eq(first, me)));
                if turn {
                    match self.admission.reserve(None, ResourceClass::Relay, 1) {
                        Ok(charge) => {
                            return Poll::Ready(Ok(Permit {
                                charge: Some(charge),
                                queue: self.queue.clone(),
                            }));
                        }
                        Err(Error::Overloaded) => {}
                        Err(error) => return Poll::Ready(Err(error)),
                    }
                }
                if waiter.is_none() {
                    if self.queue.borrow().len() >= self.admission.limits().queue_entries.get() {
                        return Poll::Ready(Err(Error::Overloaded));
                    }
                    let count = self.admission.reserve(None, ResourceClass::Waiter, 1)?;
                    let context = self.admission.reserve(
                        None,
                        ResourceClass::RequestContext,
                        std::mem::size_of::<Waiter>(),
                    )?;
                    let entry = Rc::new(Waiter {
                        scope: scope.clone(),
                        wake: RefCell::new(None),
                        queue: Rc::downgrade(&self.queue),
                        _count: count,
                        _context: context,
                    });
                    self.queue.borrow_mut().push(Rc::downgrade(&entry));
                    waiter = Some(entry);
                    #[cfg(test)]
                    self.waits.set(self.waits.get() + 1);
                }
                *waiter.as_ref().unwrap().wake.borrow_mut() = Some(cx.waker().clone());
                Poll::Pending
            })
            .await
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::identity::RequestId;
    use std::{
        num::NonZeroUsize,
        sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        },
        task::{Context, Wake},
        time::{Duration, Instant},
    };

    struct Wakes(AtomicUsize);
    impl Wake for Wakes {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }
    fn fixture() -> (RelayAdmission, RequestScope) {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.relay_transfers = NonZeroUsize::new(1).unwrap();
        limits.queue_entries = NonZeroUsize::new(2).unwrap();
        (
            RelayAdmission::new(Rc::new(Admission::new(limits))),
            RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(5)).unwrap(),
        )
    }
    #[test]
    fn release_wakes_fifo_and_abandonment_releases_waiter_quota() {
        let (gate, scope) = fixture();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let held = futures::executor::block_on(gate.acquire(&scope)).unwrap();
        let count = Arc::new(Wakes(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        let mut first = gate.acquire(&scope);
        assert!(
            first
                .as_mut()
                .poll(&mut Context::from_waker(&waker))
                .is_pending()
        );
        assert_eq!(count.0.load(Ordering::Relaxed), 0, "waiting must not spin");
        let mut second = gate.acquire(&scope);
        assert!(second.as_mut().poll(&mut cx).is_pending());
        assert_eq!(gate.admission.used(ResourceClass::Waiter), 2);
        assert!(gate.admission.used(ResourceClass::RequestContext) > 0);
        let mut excess = gate.acquire(&scope);
        assert!(matches!(
            excess.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        drop(held);
        assert!(count.0.load(Ordering::Relaxed) > 0);
        assert!(
            second.as_mut().poll(&mut cx).is_pending(),
            "later arrival bypassed FIFO"
        );
        drop(first);
        let permit = match second.as_mut().poll(&mut cx) {
            Poll::Ready(Ok(permit)) => permit,
            _ => panic!("abandoned head blocked progress"),
        };
        assert_eq!(gate.admission.used(ResourceClass::Relay), 1);
        assert_eq!(gate.admission.used(ResourceClass::Waiter), 0);
        assert_eq!(gate.admission.used(ResourceClass::RequestContext), 0);
        drop(permit);
        assert_eq!(gate.admission.used(ResourceClass::Relay), 0);
    }
    #[test]
    fn cancellation_deadline_and_stop_wake_and_drain_without_extra_relay_slots() {
        for event in ["cancel", "deadline", "stop", "drop"] {
            let (gate, mut scope) = fixture();
            let held = futures::executor::block_on(gate.acquire(&scope)).unwrap();
            if event == "deadline" {
                scope.deadline.0 = Instant::now() + Duration::from_millis(20);
            }
            let count = Arc::new(Wakes(AtomicUsize::new(0)));
            let waker = Waker::from(count.clone());
            let mut cx = Context::from_waker(&waker);
            let mut pending = gate.acquire(&scope);
            assert!(pending.as_mut().poll(&mut cx).is_pending());
            match event {
                "cancel" => scope.cancel().unwrap(),
                "deadline" => {
                    std::thread::sleep(Duration::from_millis(25));
                    gate.poll_deadlines();
                }
                "stop" => {
                    gate.admission.stop();
                    gate.poll_deadlines();
                }
                _ => {}
            }
            if event != "drop" {
                assert!(
                    count.0.load(Ordering::Relaxed) > 0,
                    "{event} did not wake the sleeping task"
                );
                let expected = match event {
                    "cancel" => Error::Cancelled,
                    "deadline" => Error::DeadlineExceeded,
                    _ => Error::Unavailable,
                };
                assert!(
                    matches!(pending.as_mut().poll(&mut cx), Poll::Ready(Err(error)) if error == expected)
                );
            }
            drop(pending);
            assert_eq!(gate.admission.used(ResourceClass::Relay), 1);
            assert_eq!(gate.admission.used(ResourceClass::Waiter), 0);
            assert_eq!(gate.admission.used(ResourceClass::RequestContext), 0);
            drop(held);
            assert_eq!(gate.admission.used(ResourceClass::Relay), 0);
        }
    }
    #[test]
    fn context_exhaustion_rolls_back_waiter_charge_before_enqueue() {
        let (gate, scope) = fixture();
        let held = futures::executor::block_on(gate.acquire(&scope)).unwrap();
        let context = gate
            .admission
            .reserve(
                None,
                ResourceClass::RequestContext,
                gate.admission.limit(ResourceClass::RequestContext),
            )
            .unwrap();
        let mut pending = gate.acquire(&scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(matches!(
            pending.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert_eq!(gate.admission.used(ResourceClass::Waiter), 0);
        assert!(gate.queue.borrow().is_empty());
        drop(context);
        drop(held);
        assert_eq!(gate.admission.used(ResourceClass::RequestContext), 0);
        assert_eq!(gate.admission.used(ResourceClass::Relay), 0);
    }
}
