//! Primary-first delayed races that drain submitted work before returning.
use std::{
    future::Future,
    task::{Context, Poll},
};

/// Child scopes must support cancellation requests independently of the parent.
pub trait Scope: crate::Scope {
    fn cancel(&self);
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Contender {
    Primary,
    Secondary,
}

/// Application classification and observation, without application result types.
pub trait Policy<E> {
    fn delay(&mut self, cx: &mut Context<'_>) -> Poll<()>;
    fn recoverable(&self, error: E) -> bool;
    fn failure(&mut self) -> E;
    fn won(&mut self, contender: Contender);
}

/// Validation belongs inside each contender. A ready primary (even a recoverable
/// failure) never launches the secondary. Cancellation requests are not fences.
///
/// The owner must keep polling this future after caller detachment and retain
/// permits/escrow until it returns. Dropping a Rust future cannot drain its work.
/// The caller also drives deadlines and the policy's delay alarm.
pub async fn race<S: Scope, T>(
    primary: impl Future<Output = Result<T, S::Error>>,
    secondary: impl Future<Output = Result<T, S::Error>>,
    primary_scope: &S,
    secondary_scope: &S,
    parent: &S,
    mut policy: impl Policy<S::Error>,
) -> Result<T, S::Error> {
    parent.check()?;
    let mut primary = std::pin::pin!(primary);
    let mut secondary = std::pin::pin!(secondary);
    let mut registration = parent.cancellation().map(|c| c.subscribe()).transpose()?;
    let mut a_done = false;
    let mut b_done = false;
    let mut launched = false;
    let mut winner = None;
    let mut fatal = None;
    let mut a_canceled = false;
    let mut b_canceled = false;
    std::future::poll_fn(|cx| {
        if let Err(error) = parent.check() {
            fatal = Some(error);
            registration.take();
        }
        if let Some(registration) = &registration {
            registration.register(cx.waker());
        }
        if fatal.is_some() || winner.is_some() {
            if !a_done && !a_canceled {
                a_canceled = true;
                primary_scope.cancel();
            }
            if !b_done && !b_canceled {
                b_canceled = true;
                secondary_scope.cancel();
            }
        }
        if !a_done && let Poll::Ready(result) = primary.as_mut().poll(cx) {
            a_done = true;
            match result {
                Ok(value) if winner.is_none() && fatal.is_none() => {
                    policy.won(Contender::Primary);
                    winner = Some(value);
                }
                Err(error) if !policy.recoverable(error) && winner.is_none() && fatal.is_none() => {
                    fatal = Some(error);
                }
                _ => {}
            }
            if !launched {
                b_done = true;
            }
        }
        if !launched
            && !b_done
            && fatal.is_none()
            && winner.is_none()
            && policy.delay(cx).is_ready()
        {
            launched = true;
        }
        if launched && !b_done {
            if (fatal.is_some() || winner.is_some()) && !b_canceled {
                b_canceled = true;
                secondary_scope.cancel();
            }
            if let Poll::Ready(result) = secondary.as_mut().poll(cx) {
                b_done = true;
                match result {
                    Ok(value) if winner.is_none() && fatal.is_none() => {
                        policy.won(Contender::Secondary);
                        winner = Some(value);
                    }
                    Err(error)
                        if !policy.recoverable(error) && winner.is_none() && fatal.is_none() =>
                    {
                        fatal = Some(error);
                    }
                    _ => {}
                }
            }
        }
        if winner.is_some() || fatal.is_some() {
            if !a_done && !a_canceled {
                a_canceled = true;
                primary_scope.cancel();
            }
            if !b_done && !b_canceled {
                b_canceled = true;
                secondary_scope.cancel();
            }
            if !launched {
                b_done = true;
            }
        }
        if a_done && b_done {
            Poll::Ready(if let Some(error) = fatal {
                Err(error)
            } else {
                winner.take().ok_or_else(|| policy.failure())
            })
        } else {
            Poll::Pending
        }
    })
    .await
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{Error, deadline::Cancellation};
    use std::{cell::Cell, rc::Rc, task::Waker};

    #[derive(Clone)]
    struct TestScope(Cancellation);
    impl TestScope {
        fn new() -> Self {
            Self(Cancellation::new().unwrap())
        }
    }
    impl crate::Scope for TestScope {
        type Error = Error;
        fn check(&self) -> Result<(), Error> {
            if self.0.is_cancelled() {
                Err(Error::Cancelled)
            } else {
                Ok(())
            }
        }
        fn cancellation(&self) -> Option<&Cancellation> {
            Some(&self.0)
        }
    }
    impl Scope for TestScope {
        fn cancel(&self) {
            self.0.cancel().unwrap();
        }
    }

    struct Hooks<'a> {
        due: &'a Cell<bool>,
        won: &'a Cell<Option<Contender>>,
        failures: &'a Cell<usize>,
    }
    impl Policy<Error> for Hooks<'_> {
        fn delay(&mut self, _: &mut Context<'_>) -> Poll<()> {
            if self.due.get() {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        }
        fn recoverable(&self, error: Error) -> bool {
            error == Error::Io
        }
        fn failure(&mut self) -> Error {
            self.failures.set(self.failures.get() + 1);
            Error::NotFound
        }
        fn won(&mut self, contender: Contender) {
            self.won.set(Some(contender));
        }
    }
    #[derive(Default)]
    struct Fixture {
        due: Cell<bool>,
        won: Cell<Option<Contender>>,
        failures: Cell<usize>,
    }
    impl Fixture {
        fn hooks(&self) -> Hooks<'_> {
            Hooks {
                due: &self.due,
                won: &self.won,
                failures: &self.failures,
            }
        }
    }

