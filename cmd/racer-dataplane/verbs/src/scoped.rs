//! Caller-scoped mailbox waits and native service composition.
use crate::{Configuration, Error, IoPort, NativePort, NativeService, Selection};
use std::task::{Context, Poll, Waker};
use uring_runtime::{
    Operation, Scope,
    group::{FailureReporter, Service},
    poll_scoped,
};

impl IoPort {
    /// Submit an owned configuration and await native activation. The caller
    /// reopens the port and supplies tags, selection policy, geometry and guards.
    /// Abandonment after submission closes admission but never releases native
    /// ownership before the native service has performed its actual fences.
    pub async fn activate<S: Scope>(
        &self,
        configuration: Configuration,
        scope: &S,
    ) -> Result<Vec<Selection>, S::Error>
    where
        S::Error: From<Error>,
    {
        let mut configure = std::pin::pin!(self.configure(configuration));
        poll_scoped(scope, |cx| {
            std::future::Future::poll(configure.as_mut(), cx)
        })
        .await?;
        struct ActivationGuard<'a> {
            port: &'a IoPort,
            completed: bool,
        }
        impl Drop for ActivationGuard<'_> {
            fn drop(&mut self) {
                if !self.completed {
                    self.port.close();
                }
            }
        }
        let mut guard = ActivationGuard {
            port: self,
            completed: false,
        };
        let cancel = scope.cancellation().map(|c| c.subscribe()).transpose()?;
        let mappings = std::future::poll_fn(|cx| {
            self.register_driver(cx.waker());
            if let Some(cancel) = &cancel {
                cancel.register(cx.waker());
            }
            if self.closed() {
                return Poll::Ready(Err(Error::Unavailable.into()));
            }
            if let Err(error) = scope.check() {
                self.close();
                return Poll::Ready(Err(error));
            }
            self.activation()
                .map(|r| r.map_err(Into::into))
                .map_or(Poll::Pending, Poll::Ready)
        })
        .await?;
        guard.completed = true;
        Ok(mappings)
    }
}

/// Compose a local service with paired native progress. Both receive the poll
/// budget; drain closes and fences native resources one slot per turn before
/// draining the inner service, even if the supplied scope has already expired.
/// Admission stop leaves native progress available for accepted work. Close and
/// fence also cover native ownership after a failed drain or inner shutdown.
pub struct WithNative<T> {
    inner: T,
    native: NativeService,
}
impl<T> WithNative<T> {
    pub fn new(inner: T, port: NativePort) -> Self {
        Self {
            inner,
            native: NativeService::new(port),
        }
    }
}
impl<S: Scope, T: Service<S>> Service<S> for WithNative<T>
where
    S::Error: From<Error>,
{
    fn set_failure_reporter(&mut self, reporter: FailureReporter<S::Error>) {
        self.inner.set_failure_reporter(reporter);
    }
    fn waker(&self) -> Result<Waker, S::Error> {
        self.inner.waker()
    }
    fn register_driver(&self, waker: &Waker) {
        self.inner.register_driver(waker);
        self.native.register_driver(waker);
    }
    fn start<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        self.inner.start(scope)
    }
    fn poll_budgeted(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<(), S::Error> {
        self.inner.poll_budgeted(cx, budget)?;
        self.native.poll_budgeted(budget).map_err(Into::into)
    }
    fn stop_admission(&mut self) -> Result<(), S::Error> {
        // Accepted work may still submit native commands until drain begins.
        self.inner.stop_admission()
    }
    fn drain<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            self.native.close_and_fence().await?;
            self.inner.drain(scope).await
        })
    }
    fn close(&mut self) -> Result<(), S::Error> {
        // Close native admission even when the inner close reports a failure.
        self.native.close();
        self.inner.close()
    }
    fn fence<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            self.native.close_and_fence().await?;
            self.inner.fence(scope).await
        })
    }
    fn shutdown<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        self.inner.shutdown(scope)
    }
}

