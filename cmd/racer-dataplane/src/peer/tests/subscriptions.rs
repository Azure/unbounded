use super::*;
use crate::{
    http::{Codec, io::HttpIo},
    memory::page::CiphertextCopy,
    model::{ExpiresAt, ObjectMetadata, PageEnvelope},
    peer::subscriptions::{Demand, PageInterval, Subscription, TransferGrant},
    runtime::reactor::Reactor,
    topology::{
        health::LinkHealth,
        membership::{Member, Membership},
        paths::Paths,
    },
};
use std::{
    cell::Cell,
    task::{Context, Poll},
    time::UNIX_EPOCH,
};

fn subscribe(
    admission: &Admission,
    sender: &str,
    id: u8,
    start: u64,
    mode: FetchMode,
) -> PeerRequest {
    let mut request = request(admission, id);
    request.origin.authorization = None;
    request.route.visited = vec![NodeId(sender.into())];
    request.route.remaining_attempts = if matches!(mode, FetchMode::Acquire) {
        3
    } else {
        0
    };
    request.operation = Operation::Subscribe {
        subscription: Subscription {
            id: [id; 16],
            sequence: 0,
            page_budget: 2,
            byte_budget: 2 * crate::model::PAGE_BYTES + 32,
            version: ObjectVersion {
                object: request.origin.object.clone(),
                etag: StrongEtag::test_value("v1"),
            },
            demand: Demand::new(vec![PageInterval { start, end: 20 }]).unwrap(),
        },
        mode,
    };
    request
}

fn copy(admission: &Rc<Admission>, page: PageId) -> CiphertextCopy {
    let length = (page.number.0 + 1) * crate::model::PAGE_BYTES - crate::model::PAGE_BYTES + 3;
    let ciphertext = BufferPool::new(admission.clone())
        .ciphertext(
            admission
                .reserve(
                    Some(&page.version.object.cache),
                    ResourceClass::Ciphertext,
                    19,
                )
                .unwrap(),
            PageEnvelope {
                page: page.clone(),
                key_id: KeyId([1; 16]),
                nonce: Nonce([2; 24]),
                plaintext_length: 3,
                ciphertext_length: 19,
            },
            vec![3; 19],
        )
        .unwrap();
    CiphertextCopy {
        metadata: ObjectMetadata {
            version: page.version,
            length,
            content_type: None,
            expires_at: ExpiresAt(UNIX_EPOCH),
        },
        ciphertext,
    }
}