    #[test]
    fn hedge_fast_primary_success_or_failure_never_launches_secondary() {
        for result in [
            Ok(Rc::new(String::from("primary"))),
            Err(Error::Io),
            Err(Error::InvalidInput),
        ] {
            let fixture = Fixture::default();
            fixture.due.set(true);
            let a = TestScope::new();
            let b = TestScope::new();
            let parent = TestScope::new();
            let expected = if result == Err(Error::Io) {
                Err(Error::NotFound)
            } else {
                result.clone()
            };
            let output = futures::executor::block_on(race(
                std::future::ready(result),
                async { panic!("secondary launched after primary completed") },
                &a,
                &b,
                &parent,
                fixture.hooks(),
            ));
            assert_eq!(output, expected);
            assert_eq!(
                fixture.won.get(),
                expected.is_ok().then_some(Contender::Primary)
            );
            assert_eq!(
                fixture.failures.get(),
                usize::from(expected == Err(Error::NotFound))
            );
        }
    }

    #[test]
    fn hedge_canceled_parent_never_polls_either_future() {
        let fixture = Fixture::default();
        let a = TestScope::new();
        let b = TestScope::new();
        let parent = TestScope::new();
        parent.cancel();
        let never = || {
            std::future::poll_fn(|_| -> Poll<Result<(), Error>> {
                panic!("submitted after cancellation")
            })
        };
        assert_eq!(
            futures::executor::block_on(race(never(), never(), &a, &b, &parent, fixture.hooks())),
            Err(Error::Cancelled)
        );
        assert_eq!(fixture.won.get(), None);
        assert_eq!(fixture.failures.get(), 0);
    }

    #[test]
    fn hedge_secondary_winner_waits_for_fence_and_parent_can_override() {
        for cancel_parent in [false, true] {
            let fixture = Fixture::default();
            let a = TestScope::new();
            let b = TestScope::new();
            let parent = TestScope::new();
            let fenced = Cell::new(false);
            let primary = std::future::poll_fn(|_| {
                if fenced.get() {
                    Poll::Ready(Err(Error::Cancelled))
                } else {
                    Poll::Pending
                }
            });
            let mut work = Box::pin(race(
                primary,
                async { Ok(String::from("secondary")) },
                &a,
                &b,
                &parent,
                fixture.hooks(),
            ));
            let mut cx = Context::from_waker(Waker::noop());
            assert!(work.as_mut().poll(&mut cx).is_pending());
            assert_eq!(fixture.won.get(), None);
            fixture.due.set(true);
            assert!(work.as_mut().poll(&mut cx).is_pending());
            assert_eq!(fixture.won.get(), Some(Contender::Secondary));
            assert!(a.0.is_cancelled());
            assert!(!parent.0.is_cancelled());
            if cancel_parent {
                parent.cancel();
            }
            assert!(work.as_mut().poll(&mut cx).is_pending());
            fenced.set(true);
            let expected = if cancel_parent {
                Err(Error::Cancelled)
            } else {
                Ok(String::from("secondary"))
            };
            assert_eq!(work.as_mut().poll(&mut cx), Poll::Ready(expected));
            assert_eq!(fixture.failures.get(), 0);
        }
    }

