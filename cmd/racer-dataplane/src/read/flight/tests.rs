mod lifecycle_tests;

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
            expires_at: ExpiresAt(std::time::UNIX_EPOCH),
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
            inner: Arc::new(CiphertextBytes {
                checksum: std::sync::OnceLock::new(),
                envelope: PageEnvelope {
                    page,
                    key_id: KeyId([1; 16]),
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
    assert!(matches!(poll(copy.wait()), Poll::Ready(Ok(_))));
    assert!(matches!(
        flights.join_copy(&fence().page, &scope),
        Ok(JoinedCopy::Complete(_))
    ));
    drop(a);
    drop(b);
    drop(copy);
    assert!(flights.table.borrow().entries.is_empty());
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
fn driver_queue_progresses_after_all_request_waiters_are_dropped() {
    let queue = Rc::new(crate::read::drivers::DriverQueue::default());
    let _owner = queue.enter();
    let flights = flights(FlightLimits::default());
    let (context, scope) = (origin(), scope());
    let mut a_budget = budget();
    let mut a = join(&flights, &context, &scope, &mut a_budget);
    let leader = lead(&mut a);
    let operation = flights.retain_operation(&leader, ()).unwrap();
    let (complete, fence) = futures::channel::oneshot::channel::<()>();
    super::super::drivers::spawn(Box::pin(async move {
        fence.await.map_err(|_| Error::Io)?;
        operation.complete()?;
        drop(leader);
        Ok(())
    }))
    .unwrap();
    flights.poll_budgeted(4).unwrap();
    drop(a);
    assert_eq!(flights.table.borrow().entries.len(), 1);
    complete.send(()).unwrap();
    flights.poll_budgeted(4).unwrap();
    assert!(flights.table.borrow().entries.is_empty());
    assert_eq!(super::super::drivers::pending(), 0);
}

#[test]
fn deadline_expiration_is_independent_and_wakes_parked_callers() {
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };
    struct Count(AtomicUsize);
    impl std::task::Wake for Count {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }
    let flights = flights(FlightLimits::default());
    let (context, scope) = (origin(), scope());
    let (mut a_budget, mut b_budget) = (budget(), budget());
    let mut a = join(&flights, &context, &scope, &mut a_budget);
    let mut b = join(&flights, &context, &scope, &mut b_budget);
    let leader = lead(&mut a);
    let count = Arc::new(Count(AtomicUsize::new(0)));
    let waker = Waker::from(count.clone());
    assert!(
        b.wait()
            .as_mut()
            .poll(&mut Context::from_waker(&waker))
            .is_pending()
    );
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
    assert!(count.0.load(Ordering::Relaxed) > 0);
    failed(&mut b, Error::DeadlineExceeded);
    assert!(a.acquisition(&leader).is_ok());
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

fn shared_result(flights: &Flights) -> PageResult {
    result(flights, fence().page)
}

#[test]
fn publication_waits_for_completion_and_shares_whole_page() {
    let flights = flights(FlightLimits::default());
    let (context, scope) = (origin(), scope());
    let mut budget = budget();
    let mut a = join(&flights, &context, &scope, &mut budget);
    let leader = lead(&mut a);
    let mut copy = match flights.join_copy(&fence().page, &scope).unwrap() {
        JoinedCopy::Waiter(waiter) => waiter,
        _ => panic!("expected copy"),
    };
    let operation = flights.retain_operation(&leader, ()).unwrap();
    let page = shared_result(&flights);
    assert_eq!(
        flights.publish(leader, page.clone()),
        Ok(FlightState::Draining)
    );
    assert!(poll(copy.wait()).is_pending());
    operation.complete().unwrap();
    let shared = match poll(copy.wait()) {
        Poll::Ready(Ok(result)) => result.verified(),
        _ => panic!("expected result"),
    };
    assert!(std::sync::Arc::ptr_eq(
        &shared.plaintext.inner,
        &page.plaintext.inner
    ));
    assert!(std::sync::Arc::ptr_eq(
        &shared.ciphertext.inner,
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
    drop(copy);
    drop(a);
    assert!(matches!(
        flights.join_copy(&fence().page, &scope),
        Ok(JoinedCopy::Miss)
    ));
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
