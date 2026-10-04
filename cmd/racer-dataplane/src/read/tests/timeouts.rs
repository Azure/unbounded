use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::ExpiresAt;
use crate::model::MembershipVersion;
use crate::model::MetadataSelector;
use crate::model::NodeId;
use crate::model::ObjectMetadata;
use crate::model::ObjectVersion;
use crate::model::OriginContext;
use crate::model::PageNumber;
use crate::model::RequestId;
use crate::model::StrongEtag;
use crate::peer::Requester;
use crate::peer::protocol::FetchMode;
use crate::peer::protocol::Operation as PeerOperation;
use crate::peer::protocol::PeerRequest;
use crate::peer::protocol::PeerResponse;
use crate::peer::protocol::VerifiedResponse;
use crate::read::candidates::*;
use crate::read::flight::AcquisitionBudget;
use crate::runtime::deadline::Deadline;
use crate::runtime::deadline::RequestScope;
use crate::security::connection::Signatures;
use crate::security::forwarding::Forwarding;
use crate::security::test_support::network;
use crate::topology::Candidates;
use crate::topology::Member;
use crate::topology::Membership;
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
            Arc::new(Default::default()),
        );
        let scope =
            RequestScope::new(RequestId([3; 16]), Instant::now() + Duration::from_secs(1)).unwrap();
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

fn poll<T>(future: Pin<&mut impl Future<Output = T>>) -> Poll<T> {
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
    let demand =
        crate::peer::subscriptions::Demand::new(vec![crate::peer::subscriptions::PageInterval {
            start: 0,
            end: 1,
        }])
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
    let demand =
        crate::peer::subscriptions::Demand::new(vec![crate::peer::subscriptions::PageInterval {
            start: 0,
            end: 1,
        }])
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
