use super::*;
use crate::{
    peer::timing::{PageTiming, STAGES},
    runtime::environment::SimulationClock,
    telemetry::metrics::{Event, Metrics},
    topology::rails::{RailId, TransportPlan},
};

fn page_request(admission: &Admission, attempt: u8) -> PeerRequest {
    let mut local = request(admission, attempt);
    local.operation = Operation::Page {
        page: PageId {
            version: ObjectVersion {
                object: local.origin.object.clone(),
                etag: StrongEtag::test_value("timing"),
            },
            number: PageNumber(0),
        },
        mode: FetchMode::CopyOnly,
    };
    local
}

fn page_response(admission: &Rc<Admission>, request: &PeerRequest) -> PeerResponse {
    let Operation::Page { page, .. } = &request.operation else {
        panic!()
    };
    PeerResponse::Page {
        metadata: crate::model::metadata::ObjectMetadata {
            content_type: None,
            version: page.version.clone(),
            length: 3,
            expires_at: crate::model::metadata::ExpiresAt(
                crate::runtime::environment::wall_now() + Duration::from_secs(60),
            ),
        },
        // Intentionally not AEAD-valid: forwarding verification does not decrypt.
        ciphertext: BufferPool::new(admission.clone())
            .ciphertext(
                admission
                    .reserve(
                        Some(&page.version.object.cache),
                        ResourceClass::Ciphertext,
                        19,
                    )
                    .unwrap(),
                crate::model::envelope::PageEnvelope {
                    page: page.clone(),
                    key_id: KeyId([1; 16]),
                    nonce: Nonce([2; 24]),
                    plaintext_length: 3,
                    ciphertext_length: 19,
                },
                vec![9; 19],
            )
            .unwrap(),
    }
}

#[test]
fn page_timing_deterministic_delays_publish_once_only_after_verified_page() {
    let signers = signers();
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let sender = Forwarding::new(signers[0].clone());
    let receiver = Forwarding::new(signers[2].clone());
    let local = page_request(&admission, 1);
    let metrics = Metrics::default();
    let clock = SimulationClock::new_at(7, Instant::now(), std::time::SystemTime::now());
    let _environment = clock.environment(1).enter();
    let mut timing = PageTiming::new(&metrics);
    timing.enable(&local, TransportPlan::Http, false);
    for stage in 0..4 {
        if stage > 0 {
            timing.begin();
        }
        clock.advance(Duration::from_nanos((stage + 1) as u64 * 11));
        timing.end(stage);
        for (count, sum) in STAGES {
            assert_eq!(metrics.count(count), 0);
            assert_eq!(metrics.count(sum), 0);
        }
    }
    let (signed, binding) = sender.sign_request(local).unwrap();
    let admitted = receiver.verify_request(signed).unwrap();
    let response = receiver
        .sign_response(
            admitted.binding(),
            page_response(&admission, admitted.request()),
        )
        .unwrap();
    let verified = sender.verify_response(response, &binding).unwrap();
    timing.success(&verified);
    timing.success(&verified);
    drop(timing);
    for (stage, (count, sum)) in STAGES.into_iter().enumerate() {
        assert_eq!(metrics.count(count), 1);
        assert_eq!(metrics.count(sum), (stage + 1) as u64 * 11);
    }
    assert_eq!(metrics.count(Event::PeerPageCensored), 0);
}

