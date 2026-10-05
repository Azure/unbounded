use super::*;
fn segments(bytes: u64, count: usize) -> Segments {
    let s = Segments::new(bytes);
    s.configure(
        bytes * count as u64,
        count,
        Alignment::new(512, 512, 512).unwrap(),
    )
    .unwrap();
    s
}
#[test]
fn no_reuse_before_lease_drop_and_no_aba() {
    let s = segments(1024, 2);
    let a = s.append(1024).unwrap();
    s.begin_evict(a.0.id()).unwrap();
    assert_eq!(s.recycle(a.0.id()), Err(Error::Busy));
    drop(a);
    s.recycle(SegmentId(0)).unwrap();
    assert!(s.lease(SegmentId(0), Generation(1)).is_err());
    assert_eq!(s.append(512).unwrap().0.generation(), Generation(2));
    let frozen = s.freeze().unwrap();
    assert!(s.append(512).is_err());
    drop(frozen);
}
#[test]
fn restore_seals_open_and_rejects_invalid_geometry() {
    let s = segments(1024, 1);
    drop(s.append(512).unwrap());
    let mut snap = s.snapshot();
    s.restore(snap.clone()).unwrap();
    assert_eq!(s.snapshot()[0].state, SegmentState::Sealed);
    snap[0].used_bytes = 513;
    assert!(s.restore(snap).is_err());
}
#[test]
fn generation_exhaustion_never_wraps_and_small_tails_are_sealed() {
    let s = segments(1024, 2);
    drop(s.append(512).unwrap());
    let next = s.append(1024).unwrap();
    assert_eq!(next.0.id(), SegmentId(1));
    drop(next);
    assert_eq!(s.state(SegmentId(0)).unwrap(), SegmentState::Sealed);
    let mut snapshot = s.snapshot();
    snapshot[0].generation = Generation(u64::MAX);
    s.restore(snapshot).unwrap();
    s.begin_evict(SegmentId(0)).unwrap();
    assert_eq!(s.recycle(SegmentId(0)), Err(Error::Unavailable));
    assert_eq!(s.snapshot()[0].generation, Generation(u64::MAX));
}
#[test]
fn validation_rejects_unwritten_ranges_generations_and_busy_restore() {
    let s = segments(1024, 1);
    assert!(s.append(0).is_err());
    assert!(s.append(513).is_err());
    let (lease, extent) = s.append(512).unwrap();
    assert_eq!(s.validate(lease.id(), lease.generation(), &extent), Ok(()));
    assert_eq!(
        s.validate(lease.id(), Generation(2), &extent),
        Err(Error::Stale)
    );
    assert_eq!(
        s.validate(
            lease.id(),
            lease.generation(),
            &Extent::new(512, 512).unwrap()
        ),
        Err(Error::Corrupt)
    );
    let mut images = s.snapshot();
    assert_eq!(s.restore(images.clone()), Err(Error::Busy));
    drop(lease);
    images[0].generation = Generation(0);
    assert_eq!(s.restore(images), Err(Error::Corrupt));
    assert_eq!(s.state(SegmentId(0)).unwrap(), SegmentState::Open);
    assert_eq!(s.count(), 1);
    assert_eq!(s.free_count(), 0);
    assert_eq!(s.capacity_bytes(), 1024);
    let frozen = s.freeze().unwrap();
    assert!(matches!(s.freeze(), Err(Error::Busy)));
    assert_eq!(s.begin_evict(SegmentId(0)), Err(Error::Busy));
    assert_eq!(s.recycle(SegmentId(0)), Err(Error::Busy));
    drop(frozen);
    drop(s.append(512).unwrap());
    assert!(matches!(s.append(512), Err(Error::Busy)));
}
#[test]
fn configuration_rejects_zero_and_misaligned_capacity() {
    let a = Alignment::new(512, 512, 512).unwrap();
    for (bytes, capacity, count) in [
        (0, 1024, 1),
        (1024, 0, 1),
        (513, 1026, 2),
        (1024, 1024, 0),
        (1024, 1024, 2),
        (1024, 1025, 1),
        (1024, 1024 * 1_000_001, 1_000_001),
    ] {
        assert_eq!(
            Segments::new(bytes).configure(capacity, count, a),
            Err(Error::InvalidConfiguration)
        );
    }
    let s = segments(1024, 1);
    assert_eq!(s.configure(1024, 1, a), Err(Error::InvalidConfiguration));
    assert!(s.state(SegmentId(u64::MAX)).is_err());
    let s = Segments::new(1024);
    assert_eq!(s.configure(1025, 1, a), Err(Error::InvalidConfiguration));
    assert!(!s.is_configured());
    assert_eq!(s.geometry(), None);
    assert_eq!(s.capacity_bytes(), 0);
    assert_eq!(s.free_count(), 0);
    assert!(s.snapshot().is_empty());
    s.configure(1024, 1, a).unwrap();
}

