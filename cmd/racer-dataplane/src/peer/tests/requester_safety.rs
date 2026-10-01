//! Real socket exchange with the adaptive controller attached to routing/requester.
use crate::peer::*;
use crate::{
    http::{
        Codec,
        connection::{ConnectionLease, HttpIo, HttpPool},
    },
    model::{MembershipVersion, ResourceClass},
    peer::{
        adaptive::{AdaptivePeers, Outcome},
        protocol::{PeerResponse, SecurityCodec, WireCodec},
        transport::RelayResponse,
    },
    runtime::{
        admission::Admission,
        reactor::{Descriptor, Reactor},
    },
    security::connection,
    telemetry::metrics::{Event, Gauge, Metrics},
    topology::{
        health::LinkHealth,
        membership::{Member, Membership},
    },
};
use std::{
    sync::Arc,
    task::{Context, Poll},
    time::{Duration, Instant},
};

#[test]
fn attached_requester_verification_and_opaque_outcome_gate_real_probe_recovery() {
    for opaque in [false, true] {
        for case in ["overloaded", "corrupt", "miss"] {
            probe_exchange(opaque, case);
        }
    }
}

#[test]
fn direct_page_hedge_transport_pins_receiver_and_uses_shared_admission() {
    probe_exchange(false, "direct");
}

#[test]
fn page_hedge_does_not_treat_multihop_destination_as_independent_first_hop() {
    let members = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            (0..1500)
                .map(|i| Member {
                    node: crate::model::NodeId(format!("{i:08x}-1111-4111-8111-111111111111")),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: format!("127.0.0.1:{}", 8000 + i),
                    rails: vec![],
                    alignment_enabled: false,
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let local = members.members()[0].node.clone();
    let network = Rc::new(
        crate::peer::PeerNetwork::new(
            local.clone(),
            crate::control::snapshot::PublishedState::for_membership(members.clone()),
        )
        .unwrap(),
    );
    let nonneighbor = members
        .members()
        .iter()
        .find(|m| m.node != local && network.endpoint(&members, &m.node).is_err())
        .unwrap();
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD, 0),
        admission.clone(),
    ));
    let pool = Rc::new(HttpPool::new(reactor, admission.clone(), 2));
    let adaptive = AdaptivePeers::new(Default::default(), Metrics::default()).unwrap();
    let signers = crate::peer::tests::signers();
    let requester = Requester::new(
        Rc::new(Paths::new(Rc::new(LinkHealth), 4).with_peer_admission(adaptive)),
        Rc::new(Forwarding::new(signers[0].clone())),
        Rc::new(Transfers::new(
            pool,
            io,
            None,
            admission.clone(),
            Rc::new(crate::peer::tests::codec(&admission)),
            signers[0].clone(),
        )),
        network,
    );
    assert!(!requester.direct_hedge_available(&members, &nonneighbor.node));
}

