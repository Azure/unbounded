// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

fn key(value: u8) -> Key {
    Key::new([value; 32])
}
fn scope(value: u8) -> NetworkFlightKey {
    NetworkFlightKey {
        value: [value; 32],
        routing: [9; 32],
        version: 2,
        destination: 3,
        dependency: NetworkDependency::Canonical { slot: 1 },
    }
}

#[test]
fn reserved_staging_preserves_downstream_ranks_and_final_holder_ownership() {
    let pool = io_test_pool(8);
    let other = pool.test_other_worker();
    let mut distant: Vec<_> = (0..5)
        .map(|_| pool.stage_reserved(key(1), 3).unwrap())
        .collect();
    assert!(other.stage_reserved(key(2), 3).is_err());
    let two = other.stage_reserved(key(2), 2).unwrap();
    assert!(pool.stage_reserved(key(2), 2).is_err());
    let one = pool.stage_reserved(key(3), 1).unwrap();
    assert!(other.stage_reserved(key(3), 1).is_err());
    let owner = other.stage_reserved(key(4), 0).unwrap();
    assert!(pool.private_fill().is_err());
    let (authority, destination) = distant.pop().unwrap().split_destination();
    drop(authority);
    assert!(
        pool.stage_reserved(key(5), 0).is_err(),
        "canceled receive still owns its slot"
    );
    drop(destination);
    assert!(
        pool.stage_reserved(key(5), 1).is_err(),
        "one free slot is still reserved"
    );
    drop((owner, one, two, distant));
    pool.assert_recovered();
    let tiny = io_test_pool(1);
    assert!(tiny.stage_reserved(key(1), 1).is_err());
    drop(tiny.stage_reserved(key(1), 0).unwrap());
    tiny.assert_recovered();
}

#[test]
fn final_live_holder_returns_slot_without_completed_residency() {
    let pool = io_test_pool(1);
    let mut fill = pool.stage(key(1)).unwrap();
    assert!(fill.as_mut_slice().iter().all(|&b| b == 0));
    fill.as_mut_slice()[..4].copy_from_slice(b"data");
    let buffer = fill.publish_checked(4, 73).unwrap();
    let address = buffer.region().region.address;
    let clone = buffer.clone();
    let completion = buffer.compute_read();
    drop(buffer);
    assert!(pool.stage(key(1)).is_err());
    assert_eq!(clone.as_slice(), b"data");
    assert_eq!(clone.checksum(), Some(73));
    drop(clone);
    assert!(pool.private_fill().is_err());
    // The last completion holder may retire on another thread.
    std::thread::spawn(move || {
        assert_eq!(completion.bytes(), b"data");
        drop(completion);
    })
    .join()
    .unwrap();
    let mut next = pool.stage(key(2)).unwrap();
    assert_eq!(next.region().region.address, address);
    assert_eq!(&next.as_mut_slice()[..4], b"data");
    assert!(!next.matches_key(key(1)));
    let next = next.publish(0).unwrap();
    assert_eq!(next.checksum(), None);
    drop(next);
    pool.assert_recovered();
}

#[test]
fn cancellation_error_and_compute_completion_release_exactly_once() {
    let pool = io_test_pool(1);
    for authority_first in [false, true] {
        let (authority, destination) = pool.stage(key(1)).unwrap().split_destination();
        let survivor: Box<dyn std::any::Any> = if authority_first {
            drop(authority);
            Box::new(destination)
        } else {
            drop(destination);
            Box::new(authority)
        };
        assert!(
            pool.private_fill().is_err(),
            "cancellation is not I/O completion"
        );
        drop(survivor);
        pool.assert_recovered();
    }
    assert!(
        pool.private_fill()
            .unwrap()
            .publish(BUFFER_SIZE + 1)
            .is_err()
    );
    pool.assert_recovered();
    let mut compute = pool.stage(key(1)).unwrap().into_compute();
    assert!(pool.private_fill().is_err());
    compute.bytes()[0] = 42;
    let fill = compute.into_fill();
    assert_eq!(pool.invariant_snapshot().refs, [1]);
    assert_eq!(fill.publish(1).unwrap().as_slice(), &[42]);
    pool.assert_recovered();
    drop(pool.private_fill().unwrap().into_compute());
    pool.assert_recovered();
}

