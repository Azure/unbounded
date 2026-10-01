//! Targeted regressions using the same assembled multi-node harness as the corpus.
use super::*;

#[test]
fn worker_subscriptions_contend_across_servers_and_recover_after_release() {
    use crate::{
        model::OriginContext,
        peer::{
            protocol::{FetchMode, Operation as PeerOperation, PeerRequest, PeerResponse},
            subscriptions::{Demand, PageInterval, Subscription},
        },
        topology::routing::RouteBudget,
    };

    let sim = Simulation::new();
    let _os = sim.enter();
    let clock = SimulationClock::new(73);
    let _time = clock.environment(0).enter();
    let _strict = crate::runtime::environment::require_simulated();
    let mut harness = Harness::new(73, sim, clock, false);
    harness.add(None);
    harness.add(None);
    let target = harness
        .nodes
        .iter()
        .position(|n| n.workers.len() == 2)
        .unwrap();
    let source = 1 - target;
    let receiver = &harness.nodes[target];
    let servers = receiver
        .workers
        .iter()
        .map(|w| w.app.peers.clone())
        .collect::<Vec<_>>();
    let membership = receiver.workers[0]
        .app
        .snapshots
        .current()
        .unwrap()
        .membership
        .clone();
    let destination = receiver.config.node.clone();
    let sender = &harness.nodes[source];
    let keys = sender.workers[0].app.keys.clone();
    let forwarding = Forwarding::new(Rc::new(Signatures::new(
        keys.clone(),
        Rc::new(Certificates::new(
            sender.config.cluster.clone(),
            keys.clone(),
        )),
    )));
    let credentials = CredentialCrypto::new(keys, sender.workers[0].runtime.admission.clone());
    let scope = scope(Duration::from_secs(5)).unwrap();
    let object = ObjectId {
        cache: harness.definition(0).id,
        key: CacheKey([0; 32]),
    };
    let signed = |sequence: u8| {
        let attempt = AttemptId([sequence + 1; 16]);
        let request = PeerRequest {
            operation: PeerOperation::Subscribe {
                subscription: Subscription {
                    id: [42; 16],
                    sequence: sequence.into(),
                    version: ObjectVersion {
                        object: object.clone(),
                        etag: StrongEtag::test_value("absent"),
                    },
                    demand: Demand::new(vec![PageInterval { start: 0, end: 1 }]).unwrap(),
                    page_budget: 3 - u32::from(sequence),
                    byte_budget: PAGE_BYTES + 16,
                },
                mode: FetchMode::CopyOnly,
            },
            origin: credentials
                .seal(
                    &OriginContext {
                        object: object.clone(),
                        metadata: None,
                        authorization: None,
                    },
                    attempt,
                    &scope,
                )
                .unwrap(),
            route: RouteBudget {
                membership: membership.version,
                request: scope.request,
                attempt,
                destination: destination.clone(),
                visited: vec![sender.config.node.clone()],
                remaining_links: 4,
                remaining_attempts: 0,
                deadline: scope.deadline,
            },
        };
        forwarding.sign_request_to(request, &destination).unwrap()
    };
    let (first, _) = signed(0);
    let (contending, binding) = signed(1);
    let (retry, retry_binding) = signed(2);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    // Do not tick the owner yet: the first server holds a live selection while
    // its real WorkerDirectory dispatch waits in the selected worker's mailbox.
    let mut held = servers[0].dispatch(first, &scope);
    assert!(held.as_mut().poll(&mut cx).is_pending());
    let mut rejected = servers[1].dispatch(contending, &scope);
    let Poll::Ready(Ok(response)) = rejected.as_mut().poll(&mut cx) else {
        panic!("second server must reject the same live subscription");
    };
    assert!(matches!(
        forwarding
            .verify_response(response, &binding)
            .unwrap()
            .response(),
        PeerResponse::Overloaded
    ));
    drop(rejected);
    drop(held);

    let mut recovered = servers[1].dispatch(retry, &scope);
    assert!(
        recovered.as_mut().poll(&mut cx).is_pending(),
        "released selection admits the other worker"
    );
    let response = (0..100)
        .find_map(|_| {
            harness.tick();
            match recovered.as_mut().poll(&mut cx) {
                Poll::Ready(result) => Some(result.unwrap()),
                Poll::Pending => None,
            }
        })
        .expect("accepted copy-only request completes through its owner");
    assert!(matches!(
        forwarding
            .verify_response(response, &retry_binding)
            .unwrap()
            .response(),
        PeerResponse::Miss
    ));
}