impl NativeService {
    async fn close_and_fence(&mut self) -> crate::Result<()> {
        self.close();
        std::future::poll_fn(|cx| {
            self.register_driver(cx.waker());
            self.poll_budgeted(1)?;
            if self.drained() {
                Poll::Ready(Ok(()))
            } else {
                // The worker's bounded tick retries failed native fences.
                Poll::Pending
            }
        })
        .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        cell::RefCell,
        rc::Rc,
        sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        },
        task::Wake,
    };
    use uring_runtime::deadline::Cancellation;

    #[derive(Clone, Copy, Debug, PartialEq, Eq)]
    enum Failure {
        Runtime(uring_runtime::Error),
        Verbs(Error),
    }
    impl From<uring_runtime::Error> for Failure {
        fn from(e: uring_runtime::Error) -> Self {
            Self::Runtime(e)
        }
    }
    impl From<Error> for Failure {
        fn from(e: Error) -> Self {
            Self::Verbs(e)
        }
    }
    #[derive(Clone)]
    struct TestScope(Option<Cancellation>);
    impl Scope for TestScope {
        type Error = Failure;
        fn check(&self) -> Result<(), Failure> {
            if self.0.as_ref().is_some_and(|c| c.is_cancelled()) {
                Err(uring_runtime::Error::Cancelled.into())
            } else {
                Ok(())
            }
        }
        fn cancellation(&self) -> Option<&Cancellation> {
            self.0.as_ref()
        }
    }
    fn config(guard: crate::Guard) -> Configuration {
        Configuration {
            discover: false,
            selector: Box::new(|ports| {
                assert!(ports.is_empty());
                Ok(vec![])
            }),
            guards: vec![guard],
            bytes: 32,
        }
    }
    struct Count(AtomicUsize);
    impl Wake for Count {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }

    #[test]
    fn scoped_wait_wakes_and_checks_cancellation_before_operation() {
        let scope = TestScope(Some(Cancellation::new().unwrap()));
        let calls = std::cell::Cell::new(0);
        let mut future = Box::pin(poll_scoped(&scope, |_| -> Poll<Result<(), Error>> {
            calls.set(calls.get() + 1);
            Poll::Pending
        }));
        let count = Arc::new(Count(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(future.as_mut().poll(&mut cx).is_pending());
        scope.0.as_ref().unwrap().cancel().unwrap();
        assert_eq!(count.0.load(Ordering::Relaxed), 1);
        assert_eq!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Failure::Runtime(uring_runtime::Error::Cancelled)))
        );
        assert_eq!(calls.get(), 1);
        assert_eq!(
            futures::executor::block_on(poll_scoped(&TestScope(None), |_| Poll::Ready(
                Err::<(), _>(Error::Io)
            ))),
            Err(Failure::Verbs(Error::Io))
        );
    }

    #[test]
    fn activation_drop_or_cancel_retains_submitted_guard_until_native_turn() {
        for cancel in [false, true] {
            let (io, port) = crate::pair(1).unwrap();
            let mut native = NativeService::new(port);
            let scope = TestScope(Some(Cancellation::new().unwrap()));
            let (guard, observer) = crate::test_guard::guard();
            let mut future = Box::pin(io.activate(config(guard), &scope));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(future.as_mut().poll(&mut cx).is_pending());
            if cancel {
                scope.0.as_ref().unwrap().cancel().unwrap();
                assert_eq!(
                    future.as_mut().poll(&mut cx),
                    Poll::Ready(Err(Failure::Runtime(uring_runtime::Error::Cancelled)))
                );
            }
            drop(future);
            assert!(io.closed());
            assert_eq!(observer.get(), 1);
            native.poll_budgeted(1).unwrap();
            assert_eq!(observer.get(), 0);
            assert!(native.drained());
        }
    }

    #[test]
    fn activation_success_and_selector_error_preserve_scope_free_use() {
        for fail in [false, true] {
            let (io, port) = crate::pair(1).unwrap();
            let mut native = NativeService::new(port);
            let scope = TestScope(None);
            let mut config = config(Arc::new(()));
            if fail {
                config.selector = Box::new(|_| Err(Error::InvalidConfiguration));
            }
            let mut future = Box::pin(io.activate(config, &scope));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(future.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(1).unwrap();
            let result = future.as_mut().poll(&mut cx);
            if fail {
                assert_eq!(
                    result,
                    Poll::Ready(Err(Failure::Verbs(Error::InvalidConfiguration)))
                );
                assert!(io.closed());
            } else {
                assert_eq!(result, Poll::Ready(Ok(vec![])));
                assert!(!io.closed());
            }
        }
    }

    struct Inner {
        calls: Rc<RefCell<Vec<&'static str>>>,
        io: Rc<IoPort>,
        failure: Option<Failure>,
        wake: Waker,
    }
    impl Service<TestScope> for Inner {
        fn waker(&self) -> Result<Waker, Failure> {
            self.calls.borrow_mut().push("waker");
            self.failure.map_or_else(|| Ok(self.wake.clone()), Err)
        }
        fn register_driver(&self, _: &Waker) {
            self.calls.borrow_mut().push("register");
        }
        fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
            self.calls.borrow_mut().push("start");
            Box::pin(async { Ok(()) })
        }
        fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<(), Failure> {
            assert_eq!(budget, 7);
            self.calls.borrow_mut().push("poll");
            Ok(())
        }
        fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
            assert!(self.io.closed());
            self.io
                .reopen()
                .expect("native fence must precede inner drain");
            self.calls.borrow_mut().push("drain");
            Box::pin(async { Ok(()) })
        }
        fn stop_admission(&mut self) -> Result<(), Failure> {
            assert!(
                !self.io.closed(),
                "accepted native work must remain available"
            );
            self.calls.borrow_mut().push("stop");
            self.failure.map_or(Ok(()), Err)
        }
        fn close(&mut self) -> Result<(), Failure> {
            assert!(self.io.closed());
            self.calls.borrow_mut().push("close");
            self.failure.map_or(Ok(()), Err)
        }
        fn fence<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
            assert!(self.io.closed());
            self.calls.borrow_mut().push("fence");
            Box::pin(async move { self.failure.map_or(Ok(()), Err) })
        }
        fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
            self.calls.borrow_mut().push("shutdown");
            Box::pin(async { Ok(()) })
        }
    }
    #[test]
    fn wrapper_drives_inner_and_fences_native_before_canceled_scope_drain() {
        let (io, port) = crate::pair(1).unwrap();
        let calls = Rc::new(RefCell::new(vec![]));
        let mut service = WithNative::new(
            Inner {
                calls: calls.clone(),
                io: Rc::new(io),
                failure: None,
                wake: Waker::from(Arc::new(Count(AtomicUsize::new(0)))),
            },
            port,
        );
        let scope = TestScope(Some(Cancellation::new().unwrap()));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(service.waker().unwrap().will_wake(&service.inner.wake));
        service.register_driver(cx.waker());
        futures::executor::block_on(service.start(&scope)).unwrap();
        service.poll_budgeted(&mut cx, 7).unwrap();
        service.stop_admission().unwrap();
        scope.0.as_ref().unwrap().cancel().unwrap();
        let mut drain = service.drain(&scope);
        assert_eq!(drain.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
        drop(drain);
        service.close().unwrap();
        futures::executor::block_on(service.fence(&scope)).unwrap();
        futures::executor::block_on(service.shutdown(&scope)).unwrap();
        futures::executor::block_on(service.fence(&scope)).unwrap();
        assert_eq!(
            *calls.borrow(),
            [
                "waker", "register", "start", "poll", "stop", "drain", "close", "fence",
                "shutdown", "fence"
            ]
        );
    }

    #[test]
    fn wrapper_forwards_hook_errors_and_closes_native_on_inner_close_failure() {
        let (io, port) = crate::pair(1).unwrap();
        let mut service = WithNative::new(
            Inner {
                calls: Rc::default(),
                io: Rc::new(io),
                failure: Some(Failure::Verbs(Error::Io)),
                wake: Waker::noop().clone(),
            },
            port,
        );
        assert!(matches!(service.waker(), Err(Failure::Verbs(Error::Io))));
        assert_eq!(service.stop_admission(), Err(Failure::Verbs(Error::Io)));
        assert_eq!(service.close(), Err(Failure::Verbs(Error::Io)));
        assert!(service.inner.io.closed());
        assert_eq!(
            futures::executor::block_on(service.fence(&TestScope(None))),
            Err(Failure::Verbs(Error::Io))
        );
    }

    #[test]
    fn wrapper_failure_reporter_reaches_inner_service_and_group() {
        use uring_runtime::group::{Factory, Group, Lane, Plan};
        struct Reporting(Option<FailureReporter<Failure>>);
        impl Service<TestScope> for Reporting {
            fn set_failure_reporter(&mut self, reporter: FailureReporter<Failure>) {
                self.0 = Some(reporter);
            }
            fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
                Box::pin(async { Ok(()) })
            }
            fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<(), Failure> {
                self.0
                    .as_ref()
                    .expect("forwarded reporter")
                    .report(Error::Cancelled.into());
                Ok(())
            }
            fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
                Box::pin(async { Ok(()) })
            }
            fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
                Box::pin(async { Ok(()) })
            }
        }
        struct Recipe;
        impl Factory<TestScope> for Recipe {
            fn build_lane(&self, _: usize) -> Result<Box<dyn Service<TestScope>>, Failure> {
                let (_io, port) = crate::pair(1)?;
                Ok(Box::new(WithNative::new(Reporting(None), port)))
            }
        }
        let cpu = *uring_runtime::affinity::current_cpus()
            .unwrap()
            .first()
            .unwrap();
        let mut group = Group::new(Plan {
            lanes: vec![Lane {
                name: "verbs-forwarding".into(),
                cpu,
            }],
            helpers: vec![],
            max_threads: 1,
        });
        assert_eq!(
            group.run(&Recipe, &TestScope(None)),
            Err(Failure::Verbs(Error::Cancelled))
        );
        assert_eq!(group.stats().done, 1);
    }

    #[test]
    fn wrapper_drain_and_fence_retry_native_failure_despite_cancellation() {
        for fence in [false, true] {
            let clock = uring_runtime::environment::SimulationClock::new(17);
            let _clock = clock.environment(0).enter();
            let sim = crate::simulation::Simulation::new()
                .with_devices(vec![crate::simulation::Device::new("sim0", [1; 16])])
                .unwrap();
            let _sim = sim.enter();
            let (io, port) = crate::pair(1).unwrap();
            let io = Rc::new(io);
            let calls = Rc::new(RefCell::new(vec![]));
            let mut service = WithNative::new(
                Inner {
                    calls: calls.clone(),
                    io: io.clone(),
                    failure: None,
                    wake: Waker::noop().clone(),
                },
                port,
            );
            let (guard, observer) = crate::test_guard::guard();
            futures::executor::block_on(io.configure(Configuration {
                discover: true,
                guards: vec![guard],
                bytes: 32,
                selector: Box::new(|_| Ok(vec![(0, 0)])),
            }))
            .unwrap();
            service.native.poll_budgeted(1).unwrap();
            service.native.poll_budgeted(1).unwrap();
            io.activation().unwrap().unwrap();
            let scope = TestScope(Some(Cancellation::new().unwrap()));
            scope.0.as_ref().unwrap().cancel().unwrap();
            sim.reject(crate::simulation::Operation::Stop, None, true);
            let mut operation = if fence {
                service.fence(&scope)
            } else {
                service.drain(&scope)
            };
            let mut cx = Context::from_waker(Waker::noop());
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert!(
                calls.borrow().is_empty(),
                "inner hook must wait for native destruction"
            );
            assert_eq!(observer.get(), 1);
            sim.reject(crate::simulation::Operation::Stop, None, false);
            clock.advance(std::time::Duration::from_millis(10));
            assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
            drop(operation);
            assert_eq!(*calls.borrow(), [if fence { "fence" } else { "drain" }]);
            assert_eq!(observer.get(), 0);
            assert_eq!(sim.live_resources(), 0);
        }
    }
}
