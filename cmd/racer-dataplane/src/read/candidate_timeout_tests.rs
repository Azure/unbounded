use super::*;
use crate::{
    model::{
        identity::{MembershipVersion, ObjectVersion, RequestId, StrongEtag},
        metadata::{ExpiresAt, ObjectMetadata},
    },
    security::{
        forwarding::Forwarding,
        signing::{Signatures, tests::network},
    },
    topology::membership::{Member, Membership},
};
use std::{
    cell::Cell,
    future::{Future, poll_fn},
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};

struct Call {
    scope: RequestScope,
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
            version: ObjectVersion {
                object: request.origin.object.clone(),
                etag: StrongEtag::test_value("fallback"),
            },
            length: 17,
            expires_at: ExpiresAt(std::time::SystemTime::now() + Duration::from_secs(60)),
        };
        let (signed, binding) = sender.sign_request(request)?;
        let admitted = receiver.verify_request(signed)?;
        let signed =
            receiver.sign_response(admitted.binding(), PeerResponse::Metadata(metadata))?;
        sender.verify_response(signed, &binding)
    }
}

impl PeerClient for Peers {
    fn request<'a>(
        &'a self,
        request: PeerRequest,
        membership: crate::topology::membership::MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        assert_eq!(scope.deadline.0, request.route.deadline.0);
        assert_eq!(scope.deadline.0, request.origin.scope().deadline.0);
        assert_eq!(scope.request, request.route.request);
        let copy = match &request.operation {
            PeerOperation::Metadata { mode, .. } | PeerOperation::Page { mode, .. } => {
                matches!(mode, FetchMode::CopyOnly)
            }
        };
        let stalled = self.calls.borrow().len() < self.stalled;
        self.calls.borrow_mut().push(Call {
            scope: scope.clone(),
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
    fn new(rank: Option<usize>, stalled: usize, late_success: bool) -> Self {
        let (_, placement, context, _, credentials) = super::tests::fixture();
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
                        alignment_enabled: false,
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
        let policy = CandidatePolicy::new(local, placement, peers.clone());
        policy.set_credentials(credentials);
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
    let deadline = f.peers.calls.borrow()[0].scope.deadline.0;
    assert!(deadline < f.scope.deadline.0);
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
    expire(f.peers.calls.borrow()[0].scope.deadline.0);
    assert!(poll(resolve.as_mut()).is_pending());
    expire(f.peers.calls.borrow()[1].scope.deadline.0);
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
    let deadline = f.peers.calls.borrow()[0].scope.deadline.0;
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
    assert!(f.peers.calls.borrow()[0].scope.deadline.0 < overall);
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
        let deadline = f.peers.calls.borrow()[index].scope.deadline.0;
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
