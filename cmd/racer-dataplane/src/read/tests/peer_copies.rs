//! Real AEAD and signed peer responses, with deterministic ranked source scripts.

use super::fill::*;
use crate::memory::CiphertextCopy;
use crate::peer::forwarding::Forwarding;
use crate::peer::forwarding::VerifiedResponse;
use crate::peer::protocol::PeerRequest;
use crate::test_support::security::network;
use crate::test_support::security::node;
use racer_control_wire::NodeId;
use std::collections::VecDeque;

enum Reply {
    Copy(CiphertextCopy),
    Miss,
    Unauthorized,
    Stale,
    VersionUnavailable,
    Deadline,
    Serve,
    Cancel(RequestScope),
    FencedCopy(CiphertextCopy, Rc<Cell<bool>>, Rc<Cell<bool>>),
}
struct ScriptedPeers {
    receiver: RefCell<Option<Rc<crate::read::Coordinator>>>,
    admission: Rc<flow_control::Quotas<crate::admission::AdmissionPolicy>>,
    hedge: Cell<bool>,
    primary_polls: Cell<usize>,
    canceled_primary: Cell<bool>,
    advance_primary: RefCell<Option<uring_runtime::environment::SimulationClock>>,
    local: usize,
    signing: Vec<Forwarding>,
    replies: RefCell<VecDeque<(NodeId, Reply)>>,
    calls: RefCell<Vec<(NodeId, bool, u32, u8, Instant)>>,
    local_deadlines: RefCell<Vec<(Instant, Instant)>>,
}
impl ScriptedPeers {
    fn requester(self: &Rc<Self>) -> Rc<Requester> {
        Requester::scripted(
            self.clone(),
            Self::direct_hedge_available,
            Self::request,
            Self::request_direct,
        )
    }
    fn direct_hedge_available(
        &self,
        _: &std::sync::Arc<crate::topology::Membership>,
        _: &NodeId,
    ) -> bool {
        self.hedge.get()
    }
    fn request_direct<'a>(
        &'a self,
        request: PeerRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        self.request(request, membership, scope)
    }
    fn request<'a>(
        &'a self,
        request: PeerRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async move {
            let (overall, share) = scope.body_deadlines.expect("local candidate time share");
            assert_eq!(overall, scope.deadline.0);
            assert_eq!(overall, request.route.deadline.0);
            assert_eq!(overall, request.origin.scope().deadline.0);
            self.local_deadlines.borrow_mut().push((overall, share));
            let destination = request.route.destination.clone();
            self.calls.borrow_mut().push((
                destination.clone(),
                matches!(
                    request.operation,
                    PeerOperation::Page {
                        mode: FetchMode::CopyOnly,
                        ..
                    }
                ),
                request.route.remaining_attempts,
                request.route.remaining_links,
                request.route.deadline.0,
            ));
            let (expected, reply) = self
                .replies
                .borrow_mut()
                .pop_front()
                .expect("no repeated source");
            assert_eq!(destination, expected);
            if self.hedge.get() && self.calls.borrow().len() == 1 {
                std::future::poll_fn(|cx| {
                    if scope.check().is_err() {
                        self.canceled_primary.set(true);
                        return std::task::Poll::Ready(());
                    }
                    if self.primary_polls.get() == 0 {
                        return std::task::Poll::Ready(());
                    }
                    self.primary_polls.set(self.primary_polls.get() - 1);
                    if let Some(clock) = self.advance_primary.borrow().as_ref() {
                        clock.advance(Duration::from_secs(1));
                    }
                    cx.waker().wake_by_ref();
                    std::task::Poll::Pending
                })
                .await;
                scope.check()?;
            }
            let response = match reply {
                Reply::FencedCopy(copy, accepted, completed) => {
                    // Own the accepted receive buffer until its explicit completion
                    // fence, including after the independent attempt is canceled.
                    accepted.set(true);
                    std::future::poll_fn(|_| {
                        if completed.get() {
                            Poll::Ready(())
                        } else {
                            Poll::Pending
                        }
                    })
                    .await;
                    PeerResponse::Page {
                        metadata: copy.metadata,
                        ciphertext: copy.ciphertext,
                    }
                }
                Reply::Cancel(parent) => {
                    parent.cancel()?;
                    return Err(Error::Cancelled);
                }
                Reply::Deadline => return Err(Error::DeadlineExceeded),
                Reply::Serve => {
                    use crate::peer::server::LocalPageService;
                    let remote = (0..4).find(|i| node(*i) == destination).unwrap();
                    let (signed, binding) = self.signing[self.local].sign_request(request)?;
                    let admitted = self.signing[remote].verify_request(signed)?;
                    let reply_binding = admitted.binding().clone();
                    let receiver = self.receiver.borrow().as_ref().unwrap().clone();
                    let response = receiver.serve_peer(admitted, membership, scope).await?;
                    // As on the wire, receive buffers belong to the requester.
                    let response = match response {
                        PeerResponse::Page {
                            metadata,
                            ciphertext,
                        } => {
                            let copy = BufferPool::new(self.admission.clone()).ciphertext(
                                self.admission.reserve(
                                    Some(&metadata.version.object.cache),
                                    ResourceClass::Ciphertext,
                                    ciphertext.bytes().len(),
                                )?,
                                ciphertext.envelope().clone(),
                                ciphertext.bytes().to_vec(),
                            )?;
                            PeerResponse::Page {
                                metadata,
                                ciphertext: copy,
                            }
                        }
                        other => other,
                    };
                    let response = self.signing[remote].sign_response(&reply_binding, response)?;
                    return self.signing[self.local].verify_response(response, &binding);
                }
                Reply::Copy(copy)
                    if matches!(request.operation, PeerOperation::Subscribe { .. }) =>
                {
                    let PeerOperation::Subscribe { subscription, .. } = &request.operation else {
                        unreachable!()
                    };
                    PeerResponse::Selected {
                        grant: crate::peer::subscriptions::TransferGrant {
                            subscription_id: subscription.id,
                            sequence: subscription.sequence,
                            page: copy.ciphertext.envelope().page.clone(),
                            membership: request.route.membership,
                            receiver: node(self.local),
                            deadline: crate::peer::protocol::number(
                                &crate::peer::protocol::request_head(&request)?,
                                "racer-route-deadline",
                            )?,
                            remaining_page_budget: subscription.page_budget - 1,
                            remaining_byte_budget: subscription.byte_budget
                                - copy.ciphertext.bytes().len() as u64,
                        },
                        metadata: copy.metadata,
                        ciphertext: copy.ciphertext,
                    }
                }
                Reply::Copy(copy) => PeerResponse::Page {
                    metadata: copy.metadata,
                    ciphertext: copy.ciphertext,
                },
                Reply::Miss => PeerResponse::Miss,
                Reply::Unauthorized => return Err(Error::Unauthorized),
                Reply::Stale => PeerResponse::StaleMembership,
                Reply::VersionUnavailable => PeerResponse::VersionUnavailable,
            };
            let remote = (0..4).find(|i| node(*i) == destination).unwrap();
            let (signed, binding) = self.signing[self.local].sign_request(request)?;
            let admitted = self.signing[remote].verify_request(signed)?;
            let response = self.signing[remote].sign_response(admitted.binding(), response)?;
            self.signing[self.local].verify_response(response, &binding)
        })
    }
}