#[test]
fn completed_peer_dispatches_do_not_exhaust_worker_cancellation() {
    use crate::model::{MetadataSelector, OriginContext};
    use crate::peer::protocol::{FetchMode, Operation as PeerOperation, PeerRequest, PeerResponse};
    use crate::topology::routing::RouteBudget;
    use futures::{Stream, stream::FuturesUnordered};

    let sim = Simulation::new();
    let _os = sim.enter();
    let clock = SimulationClock::new(73);
    let environment = clock.environment(0);
    let _time = environment.enter();
    let _strict = crate::runtime::environment::require_simulated();
    let mut harness = Harness::new(73, sim, clock, false);
    harness.add(None);
    harness.add(None);
    let target = harness
        .nodes
        .iter()
        .position(|n| n.workers.len() == 2)
        .unwrap();
    let source = 1 - target;
    let app = &harness.nodes[target].workers[0].app;
    let mut object = ObjectId {
        cache: harness.definition(0).id,
        key: CacheKey([0; 32]),
    };
    while app.directory.metadata_owner(&object).unwrap() != WorkerId(1) {
        object.key.0[0] += 1;
    }
    let server = app.peers.clone();
    // Exactly the long-lived scope used by WorkerApplication's ingress loop.
    let worker_scope = app.task_scope.clone().unwrap();
    let membership = app.snapshots.current().unwrap().membership.clone();
    let destination = harness.nodes[target].config.node.clone();
    let sender = &harness.nodes[source].workers[0].app;
    let signatures = Rc::new(Signatures::new(
        sender.keys.clone(),
        Rc::new(Certificates::new(
            harness.nodes[source].config.cluster.clone(),
            sender.keys.clone(),
        )),
    ));
    let forwarding = Forwarding::new(signatures);
    let credentials = CredentialCrypto::new(sender.keys.clone(), sender.runtime.admission.clone());
    let previous = harness.nodes[source].config.node.clone();
    for round in 0u64..1100 {
        let scope = RequestScope::new(
            RequestId([1; 16]),
            crate::runtime::environment::now() + Duration::from_secs(5),
        )
        .unwrap();
        let mut attempt = [0; 16];
        attempt[..8].copy_from_slice(&round.to_le_bytes());
        let attempt = AttemptId(attempt);
        let context = OriginContext {
            object: object.clone(),
            metadata: None,
            authorization: None,
        };
        let request = PeerRequest {
            operation: PeerOperation::Metadata {
                object: object.clone(),
                selector: MetadataSelector::Fresh,
                mode: FetchMode::CopyOnly,
            },
            origin: credentials.seal(&context, attempt, &scope).unwrap(),
            route: RouteBudget {
                membership: membership.version,
                request: scope.request,
                attempt,
                destination: destination.clone(),
                visited: vec![previous.clone()],
                remaining_links: 4,
                remaining_attempts: 0,
                deadline: scope.deadline,
            },
        };
        let (request, binding) = forwarding.sign_request_to(request, &destination).unwrap();
        let mut effective = worker_scope.clone();
        effective.request = scope.request;
        effective.deadline = scope.deadline;
        // FuturesUnordered gives each completed ingress its own task waker, just
        // like the production ingress collection. No synthetic registrations.
        let server = server.clone();
        let mut pending = FuturesUnordered::new();
        pending.push(Box::pin(async move {
            server.dispatch(request, &effective).await
        }));
        let mut response = None;
        for _ in 0..100 {
            {
                let app = &harness.nodes[target].workers[0].app;
                let _local = app
                    .directory
                    .simulation_scope(Some((app.worker, app.coordinator.clone())));
                let _drivers = app.drivers.enter();
                if let Poll::Ready(Some(result)) = std::pin::Pin::new(&mut pending)
                    .poll_next(&mut Context::from_waker(futures::task::noop_waker_ref()))
                {
                    response = Some(result.unwrap());
                    break;
                }
            }
            harness.tick();
        }
        let verified = forwarding
            .verify_response(response.expect("bounded peer dispatch"), &binding)
            .unwrap();
        assert!(
            matches!(verified.response(), PeerResponse::Miss),
            "round {round}: expected Miss; overloaded={}",
            matches!(verified.response(), PeerResponse::Overloaded)
        );
        for worker in &harness.nodes[target].workers {
            assert_eq!(worker.runtime.admission.used(ResourceClass::Waiter), 0);
            assert_eq!(worker.runtime.admission.used(ResourceClass::Flight), 0);
            assert_eq!(worker.app.drivers.pending(), 0);
        }
    }
    assert!(worker_scope.check().is_ok());
}

#[test]
fn healthy_relayed_page_reads() {
    healthy_relayed_page_reads_with_mode(false);
}

