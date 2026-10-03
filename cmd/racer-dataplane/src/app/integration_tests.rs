//! Real TLS enrollment/publication fixture driving the production application graph.
use super::test_support::*;
use super::*;
use racer_control_wire as wire;
use std::{
    io::{Read, Write},
    thread,
};

#[test]
fn two_worker_removal_preserves_late_driver_and_blocks_late_memory_and_disk_fill() {
    let mut fixture = ControlFixture::new();
    let (config, node) = fixture.bootstrap_node(2, Duration::from_secs(15));
    let (mut first, rt0, mut crypto0) = local_worker(&config, &node, 0);
    let (mut second, rt1, mut crypto1) = local_worker(&config, &node, 1);
    first
        .telemetry
        .attach_io(rt0.reactor.clone(), rt0.admission.clone())
        .unwrap();
    for app in [&mut first, &mut second] {
        futures::executor::block_on(app.store.writer.open()).unwrap();
        app.caches = vec![definition()];
    }
    first
        .snapshots
        .publish(publication(&config, 1, vec![definition()]))
        .unwrap();
    first
        .keys
        .install(wire::KeyringBundle {
            schema_version: 1,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(2),
            peer_trust_roots: (*first.keys.peer_trust_roots().unwrap()).clone(),
            cache_keys: vec![wire::CacheEncryptionKey::new(
                wire::CacheKeyRef {
                    cache: definition().id,
                    id: crate::model::key_id_from_generation(2, 7).unwrap(),
                    purpose: wire::CacheKeyPurpose::Page,
                },
                wire::CacheKeyState::Active,
                zeroize::Zeroizing::new([19; 32]),
            )],
        })
        .unwrap();
    let late0 = page(&first);
    let late1 = page(&second);
    first.memory.publish(late0.clone()).unwrap();
    second.memory.publish(late1.clone()).unwrap();
    let adapter = caches::CachePublication {
        node: node.clone(),
        listeners: first.prepared_listeners.clone(),
        capacity: config.limits.metadata_entries.get(),
    };
    assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    first.poll_cache_preparation(&mut cx).unwrap();
    let (release, receive) = futures::channel::oneshot::channel::<()>();
    let memory = second.memory.clone();
    let late = late1.clone();
    let owner = second.drivers.enter();
    crate::read::drivers::spawn(Box::pin(async move {
        receive.await.map_err(|_| Error::Cancelled)?;
        assert_eq!(memory.publish(late), Err(Error::Unavailable));
        Ok(())
    }))
    .unwrap();
    drop(owner);
    for _ in 0..4 {
        first.poll_cache_preparation(&mut cx).unwrap();
        second.poll_cache_preparation(&mut cx).unwrap();
    }
    assert_eq!(second.drivers.pending(), 1);
    assert_eq!(
        first.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );
    assert!(first.memory.get(late0.plaintext.page()).unwrap().is_some());
    // Avoid a real control poll here: drive the exact stage/acceptance handoff.
    first.control_task = Some(Box::pin(std::future::pending()));
    for _ in 0..8 {
        first.poll_cache_preparation(&mut cx).unwrap();
        second.poll_cache_preparation(&mut cx).unwrap();
    }
    let rejected = adapter.stage(&[]).unwrap();
    let mut invalid = publication(&config, 2, vec![]);
    invalid.members[0].peer_endpoint = "127.0.0.1:7444".into();
    assert!(matches!(
        first.snapshots.publish_staged(invalid, Some(rejected)),
        Err(Error::IncompatibleMembership)
    ));
    assert_eq!(
        first.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );
    assert!(first.memory.get(late0.plaintext.page()).unwrap().is_some());
    for _ in 0..4 {
        first.poll_cache_preparation(&mut cx).unwrap();
        second.poll_cache_preparation(&mut cx).unwrap();
    }
    first
        .snapshots
        .publish_staged(
            publication(&config, 2, vec![]),
            Some(adapter.stage(&[]).unwrap()),
        )
        .unwrap();
    assert_eq!(
        second.drivers.pending(),
        1,
        "removal cannot cancel an accepted driver"
    );
    assert!(second.memory.get(late1.plaintext.page()).unwrap().is_none());
    release.send(()).unwrap();
    second.drivers.poll(&mut cx, 64);
    assert_eq!(second.drivers.pending(), 0);
    for (app, late) in [(&first, late0), (&second, late1)] {
        assert!(app.memory.get(late.plaintext.page()).unwrap().is_none());
        assert_eq!(app.memory.publish(late.clone()), Err(Error::Unavailable));
        let dirty = app
            .runtime
            .admission
            .reserve(
                Some(&definition().id),
                crate::model::ResourceClass::DirtyCiphertext,
                19,
            )
            .unwrap();
        assert!(matches!(
            app.store.writer.enqueue(late.copy(), dirty),
            Err(Error::MissingKey)
        ));
    }
    assert!(second.peer_task.is_none() && second.diagnostic_task.is_none());
    // UID reuse is staged normally and consumes no cumulative tombstones.
    assert!(matches!(
        adapter.stage(&[definition()]),
        Err(Error::Unavailable)
    ));
    for (app, runtime, engine) in [
        (&mut first, &rt0, &mut crypto0),
        (&mut second, &rt1, &mut crypto1),
    ] {
        app.stop_admission().unwrap();
        if let Some(s) = &app.diagnostic_scope {
            s.cancel().unwrap();
        }
        app.peer_task.take();
        app.diagnostic_task.take();
        app.control_task.take();
        drive(runtime, engine, runtime.reactor.drain()).unwrap();
    }
}

#[test]
fn two_workers_start_from_real_control_and_checkpoint_one_complete_cut() {
    let mut fixture = ControlFixture::new();
    let (config, node) = fixture.bootstrap_node(2, Duration::from_secs(15));
    let ready = std::sync::Barrier::new(2);
    thread::scope(|threads| {
        for id in 0..2 {
            let (config, node, ready) = (&config, &node, &ready);
            threads.spawn(move || {
                let (mut app, runtime, mut engine) = local_worker(config, node, id);
                use crate::telemetry::metrics::Event;
                app.telemetry
                    .metrics
                    .record(Event::MemoryHit, u64::from(id) + 1)
                    .unwrap();
                drive(
                    &runtime,
                    &mut engine,
                    app.start(&scope(Duration::from_secs(15)).unwrap()),
                )
                .unwrap();
                ready.wait();
                assert!(node.observations.health.ready());
                assert_eq!(app.telemetry.metrics.count(Event::MemoryHit), 3);
                assert_eq!(app.peer_task.is_some(), id == 0);
                ready.wait();
                stop_worker(&mut app, &runtime, &mut engine);
            });
        }
    });
    let images = crate::store::checkpoint::read_candidates(&config.slab_directory).unwrap();
    assert_eq!(images.len(), 1);
    assert_eq!(images[0].1.shards.len(), 2);
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 2);
}

