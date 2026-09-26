//! Full layer integrity through a signed TCP relay with idle-neighbor pressure.
use super::*;
use crate::{
    http::{
        codec::{Codec, MessageHead, StartLine},
        io::HttpIo,
        pool::{ConnectionLease, Endpoint, HttpPool},
    },
    model::{
        envelope::PageEnvelope,
        metadata::{ExpiresAt, ObjectMetadata},
        range::PAGE_BYTES,
    },
    peer::{requester::PeerTransport, server::LocalPageService},
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
use sha2::{Digest, Sha256};
use std::{
    cell::Cell, future::Future, net::TcpListener, num::NonZeroUsize, os::fd::OwnedFd,
    task::Context, time::SystemTime,
};

struct Layers {
    admission: Rc<Admission>,
    served: Cell<usize>,
}
impl LocalPageService for Layers {
    fn serve_peer<'a>(
        &'a self,
        request: wire::VerifiedRequest,
        scope: &'a RequestScope,
    ) -> crate::error::Operation<'a, PeerResponse> {
        Box::pin(async move {
            scope.check()?;
            assert_eq!(
                request.request().route.visited,
                vec![NodeId(A.into()), NodeId(B.into())]
            );
            let Operation::Page { page, .. } = &request.request().operation else {
                panic!("page required")
            };
            let mut body = plaintext(page);
            let mut nonce = [0; 24];
            getrandom::getrandom(&mut nonce).unwrap();
            let envelope = PageEnvelope {
                page: page.clone(),
                key_id: KeyId([1; 16]),
                nonce: Nonce(nonce),
                plaintext_length: body.len() as u32,
                ciphertext_length: body.len() as u32 + 16,
            };
            XChaCha20Poly1305::new((&[7; 32]).into())
                .encrypt_in_place(
                    XNonce::from_slice(&envelope.nonce.0),
                    &page_aad(&envelope).unwrap(),
                    &mut body,
                )
                .unwrap();
            let reservation = self.admission.reserve(
                Some(&page.version.object.cache),
                ResourceClass::Ciphertext,
                body.capacity(),
            )?;
            let ciphertext =
                BufferPool::new(self.admission.clone()).ciphertext(reservation, envelope, body)?;
            self.served.set(self.served.get() + 1);
            Ok(PeerResponse::Page {
                metadata: ObjectMetadata {
                    version: page.version.clone(),
                    length: 4 * PAGE_BYTES + 17,
                    expires_at: ExpiresAt(SystemTime::now() + Duration::from_secs(60)),
                },
                ciphertext,
            })
        })
    }
}
fn plaintext(page: &PageId) -> Vec<u8> {
    let length = if page.number.0 == 4 {
        17
    } else {
        PAGE_BYTES as usize
    };
    vec![page.version.object.key.0[0].wrapping_add(page.number.0 as u8); length]
}
struct Never;
impl LocalPageService for Never {
    fn serve_peer<'a>(
        &'a self,
        _: wire::VerifiedRequest,
        _: &'a RequestScope,
    ) -> crate::error::Operation<'a, PeerResponse> {
        Box::pin(async { panic!("relay must not serve locally") })
    }
}
impl PeerTransport for Never {
    fn exchange<'a>(
        &'a self,
        _: wire::SignedRequest,
        _: &'a RequestScope,
    ) -> crate::error::Operation<'a, wire::SignedResponse> {
        Box::pin(async { panic!("destination must not relay") })
    }
}

#[test]
#[ignore = "full eight-layer TCP integrity regression: run with --release"]
fn full_image_through_relay_reclaims_idle_neighbor_capacity() {
    full_image_through_relay(false, 4);
}

#[test]
#[ignore = "full eight-layer TCP integrity regression: run with --release"]
fn full_image_through_relay_transfers_receive_charge() {
    for concurrency in [1, 4] {
        full_image_through_relay(true, concurrency);
    }
}