fn install_peers(f: &mut Fixture, rank: Option<usize>) -> (Rc<ScriptedPeers>, Vec<NodeId>) {
    f.membership = Arc::new(
        Membership::validate(
            racer_control_wire::MembershipVersion(1),
            (0..4)
                .map(|i| Member {
                    node: node(i),
                    shares: NonZeroU32::new(4).unwrap(),
                    peer_endpoint: format!("127.0.0.1:{}", 8000 + i),
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let placement = Rc::new(Placement::new(16));
    let ordered = placement
        .rank(f.membership.clone(), &f.context.object, f.page.number)
        .unwrap()
        .ordered;
    let local = rank
        .map(|rank| ordered[rank].clone())
        .unwrap_or_else(|| (0..4).map(node).find(|n| !ordered.contains(n)).unwrap());
    let peers = Rc::new(ScriptedPeers {
        receiver: RefCell::new(None),
        admission: f.fill.dependencies.admission.clone(),
        hedge: Cell::new(false),
        primary_polls: Cell::new(0),
        canceled_primary: Cell::new(false),
        advance_primary: RefCell::new(None),
        local: (0..4).find(|i| node(*i) == local).unwrap(),
        signing: network(4).into_iter().map(Forwarding::new).collect(),
        replies: RefCell::new(VecDeque::new()),
        calls: RefCell::new(Vec::new()),
        local_deadlines: RefCell::new(Vec::new()),
    });
    let mut dependencies = f.fill.dependencies.clone();
    dependencies.candidates = Rc::new(CandidatePolicy::new(
        local,
        placement,
        Requester::scripted(
            peers.clone(),
            ScriptedPeers::direct_hedge_available,
            ScriptedPeers::request,
            ScriptedPeers::request_direct,
        ),
        dependencies.credentials.clone(),
        Arc::new(Default::default()),
    ));
    f.fill = Fill::new(dependencies);
    (peers, ordered)
}

#[test]
fn serial_zero_grant_reaches_real_pending_and_disk_copy() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for (disk, missing) in [(false, false), (true, false), (false, true)] {
        let mut target = fixture();
        target.reactor.init().unwrap();
        futures::executor::block_on(target.fill.dependencies.writer.open()).unwrap();
        let mut seed_budget = AcquisitionBudget::new(target.scope.deadline.0, 4, 8);
        let seeded = if missing {
            None
        } else {
            Some(acquire(&mut target, &mut seed_budget).unwrap())
        };
        let expected = seeded
            .as_ref()
            .map(|p| p.ciphertext.bytes().to_vec())
            .unwrap_or_default();
        if disk {
            assert_eq!(
                drive_disk(
                    &target,
                    target.fill.dependencies.writer.progress(1, &target.scope)
                )
                .unwrap(),
                1
            );
        }
        drop(seeded);
        target
            .fill
            .dependencies
            .memory
            .remove_cache(&target.context.object.cache)
            .unwrap();
        assert!(
            target
                .fill
                .dependencies
                .memory
                .ciphertext(&target.page)
                .unwrap()
                .is_none()
        );
        assert_eq!(
            target
                .fill
                .dependencies
                .writer
                .copy_only(&target.page)
                .unwrap()
                .is_none(),
            disk || missing
        );
        install_peers(&mut target, Some(2));
        let (receiver, endpoint, _) = target.read_graph(
            Rc::new(target.fill.clone()),
            &target.membership,
            ReadGraphSettings {
                name: "zero-grant",
                snapshots: 4,
                metadata: 16,
                window: 1,
                stall: Duration::from_secs(10),
                seed: None,
            },
        );
        let mut source = fixture();
        let (peers, ordered) = install_peers(&mut source, None);
        *peers.receiver.borrow_mut() = Some(receiver);
        peers.replies.borrow_mut().extend([
            (ordered[0].clone(), Reply::Deadline),
            (ordered[1].clone(), Reply::Deadline),
            (ordered[2].clone(), Reply::Serve),
        ]);
        let scope = source.scope.clone();
        let mut budget = crate::read::range_stream::client_page_budget_for_test(scope.deadline.0);
        let fill = source.fill.clone();
        let context = OriginContext {
            object: source.context.object.clone(),
            metadata: None,
            authorization: None,
        };
        let mut read = Box::pin(fill.acquire(
            source.page.clone(),
            source.membership.clone(),
            &context,
            &scope,
            &mut budget,
        ));
        let mut result = None;
        for _ in 0..4096 {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            uring_runtime::drivers::poll(&mut cx, 64);
            for f in [&mut source, &mut target] {
                uring_runtime::group::Service::poll_budgeted(&mut f.engine, &mut cx, 64).unwrap();
                f.crypto.poll_budgeted(64).unwrap();
            }
            target.reactor.poll_budgeted(64).unwrap();
            if let Poll::Ready(done) = read.as_mut().poll(&mut cx) {
                result = Some(done);
                break;
            }
            if disk {
                target.reactor.wait(Duration::from_millis(1)).unwrap();
            }
        }
        drop(read);
        let calls = peers.calls.borrow();
        assert_eq!(calls.iter().map(|c| c.2).collect::<Vec<_>>(), [3, 2, 0]);
        assert_eq!(calls.iter().map(|c| c.3).collect::<Vec<_>>(), [4, 8, 4]);
        assert_eq!(
            calls.iter().map(|c| c.1).collect::<Vec<_>>(),
            [false, false, true]
        );
        assert!(calls.iter().all(|c| c.4 == scope.deadline.0));
        assert_eq!(
            (budget.remaining_attempts(), budget.remaining_links()),
            (0, 0)
        );
        assert_eq!(scope.check(), Ok(()));
        assert_eq!(source.origin.calls.get(), 0);
        assert_eq!(
            target.origin.calls.get(),
            usize::from(!missing),
            "only fixture seeding uses origin"
        );
        let result = result.expect("bounded coordinator completion");
        if missing {
            assert!(matches!(result, Err(Error::Unavailable)));
            assert_unpublished(&source);
        } else {
            let result = result.unwrap();
            assert_eq!(result.plaintext.bytes(), b"abc");
            assert_eq!(result.ciphertext.bytes(), expected);
        }
        drop(calls);
        // Independent signed control with the SAME original ceiling and last-route
        // allowance, not a retry or a replenishment of the exhausted acquisition.
        peers
            .replies
            .borrow_mut()
            .push_back((ordered[2].clone(), Reply::Serve));
        let mut control_budget = AcquisitionBudget::new(scope.deadline.0, 1, 4);
        let operation = PeerOperation::Page {
            page: source.page.clone(),
            mode: FetchMode::CopyOnly,
        };
        let mut control = Box::pin(fill.dependencies.candidates.request(
            &source.membership,
            &ordered[2],
            &context,
            &operation,
            FetchMode::CopyOnly,
            &scope,
            &mut control_budget,
            1,
        ));
        let mut copy = None;
        for _ in 0..4096 {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            uring_runtime::drivers::poll(&mut cx, 64);
            uring_runtime::group::Service::poll_budgeted(&mut target.engine, &mut cx, 64).unwrap();
            target.crypto.poll_budgeted(64).unwrap();
            target.reactor.poll_budgeted(64).unwrap();
            if let Poll::Ready(done) = control.as_mut().poll(&mut cx) {
                copy = Some(done);
                break;
            }
            target.reactor.wait(Duration::from_millis(1)).unwrap();
        }
        let copy = copy.expect("bounded signed copy completion").unwrap();
        drop(control);
        if missing {
            assert!(matches!(copy.response(), PeerResponse::Miss));
        } else {
            let PeerResponse::Page { ciphertext, .. } = copy.response() else {
                panic!("signed copy")
            };
            assert_eq!(ciphertext.bytes(), expected);
        }
        assert_eq!(
            (
                control_budget.remaining_attempts(),
                control_budget.remaining_links()
            ),
            (0, 0)
        );
        assert_eq!(scope.check(), Ok(()));
        drop(endpoint);
    }
}

#[test]
fn serial_zero_grant_copy_failures_and_positive_grant_acquire() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for attempts in [8, 16] {
        for case in ["good", "corrupt", "missing", "auth", "cancel"] {
            let mut f = fixture();
            let (peers, ordered) = install_peers(&mut f, None);
            let failures = crate::telemetry::Failures::default();
            let mut deps = f.fill.dependencies.clone();
            deps.candidates = Rc::new(
                CandidatePolicy::new(
                    node(peers.local),
                    Rc::new(Placement::new(16)),
                    peers.requester(),
                    deps.credentials.clone(),
                    Arc::new(Default::default()),
                )
                .with_observer(failures.observer(WorkerId(0))),
            );
            f.fill = Fill::new(deps);
            let good = encrypted_copy(&mut f);
            let bad = unusable_copy(&f, &good, false);
            let last = match case {
                "good" => Reply::Copy(good.clone()),
                "corrupt" => Reply::Copy(bad.clone()),
                "missing" => Reply::Miss,
                "auth" => Reply::Unauthorized,
                "cancel" => Reply::Cancel(f.scope.clone()),
                _ => unreachable!(),
            };
            peers.replies.borrow_mut().extend([
                (ordered[0].clone(), Reply::Deadline),
                (ordered[1].clone(), Reply::Copy(bad)),
                (ordered[2].clone(), last),
            ]);
            let deadline = f.scope.deadline.0;
            let mut budget = AcquisitionBudget::new(deadline, attempts, 16);
            let result = acquire(&mut f, &mut budget);
            match case {
                "good" => {
                    let result = result.unwrap();
                    assert_eq!(result.plaintext.bytes(), b"abc");
                    assert_eq!(result.ciphertext.bytes(), good.ciphertext.bytes());
                }
                "auth" => assert!(matches!(result, Err(Error::Unauthorized))),
                "cancel" => assert!(matches!(result, Err(Error::Cancelled))),
                _ => assert!(matches!(result, Err(Error::Unavailable))),
            }
            if case != "good" {
                assert_unpublished(&f);
            }
            let calls = peers.calls.borrow();
            assert_eq!(calls.len(), 3);
            assert_eq!(
                calls.iter().map(|c| c.1).collect::<Vec<_>>(),
                [false, false, attempts == 8]
            );
            assert_eq!(
                calls.iter().map(|c| c.2).collect::<Vec<_>>(),
                if attempts == 8 {
                    vec![3, 2, 0]
                } else {
                    vec![5, 5, 3]
                }
            );
            assert_eq!(calls.iter().map(|c| c.3).collect::<Vec<_>>(), [4, 8, 4]);
            assert!(calls.iter().all(|c| c.4 == deadline));
            assert_eq!(
                (budget.remaining_attempts(), budget.remaining_links()),
                (0, 0)
            );
            assert_eq!(budget.deadline(), deadline);
            assert_eq!(f.origin.calls.get(), 0);
            assert_eq!(f.crypto.outstanding(), 0);
            let mut diagnostics = String::new();
            failures.write_candidate_final(&mut diagnostics).unwrap();
            if case == "good" {
                assert!(diagnostics.contains("total=0 retained=0"));
            } else {
                assert!(diagnostics.contains("total=1 retained=1"), "{diagnostics}");
                let last = diagnostics
                    .lines()
                    .filter(|line| line.contains("rank=2"))
                    .last()
                    .unwrap();
                assert!(
                    last.contains(if attempts == 8 {
                        "acquire=false"
                    } else {
                        "acquire=true"
                    }),
                    "{last}"
                );
            }
        }
    }
}

#[test]
fn serial_zero_grant_cancel_waits_for_accepted_copy_fence() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    let (peers, ordered) = install_peers(&mut f, None);
    let copy = encrypted_copy(&mut f);
    let allocation = Arc::downgrade(&copy.ciphertext.inner);
    let accepted = Rc::new(Cell::new(false));
    let completed = Rc::new(Cell::new(false));
    peers.replies.borrow_mut().extend([
        (ordered[0].clone(), Reply::Deadline),
        (ordered[1].clone(), Reply::Deadline),
        (
            ordered[2].clone(),
            Reply::FencedCopy(copy, accepted.clone(), completed.clone()),
        ),
    ]);
    let policy = f.fill.dependencies.candidates.clone();
    let candidates = policy
        .candidates(f.membership.clone(), &f.context.object, f.page.number)
        .unwrap();
    let operation = PeerOperation::Page {
        page: f.page.clone(),
        mode: FetchMode::Acquire,
    };
    let deadline = f.scope.deadline.0;
    let mut budget = crate::read::range_stream::client_page_budget_for_test(deadline);
    let mut resolve =
        policy.resolve_with_budget(candidates, &f.context, operation, &f.scope, &mut budget);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(resolve.as_mut().poll(&mut cx).is_pending());
    assert!(accepted.get());
    assert!(allocation.upgrade().is_some());
    assert_eq!(
        peers
            .calls
            .borrow()
            .iter()
            .map(|c| (c.1, c.2, c.3))
            .collect::<Vec<_>>(),
        [(false, 3, 4), (false, 2, 8), (true, 0, 4)]
    );
    f.scope.cancel().unwrap();
    assert!(
        resolve.as_mut().poll(&mut cx).is_pending(),
        "cancellation is not a completion fence"
    );
    assert!(
        allocation.upgrade().is_some(),
        "accepted buffer remains owned"
    );
    completed.set(true);
    assert!(matches!(
        resolve.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    ));
    drop(resolve);
    assert!(allocation.upgrade().is_none());
    assert_eq!(
        (budget.remaining_attempts(), budget.remaining_links()),
        (0, 0)
    );
    assert_eq!(budget.deadline(), deadline);
    assert_eq!(peers.calls.borrow().len(), 3);
    assert_eq!(f.origin.calls.get(), 0);
    assert_eq!(f.crypto.outstanding(), 0);
    assert_unpublished(&f);
}

#[test]
fn serial_zero_grant_predicate_matches_unpartitioned_remote_grant() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for attempts in 0u32..=16 {
        let mut f = fixture();
        let (peers, ordered) = install_peers(&mut f, None);
        peers
            .replies
            .borrow_mut()
            .push_back((ordered[0].clone(), Reply::Copy(encrypted_copy(&mut f))));
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, attempts, 16);
        let result = acquire(&mut f, &mut budget);
        let calls = peers.calls.borrow();
        if attempts == 0 {
            assert!(matches!(result, Err(Error::Unavailable)));
            assert!(calls.is_empty());
            continue;
        }
        assert_eq!(result.unwrap().plaintext.bytes(), b"abc");
        assert_eq!(calls.len(), 1);
        let grant = (attempts - 1).div_ceil(3);
        assert_eq!(calls[0].2, grant);
        assert_eq!(calls[0].1, grant == 0);
        assert_eq!(budget.remaining_attempts() + 1 + grant, attempts);
        assert_eq!(budget.remaining_links(), 12);
    }
}

