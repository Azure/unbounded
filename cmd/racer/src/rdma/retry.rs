//! Deadline alarms for native waits, using the serving worker's reactor.
//!
//! Owner ops wake their waiting task themselves, across threads, when the
//! owner completes them, so a wait needs no periodic retry. The only alarm
//! is the scope deadline: a stalled op must still fail at that deadline.

use super::*;
use std::future::Future;
#[cfg(test)]
use std::time::Duration;

/// Timer backend shared by native handles, never by individual pending alarms.
pub(super) struct NativeRetry {
    reactor: Option<Rc<crate::runtime::Reactor>>,

    #[cfg(any(test, feature = "dst"))]
    alarms: RefCell<Vec<futures::channel::oneshot::Sender<()>>>,

    #[cfg(any(test, feature = "dst"))]
    immediate: Cell<bool>,
}

impl NativeRetry {
    /// Bind production waits to the existing worker reactor.
    pub(super) fn new(reactor: Rc<crate::runtime::Reactor>) -> Rc<Self> {
        Rc::new(Self {
            reactor: Some(reactor),
            #[cfg(any(test, feature = "dst"))]
            alarms: RefCell::new(Vec::new()),
            #[cfg(any(test, feature = "dst"))]
            immediate: Cell::new(false),
        })
    }

    /// Unattached devices cannot start native work without a retry backend.
    pub(super) fn unattached() -> Rc<Self> {
        Rc::new(Self {
            reactor: None,
            #[cfg(any(test, feature = "dst"))]
            alarms: RefCell::new(Vec::new()),
            #[cfg(any(test, feature = "dst"))]
            immediate: Cell::new(false),
        })
    }

    /// Fire at the scope deadline.
    fn alarm<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            #[cfg(any(test, feature = "dst"))]
            if self.immediate.replace(false) {
                return Ok(());
            }
            let until = scope.deadline.0;
            if let Some(reactor) = &self.reactor {
                reactor.sleep_until(until, scope).await
            } else {
                #[cfg(any(test, feature = "dst"))]
                {
                    let (send, receive) = futures::channel::oneshot::channel();
                    self.alarms.borrow_mut().push(send);
                    receive.await.map_err(|_| Error::Unavailable)
                }
                #[cfg(not(any(test, feature = "dst")))]
                Err(Error::Unavailable)
            }
        })
    }

    /// Keep one deadline alarm across unrelated wakes; native success never
    /// allocates one.
    pub(super) async fn wait<T, E: Into<Error>>(
        &self,
        scope: &RequestScope,
        operation: impl Future<Output = std::result::Result<T, E>>,
    ) -> Result<T> {
        let mut operation = std::pin::pin!(operation);
        let cancellation = scope.cancellation.subscribe()?;
        let mut alarm = None;
        poll_fn(|cx| {
            cancellation.register(cx.waker());
            scope.check()?;
            if let Poll::Ready(result) = operation.as_mut().poll(cx) {
                return Poll::Ready(result.map_err(Into::into));
            }
            if let Some(timer) = alarm.as_mut() {
                let timer: &mut Operation<'_, ()> = timer;
                match timer.as_mut().poll(cx) {
                    Poll::Pending => return Poll::Pending,
                    Poll::Ready(result) => result?,
                }
            }
            alarm = Some(self.alarm(scope));
            // Preemption can consume the positive interval before submission.
            // Recheck policy and yield once for this elapsed alarm, never loop.
            match alarm.as_mut().unwrap().as_mut().poll(cx) {
                Poll::Pending => Poll::Pending,
                Poll::Ready(Err(error)) => Poll::Ready(Err(error)),
                Poll::Ready(Ok(())) => {
                    scope.check()?;
                    alarm = None;
                    cx.waker().wake_by_ref();
                    Poll::Pending
                }
            }
        })
        .await
    }

    /// Fire only registered alarms; this is not a blanket executor wake.
    #[cfg(test)]
    pub(super) fn fire(&self) {
        for alarm in self.alarms.take() {
            let _ = alarm.send(());
        }
    }
}

#[cfg(test)]
mod tests {
    //! Explicit one-shot alarm tests, without cooperative self-wakes.
    use super::*;
    use std::{pin::Pin, task::Context};

    fn scope(env: &racer_io::Env) -> RequestScope {
        RequestScope::on(
            &env.ctx.clock,
            crate::model::RequestId([7; 16]),
            env.ctx.now() + Duration::from_secs(30),
        )
        .unwrap()
    }

