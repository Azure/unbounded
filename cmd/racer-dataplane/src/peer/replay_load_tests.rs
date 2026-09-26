//! Full signed TCP body transfers with the configured node-wide replay budget.
use super::*;
use crate::{
    config::Config,
    http::{
        codec::Codec,
        io::HttpIo,
        pool::{ConnectionLease, HttpPool},
    },
    model::{
        envelope::PageEnvelope,
        metadata::{ExpiresAt, ObjectMetadata},
    },
    peer::{requester::PeerClient, server::LocalPageService},
    runtime::reactor::Reactor,
    security::aead::page_aad,
    topology::{
        health::LinkHealth,
        membership::{Member, Membership},
        paths::Paths,
        rails::Rails,
    },
};
use chacha20poly1305::{KeyInit, XChaCha20Poly1305, XNonce, aead::AeadInPlace};
use futures::{StreamExt, stream::FuturesUnordered};
use std::{
    cell::Cell,
    future::Future,
    net::TcpListener,
    num::NonZeroUsize,
    os::fd::OwnedFd,
    task::{Context, Poll},
    time::SystemTime,
};

struct BodyService {
    page: crate::memory::pool::CiphertextPage,
    metadata: ObjectMetadata,
    served: Cell<usize>,
}
impl LocalPageService for BodyService {
    fn serve_peer<'a>(
        &'a self,
        request: wire::VerifiedRequest,
        scope: &'a RequestScope,
    ) -> crate::error::Operation<'a, PeerResponse> {
        Box::pin(async move {
            scope.check()?;
            assert!(
                matches!(&request.request().operation, Operation::Page { page, .. } if page == &self.page.envelope().page)
            );
            self.served.set(self.served.get() + 1);
            Ok(PeerResponse::Page {
                metadata: self.metadata.clone(),
                ciphertext: self.page.clone(),
            })
        })
    }
}
struct NeverRelay;
impl requester::PeerTransport for NeverRelay {
    fn exchange<'a>(
        &'a self,
        _: wire::SignedRequest,
        _: &'a RequestScope,
    ) -> crate::error::Operation<'a, wire::SignedResponse> {
        Box::pin(async { panic!("direct transfer must not relay") })
    }
}

