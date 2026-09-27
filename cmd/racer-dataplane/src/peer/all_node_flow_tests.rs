//! Simultaneous local Fill and transit on the same production peer workers.
use super::*;
use crate::peer::requester::PeerClient;

struct CrossingIngress {
    next: NodeId,
    endpoint: Endpoint,
    auth: Rc<Forwarding>,
    transfers: Rc<transfer::Transfers>,
}
impl PeerClient for CrossingIngress {
    fn request<'a>(
        &'a self,
        request: PeerRequest,
        scope: &'a RequestScope,
    ) -> FutureResult<'a, wire::VerifiedResponse> {
        Box::pin(async move { self.request_reserved(request, scope, &mut None).await })
    }
    fn request_reserved<'a>(
        &'a self,
        request: PeerRequest,
        scope: &'a RequestScope,
        output: &'a mut Option<Reservation>,
    ) -> FutureResult<'a, wire::VerifiedResponse> {
        Box::pin(async move {
            let (signed, binding) = self.auth.sign_request_to(request, &self.next)?;
            let response = self
                .transfers
                .exchange_reserved(
                    self.endpoint.clone(),
                    signed,
                    crate::topology::rails::TransportPlan::Http,
                    scope,
                    output,
                )
                .await?;
            let response = self.auth.verify_response(response, &binding)?;
            assert!(
                !matches!(response.response(), PeerResponse::Overloaded),
                "request={:?}: opposite ingress relay rejected a retained page while local Fills hold its receive capacity",
                scope.request,
            );
            Ok(response)
        })
    }
}

#[test]
#[ignore = "simultaneous production Fill/transit regression: run with --release"]
fn crossing_fills_leave_transit_progress() {
    for bytes in [4096, P] {
        crossing(bytes, false);
    }
}

#[test]
#[ignore = "corrupt streamed ciphertext through production Fill: run with --release"]
fn crossing_fills_reject_corrupt_streamed_ciphertext() {
    crossing(4096, true);
}

