use crate::{Error, Operation, Scope, environment};
use std::{task::Poll, time::Duration};

/// Submission retry policy. The owner supplies periodic polling at `interval` or
/// faster; this helper never allocates a timer on an overloaded backend.
#[derive(Clone, Copy, Debug)]
pub struct RetryOptions {
    pub interval: Duration,
    /// Number of retries after the initial attempt. None preserves scope-bounded
    /// legacy behavior; Some(0) returns the first overload without retrying.
    pub max_retries: Option<usize>,
}

impl Default for RetryOptions {
    fn default() -> Self {
        Self {
            interval: Duration::from_millis(10),
            max_retries: None,
        }
    }
}

/// Retry listener submission on queue pressure without self-waking or submitting
/// a timer to the saturated ring. The owner must poll at least every 10ms.
/// Submitted operations retain their original completion fence on cancellation.
pub fn retry_listener<'a, S: Scope, T: 'a>(
    scope: &'a S,
    submit: impl FnMut() -> Operation<'a, T, S::Error> + 'a,
) -> Operation<'a, T, S::Error>
where
    S::Error: PartialEq,
{
    retry_listener_with(scope, RetryOptions::default(), submit)
}

pub fn retry_listener_with<'a, S: Scope, T: 'a>(
    scope: &'a S,
    options: RetryOptions,
    mut submit: impl FnMut() -> Operation<'a, T, S::Error> + 'a,
) -> Operation<'a, T, S::Error>
where
    S::Error: PartialEq,
{
    Box::pin(async move {
        if options.interval.is_zero() {
            return Err(Error::InvalidConfiguration.into());
        }
        let cancellation = scope.cancellation().map(|c| c.subscribe()).transpose()?;
        let mut operation = None;
        let mut retries = 0usize;
        let mut retry_at = environment::now();
        std::future::poll_fn(|cx| {
            if operation.is_none() {
                scope.check()?;
                if let Some(cancellation) = &cancellation {
                    cancellation.register(cx.waker());
                }
                if environment::now() < retry_at {
                    return Poll::Pending;
                }
                operation = Some(submit());
            }
            match operation.as_mut().unwrap().as_mut().poll(cx) {
                Poll::Ready(Err(error)) if error == Error::Overloaded.into() => {
                    operation = None;
                    if options.max_retries.is_some_and(|limit| retries >= limit) {
                        return Poll::Ready(Err(error));
                    }
                    retries = retries.checked_add(1).ok_or(Error::Overloaded)?;
                    retry_at = environment::now()
                        .checked_add(options.interval)
                        .ok_or(Error::InvalidInput)?;
                    Poll::Pending
                }
                result => result,
            }
        })
        .await
    })
}

#[cfg(all(test, feature = "simulation"))]
mod tests {
    use super::*;
    use crate::{Result, test_util::WakeCounter};
    use std::{
        cell::Cell,
        rc::Rc,
        sync::Arc,
        task::{Context, Waker},
    };

    #[derive(Clone, Default)]
    struct TestScope(Rc<Cell<Option<Error>>>);
    impl Scope for TestScope {
        type Error = Error;
        fn check(&self) -> Result<()> {
            self.0.get().map_or(Ok(()), Err)
        }
    }

    #[test]
    fn configured_retry_limit_interval_zero_and_overflow() {
        let clock = environment::SimulationClock::new(4);
        let _role = clock.environment(0).enter();
        let scope = TestScope::default();
        let attempts = Cell::new(0);
        let mut future = retry_listener_with(
            &scope,
            RetryOptions {
                interval: Duration::from_millis(3),
                max_retries: Some(1),
            },
            || {
                attempts.set(attempts.get() + 1);
                Box::pin(async { Err::<(), _>(Error::Overloaded) })
            },
        );
        let mut cx = Context::from_waker(Waker::noop());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        clock.advance(Duration::from_millis(2));
        assert!(future.as_mut().poll(&mut cx).is_pending());
        assert_eq!(attempts.get(), 1);
        clock.advance(Duration::from_millis(1));
        assert_eq!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        );
        assert_eq!(attempts.get(), 2);
        for (interval, error) in [
            (Duration::ZERO, Error::InvalidConfiguration),
            (Duration::MAX, Error::InvalidInput),
        ] {
            let mut future = retry_listener_with(
                &scope,
                RetryOptions {
                    interval,
                    max_retries: None,
                },
                || Box::pin(async { Err::<(), _>(Error::Overloaded) }),
            );
            assert_eq!(future.as_mut().poll(&mut cx), Poll::Ready(Err(error)));
        }
    }

    #[test]
    fn retries_only_after_ten_ms_without_self_wakes_and_checks_scope() {
        let clock = environment::SimulationClock::new(45);
        let _environment = clock.environment(0).enter();
        for cancel in [false, true] {
            let scope = TestScope::default();
            let attempts = Cell::new(0);
            let wake = Arc::new(WakeCounter::default());
            let waker = Waker::from(wake.clone());
            let mut cx = Context::from_waker(&waker);
            let mut future = retry_listener(&scope, || {
                let attempt = attempts.get();
                attempts.set(attempt + 1);
                Box::pin(async move {
                    if attempt == 0 {
                        Err(Error::Overloaded)
                    } else {
                        Ok(7)
                    }
                })
            });
            assert!(future.as_mut().poll(&mut cx).is_pending());
            clock.advance(Duration::from_millis(9));
            assert!(future.as_mut().poll(&mut cx).is_pending());
            assert_eq!(attempts.get(), 1);
            assert_eq!(wake.count(), 0);
            if cancel {
                scope.0.set(Some(Error::Cancelled));
            }
            clock.advance(Duration::from_millis(1));
            assert_eq!(
                future.as_mut().poll(&mut cx),
                Poll::Ready(if cancel { Err(Error::Cancelled) } else { Ok(7) })
            );
            assert_eq!(attempts.get(), if cancel { 1 } else { 2 });
        }
    }

    #[test]
    fn accepted_pending_operation_is_not_truncated_by_scope_failure() {
        let scope = TestScope::default();
        let completed = Cell::new(false);
        let attempts = Cell::new(0);
        let mut future = retry_listener(&scope, || {
            attempts.set(attempts.get() + 1);
            Box::pin(std::future::poll_fn(|_| {
                if completed.get() {
                    Poll::Ready(Err::<(), _>(Error::Io))
                } else {
                    Poll::Pending
                }
            }))
        });
        let mut cx = Context::from_waker(Waker::noop());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        scope.0.set(Some(Error::Cancelled));
        assert!(future.as_mut().poll(&mut cx).is_pending());
        completed.set(true);
        assert_eq!(future.as_mut().poll(&mut cx), Poll::Ready(Err(Error::Io)));
        assert_eq!(attempts.get(), 1);
    }
}