#[test]
fn append_failures_preserve_open_tail_free_list_and_lease_counts() {
    let s = segments(1024, 1);
    drop(s.append(512).unwrap());
    let before = s.snapshot();
    for length in [0, 1, 513, 1536] {
        assert!(s.append(length).is_err());
        assert_eq!(s.snapshot(), before);
        assert_eq!(s.open.get(), Some(0));
        assert_eq!(s.free_count(), 0);
        assert_eq!(s.slots.borrow()[0].leases.get(), 0);
    }
    drop(s.append(512).unwrap());
    assert_eq!(s.state(SegmentId(0)), Ok(SegmentState::Sealed));

    // Synthetic saturation covers failure both on an open slot and when rotating.
    let s = segments(1024, 2);
    drop(s.append(512).unwrap());
    for (position, length) in [(0, 512), (1, 1024)] {
        s.slots.borrow()[position].leases.set(usize::MAX);
        let before = s.snapshot();
        assert!(matches!(s.append(length), Err(Error::Busy)));
        assert_eq!(s.snapshot(), before);
        assert_eq!(s.open.get(), Some(0));
        assert_eq!(*s.free.borrow(), BTreeSet::from([1]));
        assert_eq!(s.slots.borrow()[position].leases.get(), usize::MAX);
        s.slots.borrow()[position].leases.set(0);
    }
    drop(s.append(1024).unwrap());
    assert_eq!(s.state(SegmentId(0)), Ok(SegmentState::Sealed));

    // Arithmetic failures also occur before sealing the tail or consuming free.
    let s = segments(1024, 2);
    drop(s.append(512).unwrap());
    s.slots.borrow_mut()[1].image.id = SegmentId(u64::MAX);
    let before = s.snapshot();
    assert!(matches!(s.append(1024), Err(Error::InvalidConfiguration)));
    assert_eq!(s.snapshot(), before);
    assert_eq!(s.open.get(), Some(0));
    assert_eq!(*s.free.borrow(), BTreeSet::from([1]));
    assert_eq!(s.slots.borrow()[1].leases.get(), 0);
}

#[test]
fn freeze_guard_drop_thaws_and_can_outlive_table() {
    let s = segments(1024, 1);
    let frozen = s.freeze().unwrap();
    let before = s.snapshot();
    assert_eq!(s.restore(before.clone()), Err(Error::Busy));
    assert_eq!(s.validate_restore(&before), Err(Error::Busy));
    assert!(matches!(s.append(512), Err(Error::Busy)));
    assert!(matches!(s.freeze(), Err(Error::Busy)));
    assert_eq!(s.snapshot(), before);
    drop(frozen);
    drop(s.append(512).unwrap());
    let frozen = s.freeze().unwrap();
    drop(s);
    drop(frozen);

    let s = Segments::new(1024);
    let frozen = s.freeze().unwrap();
    assert_eq!(
        s.configure(1024, 1, Alignment::new(512, 512, 512).unwrap()),
        Err(Error::Busy)
    );
    drop(frozen);
    assert!(!s.is_configured());
    assert_eq!(s.geometry(), None);

    // Unwinding or canceling an owner drops the guard just like the normal path.
    let s = segments(1024, 1);
    assert!(
        std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let _frozen = s.freeze().unwrap();
            panic!("simulated owner failure");
        }))
        .is_err()
    );
    drop(s.append(512).unwrap());
}

#[test]
fn geometry_is_shared_by_configuration_and_leases() {
    let geometry =
        SegmentGeometry::new(4096, 1024, 2, Alignment::new(512, 512, 512).unwrap()).unwrap();
    let s = Segments::from_geometry(geometry).unwrap();
    assert!(s.is_configured());
    assert_eq!(s.geometry(), Some(geometry));
    assert_eq!(s.count(), 2);
    assert_eq!(s.free_count(), 2);
    let (lease, extent) = s.append(512).unwrap();
    assert_eq!(lease.geometry(), geometry);
    assert!(Rc::ptr_eq(&lease.table_identity(), &s.table_identity()));
    assert_eq!(s.validate_lease(&lease, &extent), Ok(()));
}