#[test]
fn page_timing_excludes_metadata_transit_native_and_opaque() {
    let admission = Admission::new(crate::test_support::cluster::config(false).limits);
    let metrics = Metrics::default();
    for case in ["metadata", "bootstrap", "transit", "native", "opaque"] {
        let mut local = page_request(&admission, 1);
        if case == "metadata" {
            local = request(&admission, 1);
        }
        if case == "bootstrap" {
            local.operation = Operation::Bootstrap {
                object: local.origin.object.clone(),
                mode: FetchMode::CopyOnly,
            };
        }
        if case == "transit" {
            local.route.visited.push(NodeId(B.into()));
        }
        let plan = if case == "native" {
            TransportPlan::Rdma { rail: RailId(0) }
        } else {
            TransportPlan::Http
        };
        let mut timing = PageTiming::new(&metrics);
        timing.enable(&local, plan, case == "opaque");
        for stage in 0..4 {
            timing.begin();
            timing.end(stage);
        }
        drop(timing);
    }
    assert_eq!(metrics.count(Event::PeerPageCensored), 0);
    for (count, sum) in STAGES {
        assert_eq!(metrics.count(count), 0);
        assert_eq!(metrics.count(sum), 0);
    }
}

#[test]
fn page_timing_pending_future_drop_counts_once_without_partial_samples() {
    use std::task::{Context, Poll};
    let admission = Admission::new(crate::test_support::cluster::config(false).limits);
    let metrics = Metrics::default();
    let local = page_request(&admission, 1);
    let clock = SimulationClock::new_at(8, Instant::now(), std::time::SystemTime::now());
    let _environment = clock.environment(1).enter();
    let mut work = Box::pin(async {
        let mut timing = PageTiming::new(&metrics);
        timing.enable(&local, TransportPlan::Http, false);
        clock.advance(Duration::from_nanos(17));
        timing.end(0);
        futures::future::pending::<()>().await;
        drop(timing);
    });
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for _ in 0..3 {
        assert!(matches!(work.as_mut().poll(&mut cx), Poll::Pending));
        assert_eq!(metrics.count(Event::PeerPageCensored), 0);
    }
    drop(work);
    assert_eq!(metrics.count(Event::PeerPageCensored), 1);
    for (count, sum) in STAGES {
        assert_eq!(metrics.count(count), 0);
        assert_eq!(metrics.count(sum), 0);
    }
}

#[test]
fn page_timing_invalid_signature_miss_and_drop_censor_without_partial_success() {
    let signers = signers();
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let sender = Forwarding::new(signers[0].clone());
    let receiver = Forwarding::new(signers[2].clone());
    let metrics = Metrics::default();
    for (attempt, case) in ["invalid", "miss", "drop"].into_iter().enumerate() {
        let local = page_request(&admission, attempt as u8);
        let mut timing = PageTiming::new(&metrics);
        timing.enable(&local, TransportPlan::Http, false);
        for stage in 0..4 {
            timing.begin();
            timing.end(stage);
        }
        let (signed, binding) = sender.sign_request(local).unwrap();
        let admitted = receiver.verify_request(signed).unwrap();
        let response = if case == "miss" {
            PeerResponse::Miss
        } else {
            page_response(&admission, admitted.request())
        };
        let mut response = receiver
            .sign_response(admitted.binding(), response)
            .unwrap();
        if case == "invalid" {
            Arc::get_mut(&mut response.authentication.original)
                .unwrap()
                .signature[0] ^= 1;
            assert!(sender.verify_response(response, &binding).is_err());
        } else if case == "miss" {
            timing.success(&sender.verify_response(response, &binding).unwrap());
        }
        drop(timing);
        assert_eq!(metrics.count(Event::PeerPageCensored), attempt as u64 + 1);
        for (count, sum) in STAGES {
            assert_eq!(metrics.count(count), 0);
            assert_eq!(metrics.count(sum), 0);
        }
    }
}