fn probe_exchange(opaque: bool, case: &str) {
    let signers = crate::peer::tests::signers();
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD, 0),
        admission.clone(),
    ));
    let pool = Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 2));
    let codec = Rc::new(SecurityCodec::new(
        admission.clone(),
        Rc::new(crate::memory::pool::BufferPool::new(admission.clone())),
    ));
    let transfers = Rc::new(Transfers::new(
        pool,
        io.clone(),
        None,
        admission.clone(),
        codec.clone(),
        signers[0].clone(),
    ));
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap().to_string();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            [0, 2]
                .iter()
                .map(|i| Member {
                    node: signers[*i].node().clone(),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: address.clone(),
                    rails: vec![],
                    alignment_enabled: false,
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let metrics = Metrics::default();
    let adaptive = AdaptivePeers::new(
        crate::peer::adaptive::Config {
            total: 4,
            per_peer: 1,
        },
        metrics.clone(),
    )
    .unwrap();
    let node = signers[2].node().clone();
    if case != "direct" {
        let failed = adaptive.acquire(&node).unwrap();
        failed.observe(Outcome::PeerFailure);
        drop(failed);
        std::thread::sleep(Duration::from_millis(260));
    }
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 4).with_peer_admission(adaptive.clone()));
    let forwarding = Rc::new(Forwarding::new(signers[0].clone()));
    let requester = Requester::new(
        paths,
        forwarding.clone(),
        transfers,
        Rc::new(
            crate::peer::PeerNetwork::new(
                signers[0].node().clone(),
                crate::control::snapshot::PublishedState::for_membership(membership.clone()),
            )
            .unwrap(),
        ),
    );
    let mut request = crate::peer::tests::request(&admission, 91);
    let scope = RequestScope::new(
        request.route.request,
        Instant::now() + Duration::from_secs(3),
    )
    .unwrap();
    request.route.deadline = scope.deadline;
    request.origin.scope = scope.clone();
    if case == "direct" {
        let held = adaptive.acquire(&node).unwrap();
        assert!(!requester.direct_hedge_available(&membership, &node));
        drop(held);
        request.operation = crate::peer::protocol::Operation::Page {
            page: crate::model::PageId {
                version: crate::model::ObjectVersion {
                    object: request.origin.object.clone(),
                    etag: crate::model::StrongEtag::test_value("hedge"),
                },
                number: crate::model::PageNumber(0),
            },
            mode: crate::peer::protocol::FetchMode::CopyOnly,
        };
        assert!(requester.direct_hedge_available(&membership, &node));
    }
    let (direct, signed) = if case == "direct" {
        (Some(request), None)
    } else {
        (None, Some(forwarding.sign_request(request).unwrap().0))
    };
    let server = async {
        let fd = reactor
            .accept(Rc::new(Descriptor::from(listener)), &scope)
            .await?;
        let conn = ConnectionLease::from_accepted(fd, &admission)?;
        let conn = connection::accept(&io, conn, signers[2].clone(), &scope).await?;
        let received = io.receive_head(conn, &scope).await?;
        let (head, length) = WireCodec::decode(received.value, false)?;
        assert_eq!(length, 0);
        let auth = Forwarding::new(signers[2].clone());
        let request = auth.verify_request(codec.request(head, &scope)?)?;
        let outcome = if case == "overloaded" {
            PeerResponse::Overloaded
        } else {
            PeerResponse::Miss
        };
        let mut response = auth.sign_response(request.binding(), outcome)?;
        if case == "corrupt" {
            // The session authenticates the outer frame, but the original proof
            // no longer agrees with its signed application outcome.
            let original = Arc::get_mut(&mut response.authentication.original).unwrap();
            original
                .head
                .headers
                .iter_mut()
                .find(|h| h.name == "racer-outcome")
                .unwrap()
                .value = b"overloaded".to_vec();
        }
        let head = WireCodec::encode(&response.authentication, true, 0)?;
        let sent = io.send_head(received.connection, head, &scope).await?;
        drop(sent);
        Ok::<_, Error>(())
    };
    let client = async {
        if let Some(request) = direct {
            requester
                .request_direct(request, membership, &scope)
                .await
                .map(|r| RelayResponse::Complete(r.into_signed()))
        } else if opaque {
            requester
                .exchange_relay(
                    signed.unwrap(),
                    membership,
                    Rc::new(admission.reserve(None, ResourceClass::Relay, 1)?),
                    &scope,
                )
                .await
        } else {
            requester
                .exchange(signed.unwrap(), membership, &scope)
                .await
                .map(RelayResponse::Complete)
        }
    };
    let mut work = Box::pin(async { futures::join!(client, server) });
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let (result, served) = loop {
        assert!(
            Instant::now() < scope.deadline.0,
            "real peer safety watchdog"
        );
        if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
            break result;
        }
        reactor.poll_budgeted(128).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    };
    served.unwrap();
    drop(work);
    if case == "corrupt" {
        assert!(result.is_err());
    } else {
        match result.unwrap() {
            RelayResponse::Http {
                mut connection,
                length,
                ..
            } => {
                assert_eq!(length, 0);
                assert_eq!(
                    metrics.count(Event::PeerVerified),
                    0,
                    "headers cannot recover probe"
                );
                assert_eq!(metrics.gauge(Gauge::PeerExchanges), 1);
                connection.finish_exchange().unwrap();
                drop(connection);
            }
            RelayResponse::Complete(response) => assert_eq!(
                matches!(response.response, PeerResponse::Overloaded),
                case == "overloaded"
            ),
        }
    }
    assert_eq!(metrics.count(Event::PeerProbe), u64::from(case != "direct"));
    assert_eq!(
        metrics.count(Event::PeerVerified),
        u64::from(case == "miss" || case == "direct")
    );
    assert_eq!(
        adaptive.available(&node),
        case == "miss" || case == "direct"
    );
    assert_eq!(metrics.gauge(Gauge::PeerExchanges), 0);
}
