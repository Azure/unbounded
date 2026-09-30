use super::{deadline::RequestScope, environment, retry_listener as retry};
use crate::error::Error;
use crate::{model::RequestId, runtime::environment::SimulationClock};
use std::{
    cell::Cell,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    task::{Context, Wake, Waker},
};
use std::{task::Poll, time::Duration};

#[derive(Default)]
struct Wakes(AtomicUsize);
impl Wake for Wakes {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::Relaxed);
    }
}

#[test]
fn repeated_pressure_is_rate_limited_without_self_wakes() {
    let clock = SimulationClock::new(1);
    let environment = clock.environment(0);
    let _time = environment.enter();
    let scope = RequestScope::new(
        RequestId([0; 16]),
        environment::now() + Duration::from_secs(1),
    )
    .unwrap();
    let calls = Cell::new(0);
    let wakes = Arc::new(Wakes::default());
    let waker = Waker::from(wakes.clone());
    let mut cx = Context::from_waker(&waker);
    let mut operation = retry(&scope, || {
        calls.set(calls.get() + 1);
        Box::pin(std::future::ready(if calls.get() <= 4 {
            Err(Error::Overloaded)
        } else {
            Ok(7)
        }))
    });
    for attempt in 1..=4 {
        for _ in 0..100 {
            assert!(operation.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!(calls.get(), attempt);
        clock.advance(Duration::from_millis(9));
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(calls.get(), attempt);
        clock.advance(Duration::from_millis(1));
    }
    assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Ok(7)));
    assert_eq!(wakes.0.load(Ordering::Relaxed), 0);
}

#[test]
fn backoff_preserves_shutdown_deadlines_and_fatal_errors() {
    let clock = SimulationClock::new(2);
    let environment = clock.environment(0);
    let _time = environment.enter();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for expected in [Error::Cancelled, Error::DeadlineExceeded] {
        let scope = RequestScope::new(
            RequestId([0; 16]),
            environment::now() + Duration::from_millis(1),
        )
        .unwrap();
        let mut operation = retry::<()>(&scope, || {
            Box::pin(std::future::ready(Err(Error::Overloaded)))
        });
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        if expected == Error::Cancelled {
            scope.cancel().unwrap();
        } else {
            clock.advance(Duration::from_millis(1));
        }
        assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Err(expected)));
    }
    let scope = RequestScope::new(
        RequestId([0; 16]),
        environment::now() + Duration::from_secs(1),
    )
    .unwrap();
    for error in [
        Error::Io,
        Error::Unavailable,
        Error::InvalidConfiguration,
        Error::Internal,
    ] {
        let mut operation = retry::<()>(&scope, || Box::pin(std::future::ready(Err(error))));
        assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Err(error)));
    }
}

#[test]
fn submitted_listener_cancellation_waits_for_cqe_fence() {
    use crate::runtime::{
        admission::Admission,
        reactor::{Reactor, simulation::Simulation},
    };
    use std::rc::Rc;
    let sim = Simulation::new();
    let _os = sim.enter();
    let clock = SimulationClock::new(3);
    let environment = clock.environment(0);
    let _time = environment.enter();
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Reactor::new(admission);
    let scope = RequestScope::new(
        RequestId([0; 16]),
        environment::now() + Duration::from_secs(1),
    )
    .unwrap();
    let (reader, _writer) = sim.socket_pair();
    let reader = Rc::new(reader);
    let mut operation = retry(&scope, || {
        reactor.readiness(reader.clone(), libc::POLLIN as u32, &scope)
    });
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(operation.as_mut().poll(&mut cx).is_pending());
    assert_eq!(reactor.in_flight(), 1);
    scope.cancel().unwrap();
    // Do not return cancellation early and pretend kernel ownership is fenced.
    assert!(operation.as_mut().poll(&mut cx).is_pending());
    assert_eq!(reactor.in_flight(), 1);
    for _ in 0..4 {
        reactor.poll_budgeted(8).unwrap();
    }
    assert_eq!(
        operation.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    );
    assert_eq!(reactor.in_flight(), 0);
}
