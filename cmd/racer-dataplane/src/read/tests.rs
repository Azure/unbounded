use super::*;
use crate::admission::ResourceClass;
use crate::model::AttemptId;
use racer_control_wire::CacheId;

mod fill;

/// Keep a live cache graph for mailbox admission tests without activating listeners.
pub(super) fn admission_coordinator() -> (Rc<Coordinator>, impl Sized) {
    let f = fill::fixture();
    let (local, endpoint, _) = f.read_graph(
        Rc::new(f.fill.clone()),
        &f.membership,
        fill::ReadGraphSettings {
            name: "admission-mailbox",
            snapshots: 4,
            metadata: 16,
            window: 1,
            stall: std::time::Duration::from_secs(10),
            seed: None,
        },
    );
    (local, (endpoint, f))
}
mod hot_reads;
mod peer_copies;
mod remote;

pub(crate) fn page(byte: u8) -> crate::memory::PageResult {
    use crate::memory::CiphertextBytes;
    use crate::memory::CiphertextPage;
    use crate::memory::VerifiedBytes;
    use crate::memory::VerifiedPage;
    use crate::model::*;
    use std::sync::Arc;
    let admission = flow_control::Quotas::new(crate::admission::AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let page = PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([byte; 32]),
            },
            etag: StrongEtag::test_value("v1"),
        },
        number: PageNumber(0),
    };
    crate::memory::PageResult {
        metadata: ObjectMetadata {
            version: page.version.clone(),
            length: 1,
            content_type: None,
            expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
        },
        plaintext: VerifiedPage {
            inner: Arc::new(VerifiedBytes {
                page: page.clone(),
                storage: flow_control::ChargedBytes {
                    bytes: vec![byte],
                    reservation: admission
                        .reserve(None, ResourceClass::Plaintext, 1)
                        .unwrap(),
                },
            }),
        },
        ciphertext: CiphertextPage {
            provenance: None,
            inner: Arc::new(CiphertextBytes {
                checksum: Default::default(),
                envelope: PageEnvelope {
                    page,
                    key_id: key_id_from_generation(1, 1).unwrap(),
                    nonce: Nonce([0; 24]),
                    plaintext_length: 1,
                    ciphertext_length: 17,
                },
                storage: flow_control::ChargedBytes {
                    bytes: vec![0; 17],
                    reservation: admission
                        .reserve(None, ResourceClass::Ciphertext, 17)
                        .unwrap(),
                },
            }),
        },
    }
}
#[test]
fn racer_capacity_and_reservation_errors_are_preserved() {
    use uring_runtime::drivers::*;
    assert!(matches!(
        reserve().map_err(Error::from),
        Err(Error::InvalidConfiguration)
    ));
    let queue = Rc::new(DriverQueue::new(1024));
    let _owner = queue.enter();
    let permits: Vec<_> = (0..1024).map(|_| reserve().unwrap()).collect();
    assert_eq!(queue.pending(), 1024);
    assert!(matches!(
        reserve().map_err(Error::from),
        Err(Error::Overloaded)
    ));
    drop(permits);
    assert_eq!(queue.pending(), 0);
}
#[test]
fn detached_operation_errors_release_capacity() {
    use uring_runtime::drivers::*;
    let queue = Rc::new(DriverQueue::new(1024));
    let _owner = queue.enter();
    reserve()
        .unwrap()
        .submit_detached(async { Err::<(), _>(Error::Io) });
    poll(
        &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
        1,
    );
    assert_eq!(pending(), 0);
}
#[test]
fn remote_budget_charges_final_incoming_link_and_never_restores_attempts() {
    let now = uring_runtime::environment::now();
    let scope = RequestScope::new(
        crate::model::RequestId([1; 16]),
        now + std::time::Duration::from_secs(60),
    )
    .unwrap();
    let mut route = crate::topology::RouteBudget {
        membership: racer_control_wire::MembershipVersion(1),
        request: scope.request,
        attempt: AttemptId([2; 16]),
        destination: racer_control_wire::NodeId("destination".into()),
        visited: vec![racer_control_wire::NodeId("sender".into())],
        remaining_links: 1,
        remaining_attempts: 0,
        deadline: scope.deadline,
    };
    let mut budget = inherited_budget(&route, &scope).unwrap();
    assert_eq!(budget.remaining_links(), 0);
    assert_eq!(
        budget.begin_attempt(now, scope.deadline.0),
        Err(Error::Unavailable)
    );
    route.remaining_links = 8;
    route.remaining_attempts = 3;
    route.deadline.0 = now + std::time::Duration::from_secs(5);
    let mut budget = inherited_budget(&route, &scope).unwrap();
    assert_eq!(budget.route_links(), 7);
    assert_eq!(
        budget.begin_attempt(now, scope.deadline.0),
        Ok(route.deadline.0)
    );
    assert_eq!(budget.remaining_attempts(), 2);
    route.remaining_links = 0;
    assert!(matches!(
        inherited_budget(&route, &scope),
        Err(Error::HopBudgetExhausted)
    ));
}
#[test]
fn peer_failures_are_not_copy_misses() {
    assert!(matches!(
        peer_error(Error::NotFound),
        Ok(PeerResponse::NotFound)
    ));
    assert!(matches!(
        peer_error(Error::VersionUnavailable),
        Ok(PeerResponse::VersionUnavailable)
    ));
    assert!(matches!(
        peer_error(Error::OriginRejected),
        Ok(PeerResponse::OriginRejected)
    ));
    assert!(matches!(
        peer_error(Error::Unauthorized),
        Err(Error::Unauthorized)
    ));
    assert!(matches!(
        peer_error(Error::OriginForbidden),
        Ok(PeerResponse::OriginForbidden)
    ));
    assert!(matches!(
        peer_error(Error::Overloaded),
        Ok(PeerResponse::Overloaded)
    ));
    assert!(matches!(
        peer_error(Error::CorruptRecord),
        Err(Error::CorruptRecord)
    ));
}

mod flight {
    use crate::admission::AdmissionPolicy;
    use crate::admission::ResourceClass;
    use crate::error::Error;
    use crate::error::Operation;
    use crate::error::Result;
    use crate::memory::PageResult;
    use crate::model::CacheKey;
    use crate::model::ObjectId;
    use crate::model::ObjectVersion;
    use crate::model::PageId;
    use crate::model::PageNumber;
    use crate::model::StrongEtag;
    use crate::read::flight::*;
    use crate::runtime::RequestScope;
    use crate::security::OriginContext;
    use racer_control_wire::CacheId;
    use std::rc::Rc;
    use std::task::Context;
    use std::task::Poll;
    use std::task::Waker;
    use std::time::Duration;
    use std::time::Instant;
    fn metric_lease() -> telemetry::Lease {
        crate::telemetry::Metrics::default()
            .lease(crate::telemetry::Gauge::ActiveFills)
            .unwrap()
    }
    mod lifecycle_tests {
        //! Waiter election, cancellation, and actual-completion fencing scenarios.
        use super::*;
        use crate::model::PageNumber;

        /// Expiry sampling is bounded and does not confuse table and quota ownership.
        #[test]
        fn expiry_snapshot_coverage_completion_and_external_credit() {
            for count in [0, 6, 9] {
                let flights = flights(FlightLimits {
                    entries: 16,
                    ..FlightLimits::default()
                });
                let (context, scope) = (origin(), scope());
                let mut budgets: Vec<_> = (0..count).map(|_| budget()).collect();
                let membership = std::sync::Arc::new(
                    crate::topology::Membership::validate(
                        racer_control_wire::MembershipVersion(1),
                        vec![],
                    )
                    .unwrap(),
                );
                let mut waiters = Vec::new();
                for (number, budget) in budgets.iter_mut().enumerate() {
                    let mut page = fence().page;
                    page.number = PageNumber(number as u64);
                    let JoinedFlight::Waiter(waiter) = flights
                        .join(page, membership.clone(), &context, &scope, budget)
                        .unwrap()
                    else {
                        panic!("waiter");
                    };
                    waiters.push(waiter);
                }
                let held = flights.reserve_copy_flight(&fence().page, &scope).unwrap();
                let now = uring_runtime::environment::now();
                let facts = flights.expiry_snapshot_without_ticket(now);
                assert_eq!(facts.total, count);
                assert_eq!(facts.entries.iter().flatten().count(), count.min(6));
                assert_eq!(facts.flight_used, count + 1);
                assert_eq!(facts.caller_head, None);
                assert_eq!(facts.caller_age_us, None);
                assert!(
                    facts
                        .entries
                        .iter()
                        .flatten()
                        .all(|entry| entry.stage.is_none())
                );
                if let Some(waiter) = waiters.first_mut() {
                    let leader = lead(waiter);
                    let diagnostic = flights.driver_diagnostic(&leader).unwrap();
                    diagnostic.set(DriverStage::CandidateCompositeCall);
                    let operation = flights.retain_operation(&leader, metric_lease()).unwrap();
                    let entry = flights.expiry_snapshot_without_ticket(now).entries[0].unwrap();
                    assert_eq!(entry.operations, 1);
                    assert_eq!(entry.completing, 0);
                    assert_eq!(entry.stage, Some(DriverStage::CandidateCompositeCall));
                    operation.complete().unwrap();
                    flights
                        .publish(leader, result(&flights, fence().page))
                        .unwrap();
                    let entry = flights.expiry_snapshot_without_ticket(now).entries[0].unwrap();
                    assert_eq!(entry.lifecycle, FlightState::Complete);
                    assert_eq!(entry.waiters, 1);
                    assert_eq!(entry.operations, 0);
                    assert_eq!(flights.admission.used(ResourceClass::Flight), count + 1);
                }
                drop((held, waiters));
                assert_eq!(flights.admission.used(ResourceClass::Flight), 0);
            }
        }

