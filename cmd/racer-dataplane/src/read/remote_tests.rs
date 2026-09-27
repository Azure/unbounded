//! Candidate policy exercised through real signing, handshake, TCP, and Requester.
use super::candidates::{CandidatePolicy, CandidateResolution};
use super::flight::AcquisitionBudget;
use crate::{
    control::wire::{BundleGeneration, KeyringBundle, SCHEMA_VERSION},
    error::{Error, Operation},
    http::{
        codec::Codec,
        io::HttpIo,
        pool::{ConnectionLease, HttpPool},
    },
    memory::pool::BufferPool,
    model::{
        context::OriginContext,
        identity::*,
        metadata::{ExpiresAt, MetadataSelector, ObjectMetadata},
    },
    peer::{
        PeerNetwork,
        handshake::Handshake,
        relay::Relay,
        requester::{PeerTransport, Requester},
        server::{LocalPageService, PeerServer},
        transfer::Transfers,
        wire::{self, FetchMode, PeerResponse, SignedRequest, SignedResponse, VerifiedRequest},
    },
    runtime::{admission::Admission, deadline::RequestScope, reactor::Reactor},
    security::{
        certificates::Certificates,
        credentials::CredentialCrypto,
        forwarding::Forwarding,
        identity::PendingIdentity,
        keyring::{KeyEpochs, Keyring},
        replay::{ReplayState, ReplayWindow},
        signing::Signatures,
    },
    topology::{
        health::LinkHealth,
        membership::{Member, Membership},
        paths::Paths,
        placement::Placement,
        rails::Rails,
    },
};
use std::{
    cell::{Cell, RefCell},
    net::TcpListener,
    rc::Rc,
    sync::Arc,
    task::{Context, Poll},
    time::{Duration, Instant},
};

const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
const CACHE: &str = "33333333-3333-4333-8333-333333333333";
fn node(n: usize) -> NodeId {
    NodeId(format!("22222222-2222-4222-8222-{n:012}"))
}
struct Identity {
    keys: Rc<Keyring>,
    certificates: Rc<Certificates>,
    replay: Rc<ReplayWindow>,
    signatures: Rc<Signatures>,
}
fn identities(nodes: &[NodeId]) -> Vec<Identity> {
    let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    params.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
    let issuer_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let issuer = params.self_signed(&issuer_key).unwrap();
    let roots = vec![issuer.der().to_vec()];
    nodes
        .iter()
        .map(|node| {
            let pending = PendingIdentity::generate().unwrap();
            let encoded = pending.export_pkcs8_for_persistence().unwrap();
            let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
                &rustls::pki_types::PrivatePkcs8KeyDer::from(encoded.as_slice()),
                &rcgen::PKCS_ED25519,
            )
            .unwrap();
            let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            params.subject_alt_names = vec![rcgen::SanType::URI(
                format!("spiffe://{CLUSTER}/node/{}", node.0)
                    .try_into()
                    .unwrap(),
            )];
            params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
            params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
            let cert = params.signed_by(&key, &issuer, &issuer_key).unwrap();
            let cluster = ClusterId(CLUSTER.into());
            let identity = pending
                .accept(
                    cluster.clone(),
                    node.clone(),
                    vec![cert.der().to_vec()],
                    &roots,
                )
                .unwrap();
            let keys = Rc::new(Keyring::new(
                cluster.clone(),
                node.clone(),
                Arc::new(KeyEpochs::default()),
            ));
            keys.install(KeyringBundle {
                schema_version: SCHEMA_VERSION,
                cluster: cluster.clone(),
                generation: BundleGeneration(1),
                peer_trust_roots: roots.clone(),
                cache_keys: vec![],
            })
            .unwrap();
            keys.install_signing_identity(Arc::new(identity)).unwrap();
            let certificates = Rc::new(Certificates::new(cluster, keys.clone()));
            let replay = Rc::new(ReplayWindow::new(Arc::new(ReplayState::default()), 128));
            let signatures = Rc::new(Signatures::new(
                keys.clone(),
                certificates.clone(),
                replay.clone(),
            ));
            Identity {
                keys,
                certificates,
                replay,
                signatures,
            }
        })
        .collect()
}
struct NeverRelay;
impl PeerTransport for NeverRelay {
    fn exchange<'a>(
        &'a self,
        _: SignedRequest,
        _: crate::topology::membership::MembershipLease,
        _: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        Box::pin(async { panic!("direct candidate must not relay") })
    }
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
        membership: crate::topology::membership::MembershipLease,
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
                wire::Operation::Metadata {
                    mode: FetchMode::Acquire,
                    ..
                }
            ));
            let inherited = super::serve::inherited_budget(&logical.route, scope)?;
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
        Absence::Bootstrap,
        Absence::Pinned,
        Absence::CachedPin,
    ] {
        remote_candidate(Some(case), false);
    }
}