#[test]
fn same_value_staging_is_private_and_authority_is_slot_specific() {
    let pool = io_test_pool(2);
    let other = io_test_pool(1);
    let (a, da) = pool.stage(key(1)).unwrap().split_destination();
    let (b, db) = pool.stage(key(1)).unwrap().split_destination();
    let (c, dc) = other.stage(key(1)).unwrap().split_destination();
    let (a, db) = a.reunite(db).err().expect("different slot accepted");
    let (a, dc) = a.reunite(dc).err().expect("foreign pool accepted");
    let mut first = a.reunite(da).unwrap();
    let mut second = b.reunite(db).unwrap();
    first.as_mut_slice()[0] = 1;
    second.as_mut_slice()[0] = 2;
    let first = first.publish(1).unwrap();
    let second = second.publish(1).unwrap();
    assert!(first.matches_key(key(1)) && second.matches_key(key(1)));
    assert_eq!(first.as_slice(), &[1]);
    assert_eq!(second.as_slice(), &[2]);
    let storage = Fill::from_storage(dc.into_storage())
        .err()
        .expect("authority escaped");
    let dc = Destination::from_storage(storage).unwrap_or_else(|_| panic!("lost destination"));
    drop(c.reunite(dc).unwrap());
    drop((first, second));
    pool.assert_recovered();
    other.assert_recovered();
}

#[test]
fn network_consumer_caps_takeover_and_terminal_payload_ownership() {
    let pool = io_test_pool_config(Config {
        consumers_per_flight: NonZeroUsize::new(3).unwrap(),
        ..Config::new(NonZeroUsize::new(1).unwrap())
    });
    let mut producer = pool.network_flight(scope(1)).unwrap();
    assert!(matches!(
        producer.poll(Waker::noop()),
        NetworkProgress::Produce
    ));
    let mut first = pool.network_flight(scope(1)).unwrap();
    let mut second = pool.network_flight(scope(1)).unwrap();
    assert!(
        pool.network_flight(scope(1)).is_err(),
        "unpolled leases count"
    );
    assert!(matches!(
        first.poll(Waker::noop()),
        NetworkProgress::Pending
    ));
    drop(producer);
    assert!(matches!(
        first.poll(Waker::noop()),
        NetworkProgress::Produce
    ));
    assert!(matches!(
        second.poll(Waker::noop()),
        NetworkProgress::Pending
    ));
    let mut fill = pool.stage(key(1)).unwrap();
    fill.as_mut_slice()[0] = 42;
    let buffer = fill.publish(1).unwrap();
    first.finish(Ok(&buffer));
    drop((first, buffer));
    assert!(
        pool.private_fill().is_err(),
        "unpolled terminal consumer owns result"
    );
    let mut replacement = pool.network_flight(scope(1)).unwrap();
    assert!(matches!(
        replacement.poll(Waker::noop()),
        NetworkProgress::Produce
    ));
    let NetworkProgress::Ready(Ok(result)) = second.poll(Waker::noop()) else {
        panic!()
    };
    drop(second);
    assert_eq!(result.as_slice(), &[42]);
    assert!(pool.private_fill().is_err());
    drop((result, replacement));
    pool.assert_recovered();
}

#[test]
fn typed_terminals_and_default_flight_bound_are_independent_of_slots() {
    let pool = io_test_pool_config(Config::new(NonZeroUsize::new(1).unwrap()));
    let pinned = pool.private_fill().unwrap();
    let mut flights: Vec<_> = (0..128)
        .map(|n| pool.network_flight(scope(n)).unwrap())
        .collect();
    assert!(pool.network_flight(scope(128)).is_err());
    let mut joined = pool.network_flight(scope(0)).unwrap();
    assert!(matches!(
        flights[0].poll(Waker::noop()),
        NetworkProgress::Produce
    ));
    let value = crate::metadata::Metadata {
        checksum: crate::metadata::Checksum([7; 32]),
        len: 9,
        expires: 0,
    };
    flights[0].finish_metadata(value);
    assert!(matches!(joined.poll(Waker::noop()), NetworkProgress::Metadata(m) if m == value));
    assert!(
        pool.network_flight(scope(0)).is_err(),
        "detached terminals count"
    );
    drop((flights, joined, pinned));
    pool.assert_recovered();
}

#[test]
fn network_scope_isolation_and_typed_failure_delivery() {
    let pool = io_test_pool(8);
    let mut keys = vec![scope(1)];
    let mut page = scope(2);
    keys.push(page.clone());
    page.routing[0] += 1;
    keys.push(page.clone());
    page.destination += 1;
    keys.push(page.clone());
    page.dependency = NetworkDependency::Canonical { slot: 2 };
    keys.push(page.clone());
    page.dependency = NetworkDependency::Canonical { slot: 3 };
    keys.push(page.clone());
    page.version += 1;
    keys.push(page);
    let mut leases: Vec<_> = keys
        .into_iter()
        .map(|k| pool.network_flight(k).unwrap())
        .collect();
    for lease in &mut leases {
        assert!(matches!(
            lease.poll(Waker::noop()),
            NetworkProgress::Produce
        ));
    }
    let mut joined = pool.network_flight(scope(1)).unwrap();
    assert!(matches!(
        joined.poll(Waker::noop()),
        NetworkProgress::Pending
    ));
    let error = Arc::new(crate::cache::Error::InvalidData("typed terminal"));
    leases[0].finish(Err(error.clone()));
    let NetworkProgress::Ready(Err(delivered)) = joined.poll(Waker::noop()) else {
        panic!()
    };
    assert!(Arc::ptr_eq(&error, &delivered));
    let mut replacement = pool.network_flight(scope(1)).unwrap();
    assert!(matches!(
        replacement.poll(Waker::noop()),
        NetworkProgress::Produce
    ));
    assert!(pool.network_flight(scope(99)).is_err());
    let snapshot = pool.invariant_snapshot();
    assert_eq!(
        (snapshot.flights, snapshot.consumers, snapshot.producers),
        (8, 9, 7)
    );
    drop((leases, joined, replacement));
    pool.assert_recovered();
}