    #[test]
    fn immediate_results_never_arm_an_alarm() {
        let env = racer_io::Env::host();
        let retry = NativeRetry::unattached();
        let scope = scope(&env);
        for result in [Ok(7), Err(Error::Overloaded), Err(Error::Io)] {
            let mut operation = Box::pin(retry.wait(&scope, std::future::ready(result)));
            assert_eq!(
                operation
                    .as_mut()
                    .poll(&mut Context::from_waker(std::task::Waker::noop())),
                Poll::Ready(result)
            );
        }
        assert!(retry.alarms.borrow().is_empty());
    }

    #[test]
    fn elapsed_alarm_yields_once_then_arms_without_failing_request() {
        let env = racer_io::Env::host();
        let retry = NativeRetry::unattached();
        retry.immediate.set(true);
        let scope = scope(&env);
        let wakes = std::sync::Arc::new(crate::test_support::WakeCounter::default());
        let waker = std::task::Waker::from(wakes.clone());
        let mut cx = Context::from_waker(&waker);
        let mut operation = Box::pin(retry.wait(&scope, std::future::pending::<Result<()>>()));
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(wakes.count(), 1);
        assert!(retry.alarms.borrow().is_empty());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(retry.alarms.borrow().len(), 1);
        assert_eq!(wakes.count(), 1);
    }

    #[test]
    fn unrelated_polls_retain_one_alarm_and_completion_detaches_it() {
        let env = racer_io::Env::host();
        let retry = NativeRetry::unattached();
        let scope = scope(&env);
        let ready = Cell::new(false);
        let mut operation = Box::pin(retry.wait(
            &scope,
            poll_fn(|_| {
                if ready.get() {
                    Poll::Ready(Ok::<_, Error>(()))
                } else {
                    Poll::Pending
                }
            }),
        ));
        let mut cx = Context::from_waker(std::task::Waker::noop());
        for _ in 0..20 {
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert_eq!(retry.alarms.borrow().len(), 1);
        }
        ready.set(true);
        assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
        assert!(retry.alarms.borrow()[0].is_canceled());
    }

    #[test]
    fn cancellation_and_alarm_failure_do_not_leave_pending_without_wakes() {
        let env = racer_io::Env::host();
        for cancel in [false, true] {
            let retry = NativeRetry::unattached();
            let scope = scope(&env);
            let mut operation = Box::pin(retry.wait(&scope, std::future::pending::<Result<()>>()));
            let mut cx = Context::from_waker(std::task::Waker::noop());
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            if cancel {
                scope.cancel().unwrap();
            } else {
                retry.alarms.borrow_mut().clear();
            }
            assert_eq!(
                operation.as_mut().poll(&mut cx),
                Poll::Ready(Err(if cancel {
                    Error::Cancelled
                } else {
                    Error::Unavailable
                }))
            );
        }
    }

    #[test]
    fn alarm_wakes_child_to_observe_deadline_without_native_wake() {
        use futures::{Stream, stream::FuturesUnordered};
        let clock = racer_io::SimClock::new(989);
        let env = racer_io::Env::new(racer_io::Driver::Uring, clock.ctx(0));
        let retry = NativeRetry::unattached();
        let scope = scope(&env);
        let mut active = FuturesUnordered::new();
        active.push(retry.wait(&scope, std::future::pending::<Result<()>>()));
        let mut cx = Context::from_waker(std::task::Waker::noop());
        assert!(Pin::new(&mut active).poll_next(&mut cx).is_pending());
        clock.advance(Duration::from_secs(31));
        assert!(Pin::new(&mut active).poll_next(&mut cx).is_pending());
        retry.fire();
        assert_eq!(
            Pin::new(&mut active).poll_next(&mut cx),
            Poll::Ready(Some(Err(Error::DeadlineExceeded)))
        );
    }

    #[test]
    /// An op the owner completes on its own thread wakes the waiting task
    /// directly: no alarm fires, and the only alarm armed is the deadline.
    fn owner_thread_completion_wakes_waiter_without_alarm() {
        let env = racer_io::Env::host();
        let (mut clients, port) = rdma_verbs::node(1).unwrap();
        let client = clients.remove(0);
        let retry = NativeRetry::unattached();
        let scope = scope(&env);
        let wakes = std::sync::Arc::new(crate::test_support::WakeCounter::default());
        let waker = std::task::Waker::from(wakes.clone());
        let mut cx = Context::from_waker(&waker);
        let op = client.open(rdma_verbs::LinkKey {
            peer: [3; 32],
            rail: 0,
        });
        let mut waiting = Box::pin(retry.wait(&scope, poll_fn(|cx| op.poll(cx))));
        // Poll before the owner exists, so only its completion can wake us.
        assert!(waiting.as_mut().poll(&mut cx).is_pending());
        assert_eq!(retry.alarms.borrow().len(), 1);
        let (sender, receiver) = std::sync::mpsc::channel();
        let owner = std::thread::spawn(move || {
            let owner = rdma_verbs::Owner::new(port, racer_io::WorkerCtx::host());
            let (mut role, stop) = rdma_verbs::OwnerRole::new(owner).unwrap();
            sender.send(stop).unwrap();
            racer_io::exec::run(&mut role).unwrap();
        });
        let stop = receiver.recv().unwrap();
        let end = std::time::Instant::now() + Duration::from_secs(5);
        while wakes.count() == 0 {
            assert!(std::time::Instant::now() < end, "owner completion lost");
            std::thread::sleep(Duration::from_millis(1));
        }
        // Not activated: the owner answers with an error, which still wakes.
        assert!(matches!(
            waiting.as_mut().poll(&mut cx),
            Poll::Ready(Err(_))
        ));
        assert!(retry.alarms.borrow()[0].is_canceled());
        stop.stop();
        owner.join().unwrap();
    }

