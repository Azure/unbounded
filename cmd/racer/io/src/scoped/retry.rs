//! Resubmission under queue pressure, paced by the reactor's timer heap.
//!
//! A retry never submits a timer to the saturated ring: the wait is a
//! reactor timer, fired by [`Reactor::poll_budgeted`] and bounding
//! [`Reactor::wait`] and the simulated driver's clock advance.
use super::Reactor;
use crate::{Error, Operation, Scope, ring::Sleep};
use std::{future::Future, pin::Pin, task::Poll, time::Duration};

/// Pacing for [`Reactor::retry_overloaded`].
#[derive(Clone, Copy, Debug)]
pub(super) struct RetryOptions {
    /// Delay between an overloaded submission and the next attempt.
    pub(super) interval: Duration,

    /// Retries after the first attempt; `None` retries until the scope fails.
    pub(super) max_retries: Option<usize>,
}

impl Default for RetryOptions {
    fn default() -> Self {
        Self {
            interval: Duration::from_millis(10),
            max_retries: None,
        }
    }
}

impl<S: Scope> Reactor<S> {
    /// Resubmit `submit` every 10ms of reactor time while it fails with
    /// [`Error::Overloaded`], rechecking `scope` before each attempt. An
    /// accepted operation keeps its own completion fence: a scope failure
    /// while it is pending does not truncate it.
    pub fn retry_overloaded<'a, T: 'a>(
        &'a self,
        scope: &'a S,
        submit: impl FnMut() -> Operation<'a, T, S::Error> + 'a,
    ) -> Operation<'a, T, S::Error>
    where
        S::Error: PartialEq,
    {
        self.retry_with(scope, RetryOptions::default(), submit)
    }

    pub(super) fn retry_with<'a, T: 'a>(
        &'a self,
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
            let io = self.io()?.clone();
            let cancellation = scope.cancel_wait();
            let mut operation = None;
            let mut retries = 0usize;
            let mut backoff: Option<Sleep> = None;
            std::future::poll_fn(|cx| {
                if operation.is_none() {
                    scope.check()?;
                    if let Some(cancellation) = &cancellation {
                        cancellation.register(cx.waker());
                    }
                    if let Some(sleep) = &mut backoff {
                        if Pin::new(sleep).poll(cx).is_pending() {
                            return Poll::Pending;
                        }
                        backoff = None;
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
                        let at = io
                            .now()
                            .checked_add(options.interval)
                            .ok_or(Error::InvalidInput)?;
                        let mut sleep = io.sleep_until(at);
                        // Registers the timer; `at` is in the future.
                        let _ = Pin::new(&mut sleep).poll(cx);
                        backoff = Some(sleep);
                        Poll::Pending
                    }
                    result => result,
                }
            })
            .await
        })
    }
}