#[test]
fn coordinator_copy_miss_is_not_origin_absence_and_pinned_missing_is_412() {
    use crate::peer::requester::PeerClient;
    struct NoPeers;
    impl PeerClient for NoPeers {
        fn request<'a>(
            &'a self,
            _: wire::PeerRequest,
            _: crate::topology::membership::MembershipLease,
            _: &'a RequestScope,
        ) -> Operation<'a, wire::VerifiedResponse> {
            Box::pin(async { panic!("single candidate must not probe peers") })
        }
    }
    let ids = identities(&[node(0), node(1)]);
    for id in &ids {
        for peer in &ids {
            id.signatures
                .configure_authenticated_peer_challenge(
                    peer.signatures.node().clone(),
                    peer.signatures.challenge().unwrap(),
                )
                .unwrap();
        }
    }
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            vec![Member {
                node: node(1),
                shares: std::num::NonZeroU32::new(4).unwrap(),
                peer_endpoint: "127.0.0.1:8000".into(),
                rails: vec![],
                alignment_enabled: false,
            }],
        )
        .unwrap(),
    );
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let calls = Rc::new(Cell::new(0));
    let (coordinator, _endpoint) = metadata_coordinator_with_newer_publication(
        &node(1),
        &membership,
        ids[1].keys.clone(),
        admission.clone(),
        Rc::new(Reactor::new(admission.clone())),
        Rc::new(NoPeers),
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
        let request = wire::PeerRequest {
            operation: wire::Operation::Metadata {
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
            route: crate::topology::paths::RouteBudget {
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
        let result = futures::executor::block_on(coordinator.serve_peer(
            admitted,
            membership.clone(),
            &scope,
        ))
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
        assert_eq!(calls.get(), index.saturating_sub(1));
    }
}

#[derive(Clone, Copy)]
enum Absence {
    Fresh,
    Bootstrap,
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
    use crate::control::{
        snapshot::{PublishedState, SnapshotStore},
        wire::{Publication, PublicationSequence},
    };
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
        caches: vec![],
        members: (0..4)
            .map(|i| Member {
                node: node(i),
                shares: std::num::NonZeroU32::new(4).unwrap(),
                peer_endpoint: address.clone(),
                rails: vec![],
                alignment_enabled: false,
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
    // Remaining-copy fixtures have authenticated sessions. The actual TCP
    // requester/destination pair still performs challenge discovery/handshake.
    for id in identities.iter().skip(1) {
        for peer in &identities {
            id.signatures
                .configure_authenticated_peer_challenge(
                    peer.signatures.node().clone(),
                    peer.signatures.challenge().unwrap(),
                )
                .unwrap();
        }
    }
    let a = &identities[0];
    let b = &identities[1];
    for peer in identities.iter().skip(2) {
        a.signatures
            .configure_authenticated_peer_challenge(
                peer.signatures.node().clone(),
                peer.signatures.challenge().unwrap(),
            )
            .unwrap();
    }
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(
            wire::MAX_ENVELOPE_HEAD,
            crate::model::range::PAGE_BYTES + 16,
        ),
        admission.clone(),
    ));
    let codec = Rc::new(wire::SecurityCodec::new(
        admission.clone(),
        Rc::new(BufferPool::new(admission.clone())),
    ));
    let transfers = Rc::new(
        Transfers::new(
            Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 2)),
            io.clone(),
            None,
        )
        .with_wire(admission.clone(), codec.clone()),
    );
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
    let handshake = |id: &Identity, network: Rc<PeerNetwork>| {
        Rc::new(
            Handshake::new(id.signatures.clone(), None)
                .with_http(network, transfers.clone())
                .with_discovery(id.keys.clone(), id.certificates.clone(), id.replay.clone()),
        )
    };
    let source_handshake = handshake(a, source_network.clone());
    let destination_handshake = handshake(b, destination_network.clone());
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 8));
    let auth = Rc::new(Forwarding::new(b.signatures.clone()));
    let relay = Rc::new(
        Relay::new(
            paths.clone(),
            auth.clone(),
            Rc::new(NeverRelay),
            admission.clone(),
        )
        .with_network(destination_network.clone()),
    );
    let calls = Rc::new(Cell::new(0));
    let metadata = ObjectMetadata {
        version: ObjectVersion {
            object: object.clone(),
            etag: StrongEtag::parse(b"\"remote\"").unwrap(),
        },
        length: 71,
        expires_at: ExpiresAt(std::time::SystemTime::now() + Duration::from_secs(60)),
    };
    let origin_calls = Rc::new(Cell::new(0));
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
                copies,
                origin_calls.clone(),
            )
        });
    let server = PeerServer::new(
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
    )
    .with_network(destination_network)
    .with_wire(codec)
    .with_handshake(destination_handshake);
    let requester = Rc::new(
        Requester::new(
            paths,
            Rc::new(Rails),
            Rc::new(Forwarding::new(a.signatures.clone())),
            source_handshake,
            transfers,
        )
        .with_network(source_network),
    );
    let requester: Rc<dyn crate::peer::requester::PeerClient> =
        if matches!(absence, Some(Absence::Pinned)) {
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
            })
        } else {
            requester
        };
    let ingress_calls = Rc::new(Cell::new(0));
    let ingress = matches!(absence, Some(Absence::Fresh | Absence::Bootstrap)).then(|| {
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
    let policy = CandidatePolicy::new(source, placement, requester);
    policy.set_credentials(Rc::new(CredentialCrypto::new(
        a.keys.clone(),
        admission.clone(),
    )));
    let context = OriginContext {
        object: object.clone(),
        metadata: None,
        authorization: None,
    };
    let scope =
        RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(30)).unwrap();
    let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
    let operation = wire::Operation::Metadata {
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
                Rc::new(crate::runtime::reactor::Descriptor::from(listener)),
                &scope,
            )
            .await?;
        let connection = ConnectionLease::from_accepted(fd, &admission)?;
        let connection = server.serve_connection(connection, &scope).await?;
        let connection = server.serve_connection(connection, &scope).await?;
        server.serve_connection(connection, &scope).await?;
        Ok::<(), Error>(())
    };
    let result = {
        // From here the candidate read itself owns the source lease. Network and
        // topology caches must not contribute hidden structural strong owners.
        drop(membership);
        let read = async {
            if let Some((coordinator, _endpoint)) = &ingress {
                use crate::client::request::{ClientRequest, ReadKind};
                let result = coordinator
                    .read_with_budget(
                        ClientRequest {
                            origin: context,
                            kind: if matches!(absence, Some(Absence::Bootstrap)) {
                                ReadKind::Bootstrap
                            } else {
                                ReadKind::Head
                            },
                        },
                        &scope,
                        budget.transfer(),
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
        ingress_calls.get(),
        0,
        "noncandidate ingress cannot call origin"
    );
    if let Some(case @ (Absence::Fresh | Absence::Bootstrap | Absence::Pinned)) = absence {
        let expected = if matches!(case, Absence::Fresh | Absence::Bootstrap) {
            Error::NotFound
        } else {
            Error::VersionUnavailable
        };
        assert_eq!(result.err(), Some(expected));
        let responses = crate::client::response::Responses::new(
            Rc::new(HttpIo::for_clients(reactor.clone(), admission.clone())),
            Rc::new(crate::memory::delivery::Delivery::new(
                Rc::new(crate::memory::pipe::PipePool::new(
                    admission.clone(),
                    reactor.clone(),
                )),
                Duration::from_secs(10),
            )),
        );
        assert!(matches!(responses.error_head(expected).unwrap().start,
                crate::http::codec::StartLine::Response { status }
                if status == if matches!(case, Absence::Fresh | Absence::Bootstrap) { 404 } else { 412 }));
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
                    .0
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_millis(),
                metadata
                    .expires_at
                    .0
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_millis()
            );
        }
    }
    assert_eq!(calls.get(), 1);
    if let Some(case) = absence {
        assert_eq!(
            origin_calls.get(),
            usize::from(!matches!(case, Absence::Pinned))
        );
        assert_eq!(
            copy_calls.borrow().len(),
            match case {
                Absence::Fresh | Absence::Bootstrap => 0,
                Absence::Pinned => 0,
                Absence::CachedPin => 1,
            }
        );
    }
}

