use super::*;
mod materialized_pairing {
    //! Distinct bodies make cross-exchange substitution visible across relay reuse.
    use super::*;
    use crate::{error::Result, memory::pool::CiphertextPage};
    use std::net::Shutdown;

    pub(super) async fn until(condition: impl Fn() -> bool) {
        std::future::poll_fn(|cx| {
            if condition() {
                Poll::Ready(())
            } else {
                cx.waker().wake_by_ref();
                Poll::Pending
            }
        })
        .await;
    }

    // Include a body prefix with the signed head, then use non-power-of-two body
    // fragments. Both the head read-ahead and ordinary body receive paths participate.
    pub(super) async fn prefix(
        io: &HttpIo,
        mut conn: ConnectionLease,
        response: protocol::SignedResponse,
        page: &CiphertextPage,
        scope: &RequestScope,
    ) -> Result<ConnectionLease> {
        let head = encode_envelope(&response.authentication, true, page.bytes().len())?;
        let head = conn.state_mut().session.as_mut().unwrap().sign(head)?;
        let mut encoded = Codec::new(protocol::MAX_ENVELOPE_HEAD).encode_head(&head)?;
        encoded.extend_from_slice(&page.bytes()[..173]);
        for chunk in encoded.chunks(997) {
            let mut buffer = io.buffer(chunk.len())?;
            buffer.bytes_mut()?.copy_from_slice(chunk);
            let mut offset = 0;
            while offset < chunk.len() {
                let done = io
                    .reactor()
                    .send(
                        conn.socket(),
                        BufferRange::new::<Error>(buffer, offset..chunk.len())?,
                        conn,
                        scope,
                    )
                    .await?;
                assert!(done.bytes > 0);
                offset += done.bytes;
                buffer = done.buffer.into_inner();
                conn = done.lease;
            }
        }
        conn.set_framing(
            conn.receive_remaining(),
            Some((page.bytes().len() - 173) as u64),
            false,
        );
        Ok(conn)
    }