#[test]
fn panicking_network_notifications_do_not_strand_survivors() {
    struct Notify(Arc<AtomicUsize>);
    impl std::task::Wake for Notify {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
            panic!("notification");
        }
    }
    for cancel in [false, true] {
        let pool = io_test_pool(2);
        let mut producer = pool.network_flight(scope(1)).unwrap();
        assert!(matches!(
            producer.poll(Waker::noop()),
            NetworkProgress::Produce
        ));
        let calls = Arc::new(AtomicUsize::new(0));
        let mut joined: Vec<_> = (0..4)
            .map(|_| pool.network_flight(scope(1)).unwrap())
            .collect();
        for lease in &mut joined {
            assert!(matches!(
                lease.poll(&Waker::from(Arc::new(Notify(calls.clone())))),
                NetworkProgress::Pending
            ));
        }
        if !cancel {
            producer.finish(Err(Arc::new(crate::cache::Error::InvalidData("terminal"))));
        }
        drop(producer);
        assert_eq!(calls.load(Ordering::Relaxed), 4);
        if cancel {
            assert!(matches!(
                joined[0].poll(Waker::noop()),
                NetworkProgress::Produce
            ));
        } else {
            for lease in &mut joined {
                assert!(matches!(
                    lease.poll(Waker::noop()),
                    NetworkProgress::Ready(Err(_))
                ));
            }
        }
        drop(joined);
        pool.assert_recovered();
    }
}

#[test]
fn registration_descriptors_alignment_and_mapping_lifetime() {
    let pool = io_test_pool(3);
    let lease = pool.memory_lease();
    lease.test_residency(true);
    let weak = Arc::downgrade(&lease.mapping);
    let base = lease.region().address as usize;
    assert_eq!(base % 4096, 0);
    assert_eq!(lease.region().len, 3 * BUFFER_SIZE);
    for region in lease.buffers() {
        assert_eq!(region.offset, region.index * BUFFER_SIZE);
        assert_eq!(region.region.address as usize, base + region.offset);
        assert_eq!(region.region.len, BUFFER_SIZE);
    }
    for slot in &pool.node.slots {
        let refs = &slot.refs as *const _ as usize;
        let info = &slot.info as *const _ as usize;
        assert_eq!(refs % 128, 0);
        assert_eq!(info % 128, 0);
        assert!(refs.abs_diff(info) >= 128);
    }
    let mut fill = pool.private_fill().unwrap();
    fill.as_mut_slice()[0] = 42;
    let buffer = fill.publish(1).unwrap();
    drop(pool);
    assert_eq!(buffer.as_slice(), &[42]);
    drop(buffer);
    assert!(weak.upgrade().is_some());
    drop(lease);
    assert!(weak.upgrade().is_none());
    assert!(Mapping::new(usize::MAX / BUFFER_SIZE + 1, NumaNodeId(0)).is_err());
    assert!(Mapping::new(isize::MAX as usize / BUFFER_SIZE + 1, NumaNodeId(0)).is_err());
}

#[test]
fn setup_bind_and_partial_prefault_errors_allow_retry() {
    let config = Config::new(NonZeroUsize::new(3).unwrap());
    assert_eq!(
        Node::new(config, NumaNodeId(0), |_| Err(
            io::Error::from_raw_os_error(libc::EACCES)
        ))
        .err()
        .unwrap()
        .raw_os_error(),
        Some(libc::EACCES)
    );
    assert!(
        Node::new(config, NumaNodeId(0), |mapping| {
            // SAFETY: setup exclusively owns this aligned live subrange.
            assert_eq!(
                unsafe { libc::mprotect(mapping.pointer(2).cast(), BUFFER_SIZE, libc::PROT_READ) },
                0
            );
            Ok(())
        })
        .is_err()
    );
    let pool = io_test_pool_config(config);
    pool.memory_lease().test_residency(true);
    let shared = pool.test_other_worker();
    let fills: Vec<_> = (0..3).map(|_| shared.private_fill().unwrap()).collect();
    assert!(pool.private_fill().is_err());
    drop(fills);
    pool.assert_recovered();
}