#[test]
fn invalid_signed_selection_falls_back_without_resetting_acquisition_budget() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for missing_key in [false, true] {
        for origin in [false, true] {
            let mut f = fixture();
            let (peers, ordered) = install_peers(&mut f, Some(1));
            let good = encrypted_copy(&mut f);
            let bad = unusable_copy(&f, &good, missing_key);
            peers.replies.borrow_mut().extend([
                (ordered[0].clone(), Reply::Copy(bad)),
                (
                    ordered[0].clone(),
                    if origin {
                        Reply::Miss
                    } else {
                        Reply::Copy(good)
                    },
                ),
            ]);
            let signers = network(4);
            let (_local, mut endpoint, _published) = super::hot_reads::coordinator(
                &f,
                &signers[peers.local],
                &f.membership,
                peers.requester(),
            );
            let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 10, 18);
            let demand = crate::peer::subscriptions::Demand::new(vec![
                crate::peer::subscriptions::PageInterval { start: 0, end: 1 },
            ])
            .unwrap();
            let mut work = Box::pin(f.fill.select_subscription(
                f.page.version.clone(),
                demand,
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget,
            ));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let mut result = None;
            for _ in 0..4096 {
                if let Poll::Ready(value) = work.as_mut().poll(&mut cx) {
                    result = Some(value);
                    break;
                }
                endpoint.poll(&mut cx, 64).unwrap();
                uring_runtime::drivers::poll(&mut cx, 64);
                uring_runtime::group::Service::poll_budgeted(
                    &mut f.engine,
                    &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
                    64,
                )
                .unwrap();
                f.crypto.poll_budgeted(64).unwrap();
                f.reactor.poll_budgeted(64).unwrap();
            }
            drop(work);
            assert_eq!(
                result.expect("bounded fallback").unwrap().plaintext.bytes(),
                b"abc"
            );
            assert_eq!(f.origin.calls.get(), usize::from(origin));
            assert_eq!(peers.calls.borrow().len(), 2);
            assert!(peers.replies.borrow().is_empty());
            assert!(budget.remaining_attempts() < 10);
            assert!(budget.remaining_links() < 18);
            assert_eq!(budget.deadline(), f.scope.deadline.0);
            endpoint.uninstall().unwrap();
        }
    }
}

#[test]
fn hedged_plaintext_validates_aead_and_preserves_singleflight_and_original_credits() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for corrupt in [false, true] {
        let mut f = fixture();
        let (peers, ordered) = install_peers(&mut f, None);
        let good = encrypted_copy(&mut f);
        let alternative = if corrupt {
            unusable_copy(&f, &good, false)
        } else {
            good.clone()
        };
        peers.replies.borrow_mut().extend([
            (ordered[0].clone(), Reply::Copy(good.clone())),
            (ordered[1].clone(), Reply::Copy(alternative)),
        ]);
        peers.primary_polls.set(40);
        let metrics = enable_hedge(&mut f, &peers);
        let mut first_budget = AcquisitionBudget::new(f.scope.deadline.0, 10, 18);
        let mut second_budget = AcquisitionBudget::new(f.scope.deadline.0, 10, 18);
        let (first, second) = drive(
            async {
                futures::join!(
                    f.fill.acquire(
                        f.page.clone(),
                        f.membership.clone(),
                        &f.context,
                        &f.scope,
                        &mut first_budget
                    ),
                    f.fill.acquire(
                        f.page.clone(),
                        f.membership.clone(),
                        &f.context,
                        &f.scope,
                        &mut second_budget
                    )
                )
            },
            &mut f.engine,
            &f.crypto,
        );
        let first = first.unwrap();
        let second = second.unwrap();
        assert_eq!(first.plaintext.bytes(), b"abc");
        assert_eq!(second.plaintext.bytes(), b"abc");
        assert_eq!(peers.calls.borrow().len(), 2);
        assert!(!peers.calls.borrow()[0].1);
        assert!(peers.calls.borrow()[1].1);
        assert_eq!(metrics.count(Event::PageHedgeStarted), 1);
        assert_eq!(metrics.count(Event::PageHedgeWon), u64::from(!corrupt));
        assert_eq!(peers.canceled_primary.get(), !corrupt);
        assert_eq!(first_budget.deadline(), f.scope.deadline.0);
        assert_eq!(
            first_budget.remaining_attempts() + second_budget.remaining_attempts(),
            17
        );
        assert_eq!(
            first_budget.remaining_links() + second_budget.remaining_links(),
            34
        );
        assert!(
            peers
                .calls
                .borrow()
                .iter()
                .all(|call| call.4 == f.scope.deadline.0)
        );
        assert_published(&f, &first, false);
    }
}

#[test]
fn hedge_suppresses_without_independent_route_credits_or_local_memory() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for reason in ["route", "credits", "memory", "slots"] {
        let mut f = fixture();
        let (peers, ordered) = install_peers(&mut f, None);
        peers.hedge.set(reason != "route");
        let metrics = crate::telemetry::Metrics::default();
        let hedges = crate::read::candidates::Hedges::new(
            crate::read::candidates::HedgeConfig {
                delay: Duration::from_millis(1),
                slots: 1,
                bytes: crate::read::candidates::DUPLICATE_BYTES,
            },
            metrics.clone(),
        )
        .unwrap();
        let _slot = (reason == "slots").then(|| hedges.acquire().unwrap());
        let mut deps = f.fill.dependencies.clone();
        deps.candidates = Rc::new(
            CandidatePolicy::new(
                node(peers.local),
                Rc::new(Placement::new(16)),
                peers.requester(),
                deps.credentials.clone(),
                Arc::new(Default::default()),
            )
            .with_hedges(hedges),
        );
        f.fill = Fill::new(deps);
        let admission = &f.fill.dependencies.admission;
        let _pressure = (reason == "memory").then(|| {
            admission
                .reserve(
                    None,
                    ResourceClass::Plaintext,
                    admission.limit(ResourceClass::Plaintext),
                )
                .unwrap()
        });
        let mut budget = AcquisitionBudget::new(
            f.scope.deadline.0,
            if reason == "credits" { 5 } else { 10 },
            if reason == "credits" { 9 } else { 18 },
        );
        let before = (
            budget.remaining_attempts(),
            budget.remaining_links(),
            budget.deadline(),
        );
        let candidates = crate::topology::Candidates {
            membership: f.membership.clone(),
            ordered,
        };
        let operation = PeerOperation::Page {
            page: f.page.clone(),
            mode: FetchMode::Acquire,
        };
        let result = futures::executor::block_on(f.fill.dependencies.candidates.hedge_page(
            &candidates,
            &f.context,
            &operation,
            &f.scope,
            &mut budget,
            admission,
            &mut crate::read::candidates::HedgeContinuation::default(),
            |_, _| Box::pin(async { panic!("suppressed hedge validation") }),
        ));
        assert!(matches!(result, Ok(None)));
        assert_eq!(
            before,
            (
                budget.remaining_attempts(),
                budget.remaining_links(),
                budget.deadline()
            )
        );
        assert!(peers.calls.borrow().is_empty());
        assert_eq!(metrics.count(Event::PageHedgeSuppressed), 1);
        assert_eq!(metrics.count(Event::PageHedgeStarted), 0);
    }
}