#[test]
fn signed_subscription_selects_hot_page_fans_out_and_isolates_credential_failures() {
    struct Never;
    impl PeerTransport for Never {
        fn exchange<'a>(
            &'a self,
            _: protocol::SignedRequest,
            _: crate::topology::membership::MembershipLease,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, protocol::SignedResponse> {
            Box::pin(async { panic!("destination must not relay") })
        }
    }
    struct Service {
        admission: Rc<Admission>,
        ready: Cell<bool>,
        calls: Cell<usize>,
        reject: bool,
    }
    impl server::LocalPageService for Service {
        fn serve_peer<'a>(
            &'a self,
            request: protocol::VerifiedRequest,
            _: crate::topology::membership::MembershipLease,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, PeerResponse> {
            Box::pin(async move {
                self.calls.set(self.calls.get() + 1);
                std::future::poll_fn(|_| {
                    if self.ready.get() {
                        Poll::Ready(())
                    } else {
                        Poll::Pending
                    }
                })
                .await;
                let Operation::Page { page, mode } = &request.request().operation else {
                    panic!("must project demand")
                };
                assert!(matches!(mode, FetchMode::Acquire));
                assert_eq!(request.request().route.remaining_attempts, 3);
                assert_eq!(page.number.0, 8);
                if self.reject && request.origin().node().0 == A {
                    return Ok(PeerResponse::OriginForbidden);
                }
                let copy = copy(&self.admission, page.clone());
                Ok(PeerResponse::Page {
                    metadata: copy.metadata,
                    ciphertext: copy.ciphertext,
                })
            })
        }
    }
    for reject in [false, true] {
        let signers = signers();
        let senders: Vec<_> = signers.iter().map(|s| Forwarding::new(s.clone())).collect();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let membership = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                [A, B, C]
                    .iter()
                    .enumerate()
                    .map(|(i, n)| Member {
                        node: NodeId((*n).into()),
                        shares: std::num::NonZeroU32::new(1).unwrap(),
                        peer_endpoint: format!("127.0.0.1:{}", 8000 + i),
                        rails: vec![],
                        alignment_enabled: false,
                    })
                    .collect(),
            )
            .unwrap(),
        );
        let network = Rc::new(
            PeerNetwork::new(
                NodeId(C.into()),
                crate::control::snapshot::PublishedState::for_membership(membership),
            )
            .unwrap(),
        );
        let destination = Rc::new(Forwarding::new(signers[2].clone()));
        let relay = Rc::new(Relay::new(
            Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
            destination.clone(),
            Rc::new(Never),
            admission.clone(),
            network.clone(),
        ));
        let service = Rc::new(Service {
            admission: admission.clone(),
            ready: Cell::new(false),
            calls: Cell::new(0),
            reject,
        });
        let server = server::PeerServer::new(
            Rc::new(HttpIo::with_admission(
                Rc::new(Reactor::new(admission.clone())),
                Codec::new(protocol::MAX_ENVELOPE_HEAD, crate::model::PAGE_BYTES + 16),
                admission.clone(),
            )),
            destination,
            admission.clone(),
            service.clone(),
            relay,
            Rc::new(codec(&admission)),
            signers[2].clone(),
        );
        let codec = codec(&admission);
        let first = subscribe(&admission, A, 1, 8, FetchMode::Acquire);
        let first_scope = first.origin.scope().clone();
        let (first, first_binding) = senders[0].sign_request(first).unwrap();
        // Exercise the actual versioned wire and canonical logical decoding.
        let (first, _) = WireCodec::decode(
            WireCodec::encode(&first.authentication, false, 0).unwrap(),
            false,
        )
        .unwrap();
        let mut first = server.dispatch(codec.request(first, &first_scope).unwrap(), &first_scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(first.as_mut().poll(&mut cx).is_pending());
        let second = subscribe(&admission, B, 2, 0, FetchMode::Acquire);
        let second_scope = second.origin.scope().clone();
        let (second, second_binding) = senders[1].sign_request(second).unwrap();
        let mut second = server.dispatch(second, &second_scope);
        assert!(second.as_mut().poll(&mut cx).is_pending());
        assert_eq!(service.calls.get(), 1);
        service.ready.set(true);
        let Poll::Ready(Ok(first)) = first.as_mut().poll(&mut cx) else {
            panic!("leader completes")
        };
        let Poll::Ready(Ok(second)) = second.as_mut().poll(&mut cx) else {
            panic!("follower completes")
        };
        assert_eq!(service.calls.get(), if reject { 2 } else { 1 });
        let first = senders[0].verify_response(first, &first_binding).unwrap();
        if reject {
            assert!(matches!(first.response(), PeerResponse::OriginForbidden));
        }
        let PeerResponse::Selected {
            ciphertext, grant, ..
        } = &second.response
        else {
            panic!("selected result")
        };
        assert_eq!(grant.page.number.0, 8);
        assert_eq!(grant.receiver.0, B);
        let body = ciphertext.bytes().to_vec();
        let (encoded, length) = WireCodec::decode(
            WireCodec::encode(&second.authentication, true, body.len()).unwrap(),
            true,
        )
        .unwrap();
        assert_eq!(length, 19);
        let decoded = codec.response(encoded, body, &second_scope).unwrap();
        let verified = senders[1]
            .verify_response(decoded, &second_binding)
            .unwrap();
        assert!(matches!(verified.response(), PeerResponse::Selected { .. }));
    }
}

