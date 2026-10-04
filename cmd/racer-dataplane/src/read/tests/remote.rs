//! Candidate policy exercised through real signing, handshake, TCP, and Requester.
use crate::read::candidates::CandidatePolicy;
use crate::read::candidates::CandidateResolution;
use crate::read::dispatch::WorkerMap;
use crate::read::flight::AcquisitionBudget;
use crate::error::Error;
use crate::error::Operation;
use crate::http::Codec;
use crate::http::HttpIo;
use crate::http::HttpPool;
use crate::memory::BufferPool;
use crate::model::ExpiresAt;
use crate::model::MetadataSelector;
use crate::model::ObjectMetadata;
use crate::model::OriginContext;
use crate::model::*;
use crate::peer::PeerNetwork;
use crate::peer::Relay;
use crate::peer::Requester;
use crate::peer::protocol;
use crate::peer::protocol::FetchMode;
use crate::peer::protocol::PeerResponse;
use crate::peer::protocol::VerifiedRequest;
use crate::peer::server::LocalPageService;
use crate::peer::server::PeerServer;
use crate::peer::transport::Transfers;
use crate::runtime::admission::AdmissionPolicy;
use crate::runtime::deadline::RequestScope;
use crate::runtime::reactor::Reactor;
use crate::security::credentials::CredentialCrypto;
use crate::security::forwarding::Forwarding;
use crate::security::test_support::Identity;
use crate::test_support::origin::AdapterOrigin;
use crate::topology::LinkHealth;
use crate::topology::Member;
use crate::topology::Membership;
use crate::topology::Paths;
use crate::topology::Placement;
use racer_control_wire::PublicationSequence;
use racer_control_wire::SCHEMA_VERSION;
use racer_identity::Keyring;
use std::cell::Cell;
use std::cell::RefCell;
use std::net::TcpListener;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Context;
use std::task::Poll;
use std::time::Duration;
use std::time::Instant;

const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
const CACHE: &str = "33333333-3333-4333-8333-333333333333";
fn node(n: usize) -> NodeId {
    NodeId(format!("22222222-2222-4222-8222-{n:012}"))
}
fn identities(nodes: &[NodeId]) -> Vec<Identity> {
    crate::security::test_support::identities(ClusterId(CLUSTER.into()), nodes, || {
        let mut keys = crate::security::test_support::mac_test_key(CACHE);
        keys.push(racer_control_wire::CacheEncryptionKey::new(
            racer_control_wire::CacheKeyRef {
                cache: CacheId(CACHE.into()),
                id: crate::model::key_id_from_generation(1, 1).unwrap(),
                purpose: racer_control_wire::CacheKeyPurpose::Page,
            },
            racer_control_wire::CacheKeyState::Active,
            zeroize::Zeroizing::new([7; 32]),
        ));
        keys
    })
}
struct CandidateService {
    calls: Rc<Cell<usize>>,
    sender: NodeId,
    metadata: ObjectMetadata,
    forbidden: bool,
    local: Option<Rc<dyn LocalPageService>>,
    version_unavailable: bool,
}
impl LocalPageService for CandidateService {
    fn serve_peer<'a>(
        &'a self,
        request: VerifiedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        Box::pin(async move {
            scope.check()?;
            let logical = request.request();
            assert_eq!(logical.route.visited, vec![self.sender.clone()]);
            assert_eq!(logical.route.remaining_links, 4);
            assert!(logical.route.remaining_attempts > 0);
            assert!(matches!(
                logical.operation,
                protocol::Operation::Metadata {
                    mode: FetchMode::Acquire,
                    ..
                } | protocol::Operation::Bootstrap {
                    mode: FetchMode::Acquire,
                    ..
                }
            ));
            let inherited = super::inherited_budget(&logical.route, scope)?;
            assert_eq!(
                inherited.remaining_attempts(),
                logical.route.remaining_attempts
            );
            assert_eq!(
                inherited.remaining_links(),
                3,
                "the final incoming link is charged exactly once"
            );
            self.calls.set(self.calls.get() + 1);
            if let Some(local) = &self.local {
                return local.serve_peer(request, membership, scope).await;
            }
            if self.version_unavailable {
                return Err(Error::VersionUnavailable);
            }
            if self.forbidden {
                Ok(PeerResponse::OriginForbidden)
            } else {
                Ok(PeerResponse::Metadata(self.metadata.clone()))
            }
        })
    }
}