#[test]
#[ignore = "load regression: run with --release for representative sustained peer throughput"]
fn default_replay_budget_completes_64_concurrent_full_transfers() {
    const CONCURRENCY: usize = 64;
    const ROUNDS: usize = 1500;
    let (config, _) = Config::from_lookup_with_fabric_ports(|name| {
        Ok(match name {
            "RACER_CLUSTER_ID" => Some(CLUSTER.into()),
            "RACER_CONTROL_ENDPOINT" => Some("https://control.example:7443".into()),
            _ => None,
        })
    })
    .unwrap();
    let (signers, discovery) = identities_with_replay_capacity(config.limits.replay_entries.get());
    // Aggregate 64 active clients at a receiver. Permit all of them on this one
    // loopback neighbor instead of spreading them across live mesh neighbors.
    let mut limits = config.limits;
    limits.connections_per_neighbor = NonZeroUsize::new(CONCURRENCY).unwrap();
    let admission = Rc::new(Admission::new(limits));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(
            wire::MAX_ENVELOPE_HEAD,
            crate::model::range::PAGE_BYTES + 16,
        ),
        admission.clone(),
    ));
    let codec = Rc::new(codec(&admission));
    let transfers = Rc::new(
        transfer::Transfers::new(
            Rc::new(HttpPool::new(
                reactor.clone(),
                admission.clone(),
                CONCURRENCY,
            )),
            io.clone(),
            None,
        )
        .with_wire(admission.clone(), codec.clone()),
    );
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            [A, C]
                .iter()
                .map(|name| Member {
                    node: NodeId((*name).into()),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: listener.local_addr().unwrap().to_string(),
                    rails: vec![],
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let networks: Vec<_> = [A, C]
        .iter()
        .map(|name| {
            let network = Rc::new(PeerNetwork::new(NodeId((*name).into()), 1).unwrap());
            network.install(membership.clone()).unwrap();
            network
        })
        .collect();
    let handshake = |i: usize, network: Rc<PeerNetwork>| {
        Rc::new(
            handshake::Handshake::new(signers[i].clone(), None)
                .with_http(network, transfers.clone())
                .with_discovery(
                    discovery[i].0.clone(),
                    discovery[i].1.clone(),
                    discovery[i].2.clone(),
                ),
        )
    };
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 4, 1000));
    let auth = Rc::new(Forwarding::new(signers[2].clone()));
    let relay = Rc::new(
        relay::Relay::new(
            paths.clone(),
            auth.clone(),
            Rc::new(NeverRelay),
            admission.clone(),
        )
        .with_network(networks[1].clone()),
    );
    let clear: Vec<u8> = (0..1479).map(|i| (i % 251) as u8).collect();
    let page = PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId(CACHE.into()),
                key: CacheKey([3; 32]),
            },
            etag: StrongEtag::parse(b"\"manifest\"").unwrap(),
        },
        number: PageNumber(0),
    };
    let envelope = PageEnvelope {
        page: page.clone(),
        key_id: KeyId([1; 16]),
        nonce: Nonce([2; 24]),
        plaintext_length: clear.len() as u32,
        ciphertext_length: (clear.len() + 16) as u32,
    };
    let cipher = XChaCha20Poly1305::new((&[7; 32]).into());
    let mut body = clear.clone();
    cipher
        .encrypt_in_place(
            XNonce::from_slice(&envelope.nonce.0),
            &page_aad(&envelope).unwrap(),
            &mut body,
        )
        .unwrap();
    let ciphertext = BufferPool::new(admission.clone())
        .ciphertext(
            admission
                .reserve(
                    Some(&page.version.object.cache),
                    ResourceClass::Ciphertext,
                    body.capacity(),
                )
                .unwrap(),
            envelope,
            body,
        )
        .unwrap();
    let service = Rc::new(BodyService {
        page: ciphertext,
        metadata: ObjectMetadata {
            version: page.version.clone(),
            length: clear.len() as u64,
            expires_at: ExpiresAt(SystemTime::now() + Duration::from_secs(120)),
        },
        served: Cell::new(0),
    });
    let server = server::PeerServer::new(io, auth, admission.clone(), service.clone(), relay)
        .with_network(networks[1].clone())
        .with_wire(codec)
        .with_handshake(handshake(2, networks[1].clone()));
    let requester = requester::Requester::new(
        paths,
        Rc::new(Rails),
        Rc::new(Forwarding::new(signers[0].clone())),
        handshake(0, networks[0].clone()),
        transfers,
    )
    .with_network(networks[0].clone());
    let scope = RequestScope::new(
        RequestId([9; 16]),
        Instant::now() + Duration::from_secs(180),
    )
    .unwrap();
    let started = Instant::now();
    let server_work = async {
        let listener = Rc::new(OwnedFd::from(listener));
        let mut active = FuturesUnordered::new();
        for _ in 0..CONCURRENCY {
            let accepted = reactor.accept(listener.clone(), &scope).await?;
            let connection = ConnectionLease::from_accepted(accepted, &admission)?;
            active.push(async {
                let mut connection = connection;
                loop {
                    connection = server.serve_connection(connection, &scope).await?;
                    if !connection.is_reusable() {
                        return Ok::<(), Error>(());
                    }
                }
            });
        }
        while let Some(result) = active.next().await {
            result?;
        }
        Ok::<(), Error>(())
    };
    let clients = async {
        let jobs = (0..CONCURRENCY).map(|client| {
            let (admission, requester, page, scope, cipher, clear) =
                (&admission, &requester, &page, &scope, &cipher, &clear);
            async move {
                for round in 0..ROUNDS {
                    let mut local = request(admission, 1);
                    let id = ((client * ROUNDS + round) as u128).to_le_bytes();
                    local.route.request = RequestId(id);
                    local.route.attempt = AttemptId(id);
                    local.origin.request = local.route.request;
                    local.origin.attempt = local.route.attempt;
                    local.origin.scope =
                        RequestScope::new(local.route.request, scope.deadline.0).unwrap();
                    local.route.deadline = scope.deadline;
                    local.origin.authorization = None;
                    local.operation = Operation::Page {
                        page: page.clone(),
                        mode: FetchMode::CopyOnly,
                    };
                    let request_scope = local.origin.scope.clone();
                    let response = requester.request(local, &request_scope).await?;
                    let PeerResponse::Page {
                        metadata,
                        ciphertext,
                    } = response.response()
                    else {
                        panic!("missing full body")
                    };
                    assert_eq!(metadata.length, clear.len() as u64);
                    let mut received = ciphertext.bytes().to_vec();
                    cipher
                        .decrypt_in_place(
                            XNonce::from_slice(&ciphertext.envelope().nonce.0),
                            &page_aad(ciphertext.envelope()).unwrap(),
                            &mut received,
                        )
                        .unwrap();
                    assert_eq!(&received, clear);
                }
                Ok::<(), Error>(())
            }
        });
        futures::future::try_join_all(jobs).await
    };
    {
        let operation = futures::future::select(Box::pin(clients), Box::pin(server_work));
        let mut operation = std::pin::pin!(operation);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        loop {
            if let Poll::Ready(result) = operation.as_mut().poll(&mut cx) {
                match result {
                    futures::future::Either::Left((result, _)) => {
                        result.unwrap();
                    }
                    futures::future::Either::Right((result, _)) => {
                        panic!(
                            "server stopped after {} bodies in {:?}: {result:?}",
                            service.served.get(),
                            started.elapsed()
                        )
                    }
                }
                break;
            }
            assert!(started.elapsed() < Duration::from_secs(180));
            reactor.poll_budgeted(512).unwrap();
            reactor.wait(Duration::from_micros(100)).unwrap();
        }
    }
    assert_eq!(service.served.get(), CONCURRENCY * ROUNDS);
    eprintln!(
        "{} full authenticated transfers, {} concurrent, {} plaintext bytes, {:?}",
        service.served.get(),
        CONCURRENCY,
        service.served.get() * clear.len(),
        started.elapsed()
    );
}
