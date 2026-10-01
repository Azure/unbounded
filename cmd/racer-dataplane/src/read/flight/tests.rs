mod lifecycle_tests {
    //! Waiter election, cancellation, and actual-completion fencing scenarios.
    use super::*;
    use crate::model::PageNumber;

    #[test]
    fn detached_copy_waiters_release_notifications_without_losing_live_wake() {
        use futures::{Stream, stream::FuturesUnordered};
        let flights = flights(FlightLimits::default());
        let (context, supplier, readers) = (origin(), scope(), scope());
        let mut budget = budget();
        let mut supplier = join(&flights, &context, &supplier, &mut budget);
        let leader = lead(&mut supplier);
        let operation = flights.retain_operation(&leader, ()).unwrap();
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
    fn complete_publication_waits_for_operations_and_shares_exact_bundle() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let mut b = join(&flights, &context, &scope, &mut b_budget);
        let mut copy = match flights.join_copy(&fence().page, &scope).unwrap() {
            JoinedCopy::Waiter(waiter) => waiter,
            _ => panic!("expected waiter"),
        };
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, ()).unwrap();
        let result = page_result(&flights);
        assert_eq!(
            flights.publish(leader, result.clone()),
            Ok(FlightState::Draining)
        );
        assert!(poll(b.wait()).is_pending());
        assert!(poll(copy.wait()).is_pending());
        operation.complete().unwrap();
        for waiter in [&mut a, &mut b] {
            match poll(waiter.wait()) {
                Poll::Ready(Ok(AcquisitionEvent::Complete(shared))) => {
                    assert!(std::ptr::eq(
                        shared.plaintext.bytes().as_ptr(),
                        result.plaintext.bytes().as_ptr()
                    ));
                    assert_eq!(shared.ciphertext.bytes(), result.ciphertext.bytes());
                }
                _ => panic!("expected complete bundle"),
            }
        }
        assert!(matches!(poll(copy.wait()), Poll::Ready(Ok(_))));
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
        drop(copy);
        drop(a);
        drop(b);
        assert!(flights.table.borrow().entries.is_empty());
    }

    #[test]
    fn malformed_publication_abandons_without_sharing_or_reusing_generation() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let mut b = join(&flights, &context, &scope, &mut b_budget);
        let leader = lead(&mut a);
        let generation = leader.fence.generation;
        let mut result = page_result(&flights);
        result.metadata.length = 4;
        assert_eq!(flights.publish(leader, result), Err(Error::CorruptRecord));
        failed(&mut a, Error::Cancelled);
        let retry = lead(&mut b);
        assert!(retry.fence.generation > generation);
    }

    #[test]
    fn worker_poll_drives_owned_completion_after_waiter_disappears() {
        let queue = Rc::new(crate::read::drivers::DriverQueue::default());
        let _owner = queue.enter();
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let mut a_budget = budget();
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, ()).unwrap();
        let (send, receive) = futures::channel::oneshot::channel::<()>();
        let worker = flights.clone();
        crate::read::drivers::spawn(Box::pin(async move {
            receive.await.map_err(|_| Error::Io)?;
            operation.complete()?;
            assert_eq!(
                worker.fail(leader, AcquisitionFailure::Terminal(Error::Io)),
                Err(Error::StaleFlight)
            );
            Ok(())
        }))
        .unwrap();
        drop(a);
        flights.poll_budgeted(1).unwrap();
        assert_eq!(flights.table.borrow().entries.len(), 1);
        send.send(()).unwrap();
        flights.poll_budgeted(1).unwrap();
        assert!(flights.table.borrow().entries.is_empty());
        assert_eq!(crate::read::drivers::pending(), 0);
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
            .entries
            .get_mut(&fence().page)
            .unwrap()
            .waiters
            .get_mut(&b.registration.id)
            .unwrap()
            .budget_deadline = Instant::now();
        {
            let mut table = flights.table.borrow_mut();
            let entry = table.entries.get_mut(&fence().page).unwrap();
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
        let operation = flights.retain_operation(&leader, ()).unwrap();
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
        struct Resource(Rc<std::cell::Cell<bool>>);
        impl Drop for Resource {
            fn drop(&mut self) {
                self.0.set(true);
            }
        }
        let flights = flights(FlightLimits::default());
        let (context, scope_a, scope_b) = (origin(), scope(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope_a, &mut a_budget);
        let mut b = join(&flights, &context, &scope_b, &mut b_budget);
        let leader = lead(&mut a);
        let released = Rc::new(std::cell::Cell::new(false));
        let operation = flights
            .retain_operation(&leader, Resource(released.clone()))
            .unwrap();
        drop(leader);
        drop(a);
        flights.poll_budgeted(1).unwrap();
        assert!(!released.get());
        assert_eq!(
            flights.table.borrow().entries[&fence().page].phase.state(),
            FlightState::Draining
        );
        assert!(poll(b.wait()).is_pending());
        operation.complete().unwrap();
        assert!(released.get());
        drop(lead(&mut b));
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
        let operation = flights.retain_operation(&leader, ()).unwrap();
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
        let operation = flights.retain_operation(&leader, ()).unwrap();
        assert!(matches!(
            flights.retain_operation(&leader, ()),
            Err(Error::Overloaded)
        ));
        flights
            .fail(leader, AcquisitionFailure::OriginRejected)
            .unwrap();
        operation.complete().unwrap();
        failed(&mut b, Error::Unavailable);
        drop(a);
        drop(b);
        assert!(flights.table.borrow().entries.is_empty());
        assert_eq!(flights.admission.used(ResourceClass::Flight), 0);
        assert_eq!(flights.admission.used(ResourceClass::Waiter), 0);
        assert_eq!(flights.admission.used(ResourceClass::ControlProgress), 0);
    }

    #[test]
    fn abandoned_drain_observer_and_expired_shutdown_cannot_release_work() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let mut a_budget = budget();
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, ()).unwrap();
        drop(leader);
        assert!(operation.cancellation_requested());
        assert!(matches!(
            flights.table.borrow().entries[&fence().page].phase,
            Phase::Draining(_)
        ));
        let expired = RequestScope::new(crate::model::RequestId([1; 16]), Instant::now()).unwrap();
        assert!(matches!(
            poll(flights.drain(&expired)),
            Poll::Ready(Err(Error::DeadlineExceeded))
        ));
        assert_eq!(flights.table.borrow().entries.len(), 1);
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
        let operation = flights.retain_operation(&leader, ()).unwrap();
        let (fence, id) = (operation.fence.clone(), operation.id);
        drop(operation);
        drop(leader);
        drop(a);
        flights.poll_budgeted(100).unwrap();
        assert_eq!(flights.table.borrow().entries.len(), 1);
        // Simulate the worker reaping the actual completion, not request drop.
        flights.complete_operation(&fence, id).unwrap();
        assert!(flights.table.borrow().entries.is_empty());
    }
}