fn encrypted_copy(f: &mut Fixture) -> CiphertextCopy {
    let admission = &f.fill.dependencies.admission;
    let plaintext = admission
        .reserve(Some(&f.context.object.cache), ResourceClass::Plaintext, 3)
        .unwrap();
    let ciphertext = admission
        .reserve(Some(&f.context.object.cache), ResourceClass::Ciphertext, 19)
        .unwrap();
    let mut plaintext = f.fill.dependencies.buffers.plaintext(plaintext, 3).unwrap();
    plaintext.bytes_mut().unwrap().copy_from_slice(b"abc");
    let (_, ciphertext) = drive(
        f.fill
            .dependencies
            .crypto
            .encrypt(f.page.clone(), plaintext, ciphertext, &f.scope),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    CiphertextCopy {
        metadata: f.origin.metadata.clone(),
        ciphertext,
    }
}

#[test]
fn retained_small_copy_admits_actual_plaintext_with_live_pressure() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for free in [3, 4096] {
        let mut f = fixture();
        let copy = encrypted_copy(&mut f);
        let admission = f.fill.dependencies.admission.clone();
        admission.reclaim_buffers();
        let pressure = admission
            .reserve(
                None,
                ResourceClass::Plaintext,
                admission.limit(ResourceClass::Plaintext) - free,
            )
            .unwrap();
        f.fill
            .dependencies
            .memory
            .publish_ciphertext(crate::memory::UnverifiedPage {
                copy: copy.clone(),
                disk_token: None,
            })
            .unwrap();
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
        let result = drive(
            f.fill.acquire(
                f.page.clone(),
                f.membership.clone(),
                &f.context,
                &f.scope,
                &mut budget,
            ),
            &mut f.engine,
            &f.crypto,
        )
        .expect("known three-byte ciphertext must not require a full plaintext page");
        assert_eq!(result.plaintext.bytes(), b"abc");
        assert!(Arc::ptr_eq(
            &copy.ciphertext.inner,
            &result.ciphertext.inner
        ));
        assert_eq!(
            pressure.amount(),
            admission.limit(ResourceClass::Plaintext) - free
        );
        assert_eq!(
            admission.used(ResourceClass::Plaintext),
            pressure.amount() + 3
        );
        assert_eq!(f.origin.calls.get(), 0);
        assert_eq!(budget.remaining_attempts(), 8);
        assert_eq!(f.crypto.outstanding(), 0);
    }
}

#[test]
fn known_copy_admission_validates_before_reclaim_and_preserves_failures() {
    for case in [
        "success", "short", "canceled", "metadata", "identity", "corrupt",
    ] {
        let mut f = fixture();
        let mut copy = encrypted_copy(&mut f);
        let admission = f.fill.dependencies.admission.clone();
        admission.reclaim_buffers();
        if case == "metadata" {
            copy.metadata.length = 4;
        }
        if case == "identity" {
            f.page.number = PageNumber(1);
        }
        if case == "corrupt" {
            let inner = Arc::get_mut(&mut copy.ciphertext.inner).unwrap();
            inner.bytes[0] ^= 1;
            inner.checksum.take();
        }
        let free = if case == "short" { 2 } else { 3 };
        let pressure = admission
            .reserve(
                None,
                ResourceClass::Plaintext,
                admission.limit(ResourceClass::Plaintext) - free,
            )
            .unwrap();
        let before = pressure.amount();
        if case == "canceled" {
            f.scope.cancel().unwrap();
        }
        let result = if case == "identity" {
            f.fill.reserve_copy_plaintext(&f.page, &copy).map(|_| ())
        } else {
            drive(
                f.fill.accept_selected(copy, &f.scope),
                &mut f.engine,
                &f.crypto,
            )
            .map(|p| {
                assert_eq!(p.plaintext.bytes(), b"abc");
            })
        };
        match case {
            "success" => assert!(result.is_ok(), "{result:?}"),
            "short" => assert_eq!(result, Err(Error::Overloaded)),
            "canceled" => assert_eq!(result, Err(Error::Cancelled)),
            _ => assert_eq!(result, Err(Error::CorruptRecord)),
        }
        assert_eq!(pressure.amount(), before);
        assert_eq!(f.crypto.outstanding(), 0);
        assert_eq!(f.origin.calls.get(), 0);
        if case != "success" {
            assert!(f.fill.dependencies.memory.get(&f.page).unwrap().is_none());
            admission.reclaim_buffers();
            assert_eq!(admission.used(ResourceClass::Plaintext), before);
        }
    }
}

#[test]
fn known_copy_reclaims_only_idle_bytes_after_validation() {
    let mut f = fixture();
    let copy = encrypted_copy(&mut f);
    let admission = f.fill.dependencies.admission.clone();
    admission.reclaim_buffers();
    let mut descriptor = copy.metadata.immutable();
    descriptor.version.etag = StrongEtag::test_value("busy");
    let busy = crate::memory::tests::bundle_for(&admission, descriptor.clone());
    f.fill.dependencies.memory.publish(busy.clone()).unwrap();
    descriptor.version.etag = StrongEtag::test_value("idle");
    let idle = crate::memory::tests::bundle_for(&admission, descriptor);
    let idle_id = idle.plaintext.page().clone();
    f.fill.dependencies.memory.publish(idle).unwrap();
    let pressure = admission
        .reserve(
            None,
            ResourceClass::Plaintext,
            admission.limit(ResourceClass::Plaintext) - admission.used(ResourceClass::Plaintext),
        )
        .unwrap();
    let mut wrong = copy.clone();
    wrong.metadata.length = 4;
    assert!(matches!(
        f.fill.reserve_copy_plaintext(&f.page, &wrong),
        Err(Error::CorruptRecord)
    ));
    assert!(f.fill.dependencies.memory.get(&idle_id).unwrap().is_some());
    let reservation = f.fill.reserve_copy_plaintext(&f.page, &copy).unwrap();
    assert_eq!(reservation.amount(), 3);
    assert!(f.fill.dependencies.memory.get(&idle_id).unwrap().is_none());
    assert!(
        f.fill
            .dependencies
            .memory
            .get(busy.plaintext.page())
            .unwrap()
            .is_some()
    );
    assert_eq!(busy.plaintext.bytes(), &[1; 3]);
    assert_eq!(
        admission.used(ResourceClass::Plaintext),
        pressure.amount() + 6
    );
}

