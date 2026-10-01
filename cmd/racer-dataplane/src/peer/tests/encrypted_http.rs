//! Production page AEAD across independent keyrings and the signed HTTP transport.
use super::*;
use crate::{
    control::wire::{CacheEncryptionKey, CacheKeyPurpose, CacheKeyRef, CacheKeyState},
    http::{
        Codec,
        connection::{ConnectionLease, Endpoint, HttpIo, HttpPool},
    },
    memory::pool::CiphertextPage,
    runtime::{
        crypto::{self, CryptoClient},
        reactor::{IoBuffer, Reactor},
        worker::{CryptoRuntime, CryptoService},
    },
    security::aead::{PageCrypto, PageCryptoEngine},
    telemetry::metrics::{Event, Metrics},
    topology::rails::TransportPlan,
};
use std::{
    cell::Cell,
    future::Future,
    net::TcpListener,
    task::{Context, Poll},
    time::SystemTime,
};

fn drive<T>(
    future: impl Future<Output = T>,
    reactor: &Reactor,
    engines: &mut [PageCryptoEngine],
    clients: &[Rc<CryptoClient>],
) -> T {
    let mut future = std::pin::pin!(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let until = Instant::now() + Duration::from_secs(20);
    loop {
        if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
            return result;
        }
        assert!(Instant::now() < until, "encrypted HTTP regression watchdog");
        for engine in engines.iter_mut() {
            engine.poll_budgeted(8).unwrap();
        }
        for client in clients {
            client.poll_budgeted(8).unwrap();
        }
        reactor.poll_budgeted(128).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

#[test]
fn production_aad_http_roundtrip_cancel_reuse_and_mismatched_body() {
    // Each identity owns a different KeyEpochs store, populated from equal bundles.
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
    assert!(!Rc::ptr_eq(&identities[0].keys, &identities[1].keys));
    let admissions: Vec<_> = (0..2)
        .map(|_| {
            Rc::new(Admission::new(
                crate::test_support::cluster::config(false).limits,
            ))
        })
        .collect();
    let reactor = Rc::new(Reactor::new(admissions[0].clone()));
    let mut engines = Vec::new();
    let mut clients = Vec::new();
    let mut cryptos = Vec::new();
    let metrics = Metrics::default();
    for (i, identity) in identities.iter().enumerate() {
        let (io, port) = crypto::pair(
            WorkerId(i as u16),
            1,
            std::num::NonZeroUsize::new(4).unwrap(),
        );
        let client = Rc::new(CryptoClient::new(io));
        if i == 0 {
            client.set_metrics(metrics.clone());
        }
        cryptos.push(PageCrypto::new(identity.keys.clone(), client.clone()));
        clients.push(client);
        engines.push(PageCryptoEngine::new(CryptoRuntime { port }));
    }
    let ios: Vec<_> = admissions
        .iter()
        .map(|admission| {
            Rc::new(HttpIo::with_admission(
                reactor.clone(),
                Codec::new(protocol::MAX_ENVELOPE_HEAD, PAGE_BYTES + 16),
                admission.clone(),
            ))
        })
        .collect();
    let pool = Rc::new(HttpPool::new(reactor.clone(), admissions[0].clone(), 1));
    let transfers = transport::Transfers::new(
        pool.clone(),
        ios[0].clone(),
        None,
        admissions[0].clone(),
        Rc::new(codec(&admissions[0])),
        identities[0].signatures.clone(),
    );
    let scope =
        RequestScope::new(RequestId([8; 16]), Instant::now() + Duration::from_secs(20)).unwrap();
    let length = 1 << 20; // Enter the production recycled-payload pool.
    let mut pages: Vec<CiphertextPage> = Vec::new();
    let mut expected = Vec::new();
    for i in 0..2u8 {
        let page = PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(CACHE.into()),
                    key: CacheKey([3 + i; 32]),
                },
                etag: StrongEtag::parse(if i == 0 {
                    b"\"a,\\literal\""
                } else {
                    b"\"b,\\literal\""
                })
                .unwrap(),
            },
            number: PageNumber(0),
        };
        let bytes: Vec<_> = (0..length)
            .map(|offset| ((offset + usize::from(i) * 31) % 251) as u8)
            .collect();
        let cache = &page.version.object.cache;
        let mut plaintext = BufferPool::new(admissions[1].clone())
            .plaintext(
                admissions[1]
                    .reserve(Some(cache), ResourceClass::Plaintext, length)
                    .unwrap(),
                length,
            )
            .unwrap();
        plaintext.bytes_mut().unwrap().copy_from_slice(&bytes);
        let output = admissions[1]
            .reserve(Some(cache), ResourceClass::Ciphertext, length + 16)
            .unwrap();
        let (verified, ciphertext) = drive(
            cryptos[1].encrypt(page, plaintext, output, &scope),
            &reactor,
            &mut engines,
            &clients,
        )
        .unwrap();
        assert_eq!(verified.bytes(), bytes);
        drop(verified);
        pages.push(ciphertext);
        expected.push(bytes);
    }
    assert_ne!(pages[0].envelope().nonce, pages[1].envelope().nonce);
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
    let listener = Rc::new(crate::runtime::reactor::Descriptor::from(listener));
    let canceled = RequestScope::new(RequestId([9; 16]), scope.deadline.0).unwrap();
    let accepts = Cell::new(0);
    let prefix_sent = Cell::new(false);
    let server = async {
        let auth = Forwarding::new(identities[1].signatures.clone());
        let mut connection = None;
        for attempt in 0..5 {
            let conn = match connection.take() {
                Some(conn) => conn,
                None => {
                    accepts.set(accepts.get() + 1);
                    let fd = reactor.accept(listener.clone(), &scope).await?;
                    let conn = ConnectionLease::from_accepted(fd, &admissions[1])?;
                    crate::security::connection::accept(
                        &ios[1],
                        conn,
                        identities[1].signatures.clone(),
                        &scope,
                    )
                    .await?
                }
            };
            let received = ios[1].receive_head(conn, &scope).await?;
            let (head, _) = decode_envelope(received.value, false)?;
            let request = auth.verify_request(codec(&admissions[1]).request(head, &scope)?)?;
            let index = usize::from(attempt == 2 || attempt == 4);
            let ciphertext = &pages[index];
            let response = auth.sign_response(
                request.binding(),
                PeerResponse::Page {
                    metadata: ObjectMetadata {
                        content_type: None,
                        version: ciphertext.envelope().page.version.clone(),
                        length: length as u64,
                        expires_at: ExpiresAt::test_time(
                            SystemTime::now() + Duration::from_secs(60),
                        ),
                    },
                    ciphertext: ciphertext.clone(),
                },
            )?;
            let mut conn = ios[1]
                .send_head(
                    received.connection,
                    encode_envelope(&response.authentication, true, length + 16)?,
                    &scope,
                )
                .await?
                .connection;
            // Negative control: preserve A's valid signed descriptor, send B's
            // independently valid equal-length body. Neither body is bit-flipped.
            let body = if attempt == 3 { &pages[1] } else { ciphertext };
            for offset in (0..length + 16).step_by(16381) {
                let end = (offset + 16381).min(length + 16);
                conn = ios[1]
                    .write_body_range(conn, body.clone(), offset..end, &scope)
                    .await?
                    .lease;
                if attempt == 0 {
                    prefix_sent.set(true);
                    // Do not let cancellation race ahead of receive admission:
                    // the partial exchange must own a full ciphertext allocation.
                    futures::future::poll_fn(|_| {
                        if admissions[0].used(ResourceClass::Ciphertext) == length + 16 {
                            Poll::Ready(())
                        } else {
                            Poll::Pending
                        }
                    })
                    .await;
                    canceled.cancel()?;
                    break;
                }
            }
            if attempt != 0 {
                conn.finish_exchange()?;
                connection = Some(conn);
            } // A canceled partial exchange must close, never return to the pool.
        }
        Ok::<(), Error>(())
    };
    let client = async {
        let auth = Forwarding::new(identities[0].signatures.clone());
        let mut reused = None;
        for attempt in 0..5u8 {
            let index = usize::from(attempt == 2 || attempt == 4);
            let page = &pages[index];
            let mut request = request(&admissions[0], attempt);
            request.origin.authorization = None;
            request.origin.object = page.envelope().page.version.object.clone();
            request.operation = Operation::Page {
                page: page.envelope().page.clone(),
                mode: FetchMode::CopyOnly,
            };
            let turn = if attempt == 0 { &canceled } else { &scope };
            request.route.request = turn.request;
            request.route.deadline = turn.deadline;
            request.origin.request = turn.request;
            request.origin.scope = turn.clone();
            let (signed, binding) = auth.sign_request(request)?;
            let response = transfers
                .exchange_timed(
                    endpoint.clone(),
                    signed,
                    TransportPlan::Http,
                    None,
                    None,
                    None,
                    Rc::new(Cell::new(false)),
                    None,
                    turn,
                )
                .await;
            if attempt == 0 {
                assert!(prefix_sent.get());
                assert!(matches!(response, Err(Error::Cancelled)));
                assert_eq!(metrics.count(Event::CryptoDecryptStarted), 0);
                continue;
            }
            let transport::RelayResponse::Complete(response) = response? else {
                panic!("HTTP requester must materialize ciphertext")
            };
            let response = auth.verify_response(response, &binding)?;
            let PeerResponse::Page { ciphertext, .. } = response.response() else {
                panic!("page required")
            };
            assert_eq!(ciphertext.envelope(), page.envelope());
            assert_eq!(
                ciphertext.bytes(),
                pages[if attempt == 3 { 1 } else { index }].bytes()
            );
            let pointer = ciphertext.bytes().as_ptr();
            if let Some(previous) = reused {
                assert_eq!(
                    pointer, previous,
                    "receive allocation must actually be reused"
                );
            }
            reused = Some(pointer);
            // A receiver-computed CRC succeeds even for the wrong valid body.
            assert_eq!(ciphertext.verify_checksum(), Ok(()));
            let successes = metrics.count(Event::CryptoDecryptSuccess);
            let clear = cryptos[0]
                .decrypt(
                    ciphertext.clone(),
                    admissions[0].reserve(
                        Some(&CacheId(CACHE.into())),
                        ResourceClass::Plaintext,
                        length,
                    )?,
                    turn,
                )
                .await;
            if attempt == 3 {
                assert!(matches!(clear, Err(Error::CorruptRecord)));
                assert_eq!(
                    metrics.count(Event::CryptoDecryptSuccess),
                    successes,
                    "mismatch cannot produce publishable plaintext"
                );
                assert_eq!(metrics.count(Event::CryptoDecryptAeadRejected), 1);
                assert_eq!(metrics.count(Event::CryptoDecryptCrcRejected), 0);
            } else {
                let clear = clear?;
                assert_eq!(clear.page(), &page.envelope().page);
                assert_eq!(clear.bytes(), expected[index]);
                drop(clear);
            }
            drop(response);
        }
        Ok::<(), Error>(())
    };
    drive(
        async { futures::try_join!(server, client) },
        &reactor,
        &mut engines,
        &clients,
    )
    .unwrap();
    assert_eq!(
        accepts.get(),
        2,
        "cancel closes the first socket; later exchanges reuse one socket"
    );
    assert_eq!(metrics.count(Event::CryptoDecryptSuccess), 3);
    assert_eq!(metrics.count(Event::CryptoDecryptAeadRejected), 1);
    assert_eq!(metrics.count(Event::CryptoDecryptCrcRejected), 0);
    assert!(clients.iter().all(|client| client.outstanding() == 0));
    drive(reactor.drain(), &reactor, &mut engines, &clients).unwrap();
    assert_eq!(reactor.in_flight(), 0);
}
