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
    cell::Cell,
    net::TcpListener,
    os::fd::OwnedFd,
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
}
impl LocalPageService for CandidateService {
    fn serve_peer<'a>(
        &'a self,
        request: VerifiedRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        Box::pin(async move {
            scope.check()?;
            let request = request.request();
            assert_eq!(request.route.visited, vec![self.sender.clone()]);
            assert_eq!(request.route.remaining_links, 4);
            assert!(request.route.remaining_attempts > 0);
            assert!(matches!(
                request.operation,
                wire::Operation::Metadata {
                    mode: FetchMode::Acquire,
                    ..
                }
            ));
            let inherited = super::serve::inherited_budget(&request.route, scope)?;
            assert_eq!(
                inherited.remaining_attempts(),
                request.route.remaining_attempts
            );
            assert_eq!(
                inherited.remaining_links(),
                3,
                "the final incoming link is charged exactly once"
            );
            self.calls.set(self.calls.get() + 1);
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
    for forbidden in [false, true] {
        let object = ObjectId {
            cache: CacheId(CACHE.into()),
            key: CacheKey([3; 32]),
        };
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let address = listener.local_addr().unwrap().to_string();
        let membership = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                (0..4)
                    .map(|i| Member {
                        node: node(i),
                        shares: std::num::NonZeroU32::new(4).unwrap(),
                        peer_endpoint: address.clone(),
                        rails: vec![],
                        alignment_enabled: false,
                    })
                    .collect(),
            )
            .unwrap(),
        );
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
        let identities = identities(&[source.clone(), destination.clone()]);
        let a = &identities[0];
        let b = &identities[1];
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
        let source_network = Rc::new(PeerNetwork::new(source.clone(), 1).unwrap());
        let destination_network = Rc::new(PeerNetwork::new(destination, 1).unwrap());
        source_network.install(membership.clone()).unwrap();
        destination_network.install(membership.clone()).unwrap();
        let handshake = |id: &Identity, network: Rc<PeerNetwork>| {
            Rc::new(
                Handshake::new(id.signatures.clone(), None)
                    .with_http(network, transfers.clone())
                    .with_discovery(id.keys.clone(), id.certificates.clone(), id.replay.clone()),
            )
        };
        let source_handshake = handshake(a, source_network.clone());
        let destination_handshake = handshake(b, destination_network.clone());
        let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 8, 1000));
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
        let server = PeerServer::new(
            io,
            auth,
            admission.clone(),
            Rc::new(CandidateService {
                calls: calls.clone(),
                sender: source.clone(),
                metadata: metadata.clone(),
                forbidden,
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
        let scope = RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(30))
            .unwrap();
        let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
        let operation = wire::Operation::Metadata {
            object,
            selector: MetadataSelector::Fresh,
            mode: FetchMode::Acquire,
        };
        let service = async {
            let fd = reactor
                .accept(Rc::new(OwnedFd::from(listener)), &scope)
                .await?;
            let connection = ConnectionLease::from_accepted(fd, &admission)?;
            let connection = server.serve_connection(connection, &scope).await?;
            let connection = server.serve_connection(connection, &scope).await?;
            server.serve_connection(connection, &scope).await?;
            Ok::<(), Error>(())
        };
        let result = {
            let exchange = futures::future::join(
                policy.resolve_with_budget(ranked, &context, operation, &scope, &mut budget),
                service,
            );
            let mut exchange = std::pin::pin!(exchange);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            loop {
                if let Poll::Ready((result, served)) =
                    std::future::Future::poll(exchange.as_mut(), &mut cx)
                {
                    served.unwrap();
                    break result;
                }
                reactor.poll_budgeted(128).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
        };
        assert_eq!(
            budget.remaining_attempts(),
            10,
            "one send plus five delegated credits charged once"
        );
        assert_eq!(budget.remaining_links(), 20);
        if forbidden {
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
        assert_eq!(calls.get(), 1);
    }
}
