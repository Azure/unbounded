//! Optional node-wide FIFO admission for exchanges that can receive a page.
//!
//! This gate does not grant routing authority or replenish acquisition credits.
//! A caller must drive a deadline alarm as well as cancellation while queued.
//! Active permits belong to transport completion owners, not header futures.
use crate::admission::{AdmissionPolicy, ResourceClass};
use crate::error::{Error, Result};
use crate::runtime::RequestScope;
use crate::telemetry::{Event, Gauge, Metrics};
use flow_control::{Charge, Quotas};
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll};
use std::time::{Duration, Instant};
use uring_runtime::environment::now;

#[derive(Clone, Copy, Debug)]
pub struct Config {
    /// Zero disables the experiment without allocating queue state per request.
    pub active: usize,

    pub queued: usize,

    pub wait: Duration,
}
impl Default for Config {
    fn default() -> Self {
        Self {
            active: 0,
            queued: 256,
            wait: Duration::from_millis(1000),
        }
    }
}
impl Config {
    pub fn validate(self) -> Result<()> {
        if self.active > 65536
            || self.queued == 0
            || self.queued > 65536
            || self.wait.is_zero()
            || self.wait > Duration::from_secs(30)
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}

#[repr(align(64))]
pub(crate) struct Gate {
    config: Config,

    state: Mutex<State>,

    metrics: Metrics,
}
type State = flow_control::fifo::Fifo<RequestScope>;
pub(crate) struct Ticket {
    gate: Arc<Gate>,

    id: u64,

    deadline: Instant,

    charge: Option<Charge<AdmissionPolicy>>,

    started: Instant,

    terminal: bool,
}
pub(crate) struct Permit {
    gate: Arc<Gate>,

    _charge: Charge<AdmissionPolicy>,
}
impl Gate {
    #[cfg(test)]
    pub(crate) fn new(config: Config) -> Result<Arc<Self>> {
        Self::with_metrics(config, Metrics::default())
    }
    pub(crate) fn with_metrics(config: Config, metrics: Metrics) -> Result<Arc<Self>> {
        config.validate()?;
        Ok(Arc::new(Self {
            config,
            state: Mutex::new(State::default()),
            metrics,
        }))
    }
    /// Enqueue once. The caller retains the same ticket across pending polls.
    pub(crate) fn enter(
        self: &Arc<Self>,
        admission: &Quotas<AdmissionPolicy>,
        scope: &RequestScope,
    ) -> Result<Option<Ticket>> {
        scope.check()?;
        if self.config.active == 0 {
            return Ok(None);
        }
        let deadline = scope.deadline.0.min(now() + self.config.wait);
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        if state.queued() >= self.config.queued {
            self.metrics.record(Event::PeerReceiveFull, 1);
            return Err(Error::Overloaded);
        }
        let id = state.next().ok_or(Error::Overloaded)?;
        // Covers the queue slot/capacity slack, ticket, permit and wake/registration
        // bookkeeping. Payload and signed envelopes keep their existing charges.
        let charge = admission.reserve(None, ResourceClass::RequestContext, 4096)?;
        state.enqueue(id, scope.clone(), deadline);
        self.metrics
            .set_gauge(Gauge::PeerReceiveQueued, state.queued() as u64);
        Ok(Some(Ticket {
            gate: self.clone(),
            id,
            deadline,
            charge: Some(charge),
            started: now(),
            terminal: false,
        }))
    }
}
impl Gate {
    pub(crate) async fn acquire(
        self: &Arc<Self>,
        admission: &Quotas<AdmissionPolicy>,
        scope: &RequestScope,
    ) -> Result<Option<Arc<Permit>>> {
        let Some(mut ticket) = self.enter(admission, scope)? else {
            return Ok(None);
        };
        let registration = scope.cancellation.subscribe()?;
        std::future::poll_fn(|cx| {
            registration.register(cx.waker());
            ticket.poll(scope, cx)
        })
        .await
        .map(Some)
    }
    /// Every worker turn inspects bounded round-robin entries, even without CQEs.
    /// Wake the child explicitly; FuturesUnordered does not poll sleeping children.
    pub(crate) fn poll_deadlines(&self, budget: usize) {
        if self.config.active == 0 {
            return;
        }
        let wakes = {
            let mut state = self.state.lock().unwrap_or_else(|e| e.into_inner());
            state.poll_deadlines(budget, |scope, deadline| {
                now() >= deadline || scope.check().is_err()
            })
        };
        for wake in wakes {
            wake.wake();
        }
    }
}
impl Ticket {
    #[cfg(test)]
    pub(crate) fn deadline(&self) -> Instant {
        self.deadline
    }