    #[test]
    fn hedge_classification_controls_cancellation_and_empty_failure_hook() {
        for secondary_error in [Error::Io, Error::InvalidInput] {
            for primary_result in [Ok(7), Err(Error::Io)] {
                let fixture = Fixture::default();
                fixture.due.set(true);
                let a = TestScope::new();
                let b = TestScope::new();
                let parent = TestScope::new();
                let fenced = Cell::new(false);
                let primary = std::future::poll_fn(|_| {
                    if fenced.get() {
                        Poll::Ready(primary_result)
                    } else {
                        Poll::Pending
                    }
                });
                let mut work = Box::pin(race(
                    primary,
                    std::future::ready(Err(secondary_error)),
                    &a,
                    &b,
                    &parent,
                    fixture.hooks(),
                ));
                let mut cx = Context::from_waker(Waker::noop());
                assert!(work.as_mut().poll(&mut cx).is_pending());
                assert_eq!(a.0.is_cancelled(), secondary_error == Error::InvalidInput);
                fenced.set(true);
                let expected = if secondary_error == Error::InvalidInput {
                    Err(secondary_error)
                } else {
                    primary_result.map_err(|_| Error::NotFound)
                };
                assert_eq!(work.as_mut().poll(&mut cx), Poll::Ready(expected));
                assert_eq!(
                    fixture.won.get(),
                    expected.is_ok().then_some(Contender::Primary)
                );
                assert_eq!(
                    fixture.failures.get(),
                    usize::from(expected == Err(Error::NotFound))
                );
            }
        }
    }

    #[test]
    fn hedge_parent_cancellation_wakes_and_drains_both_separate_fences() {
        let fixture = Fixture::default();
        fixture.due.set(true);
        let a = TestScope::new();
        let b = TestScope::new();
        let parent = TestScope::new();
        let a_fence = Cell::new(false);
        let b_fence = Cell::new(false);
        let child = |fence: &Cell<bool>| {
            let ready = fence.get();
            if ready {
                Poll::Ready(Ok(9))
            } else {
                Poll::Pending
            }
        };
        let mut work = Box::pin(race(
            std::future::poll_fn(|_| child(&a_fence)),
            std::future::poll_fn(|_| child(&b_fence)),
            &a,
            &b,
            &parent,
            fixture.hooks(),
        ));
        let wake = std::sync::Arc::new(crate::test_util::WakeCounter::default());
        let waker = Waker::from(wake.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(work.as_mut().poll(&mut cx).is_pending());
        parent.cancel();
        assert_eq!(wake.count(), 1);
        assert!(work.as_mut().poll(&mut cx).is_pending());
        assert!(a.0.is_cancelled() && b.0.is_cancelled());
        for _ in 0..100 {
            assert!(work.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!(
            wake.count(),
            1,
            "draining canceled children must not busy-wake"
        );
        a_fence.set(true);
        assert!(work.as_mut().poll(&mut cx).is_pending());
        b_fence.set(true);
        assert_eq!(
            work.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        );
        assert_eq!(fixture.won.get(), None);
        assert_eq!(fixture.failures.get(), 0);
    }

    #[test]
    fn hedge_simultaneous_readiness_prefers_primary_but_drains_secondary() {
        let fixture = Fixture::default();
        fixture.due.set(true);
        let a = TestScope::new();
        let b = TestScope::new();
        let parent = TestScope::new();
        let ready = Cell::new(false);
        let secondary_drained = Cell::new(false);
        let primary = std::future::poll_fn(|_| {
            if ready.get() {
                Poll::Ready(Ok(1))
            } else {
                Poll::Pending
            }
        });
        let secondary = std::future::poll_fn(|_| {
            if ready.get() {
                assert!(b.0.is_cancelled());
                secondary_drained.set(true);
                Poll::Ready(Ok(2))
            } else {
                Poll::Pending
            }
        });
        let mut work = Box::pin(race(primary, secondary, &a, &b, &parent, fixture.hooks()));
        let mut cx = Context::from_waker(Waker::noop());
        assert!(work.as_mut().poll(&mut cx).is_pending());
        ready.set(true);
        assert_eq!(work.as_mut().poll(&mut cx), Poll::Ready(Ok(1)));
        assert!(secondary_drained.get());
        assert_eq!(fixture.won.get(), Some(Contender::Primary));
    }
}
