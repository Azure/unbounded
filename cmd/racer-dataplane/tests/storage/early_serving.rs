// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

fn pending(
    cache: &mut Cache,
    ring: &mut Ring,
    upstream: &mut Fake,
    fault: Fault<Fake>,
) -> Fault<Fake> {
    match cache.poll_value(fault, ring, upstream).unwrap() {
        Progress::Pending { fault, .. } => fault,
        Progress::Ready(_) => panic!("unexpected delivery"),
    }
}

fn early(
    cache: &mut Cache,
    ring: &mut Ring,
    upstream: &mut Fake,
    mut fault: Fault<Fake>,
) -> Buffer {
    for _ in 0..16 {
        match cache.poll_value(fault, ring, upstream).unwrap() {
            Progress::Ready(CachedValue::Buffer(buffer)) => return buffer,
            Progress::Ready(_) => panic!("expected early immutable bytes"),
            Progress::Pending { fault: next, .. } => fault = next,
        }
    }
    panic!("early delivery blocked on storage")
}

#[test]
fn paused_storage_delivers_validated_admitted_shared_bytes_then_prefers_file() {
    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    let io = crate::slab_io::Io::testing(1 << 30, BUFFER_SIZE as u64, true);
    let mut cache = io.scope(|| cache(1));
    let paused = io.exhaust_and_pause_refill();
    let meta = metadata(&cache, "/early", 3, 0);
    let mut upstream = Fake {
        replies: [Reply::Hold].into(),
        ..Fake::default()
    };
    let producer = cache.page(&meta, 0, deadline()).unwrap();
    let key = *producer.key();
    let producer = pending(&mut cache, &mut ring, &mut upstream, producer);
    let consumer = cache.page(&meta, 0, deadline()).unwrap();
    let consumer = pending(&mut cache, &mut ring, &mut upstream, consumer);
    assert_eq!(upstream.starts.len(), 1);
    let producer = pending(&mut cache, &mut ring, &mut upstream, producer);
    assert!(cache.shards[0].allocator.lookup(&key, now()).is_none());
    upstream.release = true;
    let bytes = early(&mut cache, &mut ring, &mut upstream, producer);
    let shared = early(&mut cache, &mut ring, &mut upstream, consumer);
    let local = cache.page(&meta, 0, deadline()).unwrap();
    let local = early(&mut cache, &mut ring, &mut upstream, local);
    assert_eq!(bytes.as_slice(), b"xxx");
    assert_eq!(bytes.as_slice().as_ptr(), shared.as_slice().as_ptr());
    assert_eq!(bytes.as_slice().as_ptr(), local.as_slice().as_ptr());
    assert_eq!(bytes.checksum(), Some(allocator::crc64(b"xxx")));
    let lease = cache.shards[0].allocator.lookup(&key, now()).unwrap();
    for _ in 0..32 {
        cache.poll(&mut ring, 16).unwrap();
        ring.progress().unwrap();
    }
    assert!(
        lease.ready().is_none(),
        "paused write must not become FileReady"
    );
    assert_eq!(upstream.starts.len(), 1);
    drop(paused);
    cache.shutdown(&mut ring).unwrap();
    assert!(lease.ready().is_some());
    let fault = cache.page(&meta, 0, deadline()).unwrap();
    assert!(matches!(
        cache.poll_value(fault, &mut ring, &mut upstream).unwrap(),
        Progress::Ready(CachedValue::File(_))
    ));
    // Shutdown and eviction release cache ownership, not consumer ownership.
    assert!(cache.shards[0].allocator.remove(&key));
    drop((lease, cache));
    assert_eq!(shared.as_slice(), b"xxx");
    drop((bytes, shared, local));
    ring.pool().assert_recovered();
}