    #[test]
    fn materialized_pairing_survives_concurrent_short_reads_cancel_and_pool_reuse() {
        let RelayFixture {
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
            mut metadata,
            ..
        } = RelayFixture::with_pool_limit(true, 2);
        assert!(!server.opaque_relay());
        metadata.length = 3 * crate::model::PAGE_BYTES;
        metadata.expires_at = ExpiresAt::test_time(
            std::time::UNIX_EPOCH
                + Duration::from_secs(
                    std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .unwrap()
                        .as_secs()
                        + 60,
                ),
        );
        let plaintexts: Vec<Vec<u8>> = (0..3)
            .map(|page| {
                (0..crate::model::PAGE_BYTES as usize)
                    .map(|offset| ((offset + page * 73) % 251) as u8)
                    .collect()
            })
            .collect();
        let pages: Vec<_> = plaintexts
            .iter()
            .enumerate()
            .map(|(number, plaintext)| {
                let nonce = Nonce([number as u8 + 21; 24]);
                let body = seal_fixture(&nonce.0, plaintext);
                BufferPool::new(admissions[2].clone())
                    .ciphertext(
                        admissions[2]
                            .reserve(
                                Some(&CacheId(CACHE.into())),
                                ResourceClass::Ciphertext,
                                body.len(),
                            )
                            .unwrap(),
                        PageEnvelope {
                            page: PageId {
                                version: metadata.version.clone(),
                                number: PageNumber(number as u64),
                            },
                            key_id: KeyId([1; 16]),
                            nonce,
                            plaintext_length: plaintext.len() as u32,
                            ciphertext_length: body.len() as u32,
                        },
                        body,
                    )
                    .unwrap()
            })
            .collect();
        for left in 0..3 {
            for right in 0..left {
                assert_eq!(pages[left].bytes().len(), pages[right].bytes().len());
                assert_ne!(pages[left].bytes(), pages[right].bytes());
                assert!(
                    open_fixture(&pages[left].envelope().nonce.0, pages[right].bytes()).is_err()
                );
            }
        }

        let ingress = TcpListener::bind("127.0.0.1:0").unwrap();
        let healthy_socket = TcpStream::connect(ingress.local_addr().unwrap()).unwrap();
        let (healthy_relay, _) = ingress.accept().unwrap();
        let accepted = Cell::new(0);
        let prefixes = Cell::new(0);
        let canceled = Cell::new(false);
        let downstream_closed = Cell::new(false);
        let healthy_first = Cell::new(false);
        let overlapped = Cell::new(false);
        let listener: Rc<crate::runtime::reactor::Descriptor> = Rc::new(listener.into());
        let destination = |listener: Rc<crate::runtime::reactor::Descriptor>| async {
            let fd = reactors[2].accept(listener, &scope).await?;
            accepted.set(accepted.get() + 1);
            let conn = crate::http::connection::from_accepted(fd, &admissions[2])?;
            let mut conn = connection::accept(&ios[2], conn, signers[2].clone(), &scope).await?;
            let auth = Forwarding::new(signers[2].clone());
            let mut previous = None;
            loop {
                let received = ios[2].receive_head(conn, &scope).await?;
                let (head, length) = decode_envelope(received.value, false)?;
                assert_eq!(length, 0);
                let request = auth.verify_request(codec(&admissions[2]).request(head, &scope)?)?;
                let protocol::Operation::Page { page, .. } = &request.request().operation else {
                    panic!("page request required")
                };
                let number = page.number.0 as usize;
                if let Some(previous) = previous {
                    assert_eq!(
                        (previous, number),
                        (1, 2),
                        "only the healthy socket may be reused"
                    );
                    assert!(canceled.get() && downstream_closed.get());
                } else {
                    assert!(number < 2, "third page must reuse an existing connection");
                }
                let response = auth.sign_response(
                    request.binding(),
                    PeerResponse::Page {
                        metadata: metadata.clone(),
                        ciphertext: pages[number].clone(),
                    },
                )?;
                conn = prefix(
                    &ios[2],
                    received.connection,
                    response,
                    &pages[number],
                    &scope,
                )
                .await?;
                if number < 2 {
                    prefixes.set(prefixes.get() + 1);
                    until(|| overlapped.get()).await;
                }
                if number == 0 {
                    // Never send the suffix. Upstream FIN must cancel the materialized
                    // receive and close this dirty pooled connection, not recycle it.
                    let done = reactors[2]
                        .recv(conn.socket(), ios[2].buffer(1)?, conn, &scope)
                        .await?;
                    assert_eq!(done.bytes, 0, "canceled downstream socket must close");
                    drop(done);
                    downstream_closed.set(true);
                    return Ok::<_, Error>(());
                }
                for start in (173..pages[number].bytes().len()).step_by(65521) {
                    conn = ios[2]
                        .write_body_range(
                            conn,
                            pages[number].clone(),
                            start..(start + 65521).min(pages[number].bytes().len()),
                            &scope,
                        )
                        .await?
                        .lease;
                }
                conn.finish_exchange()?;
                if number == 2 {
                    return Ok(());
                }
                previous = Some(number);
            }
        };
        let canceled_relay = async {
            let conn = crate::http::connection::from_accepted(relay_socket.into(), &admissions[1])?;
            assert!(matches!(
                server.serve_connection(conn, &scope).await,
                Err(Error::Cancelled)
            ));
            canceled.set(true);
            Ok::<_, Error>(())
        };
        let healthy_relay = async {
            let mut conn =
                crate::http::connection::from_accepted(healthy_relay.into(), &admissions[1])?;
            for _ in 0..2 {
                conn = server.serve_connection(conn, &scope).await?;
                assert!(conn.is_reusable());
            }
            Ok::<_, Error>(())
        };
        let signed_request = |number: usize| {
            let mut request = request(&admissions[0], number as u8 + 1);
            request.operation = protocol::Operation::Page {
                page: pages[number].envelope().page.clone(),
                mode: protocol::FetchMode::CopyOnly,
            };
            Forwarding::new(signers[0].clone()).sign_request_to(request, signers[1].node())
        };
        let canceled_client = async {
            let shutdown = requester_socket.try_clone().unwrap();
            let conn =
                crate::http::connection::from_accepted(requester_socket.into(), &admissions[0])?;
            let conn =
                connection::connect(&ios[0], conn, signers[0].clone(), signers[1].node(), &scope)
                    .await?;
            let (signed, _) = signed_request(0)?;
            let conn = ios[0]
                .send_head(
                    conn,
                    encode_envelope(&signed.authentication, false, 0)?,
                    &scope,
                )
                .await?
                .connection;
            until(|| healthy_first.get()).await;
            assert!(overlapped.get());
            shutdown.shutdown(Shutdown::Write).unwrap();
            let done = reactors[0]
                .recv(conn.socket(), ios[0].buffer(1)?, conn, &scope)
                .await?;
            assert_eq!(
                done.bytes, 0,
                "materialized relay must not publish a partial success head"
            );
            Ok::<_, Error>(())
        };
        let healthy_client = async {
            let conn =
                crate::http::connection::from_accepted(healthy_socket.into(), &admissions[0])?;
            let mut conn =
                connection::connect(&ios[0], conn, signers[0].clone(), signers[1].node(), &scope)
                    .await?;
            for number in [1, 2] {
                if number == 2 {
                    until(|| canceled.get() && downstream_closed.get()).await;
                }
                let (signed, binding) = signed_request(number)?;
                let received = ios[0]
                    .exchange_head(
                        conn,
                        encode_envelope(&signed.authentication, false, 0)?,
                        &scope,
                    )
                    .await?;
                let (head, length) = decode_envelope(received.value, true)?;
                assert_eq!(head.hops.len(), 1);
                assert_eq!(length, pages[number].bytes().len());
                conn = received.connection;
                let mut bytes = Vec::with_capacity(length);
                while bytes.len() < length {
                    let done = ios[0]
                        .read_body(conn, ios[0].buffer(32749)?, &scope)
                        .await?;
                    assert!(done.bytes > 0);
                    bytes.extend_from_slice(&done.buffer.bytes()?[..done.bytes]);
                    conn = done.lease;
                }
                let response = codec(&admissions[0]).response(head, bytes, &scope)?;
                let response =
                    Forwarding::new(signers[0].clone()).verify_response(response, &binding)?;
                let PeerResponse::Page {
                    ciphertext,
                    metadata: actual,
                } = response.response()
                else {
                    panic!("page response required")
                };
                assert_eq!(actual, &metadata);
                assert_eq!(ciphertext.envelope(), pages[number].envelope());
                assert_eq!(
                    ciphertext.bytes(),
                    pages[number].bytes(),
                    "body must match this signed envelope, not another equal-length page"
                );
                assert_eq!(
                    open_fixture(&ciphertext.envelope().nonce.0, ciphertext.bytes()).unwrap(),
                    plaintexts[number]
                );
                conn.finish_exchange()?;
                healthy_first.set(true);
            }
            Ok::<_, Error>(())
        };
        drive(
            &reactors,
            async {
                futures::try_join!(
                    destination(listener.clone()),
                    destination(listener.clone()),
                    canceled_relay,
                    healthy_relay,
                    canceled_client,
                    healthy_client
                )
            },
            || {
                if prefixes.get() == 2
                    && admissions[1].used(ResourceClass::Ciphertext) == 2 * pages[0].bytes().len()
                {
                    overlapped.set(true);
                }
            },
        )
        .unwrap();
        assert!(overlapped.get() && canceled.get() && downstream_closed.get());
        assert_eq!(
            accepted.get(),
            2,
            "third exchange must reuse the healthy downstream socket"
        );
        pool.close();
        for reactor in &reactors {
            drive(&reactors, reactor.drain(), || ()).unwrap();
        }
        for admission in &admissions {
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
        assert_eq!(admissions[1].used(ResourceClass::Relay), 0);
    }
}
mod safety {
    //! Opaque transit keeps deadline and endpoint authentication safety boundaries.
    use super::*;
    use crate::{
        control::wire::{CacheEncryptionKey, CacheKeyPurpose, CacheKeyRef, CacheKeyState},
        runtime::{
            crypto::{self, CryptoClient},
            worker::{CryptoRuntime, CryptoService},
        },
        security::aead::{PageCrypto, PageCryptoEngine},
        telemetry::metrics::{Event, Metrics},
    };
    use std::{cell::RefCell, net::Shutdown};