        /// A prior cap observation is not fresh evidence when expiry is finally polled.
        #[test]
        fn expiry_snapshot_prior_cap_can_be_free_at_expiry() {
            use std::future::Future;
            let clock = uring_runtime::environment::SimulationClock::new(742);
            let _environment = clock.environment(0).enter();
            let flights = flights(FlightLimits {
                entries: 1,
                ..FlightLimits::default()
            });
            let failures = crate::telemetry::Failures::default();
            flights
                .admission
                .policy()
                .set_observer(failures.observer(crate::model::WorkerId(0)));
            let (context, scope) = (origin(), scope());
            let (mut first_budget, mut next_budget) = (budget(), budget());
            let first = join(&flights, &context, &scope, &mut first_budget);
            let mut page = fence().page;
            page.number = PageNumber(1);
            let mut waiting = Box::pin(flights.join_wait(
                page,
                first.membership.clone(),
                &context,
                &scope,
                &mut next_budget,
            ));
            let mut cx = Context::from_waker(Waker::noop());
            assert!(waiting.as_mut().poll(&mut cx).is_pending());
            drop(first);
            clock.advance(Duration::from_millis(101));
            assert!(matches!(
                waiting.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Overloaded))
            ));
            let mut text = String::new();
            failures.write_flight_expiry(&mut text).unwrap();
            assert!(
                text.contains("total_entries=0 sampled=0 omitted=0"),
                "{text}"
            );
            assert!(text.contains("overshoot_us=1000"), "{text}");
            assert!(text.contains("queue_len=1"), "{text}");
            assert!(text.contains("flight_used=0"), "{text}");
            text.clear();
            failures.write_gate_events(&mut text).unwrap();
            assert!(
                text.contains("last_observed_cause=Some(EntryCap)"),
                "{text}"
            );
        }

        /// Original scope expiry wins over the allowance and emits no expiry snapshot.
        #[test]
        fn expiry_snapshot_scope_deadline_is_excluded() {
            use std::future::Future;
            let clock = uring_runtime::environment::SimulationClock::new(743);
            let _environment = clock.environment(0).enter();
            let flights = flights(FlightLimits {
                entries: 1,
                ..FlightLimits::default()
            });
            let failures = crate::telemetry::Failures::default();
            flights
                .admission
                .policy()
                .set_observer(failures.observer(crate::model::WorkerId(0)));
            let (context, owner_scope) = (origin(), scope());
            let short = RequestScope::new(
                crate::model::RequestId([3; 16]),
                uring_runtime::environment::now() + Duration::from_millis(50),
            )
            .unwrap();
            let (mut active_budget, mut next_budget) = (budget(), budget());
            let active = join(&flights, &context, &owner_scope, &mut active_budget);
            let mut page = fence().page;
            page.number = PageNumber(1);
            let mut waiting = Box::pin(flights.join_wait(
                page,
                active.membership.clone(),
                &context,
                &short,
                &mut next_budget,
            ));
            let mut cx = Context::from_waker(Waker::noop());
            assert!(waiting.as_mut().poll(&mut cx).is_pending());
            clock.advance(Duration::from_millis(101));
            assert!(matches!(
                waiting.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::DeadlineExceeded))
            ));
            let mut text = String::new();
            failures.write_flight_expiry(&mut text).unwrap();
            assert!(text.contains("total=0 retained=0"));
            text.clear();
            failures.write_gate_events(&mut text).unwrap();
            assert!(text.contains("reason=ScopeDeadline kind=Join events=1"));
        }

        /// Queue fairness cannot mask original budget errors or block ready copies.
        #[test]
        fn bounded_admission_absent_budget_precedes_fifo_with_existing_ready_control() {
            use std::future::Future;
            for partial in [false, true] {
                let flights = flights(FlightLimits {
                    entries: 1,
                    ..FlightLimits::default()
                });
                let (context, scope) = (origin(), scope());
                let (mut active_budget, mut queued_budget) = (budget(), budget());
                let mut active = join(&flights, &context, &scope, &mut active_budget);
                let membership = active.membership.clone();
                let mut absent = fence().page;
                absent.number = PageNumber(1);
                let mut queued = Box::pin(flights.join_wait(
                    absent.clone(),
                    membership.clone(),
                    &context,
                    &scope,
                    &mut queued_budget,
                ));
                let mut cx = Context::from_waker(Waker::noop());
                assert!(queued.as_mut().poll(&mut cx).is_pending());
                assert!(scope.check().is_ok());
                let mut expired = AcquisitionBudget::new(uring_runtime::environment::now(), 0, 8);
                assert!(matches!(
                    flights.join_for(
                        absent.clone(),
                        membership.clone(),
                        &context,
                        &scope,
                        &mut expired,
                        true
                    ),
                    Err(Error::DeadlineExceeded)
                ));
                let mut exhausted = AcquisitionBudget::new(scope.deadline.0, 0, 8);
                assert!(matches!(
                    flights.join_for(
                        absent.clone(),
                        membership.clone(),
                        &context,
                        &scope,
                        &mut exhausted,
                        true
                    ),
                    Err(Error::Unavailable)
                ));
                let mut live = budget();
                assert!(matches!(
                    flights.join_for(
                        absent,
                        membership.clone(),
                        &context,
                        &scope,
                        &mut live,
                        true
                    ),
                    Err(Error::Overloaded)
                ));
                let leader = lead(&mut active);
                let page = result(&flights, fence().page);
                if partial {
                    flights
                        .publish_acquired(
                            leader,
                            crate::memory::AcquiredPage::Ciphertext(
                                crate::memory::UnverifiedPage {
                                    copy: page.copy(),
                                    disk_token: None,
                                },
                            ),
                        )
                        .unwrap();
                    assert!(matches!(
                        flights.join_for(
                            fence().page,
                            membership,
                            &context,
                            &scope,
                            &mut expired,
                            false
                        ),
                        Ok(JoinedFlight::Ciphertext(_))
                    ));
                } else {
                    flights.publish(leader, page).unwrap();
                    assert!(matches!(
                        flights.join_for(
                            fence().page,
                            membership,
                            &context,
                            &scope,
                            &mut expired,
                            true
                        ),
                        Ok(JoinedFlight::Complete(_))
                    ));
                }
                assert_eq!(flights.admission_state().0, 1);
                drop((queued, active));
                assert_eq!(flights.admission_state(), (0, 0, false));
            }
        }

        /// Canceling the parked head notifies an already registered successor.
        #[test]
        fn bounded_admission_head_cancel_wakes_queued_successor() {
            use std::future::Future;
            let flights = flights(FlightLimits {
                entries: 1,
                ..FlightLimits::default()
            });
            let (context, scope, canceled) = (origin(), scope(), scope());
            let (mut active_budget, mut head_budget, mut tail_budget) =
                (budget(), budget(), budget());
            let active = join(&flights, &context, &scope, &mut active_budget);
            let membership = active.membership.clone();
            let mut page = fence().page;
            page.number = PageNumber(1);
            let mut head = Box::pin(flights.join_wait(
                page.clone(),
                membership.clone(),
                &context,
                &canceled,
                &mut head_budget,
            ));
            page.number = PageNumber(2);
            let mut tail =
                Box::pin(flights.join_wait(page, membership, &context, &scope, &mut tail_budget));
            let mut cx = Context::from_waker(Waker::noop());
            assert!(head.as_mut().poll(&mut cx).is_pending());
            let count = std::sync::Arc::new(crate::test_support::WakeCounter::default());
            let wake = Waker::from(count.clone());
            let mut tail_cx = Context::from_waker(&wake);
            assert!(tail.as_mut().poll(&mut tail_cx).is_pending());
            let before = count.count();
            canceled.cancel().unwrap();
            assert!(matches!(
                head.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
            drop(head);
            assert!(count.count() > before);
            assert!(tail.as_mut().poll(&mut tail_cx).is_pending());
            let before = count.count();
            drop(active);
            assert!(
                count.count() > before,
                "successor retains capacity notification"
            );
            let Poll::Ready(Ok(JoinedFlight::Waiter(joined))) = tail.as_mut().poll(&mut tail_cx)
            else {
                panic!("successor admission");
            };
            drop((tail, joined));
            assert_eq!(flights.admission_state(), (0, 0, false));
        }

        /// Real queue capacity rejects one caller and cancellation advances the head.
        #[test]
        fn bounded_admission_queue_overflow_and_head_cancel() {
            use std::future::Future;
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.queue_entries = std::num::NonZeroUsize::new(1).unwrap();
            let flights = Rc::new(
                Flights::with_limits(
                    Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits))),
                    crate::test_support::availability(),
                    FlightLimits {
                        entries: 1,
                        ..FlightLimits::default()
                    },
                )
                .unwrap(),
            );
            let failures = crate::telemetry::Failures::default();
            flights
                .admission
                .policy()
                .set_observer(failures.observer(crate::model::WorkerId(0)));
            let (context, scope, head_scope) = (origin(), scope(), scope());
            let (mut active_budget, mut head_budget, mut next_budget) =
                (budget(), budget(), budget());
            let active = join(&flights, &context, &scope, &mut active_budget);
            let membership = active.membership.clone();
            let mut page = fence().page;
            page.number = PageNumber(1);
            let mut head = Box::pin(flights.join_wait(
                page.clone(),
                membership.clone(),
                &context,
                &head_scope,
                &mut head_budget,
            ));
            let mut cx = Context::from_waker(Waker::noop());
            assert!(head.as_mut().poll(&mut cx).is_pending());
            let mut overflow = Box::pin(flights.join_wait(
                page.clone(),
                membership.clone(),
                &context,
                &scope,
                &mut next_budget,
            ));
            assert!(matches!(
                overflow.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Overloaded))
            ));
            drop(overflow);
            let mut output = String::new();
            failures.write_gate_events(&mut output).unwrap();
            assert!(output.contains("reason=QueueFull kind=Join events=1"));
            head_scope.cancel().unwrap();
            assert!(matches!(
                head.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
            drop(head);
            let mut next =
                Box::pin(flights.join_wait(page, membership, &context, &scope, &mut next_budget));
            assert!(next.as_mut().poll(&mut cx).is_pending());
            drop(active);
            let Poll::Ready(Ok(JoinedFlight::Waiter(joined))) = next.as_mut().poll(&mut cx) else {
                panic!("successor after cancellation");
            };
            drop((next, joined));
            assert_eq!(flights.admission_state(), (0, 0, false));
        }

        /// Immediate success creates no admission ticket, deadline, or capacity wake.
        #[test]
        fn bounded_admission_fast_path_and_cancel_cleanup() {
            use std::future::Future;
            let flights = flights(FlightLimits {
                entries: 1,
                ..FlightLimits::default()
            });
            let (context, scope, canceled) = (origin(), scope(), scope());
            let (mut first_budget, mut second_budget) = (budget(), budget());
            let membership = std::sync::Arc::new(
                crate::topology::Membership::validate(
                    racer_control_wire::MembershipVersion(1),
                    vec![],
                )
                .unwrap(),
            );
            let mut first = Box::pin(flights.join_wait(
                fence().page,
                membership.clone(),
                &context,
                &scope,
                &mut first_budget,
            ));
            let mut cx = Context::from_waker(Waker::noop());
            let Poll::Ready(Ok(JoinedFlight::Waiter(active))) = first.as_mut().poll(&mut cx) else {
                panic!("immediate admission");
            };
            assert_eq!(flights.admission_state(), (0, 0, false));
            let mut page = fence().page;
            page.number = PageNumber(1);
            let mut waiting = Box::pin(flights.join_wait(
                page.clone(),
                membership.clone(),
                &context,
                &canceled,
                &mut second_budget,
            ));
            assert!(waiting.as_mut().poll(&mut cx).is_pending());
            assert_eq!(flights.admission_state(), (1, 1, true));
            canceled.cancel().unwrap();
            assert!(matches!(
                flights.join(page, membership, &context, &canceled, &mut budget()),
                Err(Error::Cancelled)
            ));
            assert!(matches!(
                waiting.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
            drop(waiting);
            assert_eq!(flights.admission_state(), (0, 0, false));
            drop((first, active));
        }

        /// Shutdown cannot report drained while a queued caller still owns admission.
        #[test]
        fn bounded_admission_drain_waits_for_ticket_retirement() {
            use std::future::Future;
            let flights = flights(FlightLimits {
                entries: 1,
                ..FlightLimits::default()
            });
            let (context, scope) = (origin(), scope());
            let (mut first_budget, mut next_budget) = (budget(), budget());
            let active = join(&flights, &context, &scope, &mut first_budget);
            let mut page = fence().page;
            page.number = PageNumber(1);
            let mut waiting = Box::pin(flights.join_wait(
                page,
                active.membership.clone(),
                &context,
                &scope,
                &mut next_budget,
            ));
            let mut cx = Context::from_waker(Waker::noop());
            assert!(waiting.as_mut().poll(&mut cx).is_pending());
            drop(active);
            let mut drain = flights.drain(&scope);
            assert!(drain.as_mut().poll(&mut cx).is_pending());
            assert!(matches!(
                waiting.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
            drop(waiting);
            assert!(matches!(drain.as_mut().poll(&mut cx), Poll::Ready(Ok(()))));
            assert_eq!(flights.admission_state(), (0, 0, false));
        }

        /// Admission expiry is finite, wakes without I/O, and never renews the budget.
        #[test]
        fn bounded_admission_original_deadline_and_allowance_are_distinct() {
            use std::future::Future;
            for original_expires in [false, true] {
                let clock = uring_runtime::environment::SimulationClock::new(741);
                let _environment = clock.environment(0).enter();
                let now = uring_runtime::environment::now();
                let flights = flights(FlightLimits {
                    entries: 1,
                    ..FlightLimits::default()
                });
                let failures = crate::telemetry::Failures::default();
                flights
                    .admission
                    .policy()
                    .set_observer(failures.observer(crate::model::WorkerId(0)));
                let context = origin();
                let scope = RequestScope::new(
                    crate::model::RequestId([0; 16]),
                    now + Duration::from_secs(30),
                )
                .unwrap();
                let mut first_budget = AcquisitionBudget::new(scope.deadline.0, 3, 8);
                let first = join(&flights, &context, &scope, &mut first_budget);
                let original = now
                    + if original_expires {
                        Duration::from_millis(50)
                    } else {
                        Duration::from_secs(30)
                    };
                let mut budget = AcquisitionBudget::new(original, 3, 8);
                let mut page = fence().page;
                page.number = PageNumber(1);
                let mut waiting = Box::pin(flights.join_wait(
                    page,
                    first.membership.clone(),
                    &context,
                    &scope,
                    &mut budget,
                ));
                let count = std::sync::Arc::new(crate::test_support::WakeCounter::default());
                let waker = Waker::from(count.clone());
                let mut cx = Context::from_waker(&waker);
                assert!(waiting.as_mut().poll(&mut cx).is_pending());
                clock.advance(Duration::from_millis(40));
                assert!(waiting.as_mut().poll(&mut cx).is_pending());
                if !original_expires {
                    clock.advance(Duration::from_millis(59));
                    assert!(waiting.as_mut().poll(&mut cx).is_pending());
                }
                let before = count.count();
                clock.advance(Duration::from_millis(if original_expires { 61 } else { 1 }));
                flights.poll_with_context(&mut cx, 8).unwrap();
                assert!(count.count() > before);
                let expected = if original_expires {
                    Error::DeadlineExceeded
                } else {
                    Error::Overloaded
                };
                assert!(
                    matches!(waiting.as_mut().poll(&mut cx), Poll::Ready(Err(error)) if error == expected)
                );
                drop(waiting);
                assert_eq!(budget.deadline(), original);
                assert_eq!(budget.remaining_attempts(), 3);
                assert_eq!(budget.remaining_links(), 8);
                let mut events = String::new();
                failures.write_gate_events(&mut events).unwrap();
                let reason = if original_expires {
                    "BudgetDeadline"
                } else {
                    "AllowanceExpired"
                };
                assert!(
                    events.contains(&format!("reason={reason} kind=Join events=1")),
                    "{events}"
                );
                assert!(events.contains("last_observed_cause=Some(EntryCap) cause_time=prior_observation_not_terminal_capacity"));
                assert!(events.contains("total=1 retained=1 overwritten=0"));
                let mut expiry = String::new();
                failures.write_flight_expiry(&mut expiry).unwrap();
                if original_expires {
                    assert!(expiry.contains("total=0 retained=0"), "{expiry}");
                } else {
                    assert!(expiry.contains("total=1 retained=1"), "{expiry}");
                    assert!(
                        expiry.contains("total_entries=1 sampled=1 omitted=0 truncated=false"),
                        "{expiry}"
                    );
                    assert!(expiry.contains("overshoot_us=0"), "{expiry}");
                    assert!(expiry.contains("caller_head=Some(true)"), "{expiry}");
                    assert!(expiry.contains("caller_age_us=Some(100000)"), "{expiry}");
                }
                drop(first);
                assert_eq!(flights.admission.used(ResourceClass::Waiter), 0);
                assert_eq!(flights.admission.used(ResourceClass::Flight), 0);
            }
        }

        /// A queued follower joins its page without waiting behind a different page.
        #[test]
        fn bounded_admission_queued_same_page_bypasses_unrelated_head() {
            use std::future::Future;
            let flights = flights(FlightLimits {
                entries: 1,
                ..FlightLimits::default()
            });
            let (context, scope) = (origin(), scope());
            let (mut active_budget, mut first_budget, mut other_budget, mut same_budget) =
                (budget(), budget(), budget(), budget());
            let active = join(&flights, &context, &scope, &mut active_budget);
            let membership = active.membership.clone();
            let mut page = fence().page;
            page.number = PageNumber(1);
            let mut other_page = page.clone();
            other_page.number = PageNumber(2);
            let mut first = Box::pin(flights.join_wait(
                page.clone(),
                membership.clone(),
                &context,
                &scope,
                &mut first_budget,
            ));
            let mut other = Box::pin(flights.join_wait(
                other_page,
                membership.clone(),
                &context,
                &scope,
                &mut other_budget,
            ));
            let mut same =
                Box::pin(flights.join_wait(page, membership, &context, &scope, &mut same_budget));
            let mut cx = Context::from_waker(Waker::noop());
            assert!(first.as_mut().poll(&mut cx).is_pending());
            assert!(other.as_mut().poll(&mut cx).is_pending());
            assert!(same.as_mut().poll(&mut cx).is_pending());
            drop(active);
            let Poll::Ready(Ok(JoinedFlight::Waiter(one))) = first.as_mut().poll(&mut cx) else {
                panic!("FIFO head");
            };
            assert!(other.as_mut().poll(&mut cx).is_pending());
            let Poll::Ready(Ok(JoinedFlight::Waiter(two))) = same.as_mut().poll(&mut cx) else {
                panic!("same page must coalesce");
            };
            assert_eq!(flights.admission.used(ResourceClass::Flight), 1);
            assert_eq!(flights.table.borrow().len(), 1);
            drop((first, same, other, one, two));
            assert_eq!(flights.admission_state(), (0, 0, false));
        }

        /// A parked admission cannot release a leader's accepted completion fence.
        #[test]
        fn bounded_admission_waits_for_actual_completion_and_transfers_waiter() {
            use std::future::Future;
            let flights = flights(FlightLimits {
                entries: 1,
                ..FlightLimits::default()
            });
            let (context, scope) = (origin(), scope());
            let (mut first_budget, mut next_budget) = (budget(), budget());
            let mut first = join(&flights, &context, &scope, &mut first_budget);
            let membership = first.membership.clone();
            let leader = lead(&mut first);
            let operation = flights.retain_operation(&leader, metric_lease()).unwrap();
            let mut page = fence().page;
            page.number = PageNumber(1);
            let count = std::sync::Arc::new(crate::test_support::WakeCounter::default());
            let waker = Waker::from(count.clone());
            let mut cx = Context::from_waker(&waker);
            let mut waiting =
                Box::pin(flights.join_wait(page, membership, &context, &scope, &mut next_budget));
            assert!(waiting.as_mut().poll(&mut cx).is_pending());
            assert_eq!(flights.admission.used(ResourceClass::Waiter), 2);
            drop((leader, first));
            assert!(waiting.as_mut().poll(&mut cx).is_pending());
            assert_eq!(flights.admission.used(ResourceClass::Flight), 1);
            let before = count.count();
            operation.complete().unwrap();
            assert!(count.count() > before);
            let Poll::Ready(Ok(JoinedFlight::Waiter(next))) = waiting.as_mut().poll(&mut cx) else {
                panic!("completion must admit the queued page");
            };
            assert_eq!(flights.admission.used(ResourceClass::Waiter), 1);
            drop((waiting, next));
            assert_eq!(flights.admission.used(ResourceClass::Waiter), 0);
            assert_eq!(flights.admission.used(ResourceClass::Flight), 0);
        }

        /// New reads cannot barge, but callers for an existing page still coalesce.
        #[test]
        fn bounded_admission_fifo_drop_and_existing_page_bypass() {
            use std::future::Future;
            let flights = flights(FlightLimits {
                entries: 1,
                ..FlightLimits::default()
            });
            let (context, scope) = (origin(), scope());
            let (mut active_budget, mut head_budget, mut tail_budget, mut joined_budget) =
                (budget(), budget(), budget(), budget());
            let active = join(&flights, &context, &scope, &mut active_budget);
            let membership = active.membership.clone();
            let mut page = fence().page;
            page.number = PageNumber(1);
            let mut head = Box::pin(flights.join_wait(
                page.clone(),
                membership.clone(),
                &context,
                &scope,
                &mut head_budget,
            ));
            page.number = PageNumber(2);
            let mut tail = Box::pin(flights.join_wait(
                page.clone(),
                membership.clone(),
                &context,
                &scope,
                &mut tail_budget,
            ));
            let mut cx = Context::from_waker(Waker::noop());
            assert!(head.as_mut().poll(&mut cx).is_pending());
            assert!(tail.as_mut().poll(&mut cx).is_pending());
            let joined = join(&flights, &context, &scope, &mut joined_budget);
            drop((joined, active));
            assert!(tail.as_mut().poll(&mut cx).is_pending());
            assert!(matches!(
                flights.join(page, membership, &context, &scope, &mut joined_budget),
                Err(Error::Overloaded)
            ));
            drop(head);
            let Poll::Ready(Ok(JoinedFlight::Waiter(next))) = tail.as_mut().poll(&mut cx) else {
                panic!("dropping the head must admit its successor");
            };
            drop((tail, next));
            assert_eq!(flights.admission.used(ResourceClass::Waiter), 0);
            assert_eq!(flights.admission.used(ResourceClass::Flight), 0);
        }

        #[test]
        fn detached_copy_waiters_release_notifications_without_losing_live_wake() {
            use futures::Stream;
            use futures::stream::FuturesUnordered;
            let flights = flights(FlightLimits::default());
            let (context, supplier, readers) = (origin(), scope(), scope());
            let mut budget = budget();
            let mut supplier = join(&flights, &context, &supplier, &mut budget);
            let leader = lead(&mut supplier);
            let operation = flights.retain_operation(&leader, metric_lease()).unwrap();
            for _ in 0..1100 {
                let mut copy = match flights.join_copy(&fence().page, &readers).unwrap() {
                    JoinedCopy::Waiter(waiter) => waiter,
                    _ => panic!("expected live flight"),
                };
                let mut tasks = FuturesUnordered::new();
                tasks.push(copy.wait());
                assert!(
                    std::pin::Pin::new(&mut tasks)
                        .poll_next(&mut Context::from_waker(futures::task::noop_waker_ref()))
                        .is_pending()
                );
                drop(tasks);
                drop(copy);
            }
            let mut first = match flights.join_copy(&fence().page, &readers).unwrap() {
                JoinedCopy::Waiter(waiter) => waiter,
                _ => panic!("expected live flight"),
            };
            let mut second = match flights.join_copy(&fence().page, &readers).unwrap() {
                JoinedCopy::Waiter(waiter) => waiter,
                _ => panic!("expected live flight"),
            };
            let count = std::sync::Arc::new(crate::test_support::WakeCounter::default());
            let waker = Waker::from(count.clone());
            let mut cx = Context::from_waker(&waker);
            assert!(first.wait().as_mut().poll(&mut cx).is_pending());
            assert!(second.wait().as_mut().poll(&mut cx).is_pending());
            drop(first);
            readers.cancel().unwrap();
            assert_eq!(
                count.count(),
                1,
                "dropping a sibling must not remove the shared executor wake"
            );
            assert!(matches!(
                second.wait().as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
            drop(second);
            assert_eq!(flights.admission.used(ResourceClass::ControlProgress), 1);
            operation.complete().unwrap();
            flights
                .fail(leader, AcquisitionFailure::Terminal(Error::Io))
                .unwrap();
            drop(supplier);
            assert_eq!(flights.admission.used(ResourceClass::Flight), 0);
            assert_eq!(flights.admission.used(ResourceClass::Waiter), 0);
        }

        #[test]
        fn worker_poll_drives_owned_completion_after_waiter_disappears() {
            let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
            let _owner = queue.enter();
            let flights = flights(FlightLimits::default());
            let (context, scope) = (origin(), scope());
            let mut a_budget = budget();
            let mut a = join(&flights, &context, &scope, &mut a_budget);
            let leader = lead(&mut a);
            let operation = flights.retain_operation(&leader, metric_lease()).unwrap();
            let (send, receive) = futures::channel::oneshot::channel::<()>();
            let worker = flights.clone();
            uring_runtime::drivers::reserve()
                .unwrap()
                .submit_detached(Box::pin(async move {
                    receive.await.map_err(|_| Error::Io)?;
                    operation.complete()?;
                    assert_eq!(
                        worker.fail(leader, AcquisitionFailure::Terminal(Error::Io)),
                        Err(Error::StaleFlight)
                    );
                    Ok::<_, Error>(())
                }));
            drop(a);
            flights.poll_budgeted(1).unwrap();
            assert_eq!(flights.table.borrow().len(), 1);
            send.send(()).unwrap();
            flights.poll_budgeted(1).unwrap();
            assert!(flights.table.borrow().is_empty());
            assert_eq!(uring_runtime::drivers::pending(), 0);
        }

        #[test]
        fn deadline_sweep_wakes_waiter_without_io_and_does_not_expire_other_callers() {
            use std::sync::Arc;
            let flights = flights(FlightLimits::default());
            let (context, scope_a, scope_b) = (origin(), scope(), scope());
            let (mut a_budget, mut b_budget) = (budget(), budget());
            let mut a = join(&flights, &context, &scope_a, &mut a_budget);
            let mut b = join(&flights, &context, &scope_b, &mut b_budget);
            let leader = lead(&mut a);
            let count = Arc::new(crate::test_support::WakeCounter::default());
            let waker = Waker::from(count.clone());
            assert!(
                b.wait()
                    .as_mut()
                    .poll(&mut Context::from_waker(&waker))
                    .is_pending()
            );
            assert!(flights.next_deadline().is_some());
            // Advance just this registration's effective deadline without sleeping.
            flights
                .table
                .borrow_mut()
                .get_mut(&fence().page)
                .unwrap()
                .waiters
                .get_mut(&b.registration.id)
                .unwrap()
                .budget_deadline = Instant::now();
            {
                let mut table = flights.table.borrow_mut();
                let entry = table.get_mut(&fence().page).unwrap();
                entry
                    .deadlines
                    .retain(|(_, id), _| *id != b.registration.id);
                entry
                    .deadlines
                    .insert((Instant::now(), b.registration.id), ());
            }
            flights.poll_budgeted(1).unwrap();
            assert!(count.count() > 0);
            failed(&mut b, Error::DeadlineExceeded);
            assert!(a.acquisition(&leader).is_ok());
        }

        #[test]
        fn rejected_credentials_retry_only_after_completion_with_independent_budget() {
            let flights = flights(FlightLimits::default());
            let (context, scope_a, scope_b) = (origin(), scope(), scope());
            let (mut a_budget, mut b_budget) = (budget(), budget());
            let mut a = join(&flights, &context, &scope_a, &mut a_budget);
            let mut b = join(&flights, &context, &scope_b, &mut b_budget);
            let leader = lead(&mut a);
            let old_fence = leader.fence.clone();
            a.acquisition(&leader)
                .unwrap()
                .budget
                .begin_peer_attempt(Instant::now(), scope_a.deadline.0, 3)
                .unwrap();
            assert!(poll(b.wait()).is_pending());
            let operation = flights.retain_operation(&leader, metric_lease()).unwrap();
            let id = operation.id;
            assert_eq!(
                flights.fail(leader, AcquisitionFailure::OriginRejected),
                Ok(FlightState::Draining)
            );
            failed(&mut a, Error::OriginRejected);
            assert!(poll(b.wait()).is_pending());
            assert!(operation.cancellation_requested());
            operation.complete().unwrap();
            let retry = lead(&mut b);
            assert!(retry.fence.generation > old_fence.generation);
            assert_eq!(
                b.acquisition(&retry).unwrap().budget.remaining_attempts(),
                3
            );
            assert_eq!(
                flights.complete_operation(&old_fence, id),
                Err(Error::StaleFlight)
            );
            let stale = FlightLeader {
                flights: flights.clone(),
                fence: old_fence,
                caller: a.registration.id,
                active: true,
            };
            assert_eq!(
                flights.fail(stale, AcquisitionFailure::Terminal(Error::Io)),
                Err(Error::StaleFlight)
            );
            assert!(b.acquisition(&retry).is_ok());
            flights
                .fail(retry, AcquisitionFailure::Terminal(Error::Io))
                .unwrap();
            failed(&mut b, Error::Io);
            drop(a);
            assert_eq!(a_budget.remaining_attempts(), 2);
            assert_eq!(a_budget.remaining_links(), 5);
        }

        #[test]
        fn drop_leader_retains_resources_until_actual_completion_without_ticket() {
            use crate::telemetry::Gauge;
            use crate::telemetry::Metrics;
            let flights = flights(FlightLimits::default());
            let (context, scope_a, scope_b) = (origin(), scope(), scope());
            let (mut a_budget, mut b_budget) = (budget(), budget());
            let mut a = join(&flights, &context, &scope_a, &mut a_budget);
            let mut b = join(&flights, &context, &scope_b, &mut b_budget);
            let leader = lead(&mut a);
            let old_diagnostic = flights.driver_diagnostic(&leader).unwrap();
            old_diagnostic.set(DriverStage::CandidateCompositeCall);
            let metrics = Metrics::default();
            let operation = flights
                .retain_operation(&leader, metrics.lease(Gauge::ActiveFills).unwrap())
                .unwrap();
            drop(leader);
            drop(a);
            flights.poll_budgeted(1).unwrap();
            assert_eq!(metrics.gauge(Gauge::ActiveFills), 1);
            assert_eq!(
                flights
                    .table
                    .borrow()
                    .get(&fence().page)
                    .unwrap()
                    .phase
                    .state(),
                FlightState::Draining
            );
            assert!(poll(b.wait()).is_pending());
            operation.complete().unwrap();
            assert_eq!(metrics.gauge(Gauge::ActiveFills), 0);
            let retry = lead(&mut b);
            let new_diagnostic = flights.driver_diagnostic(&retry).unwrap();
            new_diagnostic.set(DriverStage::DiskReadCall);
            old_diagnostic.set(DriverStage::WorkReturned);
            assert_eq!(
                flights
                    .expiry_snapshot_without_ticket(uring_runtime::environment::now())
                    .entries[0]
                    .unwrap()
                    .stage,
                Some(DriverStage::DiskReadCall)
            );
            drop(retry);
        }

        #[test]
        fn cancellation_reaps_independently_and_copy_never_elects() {
            let flights = flights(FlightLimits::default());
            let (context, scope_a, copy_scope) = (origin(), scope(), scope());
            let mut a_budget = budget();
            assert!(matches!(
                flights.join_copy(&fence().page, &copy_scope),
                Ok(JoinedCopy::Miss)
            ));
            let mut a = join(&flights, &context, &scope_a, &mut a_budget);
            let leader = lead(&mut a);
            let operation = flights.retain_operation(&leader, metric_lease()).unwrap();
            let mut copy = match flights.join_copy(&fence().page, &copy_scope).unwrap() {
                JoinedCopy::Waiter(waiter) => waiter,
                _ => panic!("expected copy waiter"),
            };
            assert!(poll(copy.wait()).is_pending());
            scope_a.cancel().unwrap();
            flights.poll_budgeted(1).unwrap();
            assert!(operation.cancellation_requested());
            failed(&mut a, Error::Cancelled);
            assert!(poll(copy.wait()).is_pending());
            operation.complete().unwrap();
            assert!(matches!(
                poll(copy.wait()),
                Poll::Ready(Err(Error::Unavailable))
            ));
            assert_eq!(
                flights.fail(leader, AcquisitionFailure::OriginRejected),
                Err(Error::StaleFlight)
            );
        }

        #[test]
        fn join_preserves_occupied_flight_until_operation_drains() {
            let flights = flights(FlightLimits::default());
            let (context, first_scope, next_scope) = (origin(), scope(), scope());
            let (mut first_budget, mut next_budget) = (budget(), budget());
            let mut first = join(&flights, &context, &first_scope, &mut first_budget);
            let leader = lead(&mut first);
            let incarnation = leader.fence.incarnation;
            let operation = flights.retain_operation(&leader, metric_lease()).unwrap();
            drop(leader);
            drop(first);
            let mut next = join(&flights, &context, &next_scope, &mut next_budget);
            assert!(poll(next.wait()).is_pending());
            assert_eq!(flights.admission.used(ResourceClass::Flight), 1);
            assert_eq!(flights.admission.used(ResourceClass::ControlProgress), 1);
            operation.complete().unwrap();
            assert_eq!(flights.admission.used(ResourceClass::ControlProgress), 0);
            let replacement = lead(&mut next);
            assert_eq!(replacement.fence.incarnation, incarnation);
            drop(replacement);
            drop(next);
            assert!(flights.table.borrow().is_empty());
            assert_eq!(flights.admission.used(ResourceClass::Flight), 0);
            assert_eq!(flights.admission.used(ResourceClass::Waiter), 0);
        }

        #[test]
        fn waiter_replacement_drops_waker_outside_flight_borrow() {
            thread_local! {
                static ON_DROP: std::cell::RefCell<Option<Box<dyn FnOnce()>>> =
                    std::cell::RefCell::new(None);
            }
            struct Reenter;
            // This waker exercises its Drop callback, not notification behavior.
            #[allow(clippy::manual_noop_waker)]
            impl std::task::Wake for Reenter {
                fn wake(self: std::sync::Arc<Self>) {}
            }
            impl Drop for Reenter {
                fn drop(&mut self) {
                    let callback = ON_DROP.with(|slot| slot.borrow_mut().take());
                    if let Some(callback) = callback {
                        callback();
                    }
                }
            }
            for copy in [false, true] {
                let flights = flights(FlightLimits::default());
                let (context, first_scope, next_scope) = (origin(), scope(), scope());
                let (mut first_budget, mut next_budget) = (budget(), budget());
                let mut first = join(&flights, &context, &first_scope, &mut first_budget);
                let leader = lead(&mut first);
                let mut next = join(&flights, &context, &next_scope, &mut next_budget);
                let JoinedCopy::Waiter(mut copy_waiter) =
                    flights.join_copy(&fence().page, &next_scope).unwrap()
                else {
                    panic!("expected copy waiter");
                };
                let mut wait: Operation<'_, ()> = if copy {
                    Box::pin(async { copy_waiter.wait().await.map(|_| ()) })
                } else {
                    Box::pin(async { next.wait().await.map(|_| ()) })
                };
                let waker = Waker::from(std::sync::Arc::new(Reenter));
                assert!(
                    wait.as_mut()
                        .poll(&mut Context::from_waker(&waker))
                        .is_pending()
                );
                drop(waker);
                let called = Rc::new(std::cell::Cell::new(false));
                ON_DROP.with(|slot| {
                    let flights = flights.clone();
                    let called = called.clone();
                    *slot.borrow_mut() = Some(Box::new(move || {
                        assert!(flights.table.try_borrow_mut().is_ok());
                        called.set(true);
                    }));
                });
                assert!(
                    wait.as_mut()
                        .poll(&mut Context::from_waker(Waker::noop()))
                        .is_pending()
                );
                assert!(called.get());
                drop(wait);
                drop((copy_waiter, next, leader, first));
                assert!(flights.table.borrow().is_empty());
            }
        }

        #[test]
        fn table_and_incarnation_fences_prevent_replacement_corruption() {
            let first = flights(FlightLimits::default());
            let other = flights(FlightLimits::default());
            let (context, scope) = (origin(), scope());
            let (mut a_budget, mut b_budget) = (budget(), budget());
            let mut a = join(&first, &context, &scope, &mut a_budget);
            let leader = lead(&mut a);
            let old = leader.fence.clone();
            let caller = leader.caller;
            assert_eq!(
                other.fail(leader, AcquisitionFailure::OriginRejected),
                Err(Error::StaleFlight)
            );
            a.detach().unwrap();
            let mut b = join(&first, &context, &scope, &mut b_budget);
            let leader = lead(&mut b);
            assert_ne!(old.incarnation, leader.fence.incarnation);
            let stale = FlightLeader {
                flights: first.clone(),
                fence: old,
                caller,
                active: true,
            };
            assert_eq!(
                first.fail(stale, AcquisitionFailure::Terminal(Error::Io)),
                Err(Error::StaleFlight)
            );
            assert!(b.acquisition(&leader).is_ok());
        }

        #[test]
        fn entry_waiter_operation_and_generation_caps_are_enforced() {
            let flights = flights(FlightLimits {
                entries: 1,
                waiters_per_flight: 2,
                operations_per_flight: 1,
                generations_per_flight: 1,
            });
            let (context, scope) = (origin(), scope());
            let failures = crate::telemetry::Failures::default();
            flights
                .admission
                .policy()
                .set_observer(failures.observer(crate::model::WorkerId(3)));
            let (mut a_budget, mut b_budget, mut extra_budget) = (budget(), budget(), budget());
            let mut a = join(&flights, &context, &scope, &mut a_budget);
            let mut b = join(&flights, &context, &scope, &mut b_budget);
            assert!(matches!(
                flights.join_copy(&fence().page, &scope),
                Err(Error::Overloaded)
            ));
            let mut another_page = fence().page;
            another_page.number = PageNumber(1);
            assert!(matches!(
                flights.join(
                    another_page,
                    a.membership.clone(),
                    &context,
                    &scope,
                    &mut extra_budget
                ),
                Err(Error::Overloaded)
            ));
            let leader = lead(&mut a);
            let operation = flights.retain_operation(&leader, metric_lease()).unwrap();
            assert!(matches!(
                flights.retain_operation(&leader, metric_lease()),
                Err(Error::Overloaded)
            ));
            flights
                .fail(leader, AcquisitionFailure::OriginRejected)
                .unwrap();
            operation.complete().unwrap();
            failed(&mut b, Error::Unavailable);
            drop(a);
            drop(b);
            assert!(flights.table.borrow().is_empty());
            assert_eq!(flights.admission.used(ResourceClass::Flight), 0);
            assert_eq!(flights.admission.used(ResourceClass::Waiter), 0);
            assert_eq!(flights.admission.used(ResourceClass::ControlProgress), 0);
            let mut events = String::new();
            failures.write_gate_events(&mut events).unwrap();
            assert!(
                events.contains("reason=EntryCap kind=Join events=1"),
                "{events}"
            );
            assert!(
                events.contains("reason=WaiterCap kind=JoinCopy events=1"),
                "{events}"
            );
            assert!(events.contains("worker=3"));
            assert!(events.contains("gate=EntryCap copy=false used=Some(1) limit=Some(1)"));
        }

        #[test]
        fn gate_join_quota_facts_and_existing_failure_are_distinct() {
            let flights = flights(FlightLimits::default());
            let failures = crate::telemetry::Failures::default();
            flights
                .admission
                .policy()
                .set_observer(failures.observer(crate::model::WorkerId(4)));
            let (context, scope) = (origin(), scope());
            let mut first_budget = budget();
            let mut first = join(&flights, &context, &scope, &mut first_budget);
            let used = flights.admission.used(ResourceClass::Waiter);
            let held = flights
                .admission
                .reserve(
                    None,
                    ResourceClass::Waiter,
                    flights.admission.limit(ResourceClass::Waiter) - used,
                )
                .unwrap();
            assert!(matches!(
                flights.join_copy(&fence().page, &scope),
                Err(Error::Overloaded)
            ));
            let mut extra_budget = budget();
            assert!(matches!(
                flights.join(
                    fence().page,
                    first.membership.clone(),
                    &context,
                    &scope,
                    &mut extra_budget
                ),
                Err(Error::Overloaded)
            ));
            drop(held);
            let held = flights
                .admission
                .reserve(
                    None,
                    ResourceClass::Flight,
                    flights.admission.limit(ResourceClass::Flight)
                        - flights.admission.used(ResourceClass::Flight),
                )
                .unwrap();
            let mut other = fence().page;
            other.number = PageNumber(1);
            assert!(matches!(
                flights.join(
                    other,
                    first.membership.clone(),
                    &context,
                    &scope,
                    &mut extra_budget
                ),
                Err(Error::Overloaded)
            ));
            drop(held);
            let leader = lead(&mut first);
            flights
                .fail(leader, AcquisitionFailure::Terminal(Error::Overloaded))
                .unwrap();
            let mut second_budget = budget();
            assert!(matches!(
                flights.join(
                    fence().page,
                    first.membership.clone(),
                    &context,
                    &scope,
                    &mut second_budget
                ),
                Err(Error::Overloaded)
            ));
            let mut output = String::new();
            failures.write_gate_events(&mut output).unwrap();
            assert!(
                output.contains("reason=WaiterQuota kind=JoinCopy events=1"),
                "{output}"
            );
            assert!(
                output.contains("quota=Some(Resource { class: Waiter"),
                "{output}"
            );
            assert!(
                output.contains("reason=ExistingFailed kind=Join events=1"),
                "{output}"
            );
            assert!(output.contains("reason=EntryCap kind=Join events=0"));
            assert!(
                output.contains("reason=WaiterQuota kind=Join events=1"),
                "{output}"
            );
            assert!(
                output.contains("reason=FlightQuota kind=Join events=1"),
                "{output}"
            );
            assert!(output.contains("quota=Some(Resource { class: Flight"));
        }

        #[test]
        fn abandoned_drain_observer_and_expired_shutdown_cannot_release_work() {
            let flights = flights(FlightLimits::default());
            let (context, scope) = (origin(), scope());
            let mut a_budget = budget();
            let mut a = join(&flights, &context, &scope, &mut a_budget);
            let leader = lead(&mut a);
            let operation = flights.retain_operation(&leader, metric_lease()).unwrap();
            drop(leader);
            assert!(operation.cancellation_requested());
            assert!(matches!(
                flights.table.borrow().get(&fence().page).unwrap().phase,
                Phase::Draining(_)
            ));
            let expired =
                RequestScope::new(crate::model::RequestId([1; 16]), Instant::now()).unwrap();
            assert!(matches!(
                poll(flights.drain(&expired)),
                Poll::Ready(Err(Error::DeadlineExceeded))
            ));
            assert_eq!(flights.table.borrow().len(), 1);
            assert!(matches!(
                flights.join_copy(&fence().page, &scope),
                Err(Error::Cancelled)
            ));
            operation.complete().unwrap();
            assert!(matches!(poll(flights.drain(&scope)), Poll::Ready(Ok(()))));
        }

        #[test]
        fn dropped_completion_token_does_not_claim_completion() {
            let flights = flights(FlightLimits::default());
            let (context, scope) = (origin(), scope());
            let mut a_budget = budget();
            let mut a = join(&flights, &context, &scope, &mut a_budget);
            let leader = lead(&mut a);
            let operation = flights.retain_operation(&leader, metric_lease()).unwrap();
            let (fence, id) = (operation.fence.clone(), operation.id);
            drop(operation);
            drop(leader);
            drop(a);
            flights.poll_budgeted(100).unwrap();
            assert_eq!(flights.table.borrow().len(), 1);
            // Simulate the worker reaping the actual completion, not request drop.
            flights.complete_operation(&fence, id).unwrap();
            assert!(flights.table.borrow().is_empty());
        }
    }

    #[test]
    fn failure_ceiling_is_inherited_without_allocating_additional_links() {
        let deadline = Instant::now() + std::time::Duration::from_secs(60);
        let mut original = AcquisitionBudget::new(deadline, 16, 24);
        assert_eq!(original.route_links(), 4);
        original
            .begin_peer_attempt(Instant::now(), deadline, 4)
            .unwrap();
        original.note_route_failure();
        assert_eq!(original.route_links(), 8);
        let mut child = original.partition(5, 8).unwrap();
        assert_eq!(child.route_links(), 8);
        child
            .begin_peer_attempt(Instant::now(), deadline, 8)
            .unwrap();
        original.reunite(child).unwrap();
        assert_eq!(original.remaining_links(), 12);
        assert_eq!(original.remaining_attempts(), 14);
        assert_eq!(original.deadline(), deadline);
        let moved = original.transfer();
        assert_eq!(moved.route_links(), 8);
        assert_eq!(original.remaining_links(), 0);
    }

    fn flights(limits: FlightLimits) -> Rc<Flights> {
        Rc::new(
            Flights::with_limits(
                Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                ))),
                crate::test_support::availability(),
                limits,
            )
            .unwrap(),
        )
    }
    fn scope() -> RequestScope {
        RequestScope::new(
            crate::model::RequestId([0; 16]),
            Instant::now() + Duration::from_secs(60),
        )
        .unwrap()
    }
    fn origin() -> OriginContext {
        OriginContext {
            object: fence().page.version.object,
            metadata: None,
            authorization: None,
        }
    }
    fn budget() -> AcquisitionBudget {
        AcquisitionBudget::new(Instant::now() + Duration::from_secs(60), 3, 8)
    }
    fn join<'a>(
        flights: &Rc<Flights>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> AcquisitionWaiter<'a> {
        let membership = std::sync::Arc::new(
            crate::topology::Membership::validate(racer_control_wire::MembershipVersion(1), vec![])
                .unwrap(),
        );
        match flights
            .join(fence().page, membership, context, scope, budget)
            .unwrap()
        {
            JoinedFlight::Waiter(waiter) => waiter,
            JoinedFlight::Complete(_) | JoinedFlight::Ciphertext(_) => {
                panic!("unexpected completed entry")
            }
        }
    }
    fn poll<T>(mut future: Operation<'_, T>) -> Poll<Result<T>> {
        future
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }
    fn lead(waiter: &mut AcquisitionWaiter<'_>) -> FlightLeader {
        match poll(waiter.wait()) {
            Poll::Ready(Ok(AcquisitionEvent::Lead(leader))) => leader,
            _ => panic!("expected election"),
        }
    }
    fn failed(waiter: &mut AcquisitionWaiter<'_>, expected: Error) {
        assert!(
            matches!(poll(waiter.wait()), Poll::Ready(Ok(AcquisitionEvent::Failed(error))) if error == expected)
        );
    }
    #[test]
    fn link_refund_is_bounded_and_transfers_do_not_duplicate_refund_rights() {
        let mut original = budget();
        assert_eq!(original.refund_links(1), Err(Error::InvalidRequest));
        original.charge_links(6).unwrap();
        let mut child = original.partition(1, 1).unwrap();
        assert_eq!(child.refund_links(1), Err(Error::InvalidRequest));
        let mut moved = original.transfer();
        assert_eq!(original.refund_links(1), Err(Error::InvalidRequest));
        moved.refund_links(2).unwrap();
        assert_eq!(moved.remaining_links(), 3);
        assert_eq!(moved.refund_links(5), Err(Error::InvalidRequest));
        assert_eq!(moved.remaining_links(), 3);
    }

    #[test]
    fn partition_transfer_and_peer_debits_conserve_original_credits() {
        let mut original = budget();
        let deadline = original.deadline();
        assert!(matches!(
            original.partition(2, 9),
            Err(Error::HopBudgetExhausted)
        ));
        assert_eq!(original.remaining_attempts(), 3);
        let mut child = original.partition(2, 5).unwrap();
        assert_eq!(
            (original.remaining_attempts(), original.remaining_links()),
            (1, 3)
        );
        assert_eq!(
            child.begin_peer_attempt(Instant::now(), deadline, 6),
            Err(Error::HopBudgetExhausted)
        );
        assert_eq!(child.remaining_attempts(), 2);
        assert_eq!(
            child.begin_peer_attempt(Instant::now(), deadline, 4),
            Ok(deadline)
        );
        let transferred = child.transfer();
        assert_eq!(
            (child.remaining_attempts(), child.remaining_links()),
            (0, 0)
        );
        assert_eq!(
            (
                transferred.remaining_attempts(),
                transferred.remaining_links()
            ),
            (1, 1)
        );
        assert_eq!(transferred.deadline(), deadline);
    }

    fn result(flights: &Flights, page: PageId) -> PageResult {
        use crate::memory::CiphertextBytes;
        use crate::memory::CiphertextPage;
        use crate::memory::VerifiedBytes;
        use crate::memory::VerifiedPage;
        use crate::model::ExpiresAt;
        use crate::model::Nonce;
        use crate::model::ObjectMetadata;
        use crate::model::PageEnvelope;
        use std::sync::Arc;
        PageResult {
            metadata: ObjectMetadata {
                content_type: None,
                version: page.version.clone(),
                length: 3,
                expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
            },
            plaintext: VerifiedPage {
                inner: Arc::new(VerifiedBytes {
                    page: page.clone(),
                    storage: flow_control::ChargedBytes {
                        bytes: vec![1, 2, 3],
                        reservation: flights
                            .admission
                            .reserve(None, ResourceClass::Plaintext, 3)
                            .unwrap(),
                    },
                }),
            },
            ciphertext: CiphertextPage {
                provenance: None,
                inner: Arc::new(CiphertextBytes {
                    checksum: std::sync::OnceLock::new(),
                    envelope: PageEnvelope {
                        page,
                        key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                        nonce: Nonce([0; 24]),
                        plaintext_length: 3,
                        ciphertext_length: 19,
                    },
                    storage: flow_control::ChargedBytes {
                        bytes: vec![0; 19],
                        reservation: flights
                            .admission
                            .reserve(None, ResourceClass::Ciphertext, 19)
                            .unwrap(),
                    },
                }),
            },
        }
    }

    #[test]
    fn validated_publication_waits_for_fences_and_shares_original_bundle() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let mut b = join(&flights, &context, &scope, &mut b_budget);
        let mut copy = match flights.join_copy(&fence().page, &scope).unwrap() {
            JoinedCopy::Waiter(waiter) => waiter,
            _ => panic!("expected copy"),
        };
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, metric_lease()).unwrap();
        let page = result(&flights, fence().page);
        assert_eq!(
            flights.publish(leader, page.clone()),
            Ok(FlightState::Draining)
        );
        assert!(poll(b.wait()).is_pending());
        assert!(poll(copy.wait()).is_pending());
        operation.complete().unwrap();
        let shared = match poll(b.wait()) {
            Poll::Ready(Ok(AcquisitionEvent::Complete(page))) => page,
            _ => panic!("expected complete"),
        };
        assert!(std::sync::Arc::ptr_eq(
            &shared.plaintext.inner,
            &page.plaintext.inner
        ));
        assert!(std::sync::Arc::ptr_eq(
            &shared.ciphertext.inner,
            &page.ciphertext.inner
        ));
        let Poll::Ready(Ok(copy_result)) = poll(copy.wait()) else {
            panic!("copy completion")
        };
        let copy_result = copy_result.verified();
        assert!(std::sync::Arc::ptr_eq(
            &copy_result.plaintext.inner,
            &page.plaintext.inner
        ));
        assert!(std::sync::Arc::ptr_eq(
            &copy_result.ciphertext.inner,
            &page.ciphertext.inner
        ));
        let Poll::Ready(Ok(AcquisitionEvent::Complete(leader_result))) = poll(a.wait()) else {
            panic!("leader completion")
        };
        assert!(std::sync::Arc::ptr_eq(
            &leader_result.plaintext.inner,
            &page.plaintext.inner
        ));
        assert!(std::sync::Arc::ptr_eq(
            &leader_result.ciphertext.inner,
            &page.ciphertext.inner
        ));
        assert!(matches!(
            flights.join_copy(&fence().page, &scope),
            Ok(JoinedCopy::Complete(_))
        ));
        assert!(matches!(
            flights.join(
                fence().page,
                a.membership.clone(),
                &context,
                &scope,
                &mut budget()
            ),
            Ok(JoinedFlight::Complete(_))
        ));
        drop(a);
        drop(b);
        drop(copy);
        assert!(flights.table.borrow().is_empty());
        assert!(matches!(
            flights.join_copy(&fence().page, &scope),
            Ok(JoinedCopy::Miss)
        ));
        // Readers own their bundles independently of the flight entry.
        assert_eq!(shared.plaintext.bytes(), &[1, 2, 3]);
    }

    #[test]
    fn invalid_publication_abandons_supplier_and_forbidden_retries_independently() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget, mut c_budget) = (budget(), budget(), budget());
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let mut b = join(&flights, &context, &scope, &mut b_budget);
        let mut c = join(&flights, &context, &scope, &mut c_budget);
        let leader = lead(&mut a);
        let generation = leader.fence.generation;
        let mut bad = result(&flights, fence().page);
        bad.metadata.length = 4;
        assert_eq!(flights.publish(leader, bad), Err(Error::CorruptRecord));
        failed(&mut a, Error::Cancelled);
        let retry = lead(&mut b);
        assert!(retry.fence.generation > generation);
        assert_eq!(
            flights.fail(retry, AcquisitionFailure::OriginForbidden),
            Ok(FlightState::RetryPending)
        );
        failed(&mut b, Error::OriginForbidden);
        let retry = lead(&mut c);
        flights
            .fail(retry, AcquisitionFailure::Terminal(Error::Unauthorized))
            .unwrap();
        failed(&mut c, Error::Unauthorized);
    }

    #[test]
    fn route_refunds_cannot_duplicate_partitioned_or_transferred_credits() {
        let mut original = budget();
        original.charge_links(6).unwrap();
        let mut child = original.partition(1, 2).unwrap();
        assert_eq!(child.refund_links(1), Err(Error::InvalidRequest));
        let mut moved = original.transfer();
        assert_eq!(original.refund_links(1), Err(Error::InvalidRequest));
        assert_eq!(moved.refund_links(7), Err(Error::InvalidRequest));
        moved.refund_links(6).unwrap();
        assert_eq!(moved.refund_links(1), Err(Error::InvalidRequest));
        assert_eq!(moved.remaining_links() + child.remaining_links(), 8);
    }

    #[test]
    fn forbidden_retries_but_peer_unauthorized_is_terminal() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let mut b = join(&flights, &context, &scope, &mut b_budget);
        let leader = lead(&mut a);
        assert_eq!(
            flights.fail(leader, AcquisitionFailure::OriginForbidden),
            Ok(FlightState::RetryPending)
        );
        failed(&mut a, Error::OriginForbidden);
        let leader = lead(&mut b);
        assert_eq!(
            flights.fail(leader, AcquisitionFailure::Terminal(Error::Unauthorized)),
            Ok(FlightState::Failed)
        );
        failed(&mut b, Error::Unauthorized);
        failed(&mut a, Error::OriginForbidden);
    }

    #[test]
    fn worker_poll_drives_parent_owned_queue_after_caller_drop() {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _owner = queue.enter();
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let mut budget = budget();
        let mut a = join(&flights, &context, &scope, &mut budget);
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, metric_lease()).unwrap();
        let (send, recv) = futures::channel::oneshot::channel::<()>();
        uring_runtime::drivers::reserve()
            .unwrap()
            .submit_detached(Box::pin(async move {
                recv.await.map_err(|_| Error::Io)?;
                operation.complete()?;
                drop(leader);
                Ok::<_, Error>(())
            }));
        drop(a);
        flights.poll_budgeted(1).unwrap();
        assert_eq!(flights.table.borrow().len(), 1);
        send.send(()).unwrap();
        assert!(matches!(poll(flights.drain(&scope)), Poll::Ready(Ok(()))));
        assert_eq!(uring_runtime::drivers::pending(), 0);
    }

    fn fence() -> Fence {
        Fence {
            page: PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId(crate::test_support::security::CACHE.into()),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::test_value("v1"),
                },
                number: PageNumber(0),
            },
            identity: flow_control::coalesce::flight::Identity {
                owner: Rc::new(()),
                incarnation: 1,
                generation: 1,
            },
        }
    }

    #[test]
    fn fences_reject_other_tables_pages_recreated_entries_and_late_retries() {
        let current = fence();
        assert_eq!(current.validate(&current.clone()), Ok(()));
        let mut stale = current.clone();
        stale.owner = Rc::new(());
        assert_eq!(stale.validate(&current), Err(Error::StaleFlight));
        let mut stale = current.clone();
        stale.page.number = PageNumber(1);
        assert_eq!(stale.validate(&current), Err(Error::StaleFlight));
        let mut stale = current.clone();
        stale.incarnation += 1;
        assert_eq!(stale.validate(&current), Err(Error::StaleFlight));
        let mut stale = current.clone();
        stale.generation += 1;
        assert_eq!(stale.validate(&current), Err(Error::StaleFlight));
    }

    #[test]
    fn request_context_must_match_both_cache_and_key_before_join() {
        let page = fence().page;
        let mut context = OriginContext {
            object: page.version.object.clone(),
            metadata: None,
            authorization: None,
        };
        assert_eq!(validate_context(&page, &context), Ok(()));
        context.object.key = CacheKey([1; 32]);
        assert_eq!(
            validate_context(&page, &context),
            Err(Error::InvalidRequest)
        );
        context.object = page.version.object.clone();
        context.object.cache = CacheId("other".into());
        assert_eq!(
            validate_context(&page, &context),
            Err(Error::InvalidRequest)
        );
    }

    #[test]
    fn retry_debits_original_attempts_and_links_without_extending_deadline() {
        let now = Instant::now();
        let original = now + Duration::from_secs(5);
        let later = original + Duration::from_secs(10);
        let mut budget = AcquisitionBudget::new(original, 2, 4);
        assert_eq!(budget.begin_attempt(now, later), Ok(original));
        assert_eq!(budget.charge_links(3), Ok(()));
        assert_eq!(budget.charge_links(2), Err(Error::HopBudgetExhausted));
        assert_eq!(budget.charge_links(1), Ok(()));
        assert_eq!(budget.begin_attempt(now, later), Ok(original));
        assert_eq!(budget.begin_attempt(now, later), Err(Error::Unavailable));
        assert_eq!(budget.charge_links(1), Err(Error::HopBudgetExhausted));
    }

    #[test]
    fn remaining_callers_keep_independent_budgets_and_inclusive_deadlines() {
        let now = Instant::now();
        let deadline = now + Duration::from_secs(5);
        let mut first = AcquisitionBudget::new(deadline, 1, 0);
        let mut remaining = AcquisitionBudget::new(deadline, 1, 4);
        assert_eq!(first.begin_attempt(now, deadline), Ok(deadline));
        assert_eq!(first.begin_attempt(now, deadline), Err(Error::Unavailable));
        assert_eq!(
            remaining.begin_attempt(now, now),
            Err(Error::DeadlineExceeded)
        );
        // Expiry checks and failed charges do not consume unrelated credits.
        assert_eq!(remaining.begin_attempt(now, deadline), Ok(deadline));
        assert_eq!(remaining.charge_links(4), Ok(()));
        assert_eq!(
            first.begin_attempt(deadline, deadline),
            Err(Error::DeadlineExceeded)
        );
    }
}

