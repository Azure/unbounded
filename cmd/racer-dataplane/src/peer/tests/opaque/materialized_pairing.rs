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
async fn prefix(
    io: &HttpIo,
    mut conn: ConnectionLease,
    response: protocol::SignedResponse,
    page: &CiphertextPage,
    scope: &RequestScope,
) -> Result<ConnectionLease> {
    let head = encode_envelope(&response.authentication, true, page.bytes().len())?;
    let head = conn.session.as_mut().unwrap().sign(head)?;
    let mut encoded = Codec::new(protocol::MAX_ENVELOPE_HEAD, crate::model::PAGE_BYTES + 16)
        .encode_head(&head)?;
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
                    BufferRange::new(buffer, offset..chunk.len())?,
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
    conn.tx_remaining = Some((page.bytes().len() - 173) as u64);
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
        cipher,
        mut metadata,
        ..
    } = RelayFixture::with_pool_limit(true, 2);
    assert!(!server.opaque_relay());
    metadata.length = 3 * crate::model::PAGE_BYTES;
    metadata.expires_at = ExpiresAt(
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
            let body = cipher
                .encrypt((&nonce.0).into(), plaintext.as_slice())
                .unwrap();
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
                cipher
                    .decrypt(
                        (&pages[left].envelope().nonce.0).into(),
                        pages[right].bytes()
                    )
                    .is_err()
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
        let conn = ConnectionLease::from_accepted(fd, &admissions[2])?;
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
        let conn = ConnectionLease::from_accepted(relay_socket.into(), &admissions[1])?;
        assert!(matches!(
            server.serve_connection(conn, &scope).await,
            Err(Error::Cancelled)
        ));
        canceled.set(true);
        Ok::<_, Error>(())
    };
    let healthy_relay = async {
        let mut conn = ConnectionLease::from_accepted(healthy_relay.into(), &admissions[1])?;
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
        let conn = ConnectionLease::from_accepted(requester_socket.into(), &admissions[0])?;
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
        let conn = ConnectionLease::from_accepted(healthy_socket.into(), &admissions[0])?;
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
                cipher
                    .decrypt((&ciphertext.envelope().nonce.0).into(), ciphertext.bytes())
                    .unwrap(),
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
