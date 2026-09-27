use super::*;

fn fixture(capacity: usize) -> Fixture {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor
        .init()
        .expect("index pressure tests require io_uring");
    f.store.writer.index().set_page_capacity(capacity).unwrap();
    f
}

fn persist(f: &Fixture, number: u8) -> PageId {
    let copy = f.copy(number, 113);
    let id = copy.ciphertext.envelope().page.clone();
    f.enqueue(copy).unwrap();
    assert_eq!(
        drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap(),
        1
    );
    assert!(f.store.writer.index().lookup(&id).unwrap().is_some());
    id
}

#[test]
fn small_records_turn_over_open_segment_and_checkpoint_recovers_only_live_mappings() {
    let f = fixture(2);
    let index = f.store.writer.index();
    let first = persist(&f, 1);
    let second = persist(&f, 2);
    let before = segment_images(&f);
    assert_eq!(before[0].2, segment::SegmentState::Open);
    assert_eq!(f.store.writer.segments_for_test().free_count(), 1);
    let third = persist(&f, 3);
    let fourth = persist(&f, 4);
    assert!(index.lookup(&first).unwrap().is_none());
    assert!(index.lookup(&second).unwrap().is_none());
    assert_eq!(index.snapshot().unwrap().entries.len(), 2);
    let after = segment_images(&f);
    assert_eq!(after[0].1, before[0].1);
    assert_eq!(after[0].2, segment::SegmentState::Open);
    assert!(after[0].3 > before[0].3);
    assert!(after[0].3 < 32 * 1024 * 1024);
    assert_eq!(after[1], before[1]);
    assert_eq!(f.store.writer.discarded_count(), 0);

    let image = futures::executor::block_on(f.store.checkpoint.snapshot_shard()).unwrap();
    futures::executor::block_on(f.store.checkpoint.publish(vec![image])).unwrap();
    f.store.checkpoint.finish_snapshot();
    let loaded = futures::executor::block_on(
        f.store
            .recovery
            .load(f.store.writer.slabs().alignment().unwrap()),
    )
    .unwrap()
    .unwrap();
    futures::executor::block_on(
        f.store
            .recovery
            .install_shard(loaded.shards.into_iter().next()),
    )
    .unwrap();
    assert!(index.lookup(&first).unwrap().is_none());
    assert!(index.lookup(&second).unwrap().is_none());
    for (id, number) in [(&third, 3), (&fourth, 4)] {
        let read = drive(&f.reactor, f.store.reader.read(id, &scope()))
            .unwrap()
            .unwrap();
        assert_eq!(read.ciphertext.bytes(), vec![number; 129]);
    }
    // Recovered sealed segments participate in index turnover too.
    persist(&f, 5);
    assert!(index.snapshot().unwrap().entries.len() <= 2);
}

#[test]
fn replacement_does_not_evict_neighbors_or_let_old_tokens_remove_new_mapping() {
    let f = fixture(2);
    let first = persist(&f, 1);
    let second = persist(&f, 2);
    let (_, token) = drive(&f.reactor, f.store.reader.read_with_token(&first, &scope()))
        .unwrap()
        .unwrap();
    let index = f.store.writer.index();
    let neighbor = index.lookup(&second).unwrap().unwrap().location;
    let old = index.lookup(&first).unwrap().unwrap().location;
    persist(&f, 1);
    let replacement = index.lookup(&first).unwrap().unwrap().location;
    assert_ne!(old, replacement);
    f.store.reader.invalidate(&token).unwrap();
    assert_eq!(index.lookup(&first).unwrap().unwrap().location, replacement);
    assert_eq!(index.lookup(&second).unwrap().unwrap().location, neighbor);
    assert_eq!(index.snapshot().unwrap().entries.len(), 2);
}