    #[test]
    fn upstream_fin_before_response_head_recovers_quota_by_signed_deadline() {
        let mut f = RelayFixture::new(false);
        let metrics = Metrics::default();
        f.server = f.server.with_metrics(metrics.clone());
        f.reactors[1].init().unwrap();
        let context_baseline = f.admissions[1].used(ResourceClass::RequestContext);
        assert!(f.server.opaque_relay());
        let admitted = Cell::new(false);
        let finished = Cell::new(false);
        let deadline = Cell::new(None);
        let shutdown = f.requester_socket.try_clone().unwrap();
        let destination = async {
            let fd = f.reactors[2]
                .accept(Rc::new(f.listener.into()), &f.scope)
                .await?;
            let conn = crate::http::connection::from_accepted(fd, &f.admissions[2])?;
            let conn = connection::accept(&f.ios[2], conn, f.signers[2].clone(), &f.scope).await?;
            let received = f.ios[2].receive_head(conn, &f.scope).await?;
            let (head, length) = decode_envelope(received.value, false)?;
            assert_eq!(length, 0);
            let request = Forwarding::new(f.signers[2].clone())
                .verify_request(codec(&f.admissions[2]).request(head, &f.scope)?)?;
            assert_eq!(
                request.request().route.visited,
                vec![NodeId(A.into()), NodeId(B.into())]
            );
            assert!(request.request().route.deadline.0 < f.scope.deadline.0);
            admitted.set(true);
            // Never send a response head. Only relay cleanup can cause this EOF.
            let done = f.reactors[2]
                .recv(
                    received.connection.socket(),
                    f.ios[2].buffer(1)?,
                    received.connection,
                    &f.scope,
                )
                .await?;
            assert_eq!(done.bytes, 0);
            Ok::<_, Error>(())
        };
        let relay = async {
            let conn =
                crate::http::connection::from_accepted(f.relay_socket.into(), &f.admissions[1])?;
            let result = f.server.serve_connection(conn, &f.scope).await;
            assert!(matches!(
                result,
                Err(Error::DeadlineExceeded | Error::Cancelled | Error::Io)
            ));
            assert!(Instant::now() <= deadline.get().unwrap() + Duration::from_secs(1));
            finished.set(true);
            Ok::<_, Error>(())
        };
        let client = async {
            let conn = crate::http::connection::from_accepted(
                f.requester_socket.into(),
                &f.admissions[0],
            )?;
            let conn = connection::connect(
                &f.ios[0],
                conn,
                f.signers[0].clone(),
                f.signers[1].node(),
                &f.scope,
            )
            .await?;
            let mut request = request(&f.admissions[0], 11);
            let end = Instant::now() + Duration::from_millis(500);
            deadline.set(Some(end));
            request.route.deadline = Deadline(end);
            request.origin.scope.deadline = Deadline(end);
            let (signed, _) = Forwarding::new(f.signers[0].clone())
                .sign_request_to(request, f.signers[1].node())?;
            let conn = f.ios[0]
                .send_head(
                    conn,
                    encode_envelope(&signed.authentication, false, 0)?,
                    &f.scope,
                )
                .await?
                .connection;
            materialized_pairing::until(|| admitted.get()).await;
            assert!(!finished.get());
            assert_eq!(f.admissions[1].used(ResourceClass::Relay), 1);
            assert_eq!(f.admissions[1].used(ResourceClass::Ciphertext), 0);
            assert_eq!(f.admissions[1].used(ResourceClass::Pipe), 0);
            shutdown.shutdown(Shutdown::Write).unwrap();
            let done = f.reactors[0]
                .recv(conn.socket(), f.ios[0].buffer(1)?, conn, &f.scope)
                .await?;
            assert_eq!(
                done.bytes, 0,
                "no success or error head after abandoned stalled request"
            );
            Ok::<_, Error>(())
        };
        drive(
            &f.reactors,
            async { futures::try_join!(destination, relay, client) },
            || {},
        )
        .unwrap();
        assert!(admitted.get() && finished.get());
        for event in [
            Event::OpaqueRelayBodyCompleted,
            Event::OpaqueRelayBodyBytes,
            Event::OpaqueRelayBodyFailed,
        ] {
            assert_eq!(
                metrics.count(event),
                0,
                "no body attempt before response head"
            );
        }
        f.pool.close();
        for reactor in &f.reactors {
            drive(&f.reactors, reactor.drain(), || {}).unwrap();
        }
        f.ios[1].reclaim_buffer();
        f.admissions[1].reclaim_buffers();
        for class in [
            ResourceClass::Connection,
            ResourceClass::Relay,
            ResourceClass::Ciphertext,
        ] {
            assert_eq!(f.admissions[1].used(class), 0, "{class:?}");
        }
        assert_eq!(
            f.admissions[1].used(ResourceClass::RequestContext),
            context_baseline
        );
        let recovered = f.admissions[1]
            .reserve(None, ResourceClass::Relay, 1)
            .unwrap();
        drop(recovered);
    }

