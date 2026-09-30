//! Explicit execution ownership. No library may introduce an unbudgeted thread pool.

pub mod admission;
pub mod affinity;
pub mod channel;
pub(crate) mod collections;
pub mod crypto;
pub mod deadline;
pub mod environment;
pub(crate) mod ingress;
#[cfg(test)]
mod listener_tests;
pub mod reactor;
pub mod worker;

use crate::error::{Error, Operation};
use deadline::RequestScope;
use std::{task::Poll, time::Duration};

/// Retry listener submission without terminating its service on queue pressure.
/// Workers poll services every turn, with a reactor::wait fallback of at most
/// 10ms. Do not self-wake or submit a timer to the saturated ring. One attempt per
/// interval bounds work even when unrelated completions keep the worker busy.
/// Only Overloaded is retried; shutdown, deadlines and fatal I/O still propagate.
pub(crate) fn retry_listener<'a, T: 'a>(
    scope: &'a RequestScope,
    mut submit: impl FnMut() -> Operation<'a, T> + 'a,
) -> Operation<'a, T> {
    Box::pin(async move {
        let mut operation = None;
        let mut retry_at = environment::now();
        std::future::poll_fn(|cx| {
            if operation.is_none() {
                scope.check()?;
                if environment::now() < retry_at {
                    return Poll::Pending;
                }
                operation = Some(submit());
            }
            match operation.as_mut().unwrap().as_mut().poll(cx) {
                Poll::Ready(Err(Error::Overloaded)) => {
                    // A failed submission published no SQE. For submitted work,
                    // the underlying future retains the existing CQE fence.
                    operation = None;
                    retry_at = environment::now() + Duration::from_millis(10);
                    Poll::Pending
                }
                result => result,
            }
        })
        .await
    })
}