#[test]
fn page_timing_requester_reuses_authenticated_session_and_rejects_bad_signature() {
    use crate::{
        http::{
            codec::Codec,
            io::HttpIo,
            pool::{ConnectionLease, HttpPool},
        },
        peer::requester::PeerClient,
        runtime::reactor::Reactor,
        topology::{
            health::LinkHealth,
            membership::{Member, Membership},
            paths::Paths,
            rails::Rails,
        },
    };
    use std::{
        net::TcpListener,
        task::{Context, Poll},
    };
    let signers = signers();
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
    let pool = Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 1));
    let transfers = Rc::new(
        transfer::Transfers::new(pool.clone(), io.clone(), None)
            .with_wire(admission.clone(), Rc::new(codec(&admission)))
            .with_signatures(signers[0].clone()),
    );
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let members = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            [A, C]
                .into_iter()
                .map(|name| Member {
                    node: NodeId(name.into()),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: address.to_string(),
                    rails: vec![],
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let metrics = Metrics::default();
    let requester = requester::Requester::new(
        Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
        Rc::new(Rails),
        Rc::new(Forwarding::new(signers[0].clone())),
        transfers,
    )
    .with_metrics(metrics.clone())
    .with_network(Rc::new(
        PeerNetwork::new(
            NodeId(A.into()),
            crate::control::snapshot::PublishedState::for_membership(members.clone()),
        )
        .unwrap(),
    ));
    let scope =
        RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
    let client = async {
        for attempt in 0..3 {
            let response = requester
                .request(page_request(&admission, attempt), members.clone(), &scope)
                .await;
            if attempt < 2 {
                assert!(matches!(
                    response.unwrap().response(),
                    PeerResponse::Page { .. }
                ));
            } else {
                assert!(matches!(response, Err(Error::Unauthorized)));
            }
            for (count, _) in STAGES {
                assert_eq!(metrics.count(count), (attempt as u64 + 1).min(2));
            }
        }
        assert_eq!(metrics.count(Event::PeerPageCensored), 1);
        Ok::<_, Error>(())
    };
    let server = async {
        // Only one accept and handshake: all three exchanges must reuse this session.
        let fd = reactor.accept(Rc::new(listener.into()), &scope).await?;
        let connection = ConnectionLease::from_accepted(fd, &admission)?;
        let mut connection =
            crate::security::connection::accept(&io, connection, signers[2].clone(), &scope)
                .await?;
        let auth = Forwarding::new(signers[2].clone());
        for attempt in 0..3 {
            let received = io.receive_head(connection, &scope).await?;
            let (head, _) = WireCodec::decode(received.value, false)?;
            let request = auth.verify_request(codec(&admission).request(head, &scope)?)?;
            let mut response = auth.sign_response(
                request.binding(),
                page_response(&admission, request.request()),
            )?;
            if attempt == 2 {
                Arc::get_mut(&mut response.authentication.original)
                    .unwrap()
                    .signature[0] ^= 1;
            }
            let PeerResponse::Page { ciphertext, .. } = response.response else {
                panic!()
            };
            let sent = io
                .send_head(
                    received.connection,
                    WireCodec::encode(&response.authentication, true, 19)?,
                    &scope,
                )
                .await?;
            let sent = io
                .write_body_range(sent.connection, ciphertext, 0..19, &scope)
                .await?;
            connection = sent.lease;
            connection.finish_exchange()?;
        }
        Ok::<_, Error>(())
    };
    let mut work = Box::pin(async { futures::try_join!(client, server) });
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    loop {
        if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
            result.unwrap();
            break;
        }
        scope.check().unwrap();
        reactor.poll_budgeted(128).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
    drop(work);
    drop(requester);
    drop(pool);
    let mut drain = reactor.drain();
    while drain.as_mut().poll(&mut cx).is_pending() {
        scope.check().unwrap();
        reactor.poll_budgeted(128).unwrap();
    }
    drop(drain);
    assert_eq!(reactor.in_flight(), 0);
    drop(io);
    drop(reactor);
    admission.reclaim_buffers();
    for class in [
        ResourceClass::Connection,
        ResourceClass::IngressConnection,
        ResourceClass::Ciphertext,
        ResourceClass::RequestContext,
        ResourceClass::ControlProgress,
    ] {
        assert_eq!(admission.used(class), 0, "{class:?}");
    }
}