    #[test]
    fn wrong_equal_length_body_through_opaque_relay_fails_aead_then_recovers() {
        let mut f = RelayFixture::new(false);
        let relay_metrics = Metrics::default();
        f.server = f.server.with_metrics(relay_metrics.clone());
        assert!(f.server.opaque_relay());
        let identities = crate::security::test_support::identities(
            ClusterId(CLUSTER.into()),
            &[NodeId(A.into()), NodeId(C.into())],
            || {
                let mut keys = crate::security::connection::signature_tests::mac_test_key(CACHE);
                keys.push(CacheEncryptionKey {
                    key: CacheKeyRef {
                        cache: CacheId(CACHE.into()),
                        id: KeyId::from_generation(1, 7).unwrap(),
                        purpose: CacheKeyPurpose::Page,
                    },
                    state: CacheKeyState::Active,
                    material: [19; 32],
                });
                keys
            },
        );
        let metrics = Metrics::default();
        let mut clients = Vec::new();
        let mut cryptos = Vec::new();
        let engines = RefCell::new(Vec::new());
        for (index, identity) in identities.iter().enumerate() {
            let (io, port) = crypto::pair(
                WorkerId(index as u16),
                1,
                std::num::NonZeroUsize::new(4).unwrap(),
            );
            let client = Rc::new(CryptoClient::new(io));
            if index == 0 {
                client.set_metrics(metrics.clone());
            }
            cryptos.push(PageCrypto::new(identity.keys.clone(), client.clone()));
            clients.push(client);
            engines
                .borrow_mut()
                .push(PageCryptoEngine::new(CryptoRuntime { port }));
        }
        let poll_crypto = || {
            for engine in engines.borrow_mut().iter_mut() {
                engine.poll_budgeted(8).unwrap();
            }
            for client in &clients {
                client.poll_budgeted(8).unwrap();
            }
        };
        let length = 1 << 20;
        f.metadata.length = length as u64;
        let mut pages = Vec::new();
        for index in 0..2u8 {
            let mut page = f.page.envelope().page.clone();
            page.version.object.key = CacheKey([3 + index; 32]);
            let cache = &page.version.object.cache;
            let mut plain = BufferPool::new(f.admissions[2].clone())
                .plaintext(
                    f.admissions[2]
                        .reserve(Some(cache), ResourceClass::Plaintext, length)
                        .unwrap(),
                    length,
                )
                .unwrap();
            plain.bytes_mut().unwrap().fill(31 + index);
            let output = f.admissions[2]
                .reserve(Some(cache), ResourceClass::Ciphertext, length + 16)
                .unwrap();
            let (verified, encrypted) = drive(
                &f.reactors,
                cryptos[1].encrypt(page, plain, output, &f.scope),
                &poll_crypto,
            )
            .unwrap();
            assert!(verified.bytes().iter().all(|b| *b == 31 + index));
            pages.push(encrypted);
        }
        assert_eq!(pages[0].bytes().len(), pages[1].bytes().len());
        assert_ne!(pages[0].bytes(), pages[1].bytes());
        let destination = async {
            let fd = f.reactors[2]
                .accept(Rc::new(f.listener.into()), &f.scope)
                .await?;
            let conn = crate::http::connection::from_accepted(fd, &f.admissions[2])?;
            let mut conn =
                connection::accept(&f.ios[2], conn, f.signers[2].clone(), &f.scope).await?;
            for wrong in [true, false] {
                let received = f.ios[2].receive_head(conn, &f.scope).await?;
                let (head, _) = decode_envelope(received.value, false)?;
                let auth = Forwarding::new(f.signers[2].clone());
                let request =
                    auth.verify_request(codec(&f.admissions[2]).request(head, &f.scope)?)?;
                assert_eq!(
                    request.request().route.visited,
                    vec![NodeId(A.into()), NodeId(B.into())]
                );
                let response = auth.sign_response(
                    request.binding(),
                    PeerResponse::Page {
                        metadata: f.metadata.clone(),
                        ciphertext: pages[0].clone(),
                    },
                )?;
                conn = f.ios[2]
                    .send_head(
                        received.connection,
                        encode_envelope(&response.authentication, true, length + 16)?,
                        &f.scope,
                    )
                    .await?
                    .connection;
                conn = f.ios[2]
                    .write_body(conn, pages[usize::from(wrong)].clone(), &f.scope)
                    .await?
                    .lease;
                conn.finish_exchange()?;
            }
            Ok::<_, Error>(())
        };
        let relay = async {
            let mut conn =
                crate::http::connection::from_accepted(f.relay_socket.into(), &f.admissions[1])?;
            for _ in 0..2 {
                conn = f.server.serve_connection(conn, &f.scope).await?;
                assert!(conn.is_reusable());
            }
            Ok::<_, Error>(())
        };
        let client = async {
            let conn = crate::http::connection::from_accepted(
                f.requester_socket.into(),
                &f.admissions[0],
            )?;
            let mut conn = connection::connect(
                &f.ios[0],
                conn,
                f.signers[0].clone(),
                f.signers[1].node(),
                &f.scope,
            )
            .await?;
            for (attempt, wrong) in [true, false].into_iter().enumerate() {
                let mut request = request(&f.admissions[0], attempt as u8 + 1);
                request.operation = protocol::Operation::Page {
                    page: pages[0].envelope().page.clone(),
                    mode: protocol::FetchMode::CopyOnly,
                };
                let auth = Forwarding::new(f.signers[0].clone());
                let (signed, binding) = auth.sign_request_to(request, f.signers[1].node())?;
                let received = f.ios[0]
                    .exchange_head(
                        conn,
                        encode_envelope(&signed.authentication, false, 0)?,
                        &f.scope,
                    )
                    .await?;
                let (head, size) = decode_envelope(received.value, true)?;
                assert_eq!(head.hops.len(), 1);
                assert_eq!(size, length + 16);
                conn = received.connection;
                let mut bytes = Vec::new();
                while bytes.len() < size {
                    let done = f.ios[0]
                        .read_body(conn, f.ios[0].buffer(32749)?, &f.scope)
                        .await?;
                    assert!(done.bytes > 0);
                    bytes.extend_from_slice(&done.buffer.bytes()?[..done.bytes]);
                    conn = done.lease;
                }
                assert_eq!(bytes, pages[usize::from(wrong)].bytes());
                let response = auth.verify_response(
                    codec(&f.admissions[0]).response(head, bytes, &f.scope)?,
                    &binding,
                )?;
                let PeerResponse::Page { ciphertext, .. } = response.response() else {
                    panic!("page required")
                };
                assert_eq!(ciphertext.envelope(), pages[0].envelope());
                let clear = cryptos[0]
                    .decrypt(
                        ciphertext.clone(),
                        f.admissions[0].reserve(
                            Some(&CacheId(CACHE.into())),
                            ResourceClass::Plaintext,
                            length,
                        )?,
                        &f.scope,
                    )
                    .await;
                if wrong {
                    assert!(matches!(clear, Err(Error::CorruptRecord)));
                    assert_eq!(
                        metrics.count(Event::CryptoDecryptSuccess),
                        0,
                        "no publishable plaintext"
                    );
                    assert_eq!(metrics.count(Event::CryptoDecryptAeadRejected), 1);
                    assert_eq!(metrics.count(Event::CryptoDecryptCrcRejected), 0);
                } else {
                    let clear = clear?;
                    assert!(clear.bytes().iter().all(|b| *b == 31));
                    assert_eq!(metrics.count(Event::CryptoDecryptSuccess), 1);
                }
                conn.finish_exchange()?;
            }
            Ok::<_, Error>(())
        };
        drive(
            &f.reactors,
            async { futures::try_join!(destination, relay, client) },
            || {
                poll_crypto();
                assert_eq!(f.admissions[1].used(ResourceClass::Ciphertext), 0);
                assert_eq!(f.admissions[1].used(ResourceClass::Plaintext), 0);
            },
        )
        .unwrap();
        f.pool.close();
        for reactor in &f.reactors {
            drive(&f.reactors, reactor.drain(), &poll_crypto).unwrap();
        }
        assert!(clients.iter().all(|client| client.outstanding() == 0));
        assert_eq!(relay_metrics.count(Event::OpaqueRelayBodyCompleted), 2);
        assert_eq!(
            relay_metrics.count(Event::OpaqueRelayBodyBytes),
            2 * (length + 16) as u64
        );
        assert_eq!(relay_metrics.count(Event::OpaqueRelayBodyFailed), 0);
        f.admissions[0].reclaim_buffers();
        assert_eq!(f.admissions[1].used(ResourceClass::Connection), 0);
        assert_eq!(f.admissions[1].used(ResourceClass::Relay), 0);
        assert_eq!(f.admissions[0].used(ResourceClass::Plaintext), 0);
    }
}
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
use racer_crypto::aead;
use std::{
    cell::Cell,
    net::{TcpListener, TcpStream},
    task::{Context, Poll},
};

// These opaque-transport fixtures intentionally use empty AAD, not page AAD.
fn seal_fixture(nonce: &[u8; 24], plaintext: &[u8]) -> Vec<u8> {
    let mut output = vec![0; plaintext.len() + aead::TAG_LEN];
    aead::seal(&[7; 32], nonce, &[], plaintext, &mut output).unwrap();
    output
}

fn open_fixture(
    nonce: &[u8; 24],
    sealed: &[u8],
) -> std::result::Result<Vec<u8>, racer_crypto::Error> {
    let mut output = vec![0; sealed.len().saturating_sub(aead::TAG_LEN)];
    aead::open(&[7; 32], nonce, &[], sealed, &mut output)?;
    Ok(output)
}

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
    body: Vec<u8>,
    metadata: ObjectMetadata,
    page: crate::memory::pool::CiphertextPage,
}

