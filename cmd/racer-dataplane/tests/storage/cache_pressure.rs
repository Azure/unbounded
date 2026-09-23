// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod pressure {
    use super::*;

    fn assert_exhaustions(cache: &Cache, expected: Option<&str>) {
        let registry = crate::metrics::Registry::new(
            1,
            std::sync::Arc::new(crate::control::Updates::default()),
        );
        registry.register(0, cache.metrics());
        cache.metrics().publish();
        let text = registry.render();
        let samples: Vec<_> = text
            .lines()
            .filter(|line| line.starts_with("racer_dataplane_cache_resource_exhaustions_total{"))
            .collect();
        assert_eq!(samples.len(), 8);
        for sample in samples {
            let (name, count) = sample.rsplit_once(' ').unwrap();
            let selected =
                expected.is_some_and(|site| name.ends_with(&format!("{{site=\"{site}\"}}")));
            assert_eq!(count, if selected { "1" } else { "0" }, "{sample}");
        }
        if let Some(site) = expected {
            assert!(text.contains(&format!(
                "racer_dataplane_cache_resource_exhaustions_total{{site=\"{site}\"}} 1\n"
            )));
        }
    }

    fn admission_failure(result: Result<Progress<Fault<Fake>, CachedValue>>) -> Error {
        match result {
            Err(error) => {
                assert!(
                    matches!(error.root(), Error::Admission(e) if e.kind() == io::ErrorKind::WouldBlock)
                );
                error
            }
            Ok(_) => panic!("terminal pressure must not permit producer takeover"),
        }
    }

    fn shared(error: &Error) -> &std::sync::Arc<Error> {
        let Error::Shared(error) = error else {
            panic!("terminal admission failure was not shared: {error:?}");
        };
        error
    }

    fn scenario(page: bool, cancel: bool, early_polls: bool) {
        let world = crate::simulation::World::new(415);
        let _scope = world.enter();
        let pool = buffers::io_test_pool(2);
        let mut ring = Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
        let disk = crate::simulation::Disk::new(32 * 1024 * 1024);
        let mut slab = allocator::Slab::simulated(disk.clone(), 32 * 1024 * 1024, 1, true).unwrap();
        let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
        cache
            .set_limits(Limits {
                resource_retries: 2,
                ..Limits::default()
            })
            .unwrap();
        let mut upstream = scoped_fake();
        let meta = metadata(&cache, "/shared-admission-pressure", 3, 0);
        let end = world.now() + Duration::from_secs(5);
        let mut faults: Vec<_> = (0..3)
            .map(|_| {
                if page {
                    cache.page::<Fake>(&meta, 0, end).unwrap()
                } else {
                    cache.metadata::<Fake>(meta.target(), end).unwrap().0
                }
            })
            .collect();
        let producer = faults.remove(0);
        let (producer, _) = pending(
            cache
                .poll_value(producer, &mut ring, &mut upstream)
                .unwrap(),
        );
        let joiners: Vec<_> = faults
            .into_iter()
            .map(|fault| {
                let (fault, work) =
                    pending(cache.poll_value(fault, &mut ring, &mut upstream).unwrap());
                assert!(!work.runnable);
                fault
            })
            .collect();
        assert_eq!(upstream.starts.len(), 1);

        // Real allocator admission rejects the completed receive. Neither metadata
        // nor payload can publish while filesystem headroom is unavailable.
        disk.set_available_bytes(0);
        let (mut producer, mut work) = pending(
            cache
                .poll_value(producer, &mut ring, &mut upstream)
                .unwrap(),
        );
        assert!(!work.runnable);
        assert!(if page {
            matches!(producer.state, Loading::Admitting(_))
        } else {
            matches!(producer.state, Loading::Metadata(_))
        });
        assert_exhaustions(&cache, None);
        if cancel {
            drop(producer);
            disk.set_available_bytes(u64::MAX);
            for fault in joiners {
                let (value, _) = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
                assert_eq!(value.as_slice().len(), if page { 3 } else { META_SIZE });
            }
            assert_eq!(
                upstream.starts.len(),
                2,
                "one surviving producer refetches after cancellation"
            );
        } else {
            let began = world.now();
            let mut retries = 0;
            let error = loop {
                retries += 1;
                assert!(retries <= 2, "sustained overload must remain bounded");
                if early_polls {
                    for _ in 0..2048 {
                        let (next, parked) = pending(
                            cache
                                .poll_value(producer, &mut ring, &mut upstream)
                                .unwrap(),
                        );
                        assert!(!parked.runnable);
                        assert_eq!(parked.deadline, work.deadline);
                        assert!(next.resource_polls <= cache.limits.resource_retries * 128);
                        producer = next;
                    }
                }
                world.advance(
                    work.deadline
                        .unwrap()
                        .saturating_duration_since(world.now()),
                );
                match cache.poll_value(producer, &mut ring, &mut upstream) {
                    Ok(Progress::Pending { fault, work: next }) => {
                        producer = fault;
                        work = next;
                    }
                    result => break admission_failure(result),
                }
            };
            assert_eq!(error.to_string(), "resource retry limit");
            assert_eq!(world.now() - began, Duration::from_millis(20));
            // Even after capacity recovers, existing joiners must see the exact
            // terminal error, rather than silently taking over and refetching.
            disk.set_available_bytes(u64::MAX);
            for fault in joiners {
                let joined = admission_failure(cache.poll_value(fault, &mut ring, &mut upstream));
                assert!(std::sync::Arc::ptr_eq(shared(&error), shared(&joined)));
            }
            assert_eq!(upstream.starts.len(), 1);
            let fresh = if page {
                cache.page::<Fake>(&meta, 0, end).unwrap()
            } else {
                cache.metadata::<Fake>(meta.target(), end).unwrap().0
            };
            let (value, _) = resolve_checked(&mut cache, &mut ring, &mut upstream, fresh);
            assert_eq!(value.as_slice().len(), if page { 3 } else { META_SIZE });
            assert_eq!(
                upstream.starts.len(),
                2,
                "new requests recover after terminal pressure"
            );
        }
        assert_eq!(cache.active_faults.get(), 0);
        assert_exhaustions(
            &cache,
            if cancel {
                None
            } else if page {
                Some("payload_admission")
            } else {
                Some("metadata_admission")
            },
        );
        cache.shutdown(&mut ring).unwrap();
        ring.shutdown().unwrap();
        pool.assert_recovered();
        drop((cache, slab, ring, pool, upstream));
        world.assert_clean();
    }

    #[test]
    fn terminal_pressure_is_shared_without_takeover_or_refetch() {
        for page in [false, true] {
            for early_polls in [false, true] {
                scenario(page, false, early_polls);
            }
        }
    }

    #[test]
    fn cancellation_during_admission_preserves_joiner_takeover() {
        for page in [false, true] {
            scenario(page, true, false);
        }
    }

    #[test]
    fn checksum_queue_exhaustion_is_shared_busy_without_failed_peer_validation() {
        let world = crate::simulation::World::new(418);
        let _scope = world.enter();
        let capacity = crate::crypto::PoolConfig::default()
            .max_outstanding_per_worker
            .get();
        let pool = buffers::io_test_pool(capacity + 2);
        let mut ring = Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
        let crypto = crate::crypto::Pool::test_pool(&pool);
        let (worker, source) = crypto.attach_local(&pool, ring.wake_handle()).unwrap();
        let worker = Rc::new(std::cell::RefCell::new(worker));
        let mut held = Vec::new();
        for _ in 0..capacity {
            let mut fill = pool.private_fill().unwrap();
            fill.as_mut_slice()[0] = 42;
            held.push(
                worker
                    .borrow_mut()
                    .checksum(fill, 1, None)
                    .unwrap_or_else(|_| panic!("checksum holder rejected")),
            );
        }
        let rejected = worker
            .borrow_mut()
            .checksum(pool.private_fill().unwrap(), 1, None)
            .err()
            .unwrap();
        assert_eq!(rejected.error, crate::crypto::Error::WouldBlock);
        drop(rejected);
        let mut slab = allocator::Slab::simulated(
            crate::simulation::Disk::new(32 * 1024 * 1024),
            32 * 1024 * 1024,
            1,
            true,
        )
        .unwrap();
        let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
        cache
            .set_limits(Limits {
                resource_retries: 2,
                ..Limits::default()
            })
            .unwrap();
        cache.set_crypto(Some(worker.clone()));
        let mut upstream = Fake::peer([Reply::Good]);
        upstream.scope = scoped_fake().scope;
        upstream.ready_peer = true;
        let meta = metadata(&cache, "/checksum-pressure", 3, 0);
        let end = world.now() + Duration::from_secs(5);
        let producer = cache.page(&meta, 0, end).unwrap();
        let joiner = cache.page(&meta, 0, end).unwrap();
        let (producer, _) = pending(
            cache
                .poll_value(producer, &mut ring, &mut upstream)
                .unwrap(),
        );
        let (joiner, _) = pending(cache.poll_value(joiner, &mut ring, &mut upstream).unwrap());
        let (mut producer, _) = pending(
            cache
                .poll_value(producer, &mut ring, &mut upstream)
                .unwrap(),
        );
        assert!(matches!(producer.state, Loading::ChecksumPending(_)));
        assert!(producer.validation.is_some());
        assert!(producer.route == Route::Peer);
        let began = world.now();
        for _ in 0..2 {
            let (next, work) = pending(
                cache
                    .poll_value(producer, &mut ring, &mut upstream)
                    .unwrap(),
            );
            assert!(matches!(next.state, Loading::ChecksumPending(_)));
            assert!(next.validation.is_some());
            assert!(!work.runnable);
            world.advance(
                work.deadline
                    .unwrap()
                    .saturating_duration_since(world.now()),
            );
            producer = next;
        }
        let error = admission_failure(cache.poll_value(producer, &mut ring, &mut upstream));
        let joined = admission_failure(cache.poll_value(joiner, &mut ring, &mut upstream));
        assert!(std::sync::Arc::ptr_eq(shared(&error), shared(&joined)));
        assert_eq!(world.now() - began, Duration::from_millis(20));
        for error in [&error, &joined] {
            assert_eq!(error.to_string(), "resource retry limit");
            assert_eq!(crate::http_auth::failure::error_status(error), 503);
            assert_eq!(
                crate::http_auth::failure::failure_reason(error),
                crate::http_client::attempt::PeerReason::Busy
            );
        }
        assert!(
            upstream.validations.is_empty(),
            "local pressure must not fail peer validation"
        );
        assert!(
            upstream.resumes.is_empty(),
            "local pressure must not retry the peer"
        );
        assert_eq!(upstream.advances, 0);
        assert_eq!(upstream.starts.len(), 1, "joiner must not refetch");
        assert_eq!(cache.active_faults.get(), 0);
        assert_exhaustions(&cache, Some("checksum_queue"));
        drop(held);
        cache.set_crypto(None);
        drop((worker, source));
        crypto.shutdown().unwrap();
        for _ in 0..capacity * 4 {
            let Some(due) = world.next_task_tick() else {
                break;
            };
            world.advance(Duration::from_millis(due.saturating_sub(world.tick())));
            world.run_tasks();
        }
        assert!(world.next_task_tick().is_none());
        cache.shutdown(&mut ring).unwrap();
        ring.shutdown().unwrap();
        pool.assert_recovered();
        drop((cache, slab, ring, pool, upstream));
        world.assert_clean();
    }

    #[test]
    fn early_wakeups_preserve_retry_budget_and_recovery() {
        let world = crate::simulation::World::new(416);
        let _scope = world.enter();
        let pool = buffers::io_test_pool(1);
        let mut ring = Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
        let mut slab = allocator::Slab::simulated(
            crate::simulation::Disk::new(32 * 1024 * 1024),
            32 * 1024 * 1024,
            1,
            true,
        )
        .unwrap();
        let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
        cache
            .set_limits(Limits {
                resource_retries: 2,
                ..Limits::default()
            })
            .unwrap();
        let mut upstream = scoped_fake();
        let meta = metadata(&cache, "/early-wakeups", 3, 0);
        let held = pool.private_fill().unwrap();
        let began = world.now();
        let end = began + Duration::from_secs(5);
        let mut fault = cache.page(&meta, 0, end).unwrap();
        // Model unrelated runnable connections repeatedly revisiting this fault
        // before its requested wakeup. No service time or caller budget expired.
        for _ in 0..2048 {
            let (next, work) = pending(cache.poll_value(fault, &mut ring, &mut upstream).unwrap());
            assert!(!work.runnable);
            assert_eq!(work.deadline, Some(began + Duration::from_millis(10)));
            assert!(next.resource_polls <= cache.limits.resource_retries * 128);
            fault = next;
        }
        assert_eq!(world.now(), began);
        assert!(upstream.starts.is_empty());
        assert_exhaustions(&cache, None);
        drop(held);
        world.advance(Duration::from_millis(10));
        let (value, _) = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
        assert_eq!(value.as_slice(), b"xxx");
        assert_eq!(upstream.starts.len(), 1);
        assert_exhaustions(&cache, None);
        drop(value);
        cache.shutdown(&mut ring).unwrap();
        ring.shutdown().unwrap();
        pool.assert_recovered();
        drop((cache, slab, ring, pool, upstream));
        world.assert_clean();
    }

    #[test]
    fn parked_resource_wait_preserves_caller_and_candidate_deadlines() {
        for candidate in [false, true] {
            let world = crate::simulation::World::new(417);
            let _scope = world.enter();
            let pool = buffers::io_test_pool(1);
            let mut ring = Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
            let mut slab = allocator::Slab::simulated(
                crate::simulation::Disk::new(32 * 1024 * 1024),
                32 * 1024 * 1024,
                1,
                true,
            )
            .unwrap();
            let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
            cache
                .set_limits(Limits {
                    resource_retries: 2,
                    ..Limits::default()
                })
                .unwrap();
            let mut upstream = scoped_fake();
            if candidate {
                upstream.candidate_cap = Some(Duration::from_millis(5));
            }
            let meta = metadata(&cache, "/parked-deadline", 3, 0);
            let held = pool.private_fill().unwrap();
            let end = world.now() + Duration::from_millis(if candidate { 1000 } else { 5 });
            let mut fault = cache.page(&meta, 0, end).unwrap();
            for _ in 0..2048 {
                let (next, _) = pending(cache.poll_value(fault, &mut ring, &mut upstream).unwrap());
                fault = next;
            }
            let (fault, work) = pending(cache.poll_value(fault, &mut ring, &mut upstream).unwrap());
            assert_eq!(work.deadline, Some(world.now() + Duration::from_millis(5)));
            world.advance(Duration::from_millis(5));
            assert!(matches!(
                cache.poll_value(fault, &mut ring, &mut upstream),
                Err(Error::Timeout)
            ));
            assert!(upstream.starts.is_empty());
            assert_eq!(cache.active_faults.get(), 0);
            assert_exhaustions(&cache, None);
            drop(held);
            cache.shutdown(&mut ring).unwrap();
            ring.shutdown().unwrap();
            pool.assert_recovered();
            drop((cache, slab, ring, pool, upstream));
            world.assert_clean();
        }
    }

    #[test]
    fn coordination_and_materialization_exhaustions_are_distinct() {
        for materialize in [false, true] {
            let world = crate::simulation::World::new(420);
            let _scope = world.enter();
            let pool = buffers::io_test_pool_config(buffers::Config {
                network_flights: std::num::NonZeroUsize::new(1).unwrap(),
                ..buffers::Config::new(std::num::NonZeroUsize::new(1).unwrap())
            });
            let mut ring = Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
            let mut slab = allocator::Slab::simulated(
                crate::simulation::Disk::new(32 * 1024 * 1024),
                32 * 1024 * 1024,
                1,
                true,
            )
            .unwrap();
            let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
            cache
                .set_limits(Limits {
                    resource_retries: 2,
                    ..Limits::default()
                })
                .unwrap();
            let mut upstream = scoped_fake();
            let meta = metadata(&cache, "/coordination-or-materialization", 3, 0);
            if materialize {
                let fault = cache
                    .page(&meta, 0, world.now() + Duration::from_secs(5))
                    .unwrap();
                let (value, _) = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
                drop(value);
                cache.shutdown(&mut ring).unwrap();
                pool.assert_recovered();
            }
            let held_buffer = materialize.then(|| pool.private_fill().unwrap());
            let held_flight = (!materialize).then(|| {
                pool.network_flight(upstream.network_scope([255; 32]).unwrap())
                    .unwrap()
            });
            let began = world.now();
            let mut fault = cache
                .page(&meta, 0, began + Duration::from_secs(5))
                .unwrap();
            fault.buffered = materialize;
            // Publishing an existing file takes one runnable transition before
            // materialization asks for a buffer. Coordination waits immediately.
            if materialize {
                (fault, _) = pending(cache.poll_value(fault, &mut ring, &mut upstream).unwrap());
            }
            for _ in 0..2 {
                let (next, work) =
                    pending(cache.poll_value(fault, &mut ring, &mut upstream).unwrap());
                assert!(!work.runnable);
                assert_exhaustions(&cache, None);
                world.advance(work.deadline.unwrap().duration_since(world.now()));
                fault = next;
            }
            let error = admission_failure(cache.poll_value(fault, &mut ring, &mut upstream));
            assert_eq!(crate::http_auth::failure::error_status(&error), 503);
            assert_eq!(world.now() - began, Duration::from_millis(20));
            assert_eq!(upstream.starts.len(), usize::from(materialize));
            assert_exhaustions(
                &cache,
                Some(if materialize {
                    "materialize_buffer"
                } else {
                    "network_flight"
                }),
            );
            drop((held_buffer, held_flight));
            cache.shutdown(&mut ring).unwrap();
            ring.shutdown().unwrap();
            pool.assert_recovered();
            drop((cache, slab, ring, pool, upstream));
            world.assert_clean();
        }
    }

    #[test]
    fn reserved_receive_wait_is_bounded_and_owner_can_use_remaining_slot() {
        let world = crate::simulation::World::new(423);
        let _scope = world.enter();
        let pool = buffers::io_test_pool(2);
        let mut ring = Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
        let mut slab = allocator::Slab::simulated(
            crate::simulation::Disk::new(32 * 1024 * 1024),
            32 * 1024 * 1024,
            1,
            true,
        )
        .unwrap();
        let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
        let held = pool.private_fill().unwrap();
        let mut peer = Fake::peer([Reply::Good]);
        peer.receive_reserve = 1;
        peer.scope = scoped_fake().scope;
        let meta = metadata(&cache, "/reserved-peer", 3, 0);
        let began = world.now();
        let end = began + Duration::from_secs(5);
        let fault = cache.page(&meta, 0, end).unwrap();
        let (mut fault, _) = pending(cache.poll_value(fault, &mut ring, &mut peer).unwrap());
        let mut owner = Fake::default();
        let own_meta = metadata(&cache, "/local-owner", 3, 0);
        let own = cache.page(&own_meta, 0, end).unwrap();
        let (value, _) = resolve_checked(&mut cache, &mut ring, &mut owner, own);
        assert_eq!(value.as_slice(), b"xxx");
        drop(value);
        assert_eq!(owner.starts.len(), 1);
        let mut terminal = None;
        for _ in 0..33 {
            match cache.poll_value(fault, &mut ring, &mut peer) {
                Ok(Progress::Pending { fault: next, work }) => {
                    assert!(!work.runnable);
                    world.advance(
                        work.deadline
                            .unwrap()
                            .saturating_duration_since(world.now()),
                    );
                    fault = next;
                }
                result => {
                    terminal = Some(admission_failure(result));
                    break;
                }
            }
        }
        assert!(terminal.is_some());
        assert!(world.now() - began >= Duration::from_millis(320));
        assert!(world.now() < end);
        assert!(peer.starts.is_empty());
        assert_exhaustions(&cache, Some("receive_buffer"));
        drop(held);
        let fresh = cache.page(&meta, 0, end).unwrap();
        let (value, _) = resolve_checked(&mut cache, &mut ring, &mut peer, fresh);
        assert_eq!(value.as_slice(), b"xxx");
        drop(value);
        cache.shutdown(&mut ring).unwrap();
        ring.shutdown().unwrap();
        pool.assert_recovered();
        drop((cache, slab, ring, pool, peer, owner));
        world.assert_clean();
    }

    #[test]
    fn opposite_cold_pages_diagnostic_requires_progress_without_busy() {
        use crate::{http_client as client, runtime::tests::dst::Cluster, simulation::Phase};

        // These are clipped cold payload pages. Each still needs a full pool
        // slot, isolating acquisition ordering from network/disk throughput.
        for (capacity, requests_per_source, paths) in [
            (8, 8, vec![vec![2, 5]]),
            (8, 8, vec![vec![2, 5], vec![5, 2]]),
            (8, 8, vec![vec![0, 1, 3, 7], vec![7, 6, 4, 0]]),
            // Minimum daemon capacity must serve idle three-hop cold routes,
            // including simultaneous opposite directions, without Busy/retries.
            (4, 1, vec![vec![0, 1, 3, 7], vec![7, 6, 4, 0]]),
        ] {
            let opposite = paths.len() > 1;
            let world = crate::simulation::World::new(421);
            let _scope = world.enter();
            let mut cluster = Cluster::with_pool(world.clone(), false, false, Some(2), capacity);
            // The shared fixture's 32 MiB slab has only seven payload extents.
            // Isolate receive progress from eviction of this 16-page working set.
            for node in 0..8 {
                let _node = world.scoped_node(Some(node));
                let mut slab = allocator::Slab::simulated(
                    crate::simulation::Disk::new(128 * 1024 * 1024),
                    128 * 1024 * 1024,
                    1,
                    true,
                )
                .unwrap();
                let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
                cache.set_metrics(cluster.ring(node).metrics().clone());
                *cluster.cache(node) = cache;
            }
            let directions: Vec<_> = paths
                .iter()
                .map(|path| (path[0], *path.last().unwrap()))
                .collect();
            let mut targets = Vec::new();
            for &(source, owner) in &directions {
                for page in 0..requests_per_source {
                    let target =
                        cluster.target(owner as u32, &format!("opposite-cold-{source}-{page}"));
                    crate::runtime::tests::dst::assert_route(
                        &cluster,
                        &target,
                        0,
                        paths.iter().find(|path| path[0] == source).unwrap(),
                    );
                    assert_eq!(cluster.head(source, &target), 200);
                    assert_eq!(cluster.head(owner, &target), 200);
                    targets.push((source, owner, target));
                }
            }
            cluster.turns(100);
            for node in 0..8 {
                cluster.ring(node).pool().assert_recovered();
            }
            // Fixture origins normally share each dataplane's ring and pool.
            // External origins must not consume the receive slots being tested.
            let mut origins: Vec<_> = (0..8)
                .map(|node| {
                    let server = cluster.separate_origin(node);
                    let _scope = world.scoped_node(Some(node));
                    let pool = buffers::test_pool(
                        buffers::Config::new(std::num::NonZeroUsize::new(8).unwrap()),
                        crate::workers::NumaNodeId(100 + node),
                        true,
                    );
                    let ring = Ring::http_test_ring(pool, uring::Config::default()).unwrap();
                    (node, ring, server)
                })
                .collect();
            let poll_origins = |origins: &mut Vec<(
                usize,
                Ring,
                crate::http_server::Server<crate::simulation::corpus::Origin>,
            )>| {
                for (node, ring, server) in origins {
                    let _scope = world.scoped_node(Some(*node));
                    ring.progress().unwrap();
                    server.poll(ring, 64).unwrap();
                }
            };
            let hits = cluster.hits.borrow().len();
            let start = world.tick();
            let gates: Vec<_> = targets
                .iter()
                .map(|(source, _, target)| {
                    let path = paths.iter().find(|path| path[0] == *source).unwrap();
                    cluster.gate((*source, path[1]), target, Phase::Request, None, false)
                })
                .collect();
            let mut requests: Vec<_> = targets
                .iter()
                .map(|(source, _, target)| {
                    client::Connection::new_address(cluster.local_address(*source), "localhost")
                        .unwrap()
                        .get_small(
                            client::Request::new(target, &[]).unwrap(),
                            3,
                            world.now() + Duration::from_secs(15),
                        )
                        .unwrap()
                })
                .collect();
            let admitted = |source| {
                requests_per_source
                    .min(capacity + 1 - paths.iter().find(|path| path[0] == source).unwrap().len())
            };
            for _ in 0..500 {
                cluster.turns(1);
                poll_origins(&mut origins);
                for ((source, _, _), request) in targets.iter().zip(&mut requests) {
                    assert!(matches!(
                        request.poll(cluster.ring(*source), 32).unwrap(),
                        crate::http::Progress::Pending(_)
                    ));
                }
                if directions.iter().all(|(source, _)| {
                    targets
                        .iter()
                        .zip(&gates)
                        .filter(|((node, _, _), gate)| node == source && world.hits(**gate) > 0)
                        .count()
                        == admitted(*source)
                }) && directions.iter().all(|(source, _)| {
                    cluster.ring(*source).pool().invariant_snapshot().producers
                        == requests_per_source
                }) {
                    break;
                }
            }
            assert!(
                directions.iter().all(|(source, _)| {
                    targets
                        .iter()
                        .zip(&gates)
                        .filter(|((node, _, _), gate)| node == source && world.hits(**gate) > 0)
                        .count()
                        == admitted(*source)
                }),
                "peer receives must leave downstream rank capacity while excess requests wait without buffers"
            );
            assert_eq!(
                cluster.hits.borrow().len(),
                hits,
                "only metadata is warm; no payload reached origin"
            );
            for &(source, _) in &directions {
                let snapshot = cluster.ring(source).pool().invariant_snapshot();
                assert_eq!(
                    snapshot.loading,
                    admitted(source),
                    "downstream rank capacity must remain free: {snapshot:?}"
                );
                assert_eq!(snapshot.producers, requests_per_source);
                assert_eq!(snapshot.flights, requests_per_source);
                assert_exhaustions(&cluster.cache(source), None);
                eprintln!(
                    "opposite={opposite} gated node={source} tick={} pool={snapshot:?}",
                    world.tick()
                );
            }
            let mut released_gates = vec![false; gates.len()];
            for (index, gate) in gates.iter().enumerate() {
                if world.hits(*gate) > 0 {
                    world.release(*gate);
                    released_gates[index] = true;
                }
            }
            let released = world.tick();
            let mut replies: Vec<Option<(u16, Vec<u8>)>> =
                (0..requests.len()).map(|_| None).collect();
            for _ in 0..2000 {
                cluster.turns(1);
                poll_origins(&mut origins);
                for (index, gate) in gates.iter().enumerate() {
                    if !released_gates[index] && world.hits(*gate) > 0 {
                        world.release(*gate);
                        released_gates[index] = true;
                    }
                }
                for (index, ((source, _, _), request)) in
                    targets.iter().zip(&mut requests).enumerate()
                {
                    if replies[index].is_none() {
                        if let crate::http::Progress::Ready(response) =
                            request.poll(cluster.ring(*source), 32).unwrap()
                        {
                            replies[index] = Some((response.status(), response.body().to_vec()));
                        }
                    }
                }
                if replies.iter().all(Option::is_some) {
                    break;
                }
            }
            assert!(
                replies.iter().all(Option::is_some),
                "diagnostic did not terminate within 2000 ticks"
            );
            assert_eq!(
                cluster.hits.borrow().len() - hits,
                targets.len(),
                "each cold page reaches its owner origin exactly once"
            );
            let events = world.events();
            assert!(
                !events.iter().any(|event| event.tick >= start
                    && matches!(
                        event.kind,
                        "candidate" | "http-timeout" | "resource-exhausted"
                    )),
                "no candidate retries or transport deadline may mask the cycle"
            );
            for event in events.iter().filter(|event| {
                event.tick >= start
                    && matches!(
                        event.kind,
                        "resource-exhausted" | "cache-error" | "candidate" | "http-timeout"
                    )
            }) {
                eprintln!(
                    "opposite={opposite} t={} node={:?} {} target={} {}",
                    event.tick, event.node, event.kind, event.target, event.detail
                );
            }
            let replies: Vec<_> = replies.into_iter().map(Option::unwrap).collect();
            for (status, body) in &replies {
                if *status == 200 {
                    assert_eq!(body, b"abc", "successful pages must retain exact bytes");
                }
            }
            eprintln!(
                "opposite={opposite} released={released} finished={} origin_payload_requests={} statuses={:?}",
                world.tick(),
                cluster.hits.borrow().len() - hits,
                replies.iter().map(|reply| reply.0).collect::<Vec<_>>()
            );
            for node in 0..8 {
                assert_exhaustions(&cluster.cache(node), None);
                let registry = crate::metrics::Registry::new(
                    1,
                    std::sync::Arc::new(crate::control::Updates::default()),
                );
                let cache = cluster.cache(node);
                registry.register(0, cache.metrics());
                cache.metrics().publish();
                for line in registry.render().lines().filter(|line| {
                    line.starts_with("racer_dataplane_cache_resource_exhaustions_total{")
                }) {
                    eprintln!("node={node} {line}");
                }
            }
            drop(requests);
            for (node, mut ring, mut server) in origins {
                let _scope = world.scoped_node(Some(node));
                server.shutdown(&mut ring).unwrap();
                ring.shutdown().unwrap();
                ring.pool().assert_recovered();
            }
            crate::runtime::tests::dst::clean_repro(cluster, &world);
            // A diagnostic failure stays red. No retries, status allowances, or
            // weakening of the exact-byte success oracle hide the observed 503.
            for ((source, owner, target), reply) in targets.iter().zip(replies) {
                assert_eq!(
                    reply,
                    (200, b"abc".to_vec()),
                    "opposite={opposite} {source}->{owner} {target}"
                );
            }
        }
    }

    #[test]
    fn exhaustion_counts_terminal_site_without_resetting_shared_budget() {
        for release_buffer in [false, true] {
            let world = crate::simulation::World::new(419);
            let _scope = world.enter();
            let pool = buffers::io_test_pool(1);
            let mut ring = Ring::http_test_ring(pool.clone(), uring::Config::default()).unwrap();
            let disk = crate::simulation::Disk::new(32 * 1024 * 1024);
            let mut slab =
                allocator::Slab::simulated(disk.clone(), 32 * 1024 * 1024, 1, true).unwrap();
            let mut cache = cache_from_slab(&mut slab, 1, allocator::Config::default());
            cache
                .set_limits(Limits {
                    resource_retries: 2,
                    ..Limits::default()
                })
                .unwrap();
            let mut upstream = scoped_fake();
            let meta = metadata(&cache, "/mixed-pressure", 3, 0);
            let held = pool.private_fill().unwrap();
            let began = world.now();
            let fault = cache
                .page(&meta, 0, began + Duration::from_secs(5))
                .unwrap();
            let (mut fault, work) =
                pending(cache.poll_value(fault, &mut ring, &mut upstream).unwrap());
            assert_exhaustions(&cache, None);
            world.advance(work.deadline.unwrap().duration_since(world.now()));
            let held = if release_buffer {
                drop(held);
                disk.set_available_bytes(0);
                // The first retry was spent on the receive buffer. Complete the
                // receive, then spend the remaining retry on allocator admission.
                (fault, _) = pending(cache.poll_value(fault, &mut ring, &mut upstream).unwrap());
                None
            } else {
                Some(held)
            };
            let (fault, work) = pending(cache.poll_value(fault, &mut ring, &mut upstream).unwrap());
            assert_exhaustions(&cache, None);
            world.advance(work.deadline.unwrap().duration_since(world.now()));
            let error = admission_failure(cache.poll_value(fault, &mut ring, &mut upstream));
            assert_eq!(crate::http_auth::failure::error_status(&error), 503);
            assert_eq!(world.now() - began, Duration::from_millis(20));
            assert_eq!(upstream.starts.len(), usize::from(release_buffer));
            assert_exhaustions(
                &cache,
                Some(if release_buffer {
                    "payload_admission"
                } else {
                    "receive_buffer"
                }),
            );
            drop(held);
            disk.set_available_bytes(u64::MAX);
            cache.shutdown(&mut ring).unwrap();
            ring.shutdown().unwrap();
            pool.assert_recovered();
            drop((cache, slab, ring, pool, upstream));
            world.assert_clean();
        }
    }
}
