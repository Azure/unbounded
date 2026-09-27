//! Production cut-through progress plus counterexamples to whole-page waiting.
use super::*;
use crate::peer::requester::PeerTransport;
use std::task::{Poll, Waker};

#[derive(Default)]
struct Observed {
    held: [usize; 2],
    waiting: [Vec<RequestId>; 2],
    first: [Vec<RequestId>; 2],
    wakes: Vec<Waker>,
}

struct OutputGate {
    side: usize,
    slots: usize,
    admission: Rc<Admission>,
    inner: Rc<requester::Requester>,
    observed: Rc<RefCell<Observed>>,
    deferred: bool,
}

struct OutputOwner<'a>(&'a OutputGate);
impl Drop for OutputOwner<'_> {
    fn drop(&mut self) {
        let wakes = {
            let mut state = self.0.observed.borrow_mut();
            state.held[self.0.side] -= 1;
            std::mem::take(&mut state.wakes)
        };
        for wake in wakes {
            wake.wake();
        }
    }
}

impl PeerTransport for OutputGate {
    fn exchange<'a>(
        &'a self,
        request: wire::SignedRequest,
        scope: &'a RequestScope,
    ) -> FutureResult<'a, wire::SignedResponse> {
        Box::pin(async move {
            if self.deferred {
                return self.inner.exchange(request, scope).await;
            }
            let cancellation = scope.cancellation.subscribe()?;
            let cache = &request.request.origin.object.cache;
            let mut registered = false;
            let charge = std::future::poll_fn(|cx| {
                cancellation.register(cx.waker());
                scope.check()?;
                let mut state = self.observed.borrow_mut();
                if state.held[self.side] == self.slots {
                    if !registered {
                        state.waiting[self.side].push(scope.request);
                        registered = true;
                    }
                    if !state.wakes.iter().any(|wake| wake.will_wake(cx.waker())) {
                        state.wakes.push(cx.waker().clone());
                    }
                    return Poll::Pending;
                }
                let charge =
                    self.admission
                        .reserve(Some(cache), ResourceClass::Ciphertext, P + 16)?;
                state.held[self.side] += 1;
                state.first[self.side].push(scope.request);
                Poll::Ready(Ok(charge))
            })
            .await?;
            let _owner = OutputOwner(self);
            let mut output = Some(charge);
            self.inner
                .exchange_reserved_for_test(request, scope, &mut output)
                .await
        })
    }
}

#[test]
#[ignore = "diagnose proposed transit pre-admission on fleet routes: run with --release"]
fn protected_transit_outputs_cycle_on_intersecting_fleet_paths() {
    for bytes in [4096, P] {
        for slots in [1, 2] {
            intersecting(slots, bytes, false, false, false);
        }
    }
}

#[test]
#[ignore = "response-ready receive experiment on fleet paths: run with --release"]
fn response_ready_transit_crosses_intersecting_fleet_paths() {
    for bytes in [4096, P] {
        for slots in [1, 2] {
            intersecting(slots, bytes, true, false, false);
        }
    }
}

#[test]
#[ignore = "diagnose response-ready reverse-buffer cycle: run with --release"]
fn response_ready_transit_cycles_with_saturated_reverse_buffers() {
    intersecting(1, P, true, true, false);
}

#[test]
#[ignore = "bounded cut-through feasibility on fleet routes: run with --release"]
fn cut_through_transit_progresses_with_saturated_reverse_page_capacity() {
    for bytes in [4096, P] {
        for slots in [1, 2] {
            intersecting(slots, bytes, true, true, true);
        }
    }
}

fn intersecting(
    slots: usize,
    bytes: usize,
    deferred: bool,
    reverse_pressure: bool,
    cut_through: bool,
) {
    intersecting_with_pressure(
        slots,
        bytes,
        deferred,
        reverse_pressure,
        cut_through,
        "none",
    );
}

#[test]
#[ignore = "cut-through scarce-resource unwind experiment: run with --release"]
fn cut_through_transit_unwinds_relay_and_connection_pressure() {
    for pressure in ["relay", "connection"] {
        intersecting_with_pressure(2, P, true, true, true, pressure);
    }
}

