use super::*;
use crate::{
    http::{
        codec::Codec,
        io::HttpIo,
        pool::{ConnectionLease, Endpoint, HttpPool},
    },
    model::{
        envelope::PageEnvelope,
        metadata::{ExpiresAt, ObjectMetadata},
    },
    runtime::reactor::Reactor,
    telemetry::Telemetry,
};
use std::{
    cell::Cell,
    net::TcpListener,
    task::{Context, Poll},
};

#[test]
fn progressing_body_diagnostics_success_share_expiry_cancel_and_eof() {
    for case in ["success", "share", "cancel", "eof"] {
        let telemetry = Telemetry::default();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        admission.set_observer(telemetry.failures.observer(WorkerId(2)));
        let mut before_metrics = String::new();
        telemetry
            .metrics
            .write_prometheus(&mut before_metrics)
            .unwrap();
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let io = Rc::new(HttpIo::with_admission(
            reactor.clone(),
            Codec::new(
                wire::MAX_ENVELOPE_HEAD,
                crate::model::range::PAGE_BYTES + 16,
            ),
            admission.clone(),
        ));
        let pool = Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 1));
        let signers = signers();
        let transfers = transfer::Transfers::new(pool.clone(), io.clone(), None)
            .with_wire(admission.clone(), Rc::new(codec(&admission)));
        transfers.set_signatures(signers[0].clone());
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let start = Instant::now();
        let original = start + Duration::from_secs(3);
        let share = start + Duration::from_millis(250);
        let mut local = request(&admission, 9);
        local.route.deadline = Deadline(share);
        local.origin.scope.deadline = Deadline(share);
        local.operation = Operation::Page {
            page: PageId {
                version: ObjectVersion {
                    object: local.origin.object.clone(),
                    etag: StrongEtag::test_value("secret-etag"),
                },
                number: PageNumber(0),
            },
            mode: FetchMode::CopyOnly,
        };
        let mut scope = local.origin.scope().clone();
        scope.body_deadlines = Some((original, share));
        let auth = Forwarding::new(signers[0].clone());
        let (signed, binding) = auth.sign_request(local).unwrap();
        let server_scope = RequestScope::new(scope.request, original).unwrap();
        let sent = Cell::new(0usize);
        let server = async {
            let fd = reactor
                .accept(Rc::new(listener.into()), &server_scope)
                .await?;
            let conn = ConnectionLease::from_accepted(fd, &admission)?;
            let conn =
                crate::security::connection::accept(&io, conn, signers[2].clone(), &server_scope)
                    .await?;
            let received = io.receive_head(conn, &server_scope).await?;
            let (head, _) = WireCodec::decode(received.value, false)?;
            let remote_auth = Forwarding::new(signers[2].clone());
            let req =
                remote_auth.verify_request(codec(&admission).request(head, &server_scope)?)?;
            let Operation::Page { page, .. } = &req.request().operation else {
                panic!()
            };
            let length = 8192usize;
            let bytes = vec![0x9a; length + 16];
            let ciphertext = BufferPool::new(admission.clone()).ciphertext(
                admission.reserve(
                    Some(&page.version.object.cache),
                    ResourceClass::Ciphertext,
                    bytes.len(),
                )?,
                PageEnvelope {
                    page: page.clone(),
                    key_id: KeyId([1; 16]),
                    nonce: Nonce([2; 24]),
                    plaintext_length: length as u32,
                    ciphertext_length: bytes.len() as u32,
                },
                bytes,
            )?;
            let response = remote_auth.sign_response(
                req.binding(),
                PeerResponse::Page {
                    metadata: ObjectMetadata {
                        content_type: None,
                        version: page.version.clone(),
                        length: length as u64,
                        expires_at: ExpiresAt(
                            std::time::SystemTime::now() + Duration::from_secs(60),
                        ),
                    },
                    ciphertext: ciphertext.clone(),
                },
            )?;
            let mut conn = io
                .send_head(
                    received.connection,
                    WireCodec::encode(&response.authentication, true, length + 16)?,
                    &server_scope,
                )
                .await?
                .connection;
            let mut next = Instant::now();
            while sent.get() < length + 16 {
                if case != "success" {
                    futures::future::poll_fn(|_| {
                        if Instant::now() >= next {
                            Poll::Ready(())
                        } else {
                            Poll::Pending
                        }
                    })
                    .await;
                }
                let offset = sent.get();
                let end = (offset + 256).min(length + 16);
                let done = io
                    .write_body_range(conn, ciphertext.clone(), offset..end, &server_scope)
                    .await?;
                conn = done.lease;
                sent.set(end);
                if end == 4096 && case == "cancel" {
                    scope.cancel()?;
                }
                if end == 4096 && case == "eof" {
                    return Ok::<_, Error>(());
                }
                next = Instant::now() + Duration::from_millis(10);
            }
            Ok::<_, Error>(())
        };
        let mut server: crate::error::Operation<'_, ()> = Box::pin(server);
        let mut client = transfers.exchange(Endpoint::Peer(address.to_string()), signed, &scope);
        let result = loop {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            if let Poll::Ready(result) = client.as_mut().poll(&mut cx) {
                break result;
            }
            if let Poll::Ready(result) = server.as_mut().poll(&mut cx) {
                assert!(result.is_ok(), "{case}: server {result:?}");
                // Completed futures must not be polled again.
                server = Box::pin(futures::future::pending());
            }
            reactor.poll_budgeted(128).unwrap();
            assert!(Instant::now() < original, "bounded body fixture");
            std::thread::sleep(Duration::from_micros(100));
        };
        if case == "success" {
            let response = auth.verify_response(result.unwrap(), &binding).unwrap();
            assert!(
                matches!(response.response(), PeerResponse::Page { ciphertext, .. } if ciphertext.bytes().len() == 8208)
            );
        } else {
            assert!(
                matches!(result, Err(e) if e == match case { "share" => Error::DeadlineExceeded, "cancel" => Error::Cancelled, _ => Error::Io })
            );
        }
        drop(client);
        drop(server);
        let mut text = String::new();
        telemetry.failures.write(&mut text).unwrap();
        if case == "success" {
            assert_eq!(text, "total=0 retained=0 capacity=128\n");
        } else {
            assert!(
                text.starts_with("total=1 retained=1 capacity=128\n"),
                "{text}"
            );
            assert!(text.contains("stage=PeerReceiveBody"));
            assert!(text.contains("attempt=09090909090909090909090909090909"));
            assert!(text.contains(&format!("remote={C}")));
            assert!(text.contains(&format!(">{address}")));
            let field = |name: &str| {
                u64::from_str_radix(
                    text.split_whitespace()
                        .find_map(|s| s.strip_prefix(name))
                        .unwrap(),
                    if name == "n=" { 10 } else { 16 },
                )
                .unwrap()
            };
            let rx = text
                .split("rx=")
                .nth(1)
                .unwrap()
                .split('/')
                .next()
                .unwrap()
                .parse::<usize>()
                .unwrap();
            assert!(rx > 0 && rx < 8208, "{text}");
            assert!(field("n=") >= 3);
            assert!(field("f=") < field("l="));
            assert!(field("l=") <= field("now="));
            assert!(field("now=") < field("orig="));
            assert_eq!(field("share="), field("sig="));
            if case == "share" {
                assert!(field("now=") >= field("share="));
                assert!(field("now=") - field("l=") < 50, "{text}");
            }
            for secret in [
                "secret-etag",
                "authorization",
                "racer-signature",
                "ciphertext",
                "nonce",
            ] {
                assert!(!text.contains(secret));
            }
        }
        drop(transfers);
        drop(pool);
        let mut drain = reactor.drain();
        while drain
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending()
        {
            reactor.poll_budgeted(128).unwrap();
            assert!(Instant::now() < original);
        }
        drop(drain);
        drop(io);
        drop(binding);
        admission.reclaim_buffers();
        assert_eq!(reactor.in_flight(), 0);
        drop(reactor);
        for class in [
            ResourceClass::Connection,
            ResourceClass::Ciphertext,
            ResourceClass::RequestContext,
            ResourceClass::ControlProgress,
        ] {
            assert_eq!(admission.used(class), 0, "{case} {class:?}");
        }
        let mut after_metrics = String::new();
        telemetry
            .metrics
            .write_prometheus(&mut after_metrics)
            .unwrap();
        assert_eq!(before_metrics, after_metrics);
    }
}
