//! Retained one-shot alarms for native waits, using the serving worker's reactor.

use super::*;
use std::{future::Future, time::Duration};

/// Timer backend shared by native handles, never by individual pending alarms.
pub(super) struct NativeRetry {
    reactor: Option<Rc<crate::runtime::Reactor>>,

    #[cfg(test)]
    alarms: RefCell<Vec<futures::channel::oneshot::Sender<()>>>,

    #[cfg(test)]
    immediate: Cell<bool>,
}

impl NativeRetry {
    /// Bind production waits to the existing worker reactor.
    pub(super) fn new(reactor: Rc<crate::runtime::Reactor>) -> Rc<Self> {
        Rc::new(Self {
            reactor: Some(reactor),
            #[cfg(test)]
            alarms: RefCell::new(Vec::new()),
            #[cfg(test)]
            immediate: Cell::new(false),
        })
    }

    /// Unattached devices cannot start native work without a retry backend.
    pub(super) fn unattached() -> Rc<Self> {
        Rc::new(Self {
            reactor: None,
            #[cfg(test)]
            alarms: RefCell::new(Vec::new()),
            #[cfg(test)]
            immediate: Cell::new(false),
        })
    }

    /// One millisecond bounds retries without spinning on a held mailbox.
    fn alarm<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            #[cfg(test)]
            if self.immediate.replace(false) {
                return Ok(());
            }
            let until = (uring_runtime::environment::now() + Duration::from_millis(1))
                .min(scope.deadline.0);
            if let Some(reactor) = &self.reactor {
                reactor
                    .sleep_until(
                        until,
                        uring_runtime::reactor::timer::SleepMode::Kernel,
                        scope,
                    )
                    .await
            } else {
                #[cfg(test)]
                {
                    let (send, receive) = futures::channel::oneshot::channel();
                    self.alarms.borrow_mut().push(send);
                    receive.await.map_err(|_| Error::Unavailable)
                }
                #[cfg(not(test))]
                Err(Error::Unavailable)
            }
        })
    }

    /// Keep one alarm across unrelated wakes; native success never allocates one.
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

    /// Retry a required fence without applying a request's cancellation policy.
    pub(super) async fn fence(
        &self,
        operation: impl Future<Output = rdma_verbs::Result<()>>,
    ) -> Result<()> {
        let mut operation = std::pin::pin!(operation);
        let mut alarm: Option<Operation<'_, ()>> = None;
        poll_fn(|cx| {
            if let Poll::Ready(result) = operation.as_mut().poll(cx) {
                return Poll::Ready(result.map_err(Into::into));
            }
            if let Some(timer) = alarm.as_mut() {
                match timer.as_mut().poll(cx) {
                    Poll::Pending => return Poll::Pending,
                    Poll::Ready(result) => result?,
                }
            }
            alarm = Some(Box::pin(async {
                // This scope owns only the alarm. It cannot cancel the DMA fence.
                let scope = RequestScope::new(
                    crate::model::RequestId([0; 16]),
                    uring_runtime::environment::now() + Duration::from_secs(30),
                )?;
                self.alarm(&scope).await
            }));
            match alarm.as_mut().unwrap().as_mut().poll(cx) {
                Poll::Pending => Poll::Pending,
                Poll::Ready(Err(error)) => Poll::Ready(Err(error)),
                Poll::Ready(Ok(())) => {
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

    /// Inject a terminal alarm delivery failure without polling the child directly.
    #[cfg(test)]
    pub(super) fn fail(&self) {
        self.alarms.borrow_mut().clear();
    }
}

#[cfg(test)]
mod tests {
    //! Explicit one-shot alarm tests, without cooperative self-wakes.
    use super::*;
    use std::{pin::Pin, task::Context};

    fn scope() -> RequestScope {
        RequestScope::new(
            crate::model::RequestId([7; 16]),
            uring_runtime::environment::now() + Duration::from_secs(30),
        )
        .unwrap()
    }

    #[test]
    fn immediate_results_never_arm_an_alarm() {
        let retry = NativeRetry::unattached();
        let scope = scope();
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
        let retry = NativeRetry::unattached();
        retry.immediate.set(true);
        let scope = scope();
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
        let retry = NativeRetry::unattached();
        let scope = scope();
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
        for cancel in [false, true] {
            let retry = NativeRetry::unattached();
            let scope = scope();
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
        let clock = uring_runtime::environment::SimulationClock::new(989);
        let _environment = clock.environment(0).enter();
        let retry = NativeRetry::unattached();
        let scope = scope();
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
    fn fence_alarm_never_reports_success_before_native_fence() {
        let retry = NativeRetry::unattached();
        let ready = Cell::new(false);
        let mut operation = Box::pin(retry.fence(poll_fn(|_| {
            if ready.get() {
                Poll::Ready(Ok(()))
            } else {
                Poll::Pending
            }
        })));
        let mut cx = Context::from_waker(std::task::Waker::noop());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        retry.fire();
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        ready.set(true);
        assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
    }

    #[test]
    fn kernel_alarm_wakes_child_and_releases_timer_after_completion() {
        use futures::{Stream, stream::FuturesUnordered};
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(true).limits,
        )));
        let reactor = Rc::new(crate::runtime::Reactor::new(admission));
        reactor
            .init()
            .expect("real io_uring required for kernel alarm validation");
        let retry = NativeRetry::new(reactor.clone());
        let scope = scope();
        let polls = Cell::new(0);
        let mut active = FuturesUnordered::new();
        active.push(retry.wait(
            &scope,
            poll_fn(|_| {
                polls.set(polls.get() + 1);
                if polls.get() == 1 {
                    Poll::Pending
                } else {
                    Poll::Ready(Ok::<_, Error>(()))
                }
            }),
        ));
        let mut cx = Context::from_waker(std::task::Waker::noop());
        assert!(Pin::new(&mut active).poll_next(&mut cx).is_pending());
        assert_eq!(polls.get(), 1);
        assert_eq!(reactor.in_flight(), 1);
        let end = std::time::Instant::now() + Duration::from_secs(2);
        loop {
            assert!(
                std::time::Instant::now() < end,
                "kernel alarm did not complete"
            );
            reactor.poll_budgeted(16).unwrap();
            if let Poll::Ready(Some(result)) = Pin::new(&mut active).poll_next(&mut cx) {
                result.unwrap();
                break;
            }
            std::thread::sleep(Duration::from_millis(1));
        }
        assert_eq!(polls.get(), 2);
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
        assert_eq!(reactor.in_flight(), 1);
        ready.set(true);
        assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
        drop(operation);
        assert_eq!(reactor.in_flight(), 1, "drop is not a kernel fence");
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
            assert_eq!(reactor.in_flight(), 8);
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