fn crossing(bytes: usize, corrupt: bool) {
    const FLOWS: usize = 7;
    let (signers, discovery) =
        named_identities(&[A, B, C, "00000004-1111-4111-8111-111111111111"], 8192);
    for signer in &signers {
        for peer in &signers {
            signer
                .configure_authenticated_peer_challenge(
                    peer.node().clone(),
                    peer.challenge().unwrap(),
                )
                .unwrap();
        }
    }
    for (keys, _, _) in &discovery {
        let mut bundle: serde_json::Value =
            serde_json::from_slice(include_bytes!("../control/testdata/bundle.json")).unwrap();
        bundle["cluster"] = CLUSTER.into();
        bundle["generation"] = "2".into();
        for (i, key) in bundle["cache_keys"]
            .as_array_mut()
            .unwrap()
            .iter_mut()
            .enumerate()
        {
            key["cache"] = CACHE.into();
            key["material"] = base64::Engine::encode(
                &base64::engine::general_purpose::STANDARD,
                [i as u8 + 7; 32],
            )
            .into();
        }
        bundle["peer_trust_roots"] = serde_json::to_value(
            keys.peer_trust_roots()
                .unwrap()
                .iter()
                .map(|root| {
                    base64::Engine::encode(&base64::engine::general_purpose::STANDARD, root)
                })
                .collect::<Vec<_>>(),
        )
        .unwrap();
        keys.install(
            crate::control::wire::decode_bundle(&serde_json::to_vec(&bundle).unwrap()).unwrap(),
        )
        .unwrap();
    }
    let listeners: Vec<_> = (0..4)
        .map(|_| {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            listener.set_nonblocking(true).unwrap();
            listener
        })
        .collect();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            signers
                .iter()
                .zip(&listeners)
                .map(|(signer, listener)| Member {
                    node: signer.node().clone(),
                    shares: NonZeroU32::new(1).unwrap(),
                    peer_endpoint: listener.local_addr().unwrap().to_string(),
                    rails: vec![],
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let placement = Placement::new(256);
    let keys: Vec<Vec<_>> = (0..2)
        .map(|source| {
            (0..10000u32)
                .filter_map(|n| {
                    let mut key = [0; 32];
                    key[..4].copy_from_slice(&n.to_le_bytes());
                    key[4] = source as u8;
                    let key = CacheKey(key);
                    let rank = placement
                        .rank(membership.clone(), &object(key), PageNumber(0))
                        .unwrap();
                    (rank.ordered[0] == *signers[source + 2].node()
                        && !rank.ordered.contains(signers[source].node()))
                    .then_some(key)
                })
                .take(FLOWS)
                .collect()
        })
        .collect();
    assert!(keys.iter().all(|keys| keys.len() == FLOWS));
    let data = Rc::new(Data {
        metadata_enabled: false,
        bytes: keys
            .iter()
            .flatten()
            .map(|key| (*key, vec![key.0[0]; bytes]))
            .collect(),
        calls: RefCell::new(HashMap::new()),
    });
    let nodes: Vec<_> = (0..4)
        .map(|i| {
            build_node_with_limits_adjustment(
                i,
                membership.clone(),
                signers[i].clone(),
                &discovery[i],
                data.clone(),
                if i < 2 { 6 } else { 31 },
                2,
                128,
                false,
                |limits| {
                    if i < 2 {
                        limits.ciphertext_bytes = NonZeroUsize::new(128 * 1024 * 1024).unwrap();
                    }
                },
            )
        })
        .collect();
    let mut endpoints: Vec<_> = nodes
        .iter()
        .map(|node| {
            node.owners
                .install(WorkerId(0), node.coordinator.clone())
                .unwrap()
        })
        .collect();
    let scope = RequestScope::new(
        RequestId([91; 16]),
        Instant::now() + Duration::from_secs(15),
    )
    .unwrap();
    let mut drive = |mut work: std::pin::Pin<Box<dyn Future<Output = ()> + '_>>| {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        loop {
            if work.as_mut().poll(&mut cx).is_ready() {
                break;
            }
            scope.check().unwrap();
            for (node, endpoint) in nodes.iter().zip(&mut endpoints) {
                node.server.poll_admission_deadlines();
                node.reactor.poll_budgeted(256).unwrap();
                node.engine.borrow_mut().poll_budgeted(64).unwrap();
                node.crypto.poll_budgeted(64).unwrap();
                endpoint.poll(&mut cx, 64).unwrap();
                node.flights.poll_with_context(&mut cx, 64).unwrap();
            }
            nodes[0].reactor.wait(Duration::from_micros(100)).unwrap();
        }
    };
    drive(Box::pin(async {
        for source in 0..2 {
            for key in &keys[source] {
                let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
                nodes[source + 2]
                    .fill
                    .acquire(
                        page(*key, 0),
                        membership.clone(),
                        &context(*key),
                        &scope,
                        &mut budget,
                    )
                    .await
                    .unwrap();
                nodes[source + 2].writer.discard_unsubmitted();
            }
        }
    }));
    if corrupt {
        for source in 0..2 {
            let node = &nodes[source + 2];
            for key in &keys[source] {
                node.memory.corrupt_ciphertext_for_test(&page(*key, 0));
            }
        }
    }
    // Both ingress nodes run actual Fill but select the opposite ingress as their
    // first hop. The next hop, relay, destination, crypto and I/O remain production.
    let fills: Vec<_> = (0..2)
        .map(|source| {
            let mut dependencies = nodes[source].fill.dependencies_for_test();
            let peers = Rc::new(CrossingIngress {
                next: signers[1 - source].node().clone(),
                endpoint: Endpoint::Peer(membership.members()[1 - source].peer_endpoint.clone()),
                auth: Rc::new(Forwarding::new(signers[source].clone())),
                transfers: nodes[source].transfers.clone(),
            });
            dependencies.peers = peers.clone();
            dependencies.candidates = Rc::new(CandidatePolicy::new(
                signers[source].node().clone(),
                Rc::new(Placement::new(128)),
                peers,
            ));
            Fill::new(dependencies)
        })
        .collect();
    let mut requests = FuturesUnordered::new();
    for source in 0..2 {
        for (index, key) in keys[source].iter().enumerate() {
            let fill = &fills[source];
            let membership = membership.clone();
            let mut id = [0; 16];
            id[0] = source as u8;
            id[1] = index as u8;
            let request_scope = RequestScope::new(RequestId(id), scope.deadline.0).unwrap();
            requests.push(async move {
                let mut budget = AcquisitionBudget::new(request_scope.deadline.0, 16, 24);
                let result = fill
                    .acquire(
                        page(*key, 0),
                        membership,
                        &context(*key),
                        &request_scope,
                        &mut budget,
                    )
                    .await;
                if corrupt {
                    assert!(
                        matches!(result, Err(Error::CorruptRecord)),
                        "corrupt stream: {:?}",
                        result.as_ref().err()
                    );
                    return;
                }
                assert!(
                    result.is_ok(),
                    "source={source} request={id:?}: {:?}",
                    result.as_ref().err()
                );
                assert_eq!(result.unwrap().plaintext.bytes(), vec![key.0[0]; bytes]);
            });
        }
    }
    // Start all local acquisitions before accepting remote work, as simultaneous
    // load on independently scheduled fleet workers can do.
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(requests.poll_next_unpin(&mut cx).is_pending());
    for _ in 0..4 {
        crate::read::drivers::poll(&mut cx, 64);
    }
    for node in nodes.iter().take(2) {
        assert_eq!(
            node.admission.used(ResourceClass::Ciphertext),
            FLOWS * (P + 16)
        );
        assert_eq!(node.admission.used(ResourceClass::Flight), FLOWS);
        assert!(node.pool.peer_waits.get() >= FLOWS - 2);
    }
    let servers = async {
        let mut all = FuturesUnordered::new();
        for (node, listener) in nodes.iter().zip(&listeners) {
            let fd = Rc::new(OwnedFd::from(listener.try_clone().unwrap()));
            let scope = &scope;
            all.push(async move {
                let mut active: FuturesUnordered<FutureResult<'_, ()>> = FuturesUnordered::new();
                loop {
                    let accept = node.reactor.accept(fd.clone(),scope);
                    futures::pin_mut!(accept);
                    let accepted = loop {
                        if active.is_empty() { break accept.await; }
                        use futures::FutureExt;
                        futures::select_biased! { _ = active.next().fuse()=>{}, result=accept.as_mut().fuse()=>break result }
                    }?;
                    let mut connection = node.pool.accept(accepted)?;
                    active.push(Box::pin(async move { loop {
                        connection = node.server.serve_connection(connection,scope).await?;
                        if !connection.is_reusable() { return Ok(()); }
                    }}));
                }
                #[allow(unreachable_code)] Ok::<(),Error>(())
            });
        }
        all.next().await.unwrap()
    };
    drive(Box::pin(async {
        let clients = async { while requests.next().await.is_some() {} };
        futures::pin_mut!(clients, servers);
        match futures::future::select(clients, servers).await {
            futures::future::Either::Left(_) => {}
            futures::future::Either::Right((result, _)) => panic!("peer server: {result:?}"),
        }
    }));
    for node in &nodes {
        node.pool.close();
        node.writer.discard_unsubmitted();
    }
    drive(Box::pin(async {
        for node in &nodes {
            node.reactor.drain().await.unwrap();
        }
    }));
    for node in &nodes {
        node.memory.evict_idle(usize::MAX).unwrap();
        for class in [
            ResourceClass::Plaintext,
            ResourceClass::Ciphertext,
            ResourceClass::Flight,
            ResourceClass::Waiter,
            ResourceClass::Relay,
            ResourceClass::Connection,
        ] {
            assert_eq!(node.admission.used(class), 0, "{class:?}");
        }
    }
}