    /// Register under the same lock that releases capacity, avoiding lost wakes.
    /// Scope checks include candidate idle/total limits, not just signed authority.
    pub(crate) fn poll(
        &mut self,
        scope: &RequestScope,
        cx: &mut Context<'_>,
    ) -> Poll<Result<Arc<Permit>>> {
        if let Err(error) = scope.check().and_then(|()| {
            if now() >= self.deadline {
                Err(Error::DeadlineExceeded)
            } else {
                Ok(())
            }
        }) {
            self.terminal = true;
            self.gate.metrics.record(
                if error == Error::Cancelled {
                    Event::PeerReceiveCancelled
                } else {
                    Event::PeerReceiveTimeout
                },
                1,
            );
            return Poll::Ready(Err(error));
        }
        let mut state = match self.gate.state.lock() {
            Ok(state) => state,
            Err(_) => return Poll::Ready(Err(Error::Unavailable)),
        };
        if state.can_admit(self.id, self.gate.config.active) {
            let Some(charge) = self.charge.take() else {
                return Poll::Ready(Err(Error::Internal));
            };
            let wake = state.admit(self.id);
            self.terminal = true;
            self.gate.metrics.record(Event::PeerReceiveAdmitted, 1);
            self.gate.metrics.record(
                Event::PeerReceiveWaitNs,
                now()
                    .saturating_duration_since(self.started)
                    .as_nanos()
                    .min(u64::MAX as u128) as u64,
            );
            self.gate
                .metrics
                .set_gauge(Gauge::PeerReceiveActive, state.active() as u64);
            self.gate
                .metrics
                .set_gauge(Gauge::PeerReceiveQueued, state.queued() as u64);
            drop(state);
            if let Some(wake) = wake {
                wake.wake();
            }
            return Poll::Ready(Ok(Arc::new(Permit {
                gate: self.gate.clone(),
                _charge: charge,
            })));
        }
        if !state.register(self.id, cx.waker()) {
            return Poll::Ready(Err(Error::Internal));
        }
        Poll::Pending
    }
}
impl Drop for Ticket {
    fn drop(&mut self) {
        let wake = {
            let mut state = self.gate.state.lock().unwrap_or_else(|e| e.into_inner());
            let wake = state.remove(self.id);
            self.gate
                .metrics
                .set_gauge(Gauge::PeerReceiveQueued, state.queued() as u64);
            wake
        };
        if !self.terminal {
            self.gate.metrics.record(Event::PeerReceiveCancelled, 1);
        }
        if let Some(wake) = wake {
            wake.wake();
        }
    }
}
impl Drop for Permit {
    fn drop(&mut self) {
        let wake = {
            let mut state = self.gate.state.lock().unwrap_or_else(|e| e.into_inner());
            let wake = state.release();
            self.gate
                .metrics
                .set_gauge(Gauge::PeerReceiveActive, state.active() as u64);
            wake
        };
        if let Some(wake) = wake {
            wake.wake();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::RequestId;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::task::Waker;
    struct Wake(AtomicUsize);
    impl std::task::Wake for Wake {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }
    #[test]
    fn receive_alarm_wakes_without_completion_and_preserves_scope() {
        use std::future::Future;
        let clock = uring_runtime::environment::SimulationClock::new(909);
        let _env = clock.environment(0).enter();
        let admission = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        for cause in ["queue", "idle", "total", "cancel"] {
            let metrics = Metrics::default();
            let gate = Gate::with_metrics(
                Config {
                    active: 1,
                    wait: Duration::from_secs(1),
                    ..Config::default()
                },
                metrics.clone(),
            )
            .unwrap();
            let parent =
                RequestScope::new(RequestId([4; 16]), now() + Duration::from_secs(10)).unwrap();
            let original = parent.deadline;
            let wake = Arc::new(Wake(AtomicUsize::new(0)));
            let waker = Waker::from(wake.clone());
            let mut cx = Context::from_waker(&waker);
            let mut held = Box::pin(gate.acquire(&admission, &parent));
            let Poll::Ready(Ok(Some(permit))) = held.as_mut().poll(&mut cx) else {
                panic!("grant");
            };
            drop(held);
            let scope = RequestScope::new(parent.request, parent.deadline.0).unwrap();
            if cause == "idle" {
                scope.set_candidate_idle(Duration::from_millis(10)).unwrap();
            }
            if cause == "total" {
                scope
                    .set_candidate_total(now() + Duration::from_millis(10))
                    .unwrap();
            }
            let mut pending = Box::pin(gate.acquire(&admission, &scope));
            assert!(pending.as_mut().poll(&mut cx).is_pending());
            assert_eq!(metrics.gauge(Gauge::PeerReceiveQueued), 1);
            let before = wake.0.load(Ordering::SeqCst);
            if cause == "cancel" {
                scope.cancel().unwrap();
            } else {
                clock.advance(if cause == "queue" {
                    Duration::from_secs(1)
                } else {
                    Duration::from_millis(11)
                });
                gate.poll_deadlines(1);
            }
            assert!(
                wake.0.load(Ordering::SeqCst) > before,
                "{cause}: no unrelated CQE required"
            );
            let expected = if cause == "cancel" {
                Error::Cancelled
            } else {
                Error::DeadlineExceeded
            };
            assert!(
                matches!(pending.as_mut().poll(&mut cx), Poll::Ready(Err(error)) if error == expected)
            );
            drop(pending);
            assert_eq!(scope.deadline.0, original.0);
            assert_eq!(metrics.gauge(Gauge::PeerReceiveQueued), 0);
            assert_eq!(metrics.gauge(Gauge::PeerReceiveActive), 1);
            assert_eq!(
                metrics.count(if cause == "cancel" {
                    Event::PeerReceiveCancelled
                } else {
                    Event::PeerReceiveTimeout
                }),
                1
            );
            drop(permit);
            assert_eq!(metrics.gauge(Gauge::PeerReceiveActive), 0);
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        }
    }

    #[test]
    fn receive_permit_is_not_retained_by_idle_http_state() {
        use http1::connection::State as _;
        let admission = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let gate = Gate::new(Config {
            active: 1,
            ..Config::default()
        })
        .unwrap();
        let scope = RequestScope::new(RequestId([5; 16]), now() + Duration::from_secs(5)).unwrap();
        let mut ticket = gate.enter(&admission, &scope).unwrap().unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let Poll::Ready(Ok(permit)) = ticket.poll(&scope, &mut cx) else {
            panic!("grant");
        };
        drop(ticket);
        let mut state = crate::http::State::default();
        state.attach(crate::http::State {
            receive_permit: Some(permit),
            ..Default::default()
        });
        assert_eq!(gate.state.lock().unwrap().active(), 1);
        let idle = state.idle();
        assert!(idle.receive_permit.is_none());
        assert_eq!(gate.state.lock().unwrap().active(), 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
    }
    #[test]
    fn receive_fifo_is_shared_bounded_and_completion_owned() {
        assert_eq!(std::mem::align_of::<Gate>(), 64);
        let admission = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let gate = Gate::new(Config {
            active: 1,
            queued: 2,
            ..Config::default()
        })
        .unwrap();
        let scope = RequestScope::new(RequestId([1; 16]), now() + Duration::from_secs(5)).unwrap();
        let wake = Arc::new(Wake(AtomicUsize::new(0)));
        let waker = Waker::from(wake.clone());
        let mut cx = Context::from_waker(&waker);
        let mut first = gate.enter(&admission, &scope).unwrap().unwrap();
        let Poll::Ready(Ok(permit)) = first.poll(&scope, &mut cx) else {
            panic!("first grant");
        };
        drop(first);
        let mut second = gate.enter(&admission, &scope).unwrap().unwrap();
        let mut third = gate.enter(&admission, &scope).unwrap().unwrap();
        assert!(matches!(
            gate.enter(&admission, &scope),
            Err(Error::Overloaded)
        ));
        assert!(second.poll(&scope, &mut cx).is_pending());
        assert!(third.poll(&scope, &mut cx).is_pending());
        let completion = permit.clone();
        drop(permit);
        assert!(second.poll(&scope, &mut cx).is_pending());
        std::thread::spawn(move || drop(completion)).join().unwrap();
        assert!(wake.0.load(Ordering::SeqCst) > 0);
        assert!(third.poll(&scope, &mut cx).is_pending(), "no barging");
        drop(second);
        let Poll::Ready(Ok(permit)) = third.poll(&scope, &mut cx) else {
            panic!("next after cancellation");
        };
        drop((third, permit));
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        assert_eq!(gate.state.lock().unwrap().active(), 0);
    }
    #[test]
    fn receive_disabled_cancel_and_expired_ticket_do_not_grant() {
        let admission = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let scope = RequestScope::new(RequestId([2; 16]), now() + Duration::from_secs(5)).unwrap();
        assert!(
            Gate::new(Config::default())
                .unwrap()
                .enter(&admission, &scope)
                .unwrap()
                .is_none()
        );
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        let gate = Gate::new(Config {
            active: 1,
            ..Config::default()
        })
        .unwrap();
        let mut ticket = gate.enter(&admission, &scope).unwrap().unwrap();
        assert!(ticket.deadline() <= scope.deadline.0);
        ticket.deadline = now();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(matches!(
            ticket.poll(&scope, &mut cx),
            Poll::Ready(Err(Error::DeadlineExceeded))
        ));
        drop(ticket);
        let mut ticket = gate.enter(&admission, &scope).unwrap().unwrap();
        scope.cancel().unwrap();
        assert!(matches!(
            ticket.poll(&scope, &mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
        drop(ticket);
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
    }

    #[test]
    fn receive_queue_pressure_and_middle_removal_preserve_fifo() {
        let admission = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let gate = Gate::new(Config {
            active: 1,
            queued: 3,
            ..Config::default()
        })
        .unwrap();
        let scope = RequestScope::new(RequestId([3; 16]), now() + Duration::from_secs(5)).unwrap();
        let held = admission
            .reserve(
                None,
                ResourceClass::RequestContext,
                admission.limit(ResourceClass::RequestContext),
            )
            .unwrap();
        assert!(matches!(
            gate.enter(&admission, &scope),
            Err(Error::Overloaded)
        ));
        assert_eq!(gate.state.lock().unwrap().queued(), 0);
        drop(held);
        let mut first = gate.enter(&admission, &scope).unwrap().unwrap();
        let middle = gate.enter(&admission, &scope).unwrap().unwrap();
        let mut last = gate.enter(&admission, &scope).unwrap().unwrap();
        drop(middle);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(last.poll(&scope, &mut cx).is_pending());
        let Poll::Ready(Ok(permit)) = first.poll(&scope, &mut cx) else {
            panic!("FIFO head");
        };
        drop((first, permit));
        let Poll::Ready(Ok(permit)) = last.poll(&scope, &mut cx) else {
            panic!("FIFO successor");
        };
        drop((last, permit));
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        assert_eq!(gate.state.lock().unwrap().queued(), 0);
    }
}