#[test]
fn listeners_survive_repeated_reactor_queue_pressure() {
    let sim = Simulation::new();
    let _os = sim.enter();
    let clock = SimulationClock::new(73);
    let environment = clock.environment(0);
    let _time = environment.enter();
    let _strict = crate::runtime::environment::require_simulated();
    let mut harness = Harness::new(73, sim.clone(), clock.clone(), false);
    harness.add(None);
    // The DST harness bypasses control enrollment and already attaches diagnostic
    // resources; install the same service task as start_diagnostics.
    let app = &mut harness.nodes[0].workers[0].app;
    let diagnostic_scope = scope(Duration::from_secs(30)).unwrap();
    app.diagnostic_scope = Some(diagnostic_scope.clone());
    let telemetry = app.telemetry.clone();
    let listen = app.diagnostics_address;
    app.diagnostic_task = Some(Box::pin(async move {
        telemetry.serve(listen, &diagnostic_scope).await
    }));
    harness.tick();
    let reactor = harness.nodes[0].workers[0].runtime.reactor.clone();
    let limit = harness.nodes[0].config.limits.queue_entries.get();
    let address = SocketAddress::Inet(harness.nodes[0].config.diagnostics_listen);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for round in 0..4 {
        // Complete the existing diagnostic accept, then fill the ordinary
        // partition before the service consumes that CQE and submits its receive.
        let probe = sim.connect(address.clone()).unwrap();
        let peer = sim
            .connect(SocketAddress::Inet(harness.nodes[0].config.peer_listen))
            .unwrap();
        reactor.poll_budgeted(128).unwrap();
        reactor.poll_budgeted(128).unwrap();
        let pressure_scope = scope(Duration::from_secs(30)).unwrap();
        let (reader, _writer) = sim.socket_pair();
        let reader = Rc::new(reader);
        let mut pressure = Vec::new();
        loop {
            let mut wait = reactor.readiness(reader.clone(), libc::POLLIN as u32, &pressure_scope);
            match wait.as_mut().poll(&mut cx) {
                Poll::Pending => pressure.push(wait),
                Poll::Ready(Err(Error::Overloaded)) => break,
                _ => panic!("unexpected pressure result"),
            }
        }
        assert!(!pressure.is_empty());
        let ordinary_in_flight = reactor.in_flight();
        assert!(ordinary_in_flight < limit);
        for _ in 0..32 {
            harness.nodes[0].workers[0]
                .app
                .poll_budgeted(&mut cx, 64)
                .unwrap();
            assert!(reactor.in_flight() >= ordinary_in_flight);
            assert!(reactor.in_flight() <= limit);
            assert!(harness.nodes[0].workers[0].app.diagnostic_task.is_some());
            assert!(harness.nodes[0].workers[0].app.peer_task.is_some());
        }
        drop(probe);
        drop(peer);
        pressure_scope.cancel().unwrap();
        drop(pressure);
        for _ in 0..32 {
            harness.tick();
        }
        for path in ["/readyz", "/metrics"] {
            let socket = sim.connect(address.clone()).unwrap();
            let request = format!("GET {path} HTTP/1.1\r\nHost: local\r\n\r\n");
            assert_eq!(
                handle(&socket).send(request.as_bytes()).unwrap(),
                request.len()
            );
            let mut response = Vec::new();
            let mut closed = false;
            for _ in 0..1000 {
                harness.tick();
                let mut bytes = [0; 4096];
                match handle(&socket).recv(&mut bytes) {
                    Ok(0) => {
                        closed = true;
                        break;
                    }
                    Ok(count) => response.extend_from_slice(&bytes[..count]),
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => (),
                    Err(error) => panic!("{error}"),
                }
            }
            assert!(closed, "round {round} {path} stalled");
            assert!(response.starts_with(b"HTTP/1.1 200 OK\r\n"), "{response:?}");
        }
        assert!(reactor.in_flight() < limit);
    }
    let worker = &mut harness.nodes[0].workers[0];
    worker.app.stop_admission().unwrap();
    worker
        .app
        .diagnostic_scope
        .as_ref()
        .unwrap()
        .cancel()
        .unwrap();
    for _ in 0..32 {
        worker.runtime.reactor.poll_budgeted(128).unwrap();
        worker.app.poll_budgeted(&mut cx, 64).unwrap();
    }
    assert!(worker.app.diagnostic_task.is_none());
    assert!(worker.app.peer_task.is_none());
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn healthy_relayed_page_reads_opaque_opt_in() {
    healthy_relayed_page_reads_with_mode(true);
}

fn healthy_relayed_page_reads_with_mode(opaque: bool) {
    let sim = Simulation::new();
    let _os = sim.enter();
    let clock = SimulationClock::new(71);
    let environment = clock.environment(0);
    let _time = environment.enter();
    let _strict = crate::runtime::environment::require_simulated();
    let mut harness = Harness::new(71, sim, clock, false);
    harness.opaque_relay = opaque;
    // Match the deployed 64 MiB per-worker payload budgets. A five-page layer
    // exceeds resident capacity and forces receive admission to reclaim idle data.
    harness.payload_regression = true;
    harness.update(1);
    for _ in 0..40 {
        harness.add(None);
    }
    for node in &harness.nodes {
        for worker in &node.workers {
            assert_eq!(worker.app.peers.opaque_relay(), opaque);
        }
    }
    for node in 0..40 {
        let mut client = harness.request_on(1, true, false, node);
        client.first = 0;
        client.end = client.size;
        // Match client credit to this small payload budget. Delivered leases no
        // longer occupy the independent server acquisition window.
        client.request = format!("POST /v2/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRacer-Page-Credits: 2\r\nRacer-Byte-Credits: {}\r\nRacer-Ordered: 1\r\nIf-Match: {}\r\nRange: bytes=0-{}\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n", key(1), 2 * PAGE_BYTES, client.tag, client.size - 1).into_bytes();
        harness.exchange(client, false);
    }
    assert_eq!(harness.coverage.success, 40);
    assert_eq!(harness.coverage.failures, 0);
    assert_eq!(harness.coverage.bytes, 40 * (4 * PAGE_BYTES as usize + 257));
    assert!(harness.coverage.relay_turns > 0);
}

#[test]
fn concurrent_relayed_layers_diagnose_receive_pressure_and_recover() {
    concurrent_relayed_layers_with_mode(false);
}

#[test]
fn concurrent_relayed_layers_opaque_opt_in_pressure_and_recovery() {
    concurrent_relayed_layers_with_mode(true);
}

fn concurrent_relayed_layers_with_mode(opaque: bool) {
    let sim = Simulation::new();
    let _os = sim.enter();
    let clock = SimulationClock::new(73);
    let environment = clock.environment(0);
    let _time = environment.enter();
    let _strict = crate::runtime::environment::require_simulated();
    let mut harness = Harness::new(73, sim, clock, false);
    harness.concurrent_layers = true;
    harness.opaque_relay = opaque;
    for object in 1..=4 {
        harness.update(object);
    }
    for _ in 0..40 {
        harness.add(None);
    }
    // Exercise success, then controlled receive pressure after success headers,
    // then recovery on the same graphs, identities, disk, and connections.
    for pressure in [false, true, false] {
        let mut clients: Vec<_> = (1..=4).map(|object| {
            let mut client = harness.request_on(object, true, false, 0);
            client.first = 0;
            client.end = client.size;
            client.request = format!("GET /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nIf-Match: {}\r\nRange: bytes=0-{}\r\nRacer-Metadata: dst opaque metadata\r\nAuthorization: Bearer dst-fixture\r\nConnection: close\r\n\r\n", key(object), client.tag, client.size - 1).into_bytes();
            client
        }).collect();
        let mut charges = Vec::new();
        let mut injected = false;
        let before = harness.coverage.failures;
        for _ in 0..MAX_TURNS {
            harness.tick();
            for client in &mut clients {
                if !client.done && client.poll() {
                    harness.check(client, pressure);
                    client.fd.take();
                    client.done = true;
                }
            }
            if pressure
                && !injected
                && clients
                    .iter()
                    .any(|client| client.response.len() > 32768 && !client.done)
            {
                for node in &harness.nodes {
                    for worker in &node.workers {
                        worker.app.memory.evict_idle(usize::MAX).unwrap();
                        worker.runtime.admission.reclaim_buffers();
                        let admission = &worker.runtime.admission;
                        let free = admission.limit(ResourceClass::Ciphertext)
                            - admission.used(ResourceClass::Ciphertext);
                        if free != 0 {
                            charges.push(
                                admission
                                    .reserve(None, ResourceClass::Ciphertext, free)
                                    .unwrap(),
                            );
                        }
                    }
                }
                injected = true;
            }
            if clients.iter().all(|client| client.done) {
                break;
            }
        }
        assert!(
            clients.iter().all(|client| client.done),
            "concurrent layer reads stalled"
        );
        if pressure {
            assert!(injected);
            assert!(harness.coverage.failures > before);
            let mut diagnostics = String::new();
            for node in &harness.nodes {
                node.workers[0]
                    .app
                    .telemetry
                    .failures
                    .write(&mut diagnostics)
                    .unwrap();
            }
            assert!(
                diagnostics.contains("stage=Admission error=Overloaded"),
                "{diagnostics}"
            );
            assert!(diagnostics.contains("stage=NextSlice"), "{diagnostics}");
            assert!(diagnostics.contains("class: Ciphertext"), "{diagnostics}");
            assert!(
                diagnostics.contains("stage=CandidateResponse")
                    || diagnostics.contains("stage=PeerReceiveAdmission"),
                "{diagnostics}"
            );
        } else {
            assert_eq!(harness.coverage.failures, before);
        }
        drop(charges);
        harness.settle();
    }
    assert!(harness.coverage.secondary_worker_turns > 0);
    assert!(harness.coverage.relay_turns > 0);
    assert_eq!(harness.coverage.success, 8);
}