#[test]
fn slow_readers_leave_demand_slots_and_budget_fallback_waits_for_file() {
    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    let io = crate::slab_io::Io::testing(1 << 30, BUFFER_SIZE as u64, true);
    let mut cache = io.scope(|| cache(1));
    let paused = io.exhaust_and_pause_refill();
    let mut upstream = Fake::default();
    let mut held = Vec::new();
    // Eight real pool slots yield four physical early-serving permits.
    for index in 0..4 {
        let meta = metadata(&cache, &format!("/slow-{index}"), 3, 0);
        let fault = cache.page(&meta, 0, deadline()).unwrap();
        held.push(early(&mut cache, &mut ring, &mut upstream, fault));
    }
    let reserve: Vec<_> = (0..4)
        .map(|_| ring.pool().private_fill().unwrap())
        .collect();
    assert!(ring.pool().private_fill().is_err());
    drop(reserve);
    let meta = metadata(&cache, "/fallback", 3, 0);
    let mut fault = cache.page(&meta, 0, deadline()).unwrap();
    for _ in 0..8 {
        fault = pending(&mut cache, &mut ring, &mut upstream, fault);
    }
    assert!(fault.early_fallback);
    let mut local = cache.page(&meta, 0, deadline()).unwrap();
    local = pending(&mut cache, &mut ring, &mut upstream, local);
    assert!(
        local.early_fallback,
        "local lookup cannot bypass the pool limit"
    );
    drop(local);
    drop(paused);
    cache.shutdown(&mut ring).unwrap();
    assert!(matches!(
        cache.poll_value(fault, &mut ring, &mut upstream).unwrap(),
        Progress::Ready(CachedValue::File(_))
    ));
    // Slow readers still own four charged slots after storage completion.
    let probe = ring.pool().private_fill().unwrap().publish(0).unwrap();
    assert!(!probe.try_early());
    drop(held);
    assert!(probe.try_early());
    drop(probe);
    ring.pool().assert_recovered();
}

#[test]
fn malformed_and_unadmitted_pages_never_serve_early() {
    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    for reply in [
        Reply::WrongLength(2),
        Reply::WrongRange,
        Reply::ChangedTag,
        Reply::Foreign,
        Reply::WrongKind,
    ] {
        let mut cache = cache(1);
        let meta = metadata(&cache, "/invalid", 3, 0);
        let mut upstream = Fake {
            replies: [reply].into(),
            ..Fake::default()
        };
        let fault = cache.page(&meta, 0, deadline()).unwrap();
        let key = *fault.key();
        let fault = pending(&mut cache, &mut ring, &mut upstream, fault);
        assert!(cache.poll_value(fault, &mut ring, &mut upstream).is_err());
        assert!(cache.shards[0].allocator.lookup(&key, now()).is_none());
    }
    let mut cache = cache(1);
    let meta = metadata(&cache, "/bad-crc", 3, 0);
    let mut upstream = Fake::peer([Reply::Checked(0, b"xxx".to_vec()), Reply::Hold]);
    let fault = cache.page(&meta, 0, deadline()).unwrap();
    let key = *fault.key();
    let mut fault = pending(&mut cache, &mut ring, &mut upstream, fault);
    for _ in 0..8 {
        fault = pending(&mut cache, &mut ring, &mut upstream, fault);
    }
    assert!(cache.shards[0].allocator.lookup(&key, now()).is_none());
    drop(fault);
    ring.pool().assert_recovered();
}

#[test]
fn full_admission_queue_never_delivers_rejected_buffer() {
    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    let path = std::env::temp_dir().join(format!("early-admission-{}", std::process::id()));
    let mut slab = allocator::Slab::create(&path, 8 * BUFFER_SIZE as u64, 1).unwrap();
    std::fs::remove_file(path).unwrap();
    let mut cache = cache_from_slab(
        &mut slab,
        1,
        allocator::Config {
            max_pending_values: 1,
            ..Default::default()
        },
    );
    let mut upstream = Fake::default();
    let meta = metadata(&cache, "/accepted", 3, 0);
    let fault = cache.page(&meta, 0, deadline()).unwrap();
    let accepted = early(&mut cache, &mut ring, &mut upstream, fault);
    let meta = metadata(&cache, "/not-admitted", 3, 0);
    let fault = cache.page(&meta, 0, deadline()).unwrap();
    let key = *fault.key();
    let mut fault = pending(&mut cache, &mut ring, &mut upstream, fault);
    for _ in 0..8 {
        fault = pending(&mut cache, &mut ring, &mut upstream, fault);
    }
    assert!(matches!(fault.state, Loading::Admitting(_)));
    assert!(cache.shards[0].allocator.lookup(&key, now()).is_none());
    drop((fault, accepted));
    cache.shutdown(&mut ring).unwrap();
    ring.pool().assert_recovered();
}