#[test]
fn remote_candidate_uses_actual_requester_signatures_and_inherited_credits() {
    remote_candidate(None, false);
    remote_candidate(None, true);
}

#[test]
fn remote_origin_absence_preserves_fresh_404_pinned_412_and_later_cached_version() {
    for case in [
        Absence::Fresh,
        Absence::Subscription,
        Absence::Pinned,
        Absence::CachedPin,
    ] {
        remote_candidate(Some(case), false);
    }
}

#[test]
fn coordinator_copy_miss_is_not_origin_absence_and_pinned_missing_is_412() {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    use crate::test_support::NoPeers;
    let ids = identities(&[node(0), node(1)]);
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            vec![Member {
                node: node(1),
                shares: std::num::NonZeroU32::new(4).unwrap(),
                peer_endpoint: "127.0.0.1:8000".into(),
                rails: vec![],
                site: String::new(),
            }],
        )
        .unwrap(),
    );
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let calls = missing_adapter();
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let (coordinator, _endpoint) = metadata_coordinator_with_newer_publication(
        &node(1),
        &membership,
        ids[1].keys.clone(),
        admission.clone(),
        reactor.clone(),
        NoPeers::requester(),
        calls.clone(),
        true,
    );
    let sender = Forwarding::new(ids[0].signatures.clone());
    let receiver = Forwarding::new(ids[1].signatures.clone());
    for (index, (selector, acquire)) in [
        (MetadataSelector::Fresh, false),
        (
            MetadataSelector::Pinned(StrongEtag::test_value("old")),
            false,
        ),
        (MetadataSelector::Fresh, true),
        (
            MetadataSelector::Pinned(StrongEtag::test_value("old")),
            true,
        ),
    ]
    .into_iter()
    .enumerate()
    {
        let scope = RequestScope::new(
            RequestId([index as u8; 16]),
            Instant::now() + Duration::from_secs(10),
        )
        .unwrap();
        let context = OriginContext {
            object: ObjectId {
                cache: CacheId(CACHE.into()),
                key: CacheKey([3; 32]),
            },
            metadata: None,
            authorization: None,
        };
        let attempt = AttemptId([index as u8; 16]);
        let request = protocol::PeerRequest {
            operation: protocol::Operation::Metadata {
                object: context.object.clone(),
                selector,
                mode: if acquire {
                    FetchMode::Acquire
                } else {
                    FetchMode::CopyOnly
                },
            },
            origin: CredentialCrypto::new(ids[0].keys.clone(), admission.clone())
                .seal(&context, attempt, &scope)
                .unwrap(),
            route: crate::topology::RouteBudget {
                membership: membership.version,
                request: scope.request,
                attempt,
                destination: node(1),
                visited: vec![node(0)],
                remaining_links: 4,
                remaining_attempts: if acquire { 4 } else { 0 },
                deadline: scope.deadline,
            },
        };
        let (signed, binding) = sender.sign_request(request).unwrap();
        let admitted = receiver.verify_request(signed).unwrap();
        let response_binding = admitted.binding().clone();
        let result = drive_origin(
            &reactor,
            coordinator.serve_peer(admitted, membership.clone(), &scope),
        )
        .unwrap();
        assert!(match index {
            0 | 1 => matches!(result, PeerResponse::Miss),
            2 => matches!(result, PeerResponse::NotFound),
            _ => matches!(result, PeerResponse::VersionUnavailable),
        });
        sender
            .verify_response(
                receiver.sign_response(&response_binding, result).unwrap(),
                &binding,
            )
            .unwrap();
        assert_eq!(calls.calls().len(), index.saturating_sub(1));
    }
}

