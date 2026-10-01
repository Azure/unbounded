use super::*;
use crate::{
    http::{
        Codec,
        connection::{BufferRange, HttpIo},
        connection::{ConnectionLease, HttpPool},
    },
    model::{ExpiresAt, ObjectMetadata, PageEnvelope},
    runtime::reactor::{IoBuffer, Reactor},
    security::connection,
    topology::{
        health::LinkHealth,
        membership::{Member, Membership},
        routing::Paths,
    },
};
use chacha20poly1305::{
    XChaCha20Poly1305,
    aead::{Aead, KeyInit},
};
use std::{
    cell::Cell,
    net::{TcpListener, TcpStream},
    task::{Context, Poll},
};

struct Never;
impl server::LocalPageService for Never {
    fn serve_peer<'a>(
        &'a self,
        _: protocol::VerifiedRequest,
        _: crate::topology::membership::MembershipLease,
        _: &'a RequestScope,
    ) -> crate::error::Operation<'a, PeerResponse> {
        Box::pin(async { panic!("relay must not acquire or decrypt") })
    }
}
fn drive<T>(
    reactors: &[Rc<Reactor>],
    work: impl std::future::Future<Output = T>,
    inspect: impl Fn(),
) -> T {
    let mut work = std::pin::pin!(work);
    let deadline = Instant::now() + Duration::from_secs(25);
    loop {
        if let Poll::Ready(result) = work
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
        {
            return result;
        }
        inspect();
        assert!(Instant::now() < deadline, "opaque relay watchdog");
        let mut progress = 0;
        for r in reactors {
            progress += r.poll_budgeted(128).unwrap();
        }
        if progress == 0 {
            std::thread::yield_now();
        }
    }
}

// Wall and thread CPU include all three roles. The same signed fixture can
// execute the pre-change materialized relay path for a local mechanism A/B.
fn exchange(
    materialized: bool,
    fallback: bool,
    rounds: usize,
    fragmented: bool,
    truncated: bool,
) -> (Duration, Duration) {
    RelayFixture::new(materialized).run(materialized, fallback, rounds, fragmented, truncated)
}

struct RelayFixture {
    signers: Vec<Rc<Signatures>>,
    admissions: Vec<Rc<Admission>>,
    reactors: Vec<Rc<Reactor>>,
    ios: Vec<Rc<HttpIo>>,
    listener: TcpListener,
    requester_socket: TcpStream,
    relay_socket: TcpStream,
    pool: Rc<HttpPool>,
    server: server::PeerServer,
    scope: RequestScope,
    plaintext: Vec<u8>,
    cipher: XChaCha20Poly1305,
    body: Vec<u8>,
    metadata: ObjectMetadata,
    page: crate::memory::pool::CiphertextPage,
}

