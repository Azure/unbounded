//! Signed native offer/fallback exchanges over real sockets and optional hardware.
use super::*;
use crate::{
    http::{
        Codec, MessageHead, StartLine,
        connection::{HttpIo, HttpPool},
    },
    memory::pool::BufferPool,
    model::{ExpiresAt, KeyId, Nonce, ObjectMetadata, PageEnvelope, ResourceClass, *},
    rdma::{Devices, RdmaTransfer, Sessions},
    runtime::{admission::Admission, reactor::Reactor},
    security::{protocol as p, signing::Signatures},
};
use std::{
    os::unix::net::UnixStream,
    rc::Rc,
    sync::Arc,
    task::{Context, Poll},
    time::{Duration, Instant},
};

fn transfers(
    signatures: Rc<Signatures>,
    admission: &Rc<Admission>,
    reactor: &Rc<Reactor>,
) -> Transfers {
    let devices = Rc::new(Devices::new());
    let sessions = Rc::new(Sessions::new(devices.clone(), 2));
    let rdma = Rc::new(RdmaTransfer::new(sessions.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(
            super::super::protocol::MAX_ENVELOPE_HEAD,
            crate::model::PAGE_BYTES + 16,
        ),
        admission.clone(),
    ));
    Transfers::new(
        Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 2)),
        io,
        Some(rdma),
        admission.clone(),
        Rc::new(super::super::protocol::SecurityCodec::new(
            admission.clone(),
            Rc::new(BufferPool::new(admission.clone())),
        )),
        signatures.clone(),
    )
    .with_native(sessions)
}