    #[test]
    fn kernel_alarm_wakes_child_and_releases_timer_after_completion() {
        use futures::{Stream, stream::FuturesUnordered};
        let env = racer_io::Env::host();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(true).limits,
        )));
        let reactor = Rc::new(crate::runtime::Reactor::new(admission, &env));
        reactor
            .init()
            .expect("real io_uring required for kernel alarm validation");
        let retry = NativeRetry::new(reactor.clone());
        // The alarm is the scope deadline: a short scope makes it fire.
        let short = RequestScope::on(
            &env.ctx.clock,
            crate::model::RequestId([7; 16]),
            env.ctx.now() + Duration::from_millis(20),
        )
        .unwrap();
        let polls = Cell::new(0);
        let mut active = FuturesUnordered::new();
        active.push(retry.wait(
            &short,
            poll_fn(|_| {
                polls.set(polls.get() + 1);
                Poll::<Result<()>>::Pending
            }),
        ));
        let mut cx = Context::from_waker(std::task::Waker::noop());
        assert!(Pin::new(&mut active).poll_next(&mut cx).is_pending());
        assert_eq!(polls.get(), 1);
        // Alarms are reactor-local timers: they hold no kernel owner.
        assert_eq!(reactor.in_flight(), 0);
        let end = std::time::Instant::now() + Duration::from_secs(2);
        loop {
            assert!(
                std::time::Instant::now() < end,
                "kernel alarm did not complete"
            );
            reactor.poll_budgeted(16).unwrap();
            if let Poll::Ready(Some(result)) = Pin::new(&mut active).poll_next(&mut cx) {
                assert_eq!(result, Err(Error::DeadlineExceeded));
                break;
            }
            std::thread::sleep(Duration::from_millis(1));
        }
        assert_eq!(polls.get(), 1, "the deadline fails the wait, no retry poll");
        let scope = scope(&env);
        while reactor.in_flight() != 0 {
            assert!(std::time::Instant::now() < end, "timer fence did not drain");
            reactor.poll_budgeted(16).unwrap();
            std::thread::sleep(Duration::from_millis(1));
        }

        // A natural event can complete before the alarm CQE. Abandoning the
        // alarm must leave its kernel owner charged until the reactor fences it.
        let ready = Cell::new(false);
        let mut operation = Box::pin(retry.wait(
            &scope,
            poll_fn(|_| {
                if ready.get() {
                    Poll::Ready(Ok::<_, Error>(()))
                } else {
                    Poll::Pending
                }
            }),
        ));
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(reactor.in_flight(), 0);
        ready.set(true);
        assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
        drop(operation);
        assert_eq!(reactor.in_flight(), 0);
        while reactor.in_flight() != 0 {
            assert!(
                std::time::Instant::now() < end,
                "abandoned timer did not drain"
            );
            reactor.poll_budgeted(16).unwrap();
            std::thread::sleep(Duration::from_millis(1));
        }

        // Rapid native stages can abandon several alarms before a reactor turn.
        // Admission remains bounded; after each bounded batch all owners drain.
        for _ in 0..4 {
            for _ in 0..8 {
                let ready = Cell::new(false);
                let mut operation = Box::pin(retry.wait(
                    &scope,
                    poll_fn(|_| {
                        if ready.get() {
                            Poll::Ready(Ok::<_, Error>(()))
                        } else {
                            Poll::Pending
                        }
                    }),
                ));
                assert!(operation.as_mut().poll(&mut cx).is_pending());
                ready.set(true);
                assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
            }
            assert_eq!(reactor.in_flight(), 0);
            while reactor.in_flight() != 0 {
                assert!(
                    std::time::Instant::now() < end,
                    "rapid-stage alarms did not drain"
                );
                reactor.poll_budgeted(16).unwrap();
                std::thread::sleep(Duration::from_millis(1));
            }
        }
    }
}