use super::*;
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
use crate::model::{CacheId, CacheKey, ObjectId, ObjectVersion, PageNumber, StrongEtag};
use std::time::Duration;

fn flights(limits: FlightLimits) -> Rc<Flights> {
    Rc::new(
        Flights::with_limits(
            Rc::new(Admission::new(
                crate::test_support::cluster::config(false).limits,
            )),
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
        crate::topology::membership::Membership::validate(
            crate::model::MembershipVersion(1),
            vec![],
        )
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
fn page_result(flights: &Flights) -> PageResult {
    result(flights, fence().page)
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
    use crate::{
        memory::pool::{CiphertextBytes, CiphertextPage, VerifiedBytes, VerifiedPage},
        model::{ExpiresAt, KeyId, Nonce, ObjectMetadata, PageEnvelope},
    };
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
                bytes: vec![1, 2, 3],
                reservation: flights
                    .admission
                    .reserve(None, ResourceClass::Plaintext, 3)
                    .unwrap(),
            }),
        },
        ciphertext: CiphertextPage {
            provenance: None,
            inner: Arc::new(CiphertextBytes {
                checksum: std::sync::OnceLock::new(),
                envelope: PageEnvelope {
                    page,
                    key_id: KeyId::from_generation(1, 1).unwrap(),
                    nonce: Nonce([0; 24]),
                    plaintext_length: 3,
                    ciphertext_length: 19,
                },
                bytes: vec![0; 19],
                reservation: flights
                    .admission
                    .reserve(None, ResourceClass::Ciphertext, 19)
                    .unwrap(),
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
    let operation = flights.retain_operation(&leader, ()).unwrap();
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
    assert!(matches!(
        poll(a.wait()),
        Poll::Ready(Ok(AcquisitionEvent::Complete(_)))
    ));
    assert!(matches!(
        flights.join_copy(&fence().page, &scope),
        Ok(JoinedCopy::Complete(_))
    ));
    drop(a);
    drop(b);
    drop(copy);
    assert!(flights.table.borrow().entries.is_empty());
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
    let mut bad = result(&flights, fence().page);
    bad.metadata.length = 4;
    assert_eq!(flights.publish(leader, bad), Err(Error::CorruptRecord));
    failed(&mut a, Error::Cancelled);
    let retry = lead(&mut b);
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
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let flights = flights(FlightLimits::default());
    let (context, scope) = (origin(), scope());
    let mut budget = budget();
    let mut a = join(&flights, &context, &scope, &mut budget);
    let leader = lead(&mut a);
    let operation = flights.retain_operation(&leader, ()).unwrap();
    let (send, recv) = futures::channel::oneshot::channel::<()>();
    super::super::drivers::spawn(Box::pin(async move {
        recv.await.map_err(|_| Error::Io)?;
        operation.complete()?;
        drop(leader);
        Ok(())
    }))
    .unwrap();
    drop(a);
    flights.poll_budgeted(1).unwrap();
    assert_eq!(flights.table.borrow().entries.len(), 1);
    send.send(()).unwrap();
    assert!(matches!(poll(flights.drain(&scope)), Poll::Ready(Ok(()))));
    assert_eq!(super::super::drivers::pending(), 0);
}

#[test]
fn link_refunds_and_transfers_cannot_duplicate_debits() {
    let mut budget = budget();
    assert_eq!(budget.refund_links(1), Err(Error::InvalidRequest));
    budget.charge_links(5).unwrap();
    let mut child = budget.partition(1, 2).unwrap();
    assert_eq!(child.refund_links(1), Err(Error::InvalidRequest));
    let mut moved = budget.transfer();
    assert_eq!(budget.refund_links(1), Err(Error::InvalidRequest));
    moved.refund_links(3).unwrap();
    assert_eq!(moved.refund_links(3), Err(Error::InvalidRequest));
    assert_eq!(moved.remaining_links() + child.remaining_links(), 6);
}

fn fence() -> Fence {
    Fence {
        owner: Rc::new(()),
        page: PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(crate::security::identity::tests::CACHE.into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            number: PageNumber(0),
        },
        incarnation: 1,
        generation: 1,
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
