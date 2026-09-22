// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod pressure {
    use super::*;

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
        drop(held);
        world.advance(Duration::from_millis(10));
        let (value, _) = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
        assert_eq!(value.as_slice(), b"xxx");
        assert_eq!(upstream.starts.len(), 1);
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
            drop(held);
            cache.shutdown(&mut ring).unwrap();
            ring.shutdown().unwrap();
            pool.assert_recovered();
            drop((cache, slab, ring, pool, upstream));
            world.assert_clean();
        }
    }
}