fn full_image_through_relay(receive_pressure: bool, concurrency: usize) {
    const LAYERS: usize = 8;
    let (signers, discovery) = identities_with_replay_capacity(4096);
    for signer in &signers {
        for peer in &signers {
            signer
                .configure_authenticated_peer_challenge(
                    peer.node().clone(),
                    peer.challenge().unwrap(),
                )
                .unwrap();
        }
    }
    let listeners: Vec<_> = (0..3)
        .map(|_| {
            let l = TcpListener::bind("127.0.0.1:0").unwrap();
            l.set_nonblocking(true).unwrap();
            l
        })
        .collect();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            [A, B, C]
                .iter()
                .zip(&listeners)
                .map(|(n, l)| Member {
                    node: NodeId((*n).into()),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: l.local_addr().unwrap().to_string(),
                    rails: vec![],
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let admissions: Vec<_> = (0..3)
        .map(|i| {
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.client_connections = NonZeroUsize::new(if i == 1 { 16 } else { 32 }).unwrap();
            limits.ciphertext_bytes = NonZeroUsize::new(512 * 1024 * 1024).unwrap();
            if receive_pressure && i == 0 {
                limits.ciphertext_bytes =
                    NonZeroUsize::new((3 * concurrency + 1) * (PAGE_BYTES as usize + 16) - 1)
                        .unwrap();
            }
            Rc::new(Admission::new(limits))
        })
        .collect();
    let reactors: Vec<_> = admissions
        .iter()
        .map(|a| Rc::new(Reactor::new(a.clone())))
        .collect();
    let ios: Vec<_> = (0..3)
        .map(|i| {
            Rc::new(HttpIo::with_admission(
                reactors[i].clone(),
                Codec::new(wire::MAX_ENVELOPE_HEAD, PAGE_BYTES + 16),
                admissions[i].clone(),
            ))
        })
        .collect();
    let pools: Vec<_> = (0..3)
        .map(|i| {
            Rc::new(HttpPool::new(
                reactors[i].clone(),
                admissions[i].clone(),
                concurrency,
            ))
        })
        .collect();
    let codecs: Vec<_> = admissions.iter().map(|a| Rc::new(codec(a))).collect();
    let transfers: Vec<_> = (0..3)
        .map(|i| {
            Rc::new(
                transfer::Transfers::new(pools[i].clone(), ios[i].clone(), None)
                    .with_wire(admissions[i].clone(), codecs[i].clone()),
            )
        })
        .collect();
    let networks: Vec<_> = [A, B, C]
        .iter()
        .map(|n| {
            let n = Rc::new(PeerNetwork::new(NodeId((*n).into()), 1).unwrap());
            n.install(membership.clone()).unwrap();
            n
        })
        .collect();
    let auth: Vec<_> = signers
        .iter()
        .map(|s| Rc::new(Forwarding::new(s.clone())))
        .collect();
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 16, 1000));
    let handshake = |i: usize| {
        Rc::new(
            handshake::Handshake::new(signers[i].clone(), None)
                .with_http(networks[i].clone(), transfers[i].clone())
                .with_discovery(
                    discovery[i].0.clone(),
                    discovery[i].1.clone(),
                    discovery[i].2.clone(),
                ),
        )
    };
    let outbound = Rc::new(
        requester::Requester::new(
            paths.clone(),
            Rc::new(Rails),
            auth[1].clone(),
            handshake(1),
            transfers[1].clone(),
        )
        .with_network(networks[1].clone()),
    );
    let relay = Rc::new(
        relay::Relay::new(
            paths.clone(),
            auth[1].clone(),
            outbound,
            admissions[1].clone(),
        )
        .with_network(networks[1].clone()),
    );
    let layers = Rc::new(Layers {
        admission: admissions[2].clone(),
        served: Cell::new(0),
    });
    let servers = [
        server::PeerServer::new(
            ios[1].clone(),
            auth[1].clone(),
            admissions[1].clone(),
            Rc::new(Never),
            relay,
        )
        .with_network(networks[1].clone())
        .with_wire(codecs[1].clone()),
        server::PeerServer::new(
            ios[2].clone(),
            auth[2].clone(),
            admissions[2].clone(),
            layers.clone(),
            Rc::new(relay::Relay::new(
                paths,
                auth[2].clone(),
                Rc::new(Never),
                admissions[2].clone(),
            )),
        )
        .with_network(networks[2].clone())
        .with_wire(codecs[2].clone()),
    ];
    let scope = RequestScope::new(
        RequestId([9; 16]),
        Instant::now() + Duration::from_secs(100),
    )
    .unwrap();
    // Twelve completed real HTTP connections to distinct inactive neighbors.
    // Four incoming layer requests consume the remaining relay connection slots.
    let idle_listeners: Vec<_> = (0..12)
        .map(|_| TcpListener::bind("127.0.0.1:0").unwrap())
        .collect();
    let idle_endpoints: Vec<_> = idle_listeners
        .iter()
        .map(|l| Endpoint::Peer(l.local_addr().unwrap().to_string()))
        .collect();
    let idle_server = std::thread::spawn(move || {
        use std::io::{Read, Write};
        let mut sockets = Vec::new();
        for listener in idle_listeners {
            listener.set_nonblocking(true).unwrap();
            let end = Instant::now() + Duration::from_secs(10);
            let mut socket = loop {
                match listener.accept() {
                    Ok((s, _)) => break s,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        assert!(Instant::now() < end);
                        std::thread::sleep(Duration::from_millis(1));
                    }
                    Err(e) => panic!("{e}"),
                }
            };
            socket
                .set_read_timeout(Some(Duration::from_secs(10)))
                .unwrap();
            socket
                .set_write_timeout(Some(Duration::from_secs(10)))
                .unwrap();
            let mut head = Vec::new();
            while !head.ends_with(b"\r\n\r\n") {
                let mut b = [0];
                socket.read_exact(&mut b).unwrap();
                head.push(b[0]);
            }
            socket
                .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
                .unwrap();
            sockets.push(socket);
        }
        sockets
    });
    let drive = |mut work: std::pin::Pin<Box<dyn Future<Output = ()> + '_>>| {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        loop {
            if work.as_mut().poll(&mut cx).is_ready() {
                break;
            }
            scope.check().unwrap();
            for r in &reactors {
                r.poll_budgeted(256).unwrap();
            }
            reactors[0].wait(Duration::from_micros(100)).unwrap();
        }
    };
    drive(Box::pin(async {
        for endpoint in &idle_endpoints {
            let connection = pools[1].checkout(endpoint, &scope).await.unwrap();
            let head = MessageHead {
                start: StartLine::Request {
                    method: "GET".into(),
                    target: "/".into(),
                },
                headers: vec![crate::http::codec::Header {
                    name: "content-length".into(),
                    value: b"0".to_vec(),
                }],
            };
            let mut response = ios[1]
                .exchange_head(connection, head, &scope)
                .await
                .unwrap()
                .connection;
            response.finish_exchange().unwrap();
        }
    }));
    let _idle_sockets = idle_server.join().unwrap();
    assert_eq!(admissions[1].used(ResourceClass::Connection), 12);
    let server_work = async {
        let mut active = FuturesUnordered::new();
        for i in 0..2 {
            let server = &servers[i];
            let reactor = &reactors[i + 1];
            let admission = &admissions[i + 1];
            let scope = &scope;
            let listener = Rc::new(OwnedFd::from(listeners[i + 1].try_clone().unwrap()));
            active.push(async move {
                let mut connections = FuturesUnordered::new();
                for _ in 0..concurrency {
                    let fd = reactor.accept(listener.clone(), scope).await?;
                    let mut connection = ConnectionLease::from_accepted(fd, admission)?;
                    connections.push(async move {
                        loop {
                            connection = server.serve_connection(connection, scope).await?;
                            if !connection.is_reusable() {
                                return Ok::<(), Error>(());
                            }
                        }
                    });
                }
                while let Some(result) = connections.next().await {
                    result?;
                }
                Ok::<(), Error>(())
            });
        }
        while let Some(result) = active.next().await {
            result?;
        }
        Ok::<(), Error>(())
    };
    let clients = async {
        for batch in 0..LAYERS / concurrency {
            let completed: Vec<_> = (0..5).map(|_| Cell::new(0)).collect();
            let mut jobs = FuturesUnordered::new();
            for layer in batch * concurrency..(batch + 1) * concurrency {
                let completed = &completed;
                let (auth, transfers, admission, scope, endpoint) = (
                    &auth[0],
                    &transfers[0],
                    &admissions[0],
                    &scope,
                    Endpoint::Peer(membership.members()[1].peer_endpoint.clone()),
                );
                jobs.push(async move {
                    let mut expected = Sha256::new();
                    let mut actual = Sha256::new();
                    let mut size = 0;
                    let mut pressure = None;
                    for (number, completed) in completed.iter().enumerate() {
                        // Two other live pages leave room for the received page,
                        // but not a second charge for its exact same allocation.
                        // Page zero succeeds first, reproducing a late 16 MiB cut.
                        if receive_pressure && number == 1 {
                            pressure = Some(admission.reserve(
                                Some(&CacheId(CACHE.into())),
                                ResourceClass::Ciphertext,
                                2 * (PAGE_BYTES as usize + 16),
                            ).unwrap());
                        }
                        let mut local = request(admission, (layer * 5 + number + 1) as u8);
                        let page = PageId {
                            version: ObjectVersion {
                                object: ObjectId {
                                    cache: CacheId(CACHE.into()),
                                    key: CacheKey([layer as u8; 32]),
                                },
                                etag: StrongEtag::parse(b"\"layer\"").unwrap(),
                            },
                            number: PageNumber(number as u64),
                        };
                        local.origin.object = page.version.object.clone();
                        local.origin.authorization = None;
                        local.operation = Operation::Page {
                            page: page.clone(),
                            mode: FetchMode::CopyOnly,
                        };
                        let (signed, binding) =
                            auth.sign_request_to(local, &NodeId(B.into())).unwrap();
                        let response = auth
                            .verify_response(
                                transfers
                                    .exchange(endpoint.clone(), signed, scope)
                                    .await
                                    .unwrap_or_else(|e| panic!("layer {layer} page {number}, received {size} bytes: {e:?}")),
                                &binding,
                            )
                            .unwrap();
                        let PeerResponse::Page {
                            metadata,
                            ciphertext,
                        } = response.response()
                        else {
                            panic!(
                                "layer {layer} page {number}: relay did not return a complete page"
                            )
                        };
                        assert_eq!(metadata.length, 4 * PAGE_BYTES + 17);
                        assert_eq!(ciphertext.envelope().page, page);
                        let mut body = ciphertext.bytes().to_vec();
                        XChaCha20Poly1305::new((&[7; 32]).into())
                            .decrypt_in_place(
                                XNonce::from_slice(&ciphertext.envelope().nonce.0),
                                &page_aad(ciphertext.envelope()).unwrap(),
                                &mut body,
                            )
                            .unwrap();
                        expected.update(plaintext(&page));
                        actual.update(&body);
                        size += body.len();
                        if receive_pressure {
                            // Hold every completed page until this concurrent
                            // window arrives, making the peak deterministic.
                            completed.set(completed.get() + 1);
                            std::future::poll_fn(|cx| {
                                scope.check()?;
                                if completed.get() == concurrency {
                                    std::task::Poll::Ready(Ok::<(), Error>(()))
                                } else {
                                    cx.waker().wake_by_ref();
                                    std::task::Poll::Pending
                                }
                            }).await.unwrap();
                        }
                    }
                    assert_eq!(size as u64, 4 * PAGE_BYTES + 17);
                    assert_eq!(actual.finalize(), expected.finalize());
                    drop(pressure);
                });
            }
            while jobs.next().await.is_some() {}
        }
    };
    drive(Box::pin(async {
        match futures::future::select(Box::pin(clients), Box::pin(server_work)).await {
            futures::future::Either::Left(_) => {}
            futures::future::Either::Right((result, _)) => {
                panic!("peer server stopped: {result:?}")
            }
        }
    }));
    assert_eq!(layers.served.get(), LAYERS * 5);
    for pool in &pools {
        pool.close();
    }
    drive(Box::pin(async {
        for reactor in &reactors {
            reactor.drain().await.unwrap();
        }
    }));
    for admission in &admissions {
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        assert_eq!(admission.used(ResourceClass::Relay), 0);
    }
    eprintln!(
        "verified {LAYERS} full layers through two TCP links, {concurrency} concurrent layers, {} bytes",
        LAYERS as u64 * (4 * PAGE_BYTES + 17)
    );
}