#[test]
fn known_copy_full_page_still_requires_full_plaintext_admission() {
    let mut f = fixture_with(PAGE_BYTES, None);
    let admission = f.fill.dependencies.admission.clone();
    let plaintext = f.fill.reserve_bootstrap(&f.context.object.cache).unwrap();
    let plaintext = f
        .fill
        .dependencies
        .buffers
        .plaintext(plaintext, PAGE_BYTES as usize)
        .unwrap();
    let ciphertext = admission
        .reserve(
            Some(&f.context.object.cache),
            ResourceClass::Ciphertext,
            PAGE_BYTES as usize + 16,
        )
        .unwrap();
    let (_, ciphertext) = drive(
        f.fill
            .dependencies
            .crypto
            .encrypt(f.page.clone(), plaintext, ciphertext, &f.scope),
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    let copy = CiphertextCopy {
        metadata: f.origin.metadata.clone(),
        ciphertext,
    };
    admission.reclaim_buffers();
    let pressure = admission
        .reserve(
            None,
            ResourceClass::Plaintext,
            admission.limit(ResourceClass::Plaintext) - PAGE_BYTES as usize + 1,
        )
        .unwrap();
    assert!(matches!(
        f.fill.reserve_copy_plaintext(&f.page, &copy),
        Err(Error::Overloaded)
    ));
    drop(pressure);
    let reservation = f.fill.reserve_copy_plaintext(&f.page, &copy).unwrap();
    assert_eq!(reservation.amount(), PAGE_BYTES as usize);
}

fn enable_hedge(f: &mut Fixture, peers: &Rc<ScriptedPeers>) -> crate::telemetry::Metrics {
    peers.hedge.set(true);
    let metrics = crate::telemetry::Metrics::default();
    let hedges = crate::read::candidates::Hedges::new(
        crate::read::candidates::HedgeConfig {
            delay: Duration::from_nanos(1),
            slots: 1,
            bytes: crate::read::candidates::DUPLICATE_BYTES,
        },
        metrics.clone(),
    )
    .unwrap();
    let mut deps = f.fill.dependencies.clone();
    deps.candidates = Rc::new(
        CandidatePolicy::new(
            node(peers.local),
            Rc::new(Placement::new(16)),
            peers.requester(),
            deps.credentials.clone(),
            Arc::new(Default::default()),
        )
        .with_hedges(hedges),
    );
    f.fill = Fill::new(deps);
    metrics
}

#[test]
fn hedge_stalled_primary_copy_miss_keeps_time_and_credits_for_later_acquire() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for third in [false, true] {
        let clock = uring_runtime::environment::SimulationClock::new(920);
        let _env = clock.environment(0).enter();
        let mut f = fixture();
        f.scope = RequestScope::new(
            f.scope.request,
            uring_runtime::environment::now() + Duration::from_secs(30),
        )
        .unwrap();
        let (peers, ordered) = install_peers(&mut f, None);
        let good = encrypted_copy(&mut f);
        let metrics = enable_hedge(&mut f, &peers);
        peers.primary_polls.set(100);
        *peers.advance_primary.borrow_mut() = Some(clock.clone());
        peers.replies.borrow_mut().extend([
            (ordered[0].clone(), Reply::Miss),
            (ordered[1].clone(), Reply::Miss),
            (
                ordered[1].clone(),
                if third {
                    Reply::Miss
                } else {
                    Reply::Copy(good.clone())
                },
            ),
        ]);
        if third {
            peers
                .replies
                .borrow_mut()
                .push_back((ordered[2].clone(), Reply::Copy(good)));
        }
        let start = uring_runtime::environment::now();
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 10, 18);
        let result = acquire(&mut f, &mut budget).unwrap();
        assert_eq!(result.plaintext.bytes(), b"abc");
        assert!(peers.canceled_primary.get());
        assert!(uring_runtime::environment::now() < f.scope.deadline.0);
        assert!(uring_runtime::environment::now().duration_since(start) <= Duration::from_secs(12));
        let calls = peers.calls.borrow();
        assert_eq!(calls.len(), if third { 4 } else { 3 });
        assert!(calls[1].1);
        assert!(!calls[2].1);
        assert_eq!(calls[0].3, 1);
        assert_eq!(calls[1].3, 1);
        assert!(calls[2..].iter().all(|c| c.3 == 8));
        assert!(
            calls[2..].iter().all(|c| c.2 >= 1),
            "later Acquire keeps origin-attempt capacity"
        );
        assert!(calls.iter().all(|c| c.4 == f.scope.deadline.0));
        assert_eq!(metrics.count(Event::PageHedgeStarted), 1);
    }
}

#[test]
fn hedge_stale_membership_refreshes_once_without_fresh_credits() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for stale_again in [false, true] {
        for secondary_stale in [false, true] {
            let mut f = fixture();
            let (peers, ordered) = install_peers(&mut f, None);
            let good = encrypted_copy(&mut f);
            let metrics = enable_hedge(&mut f, &peers);
            let latest = Arc::new(
                Membership::validate(
                    racer_control_wire::MembershipVersion(2),
                    f.membership.members().to_vec(),
                )
                .unwrap(),
            );
            let mut deps = f.fill.dependencies.clone();
            deps.candidates = Rc::new(
                CandidatePolicy::new(
                    node(peers.local),
                    Rc::new(Placement::new(16)),
                    peers.requester(),
                    deps.credentials.clone(),
                    crate::control::PublishedState::for_membership(latest.clone()),
                )
                .with_hedges(deps.candidates.hedge_owner().unwrap().clone()),
            );
            f.fill = Fill::new(deps);
            let next = f
                .fill
                .dependencies
                .candidates
                .candidates(latest, &f.context.object, f.page.number)
                .unwrap()
                .ordered;
            if secondary_stale {
                peers.primary_polls.set(40);
                peers.replies.borrow_mut().extend([
                    (ordered[0].clone(), Reply::Miss),
                    (ordered[1].clone(), Reply::Stale),
                ]);
            } else {
                peers
                    .replies
                    .borrow_mut()
                    .push_back((ordered[0].clone(), Reply::Stale));
            }
            peers.replies.borrow_mut().extend([(
                next[0].clone(),
                if stale_again {
                    Reply::Stale
                } else {
                    Reply::Copy(good)
                },
            )]);
            let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 10, 18);
            let result = acquire(&mut f, &mut budget);
            if stale_again {
                assert!(matches!(result, Err(Error::IncompatibleMembership)));
            } else {
                assert_eq!(result.unwrap().plaintext.bytes(), b"abc");
            }
            assert_eq!(
                peers.calls.borrow().len(),
                if secondary_stale { 3 } else { 2 }
            );
            assert!(budget.remaining_attempts() < 10 && budget.remaining_links() < 18);
            assert_eq!(budget.deadline(), f.scope.deadline.0);
            assert_eq!(
                metrics.count(Event::PageHedgeStarted),
                u64::from(secondary_stale)
            );
        }
    }
}

#[test]
fn hedge_continuation_preserves_version_failure_and_never_restarts_consumed_primary() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    let (peers, ordered) = install_peers(&mut f, None);
    let metrics = enable_hedge(&mut f, &peers);
    peers.replies.borrow_mut().extend(
        ordered
            .iter()
            .cloned()
            .map(|node| (node, Reply::VersionUnavailable)),
    );
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 10, 18);
    assert!(matches!(
        acquire(&mut f, &mut budget),
        Err(Error::VersionUnavailable)
    ));
    assert_eq!(peers.calls.borrow().len(), 3);
    assert_eq!(metrics.count(Event::PageHedgeStarted), 0);
    assert_eq!(budget.deadline(), f.scope.deadline.0);
}

#[test]
fn hedge_default_budget_never_sends_underfunded_second_cold_fallback() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    let (peers, ordered) = install_peers(&mut f, None);
    let metrics = enable_hedge(&mut f, &peers);
    peers.primary_polls.set(40);
    peers.replies.borrow_mut().extend([
        (ordered[0].clone(), Reply::Miss),
        (ordered[1].clone(), Reply::Miss),
        (ordered[1].clone(), Reply::Miss),
    ]);
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
    assert!(matches!(
        acquire(&mut f, &mut budget),
        Err(Error::Unavailable)
    ));
    assert_eq!(peers.calls.borrow().len(), 3);
    assert_eq!(metrics.count(Event::PageHedgeStarted), 1);
    assert_eq!(
        (budget.remaining_attempts(), budget.remaining_links()),
        (2, 6)
    );
    assert_eq!(budget.deadline(), f.scope.deadline.0);
}