#[test]
fn startup_finishes_local_snapshot_installation_while_next_long_poll_is_held() {
    let mut fixture = ControlFixture::new();
    fixture.hold_long_poll.store(true, Ordering::Release);
    let (config, node) = fixture.bootstrap_node(2, Duration::from_secs(5));
    let finished = std::sync::Barrier::new(2);
    let results = thread::scope(|threads| {
        let mut handles = Vec::new();
        for id in 0..2 {
            let (config, node, waiting, finished) =
                (&config, &node, &fixture.long_polls, &finished);
            handles.push(threads.spawn(move || {
                let (mut worker, runtime, mut engine) = local_worker(config, node, id);
                let startup = scope(Duration::from_secs(3)).unwrap();
                // Force the first publication to wait for the second worker,
                // then allow local preparation after the next poll is held.
                if id == 1 {
                    while waiting.load(Ordering::Acquire) == 0 {
                        if startup.check().is_err() {
                            break;
                        }
                        thread::sleep(Duration::from_millis(1));
                    }
                }
                let result = drive(&runtime, &mut engine, worker.start(&startup));
                finished.wait();
                if result.is_ok() {
                    assert!(node.observations.health.ready());
                    assert!(worker.started);
                    assert!(worker.snapshots.current().is_ok());
                }
                finished.wait();
                let shutdown = scope(Duration::from_secs(5)).unwrap();
                drive(&runtime, &mut engine, worker.drain(&shutdown)).unwrap();
                drive(&runtime, &mut engine, worker.shutdown(&shutdown)).unwrap();
                result
            }));
        }
        handles
            .into_iter()
            .map(|h| h.join().unwrap())
            .collect::<Vec<_>>()
    });
    assert_eq!(results, vec![Ok(()), Ok(())]);
    assert!(fixture.hold_long_poll.load(Ordering::Acquire));
    assert_eq!(fixture.long_polls.load(Ordering::Acquire), 1);
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 2);
    assert!(node.cache_cut.lock().unwrap().committed);
    fixture.hold_long_poll.store(false, Ordering::Release);
}