mod timeouts {
    use crate::error::Error;
    use crate::error::Operation;
    use crate::error::Result;
    use crate::model::ExpiresAt;
    use crate::model::MetadataSelector;
    use crate::model::ObjectMetadata;
    use crate::model::ObjectVersion;
    use crate::model::PageNumber;
    use crate::model::RequestId;
    use crate::model::StrongEtag;
    use crate::peer::Requester;
    use crate::peer::forwarding::Forwarding;
    use crate::peer::forwarding::VerifiedResponse;
    use crate::peer::protocol::FetchMode;
    use crate::peer::protocol::Operation as PeerOperation;
    use crate::peer::protocol::PeerRequest;
    use crate::peer::protocol::PeerResponse;
    use crate::peer::protocol::Signatures;
    use crate::read::candidates::*;
    use crate::read::flight::AcquisitionBudget;
    use crate::runtime::RequestScope;
    use crate::security::OriginContext;
    use crate::test_support::security::network;
    use crate::topology::Candidates;
    use crate::topology::Member;
    use crate::topology::Membership;
    use racer_control_wire::MembershipVersion;
    use racer_control_wire::NodeId;
    use std::cell::Cell;
    use std::cell::RefCell;
    use std::future::Future;
    use std::future::poll_fn;
    use std::pin::Pin;
    use std::rc::Rc;
    use std::sync::Arc;
    use std::task::Context;
    use std::task::Poll;
    use std::time::Duration;
    use std::time::Instant;
    use uring_runtime::environment::Deadline;