fn test_membership(
    signers: &[Rc<Signatures>],
    nodes: &[usize],
    sites: &[&str],
) -> crate::topology::membership::MembershipLease {
    use crate::topology::{
        membership::{Member, Membership},
        rails::{RailId, RailMapping},
    };
    Arc::new(
        Membership::validate(
            MembershipVersion(1),
            nodes
                .iter()
                .zip(sites)
                .map(|(&i, site)| Member {
                    node: signers[i].node().clone(),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: format!("127.0.0.1:{}", 9000 + i),
                    rails: vec![RailMapping {
                        rail: RailId(7),
                        fabric: "test-provider".into(),
                        numa_node: None,
                    }],
                    alignment_enabled: true,
                    site: (*site).into(),
                })
                .collect(),
        )
        .unwrap(),
    )
}
#[test]
fn real_socket_signed_offer_falls_back_when_local_provider_is_unavailable() {
    offer_fallback(false, None, false);
}
#[test]
fn real_socket_sender_failure_requires_signed_fallback_before_ciphertext() {
    offer_fallback(true, None, false);
}
#[test]
fn signed_offer_rejects_cross_site_and_missing_site_before_session_preparation() {
    for sites in [["site1", "site2"], ["", "site1"], ["site1", ""], ["", ""]] {
        offer_fallback(false, Some(sites), false);
    }
}
#[test]
fn signed_offer_cannot_substitute_the_actual_local_identity_in_response_path() {
    offer_fallback(false, None, true);
}
#[test]
fn native_subdeadline_preserves_parent_deadline_and_cancellation() {
    let scope =
        RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(30)).unwrap();
    let native = native_scope(&scope);
    assert!(native.deadline.0 <= scope.deadline.0);
    assert!(native_failure(Error::DeadlineExceeded, &scope));
    assert!(!native_failure(Error::Unauthorized, &scope));
    scope.cancel().unwrap();
    assert_eq!(native.check(), Err(Error::Cancelled));
    assert!(!native_failure(Error::DeadlineExceeded, &scope));
}
fn offer_fallback(sender_failure: bool, rejected_sites: Option<[&str; 2]>, wrong_path: bool) {
    let rejected = rejected_sites.is_some() || wrong_path;
    let signers = super::super::tests::signers();
    let membership = test_membership(
        &signers,
        &[0, 2],
        &rejected_sites.unwrap_or(["site1", "site1"]),
    );
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let receiver = transfers(signers[0].clone(), &admission, &reactor);
    let sender = transfers(signers[2].clone(), &admission, &reactor);
    let scope =
        RequestScope::new(RequestId([5; 16]), Instant::now() + Duration::from_secs(10)).unwrap();
    let cache = CacheId("cccccccc-1111-4111-8111-111111111111".into());
    let version = ObjectVersion {
        object: ObjectId {
            cache: cache.clone(),
            key: CacheKey([3; 32]),
        },
        etag: StrongEtag::parse(b"\"v1\"").unwrap(),
    };
    let envelope = PageEnvelope {
        page: PageId {
            version: version.clone(),
            number: PageNumber(0),
        },
        key_id: KeyId([1; 16]),
        nonce: Nonce([2; 24]),
        plaintext_length: 19,
        ciphertext_length: 35,
    };
    let page = BufferPool::new(admission.clone())
        .ciphertext(
            admission
                .reserve(Some(&cache), ResourceClass::Ciphertext, 35)
                .unwrap(),
            envelope,
            vec![77; 35],
        )
        .unwrap();
    let response = PeerResponse::Page {
        metadata: ObjectMetadata {
            content_type: None,
            version,
            length: 19,
            expires_at: ExpiresAt(std::time::UNIX_EPOCH),
        },
        ciphertext: page,
    };
    let mut head = p::response_head(
        &response,
        &[4; 32],
        &[
            signers[usize::from(wrong_path)].node().clone(),
            signers[2].node().clone(),
        ],
    )
    .unwrap();
    p::push(&mut head, "racer-receiver", &signers[0].node().0);
    let authentication = ForwardedHead {
        original: Arc::new(signers[2].sign(head).unwrap()),
        hops: vec![],
    };
    let mut binding = Binding {
        request: [4; 32],
        response: [0; 32],
        transfer: TransferId([6; 16]),
        membership: 1,
        deadline: p::encode_deadline(scope.deadline).unwrap(),
        rail: crate::topology::rails::RailId(7),
    };
    let accept = binding
        .sign(
            &signers[0],
            signers[2].node(),
            Phase::Accept,
            &[0; 32],
            0,
            vec![],
        )
        .unwrap();
    binding.response = envelope_digest(&authentication).unwrap();
    // A well-formed signed remote endpoint cannot manufacture a local provider.
    let mut setup = b"racer-rdma-setup-v1\0".to_vec();
    setup.extend_from_slice(&7u16.to_be_bytes());
    setup.extend_from_slice(&[1; 16]);
    setup.extend_from_slice(&[2; 16]);
    for value in [1u32, 2, 3] {
        setup.extend_from_slice(&value.to_be_bytes());
    }
    setup.extend_from_slice(&1u16.to_be_bytes());
    setup.extend_from_slice(&[1, 1]);
    let offer = binding
        .sign(
            &signers[2],
            signers[0].node(),
            Phase::Offer,
            &signed_digest(&accept).unwrap(),
            0,
            vec![extension(SETUP_HEADER, p::binary(&setup).into_bytes())],
        )
        .unwrap();
    let previous = signed_digest(&offer).unwrap();
    let mut offered = WireCodec::encode(&authentication, true, 0).unwrap();
    attach(&mut offered, &offer).unwrap();
    let response = SignedResponse {
        authentication,
        response,
    };
    let (a, b) = UnixStream::pair().unwrap();
    let a = ConnectionLease::from_accepted(a.into(), &admission).unwrap();
    let b = ConnectionLease::from_accepted(b.into(), &admission).unwrap();
    let receive = async {
        let a = crate::security::connection::connect(
            &receiver.io,
            a,
            signers[0].clone(),
            signers[2].node(),
            &scope,
        )
        .await?;
        let initial = MessageHead {
            start: StartLine::Request {
                method: "POST".into(),
                target: "/test".into(),
            },
            headers: vec![extension("content-length", b"0".to_vec())],
        };
        let sent = receiver.io.send_head(a, initial, &scope).await?;
        let mut received = receiver.io.receive_head(sent.connection, &scope).await?;
        let offer = detach(&mut received.value)?.unwrap();
        let (auth, _) = WireCodec::decode(received.value, true)?;
        let mut original = binding.clone();
        original.response = [0; 32];
        if sender_failure {
            let (verified, _) = binding.verify(
                &signers[0],
                signers[2].node(),
                offer,
                &[Phase::Offer],
                &signed_digest(&accept)?,
                0,
                &scope,
            )?;
            let mut connection = received.connection;
            connection.finish_exchange()?;
            let setup = binding.sign(
                &signers[0],
                signers[2].node(),
                Phase::Setup,
                &signed_digest(&verified.signed)?,
                0,
                vec![
                    extension(SETUP_HEADER, p::binary(&[1; 32]).into_bytes()),
                    extension(SETUP_BINDING_HEADER, p::binary(&[2; 32]).into_bytes()),
                ],
            )?;
            let setup_digest = signed_digest(&setup)?;
            connection = receiver.write_control(connection, setup, &scope).await?;
            let failed = receiver.read_control(connection, true, &scope).await?;
            let (mut connection, signed) = failed;
            let (failed, _) = binding.verify(
                &signers[0],
                signers[2].node(),
                signed,
                &[Phase::Failed],
                &setup_digest,
                0,
                &scope,
            )?;
            connection.finish_exchange()?;
            return receiver
                .receive_fallback(
                    connection,
                    auth,
                    &binding,
                    signers[2].node(),
                    signed_digest(&failed.signed)?,
                    &scope,
                )
                .await;
        }
        receiver
            .receive_native(
                received.connection,
                auth,
                original,
                accept,
                signers[2].node().clone(),
                offer,
                &membership,
                &scope,
            )
            .await
    };
    let send = async {
        let b =
            crate::security::connection::accept(&sender.io, b, signers[2].clone(), &scope).await?;
        let received = sender.io.receive_head(b, &scope).await?;
        let sent = sender
            .io
            .send_head(received.connection, offered, &scope)
            .await?;
        let mut connection = sent.connection;
        connection.finish_exchange()?;
        if rejected {
            // Keep the connection alive until the receiver returns its rejection.
            return std::future::pending::<Result<()>>().await;
        }
        if sender_failure {
            let (connection, setup) = sender.read_control(connection, false, &scope).await?;
            let (setup, _) = binding.verify(
                &signers[2],
                signers[0].node(),
                setup,
                &[Phase::Setup],
                &previous,
                0,
                &scope,
            )?;
            let (mut connection, _) = sender
                .failed_then_fallback(
                    connection,
                    &response,
                    &binding,
                    signers[0].node(),
                    signed_digest(&setup.signed)?,
                    &scope,
                )
                .await?;
            connection.finish_exchange()?;
            return Ok(());
        }
        let (connection, fallback) = sender.read_control(connection, false, &scope).await?;
        let (verified, phase) = binding.verify(
            &signers[2],
            signers[0].node(),
            fallback,
            &[Phase::Fallback],
            &previous,
            0,
            &scope,
        )?;
        assert_eq!(phase, Phase::Fallback);
        let (mut connection, sent) = sender
            .send_fallback(
                connection,
                &response,
                &binding,
                signers[0].node(),
                signed_digest(&verified.signed)?,
                &scope,
            )
            .await?;
        assert!(sent);
        connection.finish_exchange()?;
        Ok::<(), Error>(())
    };
    let mut future = std::pin::pin!(async { futures::try_join!(receive, send) });
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let result = loop {
        if let Poll::Ready(result) = std::future::Future::poll(future.as_mut(), &mut cx) {
            break result;
        }
        reactor.poll_budgeted(128).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    };
    if rejected {
        assert!(matches!(result, Err(Error::Unauthorized)));
        assert_eq!(receiver.native.as_ref().unwrap().prepare_attempts.get(), 0);
        return;
    }
    let (result, ()) = result.unwrap();
    assert_eq!(
        envelope_digest(&result.authentication).unwrap(),
        binding.response
    );
    match result.response {
        PeerResponse::Page { ciphertext, .. } => assert_eq!(ciphertext.bytes(), &[77; 35]),
        _ => panic!("expected retained ciphertext"),
    }
    assert_eq!(admission.used(ResourceClass::Registered), 0);
}

