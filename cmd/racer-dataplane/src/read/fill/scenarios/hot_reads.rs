//! Production coordinator/range/selection/Fill graph across authenticated nodes.
use super::*;
use crate::{
    client::{ClientRequest, ReadKind},
    control::{
        snapshot::{PublishedState, SnapshotStore},
        wire::*,
    },
    http::{Codec, io::HttpIo},
    memory::{delivery::Delivery, pipe::PipePool},
    model::{ByteRange, MembershipVersion},
    peer::{
        PeerNetwork, PeerTransport,
        server::{LocalPageService, PeerServer},
        wire::{PeerRequest, SignedRequest, SignedResponse, VerifiedResponse},
    },
    read::{
        Coordinator, ReadService,
        metadata::{MetadataDependencies, MetadataService},
        range_stream::RangeStreams,
    },
    security::{
        forwarding::Forwarding,
        signing::{
            Signatures,
            tests::{network, node},
        },
    },
    topology::{health::LinkHealth, paths::Paths},
};

struct Link {
    client: Rc<dyn PeerClient>,
    demands: Rc<RefCell<Vec<u64>>>,
}
impl PeerClient for Link {
    fn request<'a>(
        &'a self,
        request: PeerRequest,
        membership: MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async move {
            let PeerOperation::Subscribe { subscription, .. } = &request.operation else {
                panic!("real range must emit Subscribe")
            };
            self.demands
                .borrow_mut()
                .push(subscription.demand.page_count());
            self.client.request(request, membership, scope).await
        })
    }
}
impl PeerTransport for NoPeer {
    fn exchange<'a>(
        &'a self,
        _: SignedRequest,
        _: MembershipLease,
        _: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        Box::pin(async { panic!("no speculative relay") })
    }
}
struct Gate {
    local: Rc<Coordinator>,
    blocked: Cell<bool>,
    calls: RefCell<Vec<u64>>,
}
impl LocalPageService for Gate {
    fn serve_peer<'a>(
        &'a self,
        request: crate::peer::wire::VerifiedRequest,
        membership: MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        Box::pin(async move {
            let PeerOperation::Page { page, .. } = &request.request().operation else {
                panic!()
            };
            self.calls.borrow_mut().push(page.number.0);
            std::future::poll_fn(|cx| {
                cx.waker().wake_by_ref();
                if self.blocked.get() {
                    Poll::Pending
                } else {
                    Poll::Ready(())
                }
            })
            .await;
            Ok(self
                .local
                .serve_peer(request, membership, scope)
                .await
                .expect("provider Fill"))
        })
    }
}
fn coordinator(
    f: &Fixture,
    signer: &Rc<Signatures>,
    membership: &MembershipLease,
    peers: Rc<dyn PeerClient>,
) -> (
    Rc<Coordinator>,
    crate::read::dispatch::WorkerEndpoint,
    Arc<PublishedState>,
) {
    let published = Arc::new(PublishedState::default());
    let snapshots = Rc::new(SnapshotStore::new(
        f.keys.cluster().clone(),
        published.clone(),
        2,
    ));
    let (client_socket, origin_socket) =
        crate::control::caches::canonical_socket_paths("hot-read").unwrap();
    snapshots
        .publish(Publication {
            schema_version: SCHEMA_VERSION,
            cluster: f.keys.cluster().clone(),
            sequence: PublicationSequence(1),
            membership_version: membership.version,
            members: membership.members().to_vec(),
            caches: vec![crate::control::caches::CacheDefinition {
                id: f.context.object.cache.clone(),
                name: "hot-read".into(),
                client_socket,
                origin_socket,
            }],
        })
        .unwrap();
    let mut deps = f.fill.dependencies.clone();
    deps.candidates = Rc::new(CandidatePolicy::new(
        signer.node().clone(),
        Rc::new(Placement::new(64)),
        peers.clone(),
    ));
    let owners = deps.metadata_owner.clone();
    let fill = Rc::new(Fill::new(deps));
    let metadata = Rc::new(MetadataService::new(
        fill.dependencies.candidates.clone(),
        f.origin.clone(),
        fill.dependencies.credentials.clone(),
        32,
        MetadataDependencies {
            index: Rc::new(Index::new(WorkerId(0), 32)),
            owners: owners.clone(),
            fill: fill.clone(),
        },
    ));
    metadata
        .publish_version(f.origin.metadata.immutable())
        .unwrap();
    let delivery = Rc::new(Delivery::new(
        Rc::new(PipePool::new(
            fill.dependencies.admission.clone(),
            f.reactor.clone(),
        )),
        Duration::from_secs(30),
    ));
    let streams = Rc::new(RangeStreams::new(owners.clone(), delivery, 2));
    let local = Rc::new(Coordinator::new(
        snapshots,
        metadata,
        fill.clone(),
        streams,
        fill.dependencies.credentials.clone(),
    ));
    let endpoint = owners.install(WorkerId(0), local.clone()).unwrap();
    (local, endpoint, published)
}