#[test]
fn hedge_cold_backup_coordinators_probe_predecessors_then_reach_origin_with_original_budget() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    use crate::peer::server::LocalPageService;
    use crate::read::Coordinator;
    struct Mesh {
        signing: Vec<Forwarding>,
        nodes: RefCell<Vec<Rc<Coordinator>>>,
        calls: RefCell<Vec<(NodeId, NodeId, bool, u32, u8)>>,
        unavailable: RefCell<Vec<NodeId>>,
        admissions: Vec<Rc<flow_control::Quotas<crate::admission::AdmissionPolicy>>>,
    }
    struct Peer {
        mesh: Rc<Mesh>,
        local: usize,
    }
    impl Peer {
        fn direct_hedge_available(
            &self,
            _: &std::sync::Arc<crate::topology::Membership>,
            _: &NodeId,
        ) -> bool {
            true
        }
        fn request_direct<'a>(
            &'a self,
            r: PeerRequest,
            m: std::sync::Arc<crate::topology::Membership>,
            s: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            self.request(r, m, s)
        }
        fn request<'a>(
            &'a self,
            request: PeerRequest,
            membership: std::sync::Arc<crate::topology::Membership>,
            scope: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            Box::pin(async move {
                let destination = request.route.destination.clone();
                let copy = matches!(
                    request.operation,
                    PeerOperation::Page {
                        mode: FetchMode::CopyOnly,
                        ..
                    }
                );
                self.mesh.calls.borrow_mut().push((
                    node(self.local),
                    destination.clone(),
                    copy,
                    request.route.remaining_attempts,
                    request.route.remaining_links,
                ));
                let remote = (0..4).find(|i| node(*i) == destination).unwrap();
                let (signed, binding) = self.mesh.signing[self.local].sign_request(request)?;
                let admitted = self.mesh.signing[remote].verify_request(signed)?;
                let reply_binding = admitted.binding().clone();
                if self.mesh.calls.borrow().len() == 1 {
                    let mut polls = 0;
                    std::future::poll_fn(|cx| {
                        polls += 1;
                        if polls >= 4 {
                            Poll::Ready(())
                        } else {
                            cx.waker().wake_by_ref();
                            Poll::Pending
                        }
                    })
                    .await;
                }
                let response = if self.mesh.unavailable.borrow().contains(&destination) {
                    PeerResponse::Unavailable
                } else {
                    let coordinator = self.mesh.nodes.borrow()[remote].clone();
                    coordinator.serve_peer(admitted, membership, scope).await?
                };
                // Match wire reception: the requester owns ciphertext admission,
                // not the destination's allocation from this in-process mesh.
                let response = match response {
                    PeerResponse::Page {
                        metadata,
                        ciphertext,
                    } => {
                        let admission = &self.mesh.admissions[self.local];
                        let copy = crate::memory::BufferPool::new(admission.clone()).ciphertext(
                            admission.reserve(
                                Some(&metadata.version.object.cache),
                                ResourceClass::Ciphertext,
                                ciphertext.bytes().len(),
                            )?,
                            ciphertext.envelope().clone(),
                            ciphertext.bytes().to_vec(),
                        )?;
                        PeerResponse::Page {
                            metadata,
                            ciphertext: copy,
                        }
                    }
                    other => other,
                };
                let response = self.mesh.signing[remote].sign_response(&reply_binding, response);
                let response = response?;
                self.mesh.signing[self.local].verify_response(response, &binding)
            })
        }
    }
    for target_rank in [1usize, 2] {
        let hedge_metrics = crate::telemetry::Metrics::default();
        let mut fixtures: Vec<_> = (0..4).map(|_| fixture()).collect();
        let (_, ordered) = install_peers(&mut fixtures[0], None);
        let membership = fixtures[0].membership.clone();
        let source = (0..4).find(|i| !ordered.contains(&node(*i))).unwrap();
        let target = (0..4).find(|i| node(*i) == ordered[target_rank]).unwrap();
        let mesh = Rc::new(Mesh {
            signing: network(4).into_iter().map(Forwarding::new).collect(),
            nodes: RefCell::new(Vec::new()),
            calls: RefCell::new(Vec::new()),
            unavailable: RefCell::new(ordered[..target_rank].to_vec()),
            admissions: fixtures
                .iter()
                .map(|f| f.fill.dependencies.admission.clone())
                .collect(),
        });
        let mut endpoints = Vec::new();
        for (i, f) in fixtures.iter_mut().enumerate() {
            f.membership = membership.clone();
            let peers = Rc::new(Peer {
                mesh: mesh.clone(),
                local: i,
            });
            let mut policy = CandidatePolicy::new(
                node(i),
                Rc::new(Placement::new(16)),
                Requester::scripted(
                    peers.clone(),
                    Peer::direct_hedge_available,
                    Peer::request,
                    Peer::request_direct,
                ),
                f.fill.dependencies.credentials.clone(),
                Arc::new(Default::default()),
            );
            if i == source {
                policy = policy.with_hedges(
                    crate::read::candidates::Hedges::new(
                        crate::read::candidates::HedgeConfig {
                            slots: 1,
                            delay: Duration::from_nanos(1),
                            bytes: crate::read::candidates::DUPLICATE_BYTES,
                        },
                        hedge_metrics.clone(),
                    )
                    .unwrap(),
                );
            }
            let mut deps = f.fill.dependencies.clone();
            deps.candidates = Rc::new(policy);
            f.fill = Fill::new(deps);
            let (coordinator, endpoint, _) = f.read_graph(
                Rc::new(f.fill.clone()),
                &membership,
                ReadGraphSettings {
                    name: "cold",
                    snapshots: 4,
                    metadata: 16,
                    window: 1,
                    stall: Duration::from_secs(10),
                    seed: None,
                },
            );
            endpoints.push(endpoint);
            mesh.nodes.borrow_mut().push(coordinator);
        }
        let (attempts, links) = if target_rank == 1 { (8, 16) } else { (10, 18) };
        let mut budget = if target_rank == 1 {
            crate::read::range_stream::client_page_budget_for_test(
                fixtures[source].scope.deadline.0,
            )
        } else {
            AcquisitionBudget::new(fixtures[source].scope.deadline.0, attempts, links)
        };
        assert_eq!(
            (budget.remaining_attempts(), budget.remaining_links()),
            (attempts, links)
        );
        let source_fill = fixtures[source].fill.clone();
        let page = fixtures[source].page.clone();
        let context = OriginContext {
            object: fixtures[source].context.object.clone(),
            metadata: None,
            authorization: None,
        };
        let scope = fixtures[source].scope.clone();
        let mut read =
            Box::pin(source_fill.acquire(page, membership, &context, &scope, &mut budget));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut result = None;
        for _ in 0..512 {
            uring_runtime::drivers::poll(&mut cx, 64);
            for f in &mut fixtures {
                uring_runtime::group::Service::poll_budgeted(
                    &mut f.engine,
                    &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
                    64,
                )
                .unwrap();
                f.crypto.poll_budgeted(64).unwrap();
            }
            if let Poll::Ready(done) = read.as_mut().poll(&mut cx) {
                result = Some(done);
                break;
            }
        }
        assert_eq!(
            result
                .expect("bounded cold coordinator read")
                .unwrap_or_else(|e| panic!(
                    "cold rank{target_rank} failed {e:?}; calls={:?}; origin={:?}",
                    mesh.calls.borrow(),
                    fixtures
                        .iter()
                        .map(|f| f.origin.calls.get())
                        .collect::<Vec<_>>()
                ))
                .plaintext
                .bytes(),
            b"abc"
        );
        drop(read);
        assert_eq!(fixtures[target].origin.calls.get(), 1);
        assert_eq!(
            hedge_metrics.count(Event::PageHedgeStarted),
            1,
            "real 8/16 rank1 read launches optional hedge"
        );
        for (i, f) in fixtures.iter().enumerate() {
            if i != target {
                assert_eq!(f.origin.calls.get(), 0);
            }
        }
        let calls = mesh.calls.borrow();
        assert_eq!(
            calls
                .iter()
                .filter(|(from, _, copy, _, _)| *from == node(target) && *copy)
                .count(),
            target_rank
        );
        assert!(
            calls
                .iter()
                .any(|(from, to, copy, attempts, links)| *from == node(source)
                    && *to == node(target)
                    && !copy
                    && *attempts == target_rank as u32 + 1
                    && *links == 8)
        );
        assert!(budget.remaining_attempts() <= attempts && budget.remaining_links() <= links);
        drop(endpoints);
    }
}

#[test]
fn hedge_full_page_exact_plaintext_quotas_suppress_before_spending_serial_credits() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for pages in [2, 3, 4] {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.plaintext_bytes = std::num::NonZeroUsize::new(PAGE_BYTES as usize * pages).unwrap();
        let mut f = fixture_with(PAGE_BYTES, Some(limits));
        let (peers, ordered) = install_peers(&mut f, None);
        let plaintext = f.fill.reserve_bootstrap(&f.context.object.cache).unwrap();
        let ciphertext = f
            .fill
            .dependencies
            .admission
            .reserve(
                Some(&f.context.object.cache),
                ResourceClass::Ciphertext,
                PAGE_BYTES as usize + 16,
            )
            .unwrap();
        let plaintext = f
            .fill
            .dependencies
            .buffers
            .plaintext(plaintext, PAGE_BYTES as usize)
            .unwrap();
        let (plain, ciphertext) = drive(
            f.fill
                .dependencies
                .crypto
                .encrypt(f.page.clone(), plaintext, ciphertext, &f.scope),
            &mut f.engine,
            &f.crypto,
        )
        .unwrap();
        drop(plain);
        let copy = CiphertextCopy {
            metadata: f.origin.metadata.clone(),
            ciphertext,
        };
        peers
            .replies
            .borrow_mut()
            .push_back((ordered[0].clone(), Reply::Copy(copy.clone())));
        let metrics = enable_hedge(&mut f, &peers);
        if pages == 4 {
            peers.primary_polls.set(40);
            peers
                .replies
                .borrow_mut()
                .push_back((ordered[1].clone(), Reply::Copy(copy)));
        }
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 10, 18);
        let result = acquire(&mut f, &mut budget).unwrap();
        assert_eq!(result.plaintext.bytes().len(), PAGE_BYTES as usize);
        assert_eq!(peers.calls.borrow().len(), if pages == 4 { 2 } else { 1 });
        assert_eq!(
            peers.calls.borrow()[0].3,
            if pages == 4 { 1 } else { 4 },
            "serial path keeps original credits"
        );
        assert_eq!(
            metrics.count(Event::PageHedgeStarted),
            u64::from(pages == 4)
        );
        assert_eq!(
            metrics.count(Event::PageHedgeSuppressed),
            u64::from(pages != 4)
        );
    }
}

#[test]
fn hedge_authenticated_metadata_conflict_cannot_win_over_retained_descriptor() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for content_type in [false, true] {
        let mut f = fixture_with(PAGE_BYTES + 3, None);
        let (peers, ordered) = install_peers(&mut f, None);
        f.page.number = crate::model::PageNumber(1);
        let mut retained = encrypted_copy(&mut f);
        retained.metadata.content_type =
            Some(crate::model::ContentType::parse(b"application/expected").unwrap());
        f.fill
            .dependencies
            .memory
            .publish_ciphertext(UnverifiedPage {
                copy: retained.clone(),
                disk_token: None,
            })
            .unwrap();
        f.page.number = crate::model::PageNumber(0);
        let plaintext = f.fill.reserve_bootstrap(&f.context.object.cache).unwrap();
        let ciphertext = f
            .fill
            .dependencies
            .admission
            .reserve(
                Some(&f.context.object.cache),
                ResourceClass::Ciphertext,
                PAGE_BYTES as usize + 16,
            )
            .unwrap();
        let plaintext = f
            .fill
            .dependencies
            .buffers
            .plaintext(plaintext, PAGE_BYTES as usize)
            .unwrap();
        let (plain, ciphertext) = drive(
            f.fill
                .dependencies
                .crypto
                .encrypt(f.page.clone(), plaintext, ciphertext, &f.scope),
            &mut f.engine,
            &f.crypto,
        )
        .unwrap();
        drop(plain);
        let good = CiphertextCopy {
            metadata: retained.metadata.clone(),
            ciphertext,
        };
        let mut bad = good.clone();
        if content_type {
            bad.metadata.content_type =
                Some(crate::model::ContentType::parse(b"application/conflict").unwrap());
        } else {
            bad.metadata.length += 1;
        }
        let metrics = enable_hedge(&mut f, &peers);
        peers.primary_polls.set(40);
        peers.replies.borrow_mut().extend([
            (ordered[0].clone(), Reply::Copy(good)),
            (ordered[1].clone(), Reply::Copy(bad)),
        ]);
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 10, 18);
        let result = acquire(&mut f, &mut budget).unwrap();
        assert_eq!(result.metadata.length, PAGE_BYTES + 3);
        assert_eq!(result.metadata.content_type, retained.metadata.content_type);
        assert_eq!(metrics.count(Event::PageHedgeWon), 0);
        assert!(!peers.canceled_primary.get());
        assert_eq!(peers.calls.borrow().len(), 2);
    }
}