#[derive(Clone, Copy)]
enum Absence {
    Fresh,
    Subscription,
    Pinned,
    CachedPin,
}

fn remote_candidate(absence: Option<Absence>, forbidden: bool) {
    remote_candidate_with_churn(absence, forbidden, false);
}

#[test]
fn live_read_routes_after_cache_only_publication_and_membership_update() {
    remote_candidate_with_churn(None, false, true);
}

fn remote_candidate_with_churn(absence: Option<Absence>, forbidden: bool, churn: bool) {
    let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
    let _owner = queue.enter();
    use racer_control_wire::Publication;
    use crate::control::PublishedState;
    use crate::control::SnapshotStore;
    let object = ObjectId {
        cache: CacheId(CACHE.into()),
        key: CacheKey([3; 32]),
    };
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let address = listener.local_addr().unwrap().to_string();
    let source_publications = Arc::new(PublishedState::default());
    let source_store =
        SnapshotStore::new(ClusterId(CLUSTER.into()), source_publications.clone(), 1);
    let mut publication = Publication {
        schema_version: 1,
        cluster: ClusterId(CLUSTER.into()),
        sequence: PublicationSequence(1),
        membership_version: MembershipVersion(1),
        caches: vec![racer_control_wire::CacheDefinition {
            id: CacheId(CACHE.into()),
            name: "remote".into(),
            client_socket: "/run/racer/remote/client/socket".into(),
            origin_socket: "/run/racer/remote/origin/socket".into(),
        }],
        members: (0..4)
            .map(|i| Member {
                node: node(i),
                shares: std::num::NonZeroU32::new(4).unwrap(),
                peer_endpoint: address.clone(),
                rails: vec![],
                site: String::new(),
            })
            .collect(),
    };
    let first = source_store.publish(publication.clone()).unwrap();
    publication.sequence.0 = 2;
    let cache_only = source_store.publish(publication.clone()).unwrap();
    assert!(Arc::ptr_eq(&first.membership, &cache_only.membership));
    let membership = cache_only.membership.clone();
    let retired = Arc::downgrade(&membership);
    drop(first);
    drop(cache_only);
    let placement = Rc::new(Placement::new(16));
    let ranked = placement
        .rank(membership.clone(), &object, PageNumber(0))
        .unwrap();
    let destination = ranked.ordered[0].clone();
    let source = membership
        .members()
        .iter()
        .find(|member| !ranked.ordered.contains(&member.node))
        .unwrap()
        .node
        .clone();
    let mut nodes = vec![source.clone(), destination.clone()];
    nodes.extend(ranked.ordered.iter().skip(1).cloned());
    let identities = identities(&nodes);
    let a = &identities[0];
    let b = &identities[1];
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(protocol::MAX_ENVELOPE_HEAD),
        admission.clone(),
        crate::model::PAGE_BYTES + 16,
    ));
    let codec = Rc::new(protocol::SecurityCodec::new(
        admission.clone(),
        BufferPool::new(admission.clone()),
    ));
    let transfers = Rc::new(Transfers::new(
        Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 2)),
        io.clone(),
        None,
        admission.clone(),
        codec.clone(),
        a.signatures.clone(),
    ));
    let source_network = Rc::new(PeerNetwork::new(source.clone(), source_publications).unwrap());
    let destination_membership =
        Arc::new(Membership::validate(membership.version, membership.members().to_vec()).unwrap());
    let destination_network = Rc::new(
        PeerNetwork::new(
            destination.clone(),
            PublishedState::for_membership(destination_membership),
        )
        .unwrap(),
    );
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 8));
    let auth = Rc::new(Forwarding::new(b.signatures.clone()));
    let outbound =
        crate::peer::tests::NoOutbound::new(b.signatures.clone(), destination_network.clone());
    let relay = Rc::new(Relay::new(
        paths.clone(),
        auth.clone(),
        outbound.requester.clone(),
        admission.clone(),
        destination_network.clone(),
    ));
    let calls = Rc::new(Cell::new(0));
    let metadata = ObjectMetadata {
        content_type: Some(
            crate::model::ContentType::parse(b"application/vnd.oci.image.manifest.v1+json")
                .unwrap(),
        ),
        version: ObjectVersion {
            object: object.clone(),
            etag: StrongEtag::parse(b"\"remote\"").unwrap(),
        },
        length: 71,
        expires_at: ExpiresAt::test_time(std::time::SystemTime::now() + Duration::from_secs(60)),
    };
    let origin_calls = missing_adapter();
    let copy_calls = Rc::new(RefCell::new(Vec::new()));
    let local = absence
        .filter(|case| !matches!(case, Absence::Pinned))
        .map(|case| {
            let copies = Rc::new(CachedCopies {
                sender: Rc::new(Forwarding::new(b.signatures.clone())),
                receivers: identities
                    .iter()
                    .skip(2)
                    .map(|id| {
                        (
                            id.signatures.node().clone(),
                            Forwarding::new(id.signatures.clone()),
                        )
                    })
                    .collect(),
                calls: copy_calls.clone(),
                metadata: matches!(case, Absence::CachedPin).then(|| metadata.clone()),
            });
            metadata_coordinator(
                &destination,
                &membership,
                b.keys.clone(),
                admission.clone(),
                reactor.clone(),
                crate::peer::Requester::scripted(
                    copies,
                    CachedCopies::direct_hedge_available,
                    CachedCopies::request,
                    CachedCopies::request_direct,
                ),
                origin_calls.clone(),
            )
        });
    let server = PeerServer::for_test(
        io,
        auth,
        admission.clone(),
        Rc::new(CandidateService {
            calls: calls.clone(),
            sender: source.clone(),
            metadata: metadata.clone(),
            forbidden,
            version_unavailable: matches!(absence, Some(Absence::Pinned)),
            local: local.map(|(coordinator, _endpoint)| {
                // Keep the local metadata owner installed through dispatch.
                Rc::new(OwnedCoordinator {
                    coordinator,
                    _endpoint,
                }) as Rc<dyn LocalPageService>
            }),
        }),
        relay,
        codec,
        b.signatures.clone(),
    );
    let requester = Rc::new(Requester::new(
        paths,
        Rc::new(Forwarding::new(a.signatures.clone())),
        transfers,
        source_network,
    ));
    let requester: Rc<crate::peer::Requester> = if matches!(absence, Some(Absence::Pinned)) {
        crate::peer::Requester::scripted(
            Rc::new(PinnedFallback {
                requester,
                destination,
                sender: Forwarding::new(a.signatures.clone()),
                receivers: identities
                    .iter()
                    .skip(2)
                    .map(|id| {
                        (
                            id.signatures.node().clone(),
                            Forwarding::new(id.signatures.clone()),
                        )
                    })
                    .collect(),
            }),
            PinnedFallback::direct_hedge_available,
            PinnedFallback::request,
            PinnedFallback::request_direct,
        )
    } else {
        requester
    };
    let ingress_calls = missing_adapter();
    let ingress = matches!(absence, Some(Absence::Fresh | Absence::Subscription)).then(|| {
        metadata_coordinator(
            &source,
            &membership,
            a.keys.clone(),
            admission.clone(),
            reactor.clone(),
            requester.clone(),
            ingress_calls.clone(),
        )
    });
    let policy = CandidatePolicy::new(
        source,
        placement,
        requester,
        Rc::new(CredentialCrypto::new(a.keys.clone(), admission.clone())),
        Arc::new(Default::default()),
    );
    let context = OriginContext {
        object: object.clone(),
        metadata: None,
        authorization: None,
    };
    let scope =
        RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(30)).unwrap();
    let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
    let operation = protocol::Operation::Metadata {
        object,
        selector: if matches!(absence, Some(Absence::Pinned | Absence::CachedPin)) {
            MetadataSelector::Pinned(metadata.version.etag.clone())
        } else {
            MetadataSelector::Fresh
        },
        mode: FetchMode::Acquire,
    };
    let service = async {
        let fd = reactor
            .accept(
                Rc::new(uring_runtime::reactor::Descriptor::from(listener)),
                &scope,
            )
            .await?;
        let connection = crate::http::from_accepted(fd, &admission)?;
        server.serve_connection(connection, &scope).await?;
        Ok::<(), Error>(())
    };
    let result = {
        // From here the candidate read itself owns the source lease. Network and
        // topology caches must not contribute hidden structural strong owners.
        drop(membership);
        let read = async {
            if let Some((coordinator, _endpoint)) = &ingress {
                use crate::client::ClientRequest;
                use crate::client::ReadKind;
                let result = coordinator
                    .read(
                        ClientRequest {
                            origin: context,
                            kind: if matches!(absence, Some(Absence::Subscription)) {
                                ReadKind::Subscription {
                                    pin: None,
                                    range: None,
                                    page_credits: 1,
                                    byte_credits: crate::model::PAGE_BYTES,
                                    ordered: false,
                                }
                            } else {
                                ReadKind::Head
                            },
                        },
                        &scope,
                    )
                    .await;
                match result {
                    Err(error) => Err(error),
                    Ok(_) => panic!("missing origin must not produce a client success"),
                }
            } else {
                policy
                    .resolve_with_budget(ranked, &context, operation, &scope, &mut budget)
                    .await
            }
        };
        let exchange = futures::future::join(read, service);
        let mut exchange = std::pin::pin!(exchange);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let watchdog = Instant::now() + Duration::from_secs(10);
        let mut updated = false;
        loop {
            if let Poll::Ready((result, served)) =
                std::future::Future::poll(exchange.as_mut(), &mut cx)
            {
                served.unwrap();
                break result;
            }
            if churn && !updated {
                publication.sequence.0 = 3;
                let cache_only = source_store.publish(publication.clone()).unwrap();
                assert!(Arc::ptr_eq(
                    &retired.upgrade().unwrap(),
                    &cache_only.membership
                ));
                drop(cache_only);
                publication.sequence.0 = 4;
                publication.membership_version.0 = 2;
                for member in &mut publication.members {
                    member.peer_endpoint = "127.0.0.1:1".into();
                }
                source_store.publish(publication.clone()).unwrap();
                assert!(
                    retired.upgrade().is_some(),
                    "pending read must retain old routing"
                );
                updated = true;
            }
            assert!(Instant::now() < watchdog, "peer absence exchange stalled");
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    };
    if churn {
        assert_eq!(
            source_store.current().unwrap().membership.version,
            MembershipVersion(2)
        );
        assert!(
            retired.upgrade().is_none(),
            "finished read must release old routing"
        );
        publication.sequence.0 = 5;
        publication.membership_version.0 = 3;
        source_store.publish(publication).unwrap();
    }
    if ingress.is_none() {
        assert_eq!(
            budget.remaining_attempts(),
            if matches!(absence, Some(Absence::Pinned)) {
                0
            } else {
                10
            },
            "candidate sends and delegated credits remain spent"
        );
        assert_eq!(
            budget.remaining_links(),
            if matches!(absence, Some(Absence::Pinned)) {
                12
            } else {
                20
            }
        );
    }
    assert_eq!(
        ingress_calls.calls().len(),
        0,
        "noncandidate ingress cannot call origin"
    );
    if let Some(case @ (Absence::Fresh | Absence::Subscription | Absence::Pinned)) = absence {
        let expected = if matches!(case, Absence::Fresh | Absence::Subscription) {
            Error::NotFound
        } else {
            Error::VersionUnavailable
        };
        assert_eq!(result.err(), Some(expected));
        let responses = crate::client::response::Responses::new(
            Rc::new(HttpIo::for_clients(reactor.clone(), admission.clone())),
            Rc::new(crate::memory::delivery::Delivery::new(
                Rc::new(crate::memory::new_pipe_pool(admission.clone())),
                reactor.clone(),
                Duration::from_secs(10),
            )),
        );
        assert!(matches!(responses.error_head(expected).unwrap().start,
                http1::StartLine::Response { status }
                if status == if matches!(case, Absence::Fresh | Absence::Subscription) { 404 } else { 412 }));
    } else if forbidden {
        assert!(matches!(result, Err(Error::OriginForbidden)));
    } else {
        let CandidateResolution::Copy(response) = result.unwrap() else {
            panic!("noncandidate cannot fetch origin")
        };
        let PeerResponse::Metadata(value) = response.response() else {
            panic!("metadata response required")
        };
        assert_eq!(value.version, metadata.version);
        assert_eq!(value.length, metadata.length);
        if absence.is_none() {
            assert_eq!(
                value
                    .expires_at
                    .as_system_time()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_millis(),
                metadata
                    .expires_at
                    .as_system_time()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_millis()
            );
        }
    }
    assert_eq!(calls.get(), 1);
    if let Some(case) = absence {
        assert_eq!(
            origin_calls.calls().len(),
            usize::from(!matches!(case, Absence::Pinned))
        );
        assert_eq!(
            copy_calls.borrow().len(),
            match case {
                Absence::Fresh | Absence::Subscription => 0,
                Absence::Pinned => 0,
                Absence::CachedPin => 1,
            }
        );
    }
}