    struct Call {
        scope: RequestScope,
        signed_deadline: Instant,
        subscription: Option<crate::peer::subscriptions::Subscription>,
        destination: NodeId,
        copy: bool,
        attempts: u32,
        links: u8,
    }

    struct Peers {
        calls: RefCell<Vec<Call>>,
        signers: Vec<Rc<Signatures>>,
        stalled: usize,
        fenced: Cell<bool>,
        late_success: bool,
    }

    impl Peers {
        fn reply(&self, request: PeerRequest) -> Result<VerifiedResponse> {
            let sender = Forwarding::new(
                self.signers
                    .iter()
                    .find(|s| s.node() == &request.route.visited[0])
                    .unwrap()
                    .clone(),
            );
            let receiver = Forwarding::new(
                self.signers
                    .iter()
                    .find(|s| s.node() == &request.route.destination)
                    .unwrap()
                    .clone(),
            );
            let metadata = ObjectMetadata {
                content_type: None,
                version: ObjectVersion {
                    object: request.origin.object.clone(),
                    etag: StrongEtag::test_value("fallback"),
                },
                length: 17,
                expires_at: ExpiresAt::test_time(
                    std::time::SystemTime::now() + Duration::from_secs(60),
                ),
            };
            let response = match &request.operation {
                PeerOperation::Subscribe { .. } | PeerOperation::Page { .. } => PeerResponse::Miss,
                _ => PeerResponse::Metadata(metadata),
            };
            let (signed, binding) = sender.sign_request(request)?;
            let admitted = receiver.verify_request(signed)?;
            let signed = receiver.sign_response(admitted.binding(), response)?;
            sender.verify_response(signed, &binding)
        }
    }