#[test]
fn hedge_loser_child_cancels_accepted_crypto_but_waits_for_completion_fence() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    let (peers, ordered) = install_peers(&mut f, None);
    let good = encrypted_copy(&mut f);
    let metrics = enable_hedge(&mut f, &peers);
    peers.replies.borrow_mut().extend([
        (ordered[0].clone(), Reply::Copy(good.clone())),
        (ordered[1].clone(), Reply::Copy(good)),
    ]);
    let candidates = crate::topology::Candidates {
        membership: f.membership.clone(),
        ordered,
    };
    let operation = PeerOperation::Page {
        page: f.page.clone(),
        mode: FetchMode::Acquire,
    };
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 10, 18);
    let mut continuation = crate::read::candidates::HedgeContinuation::default();
    let scopes = RefCell::new(Vec::<RequestScope>::new());
    let primary_canceled = Cell::new(false);
    let mut pair = Box::pin(f.fill.dependencies.candidates.hedge_page(
        &candidates,
        &f.context,
        &operation,
        &f.scope,
        &mut budget,
        &f.fill.dependencies.admission,
        &mut continuation,
        |response, child| {
            let first = scopes.borrow().is_empty();
            scopes.borrow_mut().push(child.clone());
            let fill = &f.fill;
            let page = &f.page;
            let canceled = &primary_canceled;
            Box::pin(async move {
                if first {
                    let result = fill.decrypt_response(page, response, None, &child).await;
                    canceled.set(matches!(result, Err(Error::Cancelled)));
                    result
                } else {
                    Ok(crate::read::tests::page(2))
                }
            })
        },
    ));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(pair.as_mut().poll(&mut cx).is_pending());
    assert_eq!(
        f.crypto.outstanding(),
        1,
        "primary crypto accepted before winner"
    );
    assert_eq!(scopes.borrow().len(), 2);
    assert!(scopes.borrow()[0].cancellation.is_cancelled());
    assert!(!f.scope.cancellation.is_cancelled());
    assert!(
        pair.as_mut().poll(&mut cx).is_pending(),
        "cancellation is not the crypto fence"
    );
    uring_runtime::group::Service::poll_budgeted(
        &mut f.engine,
        &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
        8,
    )
    .unwrap();
    assert!(
        pair.as_mut().poll(&mut cx).is_pending(),
        "engine completion must be consumed"
    );
    f.crypto.poll_budgeted(8).unwrap();
    assert!(matches!(
        pair.as_mut().poll(&mut cx),
        Poll::Ready(Ok(Some(page))) if page.plaintext.bytes() == [2]
    ));
    assert!(primary_canceled.get());
    assert_eq!(f.crypto.outstanding(), 0);
    assert_eq!(metrics.count(Event::PageHedgeWon), 1);
}

#[test]
fn hedge_child_cancellation_removes_crypto_admission_wait_without_canceling_accepted_job() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.queue_entries = std::num::NonZeroUsize::new(1).unwrap();
    let mut f = fixture_with(3, Some(limits));
    let copy = encrypted_copy(&mut f);
    let child = RequestScope::new(f.scope.request, f.scope.deadline.0).unwrap();
    let mut accepted = Box::pin(f.fill.decrypt(
        &f.page,
        copy.clone(),
        f.fill.reserve_bootstrap(&f.context.object.cache).unwrap(),
        &f.scope,
        DecryptSource::Peer,
    ));
    let mut waiting = Box::pin(f.fill.decrypt(
        &f.page,
        copy,
        f.fill.reserve_bootstrap(&f.context.object.cache).unwrap(),
        &child,
        DecryptSource::Peer,
    ));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(accepted.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.crypto.outstanding(), 1);
    assert!(waiting.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.crypto.outstanding(), 1);
    child.cancel().unwrap();
    assert!(matches!(
        waiting.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    ));
    drop(waiting);
    assert!(!f.scope.cancellation.is_cancelled());
    assert_eq!(f.crypto.outstanding(), 1);
    uring_runtime::group::Service::poll_budgeted(
        &mut f.engine,
        &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
        8,
    )
    .unwrap();
    f.crypto.poll_budgeted(8).unwrap();
    assert!(matches!(
        accepted.as_mut().poll(&mut cx),
        Poll::Ready(Ok(_))
    ));
    drop(accepted);
    assert_eq!(f.crypto.outstanding(), 0);
}

fn unusable_copy(f: &Fixture, good: &CiphertextCopy, missing_key: bool) -> CiphertextCopy {
    let mut envelope = good.ciphertext.envelope().clone();
    let mut bytes = good.ciphertext.bytes().to_vec();
    if missing_key {
        envelope.key_id = racer_control_wire::KeyId([99; 16]);
    } else {
        bytes[0] ^= 1;
    }
    let ciphertext = f
        .fill
        .dependencies
        .buffers
        .ciphertext(
            f.fill
                .dependencies
                .admission
                .reserve(
                    Some(&f.context.object.cache),
                    ResourceClass::Ciphertext,
                    bytes.len(),
                )
                .unwrap(),
            envelope,
            bytes,
        )
        .unwrap();
    let copy = CiphertextCopy {
        metadata: good.metadata.clone(),
        ciphertext,
    };
    validate_copy(&copy, &f.page).unwrap(); // Structural checks cannot detect flipped ciphertext.
    copy
}

#[test]
fn corrupt_disk_decrypt_is_counted_without_publishing_plaintext() {
    let mut f = fixture();
    let good = encrypted_copy(&mut f);
    let bad = unusable_copy(&f, &good, false);
    let dirty = f
        .fill
        .dependencies
        .admission
        .reserve(
            Some(&f.context.object.cache),
            ResourceClass::DirtyCiphertext,
            bad.ciphertext.bytes().len(),
        )
        .unwrap();
    f.fill.dependencies.writer.enqueue(bad, dirty).unwrap();
    f.reactor.init().unwrap();
    futures::executor::block_on(f.fill.dependencies.writer.open()).unwrap();
    drive_disk(&f, f.fill.dependencies.writer.progress(1, &f.scope)).unwrap();
    let result = drive_io(
        f.fill.acquire_local_copy(&f.page, &f.scope, true),
        &f.reactor,
        &mut f.engine,
        &f.crypto,
    )
    .unwrap();
    assert!(result.is_none());
    assert_eq!(f.fill.metrics.count(Event::FillDecryptDiskCorrupt), 1);
    assert_eq!(f.fill.metrics.count(Event::FillDecryptRetainedCorrupt), 0);
    assert_eq!(f.fill.metrics.count(Event::FillDecryptPeerCorrupt), 0);
    assert_unpublished(&f);
    assert_eq!(f.origin.calls.get(), 0);
}

fn assert_published(f: &Fixture, result: &PageResult, persist: bool) {
    result.validate_for(&f.page).unwrap();
    assert_eq!(result.plaintext.bytes(), b"abc");
    let cached = f.fill.dependencies.memory.get(&f.page).unwrap().unwrap();
    assert_eq!(cached.ciphertext.bytes(), result.ciphertext.bytes());
    let pending = f.fill.dependencies.writer.copy_only(&f.page).unwrap();
    assert_eq!(pending.is_some(), persist);
    if let Some(copy) = pending {
        assert_eq!(copy.ciphertext.bytes(), result.ciphertext.bytes());
    }
    assert_eq!(f.crypto.outstanding(), 0);
    assert_eq!(uring_runtime::drivers::pending(), 0);
}

fn assert_unpublished(f: &Fixture) {
    assert!(f.fill.dependencies.memory.get(&f.page).unwrap().is_none());
    assert!(
        f.fill
            .dependencies
            .writer
            .copy_only(&f.page)
            .unwrap()
            .is_none()
    );
    assert_eq!(
        f.fill.dependencies.admission.used(ResourceClass::Plaintext),
        0
    );
    assert_eq!(
        f.fill
            .dependencies
            .admission
            .used(ResourceClass::DirtyCiphertext),
        0
    );
    assert_eq!(f.crypto.outstanding(), 0);
}