#[test]
fn same_node_renewal_backs_off_expires_closed_and_recovers() {
    use crate::telemetry::health::State;
    use uring_runtime::environment::{SimulationClock, now, wall_now};

    // Complete each real control turn through the application's error handling.
    // Leave the next turn unsubmitted so the test controls all retry boundaries.
    fn turn(
        worker: &mut WorkerApplication,
        runtime: &WorkerRuntime,
        engine: &mut PageCryptoEngine,
    ) {
        let done = Rc::new(std::cell::Cell::new(false));
        let completed = done.clone();
        let control = worker.control.clone().unwrap();
        let request_scope = scope(Duration::from_secs(40)).unwrap();
        assert!(worker.control_task.is_none());
        worker.control_task = Some(Box::pin(async move {
            let result = control.progress(&request_scope).await;
            completed.set(true);
            result.map(|_| ())
        }));
        drive(
            runtime,
            engine,
            Box::pin(std::future::poll_fn(|cx| {
                worker.poll_control(cx)?;
                if done.get() {
                    worker.control_task.take();
                    Poll::Ready(Ok(()))
                } else {
                    Poll::Pending
                }
            })),
        )
        .unwrap();
        worker.observe_health().unwrap();
        assert!(worker.started && !worker.stopping);
        assert!(!runtime.admission.is_stopped());
    }

    let mut fixture = ControlFixture::new();
    let (config, node) = fixture.bootstrap_node(1, Duration::from_secs(15));
    // An already renewal-due, still valid certificate avoids advancing the TLS
    // server's host clock. A fresh 24-hour certificate covers the later virtual
    // expiry of this old certificate, and is also valid for real TLS handshakes.
    fixture
        .certificate_age
        .store(16 * 3600 + 60, Ordering::Release);
    let (mut worker, runtime, mut engine) = local_worker(&config, &node, 0);
    drive(
        &runtime,
        &mut engine,
        worker.start(&scope(Duration::from_secs(15)).unwrap()),
    )
    .unwrap();
    assert!(node.observations.health.ready());
    // Retained-publication startup completes staging without another HTTP turn.
    // Establish a successful control turn before measuring renewal-only backoff.
    turn(&mut worker, &runtime, &mut engine);
    let control = worker.control.clone().unwrap();
    let old = control.identity().unwrap();
    assert!(old.renewal_due() && old.valid_now());
    let old_signing = worker.keys.signing_identity().unwrap();
    let committed = std::fs::read(config.identity_directory.join("identity.json")).unwrap();
    let baseline = fixture.bootstrap_requests.lock().unwrap().len();
    let polls = fixture.polls.load(Ordering::Acquire);

    let clock = SimulationClock::new_at(3, Instant::now(), std::time::SystemTime::now());
    let environment = clock.environment(0);
    let _clock = environment.enter();
    fixture.bootstrap_status.store(503, Ordering::Release);
    turn(&mut worker, &runtime, &mut engine);
    assert_eq!(control.renewal_error(), Some(Error::Unavailable));
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 1
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls + 1);
    let pending = std::fs::read(config.identity_directory.join("pending.json")).unwrap();

    // Even many successful 204 polls must neither reset renewal backoff nor
    // replace the accepted certificate. Check both sides of the 1-second retry.
    clock.advance(Duration::from_millis(999));
    for _ in 0..16 {
        turn(&mut worker, &runtime, &mut engine);
        assert!(node.observations.health.ready());
        assert!(old.valid_now());
        assert!(Arc::ptr_eq(
            &old_signing,
            &worker.keys.signing_identity().unwrap()
        ));
        assert_eq!(
            control.identity().unwrap().certificate_chain(),
            old.certificate_chain()
        );
        assert_eq!(control.renewal_error(), Some(Error::Unavailable));
    }
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 1
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls + 17);
    clock.advance(Duration::from_millis(1));
    turn(&mut worker, &runtime, &mut engine);
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 2
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls + 18);
    // This seed selects a second jittered delay greater than one second.
    // Successful polls must retain the failure count so retries grow rather
    // than restarting at the first-failure delay on every turn.
    clock.advance(Duration::from_secs(1));
    turn(&mut worker, &runtime, &mut engine);
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 2
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls + 19);
    for cert in &fixture.poll_certificates.lock().unwrap()[polls..] {
        assert_eq!(cert, &old.certificate_chain()[0]);
    }
    assert_eq!(
        std::fs::read(config.identity_directory.join("identity.json")).unwrap(),
        committed
    );
    assert_eq!(
        std::fs::read(config.identity_directory.join("pending.json")).unwrap(),
        pending
    );

    // Refresh the observation just before expiry: readiness must fail because of
    // credentials, not because its independent 2-second observation lease aged.
    clock.advance(old.expires_at().duration_since(wall_now()).unwrap() - Duration::from_millis(1));
    worker.observe_health().unwrap();
    assert!(node.observations.health.ready());
    clock.advance(Duration::from_millis(1));
    assert!(!old.valid_now());
    assert!(!node.observations.health.ready());
    worker.observe_health().unwrap();
    assert_eq!(node.observations.health.state(), Ok(State::Degraded));
    let polls_at_expiry = fixture.polls.load(Ordering::Acquire);
    assert!(matches!(
        drive(
            &runtime,
            &mut engine,
            control.poll(
                wire::SnapshotRequest { after: None },
                &scope(Duration::from_secs(10)).unwrap()
            )
        ),
        Err(Error::Unauthorized)
    ));
    turn(&mut worker, &runtime, &mut engine);
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 3
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls_at_expiry);
    assert_eq!(control.renewal_error(), Some(Error::Unavailable));
    let retry = control.next_attempt().unwrap();
    assert!(
        (Duration::from_secs(1)..=Duration::from_secs(30)).contains(&retry.duration_since(now()))
    );
    clock.advance(retry.duration_since(now()) - Duration::from_millis(1));
    for _ in 0..16 {
        turn(&mut worker, &runtime, &mut engine);
        assert!(!node.observations.health.ready());
    }
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 3
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls_at_expiry);
    assert_eq!(
        std::fs::read(config.identity_directory.join("identity.json")).unwrap(),
        committed
    );

    fixture.certificate_age.store(1, Ordering::Release);
    fixture.bootstrap_status.store(200, Ordering::Release);
    clock.advance(Duration::from_millis(1));
    turn(&mut worker, &runtime, &mut engine);
    let fresh = control.identity().unwrap();
    assert_eq!(fresh.node(), old.node());
    assert!(fresh.valid_now() && !fresh.renewal_due());
    assert_ne!(fresh.certificate_chain(), old.certificate_chain());
    assert_ne!(fresh.private_key_der(), old.private_key_der());
    assert_eq!(
        worker.keys.signing_identity().unwrap().certificate_chain(),
        fresh.certificate_chain()
    );
    assert_eq!(control.renewal_error(), None);
    assert_eq!(control.next_attempt(), Some(now()));
    assert!(node.observations.health.ready());
    assert!(!config.identity_directory.join("pending.json").exists());
    assert_ne!(
        std::fs::read(config.identity_directory.join("identity.json")).unwrap(),
        committed
    );
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 4
    );
    let requests = fixture.bootstrap_requests.lock().unwrap();
    for request in &requests[baseline..] {
        assert_eq!(request.enrollment, requests[baseline].enrollment);
        assert_eq!(request.csr_der, requests[baseline].csr_der);
    }
    assert_ne!(requests[baseline].csr_der, requests[baseline - 1].csr_der);
    drop(requests);
    turn(&mut worker, &runtime, &mut engine);
    assert_eq!(
        fixture.bootstrap_requests.lock().unwrap().len(),
        baseline + 4
    );
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls_at_expiry + 2);
    for cert in &fixture.poll_certificates.lock().unwrap()[polls_at_expiry..] {
        assert_eq!(cert, &fresh.certificate_chain()[0]);
    }
    assert!(node.observations.health.ready());
    assert_eq!(
        worker.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );

    drop(_clock);
    stop_worker(&mut worker, &runtime, &mut engine);
}

