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
fn forbidden_is_caller_only_and_peer_auth_is_terminal() {
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
    let retry = lead(&mut b);
    assert_eq!(
        flights.fail(retry, AcquisitionFailure::Terminal(Error::Unauthorized)),
        Ok(FlightState::Failed)
    );
    failed(&mut b, Error::Unauthorized);
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
    let (context, scope_a, scope_b) = (origin(), scope(), scope());
    let (mut a_budget, mut b_budget) = (budget(), budget());
    let mut a = join(&flights, &context, &scope_a, &mut a_budget);
    let mut b = join(&flights, &context, &scope_b, &mut b_budget);
    let leader = lead(&mut a);
    let count = Arc::new(Count(AtomicUsize::new(0)));
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
    assert!(count.0.load(Ordering::Relaxed) > 0);
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
        flights.table.borrow().entries[&fence().page].state,
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
    let ticket = flights.abandon(leader).unwrap();
    assert!(poll(flights.finish_draining(ticket)).is_pending());
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