impl RelayFixture {
    fn new(materialized: bool) -> Self {
        let signers = signers();
        let admissions: Vec<_> = (0..3)
            .map(|_| {
                Rc::new(Admission::new(
                    crate::test_support::cluster::config(false).limits,
                ))
            })
            .collect();
        let reactors: Vec<_> = admissions
            .iter()
            .map(|a| Rc::new(Reactor::new(a.clone())))
            .collect();
        let ios: Vec<_> = reactors
            .iter()
            .zip(&admissions)
            .map(|(r, a)| {
                Rc::new(HttpIo::with_admission(
                    r.clone(),
                    Codec::new(protocol::MAX_ENVELOPE_HEAD, crate::model::PAGE_BYTES + 16),
                    a.clone(),
                ))
            })
            .collect();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let dst_address = listener.local_addr().unwrap();
        let ingress = TcpListener::bind("127.0.0.1:0").unwrap();
        let requester_socket = TcpStream::connect(ingress.local_addr().unwrap()).unwrap();
        let (relay_socket, _) = ingress.accept().unwrap();
        let membership = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                [A, B, C]
                    .iter()
                    .enumerate()
                    .map(|(i, n)| Member {
                        node: NodeId((*n).into()),
                        shares: std::num::NonZeroU32::new(1).unwrap(),
                        peer_endpoint: if i == 2 {
                            dst_address.to_string()
                        } else {
                            format!("127.0.0.1:{}", 8000 + i)
                        },
                        rails: vec![],
                        alignment_enabled: false,
                        site: String::new(),
                    })
                    .collect(),
            )
            .unwrap(),
        );
        let network = Rc::new(
            PeerNetwork::new(
                NodeId(B.into()),
                crate::control::state::PublishedState::for_membership(membership),
            )
            .unwrap(),
        );
        let auth = Rc::new(Forwarding::new(signers[1].clone()));
        let pool = Rc::new(HttpPool::new(reactors[1].clone(), admissions[1].clone(), 1));
        let transfers = Rc::new(transport::Transfers::new(
            pool.clone(),
            ios[1].clone(),
            None,
            admissions[1].clone(),
            Rc::new(codec(&admissions[1])),
            signers[1].clone(),
        ));
        let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 4));
        let requester = Rc::new(Requester::new(
            paths.clone(),
            auth.clone(),
            transfers.clone(),
            network.clone(),
        ));
        let relay = Rc::new(Relay::new(
            paths,
            auth.clone(),
            requester,
            admissions[1].clone(),
            network.clone(),
        ));
        let server = server::PeerServer::for_test(
            ios[1].clone(),
            auth,
            admissions[1].clone(),
            Rc::new(Never),
            relay,
            Rc::new(codec(&admissions[1])),
            signers[1].clone(),
        )
        .with_transfers(transfers)
        .with_opaque_relay(!materialized);
        let scope = RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(25))
            .unwrap();
        let plaintext: Vec<_> = (0..crate::model::PAGE_BYTES as usize)
            .map(|i| (i % 251) as u8)
            .collect();
        let cipher = XChaCha20Poly1305::new((&[7; 32]).into());
        let body = cipher
            .encrypt((&[8; 24]).into(), plaintext.as_slice())
            .unwrap();
        assert_eq!(body.len(), 16 * 1024 * 1024 + 16);
        let metadata = ObjectMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(CACHE.into()),
                    key: CacheKey([3; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            length: plaintext.len() as u64,
            expires_at: ExpiresAt(std::time::SystemTime::now() + Duration::from_secs(60)),
        };
        let envelope = PageEnvelope {
            page: PageId {
                version: metadata.version.clone(),
                number: PageNumber(0),
            },
            key_id: KeyId([1; 16]),
            nonce: Nonce([8; 24]),
            plaintext_length: plaintext.len() as u32,
            ciphertext_length: body.len() as u32,
        };
        let page = BufferPool::new(admissions[2].clone())
            .ciphertext(
                admissions[2]
                    .reserve(
                        Some(&CacheId(CACHE.into())),
                        ResourceClass::Ciphertext,
                        body.len(),
                    )
                    .unwrap(),
                envelope,
                body.clone(),
            )
            .unwrap();
        Self {
            signers,
            admissions,
            reactors,
            ios,
            listener,
            requester_socket,
            relay_socket,
            pool,
            server,
            scope,
            plaintext,
            cipher,
            body,
            metadata,
            page,
        }
    }

    fn run(
        self,
        materialized: bool,
        fallback: bool,
        rounds: usize,
        fragmented: bool,
        truncated: bool,
    ) -> (Duration, Duration) {
        let Self {
            signers,
            admissions,
            reactors,
            ios,
            listener,
            requester_socket,
            relay_socket,
            pool,
            server,
            scope,
            plaintext,
            cipher,
            body,
            metadata,
            page,
        } = self;
        let accepted = Cell::new(0);
        let destination = async {
            let fd = reactors[2].accept(Rc::new(listener.into()), &scope).await?;
            accepted.set(accepted.get() + 1);
            let conn = ConnectionLease::from_accepted(fd, &admissions[2])?;
            let mut conn = connection::accept(&ios[2], conn, signers[2].clone(), &scope).await?;
            for i in 0..rounds {
                let received = ios[2].receive_head(conn, &scope).await?;
                let (head, length) = decode_envelope(received.value, false)?;
                assert_eq!(length, 0);
                let request = codec(&admissions[2]).request(head, &scope)?;
                let auth = Forwarding::new(signers[2].clone());
                let request = auth.verify_request(request)?;
                assert_eq!(
                    request.request().route.visited,
                    vec![NodeId(A.into()), NodeId(B.into())]
                );
                assert_eq!(
                    request
                        .request()
                        .origin
                        .authorization
                        .as_ref()
                        .unwrap()
                        .ciphertext,
                    vec![5; 32]
                );
                let response = match request.request().operation {
                    protocol::Operation::Bootstrap { .. } => PeerResponse::Bootstrap {
                        metadata: metadata.clone(),
                        page_zero: Some(page.clone()),
                    },
                    _ => PeerResponse::Page {
                        metadata: metadata.clone(),
                        ciphertext: page.clone(),
                    },
                };
                let response = auth.sign_response(request.binding(), response)?;
                conn = received.connection;
                let head = encode_envelope(&response.authentication, true, body.len())?;
                if truncated && i + 1 == rounds {
                    conn = ios[2].send_head(conn, head, &scope).await?.connection;
                    let done = ios[2]
                        .write_body_range(conn, page.clone(), 0..5, &scope)
                        .await?;
                    drop(done);
                    return Ok::<_, Error>(());
                }
                if fragmented {
                    let head = conn.session.as_mut().unwrap().sign(head)?;
                    let mut encoded =
                        Codec::new(protocol::MAX_ENVELOPE_HEAD, crate::model::PAGE_BYTES + 16)
                            .encode_head(&head)?;
                    encoded.extend_from_slice(&body[..173]);
                    for chunk in encoded.chunks(997) {
                        let mut buffer = ios[2].buffer(chunk.len())?;
                        buffer.bytes_mut()?.copy_from_slice(chunk);
                        let mut offset = 0;
                        while offset < chunk.len() {
                            let done = reactors[2]
                                .send(
                                    conn.socket(),
                                    BufferRange::new(buffer, offset..chunk.len())?,
                                    conn,
                                    &scope,
                                )
                                .await?;
                            offset += done.bytes;
                            buffer = done.buffer.into_inner();
                            conn = done.lease;
                        }
                    }
                    conn.tx_remaining = Some((body.len() - 173) as u64);
                    for start in (173..body.len()).step_by(65521) {
                        conn = ios[2]
                            .write_body_range(
                                conn,
                                page.clone(),
                                start..(start + 65521).min(body.len()),
                                &scope,
                            )
                            .await?
                            .lease;
                    }
                } else {
                    conn = ios[2].send_head(conn, head, &scope).await?.connection;
                    conn = ios[2].write_body(conn, page.clone(), &scope).await?.lease;
                }
                conn.finish_exchange()?;
            }
            Ok::<_, Error>(())
        };
        let relay = async {
            let mut conn = ConnectionLease::from_accepted(relay_socket.into(), &admissions[1])?;
            conn.relay_fallback = fallback;
            for i in 0..rounds {
                let result = server.serve_connection(conn, &scope).await;
                if truncated && i + 1 == rounds {
                    assert!(matches!(result, Err(Error::Io)));
                    return Ok::<_, Error>(());
                }
                conn = result?;
                assert!(conn.is_reusable());
            }
            Ok::<_, Error>(())
        };
        let client = async {
            let conn = ConnectionLease::from_accepted(requester_socket.into(), &admissions[0])?;
            let mut conn =
                connection::connect(&ios[0], conn, signers[0].clone(), signers[1].node(), &scope)
                    .await?;
            for i in 0..rounds {
                let mut request = request(&admissions[0], i as u8);
                request.operation = if i % 2 == 0 {
                    protocol::Operation::Bootstrap {
                        object: metadata.version.object.clone(),
                        mode: protocol::FetchMode::CopyOnly,
                    }
                } else {
                    protocol::Operation::Page {
                        page: page.envelope().page.clone(),
                        mode: protocol::FetchMode::CopyOnly,
                    }
                };
                let auth = Forwarding::new(signers[0].clone());
                let (signed, binding) = auth.sign_request_to(request, signers[1].node())?;
                let received = ios[0]
                    .exchange_head(
                        conn,
                        encode_envelope(&signed.authentication, false, 0)?,
                        &scope,
                    )
                    .await?;
                let (head, length) = decode_envelope(received.value, true)?;
                assert_eq!(head.hops.len(), 1);
                assert_eq!(length, body.len());
                conn = received.connection;
                let mut bytes = Vec::with_capacity(length);
                while bytes.len() < length {
                    let buffer = ios[0].buffer(if fragmented { 32749 } else { 65536 })?;
                    let result = ios[0].read_body(conn, buffer, &scope).await;
                    if truncated && i + 1 == rounds && result.is_err() {
                        assert!(matches!(result, Err(Error::Io)));
                        assert_eq!(bytes, body[..5], "no appended error after signed success");
                        return Ok::<_, Error>(());
                    }
                    let done = result?;
                    bytes.extend_from_slice(&done.buffer.bytes()?[..done.bytes]);
                    conn = done.lease;
                    if fragmented {
                        let start = Instant::now();
                        std::future::poll_fn(|cx| {
                            if start.elapsed() >= Duration::from_micros(50) {
                                Poll::Ready(())
                            } else {
                                cx.waker().wake_by_ref();
                                Poll::Pending
                            }
                        })
                        .await;
                    }
                }
                assert_eq!(bytes, body);
                let response = codec(&admissions[0]).response(head, bytes, &scope)?;
                let response = auth.verify_response(response, &binding)?;
                let ciphertext = match response.response() {
                    PeerResponse::Page { ciphertext, .. }
                    | PeerResponse::Bootstrap {
                        page_zero: Some(ciphertext),
                        ..
                    } => ciphertext,
                    _ => panic!("page required"),
                };
                assert_eq!(
                    cipher
                        .decrypt((&[8; 24]).into(), ciphertext.bytes())
                        .unwrap(),
                    plaintext
                );
                conn.finish_exchange()?;
            }
            Ok::<_, Error>(())
        };
        let start = Instant::now();
        let started_cpu = cpu();
        drive(
            &reactors,
            async { futures::try_join!(destination, relay, client) },
            || {
                if !materialized {
                    assert_eq!(admissions[1].used(ResourceClass::Ciphertext), 0);
                    assert_eq!(admissions[1].used(ResourceClass::Plaintext), 0);
                    assert!(admissions[1].used(ResourceClass::Pipe) <= 1);
                    assert!(admissions[1].used(ResourceClass::Relay) <= 1);
                }
            },
        )
        .unwrap();
        let measured = (start.elapsed(), cpu() - started_cpu);
        assert_eq!(
            accepted.get(),
            1,
            "keepalive must reuse downstream connection"
        );
        pool.close();
        for r in &reactors {
            drive(&reactors, r.drain(), || ()).unwrap();
        }
        assert_eq!(admissions[1].used(ResourceClass::Connection), 0);
        assert_eq!(admissions[1].used(ResourceClass::Relay), 0);
        measured
    }
}
fn cpu() -> Duration {
    let mut time = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    // SAFETY: clock_gettime writes one correctly sized stack-local timespec.
    assert_eq!(
        unsafe { libc::clock_gettime(libc::CLOCK_THREAD_CPUTIME_ID, &mut time) },
        0
    );
    Duration::new(time.tv_sec as u64, time.tv_nsec as u32)
}
#[test]
fn signed_full_page_bootstrap_and_page_stream_without_transit_allocation() {
    for fallback in [false, true] {
        exchange(false, fallback, 2, true, false);
    }
}
#[test]
fn signed_materialized_full_page_bootstrap_and_page_keepalive() {
    exchange(true, false, 2, true, false);
}
#[test]
fn signed_success_truncation_closes_relay_and_pooled_destination() {
    exchange(false, false, 2, false, true);
}
#[test]
#[ignore = "local bounded before/after benchmark; run explicitly"]
fn opaque_relay_benchmark() {
    for materialized in [true, false, false, true] {
        let (wall, cpu) = exchange(materialized, false, 8, false, false);
        eprintln!(
            "materialized={materialized} bytes={} wall_ms={:.3} cpu_ms={:.3}",
            8 * (16 * 1024 * 1024 + 16),
            wall.as_secs_f64() * 1000.0,
            cpu.as_secs_f64() * 1000.0
        );
    }
}