#[test]
#[ignore = "sustained signed transit and incoming keepalive arrivals: run with --release"]
fn streaming_load_reclaims_incoming_keepalives() {
    for bytes in [4096, P] {
        intersecting_with_pressure(2, bytes, true, false, true, "idle-churn");
    }
}

#[test]
#[ignore = "sustained same-page hotspot on fleet routes: run with --release"]
fn sustained_hotspot_uses_available_transit_edges() {
    intersecting_with_pressure(2, P, true, false, true, "hotspot");
}

#[test]
#[ignore = "sustained production Fill on fleet hotspot routes: run with --release"]
fn sustained_fills_use_available_transit_edges() {
    intersecting_with_pressure(2, P, true, false, true, "hotspot-fill");
}

struct HotspotIngress {
    next: NodeId,
    endpoint: Endpoint,
    auth: Rc<Forwarding>,
    transfers: Rc<transfer::Transfers>,
}
impl requester::PeerClient for HotspotIngress {
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
            self.auth.verify_response(response, &binding)
        })
    }
}

fn incoming_keepalive<'a>(
    node: &'a Node,
    source: &NodeId,
    destination: &NodeId,
    scope: &'a RequestScope,
    work: &mut FuturesUnordered<FutureResult<'a, ()>>,
    peers: &mut Vec<std::net::TcpStream>,
) {
    use std::io::Write;
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let mut peer = std::net::TcpStream::connect(listener.local_addr().unwrap()).unwrap();
    peer.set_write_timeout(Some(Duration::from_secs(1)))
        .unwrap();
    let (socket, _) = listener.accept().unwrap();
    let probe = crate::security::session::ChallengeProbe::new(source.clone(), destination.clone())
        .unwrap()
        .request_bytes();
    let body = base64::Engine::encode(&base64::engine::general_purpose::STANDARD, probe);
    peer.write_all(format!("POST /racer/peer/v1/challenge HTTP/1.1\r\ncontent-length: 0\r\nracer-probe: {body}\r\n\r\n").as_bytes()).unwrap();
    if let Ok(mut connection) = node.pool.accept(socket.into()) {
        work.push(Box::pin(async move {
            loop {
                connection = node.server.serve_connection(connection, scope).await?;
                if !connection.is_reusable() {
                    return Ok(());
                }
            }
        }));
    }
    peers.push(peer);
}

