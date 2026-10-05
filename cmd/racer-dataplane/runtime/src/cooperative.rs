//! Small explicitly polled mechanisms, without an executor or application policy.
use crate::{Error, Operation, Result, Scope, group::FailureReporter, reactor::ReactorWake};
use std::{
    cell::Cell,
    sync::Arc,
    task::{Context, Poll, Wake, Waker},
    thread,
};

/// Yield exactly one cooperative turn, waking the current task for its next poll.
pub async fn yield_now() {
    let mut yielded = false;
    std::future::poll_fn(|cx| {
        if std::mem::replace(&mut yielded, true) {
            Poll::Ready(())
        } else {
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    })
    .await
}

/// Poll after registering cancellation and checking caller-owned scope policy.
///
/// The caller must arrange deadline polling; this does not create a timer or
/// self-wake. Do not use it to truncate an operation's required completion fence.
pub async fn poll_scoped<S: Scope, T, E: Into<S::Error>>(
    scope: &S,
    mut poll: impl FnMut(&mut Context<'_>) -> Poll<Result<T, E>>,
) -> Result<T, S::Error> {
    let cancellation = scope.cancellation().map(|c| c.subscribe()).transpose()?;
    std::future::poll_fn(|cx| {
        if let Some(cancellation) = &cancellation {
            cancellation.register(cx.waker());
        }
        scope.check()?;
        poll(cx).map_err(Into::into)
    })
    .await
}

/// Worker-local exclusion flag. Dropping the guard makes the flag available.
#[must_use = "dropping the guard releases the busy flag"]
pub struct Busy<'a>(&'a Cell<bool>);

impl<'a> Busy<'a> {
    pub fn try_enter(flag: &'a Cell<bool>) -> Result<Self> {
        if flag.replace(true) {
            Err(Error::Overloaded)
        } else {
            Ok(Self(flag))
        }
    }
}

impl Drop for Busy<'_> {
    fn drop(&mut self) {
        self.0.set(false);
    }
}

struct ThreadWake(thread::Thread, Option<ReactorWake>);

impl Wake for ThreadWake {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }

    fn wake_by_ref(self: &Arc<Self>) {
        self.0.unpark();
        if let Some(reactor) = &self.1 {
            let _ = reactor.wake();
        }
    }
}

/// Wake this thread and, optionally, interrupt its reactor wait. No thread is spawned.
pub fn thread_waker(reactor: Option<ReactorWake>) -> Waker {
    Waker::from(Arc::new(ThreadWake(thread::current(), reactor)))
}