struct OwnedCoordinator {
    coordinator: Rc<super::Coordinator>,
    _endpoint: super::dispatch::WorkerEndpoint,
}

struct PinnedFallback {
    requester: Rc<Requester>,
    destination: NodeId,
    sender: Forwarding,
    receivers: Vec<(NodeId, Forwarding)>,
}
impl PinnedFallback {
    fn direct_hedge_available(
        &self,
        _: &std::sync::Arc<crate::topology::Membership>,
        _: &NodeId,
    ) -> bool {
        false
    }
    fn request_direct<'a>(
        &'a self,
        _: protocol::PeerRequest,
        _: std::sync::Arc<crate::topology::Membership>,
        _: &'a RequestScope,
    ) -> Operation<'a, protocol::VerifiedResponse> {
        panic!("pinned fallback fixture does not admit direct hedges")
    }
    fn request<'a>(
        &'a self,
        request: protocol::PeerRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, protocol::VerifiedResponse> {
        Box::pin(async move {
            if request.route.destination == self.destination {
                return crate::peer::Requester::request(
                    self.requester.as_ref(),
                    request,
                    membership,
                    scope,
                )
                .await;
            }
            let receiver = &self
                .receivers
                .iter()
                .find(|(node, _)| node == &request.route.destination)
                .unwrap()
                .1;
            let (signed, binding) = self.sender.sign_request(request)?;
            let admitted = receiver.verify_request(signed)?;
            self.sender.verify_response(
                receiver.sign_response(admitted.binding(), PeerResponse::VersionUnavailable)?,
                &binding,
            )
        })
    }
}
impl LocalPageService for OwnedCoordinator {
    fn serve_peer<'a>(
        &'a self,
        request: VerifiedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        self.coordinator.serve_peer(request, membership, scope)
    }
}