#[test]
fn send_crc_http_success_failure_drop_do_not_wait_for_crypto() {
    use crate::{
        runtime::crypto::{CryptoClient, pair},
        security::aead::PageCrypto,
        telemetry::send_crc::{Pair, Samples},
    };
    for mode in ["success", "failure", "drop"] {
        let f = RelayFixture::new(true);
        let queue = Rc::new(crate::read::drivers::DriverQueue::default());
        let _guard = queue.enter();
        let (_, ids) = identities();
        let (io, _engine) = pair(
            crate::model::WorkerId(0),
            1,
            std::num::NonZeroUsize::new(1).unwrap(),
        );
        let crypto = Rc::new(PageCrypto::new(
            ids[1].0.clone(),
            Rc::new(CryptoClient::new(io)),
        ));
        let samples = Samples::default();
        let server = f.server.with_send_crc(
            Some(Pair {
                sender: NodeId(B.into()),
                receiver: NodeId(A.into()),
            }),
            samples.clone(),
            crypto,
        );
        let client_conn =
            crate::http::connection::from_accepted(f.requester_socket.into(), &f.admissions[0])
                .unwrap();
        let server_conn =
            crate::http::connection::from_accepted(f.relay_socket.into(), &f.admissions[1])
                .unwrap();
        let (client_conn, mut server_conn) = drive(
            &f.reactors,
            async {
                futures::try_join!(
                    connection::connect(
                        &f.ios[0],
                        client_conn,
                        f.signers[0].clone(),
                        f.signers[1].node(),
                        &f.scope
                    ),
                    connection::accept(&f.ios[1], server_conn, f.signers[1].clone(), &f.scope)
                )
            },
            || {},
        )
        .unwrap();
        // This test invokes the response sender directly after the handshake,
        // bypassing receive_head's completed bodyless-request framing.
        server_conn.set_framing(Some(0), server_conn.send_remaining(), false);
        let a = Forwarding::new(f.signers[0].clone());
        let b = Forwarding::new(f.signers[1].clone());
        let mut local = request(&f.admissions[0], 9);
        local.route.destination = NodeId(B.into());
        local.operation = protocol::Operation::Bootstrap {
            object: f.metadata.version.object.clone(),
            mode: protocol::FetchMode::CopyOnly,
        };
        let (signed, _) = a.sign_request_to(local, f.signers[1].node()).unwrap();
        let admitted = b.verify_request(signed).unwrap();
        let mut page = f.page.clone();
        page.provenance = Some(crate::telemetry::failures::PeerProvenance {
            request: f.scope.request,
            attempt: crate::model::AttemptId([9; 16]),
            supplier: C.as_bytes().try_into().unwrap(),
            remote: C.as_bytes().try_into().unwrap(),
        });
        let response = b
            .sign_response(
                admitted.binding(),
                PeerResponse::Bootstrap {
                    metadata: f.metadata.clone(),
                    page_zero: Some(page),
                },
            )
            .unwrap();
        if mode == "failure" {
            f.scope.cancel().unwrap();
            assert!(
                drive(
                    &f.reactors,
                    server.send_http_response(server_conn, &response, &f.scope),
                    || {}
                )
                .is_err()
            );
            drop(client_conn);
        } else if mode == "drop" {
            let mut send = Box::pin(server.send_http_response(server_conn, &response, &f.scope));
            assert!(
                send.as_mut()
                    .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                    .is_pending()
            );
            drop(send);
            drop(client_conn);
        } else {
            let receive = async {
                let received = f.ios[0].receive_head(client_conn, &f.scope).await?;
                let mut conn = received.connection;
                let mut count = 0;
                while count < f.body.len() {
                    let done = f.ios[0]
                        .read_body(conn, f.ios[0].buffer(65536)?, &f.scope)
                        .await?;
                    count += done.bytes;
                    conn = done.lease;
                }
                Ok::<_, Error>(conn)
            };
            drive(
                &f.reactors,
                async {
                    futures::try_join!(
                        server.send_http_response(server_conn, &response, &f.scope),
                        receive
                    )
                },
                || {},
            )
            .unwrap();
        }
        // Deliberately never drive crypto or the diagnostic queue during send.
        let mut text = String::new();
        samples.write(&mut text).unwrap();
        assert!(
            text.contains(match mode {
                "success" => "send=completed",
                "failure" => "send=failed",
                _ => "send=abandoned",
            }),
            "{text}"
        );
        assert!(text.contains("status=pending"));
        drop(queue);
        drop(_guard);
    }
}