#[test]
fn subscription_selection_is_canonical_signed_and_bound_to_exact_grant() {
    let signers = signers();
    let sender = Forwarding::new(signers[0].clone());
    let destination = Forwarding::new(signers[2].clone());
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let local = subscribe(&admission, A, 1, 0, FetchMode::CopyOnly);
    let (signed, binding) = sender.sign_request(local).unwrap();
    let admitted = destination.verify_request(signed).unwrap();
    let Operation::Subscribe { subscription, .. } = &admitted.request().operation else {
        panic!()
    };
    let page = PageId {
        version: subscription.version.clone(),
        number: PageNumber(0),
    };
    let good = TransferGrant {
        subscription_id: subscription.id,
        sequence: 0,
        page: page.clone(),
        membership: MembershipVersion(1),
        receiver: NodeId(A.into()),
        deadline: crate::security::protocol::encode_deadline(admitted.request().route.deadline)
            .unwrap(),
        remaining_page_budget: 1,
        remaining_byte_budget: subscription.byte_budget - 19,
    };
    for case in 0..10 {
        let mut grant = good.clone();
        match case {
            1 => grant.subscription_id[0] ^= 1,
            2 => grant.sequence += 1,
            3 => grant.receiver = NodeId(B.into()),
            4 => grant.membership.0 += 1,
            5 => grant.deadline += 1,
            6 => grant.remaining_page_budget += 1,
            7 => grant.remaining_byte_budget += 1,
            8 => grant.page.number.0 = 20,
            9 => grant.page.version.etag = StrongEtag::test_value("changed"),
            _ => (),
        }
        let copy = copy(&admission, grant.page.clone());
        let response = PeerResponse::Selected {
            metadata: copy.metadata,
            ciphertext: copy.ciphertext,
            grant,
        };
        // Sign directly to test receiver validation independently of sign_response.
        let mut head = crate::security::protocol::response_head(
            &response,
            &crate::security::signing::signed_digest(&admitted.signed().authentication.original)
                .unwrap(),
            &[NodeId(A.into()), NodeId(C.into())],
        )
        .unwrap();
        crate::security::protocol::push(&mut head, "racer-receiver", A);
        let signed = protocol::SignedResponse {
            authentication: crate::security::forwarding::ForwardedHead {
                original: Arc::new(signers[2].sign(head).unwrap()),
                hops: vec![],
            },
            response,
        };
        assert_eq!(
            sender.verify_response(signed, &binding).is_ok(),
            case == 0,
            "case {case}"
        );
    }
    let mut bad = subscribe(&admission, A, 3, 0, FetchMode::CopyOnly);
    bad.origin.authorization = Some(EncryptedAuthorization {
        key_id: KeyId([1; 16]),
        nonce: Nonce([1; 24]),
        ciphertext: vec![0; 16],
    });
    assert!(sender.sign_request(bad).is_err());
    let mut signed = sender
        .sign_request(subscribe(&admission, A, 4, 0, FetchMode::CopyOnly))
        .unwrap()
        .0;
    let Operation::Subscribe { subscription, .. } = &mut signed.request.operation else {
        panic!()
    };
    subscription.sequence += 1;
    assert!(
        destination.verify_request(signed).is_err(),
        "logical sequence must not be elided as a session auth field"
    );
}

