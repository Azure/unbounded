//! Startup futures must drive native activation without steady-state polling.
use rdma_verbs::{Configuration, Error, IoPort, WithNative, pair};
use std::{
    rc::Rc,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    task::{Context, Poll, Wake, Waker},
};
use uring_runtime::{Operation, Scope, environment::Cancellation, group::Service};

/// Keep runtime cancellation distinct from activation errors.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Failure {
    Runtime(uring_runtime::Error),

    Verbs(Error),
}

impl From<uring_runtime::Error> for Failure {
    /// Preserve the runtime failure.
    fn from(error: uring_runtime::Error) -> Self {
        Self::Runtime(error)
    }
}

impl From<Error> for Failure {
    /// Preserve the activation failure.
    fn from(error: Error) -> Self {
        Self::Verbs(error)
    }
}

/// Startup scope with explicit cancellation and no wall-clock dependency.
#[derive(Clone)]
struct StartupScope(Cancellation);

impl Scope for StartupScope {
    type Error = Failure;

    /// Reject canceled startup.
    fn check(&self) -> Result<(), Failure> {
        if self.0.is_cancelled() {
            Err(uring_runtime::Error::Cancelled.into())
        } else {
            Ok(())
        }
    }

    /// Register activation waits with the cancellation source.
    fn cancellation(&self) -> Option<&Cancellation> {
        Some(&self.0)
    }
}

/// Inner service whose startup cannot finish until native activation completes.
struct Activating {
    io: Rc<IoPort>,

    reject: bool,
}

impl Service<StartupScope> for Activating {
    /// Await an empty plan without loading a native backend.
    fn start<'a>(&'a mut self, scope: &'a StartupScope) -> Operation<'a, (), Failure> {
        Box::pin(async move {
            let reject = self.reject;
            let selected = self
                .io
                .activate(
                    Configuration {
                        discover: false,
                        selector: Box::new(move |ports| {
                            assert!(ports.is_empty());
                            if reject {
                                Err(Error::InvalidConfiguration)
                            } else {
                                Ok(vec![])
                            }
                        }),
                        guards: vec![Arc::new(())],
                        bytes: 32,
                    },
                    scope,
                )
                .await?;
            assert!(selected.is_empty());
            Ok(())
        })
    }

    /// Steady-state polling must not be needed during startup.
    fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<(), Failure> {
        panic!("startup must drive native work through its own future")
    }

    /// No inner resources need draining.
    fn drain<'a>(&'a mut self, _: &'a StartupScope) -> Operation<'a, (), Failure> {
        Box::pin(async { Ok(()) })
    }

    /// No inner resources need shutdown.
    fn shutdown<'a>(&'a mut self, _: &'a StartupScope) -> Operation<'a, (), Failure> {
        Box::pin(async { Ok(()) })
    }
}

/// Count notifications without relying on an executor or a timer.
struct Notifications(AtomicUsize);

impl Wake for Notifications {
    /// Record a wake from configuration submission or activation completion.
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::Relaxed);
    }
}

/// Poll only startup, as the runtime does, and require native activation progress.
#[test]
fn startup_awaits_empty_plan_activation_and_preserves_selector_errors() {
    for reject in [false, true] {
        let (io, native) = pair(1).unwrap();
        let io = Rc::new(io);
        let mut service = WithNative::new(
            Activating {
                io: io.clone(),
                reject,
            },
            native,
        );
        let scope = StartupScope(Cancellation::new().unwrap());
        let notifications = Arc::new(Notifications(AtomicUsize::new(0)));
        let waker = Waker::from(notifications.clone());
        let mut cx = Context::from_waker(&waker);
        let mut startup = service.start(&scope);
        assert!(startup.as_mut().poll(&mut cx).is_pending());
        let expected = if reject {
            Err(Failure::Verbs(Error::InvalidConfiguration))
        } else {
            Ok(())
        };
        assert_eq!(startup.as_mut().poll(&mut cx), Poll::Ready(expected));
        assert!(notifications.0.load(Ordering::Relaxed) > 0);
        drop(startup);
        assert_eq!(io.closed(), reject);
        futures::executor::block_on(service.drain(&scope)).unwrap();
        assert!(io.closed());
    }
}

/// Canceling a pending startup still closes its activation pool.
#[test]
fn startup_activation_preserves_scope_cancellation() {
    let (io, native) = pair(1).unwrap();
    let io = Rc::new(io);
    let mut service = WithNative::new(
        Activating {
            io: io.clone(),
            reject: false,
        },
        native,
    );
    let scope = StartupScope(Cancellation::new().unwrap());
    let mut cx = Context::from_waker(Waker::noop());
    let mut startup = service.start(&scope);
    assert!(startup.as_mut().poll(&mut cx).is_pending());
    scope.0.cancel().unwrap();
    assert_eq!(
        startup.as_mut().poll(&mut cx),
        Poll::Ready(Err(Failure::Runtime(uring_runtime::Error::Cancelled)))
    );
    drop(startup);
    assert!(io.closed());
    futures::executor::block_on(service.drain(&scope)).unwrap();
}
