use super::*;
use crate::{
    http::{Codec, connection::HttpIo},
    memory::page::CiphertextCopy,
    model::{ExpiresAt, ObjectMetadata, PageEnvelope},
    peer::subscriptions::{Demand, PageInterval, Subscription, TransferGrant},
    runtime::reactor::Reactor,
    topology::{
        health::LinkHealth,
        membership::{Member, Membership},
        routing::Paths,
    },
};
use std::{
    cell::Cell,
    task::{Context, Poll},
    time::UNIX_EPOCH,
};

fn subscribe(
    admission: &flow_control::Quotas<AdmissionPolicy>,
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

fn copy(admission: &Rc<flow_control::Quotas<AdmissionPolicy>>, page: PageId) -> CiphertextCopy {
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
            expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
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
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
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
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
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
                        site: String::new(),
                    })
                    .collect(),
            )
            .unwrap(),
        );
        let network = Rc::new(
            PeerNetwork::new(
                NodeId(C.into()),
                crate::control::state::PublishedState::for_membership(membership),
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
        let server = server::PeerServer::for_test(
            Rc::new(HttpIo::with_admission(
                Rc::new(Reactor::new(admission.clone())),
                Codec::new(protocol::MAX_ENVELOPE_HEAD),
                admission.clone(),
                crate::model::PAGE_BYTES + 16,
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
        let (first, _) = decode_envelope(
            encode_envelope(&first.authentication, false, 0).unwrap(),
            false,
        )
        .unwrap();
        let mut first = server.dispatch(codec.request(first, &first_scope).unwrap(), &first_scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(first.as_mut().poll(&mut cx).is_pending());
        assert!(first.as_mut().poll(&mut cx).is_pending());
        let second = subscribe(&admission, B, 2, 0, FetchMode::Acquire);
        let second_scope = second.origin.scope().clone();
        let (second, second_binding) = senders[1].sign_request(second).unwrap();
        let mut second = server.dispatch(second, &second_scope);
        // Both compact endpoints may need independent cooperative eligibility
        // checks before the follower joins the held leader.
        for _ in 0..4 {
            assert!(second.as_mut().poll(&mut cx).is_pending());
        }
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
        let (encoded, length) = decode_envelope(
            encode_envelope(&second.authentication, true, body.len()).unwrap(),
            true,
        )
        .unwrap();
        assert_eq!(length, 19);
        let decoded = codec.response(encoded, body, &second_scope).unwrap();
        let verified = senders[1]
            .verify_response(decoded, &second_binding)
            .unwrap();
        assert!(matches!(verified.response(), PeerResponse::Selected { .. }));
        let PeerResponse::Selected { ciphertext, .. } = verified.response() else {
            unreachable!()
        };
        let provenance = ciphertext.provenance.unwrap();
        assert_eq!(&provenance.supplier, signers[2].node().0.as_bytes());
        assert_eq!(provenance.remote, provenance.supplier);
        let retained = ciphertext.clone();
        drop(verified);
        assert_eq!(retained.provenance, Some(provenance));
    }
}

#[test]
fn signed_ingress_cold_selection_is_bounded_cancellable_and_does_not_block_other_workers() {
    use crate::peer::subscriptions::Selection;
    use crate::topology::membership::scored_members;

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
    impl server::LocalPageService for Never {
        fn serve_peer<'a>(
            &'a self,
            _: protocol::VerifiedRequest,
            _: crate::topology::membership::MembershipLease,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, PeerResponse> {
            Box::pin(async { panic!("all demand endpoints must be ineligible") })
        }
    }
    let signers = signers();
    let sender = Forwarding::new(signers[0].clone());
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    // Full protocol membership; the local provider has negligible weight. The
    // signed unavailable response below proves all 64 endpoints were ineligible.
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            (0..100_000)
                .map(|i| Member {
                    node: NodeId(match i {
                        0 => A.into(),
                        1 => C.into(),
                        _ => format!("member-{i:06}"),
                    }),
                    shares: std::num::NonZeroU32::new(if i == 1 { 1 } else { u32::MAX }).unwrap(),
                    peer_endpoint: "127.0.0.1:8000".into(),
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let destination = Rc::new(Forwarding::new(signers[2].clone()));
    let network = Rc::new(
        PeerNetwork::new(
            NodeId(C.into()),
            crate::control::state::PublishedState::for_membership(membership),
        )
        .unwrap(),
    );
    let relay = Rc::new(Relay::new(
        Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
        destination.clone(),
        Rc::new(Never),
        admission.clone(),
        network,
    ));
    let server = server::PeerServer::for_test(
        Rc::new(HttpIo::with_admission(
            Rc::new(Reactor::new(admission.clone())),
            Codec::new(protocol::MAX_ENVELOPE_HEAD),
            admission.clone(),
            crate::model::PAGE_BYTES + 16,
        )),
        destination,
        admission.clone(),
        Rc::new(Never),
        relay,
        Rc::new(codec(&admission)),
        signers[2].clone(),
    );
    let mut request = subscribe(&admission, A, 70, 0, FetchMode::Acquire);
    let scope = RequestScope::new(
        RequestId([70; 16]),
        Instant::now() + Duration::from_secs(240),
    )
    .unwrap();
    request.origin.scope = scope.clone();
    request.origin.request = scope.request;
    request.route.request = scope.request;
    request.route.deadline = scope.deadline;
    let Operation::Subscribe { subscription, .. } = &mut request.operation else {
        panic!()
    };
    subscription.demand = Demand::new(
        (0..64)
            .map(|i| PageInterval {
                start: i * 2,
                end: i * 2 + 1,
            })
            .collect(),
    )
    .unwrap();
    let contract = subscription.clone();
    let (signed, _) = sender.sign_request(request).unwrap();
    let (wire, _) = decode_envelope(
        encode_envelope(&signed.authentication, false, 0).unwrap(),
        false,
    )
    .unwrap();
    let mut work = server.dispatch(codec(&admission).request(wire, &scope).unwrap(), &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let before = scored_members();
    assert!(work.as_mut().poll(&mut cx).is_pending());
    assert_eq!(scored_members() - before, 256);

    // Another OS worker must acquire the same node-wide scheduler while ingress
    // ranking is suspended. A bounded channel receive diagnoses lock retention.
    let scheduler = server.subscription_owner().clone();
    let mut other = contract.clone();
    other.id = [71; 16];
    let (send, receive) = std::sync::mpsc::channel();
    let worker = std::thread::spawn(move || {
        let now = crate::security::protocol::millis(std::time::SystemTime::now()).unwrap();
        let Selection::Leader { work, mut waiter } = scheduler
            .schedule(
                other,
                MembershipVersion(1),
                NodeId(B.into()),
                now + 30_000,
                now,
            )
            .unwrap()
        else {
            panic!("independent flight")
        };
        work.fail(Error::Unavailable);
        assert!(matches!(waiter.try_result(now), Err(Error::Unavailable)));
        send.send(()).unwrap();
    });
    receive.recv_timeout(Duration::from_secs(2)).unwrap();
    worker.join().unwrap();
    scope.cancel().unwrap();
    let before = scored_members();
    assert!(matches!(
        work.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    ));
    assert_eq!(
        scored_members(),
        before,
        "cancellation must hash no more members"
    );
    drop(work);
    assert_eq!(admission.used(ResourceClass::Waiter), 0);

    // Resume the accepted contract with a new signed sequence and the original
    // deadline, then traverse every ineligible interval. Every poll is measured,
    // including rank completion and transition to the next endpoint.
    let mut request = subscribe(&admission, A, 72, 0, FetchMode::Acquire);
    let resume_scope = RequestScope::new(scope.request, scope.deadline.0).unwrap();
    request.origin.scope = resume_scope.clone();
    request.origin.request = resume_scope.request;
    request.route.request = resume_scope.request;
    request.route.deadline = resume_scope.deadline;
    let mut resume = contract.clone();
    resume.sequence = 1;
    request.operation = Operation::Subscribe {
        subscription: resume,
        mode: FetchMode::Acquire,
    };
    let (signed, binding) = sender.sign_request(request).unwrap();
    let (wire, _) = decode_envelope(
        encode_envelope(&signed.authentication, false, 0).unwrap(),
        false,
    )
    .unwrap();
    let mut work = server.dispatch(
        codec(&admission).request(wire, &resume_scope).unwrap(),
        &resume_scope,
    );
    let start = scored_members();
    let mut polls = 0;
    let response = loop {
        let before = scored_members();
        let result = work.as_mut().poll(&mut cx);
        assert!(
            scored_members() - before <= 256,
            "poll {polls} exceeded rank quantum"
        );
        polls += 1;
        assert!(polls < 30_000, "bounded interval set must finish");
        if let Poll::Ready(result) = result {
            break result.unwrap();
        }
    };
    assert!(
        polls > 20_000,
        "must actually traverse large cold membership"
    );
    assert_eq!(
        scored_members() - start,
        64 * 100_000 - 256,
        "canceled partial ranking is reused"
    );
    assert!(matches!(
        sender
            .verify_response(response, &binding)
            .unwrap()
            .response(),
        PeerResponse::Unavailable
    ));
    // Persistent cache avoids another full membership traversal for later updates.
    drop(work);
    let mut request = subscribe(&admission, A, 73, 0, FetchMode::Acquire);
    request.origin.scope = resume_scope.clone();
    request.origin.request = resume_scope.request;
    request.route.request = resume_scope.request;
    request.route.deadline = resume_scope.deadline;
    let mut resume = contract;
    resume.sequence = 2;
    request.operation = Operation::Subscribe {
        subscription: resume,
        mode: FetchMode::Acquire,
    };
    let (signed, binding) = sender.sign_request(request).unwrap();
    let mut work = server.dispatch(signed, &resume_scope);
    let before = scored_members();
    for _ in 0..64 {
        assert!(
            work.as_mut().poll(&mut cx).is_pending(),
            "even hot endpoints yield"
        );
    }
    let Poll::Ready(Ok(response)) = work.as_mut().poll(&mut cx) else {
        panic!("hot sweep finishes")
    };
    assert_eq!(scored_members(), before);
    assert!(matches!(
        sender
            .verify_response(response, &binding)
            .unwrap()
            .response(),
        PeerResponse::Unavailable
    ));
}

#[test]
fn subscription_selection_is_canonical_signed_and_bound_to_exact_grant() {
    let signers = signers();
    let sender = Forwarding::new(signers[0].clone());
    let destination = Forwarding::new(signers[2].clone());
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
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
            &crate::security::connection::signed_digest(&admitted.signed().authentication.original)
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
    use crate::peer::PeerClient;
    struct Local(Rc<flow_control::Quotas<AdmissionPolicy>>);
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
    let fixture = SocketFixture::new(2);
    let transfers = fixture.transfers(signers[0].clone());
    let SocketFixture {
        admission,
        reactor,
        io,
        codec,
        ..
    } = fixture;
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
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let network = |node: &str| {
        Rc::new(
            PeerNetwork::new(
                NodeId(node.into()),
                crate::control::state::PublishedState::for_membership(membership.clone()),
            )
            .unwrap(),
        )
    };
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 4));
    let destination = Rc::new(Forwarding::new(signers[2].clone()));
    let local = Rc::new(Local(admission.clone()));
    let relay = Rc::new(Relay::new(
        paths.clone(),
        destination.clone(),
        local.clone(),
        admission.clone(),
        network(C),
    ));
    let server = server::PeerServer::for_test(
        io.clone(),
        destination,
        admission.clone(),
        local,
        relay,
        codec.clone(),
        signers[2].clone(),
    );
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
                Rc::new(uring_runtime::reactor::Descriptor::from(listener)),
                &scope,
            )
            .await?;
        server
            .serve_connection(
                crate::http::connection::from_accepted(fd, &admission)?,
                &scope,
            )
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
    let (signers, discovery) = identities();
    let sender = Forwarding::new(signers[0].clone());
    let destination = Forwarding::new(signers[2].clone());
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let (request, _) = sender
        .sign_request(subscribe(&admission, A, 1, 0, FetchMode::CopyOnly))
        .unwrap();
    let admitted = destination.verify_request(request).unwrap();
    let keys = &discovery[2].0;
    let mut replacement = crate::security::connection::signature_tests::mac_test_key(CACHE);
    for key in &mut replacement {
        key.key.id.0[4..12].copy_from_slice(&2u64.to_be_bytes());
        let (reference, state, mut material) = key.clone().into_installation();
        material[0] ^= 1;
        *key = crate::control::wire::CacheEncryptionKey::new(reference, state, material);
    }
    keys.install(KeyringBundle {
        schema_version: SCHEMA_VERSION,
        cluster: ClusterId(CLUSTER.into()),
        generation: BundleGeneration(2),
        peer_trust_roots: (*keys.peer_trust_roots().unwrap()).clone(),
        cache_keys: replacement,
    })
    .unwrap();
    assert!(
        destination
            .sign_response(admitted.binding(), PeerResponse::Miss)
            .is_err()
    );
}
use crate::control::wire::{BundleGeneration, KeyringBundle, SCHEMA_VERSION};