fn missing_adapter() -> Rc<AdapterOrigin> {
    let adapter = Rc::new(AdapterOrigin::new(
        "remote",
        ObjectMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(CACHE.into()),
                    key: CacheKey([3; 32]),
                },
                etag: StrongEtag::test_value("missing"),
            },
            length: 0,
            expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
        },
    ));
    adapter.set_missing(true);
    adapter
}

fn drive_origin<T>(reactor: &Reactor, future: impl std::future::Future<Output = T>) -> T {
    let mut future = std::pin::pin!(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
        assert!(Instant::now() < deadline, "origin absence exchange stalled");
        reactor.poll_budgeted(128).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

struct CachedCopies {
    sender: Rc<Forwarding>,
    receivers: Vec<(NodeId, Forwarding)>,
    calls: Rc<RefCell<Vec<NodeId>>>,
    metadata: Option<ObjectMetadata>,
}
impl CachedCopies {
    fn direct_hedge_available(
        &self,
        _: &std::sync::Arc<crate::topology::Membership>,
        _: &NodeId,
    ) -> bool {
        false
    }
    fn request_direct<'a>(
        &'a self,
        _: protocol::PeerRequest,
        _: std::sync::Arc<crate::topology::Membership>,
        _: &'a RequestScope,
    ) -> Operation<'a, protocol::VerifiedResponse> {
        panic!("metadata copy fixture does not admit direct hedges")
    }
    fn request<'a>(
        &'a self,
        request: protocol::PeerRequest,
        _: std::sync::Arc<crate::topology::Membership>,
        _: &'a RequestScope,
    ) -> Operation<'a, protocol::VerifiedResponse> {
        Box::pin(async move {
            assert!(matches!(
                request.operation,
                protocol::Operation::Metadata {
                    mode: FetchMode::CopyOnly,
                    ..
                }
            ));
            assert_eq!(request.route.remaining_attempts, 0);
            self.calls
                .borrow_mut()
                .push(request.route.destination.clone());
            let receiver = &self
                .receivers
                .iter()
                .find(|(node, _)| node == &request.route.destination)
                .unwrap()
                .1;
            let (signed, binding) = self.sender.sign_request(request)?;
            let admitted = receiver.verify_request(signed)?;
            let response = self
                .metadata
                .clone()
                .map(PeerResponse::Metadata)
                .unwrap_or(PeerResponse::Miss);
            self.sender.verify_response(
                receiver.sign_response(admitted.binding(), response)?,
                &binding,
            )
        })
    }
}