    impl Peers {
        fn direct_hedge_available(
            &self,
            _: &std::sync::Arc<crate::topology::Membership>,
            _: &NodeId,
        ) -> bool {
            false
        }
        fn request_direct<'a>(
            &'a self,
            _: PeerRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            _: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            panic!("completion-fence fixture does not admit direct hedges")
        }
        fn request<'a>(
            &'a self,
            request: PeerRequest,
            membership: std::sync::Arc<crate::topology::Membership>,
            scope: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            assert!(scope.deadline.0 <= request.route.deadline.0);
            assert_eq!(request.route.deadline.0, request.origin.scope().deadline.0);
            assert_eq!(scope.request, request.route.request);
            let (original, share) = scope.body_deadlines.expect("local diagnostic deadlines");
            assert_eq!(original, scope.deadline.0);
            assert!(original >= share);
            let copy = match &request.operation {
                PeerOperation::Subscribe { mode, .. }
                | PeerOperation::Bootstrap { mode, .. }
                | PeerOperation::Metadata { mode, .. }
                | PeerOperation::Page { mode, .. } => {
                    matches!(mode, FetchMode::CopyOnly)
                }
            };
            let stalled = self.calls.borrow().len() < self.stalled;
            self.calls.borrow_mut().push(Call {
                scope: scope.clone(),
                signed_deadline: request.route.deadline.0,
                subscription: match &request.operation {
                    PeerOperation::Subscribe { subscription, .. } => Some(subscription.clone()),
                    _ => None,
                },
                destination: request.route.destination.clone(),
                copy,
                attempts: request.route.remaining_attempts,
                links: request.route.remaining_links,
            });
            Box::pin(async move {
                let _membership = membership;
                if stalled {
                    // A successful result computed before timeout is still inadmissible
                    // if its transport completion fence arrives after the attempt ends.
                    let result = if self.late_success {
                        self.reply(request)
                    } else {
                        Err(Error::Cancelled)
                    };
                    poll_fn(|_| {
                        if scope.cancellation.is_cancelled() && self.fenced.get() {
                            Poll::Ready(())
                        } else {
                            Poll::Pending
                        }
                    })
                    .await;
                    result
                } else {
                    self.reply(request)
                }
            })
        }
    }

    struct Fixture {
        policy: CandidatePolicy,
        peers: Rc<Peers>,
        candidates: Candidates,
        context: OriginContext,
        scope: RequestScope,
        budget: AcquisitionBudget,
    }

    impl Fixture {
        fn bounded(mut self, request: u8, seconds: u64, links: u8) -> Self {
            let deadline = uring_runtime::environment::now() + Duration::from_secs(seconds);
            self.scope = RequestScope::new(RequestId([request; 16]), deadline).unwrap();
            self.budget = AcquisitionBudget::new(deadline, 16, links);
            self
        }
        fn new(rank: Option<usize>, stalled: usize, late_success: bool) -> Self {
            let (_, placement, context, _, credentials) = crate::read::candidates::tests::fixture();
            let signers = network(5);
            let membership = std::sync::Arc::new(
                Membership::validate(
                    MembershipVersion(1),
                    signers[..4]
                        .iter()
                        .enumerate()
                        .map(|(i, signer)| Member {
                            node: signer.node().clone(),
                            shares: std::num::NonZeroU32::new(4).unwrap(),
                            peer_endpoint: format!("127.0.0.1:{}", 8000 + i),
                            rails: vec![],
                            site: String::new(),
                        })
                        .collect(),
                )
                .unwrap(),
            );
            let candidates = placement
                .rank(membership, &context.object, PageNumber(0))
                .unwrap();
            let local = rank.map_or_else(
                || signers[4].node().clone(),
                |r| candidates.ordered[r].clone(),
            );
            let peers = Rc::new(Peers {
                calls: RefCell::new(vec![]),
                signers,
                stalled,
                fenced: Cell::new(true),
                late_success,
            });
            let policy = CandidatePolicy::new(
                local,
                placement,
                Requester::scripted(
                    peers.clone(),
                    Peers::direct_hedge_available,
                    Peers::request,
                    Peers::request_direct,
                ),
                credentials,
                Arc::new(controlplane::Published::new(
                    crate::control::Snapshot::retention(2),
                )),
            );
            let scope =
                RequestScope::new(RequestId([3; 16]), Instant::now() + Duration::from_secs(1))
                    .unwrap();
            let budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
            Self {
                policy,
                peers,
                candidates,
                context,
                scope,
                budget,
            }
        }

        fn operation(&self) -> PeerOperation {
            PeerOperation::Metadata {
                object: self.context.object.clone(),
                selector: MetadataSelector::Fresh,
                mode: FetchMode::Acquire,
            }
        }
    }

    fn poll<T>(future: Pin<&mut (impl Future<Output = T> + ?Sized)>) -> Poll<T> {
        future.poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }

    fn expire(deadline: Instant) {
        std::thread::sleep(
            deadline.saturating_duration_since(Instant::now()) + Duration::from_millis(1),
        );
    }

    #[test]
    fn continuously_slow_candidate_reserves_fenced_fallback_and_conserves_credits() {
        let clock = uring_runtime::environment::SimulationClock::new_at(
            94,
            Instant::now(),
            std::time::SystemTime::now(),
        );
        let _env = clock.environment(0).enter();
        let mut f = Fixture::new(None, 1, true).bounded(94, 30, 24);
        let original = f.scope.deadline.0;
        f.peers.fenced.set(false);
        let operation = f.operation();
        let mut resolve = Box::pin(f.policy.resolve_with_budget(
            f.candidates,
            &f.context,
            operation,
            &f.scope,
            &mut f.budget,
        ));
        assert!(poll(resolve.as_mut()).is_pending());
        let first = f.peers.calls.borrow()[0].scope.clone();
        first.candidate_body_progress(1, 1000).unwrap();
        for received in 2..=10 {
            clock.advance(Duration::from_secs(1));
            first.candidate_body_progress(received, 1000).unwrap();
            assert!(poll(resolve.as_mut()).is_pending());
        }
        clock.advance(Duration::from_secs(1));
        assert_eq!(
            first.candidate_body_progress(11, 1000),
            Err(Error::DeadlineExceeded)
        );
        assert!(poll(resolve.as_mut()).is_pending());
        assert!(first.cancellation.is_cancelled());
        assert_eq!(
            f.peers.calls.borrow().len(),
            1,
            "accepted I/O must fence first"
        );
        assert_eq!(f.scope.check(), Ok(()));
        f.peers.fenced.set(true);
        assert!(matches!(
            poll(resolve.as_mut()),
            Poll::Ready(Ok(CandidateResolution::Copy(_)))
        ));
        drop(resolve);
        let calls = f.peers.calls.borrow();
        assert_eq!(calls.len(), 2);
        assert!(calls.iter().all(|call| call.signed_deadline == original));
        assert_eq!(
            calls.iter().map(|call| call.links).collect::<Vec<_>>(),
            vec![4, 8]
        );
        assert_eq!(
            calls.iter().map(|call| call.attempts + 1).sum::<u32>() + f.budget.remaining_attempts(),
            16
        );
        assert_eq!(f.budget.remaining_links(), 12);
        assert_eq!(f.budget.deadline(), original);
        assert!(uring_runtime::environment::now() < original);
    }

    #[test]
    fn slow_body_without_alternative_or_failure_route_credit_keeps_original_ceiling() {
        for (opportunities, links) in [(1, 24), (3, 4)] {
            let clock = uring_runtime::environment::SimulationClock::new_at(
                95,
                Instant::now(),
                std::time::SystemTime::now(),
            );
            let _env = clock.environment(0).enter();
            let mut f = Fixture::new(None, 1, false).bounded(95, 30, links);
            let original = f.scope.deadline.0;
            f.peers.fenced.set(false);
            let operation = f.operation();
            let mut request = Box::pin(f.policy.request(
                &f.candidates.membership,
                &f.candidates.ordered[0],
                &f.context,
                &operation,
                FetchMode::Acquire,
                &f.scope,
                &mut f.budget,
                opportunities,
            ));
            assert!(poll(request.as_mut()).is_pending());
            let first = f.peers.calls.borrow()[0].scope.clone();
            first.candidate_body_progress(1, 1000).unwrap();
            for received in 2..=5 {
                clock.advance(Duration::from_secs(2));
                first.candidate_body_progress(received, 1000).unwrap();
                assert!(poll(request.as_mut()).is_pending());
            }
            // The original signed ceiling is retained, but known-body ETA now bounds
            // a trickle even without another affordable route. Expiry still fences.
            clock.advance(Duration::from_secs(2));
            assert_eq!(
                first.candidate_body_progress(6, 1000),
                Err(Error::DeadlineExceeded)
            );
            assert!(poll(request.as_mut()).is_pending());
            assert!(first.cancellation.is_cancelled());
            assert_eq!(first.deadline.0, original);
            assert_eq!(f.scope.check(), Ok(()));
            assert_eq!(f.peers.calls.borrow()[0].signed_deadline, original);
            f.peers.fenced.set(true);
            assert!(matches!(
                poll(request.as_mut()),
                Poll::Ready(Err(Error::Unavailable))
            ));
            drop(request);
            assert_eq!(f.peers.calls.borrow().len(), 1);
            assert_eq!(f.budget.remaining_links(), links - 4);
            assert_eq!(
                f.peers.calls.borrow()[0].attempts + 1 + f.budget.remaining_attempts(),
                16
            );
        }
    }

    #[test]
    fn known_healthy_body_outlives_share_with_or_without_affordable_fallback() {
        for links in [4, 24] {
            let clock = uring_runtime::environment::SimulationClock::new_at(
                100,
                Instant::now(),
                std::time::SystemTime::now(),
            );
            let _env = clock.environment(0).enter();
            let mut f = Fixture::new(None, 1, false).bounded(100, 30, links);
            let original = f.scope.deadline.0;
            let operation = f.operation();
            let mut request = Box::pin(f.policy.request(
                &f.candidates.membership,
                &f.candidates.ordered[0],
                &f.context,
                &operation,
                FetchMode::Acquire,
                &f.scope,
                &mut f.budget,
                3,
            ));
            assert!(poll(request.as_mut()).is_pending());
            let first = f.peers.calls.borrow()[0].scope.clone();
            first.candidate_body_progress(10, 80).unwrap();
            for received in (20..=80).step_by(10) {
                clock.advance(Duration::from_secs(2));
                first.candidate_body_progress(received, 80).unwrap();
                assert!(poll(request.as_mut()).is_pending());
                assert!(!first.cancellation.is_cancelled());
            }
            assert!(uring_runtime::environment::now() > first.body_deadlines.unwrap().1);
            assert_eq!(f.peers.calls.borrow()[0].signed_deadline, original);
            f.scope.cancel().unwrap();
            assert!(matches!(
                poll(request.as_mut()),
                Poll::Ready(Err(Error::Cancelled))
            ));
            drop(request);
            assert_eq!(f.peers.calls.borrow().len(), 1);
        }
    }

    #[test]
    fn configured_total_cap_never_renews_or_accepts_late_success() {
        let clock = uring_runtime::environment::SimulationClock::new_at(
            96,
            Instant::now(),
            std::time::SystemTime::now(),
        );
        let _env = clock.environment(0).enter();
        let mut f = Fixture::new(None, 1, true).bounded(96, 60, 24);
        let original = f.scope.deadline.0;
        f.policy = f.policy.with_attempt_timeout(Duration::from_secs(6));
        f.peers.fenced.set(false);
        let operation = f.operation();
        let mut request = Box::pin(f.policy.request(
            &f.candidates.membership,
            &f.candidates.ordered[0],
            &f.context,
            &operation,
            FetchMode::Acquire,
            &f.scope,
            &mut f.budget,
            1,
        ));
        assert!(poll(request.as_mut()).is_pending());
        let first = f.peers.calls.borrow()[0].scope.clone();
        for _ in 0..5 {
            clock.advance(Duration::from_secs(1));
            // Checkout/head or unknown-length progress cannot renew the total cap.
            first.candidate_progress().unwrap();
            assert!(poll(request.as_mut()).is_pending());
        }
        clock.advance(Duration::from_secs(1));
        assert_eq!(first.candidate_progress(), Err(Error::DeadlineExceeded));
        assert!(poll(request.as_mut()).is_pending());
        assert!(first.cancellation.is_cancelled());
        assert_eq!(f.scope.check(), Ok(()));
        assert_eq!(first.deadline.0, original);
        assert_eq!(f.peers.calls.borrow()[0].signed_deadline, original);
        f.peers.fenced.set(true);
        assert!(matches!(
            poll(request.as_mut()),
            Poll::Ready(Err(Error::Unavailable))
        ));
        drop(request);
        assert_eq!(f.peers.calls.borrow().len(), 1);
        assert_eq!(f.budget.remaining_links(), 20);
        assert_eq!(f.budget.remaining_attempts(), 0);
        assert_eq!(f.budget.deadline(), original);
    }

    #[test]
    fn candidate_final_early_credits_exhaustion_and_recovered_fallback() {
        for (credits, stalled, late, fails) in [(0, 0, false, true), (16, 1, true, false)] {
            let clock = uring_runtime::environment::SimulationClock::new(198);
            let _env = clock.environment(0).enter();
            let mut f = Fixture::new(None, stalled, late).bounded(198, 60, 24);
            f.budget = AcquisitionBudget::new(f.scope.deadline.0, credits, 24);
            let failures = crate::telemetry::Failures::default();
            f.policy = f
                .policy
                .with_observer(failures.observer(crate::model::WorkerId(4)))
                .with_attempt_timeout(Duration::from_secs(1));
            let operation = f.operation();
            let mut resolve = f.policy.resolve_with_budget(
                f.candidates,
                &f.context,
                operation,
                &f.scope,
                &mut f.budget,
            );
            if stalled != 0 {
                assert!(poll(resolve.as_mut()).is_pending());
                clock.advance(Duration::from_secs(2));
            }
            let Poll::Ready(result) = poll(resolve.as_mut()) else {
                panic!("bounded resolution")
            };
            assert_eq!(result.is_err(), fails);
            drop(resolve);
            let mut text = String::new();
            failures.write_candidate_final(&mut text).unwrap();
            if fails {
                assert!(matches!(result, Err(Error::Unavailable)));
                assert!(text.contains("total=1 retained=1"), "{text}");
                assert!(text.contains("site=Exhausted"), "{text}");
                assert!(
                    text.contains("attempt=none site=Prepare raw=None effective=Some(Unavailable)"),
                    "{text}"
                );
                assert!(f.peers.calls.borrow().is_empty());
                assert_eq!(f.budget.remaining_links(), 24);
            } else {
                assert!(text.contains("total=0 retained=0"), "{text}");
                assert_eq!(f.peers.calls.borrow().len(), 2);
                assert_eq!(f.budget.remaining_links(), 12);
            }
            assert_eq!(f.scope.check(), Ok(()));
        }
    }

    #[test]
    fn candidate_final_preserves_late_success_and_effective_deadline_error() {
        let clock = uring_runtime::environment::SimulationClock::new(199);
        let _env = clock.environment(0).enter();
        let mut f = Fixture::new(None, 3, true).bounded(199, 60, 24);
        let failures = crate::telemetry::Failures::default();
        f.policy = f
            .policy
            .with_observer(failures.observer(crate::model::WorkerId(4)))
            .with_attempt_timeout(Duration::from_secs(1));
        let operation = f.operation();
        let mut resolve = f.policy.resolve_with_budget(
            f.candidates,
            &f.context,
            operation,
            &f.scope,
            &mut f.budget,
        );
        for _ in 0..3 {
            assert!(poll(resolve.as_mut()).is_pending());
            clock.advance(Duration::from_secs(2));
        }
        assert!(matches!(
            poll(resolve.as_mut()),
            Poll::Ready(Err(Error::Unavailable))
        ));
        drop(resolve);
        let mut text = String::new();
        failures.write_candidate_final(&mut text).unwrap();
        assert!(text.contains("total=1 retained=1"), "{text}");
        assert_eq!(
            text.matches(
                "raw=Some(Success) effective=Some(Unavailable) stop=Some(DeadlineExceeded)"
            )
            .count(),
            3,
            "{text}"
        );
        assert_eq!(f.budget.remaining_attempts(), 0);
        assert_eq!(f.budget.remaining_links(), 4);
        assert_eq!(f.scope.check(), Ok(()));
    }

    #[test]
    fn candidate_final_remaining_copy_and_origin_miss_only_at_final_boundary() {
        let clock = uring_runtime::environment::SimulationClock::new(200);
        let _env = clock.environment(0).enter();
        let mut f = Fixture::new(Some(1), 1, true).bounded(200, 60, 24);
        let failures = crate::telemetry::Failures::default();
        f.policy = f
            .policy
            .with_observer(failures.observer(crate::model::WorkerId(4)))
            .with_attempt_timeout(Duration::from_secs(1));
        let operation = PeerOperation::Page {
            page: crate::model::PageId {
                version: ObjectVersion {
                    object: f.context.object.clone(),
                    etag: StrongEtag::test_value("secret-etag"),
                },
                number: PageNumber(0),
            },
            mode: FetchMode::Acquire,
        };
        let candidates = Candidates {
            membership: f.candidates.membership.clone(),
            ordered: f.candidates.ordered.clone(),
        };
        let mut resolve = f.policy.resolve_with_budget(
            candidates,
            &f.context,
            match &operation {
                PeerOperation::Page { page, .. } => PeerOperation::Page {
                    page: page.clone(),
                    mode: FetchMode::Acquire,
                },
                _ => unreachable!(),
            },
            &f.scope,
            &mut f.budget,
        );
        assert!(poll(resolve.as_mut()).is_pending());
        clock.advance(Duration::from_secs(2));
        let Poll::Ready(Ok(CandidateResolution::Origin(authority))) = poll(resolve.as_mut()) else {
            panic!("origin authority")
        };
        drop(resolve);
        let mut text = String::new();
        failures.write_candidate_final(&mut text).unwrap();
        assert!(text.contains("total=0 retained=0"));
        assert_eq!(f.policy.origin_miss_error(&authority), Error::Unavailable);
        assert_eq!(
            f.policy
                .final_origin_miss(&authority, &operation, &f.scope, &f.budget),
            Error::Unavailable
        );
        text.clear();
        failures.write_candidate_final(&mut text).unwrap();
        assert!(text.contains("site=OriginMiss"), "{text}");
        assert!(text.contains("stop=Some(DeadlineExceeded)"), "{text}");
        assert!(!text.contains("secret-etag"));
        f.budget = AcquisitionBudget::new(f.scope.deadline.0, 0, 24);
        let mut remaining = f.policy.remaining_copy(
            &f.candidates,
            &f.context,
            &operation,
            &f.scope,
            &mut f.budget,
        );
        assert!(matches!(
            poll(remaining.as_mut()),
            Poll::Ready(Err(Error::Unavailable))
        ));
        drop(remaining);
        text.clear();
        failures.write_candidate_final(&mut text).unwrap();
        assert!(text.contains("total=2 retained=2"), "{text}");
        assert!(text.contains("site=RemainingCopy"), "{text}");
        assert_eq!(f.budget.remaining_links(), 24);
    }

    #[test]
    fn retries_get_independent_local_caps_but_never_extend_overall_authority() {
        let clock = uring_runtime::environment::SimulationClock::new_at(
            101,
            Instant::now(),
            std::time::SystemTime::now(),
        );
        let _env = clock.environment(0).enter();
        let mut f = Fixture::new(None, 3, false).bounded(101, 15, 24);
        let original = f.scope.deadline.0;
        f.policy = f.policy.with_attempt_timeout(Duration::from_secs(6));
        let operation = f.operation();
        let mut resolve = Box::pin(f.policy.resolve_with_budget(
            f.candidates,
            &f.context,
            operation,
            &f.scope,
            &mut f.budget,
        ));
        assert!(poll(resolve.as_mut()).is_pending());
        for (index, seconds) in [6, 6, 3].into_iter().enumerate() {
            assert_eq!(f.peers.calls.borrow().len(), index + 1);
            let child = f.peers.calls.borrow()[index].scope.clone();
            for _ in 1..seconds {
                clock.advance(Duration::from_secs(1));
                child.candidate_progress().unwrap();
                assert!(poll(resolve.as_mut()).is_pending());
            }
            clock.advance(Duration::from_secs(1));
            assert_eq!(child.candidate_progress(), Err(Error::DeadlineExceeded));
            let result = poll(resolve.as_mut());
            if index < 2 {
                assert!(result.is_pending());
                assert_eq!(f.scope.check(), Ok(()));
            } else {
                assert!(matches!(result, Poll::Ready(Err(Error::DeadlineExceeded))));
            }
            assert!(child.cancellation.is_cancelled());
        }
        drop(resolve);
        let calls = f.peers.calls.borrow();
        assert!(
            calls
                .iter()
                .all(|call| call.signed_deadline == original && call.scope.deadline.0 == original)
        );
        assert_eq!(
            calls.iter().map(|call| call.attempts + 1).sum::<u32>() + f.budget.remaining_attempts(),
            16
        );
        assert_eq!(f.budget.remaining_links(), 4);
        assert_eq!(f.budget.deadline(), original);
        assert!(!f.scope.cancellation.is_cancelled());
    }

    #[test]
    fn subscription_stall_must_leave_time_for_fixed_page_fallback() {
        let mut f = Fixture::new(Some(1), 1, false);
        f.peers.fenced.set(false);
        let scheduler = crate::read::range_stream::Scheduler::new(1);
        let demand = crate::peer::subscriptions::Demand::new(vec![
            crate::peer::subscriptions::PageInterval { start: 0, end: 1 },
        ])
        .unwrap();
        let version = ObjectVersion {
            object: f.context.object.clone(),
            etag: StrongEtag::test_value("v1"),
        };
        let mut subscribe = Box::pin(f.policy.subscribe(
            version.clone(),
            demand.clone(),
            &scheduler,
            f.candidates.membership.clone(),
            &f.context,
            &f.scope,
            &mut f.budget,
        ));
        assert!(poll(subscribe.as_mut()).is_pending());
        let deadline = f.peers.calls.borrow()[0].scope.body_deadlines.unwrap().1;
        assert!(deadline < f.scope.deadline.0);
        assert_eq!(
            f.peers.calls.borrow()[0].signed_deadline,
            f.scope.deadline.0
        );
        expire(deadline);
        assert!(
            poll(subscribe.as_mut()).is_pending(),
            "accepted exchange is not fenced"
        );
        assert!(f.peers.calls.borrow()[0].scope.cancellation.is_cancelled());
        assert!(f.scope.check().is_ok());
        f.peers.fenced.set(true);
        assert!(matches!(poll(subscribe.as_mut()), Poll::Ready(Ok(None))));
        drop(subscribe);
        let operation = PeerOperation::Page {
            page: crate::model::PageId {
                version: version.clone(),
                number: PageNumber(0),
            },
            mode: FetchMode::Acquire,
        };
        let result = futures::executor::block_on(f.policy.resolve_with_budget(
            f.candidates.clone(),
            &f.context,
            operation,
            &f.scope,
            &mut f.budget,
        ))
        .unwrap();
        let CandidateResolution::Origin(authority) = result else {
            panic!("fallback authority")
        };
        authority
            .validate(&f.context.object, PageNumber(0))
            .unwrap();
        // Another selection keeps the original signed deadline and spent ceilings,
        // even though its local attempt starts later.
        assert!(
            futures::executor::block_on(f.policy.subscribe(
                version,
                demand,
                &scheduler,
                f.candidates.membership.clone(),
                &f.context,
                &f.scope,
                &mut f.budget,
            ))
            .unwrap()
            .is_none()
        );
        let calls = f.peers.calls.borrow();
        let first = calls[0].subscription.as_ref().unwrap();
        let next = calls[2].subscription.as_ref().unwrap();
        assert_eq!(first.id, next.id);
        assert_eq!(first.sequence + 1, next.sequence);
        assert_eq!(first.page_budget - 1, next.page_budget);
        assert_eq!(
            first.byte_budget - crate::model::PAGE_BYTES - 16,
            next.byte_budget
        );
        assert_eq!(calls[0].signed_deadline, calls[2].signed_deadline);
        let provider = std::sync::Arc::new(
            crate::peer::subscriptions::Subscriptions::new(Default::default()).unwrap(),
        );
        for (subscription, call) in [(first, &calls[0]), (next, &calls[2])] {
            let selected = provider
                .schedule(
                    subscription.clone(),
                    f.candidates.membership.version,
                    f.policy.node.clone(),
                    crate::peer::protocol::encode_deadline(Deadline(call.signed_deadline)).unwrap(),
                    crate::peer::protocol::millis(uring_runtime::environment::wall_now()).unwrap(),
                )
                .expect("later exchange must not renew the provider contract");
            let crate::peer::subscriptions::Selection::Leader { work, waiter } = selected else {
                panic!("independent completed attempts")
            };
            work.fail(Error::Unavailable);
            drop(waiter);
        }
        assert_eq!(
            calls.iter().map(|call| call.attempts + 1).sum::<u32>() + f.budget.remaining_attempts(),
            16
        );
        assert_eq!(
            calls.iter().map(|call| u32::from(call.links)).sum::<u32>()
                + u32::from(f.budget.remaining_links()),
            24
        );
    }

    #[test]
    fn subscription_parent_cancellation_waits_for_fence_without_fallback() {
        let mut f = Fixture::new(Some(1), 1, false);
        f.peers.fenced.set(false);
        let scheduler = crate::read::range_stream::Scheduler::new(1);
        let demand = crate::peer::subscriptions::Demand::new(vec![
            crate::peer::subscriptions::PageInterval { start: 0, end: 1 },
        ])
        .unwrap();
        let mut subscribe = Box::pin(f.policy.subscribe(
            ObjectVersion {
                object: f.context.object.clone(),
                etag: StrongEtag::test_value("v1"),
            },
            demand,
            &scheduler,
            f.candidates.membership.clone(),
            &f.context,
            &f.scope,
            &mut f.budget,
        ));
        assert!(poll(subscribe.as_mut()).is_pending());
        f.scope.cancel().unwrap();
        assert!(poll(subscribe.as_mut()).is_pending());
        assert!(f.peers.calls.borrow()[0].scope.cancellation.is_cancelled());
        f.peers.fenced.set(true);
        assert!(matches!(
            poll(subscribe.as_mut()),
            Poll::Ready(Err(Error::Cancelled))
        ));
        drop(subscribe);
        assert_eq!(f.peers.calls.borrow().len(), 1);
        assert_eq!(f.budget.remaining_links(), 20);
        assert_eq!(f.budget.remaining_attempts(), 7);
    }

    #[test]
    fn stalled_first_candidate_drains_before_healthy_fallback_without_new_credits() {
        let mut f = Fixture::new(None, 1, true);
        f.peers.fenced.set(false);
        let operation = f.operation();
        let mut resolve = Box::pin(f.policy.resolve_with_budget(
            f.candidates,
            &f.context,
            operation,
            &f.scope,
            &mut f.budget,
        ));
        assert!(poll(resolve.as_mut()).is_pending());
        let deadline = f.peers.calls.borrow()[0].scope.body_deadlines.unwrap().1;
        assert!(deadline < f.scope.deadline.0);
        assert_eq!(
            f.peers.calls.borrow()[0].scope.body_deadlines,
            Some((f.scope.deadline.0, deadline))
        );
        expire(deadline);
        assert!(poll(resolve.as_mut()).is_pending());
        assert_eq!(
            f.peers.calls.borrow().len(),
            1,
            "must wait for completion fence"
        );
        assert!(f.peers.calls.borrow()[0].scope.cancellation.is_cancelled());
        assert_eq!(f.scope.check(), Ok(()), "attempt must not cancel parent");
        f.peers.fenced.set(true);
        let Poll::Ready(Ok(CandidateResolution::Copy(response))) = poll(resolve.as_mut()) else {
            panic!("healthy fallback must succeed")
        };
        assert!(matches!(response.response(), PeerResponse::Metadata(m) if m.length == 17));
        drop(resolve);
        let calls = f.peers.calls.borrow();
        assert_eq!(calls.len(), 2, "late first success must be discarded");
        assert!(calls.iter().all(|c| !c.copy));
        assert_eq!(
            calls.iter().map(|c| c.links).collect::<Vec<_>>(),
            vec![4, 8]
        );
        assert_eq!(
            calls.iter().map(|c| c.attempts + 1).sum::<u32>() + f.budget.remaining_attempts(),
            16
        );
        assert_eq!(f.budget.remaining_links(), 12);
        assert_eq!(f.budget.deadline(), f.scope.deadline.0);
    }

    #[test]
    fn stalled_predecessors_leave_local_origin_time_and_credit() {
        let mut f = Fixture::new(Some(2), 2, false);
        let ordered = f.candidates.ordered.clone();
        let operation = f.operation();
        let mut resolve = Box::pin(f.policy.resolve_with_budget(
            f.candidates,
            &f.context,
            operation,
            &f.scope,
            &mut f.budget,
        ));
        assert!(poll(resolve.as_mut()).is_pending());
        expire(f.peers.calls.borrow()[0].scope.body_deadlines.unwrap().1);
        assert!(poll(resolve.as_mut()).is_pending());
        expire(f.peers.calls.borrow()[1].scope.body_deadlines.unwrap().1);
        let Poll::Ready(Ok(CandidateResolution::Origin(authority))) = poll(resolve.as_mut()) else {
            panic!("local origin must retain an opportunity")
        };
        authority
            .validate(&f.context.object, PageNumber(0))
            .unwrap();
        assert_eq!(f.policy.origin_miss_error(&authority), Error::Unavailable);
        drop(resolve);
        let calls = f.peers.calls.borrow();
        assert_eq!(
            calls
                .iter()
                .map(|c| c.destination.clone())
                .collect::<Vec<_>>(),
            ordered[..2]
        );
        assert!(calls.iter().all(|c| c.copy && c.attempts == 0));
        assert_eq!(f.budget.remaining_attempts(), 14);
        assert_eq!(f.budget.remaining_links(), 12);
        assert_eq!(f.scope.check(), Ok(()));
        assert_eq!(
            f.budget.begin_attempt(Instant::now(), f.scope.deadline.0),
            Ok(f.scope.deadline.0)
        );
    }

    #[test]
    fn remaining_copy_slices_time_for_later_healthy_copy() {
        let mut f = Fixture::new(Some(0), 1, false);
        let operation = f.operation();
        let mut resolve = Box::pin(f.policy.remaining_copy(
            &f.candidates,
            &f.context,
            &operation,
            &f.scope,
            &mut f.budget,
        ));
        assert!(poll(resolve.as_mut()).is_pending());
        let deadline = f.peers.calls.borrow()[0].scope.body_deadlines.unwrap().1;
        assert!(deadline < f.scope.deadline.0);
        expire(deadline);
        assert!(matches!(poll(resolve.as_mut()), Poll::Ready(Ok(Some(_)))));
        drop(resolve);
        assert_eq!(f.peers.calls.borrow().len(), 2);
        assert!(
            f.peers
                .calls
                .borrow()
                .iter()
                .all(|c| c.copy && c.attempts == 0)
        );
        assert_eq!(f.budget.remaining_attempts(), 14);
    }

    #[test]
    fn exhausted_overall_budget_is_terminal_even_while_parent_scope_is_live() {
        let mut f = Fixture::new(None, 3, false);
        let overall = Instant::now() + Duration::from_millis(120);
        f.budget = AcquisitionBudget::new(overall, 16, 24);
        let operation = f.operation();
        let mut resolve = Box::pin(f.policy.resolve_with_budget(
            f.candidates,
            &f.context,
            operation,
            &f.scope,
            &mut f.budget,
        ));
        assert!(poll(resolve.as_mut()).is_pending());
        assert_eq!(f.peers.calls.borrow()[0].scope.deadline.0, overall);
        expire(overall);
        assert!(matches!(
            poll(resolve.as_mut()),
            Poll::Ready(Err(Error::DeadlineExceeded))
        ));
        drop(resolve);
        assert_eq!(f.peers.calls.borrow().len(), 1);
        assert_eq!(f.budget.remaining_attempts(), 10);
        assert_eq!(f.budget.remaining_links(), 20);
        assert_eq!(f.scope.check(), Ok(()));
    }

    #[test]
    fn all_stalled_candidates_stop_at_original_deadline_without_refunding_credits() {
        let mut f = Fixture::new(None, 3, false);
        f.scope.deadline.0 = Instant::now() + Duration::from_millis(120);
        let operation = f.operation();
        let mut resolve = Box::pin(f.policy.resolve_with_budget(
            f.candidates,
            &f.context,
            operation,
            &f.scope,
            &mut f.budget,
        ));
        for index in 0..3 {
            assert!(poll(resolve.as_mut()).is_pending());
            let deadline = f.peers.calls.borrow()[index]
                .scope
                .body_deadlines
                .unwrap()
                .1;
            assert!(deadline <= f.scope.deadline.0);
            expire(deadline);
        }
        assert!(matches!(
            poll(resolve.as_mut()),
            Poll::Ready(Err(Error::DeadlineExceeded))
        ));
        drop(resolve);
        assert_eq!(f.peers.calls.borrow().len(), 3);
        assert_eq!(f.budget.remaining_attempts(), 0);
        assert_eq!(f.budget.remaining_links(), 4);
        assert!(!f.scope.cancellation.is_cancelled());
    }

    #[test]
    fn parent_cancellation_cancels_child_but_waits_for_fence_without_fallback() {
        let mut f = Fixture::new(None, 1, false);
        f.peers.fenced.set(false);
        let membership = std::sync::Arc::downgrade(&f.candidates.membership);
        let operation = f.operation();
        let mut resolve = Box::pin(f.policy.resolve_with_budget(
            f.candidates,
            &f.context,
            operation,
            &f.scope,
            &mut f.budget,
        ));
        assert!(poll(resolve.as_mut()).is_pending());
        f.scope.cancel().unwrap();
        assert!(poll(resolve.as_mut()).is_pending());
        assert!(f.peers.calls.borrow()[0].scope.cancellation.is_cancelled());
        assert!(
            membership.upgrade().is_some(),
            "cancellation is not a membership completion fence"
        );
        f.peers.fenced.set(true);
        assert!(matches!(
            poll(resolve.as_mut()),
            Poll::Ready(Err(Error::Cancelled))
        ));
        drop(resolve);
        assert!(
            membership.upgrade().is_none(),
            "completion releases the operation lease"
        );
        assert_eq!(f.peers.calls.borrow().len(), 1);
    }

    #[test]
    fn expired_budget_cannot_mint_local_origin_authority_or_spend_credits() {
        let mut f = Fixture::new(Some(0), 0, false);
        f.budget = AcquisitionBudget::new(Instant::now(), 16, 24);
        let operation = f.operation();
        let result = futures::executor::block_on(f.policy.resolve_with_budget(
            f.candidates,
            &f.context,
            operation,
            &f.scope,
            &mut f.budget,
        ));
        assert!(matches!(result, Err(Error::DeadlineExceeded)));
        assert!(f.peers.calls.borrow().is_empty());
        assert_eq!(f.budget.remaining_attempts(), 16);
        assert_eq!(f.budget.remaining_links(), 24);
    }
}
