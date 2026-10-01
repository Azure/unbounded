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
        let conn = ConnectionLease::from_accepted(fd, &f.admissions[2])?;
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
        let conn = ConnectionLease::from_accepted(f.relay_socket.into(), &f.admissions[1])?;
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
        let conn = ConnectionLease::from_accepted(f.requester_socket.into(), &f.admissions[0])?;
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
        let (signed, _) =
            Forwarding::new(f.signers[0].clone()).sign_request_to(request, f.signers[1].node())?;
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
                    id: KeyId([7; 16]),
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
        let conn = ConnectionLease::from_accepted(fd, &f.admissions[2])?;
        let mut conn = connection::accept(&f.ios[2], conn, f.signers[2].clone(), &f.scope).await?;
        for wrong in [true, false] {
            let received = f.ios[2].receive_head(conn, &f.scope).await?;
            let (head, _) = decode_envelope(received.value, false)?;
            let auth = Forwarding::new(f.signers[2].clone());
            let request = auth.verify_request(codec(&f.admissions[2]).request(head, &f.scope)?)?;
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
        let mut conn = ConnectionLease::from_accepted(f.relay_socket.into(), &f.admissions[1])?;
        for _ in 0..2 {
            conn = f.server.serve_connection(conn, &f.scope).await?;
            assert!(conn.is_reusable());
        }
        Ok::<_, Error>(())
    };
    let client = async {
        let conn = ConnectionLease::from_accepted(f.requester_socket.into(), &f.admissions[0])?;
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