/// Drive resource completions while a local operation borrows its service graph.
///
/// Backend failures are reported immediately but do not abandon the operation.
/// Its completion fence runs to completion, then the first backend error wins.
/// `wait` must be bounded. After it returns, unpark this thread so a surrounding
/// thread-parking driver does not add a second sleep before consuming completions.
pub fn drive_local_with<'a, T: 'a, E: Copy + 'a>(
    mut operation: Operation<'a, T, E>,
    reporter: Option<&'a FailureReporter<E>>,
    mut poll: impl FnMut(&Waker) -> Result<(), E> + 'a,
    mut wait: impl FnMut() -> Result<(), E> + 'a,
) -> Operation<'a, T, E> {
    let mut error = None;
    Box::pin(std::future::poll_fn(move |cx| {
        if let Err(failure) = poll(cx.waker()) {
            if let Some(reporter) = reporter {
                reporter.report(failure);
            }
            error.get_or_insert(failure);
        }
        if let Poll::Ready(result) = operation.as_mut().poll(cx) {
            return Poll::Ready(error.map_or(result, Err));
        }
        if let Err(failure) = wait() {
            if let Some(reporter) = reporter {
                reporter.report(failure);
            }
            error.get_or_insert(failure);
        }
        thread::current().unpark();
        Poll::Pending
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::deadline::Cancellation;
    use crate::test_util::WakeCounter;
    use std::{future::Future, pin::pin, rc::Rc};

    #[derive(Clone)]
    struct TestScope {
        cancellation: Option<Cancellation>,
        error: Rc<Cell<Option<Error>>>,
    }

    impl Scope for TestScope {
        type Error = Error;
        fn check(&self) -> Result<()> {
            if self
                .cancellation
                .as_ref()
                .is_some_and(Cancellation::is_cancelled)
            {
                return Err(Error::Cancelled);
            }
            self.error.get().map_or(Ok(()), Err)
        }
        fn cancellation(&self) -> Option<&Cancellation> {
            self.cancellation.as_ref()
        }
    }

    #[test]
    fn yield_is_pending_once_and_wakes_once() {
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let mut future = pin!(yield_now());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        assert_eq!(count.count(), 1);
        assert!(future.as_mut().poll(&mut cx).is_ready());
        assert_eq!(count.count(), 1);
    }

    #[test]
    fn scoped_poll_checks_policy_before_callback_with_or_without_cancellation() {
        for cancellation in [None, Some(Cancellation::new().unwrap())] {
            let scope = TestScope {
                cancellation,
                error: Rc::new(Cell::new(None)),
            };
            let calls = Cell::new(0);
            let mut future = pin!(poll_scoped(&scope, |_| -> Poll<Result<()>> {
                calls.set(calls.get() + 1);
                Poll::Pending
            }));
            let mut cx = Context::from_waker(Waker::noop());
            assert!(future.as_mut().poll(&mut cx).is_pending());
            scope.error.set(Some(Error::DeadlineExceeded));
            assert_eq!(
                future.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::DeadlineExceeded))
            );
            assert_eq!(calls.get(), 1);
        }
    }

    #[test]
    fn scoped_poll_wakes_on_cancel_and_releases_registration_on_drop() {
        let cancellation = Cancellation::new().unwrap();
        let scope = TestScope {
            cancellation: Some(cancellation.clone()),
            error: Rc::new(Cell::new(None)),
        };
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let mut future = Box::pin(poll_scoped(&scope, |_| -> Poll<Result<()>> {
            Poll::Pending
        }));
        assert!(future.as_mut().poll(&mut cx).is_pending());
        cancellation.cancel().unwrap();
        assert_eq!(count.count(), 1);
        assert_eq!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        );
        drop(future);
        assert_eq!(Arc::strong_count(&count), 2);
    }

    #[test]
    fn scoped_poll_preserves_ready_results_and_subscription_failure() {
        let cancellation = Cancellation::new().unwrap();
        let scope = TestScope {
            cancellation: Some(cancellation.clone()),
            error: Rc::new(Cell::new(None)),
        };
        assert_eq!(
            futures::executor::block_on(poll_scoped(&scope, |_| Poll::Ready(Ok::<_, Error>(7)))),
            Ok(7)
        );
        assert_eq!(
            futures::executor::block_on(poll_scoped(&scope, |_| Poll::Ready(Err::<(), _>(
                Error::Io
            )))),
            Err(Error::Io)
        );
        let _registrations: Vec<_> = (0..1024)
            .map(|_| cancellation.subscribe().unwrap())
            .collect();
        assert_eq!(
            futures::executor::block_on(poll_scoped(&scope, |_| -> Poll<Result<()>> {
                panic!("subscription must fail first")
            })),
            Err(Error::Overloaded)
        );
    }

    #[test]
    fn busy_excludes_reentry_and_releases_on_drop_and_unwind() {
        let flag = Cell::new(false);
        let guard = Busy::try_enter(&flag).unwrap();
        assert!(matches!(Busy::try_enter(&flag), Err(Error::Overloaded)));
        assert!(flag.get());
        drop(guard);
        assert!(!flag.get());
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let _guard = Busy::try_enter(&flag).unwrap();
            panic!("unwind");
        }));
        assert!(result.is_err());
        assert!(!flag.get());
        drop(Busy::try_enter(&flag).unwrap());
    }

    #[test]
    fn driver_retains_operation_and_first_backend_error_until_completion() {
        for fail_poll in [false, true] {
            let ready = Cell::new(false);
            let waits = Cell::new(0);
            let operation = Box::pin(std::future::poll_fn(|_| {
                if ready.get() {
                    Poll::Ready(Err::<(), _>(Error::Unavailable))
                } else {
                    Poll::Pending
                }
            }));
            let mut future = drive_local_with(
                operation,
                None,
                |_| if fail_poll { Err(Error::Io) } else { Ok(()) },
                || {
                    waits.set(waits.get() + 1);
                    Err(Error::Overloaded)
                },
            );
            let mut cx = Context::from_waker(Waker::noop());
            assert!(future.as_mut().poll(&mut cx).is_pending());
            ready.set(true);
            assert_eq!(
                future.as_mut().poll(&mut cx),
                Poll::Ready(Err(if fail_poll {
                    Error::Io
                } else {
                    Error::Overloaded
                }))
            );
            assert_eq!(waits.get(), 1);
        }
    }

    #[test]
    fn driver_preserves_ready_value_and_does_not_wait() {
        for result in [Ok(7), Err(Error::Unavailable)] {
            let mut future = drive_local_with(
                Box::pin(async move { result }),
                None,
                |_| Ok(()),
                || panic!("ready operation must not wait"),
            );
            assert_eq!(
                future
                    .as_mut()
                    .poll(&mut Context::from_waker(Waker::noop())),
                Poll::Ready(result)
            );
        }
    }

    #[test]
    fn thread_waker_targets_creator_from_another_thread() {
        let (send, receive) = std::sync::mpsc::channel();
        let target = thread::spawn(move || {
            send.send(thread_waker(None)).unwrap();
            thread::park_timeout(std::time::Duration::from_secs(2));
            send.send(thread_waker(None)).unwrap();
        });
        let waker = receive
            .recv_timeout(std::time::Duration::from_secs(2))
            .unwrap();
        waker.wake_by_ref();
        // The second message confirms the target resumed, not the waking thread.
        receive
            .recv_timeout(std::time::Duration::from_secs(1))
            .unwrap();
        target.join().unwrap();
    }
}