#[test]
fn ordered_acquisitions_overlap_delivery_share_work_and_bound_reordering() {
    let signers = network(1);
    let mut f = fixture_with(3 * PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            vec![Member {
                node: signers[0].node().clone(),
                shares: NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:8000".into(),
                rails: vec![],
                alignment_enabled: false,
            }],
        )
        .unwrap(),
    );
    let (local, mut endpoint, _publication) =
        coordinator(&f, &signers[0], &membership, Rc::new(NoPeer));
    let open = |ordered, credits, start| {
        let request = ClientRequest {
            kind: ReadKind::Subscription {
                pin: Some(f.page.version.etag.clone()),
                range: Some(ByteRange::From(start)),
                page_credits: credits,
                byte_credits: PAGE_BYTES,
                ordered,
            },
            origin: OriginContext {
                object: f.context.object.clone(),
                metadata: None,
                authorization: None,
            },
        };
        let scope = RequestScope::new(f.scope.request, f.scope.deadline.0).unwrap();
        futures::executor::block_on(local.read(request, &scope))
            .unwrap()
            .body
            .unwrap()
    };
    // A one-byte first slice plus a whole page exceeds the byte credit despite
    // two available page slots. No page-one work may start yet.
    let mut a = open(true, 2, PAGE_BYTES - 1);
    let mut slow = open(true, 1, PAGE_BYTES - 1);
    let mut unordered = open(false, 1, PAGE_BYTES - 1);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut pump = |endpoint: &mut crate::read::dispatch::WorkerEndpoint| {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        endpoint.poll(&mut cx, 64).unwrap();
        crate::read::drivers::poll(&mut cx, 64);
        f.engine.poll_budgeted(64).unwrap();
        f.crypto.poll_budgeted(64).unwrap();
        f.reactor.poll_budgeted(128).unwrap();
    };
    f.origin.blocked_pages.borrow_mut().insert(0);
    for _ in 0..32 {
        assert!(a.next_slice().as_mut().poll(&mut cx).is_pending());
        assert!(slow.next_slice().as_mut().poll(&mut cx).is_pending());
        assert!(unordered.next_slice().as_mut().poll(&mut cx).is_pending());
        pump(&mut endpoint);
    }
    assert_eq!(
        &*f.origin.started_pages.borrow(),
        &[0],
        "same-page ordered work singleflights and unordered waits"
    );
    f.origin.blocked_pages.borrow_mut().clear();
    let mut first = None;
    let mut slow_first = None;
    let mut unordered_first = None;
    for _ in 0..1024 {
        if first.is_none() {
            if let Poll::Ready(result) = a.next_slice().as_mut().poll(&mut cx) {
                first = result.unwrap();
            }
        }
        if slow_first.is_none() {
            if let Poll::Ready(result) = slow.next_slice().as_mut().poll(&mut cx) {
                slow_first = result.unwrap();
            }
        }
        if unordered_first.is_none() {
            if let Poll::Ready(result) = unordered.next_slice().as_mut().poll(&mut cx) {
                unordered_first = result.unwrap();
            }
        }
        if first.is_some() && slow_first.is_some() && unordered_first.is_some() {
            break;
        }
        pump(&mut endpoint);
    }
    assert_eq!(first.as_ref().unwrap().slice().length, 1);
    assert!(slow_first.is_some() && unordered_first.is_some());
    assert_eq!(
        f.origin.calls.get(),
        1,
        "mixed reader reuses verified fixed-page result"
    );
    drop(first);
    a.release_page(PageNumber(0), 1).unwrap();
    // Only prefetch, not next_slice, drives real new work while readers retain
    // their current delivery leases. The one-page byte cap stops page two.
    for _ in 0..1024 {
        a.poll_prefetch(&mut cx);
        pump(&mut endpoint);
        if f.fill
            .dependencies
            .memory
            .get(&PageId {
                version: f.page.version.clone(),
                number: PageNumber(1),
            })
            .unwrap()
            .is_some()
        {
            break;
        }
    }
    assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1]);
    let second = futures::executor::block_on(a.next_slice())
        .unwrap()
        .unwrap();
    assert_eq!(second.slice().page, PageNumber(1));
    drop(second);
    a.release_page(PageNumber(1), PAGE_BYTES as u32).unwrap();
    drop((a, slow, unordered, slow_first, unordered_first));
    for _ in 0..32 {
        pump(&mut endpoint);
    }
    drop(pump);
    endpoint.uninstall().unwrap();
}