#[test]
fn removal_publication_finishes_locally_after_controller_disappears() {
    use crate::model::{VersionMetadata, *};
    let mut fixture = ControlFixture::new();
    let mut config = fixture.config.take().unwrap();
    let diagnostic_address = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    config.diagnostics_listen = diagnostic_address.local_addr().unwrap();
    drop(diagnostic_address);
    let keep = crate::control::state::CacheDefinition {
        id: CacheId("44444444-4444-4444-8444-444444444444".into()),
        name: "keep".into(),
        client_socket: "/run/racer/keep/client/socket".into(),
        origin_socket: "/run/racer/keep/origin/socket".into(),
    };
    *fixture.publication.lock().unwrap() =
        Some(publication(&config, 1, vec![definition(), keep.clone()]));
    let node = Arc::new(NodeState::new(vec![WorkerId(0)], 64).unwrap());
    config.node = bootstrap(
        &config,
        &node,
        &config.limits,
        &scope(Duration::from_secs(15)).unwrap(),
    )
    .unwrap();
    let (mut worker, runtime, mut engine) = local_worker(&config, &node, 0);
    Rc::get_mut(&mut worker.clients)
        .unwrap()
        .set_root(fixture.directory.join("sockets"));
    drive(
        &runtime,
        &mut engine,
        worker.start(&scope(Duration::from_secs(15)).unwrap()),
    )
    .unwrap();
    worker
        .keys
        .install(wire::KeyringBundle {
            schema_version: 1,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(2),
            peer_trust_roots: (*worker.keys.peer_trust_roots().unwrap()).clone(),
            cache_keys: vec![wire::CacheEncryptionKey::new(
                wire::CacheKeyRef {
                    cache: keep.id.clone(),
                    id: crate::model::key_id_from_generation(2, 9).unwrap(),
                    purpose: wire::CacheKeyPurpose::Page,
                },
                wire::CacheKeyState::Active,
                zeroize::Zeroizing::new([29; 32]),
            )],
        })
        .unwrap();
    let metadata = VersionMetadata {
        content_type: None,
        version: ObjectVersion {
            object: ObjectId {
                cache: keep.id.clone(),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::test_value("kept"),
        },
        length: 0,
    };
    worker
        .store
        .writer
        .index()
        .publish_version(metadata.clone())
        .unwrap();
    // A full removal snapshot also carries a new topology; retain both locally.
    let mut next = publication(&config, 2, vec![keep.clone()]);
    next.membership_version = MembershipVersion(2);
    next.members[0].peer_endpoint = "127.0.0.1:7555".into();
    *fixture.publication.lock().unwrap() = Some(next);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let until = Instant::now() + Duration::from_secs(10);
    while node.cache_cut.lock().unwrap().definitions.len() != 1 {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker.poll_control(&mut cx).unwrap();
        runtime.reactor.wait(Duration::from_millis(1)).unwrap();
        assert!(Instant::now() < until);
    }
    assert_eq!(
        worker.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );
    fixture.stop.store(true, Ordering::Release);
    fixture.server.take().unwrap().join().unwrap();
    let polls = fixture.polls.load(Ordering::Acquire);
    while worker.snapshots.cursor().unwrap() != Some(wire::PublicationSequence(2)) {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker.poll_budgeted(&mut cx, 64).unwrap();
        assert!(
            Instant::now() < until,
            "accepted publication depended on a second HTTP poll"
        );
    }
    assert_eq!(fixture.polls.load(Ordering::Acquire), polls);
    let current = worker.snapshots.current().unwrap();
    assert_eq!(current.membership.version, MembershipVersion(2));
    assert_eq!(
        current
            .membership
            .member(&config.node)
            .unwrap()
            .peer_endpoint,
        "127.0.0.1:7555"
    );
    assert!(worker.peer_task.is_some() && worker.diagnostic_task.is_some());
    assert!(worker.listener_scope.as_ref().unwrap().check().is_ok());
    assert!(worker.diagnostic_scope.as_ref().unwrap().check().is_ok());
    assert_eq!(
        worker
            .store
            .writer
            .index()
            .version(&metadata.version)
            .unwrap(),
        Some(metadata.clone())
    );
    // An unrelated cache still serves a pinned HEAD over its real owned UDS.
    let directory = std::fs::File::open(fixture.directory.join("sockets/keep/client")).unwrap();
    use std::os::fd::AsRawFd;
    let mut client = std::os::unix::net::UnixStream::connect(format!(
        "/proc/self/fd/{}/socket",
        directory.as_raw_fd()
    ))
    .unwrap();
    client.set_nonblocking(true).unwrap();
    client.write_all(format!("HEAD /v2/objects/{} HTTP/1.1\r\nHost: racer\r\nIf-Match: \"kept\"\r\nConnection: close\r\n\r\n", "0".repeat(64)).as_bytes()).unwrap();
    let mut response = Vec::new();
    while !response.windows(4).any(|w| w == b"\r\n\r\n") {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker.poll_budgeted(&mut cx, 64).unwrap();
        let mut bytes = [0; 1024];
        match client.read(&mut bytes) {
            Ok(n) => response.extend_from_slice(&bytes[..n]),
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
            Err(e) => panic!("{e}"),
        }
        assert!(Instant::now() < until, "unrelated cache stopped serving");
    }
    assert!(
        response.starts_with(b"HTTP/1.1 200"),
        "{}",
        String::from_utf8_lossy(&response)
    );
    let mut diagnostic = std::net::TcpStream::connect(config.diagnostics_listen).unwrap();
    diagnostic.set_nonblocking(true).unwrap();
    diagnostic
        .write_all(b"GET /healthz HTTP/1.1\r\nHost: racer\r\nConnection: close\r\n\r\n")
        .unwrap();
    let mut response = Vec::new();
    while !response.windows(4).any(|w| w == b"\r\n\r\n") {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker.poll_budgeted(&mut cx, 64).unwrap();
        let mut bytes = [0; 1024];
        match diagnostic.read(&mut bytes) {
            Ok(n) => response.extend_from_slice(&bytes[..n]),
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
            Err(e) => panic!("{e}"),
        }
        assert!(Instant::now() < until, "diagnostics stopped during removal");
    }
    assert!(response.starts_with(b"HTTP/1.1 200"));
    let shutdown = scope(Duration::from_secs(5)).unwrap();
    drive(&runtime, &mut engine, worker.drain(&shutdown)).unwrap();
    drive(&runtime, &mut engine, runtime.reactor.drain()).unwrap();
    drive(&runtime, &mut engine, worker.shutdown(&shutdown)).unwrap();
}

#[test]
fn startup_retries_tls_internal_error_before_enrollment_and_worker_snapshot() {
    let mut fixture = ControlFixture::new();
    fixture.handshake_alerts.lock().unwrap().push_back(80);
    let (config, node) = fixture.bootstrap_node(1, Duration::from_secs(15));
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 1);
    assert_eq!(fixture.polls.load(Ordering::Acquire), 0);
    fixture.handshake_alerts.lock().unwrap().push_back(80);
    let (mut worker, runtime, mut engine) = local_worker(&config, &node, 0);
    drive(
        &runtime,
        &mut engine,
        worker.start(&scope(Duration::from_secs(15)).unwrap()),
    )
    .unwrap();
    assert!(node.observations.health.ready());
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 2);
    assert!(fixture.polls.load(Ordering::Acquire) > 0);
    assert!(!fixture.poll_certificates.lock().unwrap().is_empty());
    let shutdown = scope(Duration::from_secs(5)).unwrap();
    drive(&runtime, &mut engine, worker.drain(&shutdown)).unwrap();
    drive(&runtime, &mut engine, worker.shutdown(&shutdown)).unwrap();
}

#[test]
fn startup_tls_authentication_alert_remains_terminal() {
    let mut fixture = ControlFixture::new();
    let config = fixture.config.take().unwrap();
    let node = NodeState::new(vec![WorkerId(0)], 64).unwrap();
    fixture.handshake_alerts.lock().unwrap().push_back(42);
    assert_eq!(
        bootstrap(
            &config,
            &node,
            &config.limits,
            &scope(Duration::from_secs(5)).unwrap()
        ),
        Err(Error::Unauthorized)
    );
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 0);
    assert_eq!(fixture.polls.load(Ordering::Acquire), 0);
    assert!(!config.identity_directory.join("identity.json").exists());
    assert!(!node.observations.health.ready());
}