#[test]
fn early_uds_send_delivers_while_slab_write_is_paused() {
    use std::{io::Read, os::unix::net::UnixStream};
    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    let io = crate::slab_io::Io::testing(1 << 30, BUFFER_SIZE as u64, true);
    let mut cache = io.scope(|| cache(1));
    let paused = io.exhaust_and_pause_refill();
    let meta = metadata(&cache, "/uds-early", 3, 0);
    let mut upstream = Fake::default();
    let fault = cache.page(&meta, 0, deadline()).unwrap();
    let key = *fault.key();
    let bytes = early(&mut cache, &mut ring, &mut upstream, fault);
    let lease = cache.shards[0].allocator.lookup(&key, now()).unwrap();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
    let mut send = ring
        .send(
            uring::File::new(socket.into()).into(),
            bytes,
            uring::BufferRange::new(0..3).unwrap(),
        )
        .unwrap();
    let end = Instant::now() + Duration::from_secs(3);
    loop {
        assert!(Instant::now() < end);
        cache.poll(&mut ring, 16).unwrap();
        ring.progress().unwrap();
        if let Some(done) = ring.take_write(&mut send).unwrap() {
            assert_eq!(done.result.unwrap(), 3);
            break;
        }
        thread::yield_now();
    }
    let mut delivered = [0; 3];
    peer.read_exact(&mut delivered).unwrap();
    assert_eq!(&delivered, b"xxx");
    assert!(lease.ready().is_none());
    drop(paused);
    cache.shutdown(&mut ring).unwrap();
    assert!(lease.ready().is_some());
    drop((lease, cache));
    ring.shutdown().unwrap();
    ring.pool().assert_recovered();
}

#[test]
fn early_send_cancellation_retains_budget_until_driver_quiescence() {
    use std::os::unix::net::UnixStream;
    let pool = buffers::io_test_pool(5);
    let mut ring = match Ring::http_test_ring(pool.clone(), Default::default()) {
        Ok(ring) => ring,
        Err(error) => {
            assert!(std::env::var_os("RACER_REQUIRE_URING").is_none(), "{error}");
            eprintln!("SKIP io_uring: {error}");
            return;
        }
    };
    let mut cache = cache(1);
    let meta = metadata(&cache, "/cancel-send", 1 << 20, 0);
    let mut upstream = Fake::default();
    let fault = cache.page(&meta, 0, deadline()).unwrap();
    let key = *fault.key();
    let bytes = early(&mut cache, &mut ring, &mut upstream, fault);
    let (socket, _slow_peer) = UnixStream::pair().unwrap();
    let send = ring
        .send(
            uring::File::new(socket.into()).into(),
            bytes,
            uring::BufferRange::new(0..1 << 20).unwrap(),
        )
        .unwrap();
    ring.progress().unwrap();
    // Evict before maintenance submits any write. Only the actual send owns bytes.
    assert!(cache.shards[0].allocator.remove(&key));
    drop(cache);
    let probe = pool.private_fill().unwrap().publish(0).unwrap();
    assert!(!probe.try_early());
    drop(send.cancel_on_drop());
    assert!(
        !probe.try_early(),
        "dropping a ticket is not a terminal CQE"
    );
    ring.shutdown().unwrap();
    assert!(probe.try_early());
    drop(probe);
    pool.assert_recovered();
}

#[test]
fn retired_unwritten_fallback_fails_instead_of_waiting_forever() {
    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    let mut cache = cache(1);
    let meta = metadata(&cache, "/retired-fallback", 3, 0);
    let mut upstream = Fake::default();
    let mut fault = cache.page(&meta, 0, deadline()).unwrap();
    fault.early_fallback = true;
    let key = *fault.key();
    fault = pending(&mut cache, &mut ring, &mut upstream, fault);
    fault = pending(&mut cache, &mut ring, &mut upstream, fault);
    assert!(matches!(fault.state, Loading::Publishing(_)));
    assert!(cache.shards[0].allocator.remove(&key));
    assert!(
        matches!(cache.poll_value(fault, &mut ring, &mut upstream), Err(error) if matches!(error.root(), Error::Admission(_)))
    );
    drop(cache);
    ring.pool().assert_recovered();
}