// Production read graph and origin client; only the adapter boundary and later
// candidate caches are doubles. Metadata-only requests need no slab I/O or crypto.
fn metadata_coordinator(
    node: &NodeId,
    membership: &Arc<Membership>,
    keys: Rc<Keyring>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    reactor: Rc<Reactor>,
    peers: Rc<crate::peer::Requester>,
    adapter: Rc<AdapterOrigin>,
) -> (Rc<super::Coordinator>, super::dispatch::WorkerEndpoint) {
    metadata_coordinator_with_newer_publication(
        node, membership, keys, admission, reactor, peers, adapter, false,
    )
}

fn metadata_coordinator_with_newer_publication(
    node: &NodeId,
    membership: &Arc<Membership>,
    keys: Rc<Keyring>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    reactor: Rc<Reactor>,
    peers: Rc<crate::peer::Requester>,
    adapter: Rc<AdapterOrigin>,
    newer_publication: bool,
) -> (Rc<super::Coordinator>, super::dispatch::WorkerEndpoint) {
    use racer_control_wire::Publication;
    use crate::control::PublishedState;
    use crate::control::SnapshotStore;
    use crate::memory::cache::MemoryCache;
    use crate::memory::delivery::Delivery;
    use crate::memory::new_pipe_pool;
    use crate::runtime::crypto;
    use crate::runtime::crypto::CryptoClient;
    use crate::security::aead::PageCrypto;
    use crate::store::StoreReader;
    use crate::store::StoreWriter;
    use crate::store::catalog::Index;
    use crate::store::catalog::SegmentClock;
    let published = Arc::new(PublishedState::default());
    let availability = Rc::new(crate::control::Availability::new(
        published.clone(),
        keys.clone(),
    ));
    let snapshots = Rc::new(SnapshotStore::new(ClusterId(CLUSTER.into()), published, 4));
    let mut publication = Publication {
        schema_version: SCHEMA_VERSION,
        cluster: ClusterId(CLUSTER.into()),
        sequence: PublicationSequence(1),
        membership_version: membership.version,
        members: membership.members().to_vec(),
        caches: vec![racer_control_wire::CacheDefinition {
            id: CacheId(CACHE.into()),
            name: "remote".into(),
            client_socket: "/run/racer/remote/client/socket".into(),
            origin_socket: "/run/racer/remote/origin/socket".into(),
        }],
    };
    snapshots.publish(publication.clone()).unwrap();
    if newer_publication {
        publication.sequence.0 += 1;
        publication.membership_version.0 += 1;
        publication.members.clear();
        snapshots.publish(publication).unwrap();
        // Peer acquisition must use the ingress lease, even when current
        // membership has advanced and no longer includes this candidate.
    }
    let buffers = BufferPool::new(admission.clone());
    let index = Rc::new(Index::new(WorkerId(0), 16, availability.clone()));
    let segments = Rc::new(page_alloc::Segments::new(64 * 1024 * 1024));
    let slabs = Rc::new(page_alloc::Slab::new(
        "unused/absence-slabs/worker-0-slab-0.dat".into(),
        1024 * 1024 * 1024,
        64 * 1024 * 1024,
        crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
    ));
    let disk = Rc::new(StoreReader::new(
        Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1)),
        index.clone(),
        segments.clone(),
        slabs.clone(),
        admission.clone(),
        reactor.clone(),
        buffers.clone(),
    ));
    let writer = Rc::new(StoreWriter::new(
        index.clone(),
        segments,
        slabs,
        admission.clone(),
        reactor.clone(),
        availability.clone(),
    ));
    let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
    let candidates = Rc::new(CandidatePolicy::new(
        node.clone(),
        Rc::new(Placement::new(16)),
        peers.clone(),
        credentials.clone(),
        Arc::new(Default::default()),
    ));
    let origin = adapter.client(
        snapshots.clone(),
        admission.clone(),
        reactor.clone(),
        buffers.clone(),
    );
    let owners = Arc::new(
        super::dispatch::WorkerDirectory::new(
            Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
            vec![WorkerId(0)],
            16,
        )
        .unwrap(),
    );
    let (port, _engine) = crypto::pair(WorkerId(0), 0, std::num::NonZeroUsize::new(16).unwrap());
    let fill = Rc::new(crate::read::fill::Fill::new(
        crate::read::fill::FillDependencies {
            memory: Rc::new(MemoryCache::new(buffers.clone(), availability.clone())),
            buffers,
            disk,
            writer,
            origin: origin.clone(),
            candidates: candidates.clone(),
            flights: Rc::new(crate::read::flight::Flights::new(
                admission.clone(),
                availability.clone(),
            )),
            crypto: Rc::new(PageCrypto::new(keys, Rc::new(CryptoClient::new(port)))),
            credentials: credentials.clone(),
            admission: admission.clone(),
            metadata_owner: owners.clone(),
        },
    ));
    let metadata = Rc::new(super::metadata::MetadataService::new(
        candidates,
        origin,
        credentials.clone(),
        16,
        super::metadata::MetadataDependencies {
            index,
            fill: fill.clone(),
            owners: owners.clone(),
        },
    ));
    let delivery = Rc::new(Delivery::new(
        Rc::new(new_pipe_pool(admission)),
        reactor,
        Duration::from_secs(10),
    ));
    let streams = Rc::new(super::range_stream::RangeStreams::new(
        owners.clone(),
        delivery,
        2,
    ));
    let coordinator = Rc::new(super::Coordinator::new(
        snapshots,
        metadata,
        fill,
        streams,
        credentials,
        availability,
    ));
    let endpoint = owners.install(WorkerId(0), coordinator.clone()).unwrap();
    (coordinator, endpoint)
}
