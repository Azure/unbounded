use crate::{Error, Operation, Scope, environment};
use std::{task::Poll, time::Duration};

/// Retry listener submission on queue pressure without self-waking or submitting
/// a timer to the saturated ring. The owner must poll at least every 10ms.
/// Submitted operations retain their original completion fence on cancellation.
pub fn retry_listener<'a, S: Scope, T: 'a>(
    scope: &'a S,
    mut submit: impl FnMut() -> Operation<'a, T, S::Error> + 'a,
) -> Operation<'a, T, S::Error>
where
    S::Error: PartialEq,
{
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
                Poll::Ready(Err(error)) if error == Error::Overloaded.into() => {
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