fn intersecting_with_pressure(
    slots: usize,
    bytes: usize,
    deferred: bool,
    reverse_pressure: bool,
    cut_through: bool,
    pressure: &str,
) {
    let hotspot = pressure.starts_with("hotspot");
    // Actual 1500-member radix-18 graph: both middle edges occur in production
    // shortest paths. Hotspot cases activate one additional detour peer.
    const BASE_POSITIONS: [usize; 6] = [3, 0, 1, 18, 19, 2];
    const HOTSPOT_POSITIONS: [usize; 7] = [3, 0, 1, 18, 19, 2, 333];
    let positions: &[usize] = if hotspot {
        &HOTSPOT_POSITIONS
    } else {
        &BASE_POSITIONS
    };
    let names: Vec<_> = (0..1500)
        .map(|i| format!("{:08x}-1111-4111-8111-111111111111", i + 1))
        .collect();
    let active_names: Vec<_> = positions.iter().map(|&i| names[i].as_str()).collect();
    let (signers, discovery) = named_identities(&active_names, 8192);
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
    let listeners: Vec<_> = positions
        .iter()
        .map(|_| {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            listener.set_nonblocking(true).unwrap();
            listener
        })
        .collect();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            names
                .iter()
                .enumerate()
                .map(|(i, name)| Member {
                    node: NodeId(name.clone()),
                    shares: NonZeroU32::new(1).unwrap(),
                    peer_endpoint: positions.iter().position(|&p| p == i).map_or_else(
                        || "127.0.0.1:9".into(),
                        |j| listeners[j].local_addr().unwrap().to_string(),
                    ),
                    rails: vec![],
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let placement = Placement::new(4096);
    let keys: Vec<_> = [3, 5]
        .iter()
        .map(|&destination| {
            (0..100000u32)
                .find_map(|n| {
                    let mut key = [0; 32];
                    key[..4].copy_from_slice(&n.to_le_bytes());
                    let key = CacheKey(key);
                    (placement
                        .rank(membership.clone(), &object(key), PageNumber(0))
                        .unwrap()
                        .ordered[0]
                        == *signers[destination].node())
                    .then_some(key)
                })
                .unwrap()
        })
        .collect();
    let data = Rc::new(Data {
        metadata_enabled: false,
        bytes: keys
            .iter()
            .map(|key| (*key, vec![key.0[0]; bytes]))
            .collect(),
        calls: RefCell::new(HashMap::new()),
    });
    let mut nodes: Vec<_> = (0..positions.len())
        .map(|i| {
            build_node_with_limits_adjustment(
                i,
                membership.clone(),
                signers[i].clone(),
                &discovery[i],
                data.clone(),
                if reverse_pressure && [1, 2].contains(&i) {
                    slots - 1
                } else {
                    6
                },
                2,
                128,
                false,
                |limits| {
                    if reverse_pressure && cut_through && [1, 2].contains(&i) {
                        limits.ciphertext_bytes = NonZeroUsize::new(P + 16 + 8 * 16384).unwrap();
                    }
                    if [1, 2].contains(&i) {
                        if pressure == "relay" {
                            limits.relay_transfers = NonZeroUsize::new(2).unwrap();
                        }
                        if pressure == "connection" {
                            limits.client_connections = NonZeroUsize::new(5).unwrap();
                        }
                    }
                },
            )
        })
        .collect();
    let observed = Rc::new(RefCell::new(Observed::default()));
    if reverse_pressure {
        for i in [1, 2] {
            nodes[i].transfers.wait_receive_for_test.set(true);
        }
    }
    for (side, i) in [1, 2].into_iter().enumerate() {
        let node = &nodes[i];
        let network = Rc::new(PeerNetwork::new(signers[i].node().clone(), 1).unwrap());
        network.install(membership.clone()).unwrap();
        let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 128, 150000));
        let auth = Rc::new(Forwarding::new(signers[i].clone()));
        let handshake = Rc::new(
            handshake::Handshake::new(signers[i].clone(), None)
                .with_http(network.clone(), node.transfers.clone())
                .with_discovery(
                    discovery[i].0.clone(),
                    discovery[i].1.clone(),
                    discovery[i].2.clone(),
                ),
        );
        let inner = Rc::new(
            requester::Requester::new(
                paths.clone(),
                Rc::new(Rails),
                auth.clone(),
                handshake.clone(),
                node.transfers.clone(),
            )
            .with_network(network.clone()),
        );
        let gate = Rc::new(OutputGate {
            side,
            slots,
            admission: node.admission.clone(),
            inner,
            observed: observed.clone(),
            deferred,
        });
        let relay = Rc::new(
            relay::Relay::new(paths, auth.clone(), gate, node.admission.clone())
                .with_network(network.clone())
                .with_handshake(handshake.clone()),
        );
        let io = Rc::new(HttpIo::with_admission(
            node.reactor.clone(),
            Codec::new(wire::MAX_ENVELOPE_HEAD, PAGE_BYTES + 16),
            node.admission.clone(),
        ));
        nodes[i].server = server::PeerServer::new(
            io,
            auth,
            node.admission.clone(),
            node.coordinator.clone(),
            relay,
        )
        .with_network(network)
        .with_wire(Rc::new(codec(&node.admission)))
        .with_handshake(handshake)
        .with_transfers(node.transfers.clone());
        nodes[i].server.legacy_relay_for_test = !cut_through;
    }
    let scope = RequestScope::new(
        RequestId([93; 16]),
        Instant::now()
            + Duration::from_secs(if pressure == "idle-churn" || hotspot {
                30
            } else {
                10
            }),
    )
    .unwrap();
    let mut endpoints: Vec<_> = nodes
        .iter()
        .map(|node| {
            node.owners
                .install(WorkerId(0), node.coordinator.clone())
                .unwrap()
        })
        .collect();
    let mut tick = |cx: &mut Context<'_>| {
        for (node, endpoint) in nodes.iter().zip(&mut endpoints) {
            node.transfers.poll_receive_for_test();
            node.server.poll_admission_deadlines();
            node.reactor.poll_budgeted(256).unwrap();
            node.engine.borrow_mut().poll_budgeted(64).unwrap();
            node.crypto.poll_budgeted(64).unwrap();
            endpoint.poll(cx, 64).unwrap();
            node.flights.poll_with_context(cx, 64).unwrap();
        }
        nodes[0].reactor.wait(Duration::from_micros(100)).unwrap();
    };
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut seed = Box::pin(async {
        for (side, destination) in [3, 5].into_iter().enumerate() {
            let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
            nodes[destination]
                .fill
                .acquire(
                    page(keys[side], 0),
                    membership.clone(),
                    &context(keys[side]),
                    &scope,
                    &mut budget,
                )
                .await
                .unwrap();
            nodes[destination].writer.discard_unsubmitted();
        }
    });
    while seed.as_mut().poll(&mut cx).is_pending() {
        scope.check().unwrap();
        tick(&mut cx);
    }
    drop(seed);
    let mut hot_connections = Vec::new();
    if hotspot {
        // Keep exactly the existing two slots on the canonical middle edge busy.
        // Other graph edges remain available; no quota is increased for detours.
        let endpoint = Endpoint::Peer(listeners[2].local_addr().unwrap().to_string());
        for _ in 0..2 {
            let mut checkout = nodes[1].pool.checkout(&endpoint, &scope);
            loop {
                match checkout.as_mut().poll(&mut cx) {
                    Poll::Ready(connection) => {
                        hot_connections.push(connection.unwrap());
                        break;
                    }
                    Poll::Pending => tick(&mut cx),
                }
                scope.check().unwrap();
            }
        }
    }
    let copies: Vec<_> = [3, 5]
        .iter()
        .enumerate()
        .map(|(side, &destination)| {
            nodes[destination]
                .memory
                .get(&page(keys[side], 0))
                .unwrap()
                .unwrap()
        })
        .collect();
    let calls = data.calls.borrow().clone();
    let paths = Paths::new(Rc::new(LinkHealth), 128, 150000);
    let churn = pressure == "idle-churn";
    let mut idle_peers = Vec::new();
    let mut idle_work: FuturesUnordered<FutureResult<'_, ()>> = FuturesUnordered::new();
    if churn {
        // Real completed challenge exchanges, then non-consuming peer idle waits.
        // Keep sending new arrivals after streams start, not just one batch.
        for _ in 0..58 {
            for i in [1, 2] {
                incoming_keepalive(
                    &nodes[i],
                    signers[0].node(),
                    signers[i].node(),
                    &scope,
                    &mut idle_work,
                    &mut idle_peers,
                );
            }
        }
    }
    if churn {
        for _ in 0..64 {
            while let Poll::Ready(Some(_)) = idle_work.poll_next_unpin(&mut cx) {}
            tick(&mut cx);
        }
    }
    let mut requests = FuturesUnordered::new();
    let rounds = if churn || hotspot { 12 } else { 1 };
    for (side, (source, first, destination)) in [(0, 1, 3), (4, 2, 5)].into_iter().enumerate() {
        for index in 0..slots {
            let node = &nodes[source];
            let request_scope = RequestScope::new(
                RequestId([side as u8 * 16 + index as u8; 16]),
                scope.deadline.0,
            )
            .unwrap();
            let attempt = AttemptId([index as u8 + 1; 16]);
            let request = PeerRequest {
                operation: Operation::Page {
                    page: page(keys[side], 0),
                    mode: FetchMode::CopyOnly,
                },
                origin: node
                    .credentials
                    .seal(&context(keys[side]), attempt, &request_scope)
                    .unwrap(),
                route: RouteBudget {
                    membership: membership.version,
                    request: request_scope.request,
                    attempt,
                    destination: signers[destination].node().clone(),
                    visited: vec![signers[source].node().clone()],
                    remaining_links: 4,
                    remaining_attempts: 0,
                    deadline: request_scope.deadline,
                },
            };
            let route = paths
                .shortest(
                    membership.clone(),
                    signers[first].node(),
                    &crate::peer::search_budget(&request.route, signers[first].node()).unwrap(),
                )
                .unwrap();
            let expected = if side == 0 { [0, 1, 18] } else { [1, 0, 2] };
            assert_eq!(
                route.nodes,
                expected.map(|i| NodeId(names[i].clone())).to_vec()
            );
            let source_auth = Forwarding::new(signers[source].clone());
            let endpoint = Endpoint::Peer(listeners[first].local_addr().unwrap().to_string());
            let first = signers[first].node().clone();
            let copies = &copies;
            let key = keys[side];
            let signer = signers[source].clone();
            let membership = membership.clone();
            requests.push(async move {
                let mut dependencies = node.fill.dependencies_for_test();
                let peers = Rc::new(HotspotIngress {
                    next: first.clone(),
                    endpoint: endpoint.clone(),
                    auth: Rc::new(Forwarding::new(signer.clone())),
                    transfers: node.transfers.clone(),
                });
                dependencies.peers = peers.clone();
                dependencies.candidates = Rc::new(CandidatePolicy::new(
                    signer.node().clone(),
                    Rc::new(Placement::new(128)),
                    peers,
                ));
                let fill = Fill::new(dependencies);
                for round in 0..rounds {
                    if hotspot {
                        let arrival = Instant::now() + Duration::from_millis(20);
                        std::future::poll_fn(|cx| {
                            if Instant::now() >= arrival {
                                Poll::Ready(())
                            } else {
                                cx.waker().wake_by_ref();
                                Poll::Pending
                            }
                        })
                        .await;
                    }
                    if pressure == "hotspot-fill" {
                        let mut budget = AcquisitionBudget::new(request_scope.deadline.0, 8, 16);
                        let result = fill
                            .acquire(
                                page(key, 0),
                                membership.clone(),
                                &context(key),
                                &request_scope,
                                &mut budget,
                            )
                            .await?;
                        assert_eq!(result.plaintext.bytes(), vec![key.0[0]; bytes]);
                        drop(result);
                        node.writer.discard_unsubmitted();
                        node.memory.evict_idle(usize::MAX).unwrap();
                    }
                    // Fresh signed attempts under one unchanged scope; these are
                    // independent arrivals, not retries of failed transmissions.
                    let attempt = AttemptId([round as u8 + 32 * index as u8; 16]);
                    let mut route = request.route.clone();
                    route.attempt = attempt;
                    let request = PeerRequest {
                        operation: Operation::Page {
                            page: page(key, 0),
                            mode: FetchMode::CopyOnly,
                        },
                        route,
                        origin: node
                            .credentials
                            .seal(&context(key), attempt, &request_scope)?,
                    };
                    let (signed, binding) = source_auth.sign_request_to(request, &first)?;
                    let response = node
                        .transfers
                        .exchange(endpoint.clone(), signed, &request_scope)
                        .await?;
                    let response = source_auth
                        .verify_response(response, &binding)?
                        .into_signed();
                    if round + 1 == rounds {
                        return Ok(response);
                    }
                    let PeerResponse::Page { ciphertext, .. } = response.response else {
                        panic!("sustained stream failed at round {round}");
                    };
                    let expected = copies
                        .iter()
                        .find(|copy| copy.ciphertext.envelope().page == ciphertext.envelope().page)
                        .unwrap();
                    assert_eq!(ciphertext.bytes(), expected.ciphertext.bytes());
                }
                unreachable!()
            });
        }
    }
    let servers = async {
        let mut all = FuturesUnordered::new();
        for (node, listener) in nodes.iter().zip(&listeners) {
            let fd = Rc::new(OwnedFd::from(listener.try_clone().unwrap()));
            let scope = &scope;
            all.push(async move {
                let mut active: FuturesUnordered<FutureResult<'_,()>>=FuturesUnordered::new();
                loop {
                    let accept=node.reactor.accept(fd.clone(),scope); futures::pin_mut!(accept);
                    let accepted=loop {
                        if active.is_empty() { break accept.await; }
                        use futures::FutureExt;
                        futures::select_biased! { _=active.next().fuse()=>{}, result=accept.as_mut().fuse()=>break result }
                    }?;
                    let mut connection=match node.pool.accept(accepted) {
                        Ok(connection)=>connection,
                        Err(Error::Overloaded) if pressure != "none" => continue,
                        Err(error)=>return Err(error),
                    };
                    active.push(Box::pin(async move { loop {
                        connection=node.server.serve_connection(connection,scope).await?;
                        if !connection.is_reusable() { return Ok(()); }
                    }}));
                }
                #[allow(unreachable_code)] Ok::<(),Error>(())
            });
        }
        all.next().await.unwrap()
    };
    let mut servers = Box::pin(servers);
    if reverse_pressure && !cut_through {
        let end = Instant::now() + Duration::from_secs(3);
        loop {
            assert!(
                requests.poll_next_unpin(&mut cx).is_pending(),
                "response unexpectedly completed"
            );
            assert!(servers.as_mut().poll(&mut cx).is_pending());
            tick(&mut cx);
            if [1, 2].iter().all(|&i| {
                !nodes[i]
                    .transfers
                    .receive_waits_for_test
                    .borrow()
                    .is_empty()
            }) {
                break;
            }
            assert!(Instant::now() < end, "reverse cycle not reached");
        }
        for _ in 0..64 {
            assert!(requests.poll_next_unpin(&mut cx).is_pending());
            assert!(servers.as_mut().poll(&mut cx).is_pending());
            tick(&mut cx);
        }
        for i in [1, 2] {
            assert_eq!(nodes[i].admission.used(ResourceClass::Ciphertext), P + 16);
            assert_eq!(nodes[i].admission.used(ResourceClass::Flight), 0);
            eprintln!(
                "reverse response cycle: relay={i} bytes={} waits={:?}",
                nodes[i].admission.used(ResourceClass::Ciphertext),
                nodes[i].transfers.receive_waits_for_test.borrow()
            );
        }
    } else if deferred {
        let end = Instant::now() + Duration::from_secs(if churn || hotspot { 25 } else { 3 });
        let started = Instant::now();
        let mut arrivals = 0;
        let mut complete = 0;
        let mut failed = 0;
        while complete < 2 * slots {
            if churn {
                // 200 completed-keepalive arrivals/s per relay throughout the
                // run, while four clients issue successive pages.
                if started.elapsed() >= Duration::from_millis(arrivals * 5) {
                    for i in [1, 2] {
                        incoming_keepalive(
                            &nodes[i],
                            signers[0].node(),
                            signers[i].node(),
                            &scope,
                            &mut idle_work,
                            &mut idle_peers,
                        );
                    }
                    arrivals += 1;
                }
                while let Poll::Ready(Some(_)) = idle_work.poll_next_unpin(&mut cx) {}
            }
            while let Poll::Ready(Some(result)) = requests.poll_next_unpin(&mut cx) {
                let response = match result {
                    Ok(response) => response,
                    Err(error) if pressure != "none" && !churn && !hotspot => {
                        assert!(
                            matches!(error, Error::Io | Error::Overloaded | Error::Unavailable),
                            "{error:?}"
                        );
                        complete += 1;
                        failed += 1;
                        continue;
                    }
                    Err(error) => panic!("unexpected cut-through failure: {error:?}"),
                };
                let PeerResponse::Page { ciphertext, .. } = response.response else {
                    if pressure != "none"
                        && !churn
                        && !hotspot
                        && matches!(
                            response.response,
                            PeerResponse::Overloaded | PeerResponse::Unavailable
                        )
                    {
                        complete += 1;
                        failed += 1;
                        continue;
                    }
                    panic!("response-ready path returned a non-page");
                };
                assert_eq!(ciphertext.bytes().len(), bytes + 16);
                let expected = copies
                    .iter()
                    .find(|copy| copy.ciphertext.envelope().page == ciphertext.envelope().page)
                    .unwrap();
                assert_eq!(ciphertext.bytes(), expected.ciphertext.bytes());
                complete += 1;
            }
            assert!(servers.as_mut().poll(&mut cx).is_pending());
            tick(&mut cx);
            assert!(Instant::now() < end, "response-ready receive stalled");
        }
        assert_eq!(*data.calls.borrow(), calls);
        if churn {
            assert!(arrivals >= 12, "sustained arrivals were not exercised");
            eprintln!(
                "sustained arrivals={} rounds={} elapsed={:?}",
                arrivals,
                rounds,
                started.elapsed()
            );
        }
        if pressure != "none" && !churn && !hotspot {
            assert!(failed > 0, "pressure was not exercised");
        }
        eprintln!(
            "response-ready complete: cut_through={cut_through} pressure={pressure} failures={failed} bytes={bytes} concurrent={}",
            2 * slots
        );
    } else {
        let end = Instant::now() + Duration::from_secs(3);
        loop {
            assert!(
                requests.poll_next_unpin(&mut cx).is_pending(),
                "unexpected progress or failure before cycle"
            );
            assert!(servers.as_mut().poll(&mut cx).is_pending());
            tick(&mut cx);
            let state = observed.borrow();
            if state.waiting.iter().all(|waiters| waiters.len() == slots) {
                break;
            }
            assert!(
                Instant::now() < end,
                "cycle not reached: held={:?}, waiting={:?}",
                state.held,
                state.waiting
            );
        }
        // No work completes despite driving every production service again. These
        // are parked dependencies, not a snapshot between runnable completions.
        for _ in 0..64 {
            assert!(requests.poll_next_unpin(&mut cx).is_pending());
            assert!(servers.as_mut().poll(&mut cx).is_pending());
            tick(&mut cx);
            assert_eq!(observed.borrow().held, [slots, slots]);
        }
        {
            let state = observed.borrow();
            assert_eq!(state.held, [slots, slots]);
            for side in 0..2 {
                assert!(
                    state.waiting[side]
                        .iter()
                        .all(|id| state.first[1 - side].contains(id))
                );
                let admission = &nodes[side + 1].admission;
                assert_eq!(admission.used(ResourceClass::Ciphertext), slots * (P + 16));
                assert_eq!(admission.used(ResourceClass::Relay), 2 * slots);
                assert_eq!(
                    admission.used(ResourceClass::Flight),
                    0,
                    "no local Fill holds transit capacity"
                );
            }
            eprintln!(
                "proposed gate cycle: bytes={bytes}, slots={slots}, held={:?}, first={:?}, waiting={:?}",
                state.held, state.first, state.waiting
            );
        }
        assert_eq!(
            *data.calls.borrow(),
            calls,
            "destinations already hold pages; no additional origin work"
        );
    }
    scope.cancel().unwrap();
    drop(servers);
    drop(requests);
    drop(hot_connections);
    drop(idle_work);
    drop(idle_peers);
    drop(copies);
    for node in &nodes {
        node.pool.close();
    }
    let mut drain = Box::pin(async {
        for node in &nodes {
            node.reactor.drain().await.unwrap();
        }
    });
    let end = Instant::now() + Duration::from_secs(2);
    while drain.as_mut().poll(&mut cx).is_pending() {
        assert!(Instant::now() < end);
        tick(&mut cx);
    }
    assert_eq!(observed.borrow().held, [0, 0]);
    for node in &nodes {
        node.writer.discard_unsubmitted();
        node.memory.evict_idle(usize::MAX).unwrap();
        for class in [
            ResourceClass::Ciphertext,
            ResourceClass::Plaintext,
            ResourceClass::Connection,
            ResourceClass::Relay,
            ResourceClass::Flight,
            ResourceClass::Waiter,
        ] {
            assert_eq!(node.admission.used(class), 0, "{class:?}");
        }
    }
}