#[test]
fn index_eviction_during_read_cannot_resurrect_copy_or_recycle_active_generation() {
    let f = fixture(1);
    let first = persist(&f, 1);
    let index = f.store.writer.index();
    let old = index.lookup(&first).unwrap().unwrap().location;
    let (_, token) = drive(&f.reactor, f.store.reader.read_with_token(&first, &scope()))
        .unwrap()
        .unwrap();
    let held = f.store.writer.lease(&old).unwrap();
    let request = scope();
    let mut read = f.store.reader.read(&first, &request);
    assert!(
        read.as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending()
    );
    persist(&f, 2);
    assert!(drive(&f.reactor, read).unwrap().is_none());
    f.store
        .writer
        .segments_for_test()
        .validate_location(&old)
        .unwrap();

    // Seal the old open segment, then exercise the normal slab-pressure fence.
    let segments = f.store.writer.segments_for_test();
    drop(segments.append(32 * 1024 * 1024).unwrap());
    let clock = eviction::SegmentClock::new(index.clone(), segments.clone(), 2);
    assert_eq!(clock.reclaim_now(), Err(Error::Overloaded));
    assert_eq!(
        segments.state(old.segment).unwrap(),
        segment::SegmentState::Evicting
    );
    assert_eq!(segments.recycle(old.segment), Err(Error::Overloaded));
    drop(held);
    segments.recycle(old.segment).unwrap();
    assert!(segments.lease(old.segment, old.generation).is_err());
    persist(&f, 1);
    let new = index.lookup(&first).unwrap().unwrap().location;
    assert_ne!(new.generation, old.generation);
    f.store.reader.invalidate(&token).unwrap();
    assert_eq!(index.lookup(&first).unwrap().unwrap().location, new);
}

#[test]
fn retirement_during_pressure_write_prevents_late_publication() {
    let f = fixture(1);
    let first = persist(&f, 1);
    let copy = f.copy(2, 113);
    let second = copy.ciphertext.envelope().page.clone();
    f.enqueue(copy).unwrap();
    let request = scope();
    let mut write = f.store.writer.progress(1, &request);
    assert!(
        write
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending()
    );
    assert!(f.store.writer.index().lookup(&first).unwrap().is_none());
    f.store
        .writer
        .retire_key(&CacheId("cache".into()), KeyId([1; 16]))
        .unwrap();
    drive(&f.reactor, write).unwrap();
    assert!(f.store.writer.index().lookup(&second).unwrap().is_none());
    assert!(f.store.writer.is_idle());
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert!(f.enqueue(f.copy(3, 113)).is_ok());
}

#[test]
fn index_clock_gives_recent_segments_a_second_chance_and_handles_no_victim() {
    let f = Fixture::new();
    let index = Rc::new(index::Index::new(WorkerId(0), 2));
    index.set_page_capacity(2).unwrap();
    let segments = Rc::new(segment::Segments::new(WorkerId(0), 512));
    segments
        .configure(
            1536,
            3,
            direct::DirectAlignment::validate(512, 512, 512).unwrap(),
        )
        .unwrap();
    let clock = eviction::SegmentClock::new(index.clone(), segments.clone(), 1);
    let mut ids = Vec::new();
    for number in [1, 2] {
        let copy = f.copy(number, 113);
        let id = copy.ciphertext.envelope().page.clone();
        let append = segments.append(512).unwrap();
        index
            .publish(
                id.clone(),
                index::IndexedPage {
                    location: index::RecordLocation {
                        segment: append.segment.id(),
                        generation: append.segment.generation(),
                        location: append.location,
                    },
                    metadata: copy.metadata.immutable(),
                    key_id: copy.ciphertext.envelope().key_id,
                },
            )
            .unwrap();
        ids.push(id);
    }
    let incoming = f.copy(3, 113).ciphertext.envelope().page.clone();
    clock.mark_read(segment::SegmentId(0)).unwrap();
    clock.reclaim_index_for(&incoming).unwrap();
    assert!(index.lookup(&ids[0]).unwrap().is_some());
    assert!(index.lookup(&ids[1]).unwrap().is_none());
    assert_eq!(segments.free_count(), 1);
    // Even if every retained segment is recent, the second rotation makes room.
    index.set_page_capacity(1).unwrap();
    clock.mark_read(segment::SegmentId(0)).unwrap();
    clock.reclaim_index_for(&incoming).unwrap();
    assert!(index.lookup(&ids[0]).unwrap().is_none());

    // An unavailable segment table cannot spin or manufacture a free index slot.
    let empty = Rc::new(segment::Segments::new(WorkerId(0), 512));
    let copy = f.copy(1, 113);
    let append = segments.append(512).unwrap();
    index
        .publish(
            ids[0].clone(),
            index::IndexedPage {
                location: index::RecordLocation {
                    segment: append.segment.id(),
                    generation: append.segment.generation(),
                    location: append.location,
                },
                metadata: copy.metadata.immutable(),
                key_id: copy.ciphertext.envelope().key_id,
            },
        )
        .unwrap();
    assert_eq!(
        eviction::SegmentClock::new(index, empty, 1).reclaim_index_for(&incoming),
        Err(Error::Overloaded)
    );
}