#[test]
fn startup_tls_internal_error_respects_deadline() {
    let mut fixture = ControlFixture::new();
    let config = fixture.config.take().unwrap();
    let node = NodeState::new(vec![WorkerId(0)], 64).unwrap();
    fixture.handshake_alerts.lock().unwrap().extend([80; 16]);
    assert_eq!(
        bootstrap(
            &config,
            &node,
            &config.limits,
            &scope(Duration::from_millis(500)).unwrap()
        ),
        Err(Error::DeadlineExceeded)
    );
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 0);
    assert_eq!(fixture.polls.load(Ordering::Acquire), 0);
    assert!(!config.identity_directory.join("identity.json").exists());
    assert!(!node.observations.health.ready());
}

#[test]
fn startup_reauthenticates_retained_identity_and_fails_closed() {
    let mut fixture = ControlFixture::new();
    let config = fixture.config.take().unwrap();
    let start = || {
        let node = NodeState::new(vec![WorkerId(0)], 64).unwrap();
        bootstrap(
            &config,
            &node,
            &config.limits,
            &scope(Duration::from_secs(5)).unwrap(),
        )
    };
    let old = start().unwrap();
    let committed = std::fs::read(config.identity_directory.join("identity.json")).unwrap();
    let new = NodeId("99999999-9999-4999-8999-999999999999".into());
    *fixture.binding.lock().unwrap() = new.clone();
    fixture.bootstrap_status.store(403, Ordering::Release);
    assert_eq!(start(), Err(Error::Unauthorized));
    assert_eq!(
        std::fs::read(config.identity_directory.join("identity.json")).unwrap(),
        committed
    );
    assert!(config.identity_directory.join("pending.json").exists());
    assert_eq!(fixture.polls.load(Ordering::Acquire), 0);
    fixture.bootstrap_status.store(200, Ordering::Release);
    assert_eq!(start().unwrap(), new);
    assert_ne!(old, new);
    assert!(!config.identity_directory.join("pending.json").exists());
    assert_eq!(start().unwrap(), new);
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 3);
}

#[test]
fn node_replacement_drains_all_workers_and_restart_converges() {
    use crate::runtime::affinity::WorkerPair;
    use uring_runtime::affinity::EffectiveTopology;
    for renewal_due in [false, true] {
        let mut fixture = ControlFixture::new();
        let (config, node) = fixture.bootstrap_node(2, Duration::from_secs(15));
        if renewal_due {
            fixture
                .certificate_age
                .store(16 * 3600 + 60, Ordering::Release);
        }
        let app = Arc::new(Application {
            limits: config.limits.clone(),
            config: Arc::new(config),
            node: node.clone(),
            discovered_nics: vec![],
        });
        let cpu = EffectiveTopology::discover().unwrap().cpus[0].clone();
        let mut group = WorkerGroup::new(AffinityPlan {
            max_threads: 5,
            pairs: (0..2)
                .map(|id| WorkerPair {
                    worker: WorkerId(id),
                    io: cpu.clone(),
                    crypto: cpu.clone(),
                    nic: None,
                })
                .collect(),
        });
        group
            .start(app.clone(), &scope(Duration::from_secs(15)).unwrap())
            .unwrap();
        assert!(node.observations.health.ready());
        let new = NodeId("99999999-9999-4999-8999-999999999999".into());
        *fixture.binding.lock().unwrap() = new.clone();
        if !renewal_due {
            // Only explicit binding rejection forces early enrollment. A 503
            // can come from a lagging replica and must retain the current state.
            fixture.poll_status.store(403, Ordering::Release);
        }
        let until = Instant::now() + Duration::from_secs(15);
        while node.observations.health.ready() {
            assert!(
                Instant::now() < until,
                "replacement did not stop the worker graph"
            );
            thread::sleep(Duration::from_millis(1));
        }
        assert_eq!(group.join(), Err(Error::NodeIdentityChanged));
        assert!(!node.observations.health.ready());
        // The old shared signing epoch was never rebound, even after committing
        // the replacement to disk. Both workers completed their checkpoint cut.
        let old_keys = Keyring::new(
            app.config.cluster.clone(),
            app.config.node.clone(),
            node.keys.clone(),
        );
        assert_eq!(
            old_keys.signing_identity().unwrap().node(),
            &app.config.node
        );
        let images = crate::store::checkpoint::read_candidates(&app.config.slab_directory).unwrap();
        assert_eq!(images[0].1.shards.len(), 2);
        fixture.poll_status.store(200, Ordering::Release);
        fixture.certificate_age.store(1, Ordering::Release);
        let fresh = Arc::new(NodeState::new(vec![WorkerId(0)], 64).unwrap());
        drop(group);
        let app = Arc::try_unwrap(app).ok().unwrap();
        let mut config = Arc::try_unwrap(app.config).ok().unwrap();
        config.node = bootstrap(
            &config,
            &fresh,
            &config.limits,
            &scope(Duration::from_secs(15)).unwrap(),
        )
        .unwrap();
        assert_eq!(config.node, new);
        let (mut worker, runtime, mut engine) = local_worker(&config, &fresh, 0);
        drive(
            &runtime,
            &mut engine,
            worker.start(&scope(Duration::from_secs(15)).unwrap()),
        )
        .unwrap();
        assert!(fresh.observations.health.ready());
        assert_eq!(worker.keys.node(), &new);
        assert_eq!(
            worker
                .snapshots
                .current()
                .unwrap()
                .membership
                .member(&new)
                .unwrap()
                .node,
            new
        );
        stop_worker(&mut worker, &runtime, &mut engine);
    }
}