#[test]
fn ordered_later_page_completes_before_head_and_cancellation_keeps_completion_fence() {
    for mode in ["success", "cancel", "drop", "failure"] {
        let signers = network(1);
        let mut f = fixture_with(2 * PAGE_BYTES + 7, None);
        f.reactor.init().unwrap();
        let membership = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                vec![Member {
                    node: signers[0].node().clone(),
                    shares: NonZeroU32::new(1).unwrap(),
                    peer_endpoint: "127.0.0.1:8000".into(),
                    rails: vec![],
                    alignment_enabled: false,
                }],
            )
            .unwrap(),
        );
        let (local, mut endpoint, _publication) =
            coordinator(&f, &signers[0], &membership, Rc::new(NoPeer));
        let scope = RequestScope::new(f.scope.request, f.scope.deadline.0).unwrap();
        let request = ClientRequest {
            kind: ReadKind::Subscription {
                pin: Some(f.page.version.etag.clone()),
                range: None,
                page_credits: 64,
                byte_credits: 64 * PAGE_BYTES,
                ordered: true,
            },
            origin: OriginContext {
                object: f.context.object.clone(),
                metadata: None,
                authorization: None,
            },
        };
        let mut stream = futures::executor::block_on(local.read(request, &scope))
            .unwrap()
            .body
            .unwrap();
        let mut unordered = f
            .fill
            .dependencies
            .metadata_owner
            .subscriptions
            .register(
                f.page.version.clone(),
                ByteRange::From(0)
                    .resolve(f.origin.metadata.length)
                    .unwrap(),
                1,
                PAGE_BYTES,
                false,
            )
            .unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut pump = |endpoint: &mut crate::read::dispatch::WorkerEndpoint| {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            endpoint.poll(&mut cx, 64).unwrap();
            crate::read::drivers::poll(&mut cx, 64);
            f.engine.poll_budgeted(64).unwrap();
            f.crypto.poll_budgeted(64).unwrap();
            f.reactor.poll_budgeted(128).unwrap();
        };
        f.origin.blocked_pages.borrow_mut().insert(0);
        for _ in 0..1024 {
            assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
            pump(&mut endpoint);
            if f.fill
                .dependencies
                .memory
                .get(&PageId {
                    version: f.page.version.clone(),
                    number: PageNumber(1),
                })
                .unwrap()
                .is_some()
            {
                break;
            }
        }
        assert!(
            f.fill
                .dependencies
                .memory
                .get(&PageId {
                    version: f.page.version.clone(),
                    number: PageNumber(1)
                })
                .unwrap()
                .is_some(),
            "later page really completed"
        );
        assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1]);
        assert_eq!(stream.buffered_pages(), 2);
        assert!(unordered.poll_next(&mut cx).is_pending());
        if mode == "cancel" || mode == "drop" {
            if mode == "cancel" {
                futures::executor::block_on(stream.cancel()).unwrap();
            }
            drop(stream);
            for _ in 0..32 {
                pump(&mut endpoint);
            }
            assert!(
                unordered.poll_next(&mut cx).is_pending(),
                "cancelled waiter is not actual origin completion"
            );
            f.origin.blocked_pages.borrow_mut().clear();
            for _ in 0..1024 {
                pump(&mut endpoint);
            }
            assert!(matches!(
                unordered.poll_next(&mut cx),
                Poll::Ready(Ok(crate::read::subscription::Next::Select(_)))
            ));
        } else if mode == "failure" {
            f.origin.version_unavailable.set(true);
            f.origin.blocked_pages.borrow_mut().clear();
            let mut failure = None;
            for _ in 0..1024 {
                if let Poll::Ready(result) = stream.next_slice().as_mut().poll(&mut cx) {
                    failure = result.err();
                    break;
                }
                pump(&mut endpoint);
            }
            assert_eq!(failure, Some(Error::VersionUnavailable));
            assert_eq!(stream.buffered_pages(), 0);
            assert!(
                futures::executor::block_on(stream.next_slice())
                    .unwrap()
                    .is_none()
            );
            assert_eq!(
                &*f.origin.started_pages.borrow(),
                &[0, 1],
                "failure never retries or starts the tail"
            );
            drop(stream);
        } else {
            f.origin.blocked_pages.borrow_mut().clear();
            let mut first = None;
            for _ in 0..1024 {
                if let Poll::Ready(result) = stream.next_slice().as_mut().poll(&mut cx) {
                    first = result.unwrap();
                    break;
                }
                pump(&mut endpoint);
            }
            assert_eq!(first.as_ref().unwrap().slice().page, PageNumber(0));
            let second = futures::executor::block_on(stream.next_slice())
                .unwrap()
                .unwrap();
            assert_eq!(second.slice().page, PageNumber(1));
            drop(first);
            stream
                .release_page(PageNumber(0), PAGE_BYTES as u32)
                .unwrap();
            // The waiting unordered ticket must get its turn before ordered
            // prefetch can refill, even though this stream still has credit.
            stream.poll_prefetch(&mut cx);
            assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1]);
            let Poll::Ready(Ok(crate::read::subscription::Next::Select(selection))) =
                unordered.poll_next(&mut cx)
            else {
                panic!("unordered turn after the fixed batch completes")
            };
            drop(selection);
            let delivery = Delivery::new(
                Rc::new(PipePool::new(
                    f.fill.dependencies.admission.clone(),
                    f.reactor.clone(),
                )),
                Duration::from_secs(30),
            );
            let (socket, _stalled_client) = std::os::unix::net::UnixStream::pair().unwrap();
            let mut connection = crate::http::pool::ConnectionLease::from_accepted(
                socket.into(),
                &f.fill.dependencies.admission,
            )
            .unwrap();
            connection.tx_remaining = Some(PAGE_BYTES);
            let mut write = delivery.finish_progressing(second, connection, &scope);
            // No call to next_slice: page two is acquired while the current
            // delivery lease (page one) is held, including its pipe and credit.
            for _ in 0..1024 {
                stream.poll_prefetch(&mut cx);
                assert!(
                    write.as_mut().poll(&mut cx).is_pending(),
                    "client has not read any payload"
                );
                pump(&mut endpoint);
                if f.fill
                    .dependencies
                    .memory
                    .get(&PageId {
                        version: f.page.version.clone(),
                        number: PageNumber(2),
                    })
                    .unwrap()
                    .is_some()
                {
                    break;
                }
            }
            assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1, 2]);
            let third = futures::executor::block_on(stream.next_slice())
                .unwrap()
                .unwrap();
            assert_eq!(third.slice().page, PageNumber(2));
            assert_eq!(third.slice().length, 7);
            assert!(
                futures::executor::block_on(stream.next_slice())
                    .unwrap()
                    .is_none()
            );
            drop((stream, write, third));
        }
        drop(unordered);
        for _ in 0..32 {
            pump(&mut endpoint);
        }
        drop(pump);
        endpoint.uninstall().unwrap();
    }
}