#[test]
fn async_checksum_and_cross_cache_consumers_share_early_slot() {
    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    let crypto = crate::crypto::Pool::test_pool(ring.pool());
    let (worker, source) = crypto
        .attach_local(ring.pool(), ring.wake_handle())
        .unwrap();
    let worker = Rc::new(std::cell::RefCell::new(worker));
    let mut first = cache(1);
    let mut second = cache(1);
    // Fixture caches normally use the same namespace but separate owner identities.
    let mut meta = metadata(&first, "/crypto-early", 3, 0);
    meta.context = Context::new(Namespace(first.namespace)).with_crypto(Some(worker.clone()));
    let other_meta = metadata(&second, "/crypto-early", 3, 0);
    let mut upstream = Fake::peer([Reply::Hold]);
    let fault = first.page(&meta, 0, deadline()).unwrap();
    let mut producer = pending(&mut first, &mut ring, &mut upstream, fault);
    let fault = second.page(&other_meta, 0, deadline()).unwrap();
    let consumer = pending(&mut second, &mut ring, &mut upstream, fault);
    upstream.release = true;
    let end = Instant::now() + Duration::from_secs(3);
    let bytes = loop {
        assert!(Instant::now() < end);
        match first
            .poll_value(producer, &mut ring, &mut upstream)
            .unwrap()
        {
            Progress::Ready(CachedValue::Buffer(bytes)) => break bytes,
            Progress::Ready(_) => panic!(),
            Progress::Pending { fault, .. } => producer = fault,
        }
        thread::yield_now();
    };
    let shared = early(&mut second, &mut ring, &mut upstream, consumer);
    assert_eq!(upstream.starts.len(), 1);
    assert_eq!(bytes.as_slice().as_ptr(), shared.as_slice().as_ptr());
    assert_eq!(shared.checksum(), Some(allocator::crc64(b"xxx")));
    let key = meta.page_key(0).unwrap();
    assert!(
        first.shards[0]
            .allocator
            .lookup(&key, now())
            .unwrap()
            .ready()
            .is_none()
    );
    assert!(
        second.shards[0]
            .allocator
            .lookup(&key, now())
            .unwrap()
            .ready()
            .is_none()
    );
    drop((bytes, shared));
    first.shutdown(&mut ring).unwrap();
    second.shutdown(&mut ring).unwrap();
    drop((first, second, meta, worker, source));
    crypto.shutdown().unwrap();
    ring.pool().assert_recovered();
}

#[test]
fn early_budget_survives_abandoned_submitted_write_until_collection() {
    let pool = buffers::io_test_pool(5);
    let mut ring = match Ring::http_test_ring(pool.clone(), Default::default()) {
        Ok(ring) => ring,
        Err(error) => {
            assert!(std::env::var_os("RACER_REQUIRE_URING").is_none(), "{error}");
            eprintln!("SKIP io_uring: {error}");
            return;
        }
    };
    let path = std::env::temp_dir().join(format!("early-write-{}", std::process::id()));
    let file = std::fs::File::create(&path).unwrap();
    std::fs::remove_file(path).unwrap();
    let bytes = pool.private_fill().unwrap().publish(4096).unwrap();
    assert!(bytes.try_early());
    let write = ring
        .write(
            uring::File::new(file.into()).into(),
            bytes,
            uring::BufferRange::new(0..4096).unwrap(),
            uring::FileOffset::new(0).unwrap(),
        )
        .unwrap();
    ring.progress().unwrap();
    let probe = pool.private_fill().unwrap().publish(0).unwrap();
    assert!(!probe.try_early());
    // Abandon observation of an actual submitted write, without canceling it.
    drop(write);
    assert!(!probe.try_early());
    ring.shutdown().unwrap();
    assert!(probe.try_early());
    drop(probe);
    pool.assert_recovered();
}
