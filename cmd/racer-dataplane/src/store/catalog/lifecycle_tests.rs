use super::*;

#[test]
fn no_reuse_before_lease_drop_and_no_aba() {
    let s = Segments::new(WorkerId(0), 1024);
    s.configure(2048, 2, DirectAlignment::validate(512, 512, 512).unwrap())
        .unwrap();
    let a = s.append(1024).unwrap();
    s.begin_evict(a.segment.id).unwrap();
    assert!(s.recycle(a.segment.id).is_err());
    drop(a);
    s.recycle(SegmentId(0)).unwrap();
    assert!(s.lease(SegmentId(0), Generation(1)).is_err());
    assert_eq!(s.append(512).unwrap().segment.generation, Generation(2));
    s.freeze().unwrap();
    assert!(s.append(512).is_err());
    s.thaw();
}

#[test]
fn restore_seals_open_and_rejects_invalid_geometry() {
    let s = Segments::new(WorkerId(0), 1024);
    s.configure(1024, 1, DirectAlignment::validate(512, 512, 512).unwrap())
        .unwrap();
    drop(s.append(512).unwrap());
    let mut snap = s.snapshot().unwrap();
    s.restore(snap.clone()).unwrap();
    assert_eq!(s.snapshot().unwrap()[0].state, SegmentState::Sealed);
    snap[0].used_bytes = 513;
    assert!(s.restore(snap).is_err());
}

#[test]
fn generation_exhaustion_never_wraps_and_small_tails_are_sealed() {
    let s = Segments::new(WorkerId(0), 1024);
    s.configure(2048, 2, DirectAlignment::validate(512, 512, 512).unwrap())
        .unwrap();
    drop(s.append(512).unwrap());
    let next = s.append(1024).unwrap();
    assert_eq!(next.segment.id(), SegmentId(1));
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
fn busy_victim_waits_and_clock_makes_progress() {
    let segments = Rc::new(Segments::new(WorkerId(0), 512));
    segments
        .configure(1024, 2, DirectAlignment::validate(512, 512, 512).unwrap())
        .unwrap();
    let held = segments.append(512).unwrap();
    drop(segments.append(512).unwrap());
    let clock = SegmentClock::new(
        Rc::new(Index::new(
            WorkerId(0),
            1,
            crate::test_support::availability(),
        )),
        segments.clone(),
        2,
    );
    clock.mark_read(SegmentId(1)).unwrap();
    assert_eq!(clock.reclaim_now(), Err(Error::Overloaded));
    assert_eq!(segments.free_count(), 1);
    drop(held);
    clock.reclaim_now().unwrap();
    assert_eq!(segments.free_count(), 2);
}