struct OwnedCoordinator {
    coordinator: Rc<super::serve::Coordinator>,
    _endpoint: super::dispatch::WorkerEndpoint,
}

struct PinnedFallback {
    requester: Rc<Requester>,
    destination: NodeId,
    sender: Forwarding,
    receivers: Vec<(NodeId, Forwarding)>,
}
impl crate::peer::requester::PeerClient for PinnedFallback {
    fn request<'a>(
        &'a self,
        request: wire::PeerRequest,
        membership: crate::topology::membership::MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, wire::VerifiedResponse> {
        Box::pin(async move {
            if request.route.destination == self.destination {
                return crate::peer::requester::PeerClient::request(
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
        membership: crate::topology::membership::MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        self.coordinator.serve_peer(request, membership, scope)
    }
}

struct MissingOrigin(Rc<Cell<usize>>);
impl crate::origin::client::Origin for MissingOrigin {
    fn metadata<'a>(
        &'a self,
        authority: &'a super::candidates::OriginAuthority,
        context: &'a OriginContext,
        selector: MetadataSelector,
        scope: &'a RequestScope,
    ) -> Operation<'a, crate::origin::metadata::MetadataReply> {
        Box::pin(async move {
            scope.check()?;
            authority.validate(&context.object, PageNumber(0))?;
            self.0.set(self.0.get() + 1);
            let status = if matches!(selector, MetadataSelector::Fresh) {
                404
            } else {
                412
            };
            let raw = format!("HTTP/1.1 {status} Result\r\nContent-Length: 0\r\n\r\n");
            let (head, _) = Codec::new(32768, 0).decode_head(raw.as_bytes())?.unwrap();
            crate::origin::metadata::validate(&head, &context.object).map(|metadata| {
                crate::origin::metadata::MetadataReply {
                    metadata,
                    page_zero: None,
                }
            })
        })
    }
    fn page<'a>(
        &'a self,
        _: &'a super::candidates::OriginAuthority,
        _: &'a OriginContext,
        _: &'a PageId,
        _: &'a RequestScope,
    ) -> Operation<'a, crate::origin::page::OriginPage> {
        Box::pin(async { panic!("metadata absence must not fetch pages") })
    }
}