#[test]
fn subscription_runs_through_real_tcp_requester_session_and_provider() {
    use crate::{
        http::connection::{ConnectionLease, HttpPool},
        peer::PeerClient,
    };
    struct Local(Rc<Admission>);
    impl server::LocalPageService for Local {
        fn serve_peer<'a>(
            &'a self,
            request: protocol::VerifiedRequest,
            _: crate::topology::membership::MembershipLease,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, PeerResponse> {
            Box::pin(async move {
                let Operation::Page {
                    page,
                    mode: FetchMode::CopyOnly,
                } = &request.request().operation
                else {
                    panic!("copy-only projection required")
                };
                assert!(request.request().origin.authorization.is_none());
                assert_eq!(request.request().route.remaining_attempts, 0);
                let copy = copy(&self.0, page.clone());
                Ok(PeerResponse::Page {
                    metadata: copy.metadata,
                    ciphertext: copy.ciphertext,
                })
            })
        }
    }
    impl PeerTransport for Local {
        fn exchange<'a>(
            &'a self,
            _: protocol::SignedRequest,
            _: crate::topology::membership::MembershipLease,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, protocol::SignedResponse> {
            Box::pin(async { panic!("no relay or speculative backup") })
        }
    }
    let signers = signers();
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(protocol::MAX_ENVELOPE_HEAD, crate::model::PAGE_BYTES + 16),
        admission.clone(),
    ));
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let address = listener.local_addr().unwrap();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            [A, C]
                .iter()
                .enumerate()
                .map(|(i, n)| Member {
                    node: NodeId((*n).into()),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: if i == 1 {
                        address.to_string()
                    } else {
                        "127.0.0.1:9000".into()
                    },
                    rails: vec![],
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let network = |node: &str| {
        Rc::new(
            PeerNetwork::new(
                NodeId(node.into()),
                crate::control::snapshot::PublishedState::for_membership(membership.clone()),
            )
            .unwrap(),
        )
    };
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 4));
    let destination = Rc::new(Forwarding::new(signers[2].clone()));
    let local = Rc::new(Local(admission.clone()));
    let codec = Rc::new(codec(&admission));
    let relay = Rc::new(Relay::new(
        paths.clone(),
        destination.clone(),
        local.clone(),
        admission.clone(),
        network(C),
    ));
    let server = server::PeerServer::new(
        io.clone(),
        destination,
        admission.clone(),
        local,
        relay,
        codec.clone(),
        signers[2].clone(),
    );
    let transfers = Rc::new(transport::Transfers::new(
        Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 2)),
        io,
        None,
        admission.clone(),
        codec,
        signers[0].clone(),
    ));
    let requester = Requester::new(
        paths,
        Rc::new(Forwarding::new(signers[0].clone())),
        transfers,
        network(A),
    );
    let request = subscribe(&admission, A, 1, 8, FetchMode::CopyOnly);
    let scope = request.origin.scope().clone();
    let server_work = async {
        let fd = reactor
            .accept(
                Rc::new(crate::runtime::reactor::Descriptor::from(listener)),
                &scope,
            )
            .await?;
        server
            .serve_connection(ConnectionLease::from_accepted(fd, &admission)?, &scope)
            .await?;
        Ok::<_, Error>(())
    };
    let exchange =
        async { futures::try_join!(requester.request(request, membership, &scope), server_work) };
    let mut exchange = std::pin::pin!(exchange);
    let watchdog = Instant::now() + Duration::from_secs(10);
    let (response, ()) = loop {
        if let Poll::Ready(result) = exchange
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
        {
            break result.unwrap();
        }
        assert!(Instant::now() < watchdog);
        reactor.poll_budgeted(128).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    };
    let PeerResponse::Selected {
        grant, ciphertext, ..
    } = response.response()
    else {
        panic!("selected response")
    };
    assert_eq!(grant.page.number.0, 8);
    assert_eq!(ciphertext.bytes(), &[3; 19]);
    assert_eq!(grant.remaining_page_budget, 1);
}

#[test]
fn retained_subscription_cannot_complete_after_request_mac_key_retirement() {
    use crate::control::wire::CacheKeyState;
    let (signers, discovery) = identities();
    let sender = Forwarding::new(signers[0].clone());
    let destination = Forwarding::new(signers[2].clone());
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let (request, _) = sender
        .sign_request(subscribe(&admission, A, 1, 0, FetchMode::CopyOnly))
        .unwrap();
    let admitted = destination.verify_request(request).unwrap();
    let keys = &discovery[2].0;
    let mut retired = crate::security::signing::tests::mac_test_key(CACHE);
    for key in &mut retired {
        key.state = CacheKeyState::Retiring;
    }
    let mut replacement = crate::security::signing::tests::mac_test_key(CACHE);
    for key in &mut replacement {
        key.key.id.0[0] ^= 1;
        key.material[0] ^= 1;
    }
    retired.extend(replacement);
    keys.install(KeyringBundle {
        schema_version: SCHEMA_VERSION,
        cluster: ClusterId(CLUSTER.into()),
        generation: BundleGeneration(2),
        peer_trust_roots: (*keys.peer_trust_roots().unwrap()).clone(),
        cache_keys: retired,
    })
    .unwrap();
    assert!(
        destination
            .sign_response(admitted.binding(), PeerResponse::Miss)
            .is_err()
    );
}