#[cfg(feature = "rdma")]
#[test]
#[ignore = "requires real ABI v2 adapter and RACER_RDMA_TEST_DEVICE/PORT/GID for an active type-2B port"]
fn native_provider_signed_setup_grant_write_completion_roundtrip() {
    native_roundtrip(false, false, None, false);
}

#[test]
fn simulated_native_mixed_site_hop_both_directions() {
    for reverse in [false, true] {
        for relayed in [false, true] {
            native_roundtrip(true, reverse, None, relayed);
        }
    }
}

#[test]
fn simulated_native_sender_rejects_cross_site_before_session_preparation() {
    for site in ["site2", ""] {
        native_roundtrip(true, false, Some(site), false);
        native_roundtrip(true, true, Some(site), true);
    }
}

fn native_roundtrip(simulated: bool, reverse: bool, rejected_site: Option<&str>, relayed: bool) {
    let reject_sender = rejected_site.is_some();
    use crate::rdma::{
        FabricPort,
        lifecycle::{NativeService, pair},
    };
    use crate::topology::{
        membership::{Member, Membership},
        rails::{RailId, RailMapping},
    };
    let device = if simulated {
        "sim-rnic".into()
    } else {
        std::env::var("RACER_RDMA_TEST_DEVICE").expect("select real device")
    };
    let port = if simulated {
        1
    } else {
        std::env::var("RACER_RDMA_TEST_PORT")
            .expect("select port")
            .parse()
            .unwrap()
    };
    let text = if simulated {
        "11".repeat(16)
    } else {
        std::env::var("RACER_RDMA_TEST_GID").expect("32 lowercase hex GID digits")
    };
    assert_eq!(text.len(), 32);
    let mut gid = [0; 16];
    for (i, b) in gid.iter_mut().enumerate() {
        *b = u8::from_str_radix(&text[2 * i..2 * i + 2], 16).unwrap();
    }
    let fabric = crate::rdma::lifecycle::simulation::Simulation::new()
        .with_devices(vec![crate::rdma::lifecycle::simulation::Device::new(
            device.clone(),
            gid,
        )])
        .unwrap();
    let _fabric = simulated.then(|| fabric.enter());
    let mut signers = super::super::tests::signers();
    if reverse {
        signers.swap(0, 2);
    }
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(true).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let mappings = vec![RailMapping {
        rail: RailId(7),
        fabric: "test-provider".into(),
        numa_node: None,
    }];
    let associations = vec![FabricPort {
        fabric: "test-provider".into(),
        device,
        port,
        gid: Some(gid),
    }];
    let scope =
        RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(30)).unwrap();
    let make = |signatures: Rc<Signatures>| {
        let (io, port) = pair(2).unwrap();
        let devices = Rc::new(Devices::new());
        devices.attach(io).unwrap();
        let sessions = Rc::new(Sessions::new(devices.clone(), 2));
        let rdma = Rc::new(RdmaTransfer::new(sessions.clone()));
        let http = Rc::new(HttpIo::with_admission(
            reactor.clone(),
            Codec::new(
                super::super::protocol::MAX_ENVELOPE_HEAD,
                crate::model::PAGE_BYTES + 16,
            ),
            admission.clone(),
        ));
        let transfer = Transfers::new(
            Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 2)),
            http,
            Some(rdma),
            admission.clone(),
            Rc::new(super::super::protocol::SecurityCodec::new(
                admission.clone(),
                Rc::new(BufferPool::new(admission.clone())),
            )),
            signatures.clone(),
        )
        .with_native(sessions);
        (devices, NativeService::new(port), transfer)
    };
    let (receive_devices, mut receive_engine, receiver) = make(signers[0].clone());
    let (send_devices, mut send_engine, sender) = make(signers[2].clone());
    let mut activate = std::pin::pin!(async {
        futures::try_join!(
            receive_devices.activate(
                mappings.clone(),
                associations.clone(),
                &admission,
                4096,
                &scope
            ),
            send_devices.activate(
                mappings.clone(),
                associations.clone(),
                &admission,
                4096,
                &scope
            )
        )
    });
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    loop {
        if let Poll::Ready(result) = std::future::Future::poll(activate.as_mut(), &mut cx) {
            result.expect("native provider activation must succeed");
            break;
        }
        receive_engine.poll_budgeted(8).unwrap();
        send_engine.poll_budgeted(8).unwrap();
    }
    let cache = CacheId("cccccccc-1111-4111-8111-111111111111".into());
    let version = ObjectVersion {
        object: ObjectId {
            cache: cache.clone(),
            key: CacheKey([9; 32]),
        },
        etag: StrongEtag::parse(b"\"provider\"").unwrap(),
    };
    let envelope = PageEnvelope {
        page: PageId {
            version: version.clone(),
            number: PageNumber(0),
        },
        key_id: KeyId([2; 16]),
        nonce: Nonce([3; 24]),
        plaintext_length: 128,
        ciphertext_length: 144,
    };
    let page = BufferPool::new(admission.clone())
        .ciphertext(
            admission
                .reserve(Some(&cache), ResourceClass::Ciphertext, 144)
                .unwrap(),
            envelope,
            vec![0x5a; 144],
        )
        .unwrap();
    let response = PeerResponse::Page {
        metadata: ObjectMetadata {
            content_type: None,
            version,
            length: 128,
            expires_at: ExpiresAt(std::time::UNIX_EPOCH),
        },
        ciphertext: page,
    };
    let path = if relayed {
        vec![
            signers[0].node().clone(),
            signers[2].node().clone(),
            signers[1].node().clone(),
        ]
    } else {
        vec![
            signers[1].node().clone(),
            signers[0].node().clone(),
            signers[2].node().clone(),
        ]
    };
    let mut head = p::response_head(&response, &[4; 32], &path).unwrap();
    p::push(&mut head, "racer-receiver", &path[1].0);
    let original = Arc::new(signers[if relayed { 1 } else { 2 }].sign(head).unwrap());
    let mut hops = vec![];
    if relayed {
        let mut hop = MessageHead {
            start: StartLine::Request {
                method: "POST".into(),
                target: "/racer/peer/v1/hop".into(),
            },
            headers: vec![],
        };
        p::push(&mut hop, "racer-kind", "response-hop");
        p::push(&mut hop, "content-length", 0);
        p::push_binary(
            &mut hop,
            "racer-original",
            &signed_digest(&original).unwrap(),
        );
        p::push_binary(
            &mut hop,
            "racer-previous",
            &signed_digest(&original).unwrap(),
        );
        p::push_binary(&mut hop, "racer-request-binding", &[4; 32]);
        p::push(&mut hop, "racer-response-path", p::nodes(&path).unwrap());
        p::push(&mut hop, "racer-reverse-index", 0);
        p::push(&mut hop, "racer-receiver", &signers[0].node().0);
        hops.push(signers[2].sign(hop).unwrap());
    }
    let response = SignedResponse {
        authentication: ForwardedHead { original, hops },
        response,
    };
    let binding = Binding {
        request: [4; 32],
        response: [0; 32],
        transfer: TransferId([6; 16]),
        membership: 1,
        deadline: p::encode_deadline(scope.deadline).unwrap(),
        rail: RailId(7),
    };
    let accept = binding
        .sign(
            &signers[0],
            signers[2].node(),
            Phase::Accept,
            &[0; 32],
            0,
            vec![],
        )
        .unwrap();
    let accept_wire = super::super::protocol::encode_signed(&accept).unwrap();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            [0, 1, 2]
                .into_iter()
                .map(|i| Member {
                    node: signers[i].node().clone(),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: format!("127.0.0.1:{}", 9000 + i),
                    rails: mappings.clone(),
                    alignment_enabled: true,
                    site: if i == 1 {
                        "site2"
                    } else if i == 2 {
                        rejected_site.unwrap_or("site1")
                    } else {
                        "site1"
                    }
                    .into(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let (a, b) = UnixStream::pair().unwrap();
    let a = ConnectionLease::from_accepted(a.into(), &admission).unwrap();
    let b = ConnectionLease::from_accepted(b.into(), &admission).unwrap();
    let receive = async {
        let a = crate::security::connection::connect(
            &receiver.io,
            a,
            signers[0].clone(),
            signers[2].node(),
            &scope,
        )
        .await?;
        let initial = MessageHead {
            start: StartLine::Request {
                method: "POST".into(),
                target: "/test".into(),
            },
            headers: vec![extension("content-length", b"0".to_vec())],
        };
        let sent = receiver.io.send_head(a, initial, &scope).await?;
        let mut offered = receiver.io.receive_head(sent.connection, &scope).await?;
        let control = detach(&mut offered.value)?;
        let (auth, length) = WireCodec::decode(offered.value, true)?;
        if reject_sender {
            assert!(control.is_none());
            let (_, body) = receiver
                .read_ciphertext(offered.connection, length, &scope)
                .await?;
            return receiver
                .wire
                .1
                .response(auth, body.bytes()?.to_vec(), &scope);
        }
        let control = control.ok_or(Error::Unavailable)?;
        assert_eq!(length, 0, "provider test requires native offer");
        receiver
            .receive_native(
                offered.connection,
                auth,
                binding.clone(),
                accept,
                signers[2].node().clone(),
                control,
                &membership,
                &scope,
            )
            .await
    };
    let send = async {
        let b =
            crate::security::connection::accept(&sender.io, b, signers[2].clone(), &scope).await?;
        let received = sender.io.receive_head(b, &scope).await?;
        let (verified, _) = binding.verify(
            &signers[2],
            signers[0].node(),
            super::super::protocol::decode_signed(&accept_wire)?,
            &[Phase::Accept],
            &[0; 32],
            0,
            &scope,
        )?;
        let (mut conn, sent) = sender
            .send_native(
                received.connection,
                &response,
                (binding.clone(), verified),
                &membership,
                &scope,
            )
            .await?;
        if reject_sender {
            assert!(!sent);
            assert_eq!(sender.native.as_ref().unwrap().prepare_attempts.get(), 0);
            let head = WireCodec::encode(&response.authentication, true, 144)?;
            conn = sender.io.send_head(conn, head, &scope).await?.connection;
            let PeerResponse::Page { ciphertext, .. } = &response.response else {
                unreachable!()
            };
            conn = sender
                .io
                .write_body(conn, ciphertext.clone(), &scope)
                .await?
                .lease;
        } else {
            assert!(sent);
        }
        assert_eq!(conn.tx_remaining, Some(0));
        assert_eq!(conn.rx_remaining, Some(0));
        conn.finish_exchange()?;
        Ok::<(), Error>(())
    };
    let mut exchange = std::pin::pin!(async { futures::try_join!(receive, send) });
    let (result, ()) = loop {
        if let Poll::Ready(result) = std::future::Future::poll(exchange.as_mut(), &mut cx) {
            break result.expect("native provider roundtrip");
        }
        receive_engine.poll_budgeted(8).unwrap();
        send_engine.poll_budgeted(8).unwrap();
        reactor.poll_budgeted(128).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    };
    match result.response {
        PeerResponse::Page { ciphertext, .. } => assert_eq!(ciphertext.bytes(), &[0x5a; 144]),
        _ => panic!("expected page"),
    }
    assert_eq!(
        sender.native_completed.get(),
        usize::from(!reject_sender),
        "HTTP fallback cannot pass a native success test"
    );
    assert_eq!(sender.native_fallbacks.get(), 0);
    assert_eq!(
        receiver.native_completions.get(),
        usize::from(!reject_sender)
    );
}