struct CachedCopies {
    sender: Rc<Forwarding>,
    receivers: Vec<(NodeId, Forwarding)>,
    calls: Rc<RefCell<Vec<NodeId>>>,
    metadata: Option<ObjectMetadata>,
}
impl crate::peer::requester::PeerClient for CachedCopies {
    fn request<'a>(
        &'a self,
        request: wire::PeerRequest,
        _: crate::topology::membership::MembershipLease,
        _: &'a RequestScope,
    ) -> Operation<'a, wire::VerifiedResponse> {
        Box::pin(async move {
            assert!(matches!(
                request.operation,
                wire::Operation::Metadata {
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

// Production read graph with only origin replies and later-candidate caches as
// doubles. No slab I/O or page crypto is needed for these metadata-only requests.
fn metadata_coordinator(
    node: &NodeId,
    membership: &Arc<Membership>,
    keys: Rc<Keyring>,
    admission: Rc<Admission>,
    reactor: Rc<Reactor>,
    peers: Rc<dyn crate::peer::requester::PeerClient>,
    calls: Rc<Cell<usize>>,
) -> (
    Rc<super::serve::Coordinator>,
    super::dispatch::WorkerEndpoint,
) {
    metadata_coordinator_with_newer_publication(
        node, membership, keys, admission, reactor, peers, calls, false,
    )
}

fn metadata_coordinator_with_newer_publication(
    node: &NodeId,
    membership: &Arc<Membership>,
    keys: Rc<Keyring>,
    admission: Rc<Admission>,
    reactor: Rc<Reactor>,
    peers: Rc<dyn crate::peer::requester::PeerClient>,
    calls: Rc<Cell<usize>>,
    newer_publication: bool,
) -> (
    Rc<super::serve::Coordinator>,
    super::dispatch::WorkerEndpoint,
) {
    use crate::{
        control::{
            snapshot::{PublishedState, SnapshotStore},
            wire::{Publication, PublicationSequence},
        },
        memory::{cache::MemoryCache, delivery::Delivery, pipe::PipePool},
        runtime::{
            crypto::{self, CryptoClient},
            worker::WorkerMap,
        },
        security::aead::PageCrypto,
        store::{
            eviction::SegmentClock, index::Index, reader::StoreReader, segment::Segments,
            slab::Slabs, writer::StoreWriter,
        },
    };
    let snapshots = Rc::new(SnapshotStore::new(
        ClusterId(CLUSTER.into()),
        Arc::new(PublishedState::default()),
        4,
    ));
    let mut publication = Publication {
        schema_version: SCHEMA_VERSION,
        cluster: ClusterId(CLUSTER.into()),
        sequence: PublicationSequence(1),
        membership_version: membership.version,
        members: membership.members().to_vec(),
        caches: vec![],
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
    let buffers = Rc::new(BufferPool::new(admission.clone()));
    let index = Rc::new(Index::new(WorkerId(0), 16));
    let segments = Rc::new(Segments::new(WorkerId(0), 64 * 1024 * 1024));
    let slabs = Rc::new(Slabs::new(
        WorkerId(0),
        "unused/absence-slabs".into(),
        reactor.clone(),
        1024 * 1024 * 1024,
        64 * 1024 * 1024,
    ));
    let disk = Rc::new(StoreReader::new(
        Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1)),
        index.clone(),
        segments.clone(),
        slabs.clone(),
        buffers.clone(),
    ));
    let writer = Rc::new(StoreWriter::new(index.clone(), segments, slabs));
    let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
    let candidates = Rc::new(CandidatePolicy::new(
        node.clone(),
        Rc::new(Placement::new(16)),
        peers.clone(),
    ));
    let origin = Rc::new(MissingOrigin(calls));
    let owners = Arc::new(
        super::dispatch::WorkerDirectory::new(
            Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
            vec![WorkerId(0)],
            16,
        )
        .unwrap(),
    );
    let (port, _engine) = crypto::pair(WorkerId(0), 0, std::num::NonZeroUsize::new(16).unwrap());
    let fill = Rc::new(super::fill::Fill::new(super::fill::FillDependencies {
        memory: Rc::new(MemoryCache::new(buffers.clone())),
        buffers,
        disk,
        writer,
        peers: peers.clone(),
        origin: origin.clone(),
        candidates: candidates.clone(),
        flights: Rc::new(super::flight::Flights::new(admission.clone())),
        crypto: Rc::new(PageCrypto::new(keys, Rc::new(CryptoClient::new(port)))),
        credentials: credentials.clone(),
        admission: admission.clone(),
        metadata_owner: owners.clone(),
    }));
    let metadata = Rc::new(super::metadata::MetadataService::new(
        candidates,
        origin,
        peers,
        credentials.clone(),
        16,
        super::metadata::MetadataDependencies {
            index,
            fill: fill.clone(),
            owners: owners.clone(),
        },
    ));
    let delivery = Rc::new(Delivery::new(
        Rc::new(PipePool::new(admission, reactor)),
        Duration::from_secs(10),
    ));
    let streams = Rc::new(super::range_stream::RangeStreams::new(
        fill.clone(),
        owners.clone(),
        delivery,
        2,
    ));
    let coordinator = Rc::new(super::serve::Coordinator::new(
        snapshots,
        metadata,
        fill,
        streams,
        credentials,
    ));
    let endpoint = owners.install(WorkerId(0), coordinator.clone()).unwrap();
    (coordinator, endpoint)
}
