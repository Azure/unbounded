// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[test]
fn saturated_relay_receives_can_starve_an_independent_owner_fault() {
    // Isolate receive admission from network speed and page size. A relay's
    // upstream exchange owns its destination while its next hop resolves.
    // Preserve the original zero-reserve reproduction and its protected-slot control.
    for reserve in [0, 1] {
        let mut ring = match Ring::http_test_ring(
            buffers::io_test_pool_config(buffers::Config::new(
                std::num::NonZeroUsize::new(4).unwrap(),
            )),
            uring::Config::default(),
        ) {
            Ok(ring) => ring,
            Err(error)
                if matches!(
                    error.raw_os_error(),
                    Some(libc::EPERM | libc::ENOSYS | libc::ENOMEM)
                ) || error.kind() == io::ErrorKind::Unsupported =>
            {
                assert!(
                    std::env::var_os("RACER_REQUIRE_URING").is_none(),
                    "io_uring required: {error}"
                );
                eprintln!("SKIP relay admission test: {error}");
                return;
            }
            Err(error) => panic!("ring: {error}"),
        };
        let pool = ring.pool().clone();
        let mut cache = cache(1);
        let mut relay = Fake {
            peer: true,
            receive_reserve: reserve,
            replies: (0..4).map(|_| Reply::Hold).collect(),
            ..Fake::default()
        };
        let mut held = Vec::new();
        for index in 0..4 {
            let meta = metadata(&cache, &format!("/relay-holder-{index}"), 3, 0);
            let fault = cache.page(&meta, 0, deadline()).unwrap();
            let (fault, _) = pending_fault(&mut cache, &mut ring, &mut relay, fault);
            held.push(fault);
        }
        assert_eq!(relay.starts.len(), 4 - reserve);
        let mut owner = Fake::default();
        let meta = metadata(&cache, "/independent-owner", 3, 0);
        let fault = cache.page(&meta, 0, deadline()).unwrap();
        let end = fault.deadline();
        let (mut fault, _) = pending_fault(&mut cache, &mut ring, &mut owner, fault);
        if reserve == 0 {
            assert!(
                pool.private_fill().is_err(),
                "relay receives did not fill the pool"
            );
            assert!(
                fault.buffer_wait.is_some(),
                "initial owner wait missing, starts={}",
                owner.starts.len()
            );
            // Polling the reactor cannot make progress: all capacity is held by
            // unresolved peer receives, and even the final owner cannot start.
            for _ in 0..128 {
                ring.progress().unwrap();
                cache.poll(&mut ring, 16).unwrap();
                let (next, work) = pending_fault(&mut cache, &mut ring, &mut owner, fault);
                fault = next;
                assert!(!work.runnable);
                assert_eq!(work.deadline, Some(end));
                assert!(
                    fault.buffer_wait.is_some(),
                    "owner wait disappeared, starts={}, acquiring={}",
                    owner.starts.len(),
                    matches!(fault.state, Loading::Acquire)
                );
                assert_eq!(fault.resource_retries, 0);
                assert!(owner.starts.is_empty());
            }
            // Cancellation, rather than downstream service, breaks the hold.
            drop(held.pop());
            (fault, _) = pending_fault(&mut cache, &mut ring, &mut owner, fault);
        }
        assert_eq!(owner.starts.len(), 1);
        assert!(fault.buffer_wait.is_none());
        drop((fault, held));
        cache.shutdown(&mut ring).unwrap();
        ring.shutdown().unwrap();
        pool.assert_recovered();
    }
}