#[test]
fn large_physical_geometry_supports_bounded_partial_tables() {
    let capacity = 1024 * (MAX_SEGMENTS + 1);
    let alignment = Alignment::new(512, 512, 512).unwrap();
    let full = SegmentGeometry::new(capacity, 1024, MAX_SEGMENTS + 1, alignment).unwrap();
    assert!(matches!(
        Segments::from_geometry(full),
        Err(Error::InvalidConfiguration)
    ));
    let s = Segments::new(1024);
    assert_eq!(
        s.configure(capacity, (MAX_SEGMENTS + 1) as usize, alignment),
        Err(Error::InvalidConfiguration)
    );
    assert!(!s.is_configured());
    s.configure(capacity, 2, alignment).unwrap();
    assert_eq!(s.capacity_bytes(), capacity);
    assert_eq!(s.count(), 2);
    assert_eq!(s.geometry().unwrap().segment_count(), 2);
    drop(s.append(1024).unwrap());
    assert_eq!(s.free_count(), 1);
}

#[test]
fn leases_validate_table_and_captured_used_range_through_eviction() {
    let s = segments(1024, 1);
    let other = segments(1024, 1);
    let (lease, extent) = s.append(512).unwrap();
    let (_, later) = s.append(512).unwrap();
    assert_eq!(s.validate_lease(&lease, &later), Err(Error::Corrupt));
    assert_eq!(other.validate_lease(&lease, &extent), Err(Error::Stale));
    assert_eq!(
        lease.validate_extent(&Extent::new(1, 511).unwrap()),
        Err(Error::Corrupt)
    );
    assert_eq!(
        lease.validate_extent(&Extent::new(0, 1).unwrap()),
        Err(Error::Corrupt)
    );
    s.begin_evict(SegmentId(0)).unwrap();
    assert_eq!(s.validate_lease(&lease, &extent), Ok(()));
    assert_eq!(
        s.validate(lease.id(), lease.generation(), &extent),
        Err(Error::Stale)
    );
    assert!(matches!(
        s.lease(lease.id(), lease.generation()),
        Err(Error::Stale)
    ));
    assert!(matches!(
        s.lease(SegmentId(1), Generation(1)),
        Err(Error::Corrupt)
    ));
    drop(s);
    assert_eq!(lease.validate_extent(&extent), Ok(()));
    drop(lease);
}

#[test]
fn malformed_extents_are_corrupt_not_configuration_errors() {
    let s = segments(1024, 1);
    let (lease, _) = s.append(1024).unwrap();
    for extent in [
        Extent::new(1, 512).unwrap(),
        Extent::new(0, 513).unwrap(),
        Extent::new(1024, 512).unwrap(),
    ] {
        assert_eq!(
            s.validate(lease.id(), lease.generation(), &extent),
            Err(Error::Corrupt)
        );
    }
}

#[test]
fn invalid_restore_states_are_atomic_and_valid_states_round_trip() {
    let s = segments(1024, 3);
    drop(s.append(1024).unwrap());
    drop(s.append(512).unwrap());
    let before = s.snapshot();
    let mut cases = Vec::new();
    for state in [
        SegmentState::Open,
        SegmentState::Sealed,
        SegmentState::Evicting,
    ] {
        let mut images = before.clone();
        images[2].state = state;
        cases.push(images);
    }
    let mut images = before.clone();
    images[0].state = SegmentState::Open; // A full Open slot is impossible.
    cases.push(images);
    let mut images = before.clone();
    images[0].state = SegmentState::Open;
    images[0].used_bytes = 512; // Two partial Open slots are also impossible.
    cases.push(images);
    for images in cases {
        assert_eq!(s.restore(images), Err(Error::Corrupt));
        assert_eq!(s.snapshot(), before);
        assert_eq!(s.free_count(), 1);
        assert_eq!(s.open.get(), Some(1));
        assert_eq!(s.restore_epoch(), 0);
    }
    let mut images = before;
    images[0].state = SegmentState::Evicting;
    s.restore(images).unwrap();
    assert_eq!(s.restore_epoch(), 1);
    assert_eq!(s.state(SegmentId(0)), Ok(SegmentState::Evicting));
    assert_eq!(s.state(SegmentId(1)), Ok(SegmentState::Sealed));
    s.recycle(SegmentId(0)).unwrap();
    assert_eq!(s.free_count(), 2);
    let before = s.snapshot();
    s.restore_epoch.set(u64::MAX - 1);
    assert_eq!(s.validate_restore(&before), Ok(()));
    s.restore(before.clone()).unwrap();
    assert_eq!(s.restore_epoch(), u64::MAX);
    assert_eq!(s.snapshot(), before);
    s.restore_epoch.set(u64::MAX);
    assert_eq!(s.validate_restore(&before), Err(Error::Unavailable));
    assert_eq!(s.restore(before.clone()), Err(Error::Unavailable));
    assert_eq!(s.snapshot(), before);
}