#[test]
fn unusable_peer_copy_advances_to_alternate_or_authorized_origin() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for missing_key in [false, true] {
        for (rank, use_origin) in [(None, false), (Some(2), false), (Some(2), true)] {
            let mut f = fixture();
            let (peers, ordered) = install_peers(&mut f, rank);
            let good = encrypted_copy(&mut f);
            let bad = unusable_copy(&f, &good, missing_key);
            peers.replies.borrow_mut().extend([
                (ordered[0].clone(), Reply::Copy(bad.clone())),
                (
                    ordered[1].clone(),
                    Reply::Copy(if use_origin { bad } else { good.clone() }),
                ),
            ]);
            let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
            let result = acquire(&mut f, &mut budget).unwrap();
            assert_published(&f, &result, rank.is_some());
            assert_eq!(f.origin.calls.get(), usize::from(use_origin));
            assert_eq!(
                f.fill.metrics.count(Event::OriginFill),
                u64::from(use_origin)
            );
            assert_eq!(f.fill.metrics.count(Event::PeerHit), u64::from(!use_origin));
            assert_eq!(
                f.fill.metrics.count(Event::FillDecryptPeerCorrupt),
                if missing_key {
                    0
                } else if use_origin {
                    2
                } else {
                    1
                }
            );
            assert_eq!(f.fill.metrics.count(Event::FillDecryptDiskCorrupt), 0);
            assert_eq!(f.fill.metrics.count(Event::FillDecryptRetainedCorrupt), 0);
            assert_eq!(
                f.fill.metrics.count(Event::CorruptMiss),
                if missing_key {
                    0
                } else if use_origin {
                    2
                } else {
                    1
                }
            );
            assert_eq!(f.fill.metrics.gauge(Gauge::ActiveFills), 0);
            if !use_origin {
                assert_eq!(result.ciphertext.bytes(), good.ciphertext.bytes());
            }
            let calls = peers.calls.borrow();
            assert_eq!(calls.len(), 2);
            assert!(
                calls
                    .iter()
                    .all(|(_, copy, _, links, deadline)| *copy == rank.is_some()
                        && *links == 4
                        && *deadline == f.scope.deadline.0)
            );
            // Both probes retain later candidates or local origin as a fallback.
            // Only local time shares advance; signed authority never renews.
            let deadlines = peers.local_deadlines.borrow();
            assert_eq!(deadlines.len(), 2);
            assert!(deadlines.iter().all(|(overall, share)| *share < *overall));
            assert!(deadlines[0].1 < deadlines[1].1);
            let spent: u32 = calls.iter().map(|(_, _, credits, _, _)| 1 + credits).sum();
            assert_eq!(
                budget.remaining_attempts(),
                8 - spent - f.origin.calls.get() as u32
            );
            assert_eq!(budget.remaining_links(), 8);
            assert_eq!(budget.deadline(), f.scope.deadline.0);
        }
    }
}

#[test]
fn peer_ciphertext_from_another_version_cannot_be_published_under_the_pin() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    let (peers, ordered) = install_peers(&mut f, Some(1));
    let pinned = f.page.version.etag.clone();
    f.page.version.etag = StrongEtag::test_value("different-version");
    let other = encrypted_copy(&mut f);
    f.page.version.etag = pinned;
    // The signed response claims the requested pin but the AEAD bytes were
    // encrypted under another version. Structural and signature checks pass.
    let mut envelope = other.ciphertext.envelope().clone();
    envelope.page = f.page.clone();
    let bytes = other.ciphertext.bytes().to_vec();
    let ciphertext = f
        .fill
        .dependencies
        .buffers
        .ciphertext(
            f.fill
                .dependencies
                .admission
                .reserve(
                    Some(&f.context.object.cache),
                    ResourceClass::Ciphertext,
                    bytes.len(),
                )
                .unwrap(),
            envelope,
            bytes,
        )
        .unwrap();
    peers.replies.borrow_mut().push_back((
        ordered[0].clone(),
        Reply::Copy(CiphertextCopy {
            metadata: f.origin.metadata.clone(),
            ciphertext,
        }),
    ));
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
    let result = acquire(&mut f, &mut budget).unwrap();
    assert_published(&f, &result, true);
    assert_eq!(f.origin.calls.get(), 1);
    assert_eq!(peers.calls.borrow().len(), 1);
    assert_eq!(result.metadata.version, f.page.version);
    assert_ne!(result.ciphertext.bytes(), other.ciphertext.bytes());
}

#[test]
fn origin_version_miss_checks_every_later_copy_without_claiming_false_absence() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for healthy_last in [false, true] {
        let mut f = fixture();
        let (peers, ordered) = install_peers(&mut f, Some(0));
        f.origin.version_unavailable.set(true);
        let good = encrypted_copy(&mut f);
        let bad = unusable_copy(&f, &good, false);
        peers.replies.borrow_mut().extend([
            (ordered[1].clone(), Reply::Copy(bad.clone())),
            (
                ordered[2].clone(),
                Reply::Copy(if healthy_last { good.clone() } else { bad }),
            ),
        ]);
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
        let result = acquire(&mut f, &mut budget);
        if healthy_last {
            let result = result.unwrap();
            assert_eq!(result.ciphertext.bytes(), good.ciphertext.bytes());
            assert_published(&f, &result, true);
        } else {
            assert!(matches!(result, Err(Error::Unavailable)));
            assert_unpublished(&f);
        }
        assert_eq!(f.origin.calls.get(), 1);
        assert_eq!(peers.calls.borrow().len(), 2);
        assert!(
            peers
                .calls
                .borrow()
                .iter()
                .all(|(_, copy, credits, _, _)| *copy && *credits == 0)
        );
        assert_eq!(budget.remaining_attempts(), 5);
        assert_eq!(budget.remaining_links(), 8);
    }
}

#[test]
fn corrupt_predecessor_evidence_survives_origin_and_later_copy_misses() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    let (peers, ordered) = install_peers(&mut f, Some(1));
    f.origin.version_unavailable.set(true);
    let good = encrypted_copy(&mut f);
    let bad = unusable_copy(&f, &good, false);
    peers.replies.borrow_mut().extend([
        (ordered[0].clone(), Reply::Copy(bad)),
        (ordered[2].clone(), Reply::Miss),
    ]);
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
    assert!(matches!(
        acquire(&mut f, &mut budget),
        Err(Error::Unavailable)
    ));
    assert_eq!(f.origin.calls.get(), 1);
    assert_eq!(peers.calls.borrow().len(), 2);
    assert_eq!(budget.remaining_attempts(), 5);
    assert_unpublished(&f);
}

#[test]
fn corrupt_copies_exhaust_sources_or_original_credits_without_origin_for_noncandidate() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    for (attempts, links, expected_calls, expected_error) in [
        (8, 16, 3, Error::Unavailable),
        (1, 16, 1, Error::Unavailable),
        (8, 4, 1, Error::HopBudgetExhausted),
    ] {
        let mut f = fixture();
        let (peers, ordered) = install_peers(&mut f, None);
        let good = encrypted_copy(&mut f);
        let bad = unusable_copy(&f, &good, false);
        peers.replies.borrow_mut().extend(
            ordered
                .iter()
                .map(|n| (n.clone(), Reply::Copy(bad.clone()))),
        );
        let mut budget = AcquisitionBudget::new(f.scope.deadline.0, attempts, links);
        assert!(matches!(acquire(&mut f, &mut budget), Err(error) if error == expected_error));
        let calls = peers.calls.borrow();
        assert_eq!(calls.len(), expected_calls);
        assert_eq!(
            budget.remaining_attempts()
                + calls
                    .iter()
                    .map(|(_, _, credits, _, _)| 1 + credits)
                    .sum::<u32>(),
            attempts
        );
        assert_eq!(
            budget.remaining_links() + calls.iter().map(|(_, _, _, links, _)| links).sum::<u8>(),
            links
        );
        assert_eq!(f.origin.calls.get(), 0);
        assert_unpublished(&f);
    }
}

#[test]
fn unauthorized_after_corrupt_copy_is_terminal_not_origin_evidence() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    let (peers, ordered) = install_peers(&mut f, Some(2));
    let good = encrypted_copy(&mut f);
    let bad = unusable_copy(&f, &good, false);
    peers.replies.borrow_mut().extend([
        (ordered[0].clone(), Reply::Copy(bad)),
        (ordered[1].clone(), Reply::Unauthorized),
    ]);
    let mut budget = AcquisitionBudget::new(f.scope.deadline.0, 8, 16);
    assert!(matches!(
        acquire(&mut f, &mut budget),
        Err(Error::Unauthorized)
    ));
    assert_eq!(f.origin.calls.get(), 0);
    assert_eq!(peers.calls.borrow().len(), 2);
    assert_eq!(budget.remaining_attempts(), 6);
    assert_unpublished(&f);
}

#[test]
fn corrupt_copy_cannot_extend_original_budget_deadline() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    let mut f = fixture();
    let (peers, ordered) = install_peers(&mut f, Some(2));
    let good = encrypted_copy(&mut f);
    let bad = unusable_copy(&f, &good, false);
    peers
        .replies
        .borrow_mut()
        .push_back((ordered[0].clone(), Reply::Copy(bad)));
    let deadline = Instant::now() + Duration::from_millis(100);
    let mut budget = AcquisitionBudget::new(deadline, 8, 16);
    let mut future = Box::pin(f.fill.acquire_once(
        &f.page,
        f.membership.clone(),
        &f.context,
        &f.scope,
        &mut budget,
        true,
    ));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.crypto.outstanding(), 1);
    // Complete AEAD while live, then resume fallback after the tighter budget
    // deadline. The caller scope is still live and must not renew that deadline.
    uring_runtime::group::Service::poll_budgeted(
        &mut f.engine,
        &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
        64,
    )
    .unwrap();
    std::thread::sleep(deadline.saturating_duration_since(Instant::now()));
    assert!(matches!(
        drive(future, &mut f.engine, &f.crypto),
        Err(Error::DeadlineExceeded)
    ));
    assert_eq!(peers.calls.borrow().len(), 1);
    assert_eq!(budget.remaining_attempts(), 7);
    assert_eq!(budget.remaining_links(), 12);
    assert_eq!(budget.deadline(), deadline);
    assert_eq!(f.origin.calls.get(), 0);
    assert_unpublished(&f);
}
