//! Racer policy and request-scope wiring for the generic pipe pool.
use crate::{
    error::Operation,
    model::ResourceClass,
    runtime::{
        admission::{AdmissionExt, AdmissionPolicy},
        deadline::RequestScope,
    },
};
use flow_control::{
    Quotas,
    pipe::{PipeLease, PipePool},
};
use std::{rc::Rc, task::Waker};

pub fn new_pipe_pool(admission: Rc<Quotas<AdmissionPolicy>>) -> PipePool<AdmissionPolicy> {
    let waiter_limit = admission.limits().queue_entries.get();
    PipePool::new(
        admission,
        ResourceClass::Pipe,
        ResourceClass::RequestContext,
        waiter_limit,
    )
}

pub(crate) fn acquire_wait<'a>(
    pool: &'a PipePool<AdmissionPolicy>,
    scope: &'a RequestScope,
) -> Operation<'a, PipeLease<AdmissionPolicy>> {
    Box::pin(pool.acquire_wait(
        || scope.check(),
        || {
            let cancellation = scope.cancellation.subscribe()?;
            Ok(move |waker: &Waker| cancellation.register(waker))
        },
    ))
}

#[cfg(test)]
pub(super) mod tests {
    use super::*;
    use crate::{error::Error, model::Limits};

    pub(in crate::memory) fn admission(pipes: usize) -> Rc<Quotas<AdmissionPolicy>> {
        let small = std::num::NonZeroUsize::new(8).unwrap();
        let bytes = std::num::NonZeroUsize::new(32 * 1024 * 1024).unwrap();
        Rc::new(Quotas::new(AdmissionPolicy::new(Limits {
            plaintext_bytes: bytes,
            ciphertext_bytes: bytes,
            dirty_bytes: bytes,
            registered_bytes: bytes,
            request_context_bytes: bytes,
            flights: small,
            waiters_per_flight: small,
            queue_entries: small,
            connections_per_neighbor: small,
            client_connections: small,
            pipes: std::num::NonZeroUsize::new(pipes).unwrap(),
            range_window_pages: small,
            header_bytes: small,
            cached_rankings: small,
            cached_paths: small,
            retained_snapshots: small,
            metadata_entries: small,
            relay_transfers: small,
        })))
    }

    #[test]
    fn immediate_acquisition_does_not_subscribe_to_cancellation() {
        use crate::model::RequestId;
        use std::{
            task::{Context, Poll},
            time::{Duration, Instant},
        };
        let admission = admission(1);
        let pool = new_pipe_pool(admission.clone());
        let scope =
            RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
        let mut registrations = Vec::new();
        loop {
            match scope.cancellation.subscribe() {
                Ok(registration) => registrations.push(registration),
                Err(Error::Overloaded) => break,
                Err(error) => panic!("unexpected registration failure: {error:?}"),
            }
            assert!(registrations.len() <= 1024);
        }
        let mut cx = Context::from_waker(Waker::noop());
        let Poll::Ready(Ok(held)) = acquire_wait(&pool, &scope).as_mut().poll(&mut cx) else {
            panic!("immediate acquisition unnecessarily subscribed")
        };
        assert!(matches!(
            acquire_wait(&pool, &scope).as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        assert_eq!(admission.used(ResourceClass::Pipe), 1);
        drop(registrations);
        let mut wait = acquire_wait(&pool, &scope);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        drop(held);
        assert!(matches!(wait.as_mut().poll(&mut cx), Poll::Ready(Ok(_))));
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
    }

    #[test]
    fn scheduled_wait_cancellation_deadline_stop_and_abandonment_release_admission() {
        use crate::{model::RequestId, test_support::WakeCounter};
        use std::{
            sync::Arc,
            task::{Context, Poll},
            time::{Duration, Instant},
        };
        for failure in [
            Some(Error::Cancelled),
            Some(Error::DeadlineExceeded),
            Some(Error::Unavailable),
            None,
        ] {
            let admission = admission(1);
            let pool = new_pipe_pool(admission.clone());
            let held = pool.acquire().unwrap();
            let scope = RequestScope::new(
                RequestId([0; 16]),
                Instant::now()
                    + if failure == Some(Error::DeadlineExceeded) {
                        Duration::from_millis(10)
                    } else {
                        Duration::from_secs(5)
                    },
            )
            .unwrap();
            let count = Arc::new(WakeCounter::default());
            let waker = Waker::from(count.clone());
            let mut cx = Context::from_waker(&waker);
            let mut wait = acquire_wait(&pool, &scope);
            assert!(wait.as_mut().poll(&mut cx).is_pending());
            assert_eq!(
                admission.used(ResourceClass::RequestContext),
                pool.waiter_bytes()
            );
            match failure {
                Some(Error::Cancelled) => {
                    scope.cancel().unwrap();
                    assert!(count.count() > 0);
                }
                Some(Error::DeadlineExceeded) => std::thread::sleep(Duration::from_millis(20)),
                Some(Error::Unavailable) => admission.stop(),
                _ => {}
            }
            if let Some(expected) = failure {
                assert!(
                    matches!(wait.as_mut().poll(&mut cx), Poll::Ready(Err(error)) if error == expected)
                );
            }
            drop(wait);
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
            assert_eq!(admission.used(ResourceClass::Pipe), 1);
            drop(held);
            if failure != Some(Error::Unavailable) {
                // No stale FIFO entry may prevent the next caller's progress.
                let fresh =
                    RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(5))
                        .unwrap();
                assert!(matches!(
                    acquire_wait(&pool, &fresh).as_mut().poll(&mut cx),
                    Poll::Ready(Ok(_))
                ));
            }
            drop(pool);
            assert_eq!(admission.used(ResourceClass::Pipe), 0);
        }
    }
}