#[test]
fn two_worker_real_control_key_lease_drain_and_checkpoint_cut() {
    use crate::{runtime::affinity::WorkerPair, store::checkpoint};
    let mut fixture = ControlFixture::new();
    let (config, node) = fixture.bootstrap_node(2, Duration::from_secs(15));
    let keys = Keyring::new(
        config.cluster.clone(),
        config.node.clone(),
        node.keys.clone(),
    );
    let limits = config.limits.clone();
    let app = Arc::new(Application {
        config: Arc::new(config),
        node: node.clone(),
        limits,
        discovered_nics: vec![],
    });
    let cpu = EffectiveTopology::discover().unwrap().cpus[0].clone();
    let plan = AffinityPlan {
        max_threads: 5,
        pairs: (0..2)
            .map(|id| WorkerPair {
                worker: WorkerId(id),
                io: cpu.clone(),
                crypto: cpu.clone(),
                nic: None,
            })
            .collect(),
    };
    let mut group = WorkerGroup::new(plan);
    group
        .start(app.clone(), &scope(Duration::from_secs(15)).unwrap())
        .unwrap();
    assert_eq!(node.prepared.load(Ordering::Acquire), 2);
    assert!(node.observations.health.ready());
    let cache = crate::model::CacheId("33333333-3333-4333-8333-333333333333".into());
    let key = wire::CacheKeyRef {
        cache: cache.clone(),
        id: crate::model::key_id_from_generation(2, 8).unwrap(),
        purpose: wire::CacheKeyPurpose::Page,
    };
    let roots = (*keys.peer_trust_roots().unwrap()).clone();
    keys.install(wire::KeyringBundle {
        schema_version: 1,
        cluster: app.config.cluster.clone(),
        generation: wire::BundleGeneration(2),
        peer_trust_roots: roots.clone(),
        cache_keys: vec![wire::CacheEncryptionKey::new(
            key.clone(),
            wire::CacheKeyState::Active,
            zeroize::Zeroizing::new([21; 32]),
        )],
    })
    .unwrap();
    let lease = keys.lease(Some(&cache), key.id, KeyPurpose::Page).unwrap();
    keys.install(wire::KeyringBundle {
        schema_version: 1,
        cluster: app.config.cluster.clone(),
        generation: wire::BundleGeneration(3),
        peer_trust_roots: roots,
        cache_keys: vec![],
    })
    .unwrap();
    let until = Instant::now() + Duration::from_secs(10);
    assert!(keys.lease(Some(&cache), key.id, KeyPurpose::Page).is_err());
    crate::security::test_support::assert_page_key(&lease, &[21; 32]);
    assert!(node.observations.health.ready());
    drop(lease);
    while !node.observations.health.ready() {
        assert!(
            Instant::now() < until,
            "two-worker readiness did not progress"
        );
        thread::sleep(Duration::from_millis(1));
    }
    assert!(keys.active(&cache, KeyPurpose::Page).is_err());
    let shutdown = scope(Duration::from_secs(10)).unwrap();
    group.drain(&shutdown).unwrap();
    group.shutdown(&shutdown).unwrap();
    group.join().unwrap();
    let bytes = std::fs::read(fixture.directory.join("slabs/checkpoint.0")).unwrap();
    let image = checkpoint::decode(&bytes).unwrap();
    let mut workers: Vec<_> = image.shards.iter().map(|shard| shard.worker.0).collect();
    workers.sort_unstable();
    assert_eq!(workers, vec![0, 1]);
    assert!(!node.observations.health.ready());
}
fn stop_worker(
    worker: &mut WorkerApplication,
    runtime: &WorkerRuntime,
    engine: &mut dyn CryptoService,
) {
    drive(
        runtime,
        engine,
        worker.drain(&scope(Duration::from_secs(5)).unwrap()),
    )
    .unwrap();
    drive(runtime, engine, runtime.reactor.drain()).unwrap();
    drive(
        runtime,
        engine,
        worker.shutdown(&scope(Duration::from_secs(5)).unwrap()),
    )
    .unwrap();
}

fn drive<T>(
    runtime: &WorkerRuntime,
    engine: &mut dyn CryptoService,
    future: Operation<'_, T>,
) -> Result<T> {
    let mut future = future;
    let until = Instant::now() + Duration::from_secs(15);
    loop {
        runtime.reactor.poll_budgeted(64)?;
        engine.poll_budgeted(64)?;
        runtime.crypto.poll_budgeted(64)?;
        if let Poll::Ready(result) = future
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
        {
            return result;
        }
        assert!(
            Instant::now() < until,
            "application lifecycle did not progress"
        );
        runtime.reactor.wait(Duration::from_millis(1))?;
    }
}
#[test]
fn blocked_publication_is_superseded_while_projection_rotates() {
    let mut fixture = ControlFixture::new();
    let (config, node) = fixture.bootstrap_node(1, Duration::from_secs(15));
    let (mut worker, runtime, mut engine) = local_worker(&config, &node, 0);
    drive(
        &runtime,
        &mut engine,
        worker.start(&scope(Duration::from_secs(15)).unwrap()),
    )
    .unwrap();
    let control = worker.control.clone().unwrap();
    // Drive control without polling worker preparation. The real rendezvous must
    // keep publications pending until the worker prepares their resources.
    // This test drives key delivery explicitly below rather than the worker task.
    worker.keyring_task.take();
    *fixture.publication.lock().unwrap() = Some(publication(&config, 2, vec![definition()]));
    drive(
        &runtime,
        &mut engine,
        control.progress(&scope(Duration::from_secs(5)).unwrap()),
    )
    .unwrap();
    assert_eq!(
        worker.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );
    {
        let mut bundle = fixture.bundle.lock().unwrap();
        bundle.generation = wire::BundleGeneration(2);
        bundle.cache_keys.clear();
    }
    drive(
        &runtime,
        &mut engine,
        control.keyring_progress(&scope(Duration::from_secs(5)).unwrap()),
    )
    .unwrap();
    *fixture.publication.lock().unwrap() = Some(publication(&config, 3, vec![]));
    drive(
        &runtime,
        &mut engine,
        control.progress(&scope(Duration::from_secs(5)).unwrap()),
    )
    .unwrap();
    assert!(
        worker
            .keys
            .active(&definition().id, KeyPurpose::Page)
            .is_err()
    );
    assert_eq!(control.projection_error(), None);
    assert_eq!(
        worker.snapshots.cursor().unwrap(),
        Some(wire::PublicationSequence(1))
    );
    // Resume worker preparation. Only the newer publication may commit.
    let until = Instant::now() + Duration::from_secs(5);
    while worker.snapshots.cursor().unwrap() != Some(wire::PublicationSequence(3)) {
        assert!(Instant::now() < until);
        runtime.reactor.poll_budgeted(64).unwrap();
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
        engine.poll_budgeted(64).unwrap();
    }
    assert!(worker.snapshots.current().unwrap().caches.is_empty());
    stop_worker(&mut worker, &runtime, &mut engine);
}

