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
    s.freeze().unwrap();
    assert!(s.append(512).is_err());
    s.thaw();
}
#[test]
fn restore_seals_open_and_rejects_invalid_geometry() {
    let s = segments(1024, 1);
    drop(s.append(512).unwrap());
    let mut snap = s.snapshot().unwrap();
    s.restore(snap.clone()).unwrap();
    assert_eq!(s.snapshot().unwrap()[0].state, SegmentState::Sealed);
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
    let mut snapshot = s.snapshot().unwrap();
    snapshot[0].generation = Generation(u64::MAX);
    s.restore(snapshot).unwrap();
    s.begin_evict(SegmentId(0)).unwrap();
    assert_eq!(s.recycle(SegmentId(0)), Err(Error::Unavailable));
    assert_eq!(s.snapshot().unwrap()[0].generation, Generation(u64::MAX));
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
        Err(Error::Corrupt)
    );
    assert_eq!(
        s.validate(
            lease.id(),
            lease.generation(),
            &Extent::new(512, 512).unwrap()
        ),
        Err(Error::Corrupt)
    );
    let mut images = s.snapshot().unwrap();
    assert_eq!(s.restore(images.clone()), Err(Error::Corrupt));
    drop(lease);
    images[0].generation = Generation(0);
    assert_eq!(s.restore(images), Err(Error::Corrupt));
    assert_eq!(s.state(SegmentId(0)).unwrap(), SegmentState::Open);
    assert_eq!(s.count(), 1);
    assert_eq!(s.free_count(), 0);
    assert_eq!(s.capacity_bytes(), 1024);
    s.freeze().unwrap();
    assert_eq!(s.freeze(), Err(Error::Busy));
    assert_eq!(s.begin_evict(SegmentId(0)), Err(Error::Busy));
    assert_eq!(s.recycle(SegmentId(0)), Err(Error::Busy));
    s.thaw();
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
    ] {
        assert_eq!(
            Segments::new(bytes).configure(capacity, count, a),
            Err(Error::InvalidConfiguration)
        );
    }
    let s = segments(1024, 1);
    assert_eq!(s.configure(1024, 1, a), Err(Error::InvalidConfiguration));
    assert!(s.state(SegmentId(u64::MAX)).is_err());
}