#[test]
fn opaque_head_rejects_binding_length_authority_and_reverse_proof_substitution() {
    use crate::security::{forwarding::ForwardedHead, protocol};
    fn copy(head: &crate::http::MessageHead) -> crate::http::MessageHead {
        let codec = Codec::new(
            crate::peer::protocol::MAX_SIGNED_HEAD,
            crate::model::PAGE_BYTES + 16,
        );
        let mut head = codec
            .decode_head(&codec.encode_head(head).unwrap())
            .unwrap()
            .unwrap()
            .0;
        head.headers.retain(|h| {
            !crate::security::connection::is_auth_field(&h.name) || h.name == "racer-receiver"
        });
        head
    }
    for attack in [
        "valid",
        "binding",
        "length",
        "unknown",
        "path",
        "signature",
        "hop",
        "deadline",
    ] {
        let signers = signers();
        let a = Forwarding::new(signers[0].clone());
        let b = Forwarding::new(signers[1].clone());
        let c = Forwarding::new(signers[2].clone());
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let local = request(&admission, 1);
        let (signed, _) = a.sign_request_to(local, signers[1].node()).unwrap();
        let verified = b.verify_request(signed).unwrap();
        let binding = verified.binding().clone();
        let mut budget = verified.request().route.clone();
        budget.visited.push(signers[1].node().clone());
        budget.remaining_links -= 1;
        let outbound = b
            .append_request(verified, signers[2].node(), budget)
            .unwrap();
        let verified = c.verify_request(outbound).unwrap();
        let mut response = c
            .sign_response(verified.binding(), PeerResponse::Miss)
            .unwrap()
            .authentication;
        if attack == "signature" {
            response.original = Arc::new(signers[0].sign(copy(&response.original.head)).unwrap());
        } else if attack == "hop" {
            let head = c
                .sign_response(verified.binding(), PeerResponse::Miss)
                .unwrap()
                .authentication;
            response
                .hops
                .push(signers[1].sign(copy(&head.original.head)).unwrap());
        } else if matches!(attack, "binding" | "unknown" | "path") {
            let mut head = copy(&response.original.head);
            match attack {
                "binding" => {
                    head.headers
                        .iter_mut()
                        .find(|h| h.name == "racer-request-binding")
                        .unwrap()
                        .value = protocol::binary(&[0; 32]).into_bytes()
                }
                "path" => {
                    head.headers
                        .iter_mut()
                        .find(|h| h.name == "racer-response-path")
                        .unwrap()
                        .value =
                        protocol::nodes(&[signers[0].node().clone(), signers[2].node().clone()])
                            .unwrap()
                            .into_bytes()
                }
                _ => protocol::push(&mut head, "racer-unknown", "1"),
            }
            response = ForwardedHead {
                original: Arc::new(signers[2].sign(head).unwrap()),
                hops: vec![],
            };
        }
        if attack == "deadline" {
            // The binding retains its signed monotonic deadline; scope changes
            // cannot make a stale head eligible for reverse forwarding.
            let clock = crate::runtime::environment::SimulationClock::new_at(
                91,
                Instant::now() + Duration::from_secs(60),
                std::time::SystemTime::now() + Duration::from_secs(60),
            );
            let environment = clock.environment(0);
            let _guard = environment.enter();
            assert!(
                b.forward_opaque(response, 0, &binding, signers[0].node())
                    .is_err()
            );
        } else {
            let result = b.forward_opaque(
                response,
                usize::from(attack == "length"),
                &binding,
                signers[0].node(),
            );
            assert_eq!(result.is_ok(), attack == "valid", "{attack}");
        }
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }
}