#[test]
fn network_keyring_bootstrap_rotation_recovery_and_failure_retention() {
    let mut fixture = ControlFixture::new();
    let mut config = fixture.config.take().unwrap();
    let node = Arc::new(NodeState::new(vec![WorkerId(0)], 64).unwrap());
    {
        let mut bundle = fixture.bundle.lock().unwrap();
        *bundle =
            crate::security::test_support::rotation_bundle(1, bundle.peer_trust_roots.clone());
        bundle.cluster = config.cluster.clone();
    }
    config.node = bootstrap(
        &config,
        &node,
        &config.limits,
        &scope(Duration::from_secs(15)).unwrap(),
    )
    .unwrap();
    assert_eq!(
        *fixture.keyring_tokens.lock().unwrap(),
        vec!["fixture.token"]
    );
    assert!(!fixture.directory.join("secrets").exists());
    let (mut worker, runtime, mut engine) = local_worker(&config, &node, 0);
    drive(
        &runtime,
        &mut engine,
        worker.start(&scope(Duration::from_secs(15)).unwrap()),
    )
    .unwrap();
    worker.keyring_task.take();
    let control = worker.control.clone().unwrap();
    let cache = fixture.bundle.lock().unwrap().cache_keys[0]
        .key
        .cache
        .clone();
    let lease = worker.keys.active(&cache, KeyPurpose::Page).unwrap();
    let old_id = lease.id();
    let mut old_sealed = [0; 19];
    lease
        .seal_page(&cache, &[1; 24], b"retained", b"abc", &mut old_sealed)
        .unwrap();
    fixture.hold_long_poll.store(true, Ordering::Release);
    let poll_scope = scope(Duration::from_secs(30)).unwrap();
    let mut topology = control.progress(&poll_scope);
    let key_scope = scope(Duration::from_secs(15)).unwrap();
    {
        let mut bundle = fixture.bundle.lock().unwrap();
        *bundle =
            crate::security::test_support::rotation_bundle(2, bundle.peer_trust_roots.clone());
        bundle.cluster = config.cluster.clone();
    }
    let mut rotation = control.keyring_progress(&key_scope);
    drive(
        &runtime,
        &mut engine,
        Box::pin(std::future::poll_fn(|cx| {
            assert!(topology.as_mut().poll(cx).is_pending());
            rotation.as_mut().poll(cx)
        })),
    )
    .unwrap();
    assert_eq!(control.projection_error(), None);
    assert_ne!(
        worker.keys.active(&cache, KeyPurpose::Page).unwrap().id(),
        old_id
    );
    assert!(
        worker
            .keys
            .lease(Some(&cache), old_id, KeyPurpose::Page)
            .is_err()
    );
    let mut opened = [0; 3];
    lease
        .open_page(
            &cache,
            old_id,
            &[1; 24],
            b"retained",
            &old_sealed,
            &mut opened,
        )
        .unwrap();
    assert_eq!(&opened, b"abc");
    drop(topology);
    fixture.hold_long_poll.store(false, Ordering::Release);
    let accepted = worker.keys.active(&cache, KeyPurpose::Page).unwrap().id();
    for response in [
        (200, b"{}".to_vec()),
        (200, vec![b' '; wire::MAX_BUNDLE_BYTES + 1]),
        (0, Vec::new()),
        (409, br#"{"code":"conflict"}"#.to_vec()),
    ] {
        *fixture.keyring_override.lock().unwrap() = Some(response);
        drive(
            &runtime,
            &mut engine,
            control.keyring_progress(&scope(Duration::from_secs(40)).unwrap()),
        )
        .unwrap();
        assert!(control.projection_error().is_some());
        assert_eq!(
            worker.keys.active(&cache, KeyPurpose::Page).unwrap().id(),
            accepted
        );
        *fixture.keyring_override.lock().unwrap() = None;
        drive(
            &runtime,
            &mut engine,
            control.keyring_progress(&scope(Duration::from_secs(10)).unwrap()),
        )
        .unwrap();
        assert_eq!(control.projection_error(), None);
    }
    *fixture.keyring_override.lock().unwrap() = None;
    fixture.reject_keyring_mtls.store(true, Ordering::Release);
    std::fs::write(&config.service_account_token, b"fixture.token.fresh").unwrap();
    {
        let mut bundle = fixture.bundle.lock().unwrap();
        *bundle =
            crate::security::test_support::rotation_bundle(3, bundle.peer_trust_roots.clone());
        bundle.cluster = config.cluster.clone();
    }
    drive(
        &runtime,
        &mut engine,
        control.keyring_progress(&scope(Duration::from_secs(40)).unwrap()),
    )
    .unwrap();
    assert_eq!(control.projection_error(), None);
    assert_eq!(
        fixture.keyring_tokens.lock().unwrap().last().unwrap(),
        "fixture.token.fresh"
    );
    assert_ne!(
        worker.keys.active(&cache, KeyPurpose::Page).unwrap().id(),
        accepted
    );
    fixture.reject_keyring_mtls.store(false, Ordering::Release);
    fixture.handshake_alerts.lock().unwrap().push_back(42); // bad_certificate
    std::fs::write(&config.service_account_token, b"fixture.token.newer").unwrap();
    drive(
        &runtime,
        &mut engine,
        control.keyring_progress(&scope(Duration::from_secs(10)).unwrap()),
    )
    .unwrap();
    assert_eq!(control.projection_error(), None);
    assert_eq!(
        fixture.keyring_tokens.lock().unwrap().last().unwrap(),
        "fixture.token.newer"
    );
    let tokens = fixture.keyring_tokens.lock().unwrap().len();
    let untrusted = rcgen::generate_simple_self_signed(vec!["untrusted.invalid".into()])
        .unwrap()
        .cert;
    std::fs::write(&config.trust_bundle, untrusted.pem()).unwrap();
    drive(
        &runtime,
        &mut engine,
        control.keyring_progress(&scope(Duration::from_secs(10)).unwrap()),
    )
    .unwrap();
    assert_eq!(control.projection_error(), Some(Error::Unauthorized));
    assert_eq!(
        fixture.keyring_tokens.lock().unwrap().len(),
        tokens,
        "never disclose token to an untrusted TLS endpoint"
    );
    assert!(!fixture.directory.join("secrets").exists());
}

#[test]
fn real_control_bootstrap_recovery_publication_readiness_and_shutdown() {
    let mut fixture = ControlFixture::new();
    let mut config = fixture.config.take().unwrap();
    let node = Arc::new(NodeState::new(vec![WorkerId(0)], 64).unwrap());
    let startup = scope(Duration::from_secs(15)).unwrap();
    config.node = bootstrap(&config, &node, &config.limits, &startup).unwrap();
    assert_eq!(fixture.enrollments.load(Ordering::Acquire), 1);
    let (mut worker, runtime, mut engine) = local_worker(&config, &node, 0);
    let startup = scope(Duration::from_secs(15)).unwrap();
    drive(&runtime, &mut engine, worker.start(&startup)).unwrap();
    assert!(node.observations.health.ready());
    assert!(fixture.polls.load(Ordering::Acquire) >= 1);
    assert_eq!(
        worker.snapshots.current().unwrap().sequence,
        racer_control_wire::PublicationSequence(1)
    );
    assert_eq!(
        fixture.enrollments.load(Ordering::Acquire),
        2,
        "worker reauthenticates the current binding before serving"
    );
    for _ in 0..8 {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
    }
    // Retire an omitted epoch through the actual serving loop. Accepted crypto
    // retains its key and buffers, without pausing listeners or deleting checkpoints.
    let cache = crate::model::CacheId("33333333-3333-4333-8333-333333333333".into());
    let reference = wire::CacheKeyRef {
        cache: cache.clone(),
        id: crate::model::key_id_from_generation(2, 7).unwrap(),
        purpose: wire::CacheKeyPurpose::Page,
    };
    let roots = (*worker.keys.peer_trust_roots().unwrap()).clone();
    worker
        .keys
        .install(wire::KeyringBundle {
            schema_version: 1,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(2),
            peer_trust_roots: roots.clone(),
            cache_keys: vec![wire::CacheEncryptionKey::new(
                reference.clone(),
                wire::CacheKeyState::Active,
                zeroize::Zeroizing::new([19; 32]),
            )],
        })
        .unwrap();
    let lease = worker
        .keys
        .lease(Some(&cache), reference.id, KeyPurpose::Page)
        .unwrap();
    let page = crate::model::PageId {
        version: crate::model::ObjectVersion {
            object: crate::model::ObjectId {
                cache: cache.clone(),
                key: crate::model::CacheKey([4; 32]),
            },
            etag: crate::model::StrongEtag::test_value("accepted"),
        },
        number: crate::model::PageNumber(0),
    };
    let buffers = BufferPool::new(runtime.admission.clone());
    let plaintext = buffers
        .plaintext(
            runtime
                .admission
                .reserve(Some(&cache), crate::model::ResourceClass::Plaintext, 8)
                .unwrap(),
            8,
        )
        .unwrap();
    let ciphertext = runtime
        .admission
        .reserve(Some(&cache), crate::model::ResourceClass::Ciphertext, 24)
        .unwrap();
    let accepted_scope = scope(Duration::from_secs(10)).unwrap();
    let mut accepted = runtime.crypto.execute(
        crate::runtime::crypto::CryptoInput::Encrypt {
            page,
            plaintext,
            ciphertext,
        },
        worker
            .keys
            .lease(Some(&cache), reference.id, KeyPurpose::Page)
            .unwrap(),
        &accepted_scope,
    );
    assert!(
        accepted
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending()
    );
    assert_eq!(runtime.crypto.outstanding(), 1);
    drop(accepted);
    accepted_scope.cancel().unwrap();
    let shard = futures::executor::block_on(worker.store.checkpoint.snapshot_shard()).unwrap();
    futures::executor::block_on(worker.store.checkpoint.publish(vec![shard])).unwrap();
    worker.store.checkpoint.finish_snapshot();
    assert!(fixture.directory.join("slabs/checkpoint.0").is_file());
    let shard = futures::executor::block_on(worker.store.checkpoint.snapshot_shard()).unwrap();
    futures::executor::block_on(worker.store.checkpoint.publish(vec![shard])).unwrap();
    worker.store.checkpoint.finish_snapshot();
    assert!(fixture.directory.join("slabs/checkpoint.1").is_file());
    worker
        .keys
        .install(wire::KeyringBundle {
            schema_version: 1,
            cluster: config.cluster.clone(),
            generation: wire::BundleGeneration(3),
            peer_trust_roots: roots,
            cache_keys: vec![],
        })
        .unwrap();
    let until = Instant::now() + Duration::from_secs(5);
    for _ in 0..8 {
        runtime.reactor.poll_budgeted(64).unwrap();
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
    }
    assert!(
        fixture.directory.join("slabs/checkpoint.0").is_file(),
        "historical checkpoints remain disposable cache"
    );
    assert_eq!(runtime.crypto.outstanding(), 1);
    assert!(worker.peer_task.is_some());
    assert!(worker.diagnostic_task.is_some());
    crate::security::test_support::assert_page_key(&lease, &[19; 32]);
    assert!(
        worker
            .keys
            .lease(Some(&cache), reference.id, KeyPurpose::Page)
            .is_err()
    );
    while runtime.crypto.outstanding() != 0 {
        engine.poll_budgeted(64).unwrap();
        runtime.crypto.poll_budgeted(64).unwrap();
        runtime.reactor.poll_budgeted(64).unwrap();
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
        assert!(Instant::now() < until, "accepted crypto completion stalled");
    }
    // Continued serving does not need an invalidation CQE or final key release.
    for _ in 0..4 {
        worker
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                64,
            )
            .unwrap();
        assert!(worker.peer_task.is_some());
        assert!(worker.diagnostic_task.is_some());
    }
    assert!(fixture.directory.join("slabs/checkpoint.0").exists());
    assert!(fixture.directory.join("slabs/checkpoint.1").exists());
    assert!(worker.keys.active(&cache, KeyPurpose::Page).is_err());
    let late = self::page(&worker);
    assert!(worker.memory.publish(late.clone()).is_err());
    let dirty = runtime
        .admission
        .reserve(
            Some(&cache),
            crate::model::ResourceClass::DirtyCiphertext,
            19,
        )
        .unwrap();
    assert!(matches!(
        worker.store.writer.enqueue(late.copy(), dirty),
        Err(Error::MissingKey)
    ));
    assert_eq!(runtime.crypto.outstanding(), 0);
    drop(lease);
    runtime.reactor.poll_budgeted(64).unwrap();
    worker
        .poll_budgeted(
            &mut Context::from_waker(futures::task::noop_waker_ref()),
            64,
        )
        .unwrap();
    assert!(node.observations.health.ready());
    let shutdown = scope(Duration::from_secs(5)).unwrap();
    drive(&runtime, &mut engine, worker.drain(&shutdown)).unwrap();
    assert!(!node.observations.health.ready());
    drive(&runtime, &mut engine, runtime.reactor.drain()).unwrap();
    drive(&runtime, &mut engine, worker.shutdown(&shutdown)).unwrap();
    assert_eq!(runtime.reactor.in_flight(), 0);
    assert_eq!(runtime.crypto.outstanding(), 0);
    assert!(fixture.directory.join("slabs/checkpoint.0").is_file());
}
use uring_runtime::affinity::EffectiveTopology;
