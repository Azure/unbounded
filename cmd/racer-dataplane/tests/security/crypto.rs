mod regression_tests {
    use super::*;
    use buffers::Key;
    use std::sync::atomic::AtomicUsize;

    fn wait(mut ready: impl FnMut() -> bool) {
        let deadline = Instant::now() + Duration::from_secs(5);
        while !ready() {
            assert!(Instant::now() < deadline, "completion timed out");
            thread::yield_now();
        }
    }

    fn fill(buffers: &buffers::WorkerPool, id: u8) -> Fill {
        let mut fill = buffers.stage(Key::new([id; 32])).unwrap();
        fill.as_mut_slice()[..16].fill(42);
        fill
    }

    fn attach(pool: &Pool, buffers: &buffers::WorkerPool) -> (Worker, Source) {
        pool.attach_local(buffers, Arc::new(uring::Wake::new().unwrap()))
            .unwrap()
    }

    fn pool(buffers: &buffers::WorkerPool, limit: usize) -> Pool {
        Pool::start_on(
            &[(workers::CpuId(0), buffers.numa_node_id())],
            PoolConfig {
                max_outstanding_per_worker: NonZeroUsize::new(limit).unwrap(),
            },
            |_| Ok(()),
        )
        .unwrap()
    }

    // Drive the real admission, execution, publication and cleanup operations in a
    // chosen order. No sleeps or probabilistic "running" jobs are needed to cover
    // cancellation on either side of the result-publication boundary.
    fn manual_pool(buffers: &buffers::WorkerPool, limit: usize) -> Pool {
        let queue = Arc::new(Queue {
            jobs: Mutex::new(VecDeque::new()),
            changed: Condvar::new(),
            stopped: AtomicBool::new(false),
            outstanding: AtomicUsize::new(0),
            limit,
            slots: Mutex::new(Vec::new()),
            endpoints: Mutex::new(Vec::new()),
            cleanup: Mutex::new(VecDeque::new()),
        });
        Pool {
            queues: BTreeMap::from([(buffers.numa_node_id(), queue)]),
            threads: vec![],
            limit,
        }
    }

    fn pop(queue: &Queue) -> Job {
        queue.jobs.lock().unwrap().pop_front().unwrap()
    }

    fn clean(queue: &Queue) {
        loop {
            let output = queue.cleanup.lock().unwrap().pop_front();
            let Some(output) = output else { break };
            queue.clean(output);
        }
    }

    #[derive(Default)]
    struct CountWake(AtomicUsize);
    impl std::task::Wake for CountWake {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }

    #[test]
    fn checksum_worker_pins_original_and_retains_crc() {
        let buffers = buffers::io_test_pool(1);
        let pool = manual_pool(&buffers, 1);
        let (mut worker, source) = attach(&pool, &buffers);
        let crc = allocator::crc64(&[42; 16]);
        for expected in [None, Some(crc)] {
            let mut fill = fill(&buffers, 1);
            let ptr = fill.as_mut_slice().as_ptr();
            let mut ticket = worker
                .checksum(fill, 16, expected)
                .unwrap_or_else(|_| panic!("checksum rejected"));
            assert!(buffers.private_fill().is_err(), "all pool slots pinned");
            assert!(worker.take_checksum(&mut ticket).is_none());
            let job = pop(&worker.queue);
            run_job(&worker.queue, job);
            let (mut checked, len, actual) = worker.take_checksum(&mut ticket).unwrap().unwrap();
            assert_eq!(checked.as_mut_slice().as_ptr(), ptr);
            assert_eq!((len, actual), (16, crc));
            drop(checked);
        }
        let mut ticket = worker
            .checksum(fill(&buffers, 2), 16, Some(crc ^ 1))
            .unwrap_or_else(|_| panic!("checksum"));
        run_job(&worker.queue, pop(&worker.queue));
        assert!(matches!(
            worker.take_checksum(&mut ticket),
            Some(Err(Error::Authentication))
        ));
        clean(&worker.queue);
        drop((ticket, worker, source));
        pool.shutdown().unwrap();
    }

    #[test]
    fn cancelled_checksum_keeps_lease_alive_until_worker_completion() {
        let buffers = buffers::io_test_pool(1);
        let pool = manual_pool(&buffers, 1);
        let (mut worker, mut source) = attach(&pool, &buffers);
        let ticket = worker
            .checksum(fill(&buffers, 1), 16, None)
            .unwrap_or_else(|_| panic!("checksum"));
        let job = pop(&worker.queue);
        drop(ticket);
        source.close();
        assert!(buffers.private_fill().is_err());
        run_job(&worker.queue, job);
        clean(&worker.queue);
        assert_eq!(worker.queue.outstanding.load(Ordering::Acquire), 0);
        assert!(buffers.private_fill().is_ok());
        pool.shutdown().unwrap();
    }

    #[test]
    fn simulated_cancellation_boundaries_preserve_crc_and_ownership() {
        for boundary in 0..=3 {
            for close_source in [false, true] {
                let world = crate::simulation::World::new(91);
                let _scope = world.enter();
                let buffers = buffers::test_pool(
                    buffers::Config::new(NonZeroUsize::new(1).unwrap()),
                    workers::NumaNodeId(0),
                    true,
                );
                let pool = Pool::test_pool(&buffers);
                let (mut worker, mut source) = attach(&pool, &buffers);
                let original = fill(&buffers, 1);
                let address = original.region().region.address;
                let mut ticket = Some(
                    worker
                        .checksum(original, 16, None)
                        .unwrap_or_else(|_| panic!("checksum")),
                );
                let step = || {
                    let due = world.next_task_tick().expect("compute event");
                    world.advance(Duration::from_millis(due - world.tick()));
                    world.run_tasks();
                };
                // queued, running, completed, successful consumption
                for _ in 0..boundary.min(2) {
                    step();
                }
                assert_eq!(buffers.invariant_snapshot().refs, [1]);
                assert!(buffers.private_fill().is_err());
                if boundary < 2 {
                    assert!(worker.take_checksum(ticket.as_mut().unwrap()).is_none());
                } else {
                    let guard = ticket.as_ref().unwrap().0.slot.result.lock().unwrap();
                    let Some(Ok(Output::Checked(_, len, crc))) = guard.as_ref() else {
                        panic!("missing CRC")
                    };
                    assert_eq!((*len, *crc), (16, allocator::crc64(&[42; 16])));
                }
                if boundary == 3 {
                    let (fill, len, crc) = worker
                        .take_checksum(ticket.as_mut().unwrap())
                        .unwrap()
                        .unwrap();
                    assert_eq!(fill.region().region.address, address);
                    let buffer = fill.publish_checked(len, crc).unwrap();
                    assert_eq!(buffer.as_slice(), &[42; 16]);
                    assert_eq!(buffer.checksum(), Some(allocator::crc64(buffer.as_slice())));
                    drop(buffer);
                } else {
                    if close_source {
                        source.close();
                    } else {
                        drop(ticket.take());
                    }
                    if boundary < 2 {
                        assert!(buffers.private_fill().is_err(), "premature recycling");
                    }
                    for _ in boundary..2 {
                        step();
                    }
                }
                drop(ticket);
                source.poll_ready(8);
                drop((worker, source));
                pool.shutdown().unwrap();
                buffers.assert_recovered();
                world.assert_clean();
            }
        }
    }

    #[test]
    fn source_drop_and_shutdown_reclaim_completed_checksums_with_live_owners() {
        for shutdown in [false, true] {
            let buffers = buffers::io_test_pool(2);
            let pool = pool(&buffers, 1);
            let (mut worker, mut source) = attach(&pool, &buffers);
            let (mut other, mut other_source) = attach(&pool, &buffers);
            let mut ticket = worker
                .checksum(fill(&buffers, 1), 16, None)
                .unwrap_or_else(|_| panic!("checksum rejected"));
            wait(|| ticket.0.slot.result.lock().unwrap().is_some());
            let wake = Arc::new(CountWake::default());
            let waker = Waker::from(wake.clone());
            assert!(
                other
                    .poll_capacity(&mut Context::from_waker(&waker))
                    .is_pending()
            );
            if shutdown {
                source.close(); // CompletionSource::shutdown delegates here.
            } else {
                drop(source);
            }
            wait(|| worker.queue.outstanding.load(Ordering::Acquire) == 0);
            other_source.poll_ready(1);
            assert_eq!(wake.0.load(Ordering::Relaxed), 1);
            assert_eq!(other.available(), Ok(()));
            assert_eq!(worker.available(), Err(Error::Closed));
            assert!(matches!(
                worker.take_checksum(&mut ticket),
                Some(Err(Error::Closed))
            ));
            let _reused = buffers.stage(Key::new([2; 32])).unwrap();
            pool.shutdown().unwrap();
        }
    }

    #[test]
    fn dropped_ticket_then_source_releases_capacity_without_dropping_worker() {
        let buffers = buffers::io_test_pool(2);
        let pool = pool(&buffers, 1);
        let (mut worker, source) = attach(&pool, &buffers);
        let (other, _source) = attach(&pool, &buffers);
        let ticket = worker
            .checksum(fill(&buffers, 1), 16, None)
            .unwrap_or_else(|_| panic!("checksum"));
        wait(|| ticket.0.slot.result.lock().unwrap().is_some());
        drop(ticket);
        drop(source);
        wait(|| other.available() == Ok(()));
        assert!(worker.slots.borrow().is_empty());
        pool.shutdown().unwrap();
    }

    #[test]
    fn cancellation_before_execution_during_execution_and_after_completion() {
        for phase in 0..3 {
            for close_source in [false, true] {
                let buffers = buffers::io_test_pool(2);
                let pool = manual_pool(&buffers, 1);
                let (mut worker, mut source) = attach(&pool, &buffers);
                let ticket = worker
                    .checksum(fill(&buffers, 1), 16, None)
                    .unwrap_or_else(|_| panic!("checksum"));
                let slot = ticket.0.slot.clone();
                let job = pop(&worker.queue);
                let mut ticket = Some(ticket);
                let mut cancel = || {
                    if close_source {
                        source.close();
                    } else {
                        drop(ticket.take());
                    }
                };
                if phase == 0 {
                    cancel();
                    run_job(&worker.queue, job);
                } else {
                    let result = execute(job.input);
                    if phase == 1 {
                        cancel();
                    }
                    complete(&worker.queue, &job.slot, &job.endpoint, result);
                    if phase == 2 {
                        cancel();
                    }
                    drop(job.slot);
                }
                drop(ticket);
                source.poll_ready(1);
                // A retained result-slot observer must not prevent source teardown
                // from cleaning output. Normal ticket cancellation is polled.
                drop(slot);
                clean(&worker.queue);
                assert_eq!(worker.queue.outstanding.load(Ordering::Acquire), 0);
                assert!(worker.slots.borrow().is_empty());
                pool.shutdown().unwrap();
            }
        }
    }

    #[test]
    fn shared_capacity_wakes_and_source_poll_obeys_budget() {
        let buffers = buffers::io_test_pool(3);
        let pool = manual_pool(&buffers, 2);
        let (mut worker, mut source) = attach(&pool, &buffers);
        let (mut other, mut other_source) = attach(&pool, &buffers);
        let a = worker
            .checksum(fill(&buffers, 1), 16, None)
            .unwrap_or_else(|_| panic!("checksum"));
        let b = worker
            .checksum(fill(&buffers, 2), 16, None)
            .unwrap_or_else(|_| panic!("checksum"));
        run_job(&worker.queue, pop(&worker.queue));
        run_job(&worker.queue, pop(&worker.queue));
        let wake = Arc::new(CountWake::default());
        let waker = Waker::from(wake.clone());
        assert!(
            other
                .poll_capacity(&mut Context::from_waker(&waker))
                .is_pending()
        );
        drop((a, b));
        assert!(!source.poll_ready(0).runnable);
        assert_eq!(worker.slots.borrow().len(), 2);
        assert!(source.poll_ready(1).runnable);
        assert_eq!(worker.slots.borrow().len(), 1);
        assert_eq!(other.available(), Err(Error::WouldBlock));
        clean(&worker.queue);
        other_source.poll_ready(1);
        assert_eq!(wake.0.load(Ordering::Relaxed), 1);
        assert!(matches!(
            other.poll_capacity(&mut Context::from_waker(&waker)),
            Poll::Ready(Ok(()))
        ));
        assert!(source.poll_ready(1).runnable);
        assert!(!source.poll_ready(1).runnable);
        clean(&worker.queue);
        assert_eq!(worker.queue.outstanding.load(Ordering::Acquire), 0);
        pool.shutdown().unwrap();
    }

    #[test]
    fn queued_cancellation_does_not_kill_compute_worker() {
        for phase in 0..4 {
            let buffers = buffers::io_test_pool(3);
            let pool = manual_pool(&buffers, 2);
            let (mut worker, mut source) = attach(&pool, &buffers);
            let ticket = worker
                .checksum(fill(&buffers, 1), 16, None)
                .unwrap_or_else(|_| panic!("checksum"));
            if phase == 0 {
                drop(ticket);
            } else if phase == 1 {
                run_job(&worker.queue, pop(&worker.queue));
                drop(ticket);
            } else if phase == 2 {
                let job = pop(&worker.queue);
                let result = execute(job.input);
                drop(ticket);
                complete(&worker.queue, &job.slot, &job.endpoint, result);
            } else {
                run_job(&worker.queue, pop(&worker.queue));
                source.close();
            }
            let queue = worker.queue.clone();
            let thread = thread::spawn(move || compute_loop(queue));
            wait(|| {
                source.poll_ready(2);
                worker.queue.outstanding.load(Ordering::Acquire) == 0
            });
            buffers.assert_recovered();
            let (mut worker, _next_source) = attach(&pool, &buffers);
            let mut next = worker
                .checksum(fill(&buffers, 2), 16, None)
                .unwrap_or_else(|_| panic!("checksum"));
            wait(|| next.0.slot.result.lock().unwrap().is_some());
            let (fill, len, crc) = worker.take_checksum(&mut next).unwrap().unwrap();
            drop(fill.publish_checked(len, crc).unwrap());
            pool.shutdown().unwrap();
            thread.join().unwrap();
        }
    }

    #[test]
    fn foreign_owner_and_length_rejections_preserve_resources() {
        let buffers = buffers::io_test_pool(6);
        let foreign = buffers::io_test_pool(2);
        let pool = manual_pool(&buffers, 4);
        let (mut worker, _source) = attach(&pool, &buffers);
        let (mut other, _other_source) = attach(&pool, &buffers);
        let rejected = worker.checksum(fill(&foreign, 1), 16, None).err().unwrap();
        assert_eq!(rejected.error, Error::ForeignOwner);
        assert!(foreign.owns_fill(&rejected.resource));
        drop(rejected);
        let rejected = worker
            .checksum(fill(&buffers, 1), buffers::BUFFER_SIZE + 1, None)
            .err()
            .unwrap();
        assert_eq!(rejected.error, Error::Invalid);
        let mut ticket = worker
            .checksum(rejected.resource, 16, None)
            .unwrap_or_else(|_| panic!("checksum"));
        assert!(matches!(
            other.take_checksum(&mut ticket),
            Some(Err(Error::ForeignOwner))
        ));
        run_job(&worker.queue, pop(&worker.queue));
        let (fill, len, crc) = worker.take_checksum(&mut ticket).unwrap().unwrap();
        assert!(matches!(
            worker.take_checksum(&mut ticket),
            Some(Err(Error::Invalid))
        ));
        let buffer = fill.publish_checked(len, crc).unwrap();
        assert_eq!(buffer.checksum(), Some(allocator::crc64(buffer.as_slice())));
        assert_eq!(worker.queue.outstanding.load(Ordering::Acquire), 0);
        pool.shutdown().unwrap();
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use buffers::Key;
    pub(crate) fn trust(generation: u64) -> (crate::signing::Keys, Snapshot) {
        let seed = [generation as u8; 32];
        let public = ed25519_dalek::SigningKey::from_bytes(&seed)
            .verifying_key()
            .to_bytes();
        let keys = crate::signing::Keys::new(Some(seed), vec![public]).unwrap();
        let snapshot = Snapshot::signed(UniverseId([8; 32]), keys.clone());
        (keys, snapshot)
    }
    fn fill(pool: &buffers::WorkerPool, id: u8, len: usize) -> Fill {
        let mut fill = pool.stage(Key::new([id; 32])).unwrap();
        fill.as_mut_slice()[..len].fill(42);
        fill
    }
    fn wait<T>(mut f: impl FnMut() -> Option<T>) -> T {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if let Some(t) = f() {
                return t;
            }
            assert!(Instant::now() < deadline);
            thread::yield_now();
        }
    }
    fn sessions(snapshot: &Snapshot) -> (auth::Session, auth::Session) {
        let expected = auth::PeerContext::new([1; 32], [2; 32]).unwrap();
        let (initiator, hello) = auth::Initiator::start(
            snapshot.clone(),
            expected.clone(),
            None,
            Duration::from_secs(1),
        )
        .unwrap();
        let (responder, reply) = auth::Responder::accept(
            snapshot.clone(),
            expected,
            hello,
            None,
            Duration::from_secs(1),
        )
        .unwrap();
        let (a, finish) = initiator.finish(reply).unwrap();
        (a, responder.finish(finish).unwrap())
    }
    #[test]
    fn handshake_and_bidirectional_controls_use_distinct_private_keys() {
        let snapshot = |local, remote| {
            let public = ed25519_dalek::SigningKey::from_bytes(&[remote; 32])
                .verifying_key()
                .to_bytes();
            Snapshot::signed(
                UniverseId::new([8; 32]),
                crate::signing::Keys::new(Some([local; 32]), vec![public]).unwrap(),
            )
        };
        let a_keys = snapshot(1, 2);
        let b_keys = snapshot(2, 1);
        let context = auth::PeerContext::new([1; 32], [2; 32]).unwrap();
        let (initiator, hello) = auth::Initiator::start(
            a_keys.clone(),
            context.clone(),
            None,
            Duration::from_secs(1),
        )
        .unwrap();
        let (responder, reply) =
            auth::Responder::accept(b_keys.clone(), context, hello, None, Duration::from_secs(1))
                .unwrap();
        let (mut a, finish) = initiator.finish(reply).unwrap();
        let mut b = responder.finish(finish).unwrap();
        let request = a
            .sign(&a_keys, auth::Control::new(1, b"request".to_vec()).unwrap())
            .unwrap();
        assert_eq!(b.verify(&b_keys, request).unwrap().body(), b"request");
        let response = b
            .sign(&b_keys, auth::Control::new(1, b"reply".to_vec()).unwrap())
            .unwrap();
        assert_eq!(a.verify(&a_keys, response).unwrap().body(), b"reply");
    }
    #[test]
    fn handshake_and_control_reject_tamper_replay_reflection_and_key_change() {
        let (_, snapshot) = trust(1);
        let (mut a, mut b) = sessions(&snapshot);
        let signed = a
            .sign(
                &snapshot,
                auth::Control::new(17, b"rkey,descriptor,length".to_vec()).unwrap(),
            )
            .unwrap();
        let original = signed.encode().to_vec();
        let mut tampered = original.clone();
        tampered[8] ^= 1;
        assert!(
            b.verify(&snapshot, auth::SignedControl::decode(&tampered).unwrap())
                .is_err()
        );
        assert!(
            a.verify(&snapshot, auth::SignedControl::decode(&original).unwrap())
                .is_err()
        );
        let verified = b.verify(&snapshot, signed).unwrap();
        assert_eq!(verified.request_id(), 17);
        assert_eq!(verified.body(), b"rkey,descriptor,length");
        assert!(
            b.verify(&snapshot, auth::SignedControl::decode(&original).unwrap())
                .is_err()
        );
        let (_, mut another) = sessions(&snapshot);
        assert!(
            another
                .verify(&snapshot, auth::SignedControl::decode(&original).unwrap())
                .is_err()
        );
        let (_, updated) = trust(2);
        assert!(
            a.sign(&updated, auth::Control::new(1, vec![]).unwrap())
                .is_err()
        );
        let expected = auth::PeerContext::new([1; 32], [2; 32]).unwrap();
        let (initiator, hello) =
            auth::Initiator::start(snapshot.clone(), expected, None, Duration::from_secs(1))
                .unwrap();
        let wrong = auth::PeerContext::new([2; 32], [1; 32]).unwrap();
        let (_, reply) =
            auth::Responder::accept(snapshot, wrong, hello, None, Duration::from_secs(1)).unwrap();
        assert!(initiator.finish(reply).is_err());
    }
    #[test]
    fn offload_full_payload_atomic_checksum_and_backpressure() {
        let buffers = buffers::io_test_pool(4);
        let pool = Pool::start_on(
            &[(workers::CpuId(0), buffers.numa_node_id())],
            PoolConfig {
                max_outstanding_per_worker: NonZeroUsize::new(1).unwrap(),
            },
            |_| Ok(()),
        )
        .unwrap();
        let (mut worker, source) = pool
            .attach_local(&buffers, Arc::new(uring::Wake::new().unwrap()))
            .unwrap();
        let len = buffers::BUFFER_SIZE;
        let mut ticket = worker
            .checksum(fill(&buffers, 1, len), len, None)
            .unwrap_or_else(|_| panic!("checksum rejected"));
        wait(|| ticket.0.slot.result.lock().unwrap().as_ref().map(|_| ()));
        let other = fill(&buffers, 2, 1);
        let rejected = worker.checksum(other, 1, None).err().unwrap();
        assert_eq!(rejected.error, Error::WouldBlock);
        let (checked, len, crc) = worker.take_checksum(&mut ticket).unwrap().unwrap();
        let buffer = checked.publish_checked(len, crc).unwrap();
        assert_eq!(crc, allocator::crc64(buffer.as_slice()));
        assert_eq!(buffer.as_slice(), vec![42; len]);
        assert_eq!(buffer.checksum(), Some(crc));
        let mut next = worker
            .checksum(rejected.resource, 1, None)
            .unwrap_or_else(|_| panic!("checksum"));
        let (_, len, crc) = wait(|| worker.take_checksum(&mut next)).unwrap();
        assert_eq!((len, crc), (1, allocator::crc64(&[42])));
        drop(buffer);
        drop(source);
        drop(worker);
        pool.shutdown().unwrap();
    }
    #[test]
    fn shutdown_reclaims_completed_and_running_jobs_with_live_tickets() {
        let buffers = buffers::io_test_pool(4);
        let pool = Pool::start_on(
            &[(workers::CpuId(0), buffers.numa_node_id())],
            PoolConfig::default(),
            |_| Ok(()),
        )
        .unwrap();
        let (mut worker, source) = pool
            .attach_local(&buffers, Arc::new(uring::Wake::new().unwrap()))
            .unwrap();
        let len = buffers::BUFFER_SIZE;
        let mut completed = worker
            .checksum(fill(&buffers, 1, len), len, None)
            .unwrap_or_else(|_| panic!("checksum"));
        wait(|| completed.0.slot.result.lock().unwrap().as_ref().map(|_| ()));
        let mut running = worker
            .checksum(fill(&buffers, 2, len), len, None)
            .unwrap_or_else(|_| panic!("checksum"));
        pool.shutdown().unwrap();
        assert_eq!(worker.queue.outstanding.load(Ordering::Acquire), 0);
        assert!(matches!(
            worker.take_checksum(&mut completed),
            Some(Err(Error::Closed))
        ));
        assert!(matches!(
            worker.take_checksum(&mut running),
            Some(Err(Error::Closed))
        ));
        assert!(worker.queue.cleanup.lock().unwrap().is_empty());
        drop(source);
    }
    #[test]
    fn compute_leases_survive_dropped_origin_and_never_alias() {
        let buffers = buffers::io_test_pool(1);
        let mut lease = fill(&buffers, 1, 4).into_compute();
        let clone = buffers.clone();
        let (go, wait) = std::sync::mpsc::channel();
        let thread = thread::spawn(move || {
            wait.recv().unwrap();
            lease.bytes()[..4].fill(8);
            lease
        });
        assert!(clone.stage(Key::new([2; 32])).is_err());
        drop(buffers);
        go.send(()).unwrap();
        let buffer = thread.join().unwrap().into_fill().publish(4).unwrap();
        let read = buffer.compute_read();
        drop(buffer);
        assert!(clone.stage(Key::new([2; 32])).is_err());
        thread::spawn(move || {
            assert_eq!(read.bytes(), &[8; 4]);
            drop(read);
        })
        .join()
        .unwrap();
        assert!(clone.stage(Key::new([2; 32])).is_ok());
    }
}