#[test]
fn production_range_provider_selects_out_of_order_and_fans_out_to_two_nodes_and_local_readers() {
    let signers = network(3);
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    drop(listener);
    // A heavily weighted primary keeps this small object's pages on node 2 while
    // preserving production HRW, membership checks and origin authority.
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            (0..3)
                .map(|i| Member {
                    node: node(i),
                    shares: NonZeroU32::new(if i == 2 { 1_000_000 } else { 1 }).unwrap(),
                    peer_endpoint: if i == 2 {
                        address.to_string()
                    } else {
                        format!("127.0.0.1:{}", 8100 + i)
                    },
                    rails: vec![],
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let mut fixtures: Vec<_> = (0..3).map(|_| fixture_with(4 * PAGE_BYTES, None)).collect();
    for fixture in &fixtures {
        fixture.reactor.init().unwrap();
    }
    let demands = Rc::new(RefCell::new(Vec::new()));
    let mut locals = Vec::new();
    let mut endpoints = Vec::new();
    let mut publications = Vec::new();
    for i in 0..3 {
        let peers: Rc<dyn PeerClient> = if i == 2 {
            Rc::new(NoPeer)
        } else {
            let admission = fixtures[i].fill.dependencies.admission.clone();
            let reactor = fixtures[i].reactor.clone();
            let codec = Rc::new(crate::peer::wire::SecurityCodec::new(
                admission.clone(),
                Rc::new(BufferPool::new(admission.clone())),
            ));
            let io = Rc::new(HttpIo::with_admission(
                reactor.clone(),
                Codec::new(crate::peer::wire::MAX_ENVELOPE_HEAD, PAGE_BYTES + 16),
                admission.clone(),
            ));
            let transfers = Rc::new(crate::peer::transfer::Transfers::new(
                Rc::new(crate::http::pool::HttpPool::new(
                    reactor,
                    admission.clone(),
                    4,
                )),
                io,
                None,
                admission,
                codec,
                signers[i].clone(),
            ));
            let requester = Rc::new(crate::peer::Requester::new(
                Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
                Rc::new(Forwarding::new(signers[i].clone())),
                transfers,
                Rc::new(
                    PeerNetwork::new(node(i), PublishedState::for_membership(membership.clone()))
                        .unwrap(),
                ),
            ));
            Rc::new(Link {
                client: requester,
                demands: demands.clone(),
            })
        };
        let (local, endpoint, published) =
            coordinator(&fixtures[i], &signers[i], &membership, peers);
        locals.push(local);
        endpoints.push(endpoint);
        publications.push(published);
    }
    let gate = Rc::new(Gate {
        local: locals[2].clone(),
        blocked: Cell::new(false),
        calls: RefCell::new(Vec::new()),
    });
    let admission = fixtures[2].fill.dependencies.admission.clone();
    let auth = Rc::new(Forwarding::new(signers[2].clone()));
    let network = Rc::new(PeerNetwork::new(node(2), publications[2].clone()).unwrap());
    let relay = Rc::new(crate::peer::Relay::new(
        Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
        auth.clone(),
        Rc::new(NoPeer),
        admission.clone(),
        network.clone(),
    ));
    let codec = Rc::new(crate::peer::wire::SecurityCodec::new(
        admission.clone(),
        Rc::new(BufferPool::new(admission.clone())),
    ));
    let server = PeerServer::new(
        Rc::new(HttpIo::with_admission(
            fixtures[2].reactor.clone(),
            Codec::new(crate::peer::wire::MAX_ENVELOPE_HEAD, PAGE_BYTES + 16),
            admission.clone(),
        )),
        auth,
        admission,
        gate.clone(),
        relay,
    )
    .with_wire(codec)
    .with_signatures(signers[2].clone());
    let serving_scope = RequestScope::new(
        RequestId([99; 16]),
        Instant::now() + Duration::from_secs(30),
    )
    .unwrap();
    let mut serving = server.listen(address, &serving_scope);
    let open = |index: usize, start: u64| {
        let f = &fixtures[index];
        let request = ClientRequest {
            kind: ReadKind::Subscription {
                pin: Some(f.page.version.etag.clone()),
                range: Some(ByteRange::From(start * PAGE_BYTES)),
                page_credits: 1,
                byte_credits: PAGE_BYTES,
                ordered: false,
            },
            origin: OriginContext {
                object: f.context.object.clone(),
                metadata: None,
                authorization: None,
            },
        };
        futures::executor::block_on(locals[index].read(request, &f.scope))
            .unwrap()
            .body
            .unwrap()
    };
    let mut a = open(0, 0);
    let mut a2 = open(0, 0);
    let mut b = open(1, 2);
    let warm_request = ClientRequest {
        kind: ReadKind::Subscription {
            pin: Some(fixtures[0].page.version.etag.clone()),
            range: None,
            page_credits: 1,
            byte_credits: PAGE_BYTES,
            ordered: false,
        },
        origin: OriginContext {
            object: fixtures[0].context.object.clone(),
            metadata: None,
            authorization: None,
        },
    };
    let warm_scope = fixtures[0].scope.clone();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut pump = |endpoints: &mut Vec<crate::read::dispatch::WorkerEndpoint>| {
        assert!(
            serving
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                .is_pending()
        );
        for endpoint in endpoints {
            endpoint
                .poll(
                    &mut Context::from_waker(futures::task::noop_waker_ref()),
                    64,
                )
                .unwrap();
        }
        crate::read::drivers::poll(
            &mut Context::from_waker(futures::task::noop_waker_ref()),
            64,
        );
        for f in &mut fixtures {
            f.engine.poll_budgeted(64).unwrap();
            f.crypto.poll_budgeted(64).unwrap();
            f.reactor.poll_budgeted(128).unwrap();
        }
    };
    // First assignment is ordered progress, shared to both local subscribers.
    let first = {
        let mut work = a.next_slice();
        let mut result = None;
        for _ in 0..1024 {
            if let Poll::Ready(value) = work.as_mut().poll(&mut cx) {
                result = Some(value.unwrap().unwrap());
                break;
            }
            pump(&mut endpoints);
        }
        result.expect("first production page")
    };
    assert_eq!(first.slice().page.0, 0);
    let duplicate = futures::executor::block_on(a2.next_slice())
        .unwrap()
        .unwrap();
    assert_eq!(duplicate.slice().page.0, 0);
    drop((first, duplicate));
    a.release_page(PageNumber(0), PAGE_BYTES as u32).unwrap();
    a2.release_page(PageNumber(0), PAGE_BYTES as u32).unwrap();
    gate.blocked.set(true);
    let mut wb = b.next_slice();
    for _ in 0..32 {
        assert!(wb.as_mut().poll(&mut cx).is_pending());
        pump(&mut endpoints);
    }
    assert_eq!(&*gate.calls.borrow(), &[0, 2]);
    let mut wa = a.next_slice();
    for _ in 0..32 {
        assert!(wa.as_mut().poll(&mut cx).is_pending());
        pump(&mut endpoints);
    }
    assert_eq!(
        &*gate.calls.borrow(),
        &[0, 2],
        "second receiving node joins provider work"
    );
    gate.blocked.set(false);
    let mut ra = None;
    let mut rb = None;
    for _ in 0..1024 {
        if ra.is_none() {
            if let Poll::Ready(value) = wa.as_mut().poll(&mut cx) {
                ra = Some(value.unwrap().unwrap());
            }
        }
        if rb.is_none() {
            if let Poll::Ready(value) = wb.as_mut().poll(&mut cx) {
                rb = Some(value.unwrap().unwrap());
            }
        }
        if ra.is_some() && rb.is_some() {
            break;
        }
        pump(&mut endpoints);
    }
    assert_eq!(
        ra.unwrap().slice().page.0,
        2,
        "provider chose page 2 before page 1"
    );
    assert_eq!(rb.unwrap().slice().page.0, 2);
    drop((wa, wb));
    let local = futures::executor::block_on(a2.next_slice())
        .unwrap()
        .unwrap();
    assert_eq!(local.slice().page.0, 2);
    assert!(
        demands.borrow().iter().any(|count| *count > 1),
        "not fixed-page wire requests"
    );
    assert_eq!(
        demands.borrow().len(),
        3,
        "one transfer per receiving node, not per local subscriber"
    );
    drop(local);
    a.release_page(PageNumber(2), PAGE_BYTES as u32).unwrap();
    a2.release_page(PageNumber(2), PAGE_BYTES as u32).unwrap();
    // The next turn restores oldest-page progress without redelivering page 2.
    let next = {
        let mut work = a.next_slice();
        let mut result = None;
        for _ in 0..2048 {
            if let Poll::Ready(value) = work.as_mut().poll(&mut cx) {
                result = Some(value.unwrap().unwrap());
                break;
            }
            pump(&mut endpoints);
        }
        result.expect("continued selection")
    };
    assert_eq!(next.slice().page.0, 1);
    drop(next);
    a.release_page(PageNumber(1), PAGE_BYTES as u32).unwrap();
    drop(
        futures::executor::block_on(a2.next_slice())
            .unwrap()
            .unwrap(),
    );
    a2.release_page(PageNumber(1), PAGE_BYTES as u32).unwrap();
    let last = {
        let mut work = a.next_slice();
        let mut result = None;
        for _ in 0..2048 {
            if let Poll::Ready(value) = work.as_mut().poll(&mut cx) {
                result = Some(value.unwrap().unwrap());
                break;
            }
            pump(&mut endpoints);
        }
        result.expect("last selection")
    };
    assert_eq!(last.slice().page.0, 3);
    drop(last);
    assert!(
        futures::executor::block_on(a.next_slice())
            .unwrap()
            .is_none()
    );
    // A later local subscriber must reuse already verified pages without asking
    // the provider to transfer pages excluded by its completed-page ledger.
    let transfers = demands.borrow().len();
    let mut warm = futures::executor::block_on(locals[0].read(warm_request, &warm_scope))
        .unwrap()
        .body
        .unwrap();
    let cached = {
        let mut work = warm.next_slice();
        let mut result = None;
        for _ in 0..2048 {
            if let Poll::Ready(value) = work.as_mut().poll(&mut cx) {
                result = Some(value.unwrap().unwrap());
                break;
            }
            pump(&mut endpoints);
        }
        result.expect("warm local selection")
    };
    assert_eq!(cached.slice().page.0, 0);
    assert_eq!(
        demands.borrow().len(),
        transfers,
        "warm read sent a peer request"
    );
    drop((cached, warm));
    drop(pump);
    serving_scope.cancel().unwrap();
    drop(serving);
    for f in &fixtures {
        let mut drain = f.reactor.drain();
        let until = Instant::now() + Duration::from_secs(5);
        loop {
            if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
                result.unwrap();
                break;
            }
            assert!(Instant::now() < until, "reactor drain watchdog");
            f.reactor.poll_budgeted(128).unwrap();
            f.reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }
    assert_eq!(fixtures[2].origin.calls.get(), 4);
    assert_eq!(
        fixtures[0].origin.calls.get() + fixtures[1].origin.calls.get(),
        0
    );
    drop((a, a2, b));
    for endpoint in &mut endpoints {
        endpoint.uninstall().unwrap();
    }
}