impl RelayFixture {
    fn new(materialized: bool) -> Self {
        Self::with_pool_limit(materialized, 1)
    }

    fn with_pool_limit(materialized: bool, limit: usize) -> Self {
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
                    Codec::new(protocol::MAX_ENVELOPE_HEAD),
                    a.clone(),
                    crate::model::PAGE_BYTES + 16,
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
        let pool = Rc::new(HttpPool::new(
            reactors[1].clone(),
            admissions[1].clone(),
            limit,
        ));
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
        let body = seal_fixture(&[8; 24], &plaintext);
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
            expires_at: ExpiresAt::test_time(
                std::time::SystemTime::now() + Duration::from_secs(60),
            ),
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
            body,
            metadata,
            page,
        } = self;
        use crate::telemetry::metrics::{Event, Metrics};
        let metrics = Metrics::default();
        let server = server.with_metrics(metrics.clone());
        let accepted = Cell::new(0);
        let destination = async {
            let fd = reactors[2].accept(Rc::new(listener.into()), &scope).await?;
            accepted.set(accepted.get() + 1);
            let conn = crate::http::connection::from_accepted(fd, &admissions[2])?;
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
                    conn = materialized_pairing::prefix(&ios[2], conn, response, &page, &scope)
                        .await?;
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
            let mut conn =
                crate::http::connection::from_accepted(relay_socket.into(), &admissions[1])?;
            conn.state_mut().relay_fallback = fallback;
            for i in 0..rounds {
                let result = server.serve_connection(conn, &scope).await;
                if truncated && i + 1 == rounds {
                    assert!(matches!(result, Err(Error::Io)));
                    assert_eq!(metrics.count(Event::OpaqueRelayBodyFailed), 1);
                    assert_eq!(metrics.count(Event::OpaqueRelayBodyCompleted), i as u64);
                    assert_eq!(
                        metrics.count(Event::OpaqueRelayBodyBytes),
                        (i * body.len()) as u64
                    );
                    return Ok::<_, Error>(());
                }
                conn = result?;
                assert!(conn.is_reusable());
                let completed = if materialized { 0 } else { i + 1 };
                assert_eq!(
                    metrics.count(Event::OpaqueRelayBodyCompleted),
                    completed as u64
                );
                assert_eq!(
                    metrics.count(Event::OpaqueRelayBodyBytes),
                    (completed * body.len()) as u64
                );
                assert_eq!(metrics.count(Event::OpaqueRelayBodyFailed), 0);
            }
            Ok::<_, Error>(())
        };
        let client = async {
            let conn =
                crate::http::connection::from_accepted(requester_socket.into(), &admissions[0])?;
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
                    open_fixture(&[8; 24], ciphertext.bytes()).unwrap(),
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
fn opaque_truncation_without_prior_success_credits_no_complete_bytes() {
    for fallback in [false, true] {
        exchange(false, fallback, 1, false, true);
    }
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
        let codec = Codec::new(crate::peer::protocol::MAX_SIGNED_HEAD);
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