fn buffer_backpressure() {
    let mut ring =
        Ring::http_test_ring(buffers::io_test_pool(1), uring::Config::default()).unwrap();
    let pool = ring.pool().clone();
    let mut cache = cache(1);
    let mut upstream = Fake::default();
    let meta = metadata(&cache, "/buffer-backpressure", 3, now() + 60);
    let end = deadline();
    let held = pool.private_fill().unwrap().into_compute();
    let fault = cache.page(&meta, 0, end).unwrap();
    let (mut fault, work) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
    assert!(!work.runnable);
    assert_eq!(work.deadline, Some(end));
    assert!(!fault.can_prefetch());

    // Exceed the old timed retry budget, including many unrelated reactor polls.
    let until = Instant::now() + Duration::from_millis(400);
    while Instant::now() < until {
        let (next, work) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
        fault = next;
        assert!(!work.runnable);
        assert_eq!(work.deadline, Some(end));
        assert_eq!(fault.resource_retries, 0);
        assert_eq!(fault.resource_polls, 0);
        assert!(upstream.starts.is_empty());
        thread::sleep(Duration::from_millis(1));
    }
    // A real worker sleeps until another thread releases the final holder.
    ring.progress().unwrap();
    let releaser = thread::spawn(move || {
        thread::sleep(Duration::from_millis(30));
        drop(held);
    });
    ring.wait(work.deadline).unwrap();
    ring.progress().unwrap();
    releaser.join().unwrap();
    assert!(
        Instant::now() < end,
        "buffer release failed to wake the ring"
    );
    let (fault, _) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
    assert!(fault.can_prefetch());
    assert_eq!(upstream.starts.len(), 1);
    let (value, _) = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
    let bytes = value.as_slice().to_vec();
    drop(value);

    // A disk hit needing registered storage uses the same deadline-only wait.
    let held = pool.private_fill().unwrap();
    let mut fault = cache.page(&meta, 0, deadline()).unwrap();
    loop {
        let (next, _) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
        fault = next;
        if fault.buffer_wait.is_some() {
            break;
        }
        ring.progress().unwrap();
        cache.poll(&mut ring, 16).unwrap();
    }
    assert!(matches!(fault.state, Loading::File(_)));
    assert!(!fault.can_prefetch());
    let (fault, work) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
    assert!(!work.runnable);
    assert_eq!(work.deadline, Some(fault.deadline()));
    assert_eq!(fault.resource_retries, 0);
    drop(held);
    let (value, disk) = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
    assert!(disk);
    assert_eq!(value.as_slice(), bytes);
    assert_eq!(upstream.starts.len(), 1);
    drop(value);
    cache.shutdown(&mut ring).unwrap();
    pool.assert_recovered();

    // Caller and candidate expiry both relinquish capacity without peer blame.
    for candidate in [false, true] {
        let mut cache = self::cache(1);
        let meta = metadata(&cache, "/buffer-expiry", 3, 0);
        let mut upstream = Fake {
            peer: true,
            proven: true,
            candidate_cap: candidate.then_some(Duration::from_millis(30)),
            ..Fake::default()
        };
        let held = pool.private_fill().unwrap();
        let end = if candidate {
            deadline()
        } else {
            Instant::now() + Duration::from_millis(30)
        };
        let fault = cache.page(&meta, 0, end).unwrap();
        let (fault, work) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
        let effective = fault.deadline();
        assert_eq!(work.deadline, Some(effective));
        if candidate {
            assert!(effective < end);
        }
        thread::sleep(effective.saturating_duration_since(Instant::now()));
        assert!(matches!(
            cache.poll_fault(fault, &mut ring, &mut upstream),
            Err(Error::Timeout)
        ));
        assert!(upstream.starts.is_empty());
        assert_eq!(upstream.advances, 0);
        let diagnostics = upstream.diagnostics.borrow();
        let event = diagnostics.last().expect("expiry must retain causal state");
        assert_eq!(event.state, "acquire");
        assert!(event.buffer_wait);
        assert_eq!(event.offset, Some(0));
        assert_eq!(event.key.len(), 64);
        assert_eq!(event.candidate_remaining_ms, 0);
        if candidate {
            assert_eq!(event.site, "candidate_before_step");
            assert!(event.caller_remaining_ms > 0);
        } else {
            assert_eq!(event.site, "caller_before_poll");
            assert_eq!(event.caller_remaining_ms, 0);
        }
        drop(diagnostics);
        drop(held);
        cache.shutdown(&mut ring).unwrap();
        pool.assert_recovered();
    }

    // Producer cancellation transfers the shared flight to a surviving consumer.
    let mut cache = self::cache(1);
    let meta = metadata(&cache, "/buffer-takeover", 3, 0);
    let mut upstream = Fake::default();
    let held = pool.private_fill().unwrap();
    let producer = cache.page(&meta, 0, deadline()).unwrap();
    let consumer = cache.page(&meta, 0, deadline()).unwrap();
    let (producer, _) = pending_fault(&mut cache, &mut ring, &mut upstream, producer);
    let (consumer, _) = pending_fault(&mut cache, &mut ring, &mut upstream, consumer);
    drop(held); // Producer owns an unclaimed grant when canceled.
    drop(producer);
    let (value, _) = resolve_checked(&mut cache, &mut ring, &mut upstream, consumer);
    assert_eq!(upstream.starts.len(), 1);
    drop(value);
    cache.shutdown(&mut ring).unwrap();
    pool.assert_recovered();

    // Retired peer I/O can retain its old destination while retry acquisition parks.
    let mut cache = self::cache(1);
    let meta = metadata(&cache, "/buffer-peer-retry", 3, 0);
    let mut upstream = Fake::peer([Reply::RetryPeer, Reply::Good]);
    let end = deadline();
    let fault = cache.page(&meta, 0, end).unwrap();
    let (fault, _) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
    let (fault, _) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
    assert!(matches!(fault.state, Loading::RetryPeer { .. }));
    let (mut fault, work) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
    assert!(!work.runnable);
    assert_eq!(work.deadline, Some(end));
    assert!(!fault.can_prefetch());
    for _ in 0..5000 {
        (fault, _) = pending_fault(&mut cache, &mut ring, &mut upstream, fault);
    }
    assert_eq!(fault.resource_retries, 0);
    assert!(upstream.resumes.is_empty());
    drop(upstream.held.take());
    let (value, _) = resolve_checked(&mut cache, &mut ring, &mut upstream, fault);
    assert_eq!(upstream.resumes, [end]);
    assert_eq!(upstream.starts.len(), 1);
    drop(value);
    cache.shutdown(&mut ring).unwrap();
    pool.assert_recovered();
    ring.shutdown().unwrap();
}
