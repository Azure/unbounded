use super::*;
use page_alloc::{Alignment, Generation, SegmentId, SegmentState, Segments, Slab};
mod index_pressure {
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
        assert_eq!(before[0].2, SegmentState::Open);
        assert_eq!(f.segments.free_count(), 1);
        let third = persist(&f, 3);
        let fourth = persist(&f, 4);
        assert!(index.lookup(&first).unwrap().is_none());
        assert!(index.lookup(&second).unwrap().is_none());
        assert_eq!(index.snapshot().unwrap().entries.len(), 2);
        let after = segment_images(&f);
        assert_eq!(after[0].1, before[0].1);
        assert_eq!(after[0].2, SegmentState::Open);
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
        f.segments
            .validate(old.segment, old.generation, &old.extent)
            .unwrap();
        let segments = &f.segments;
        drop(segments.append(32 * 1024 * 1024).unwrap());
        let clock = catalog::SegmentClock::new(index.clone(), segments.clone(), 2);
        assert_eq!(clock.reclaim_now(), Err(Error::Overloaded));
        assert_eq!(segments.state(old.segment).unwrap(), SegmentState::Evicting);
        assert_eq!(
            segments.recycle(old.segment).map_err(Error::from),
            Err(Error::Overloaded)
        );
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
    fn cache_removal_during_pressure_write_prevents_late_publication() {
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
            .remove_cache(&CacheId(crate::security::test_support::CACHE.into()))
            .unwrap();
        drive(&f.reactor, write).unwrap();
        assert!(f.store.writer.index().lookup(&second).unwrap().is_none());
        assert!(f.store.writer.is_idle());
        assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
        f.store.writer.reclaim_idle_buffer();
        assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
        assert!(f.enqueue(f.copy(3, 113)).is_ok());
    }
    fn publish(f: &Fixture, index: &catalog::Index, segments: &Segments, number: u8) -> PageId {
        let copy = f.copy(number, 113);
        let id = copy.ciphertext.envelope().page.clone();
        let append = segments.append(512).unwrap();
        index
            .publish(
                id.clone(),
                catalog::IndexedPage {
                    location: catalog::RecordLocation {
                        segment: append.0.id(),
                        generation: append.0.generation(),
                        extent: append.1,
                    },
                    metadata: copy.metadata.immutable(),
                    key_id: copy.ciphertext.envelope().key_id,
                },
            )
            .unwrap();
        id
    }
    #[test]
    fn index_clock_gives_recent_segments_a_second_chance_and_handles_no_victim() {
        let f = Fixture::new();
        let index = Rc::new(catalog::Index::new(
            WorkerId(0),
            2,
            crate::test_support::availability(),
        ));
        index.set_page_capacity(2).unwrap();
        let segments = Rc::new(Segments::new(512));
        segments
            .configure(1536, 3, Alignment::new(512, 512, 512).unwrap())
            .unwrap();
        let clock = catalog::SegmentClock::new(index.clone(), segments.clone(), 1);
        let ids = [
            publish(&f, &index, &segments, 1),
            publish(&f, &index, &segments, 2),
        ];
        let incoming = f.copy(3, 113).ciphertext.envelope().page.clone();
        clock.mark_read(SegmentId(0)).unwrap();
        clock.reclaim_index_for(&incoming).unwrap();
        assert!(index.lookup(&ids[0]).unwrap().is_some());
        assert!(index.lookup(&ids[1]).unwrap().is_none());
        assert_eq!(segments.free_count(), 1);
        index.set_page_capacity(1).unwrap();
        clock.mark_read(SegmentId(0)).unwrap();
        clock.reclaim_index_for(&incoming).unwrap();
        assert!(index.lookup(&ids[0]).unwrap().is_none());
        let empty = Rc::new(Segments::new(512));
        publish(&f, &index, &segments, 1);
        assert_eq!(
            catalog::SegmentClock::new(index, empty, 1).reclaim_index_for(&incoming),
            Err(Error::Overloaded)
        );
    }
}
use crate::{
    error::{Error, Result},
    memory::{page::CiphertextCopy, pool::BufferPool},
    model::{
        CacheId, CacheKey, ExpiresAt, Nonce, ObjectId, ObjectMetadata, ObjectVersion, PageEnvelope,
        PageId, PageNumber, RequestId, ResourceClass, StrongEtag, WorkerId,
    },
    runtime::{
        admission::{AdmissionExt, AdmissionPolicy},
        deadline::RequestScope,
        reactor::Reactor,
    },
};
use std::{
    future::Future,
    path::PathBuf,
    sync::atomic::{AtomicU64, Ordering},
    task::{Context, Poll},
    time::{Duration, Instant},
};
static NEXT: AtomicU64 = AtomicU64::new(0);
pub(super) struct Directory(pub(super) PathBuf);
impl Directory {
    pub(super) fn new() -> Self {
        let p = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!(
                "store-test-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
        std::fs::create_dir_all(&p).unwrap();
        Self(p)
    }
}
impl Drop for Directory {
    fn drop(&mut self) {
        std::fs::remove_dir_all(&self.0).unwrap();
    }
}
struct Fixture {
    metrics: crate::telemetry::metrics::Metrics,
    store: Store,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    reactor: Rc<Reactor>,
    pool: BufferPool,
    segments: Rc<Segments>,
    _directory: Directory,
}
impl Fixture {
    fn new() -> Self {
        Self::in_directory(Directory::new())
    }
    fn in_directory(directory: Directory) -> Self {
        Self::assemble(directory, crate::test_support::availability())
    }
    fn assemble(
        directory: Directory,
        availability: Rc<crate::control::state::Availability>,
    ) -> Self {
        let metrics = crate::telemetry::metrics::Metrics::default();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = BufferPool::new(admission.clone());
        let index = Rc::new(catalog::Index::new(WorkerId(0), 16, availability.clone()));
        let segments = Rc::new(Segments::new(32 * 1024 * 1024));
        let eviction = Rc::new(catalog::SegmentClock::new(
            index.clone(),
            segments.clone(),
            1,
        ));
        let slabs = Rc::new(Slab::new(
            directory.0.join("worker-0-slab-0.dat"),
            64 * 1024 * 1024,
            32 * 1024 * 1024,
            crate::model::PAGE_BYTES as usize + format::MAX_HEADER_BYTES + 16,
        ));
        let reader = Rc::new(
            StoreReader::new(
                eviction.clone(),
                index.clone(),
                segments.clone(),
                slabs.clone(),
                admission.clone(),
                reactor.clone(),
                pool.clone(),
            )
            .with_metrics(metrics.clone()),
        );
        let writer = Rc::new(
            writer::StoreWriter::new(
                index.clone(),
                segments.clone(),
                slabs,
                admission.clone(),
                reactor.clone(),
                availability,
            )
            .with_metrics(metrics.clone()),
        );
        let store = Store {
            reader,
            writer,
            checkpoint: Rc::new(checkpoint::Checkpointer::new(
                directory.0.clone(),
                index.clone(),
                segments.clone(),
            )),
            recovery: checkpoint::Recovery::new(directory.0.clone(), index, segments.clone()),
            eviction,
        };
        store.configure(admission.clone(), 2, 16).unwrap();
        let foreign = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            admission.limits().clone(),
        )));
        assert_eq!(
            store.configure(foreign, 2, 16),
            Err(Error::InvalidConfiguration)
        );
        Self {
            metrics,
            store,
            admission,
            reactor,
            pool,
            segments,
            _directory: directory,
        }
    }
    fn copy(&self, number: u8, length: usize) -> CiphertextCopy {
        let version = ObjectVersion {
            object: ObjectId {
                cache: CacheId(crate::security::test_support::CACHE.into()),
                key: CacheKey([number; 32]),
            },
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        };
        let envelope = PageEnvelope {
            page: PageId {
                version: version.clone(),
                number: PageNumber(0),
            },
            key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
            nonce: Nonce([2; 24]),
            plaintext_length: length as u32,
            ciphertext_length: length as u32 + 16,
        };
        let reservation = self
            .admission
            .reserve(
                Some(&version.object.cache),
                ResourceClass::Ciphertext,
                length + 16,
            )
            .unwrap();
        CiphertextCopy {
            metadata: ObjectMetadata {
                content_type: None,
                version,
                length: length as u64,
                expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
            },
            ciphertext: self
                .pool
                .ciphertext(reservation, envelope, vec![number; length + 16])
                .unwrap(),
        }
    }
    fn enqueue(&self, copy: CiphertextCopy) -> Result<u64> {
        let dirty = self.admission.reserve(
            Some(&copy.metadata.version.object.cache),
            ResourceClass::DirtyCiphertext,
            copy.ciphertext.bytes().len(),
        )?;
        self.store.writer.enqueue(copy, dirty)
    }

    fn restart(self) -> Self {
        let Self {
            metrics,
            store,
            admission,
            reactor,
            pool,
            segments,
            _directory,
        } = self;
        assert!(store.writer.is_idle());
        assert_eq!(reactor.in_flight(), 0);
        drop((store, reactor, pool, segments, admission, metrics));
        Self::in_directory(_directory)
    }
}
fn scope() -> RequestScope {
    RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(10)).unwrap()
}

#[test]
fn storage_requires_published_cache_and_live_keys_including_restore() {
    use crate::{
        control::state::{Availability, PublishedState, for_caches},
        memory::{cache::MemoryCache, pool::tests::bundle_for},
        security::test_support::{keys, rotation_bundle},
    };
    use std::sync::Arc;

    for mode in ["unpublished", "absent", "keyless", "available"] {
        let keys = Rc::new(keys());
        let cache = CacheId(crate::security::test_support::CACHE.into());
        let availability = match mode {
            "unpublished" => Rc::new(Availability::new(
                Arc::new(PublishedState::default()),
                keys.clone(),
            )),
            "absent" => for_caches(keys.clone(), vec![]),
            _ => for_caches(keys.clone(), vec![cache.clone()]),
        };
        let revoke = || {
            let mut next = rotation_bundle(2, (*keys.peer_trust_roots().unwrap()).clone());
            next.cache_keys.clear();
            keys.install(next).unwrap();
        };
        if mode == "keyless" {
            revoke();
        }
        let f = Fixture::assemble(Directory::new(), availability.clone());
        let memory = MemoryCache::new(f.pool.clone(), availability);
        let copy = f.copy(1, 3);
        let descriptor = copy.metadata.immutable();
        let page = bundle_for(&f.admission, descriptor.clone());
        let id = page.plaintext.page().clone();
        let index = f.store.writer.index();
        index.publish_version(descriptor.clone()).unwrap();
        if mode != "available" {
            assert!(index.version(&id.version).unwrap().is_none());
            assert_eq!(
                memory.publish(page.clone()),
                Err(if mode == "keyless" {
                    Error::MissingKey
                } else {
                    Error::Unavailable
                })
            );
            assert_eq!(
                memory.publish_ciphertext(crate::memory::page::UnverifiedPage {
                    copy: page.copy(),
                    disk_token: None
                }),
                Err(Error::MissingKey)
            );
            assert!(matches!(f.enqueue(copy), Err(Error::MissingKey)));
            assert_eq!(f.store.writer.pending_count(), 0);
            assert!(memory.get(&id).unwrap().is_none());
            continue;
        }
        futures::executor::block_on(f.store.open()).unwrap();
        f.reactor.init().unwrap();
        f.enqueue(copy).unwrap();
        drive(&f.reactor, f.store.writer.progress(1, &scope())).unwrap();
        memory.publish(page.clone()).unwrap();
        assert!(memory.get(&id).unwrap().is_some());
        assert!(index.lookup(&id).unwrap().is_some());
        assert_eq!(index.version(&id.version).unwrap(), Some(descriptor));
        let snapshot = index.snapshot().unwrap();
        revoke();
        assert!(memory.get(&id).unwrap().is_none());
        assert!(index.lookup(&id).unwrap().is_none());
        assert_eq!(memory.publish(page.clone()), Err(Error::MissingKey));
        assert!(matches!(f.enqueue(page.copy()), Err(Error::MissingKey)));
        index.restore(snapshot).unwrap();
        assert!(index.snapshot().unwrap().entries.is_empty());
        assert!(index.snapshot().unwrap().metadata.is_empty());
        assert_eq!(page.plaintext.bytes(), &[1; 3]);
    }
}
fn drive<T>(reactor: &Reactor, future: impl Future<Output = Result<T>>) -> Result<T> {
    let mut future = std::pin::pin!(future);
    let waker = futures::task::noop_waker();
    let mut cx = Context::from_waker(&waker);
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
            return result;
        }
        reactor.poll_budgeted(32)?;
        assert!(Instant::now() < deadline, "storage I/O did not complete");
        std::thread::yield_now();
    }
}
#[test]
fn concurrent_writes_reserve_distinct_extents_and_capacity_before_completion() {
    let f = Fixture::new();
    drive(&f.reactor, f.store.open()).unwrap();
    let a = f.copy(21, 4096);
    let b = f.copy(22, 4096);
    let aid = a.ciphertext.envelope().page.clone();
    let bid = b.ciphertext.envelope().page.clone();
    f.enqueue(a).unwrap();
    f.enqueue(b).unwrap();
    let scope = scope();
    let mut writes = f.store.writer.progress(8, &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(writes.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.store.writer.writes_in_flight(), 2);
    assert!(f.store.writer.index().lookup(&aid).unwrap().is_none());
    assert!(f.store.writer.index().lookup(&bid).unwrap().is_none());
    assert_eq!(drive(&f.reactor, writes).unwrap(), 2);
    let a = f.store.writer.index().lookup(&aid).unwrap().unwrap();
    let b = f.store.writer.index().lookup(&bid).unwrap().unwrap();
    assert_ne!(a.location.extent.offset(), b.location.extent.offset());
    assert!(f.store.writer.is_idle());
    for (id, expected) in [(&aid, 21), (&bid, 22)] {
        let copy = drive(&f.reactor, f.store.reader.read(id, &scope))
            .unwrap()
            .unwrap();
        assert_eq!(copy.ciphertext.bytes(), vec![expected; 4112]);
    }
}

#[test]
fn pipeline_out_of_order_failure_and_short_cqes_preserve_other_mapping() {
    use uring_runtime::reactor::simulation::{Fault, Simulation};
    for fault in [Fault::Delay(6), Fault::Errno(libc::EIO), Fault::Short(512)] {
        let simulation = Simulation::new();
        let _environment = simulation.enter();
        let f = Fixture::new();
        drive(&f.reactor, f.store.open()).unwrap();
        let a = f.copy(21, 4096);
        let b = f.copy(22, 4096);
        let aid = a.ciphertext.envelope().page.clone();
        let bid = b.ciphertext.envelope().page.clone();
        f.enqueue(a).unwrap();
        f.enqueue(b).unwrap();
        simulation.inject("write", fault);
        let scope = scope();
        let mut writes = f.store.writer.progress(8, &scope);
        let mut cx = Context::from_waker(std::task::Waker::noop());
        assert!(writes.as_mut().poll(&mut cx).is_pending());
        assert_eq!(f.store.writer.writes_in_flight(), 2);
        let _ = drive(&f.reactor, writes);
        assert!(f.store.writer.is_idle());
        assert!(f.store.writer.index().lookup(&bid).unwrap().is_some());
        if f.store.writer.index().lookup(&aid).unwrap().is_some() {
            let copy = drive(&f.reactor, f.store.reader.read(&aid, &scope))
                .unwrap()
                .unwrap();
            assert_eq!(copy.ciphertext.bytes(), vec![21; 4112]);
        }
        assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    }
}

#[test]
fn incremental_checkpoint_budget_thaws_and_async_publication_roundtrips() {
    let f = Fixture::new();
    drive(&f.reactor, f.store.open()).unwrap();
    f.enqueue(f.copy(1, 4096)).unwrap();
    drive(&f.reactor, f.store.writer.progress(8, &scope())).unwrap();
    assert!(matches!(
        drive(&f.reactor, f.store.checkpoint.snapshot_incremental(1)),
        Err(Error::Overloaded)
    ));
    let shard = drive(
        &f.reactor,
        f.store.checkpoint.snapshot_incremental(1024 * 1024),
    )
    .unwrap();
    assert_eq!(shard.index.entries.len(), 1);
    let task = f
        .store
        .checkpoint
        .publish_async(vec![shard], f.reactor.clone(), scope(), 7, 1, 1024 * 1024)
        .unwrap();
    drive(&f.reactor, task).unwrap();
    f.store.checkpoint.finish_snapshot();
    let bytes = std::fs::read(f._directory.0.join("checkpoint.1")).unwrap();
    let image = checkpoint::decode(&bytes).unwrap();
    assert_eq!(image.sequence, 7);
    assert_eq!(image.shards[0].index.entries.len(), 1);
    let geometry = checkpoint::CheckpointGeometry::new(
        1024 * 1024 * 1024,
        64 * 1024 * 1024,
        16,
        Alignment::new(4096, 4096, 4096).unwrap(),
    )
    .unwrap();
    let (payload, tail) = geometry.payload_capacity(2, 65536).unwrap();
    assert_eq!(payload, 672 * 1024 * 1024);
    assert!(tail > 200 * 1024 * 1024);
}

#[test]
fn record_round_trip_preserves_ciphertext_zeroes_padding_and_rejects_torn_header() {
    let f = Fixture::new();
    let a = Alignment::new(512, 512, 512).unwrap();
    for length in [3, crate::model::PAGE_BYTES as usize] {
        let page = f.copy(9, length);
        let disk = a.extent(0, format::logical_length(&page).unwrap()).unwrap();
        let reserve = f
            .admission
            .reserve(None, ResourceClass::Ciphertext, disk.length())
            .unwrap();
        let buffer = a.allocate(disk.length(), reserve).unwrap();
        assert_eq!(buffer.bytes().unwrap().as_ptr() as usize % a.memory(), 0);
        let mut encoded = format::encode(&page, Generation(7), a, buffer).unwrap();
        let parsed = format::parse(&encoded.buffer, disk).unwrap();
        assert_eq!(
            &encoded.buffer.bytes().unwrap()[parsed.ciphertext.clone()],
            page.ciphertext.bytes()
        );
        assert_eq!(parsed.header.metadata, page.metadata.immutable());
        assert!(
            encoded.buffer.bytes().unwrap()[parsed.ciphertext.end..]
                .iter()
                .all(|b| *b == 0)
        );
        assert_eq!(
            format::decode(&encoded.buffer, &encoded.header).unwrap(),
            *page.ciphertext.envelope()
        );
        encoded.header.generation = Generation(8);
        assert!(format::decode(&encoded.buffer, &encoded.header).is_err());
        encoded.buffer.bytes_mut().unwrap()[24] ^= 1;
        assert!(format::parse(&encoded.buffer, disk).is_err());
    }
}
#[test]
fn dirty_queue_is_bounded_and_cache_removal_discards_without_io() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    let first = f.copy(1, 3);
    let id = first.ciphertext.envelope().page.clone();
    let ticket = f.enqueue(first.clone()).unwrap();
    assert_eq!(f.enqueue(first).unwrap(), ticket);
    f.enqueue(f.copy(2, 3)).unwrap();
    assert!(matches!(f.enqueue(f.copy(3, 3)), Err(Error::Overloaded)));
    assert_eq!(
        f.metrics
            .count(crate::telemetry::metrics::Event::DirtyDiscard),
        0
    );
    assert!(f.store.writer.copy_only(&id).unwrap().is_some());
    f.store
        .writer
        .remove_cache(&CacheId(crate::security::test_support::CACHE.into()))
        .unwrap();
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    // Eviction alone has no permanent tombstone; production admission is the
    // current positive cache/key set, independently tested through the app.
    assert!(f.enqueue(f.copy(4, 3)).is_ok());
    assert_eq!(
        f.metrics
            .count(crate::telemetry::metrics::Event::DirtyDiscard),
        2
    );
}

#[test]
fn allocator_integration_rejects_foreign_and_mismatched_writer_charges() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    let foreign = flow_control::Quotas::new(AdmissionPolicy::new(f.admission.limits().clone()));
    let page = f.copy(1, 64);
    let cache = &page.metadata.version.object.cache;
    let other_cache = CacheId("other".into());
    for (owner, class, cache, amount) in [
        (&foreign, ResourceClass::DirtyCiphertext, cache, 80),
        (&*f.admission, ResourceClass::Ciphertext, cache, 80),
        (
            &*f.admission,
            ResourceClass::DirtyCiphertext,
            &other_cache,
            80,
        ),
        (&*f.admission, ResourceClass::DirtyCiphertext, cache, 79),
    ] {
        let dirty = owner.reserve(Some(cache), class, amount).unwrap();
        assert_eq!(
            f.store.writer.enqueue(page.clone(), dirty),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(f.store.writer.pending_count(), 0);
    }
    for variant in 0..4 {
        let dirty = f
            .admission
            .reserve(Some(cache), ResourceClass::DirtyCiphertext, 80)
            .unwrap();
        let result = f
            .store
            .writer
            .enqueue_reclaiming(page.clone(), dirty, |length| {
                let owner = if variant == 0 {
                    &foreign
                } else {
                    &*f.admission
                };
                let class = if variant == 1 {
                    ResourceClass::DirtyCiphertext
                } else {
                    ResourceClass::Ciphertext
                };
                let cache = if variant == 2 { &other_cache } else { cache };
                owner
                    .reserve(Some(cache), class, length - usize::from(variant == 3))
                    .map_err(Into::into)
            });
        assert_eq!(result, Err(Error::InvalidConfiguration));
        assert_eq!(f.store.writer.pending_count(), 0);
    }
    assert_eq!(foreign.used(ResourceClass::Ciphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    assert_eq!(f.reactor.in_flight(), 0);
    f.enqueue(page).unwrap();
    assert_eq!(f.store.writer.pending_count(), 1);
}

#[test]
fn allocator_integration_validates_reader_charges_before_submission() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    let page = f.copy(1, 64);
    let id = page.ciphertext.envelope().page.clone();
    let cache = &id.version.object.cache;
    let foreign = flow_control::Quotas::new(AdmissionPolicy::new(f.admission.limits().clone()));
    let other_cache = CacheId("other".into());
    let length = f
        .store
        .writer
        .slabs()
        .alignment()
        .unwrap()
        .extent(0, 512)
        .unwrap()
        .length();
    let (lease, extent) = f.segments.append(length).unwrap();
    f.store
        .writer
        .index()
        .publish(
            id.clone(),
            catalog::IndexedPage {
                location: catalog::RecordLocation {
                    segment: lease.id(),
                    generation: lease.generation(),
                    extent,
                },
                metadata: page.metadata.immutable(),
                key_id: page.ciphertext.envelope().key_id,
            },
        )
        .unwrap();
    drop(lease);
    for variant in 0..4 {
        let request = scope();
        let result = futures::executor::block_on(f.store.reader.read_with_token_reclaim(
            &id,
            &request,
            |amount| {
                let owner = if variant == 0 {
                    &foreign
                } else {
                    &*f.admission
                };
                let class = if variant == 1 {
                    ResourceClass::DirtyCiphertext
                } else {
                    ResourceClass::Ciphertext
                };
                let cache = if variant == 2 { &other_cache } else { cache };
                owner
                    .reserve(Some(cache), class, amount - usize::from(variant == 3))
                    .map_err(Into::into)
            },
        ));
        assert!(matches!(result, Err(Error::InvalidConfiguration)));
        assert_eq!(f.reactor.in_flight(), 0);
        assert!(f.store.writer.index().lookup(&id).unwrap().is_some());
    }
    assert_eq!(foreign.used(ResourceClass::Ciphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
}

#[test]
fn allocator_integration_reclaims_idle_charge_before_writer_admission_retry() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    let page = f.copy(1, 64);
    let cache = &page.metadata.version.object.cache;
    let slab = f.store.writer.slabs();
    let length = slab
        .alignment()
        .unwrap()
        .extent(0, format::logical_length(&page).unwrap())
        .unwrap()
        .length();
    let charge = f
        .admission
        .reserve(Some(cache), ResourceClass::Ciphertext, length)
        .unwrap();
    drop(slab.allocate(length, charge).unwrap());
    assert_eq!(slab.idle_bytes(), length);
    let pressure = f
        .admission
        .reserve(
            None,
            ResourceClass::Ciphertext,
            f.admission.limit(ResourceClass::Ciphertext)
                - f.admission.used(ResourceClass::Ciphertext),
        )
        .unwrap();
    f.enqueue(page).unwrap();
    assert_eq!(slab.idle_bytes(), 0);
    assert_eq!(f.store.writer.pending_count(), 1);
    assert_eq!(f.store.writer.discard_unsubmitted(), 1);
    drop(pressure);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
}

fn segment_images(f: &Fixture) -> Vec<(SegmentId, Generation, SegmentState, u64)> {
    f.segments
        .snapshot()
        .unwrap()
        .into_iter()
        .map(|s| (s.id, s.generation, s.state, s.used_bytes))
        .collect()
}

#[test]
fn index_capacity_rejection_preserves_segments_and_releases_all_charges_without_io() {
    let mut f = Fixture::new();
    let alignment = futures::executor::block_on(f.store.open()).unwrap();
    // A writer without a configured clock must still reject safely, before I/O.
    f.store.writer = Rc::new(writer::StoreWriter::new(
        f.store.writer.index().clone(),
        f.segments.clone(),
        f.store.writer.slabs().clone(),
        f.admission.clone(),
        f.reactor.clone(),
        crate::test_support::availability(),
    ));
    let index = f.store.writer.index();
    index.set_page_capacity(1).unwrap();
    // Both pages can enqueue while the one index slot is still free.
    f.enqueue(f.copy(1, 64)).unwrap();
    f.enqueue(f.copy(2, 64)).unwrap();
    assert_eq!(f.store.writer.pending_count(), 2);
    assert!(f.admission.used(ResourceClass::DirtyCiphertext) > 0);
    assert!(f.admission.used(ResourceClass::Ciphertext) > 160);

    // Seed a completed mapping to deterministically saturate the index before
    // progress, without initializing the reactor or submitting any payload I/O.
    let retained = f.copy(3, 64);
    let retained_id = retained.ciphertext.envelope().page.clone();
    let append = f
        .segments
        .append(alignment.extent(0, 512).unwrap().length())
        .unwrap();
    let location = catalog::RecordLocation {
        segment: append.0.id(),
        generation: append.0.generation(),
        extent: append.1,
    };
    index
        .publish(
            retained_id.clone(),
            catalog::IndexedPage {
                location: location.clone(),
                metadata: retained.metadata.immutable(),
                key_id: retained.ciphertext.envelope().key_id,
            },
        )
        .unwrap();
    drop((retained, append));
    let before = segment_images(&f);
    let request = scope();
    let mut progress = f.store.writer.progress(2, &request);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert_eq!(progress.as_mut().poll(&mut cx), Poll::Ready(Ok(2)));
    drop(progress);
    assert_eq!(segment_images(&f), before);
    assert_eq!(
        index.lookup(&retained_id).unwrap().unwrap().location,
        location
    );
    assert_eq!(f.store.writer.discarded_count(), 2);
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(f.store.writer.queued_count(), 0);
    assert!(f.store.writer.is_idle());
    assert_eq!(f.store.writer.writes_in_flight(), 0);
    assert_eq!(f.reactor.in_flight(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    f.store.writer.reclaim_idle_buffer();
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);

    assert!(matches!(f.enqueue(f.copy(4, 64)), Err(Error::Overloaded)));
    let mut malformed = f.copy(5, 64);
    malformed.metadata.length = 0;
    assert_eq!(
        format::logical_length(&malformed),
        Err(Error::CorruptRecord)
    );
    // Capacity rejection precedes even header validation/length calculation.
    assert!(matches!(f.enqueue(malformed), Err(Error::Overloaded)));
    assert_eq!(segment_images(&f), before);
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(f.store.writer.queued_count(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
}

#[test]
fn real_writer_rechecks_index_capacity_and_replaces_same_page_when_full() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().expect("storage test requires io_uring");
    let index = f.store.writer.index();
    index.set_page_capacity(1).unwrap();
    let first = f.copy(1, 64);
    let first_id = first.ciphertext.envelope().page.clone();
    let second = f.copy(2, 64);
    let second_id = second.ciphertext.envelope().page.clone();
    f.enqueue(first).unwrap();
    f.enqueue(second).unwrap();
    assert_eq!(
        f.metrics
            .gauge(crate::telemetry::metrics::Gauge::PendingDiskWrites),
        2
    );
    assert_eq!(
        f.metrics
            .count(crate::telemetry::metrics::Event::DiskPublication),
        0
    );
    let request = scope();
    let mut progress = f.store.writer.progress(1, &request);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(progress.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    assert_eq!(
        futures::executor::block_on(f.store.writer.progress(1, &request)),
        Err(Error::Overloaded)
    );
    assert_eq!(drive(&f.reactor, progress).unwrap(), 1);
    let original = index.lookup(&first_id).unwrap().unwrap().location;
    assert_eq!(
        f.metrics
            .count(crate::telemetry::metrics::Event::DiskPublication),
        1
    );
    assert_eq!(
        f.metrics
            .gauge(crate::telemetry::metrics::Gauge::PendingDiskWrites),
        1
    );
    // The queued page now turns over the full index and performs real I/O.
    let mut progress = f.store.writer.progress(1, &request);
    assert!(progress.as_mut().poll(&mut cx).is_pending());
    assert_eq!(drive(&f.reactor, progress).unwrap(), 1);
    assert!(index.lookup(&second_id).unwrap().is_some());
    assert!(index.lookup(&first_id).unwrap().is_none());
    assert_eq!(
        f.metrics
            .count(crate::telemetry::metrics::Event::DiskPublication),
        2
    );
    assert_eq!(
        f.metrics
            .gauge(crate::telemetry::metrics::Gauge::PendingDiskWrites),
        0
    );
    assert_eq!(f.store.writer.discarded_count(), 0);
    assert!(f.store.writer.is_idle());
    assert_eq!(f.store.writer.queued_count(), 0);
    assert_eq!(f.store.writer.writes_in_flight(), 0);
    assert_eq!(f.reactor.in_flight(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    f.store.writer.reclaim_idle_buffer();
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);

    f.enqueue(f.copy(1, 64)).unwrap();
    assert_eq!(
        drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap(),
        1
    );
    let replacement = index.lookup(&first_id).unwrap().unwrap().location;
    assert_ne!(replacement, original);
    assert_eq!(replacement.generation, original.generation);
    assert_eq!(index.snapshot().unwrap().entries.len(), 1);
    let read = drive(&f.reactor, f.store.reader.read(&first_id, &request))
        .unwrap()
        .unwrap();
    assert_eq!(read.ciphertext.bytes(), &[1; 80]);
    drop(read);
    // An actual same-key replacement remains admissible at capacity.
    f.enqueue(f.copy(1, 64)).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap();
    let latest = index.lookup(&first_id).unwrap().unwrap().location;
    assert_ne!(latest, replacement);
    index.remove_if_matches(&first_id, &replacement);
    assert_eq!(index.lookup(&first_id).unwrap().unwrap().location, latest);
    f.enqueue(f.copy(2, 64)).unwrap();
    assert_eq!(
        drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap(),
        1
    );
    assert!(index.lookup(&second_id).unwrap().is_some());
    assert!(index.lookup(&first_id).unwrap().is_none());
    assert!(f.store.writer.is_idle());
    assert_eq!(f.reactor.in_flight(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    f.store.writer.reclaim_idle_buffer();
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
}

#[test]
fn persistence_pressure_and_restart_preserve_only_live_pages() {
    for (capacity, length) in [(1, 3), (2, 4096), (4, 65536)] {
        let f = Fixture::new();
        f.store.writer.index().set_page_capacity(capacity).unwrap();
        drive(&f.reactor, f.store.open()).unwrap();
        let request = scope();
        let mut expected = Vec::new();
        for number in 1..=6 {
            let page = f.copy(number, length);
            expected.push(page.ciphertext.envelope().page.clone());
            f.enqueue(page).unwrap();
            drive(&f.reactor, f.store.writer.progress(8, &request)).unwrap();
            let copy = drive(
                &f.reactor,
                f.store.reader.read(expected.last().unwrap(), &request),
            )
            .unwrap()
            .unwrap();
            assert_eq!(copy.ciphertext.bytes(), vec![number; length + 16]);
        }
        let live: Vec<_> = expected
            .iter()
            .map(|id| {
                drive(&f.reactor, f.store.reader.read(id, &request))
                    .unwrap()
                    .is_some()
            })
            .collect();
        assert!(live.iter().filter(|&&present| present).count() <= capacity);
        assert!(live.last().copied().unwrap());
        let image = drive(&f.reactor, f.store.checkpoint.snapshot_shard()).unwrap();
        drive(&f.reactor, f.store.checkpoint.publish(vec![image])).unwrap();
        f.store.checkpoint.finish_snapshot();
        assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);

        // Drop every old component, then reopen the same actual slab/checkpoint files.
        let f = f.restart();
        // Ring teardown may release the kernel's final file reference after the
        // userspace owners drop. Bound the wait for the exclusive slab lock;
        // an actual leaked owner must still fail the restart scenario.
        let unlock_deadline = Instant::now() + Duration::from_secs(2);
        let alignment = loop {
            match drive(&f.reactor, f.store.open()) {
                Ok(alignment) => break alignment,
                Err(Error::Unavailable) if Instant::now() < unlock_deadline => {
                    std::thread::sleep(Duration::from_millis(1));
                }
                result => panic!("slab did not reopen after owner teardown: {result:?}"),
            }
        };
        let image = drive(&f.reactor, f.store.recovery.load(alignment))
            .unwrap()
            .unwrap();
        drive(
            &f.reactor,
            f.store
                .recovery
                .install_shard(image.shards.into_iter().next()),
        )
        .unwrap();
        for (index, id) in expected.iter().enumerate() {
            let copy = drive(&f.reactor, f.store.reader.read(id, &request)).unwrap();
            assert_eq!(copy.is_some(), live[index]);
            if let Some(copy) = copy {
                assert_eq!(copy.ciphertext.bytes(), vec![index as u8 + 1; length + 16]);
                assert_eq!(copy.ciphertext.envelope().page, *id);
            }
        }
    }
}

#[test]
fn real_direct_slab_roundtrip_checkpoint_and_corruption_miss() {
    let f = Fixture::new();
    let alignment = futures::executor::block_on(f.store.open())
        .expect("test filesystem must support O_DIRECT and STATX_DIOALIGN");
    f.reactor.init().expect("storage test requires io_uring");
    let page = f.copy(3, 23);
    let id = page.ciphertext.envelope().page.clone();
    f.enqueue(page.clone()).unwrap();
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    let request = scope();
    assert_eq!(
        drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap(),
        1
    );
    let (read, token) = drive(&f.reactor, f.store.reader.read_with_token(&id, &request))
        .unwrap()
        .unwrap();
    assert_eq!(read.ciphertext.bytes(), page.ciphertext.bytes());
    assert_eq!(read.ciphertext.envelope(), page.ciphertext.envelope());
    let image = futures::executor::block_on(f.store.checkpoint.snapshot_shard()).unwrap();
    futures::executor::block_on(f.store.checkpoint.publish(vec![image])).unwrap();
    f.store.checkpoint.finish_snapshot();
    let loaded = futures::executor::block_on(f.store.recovery.load(alignment))
        .unwrap()
        .unwrap();
    futures::executor::block_on(
        f.store
            .recovery
            .install_shard(loaded.shards.into_iter().next()),
    )
    .unwrap();
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &request))
            .unwrap()
            .is_some()
    );
    let location = f
        .store
        .writer
        .index()
        .lookup(&id)
        .unwrap()
        .unwrap()
        .location;
    let damaged = f
        .store
        .writer
        .slabs()
        .allocate(
            location.extent.length(),
            f.admission
                .reserve(None, ResourceClass::Ciphertext, location.extent.length())
                .unwrap(),
        )
        .unwrap();
    let lease = f.store.writer.lease(&location).unwrap();
    drive(
        &f.reactor,
        f.store
            .writer
            .slabs()
            .write(&f.reactor, location.extent, damaged, lease, &request),
    )
    .unwrap();
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &request))
            .unwrap()
            .is_none()
    );
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    f.store.reader.invalidate(&token).unwrap();
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &request))
            .unwrap()
            .is_none()
    );
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    assert_eq!(
        f.metrics
            .count(crate::telemetry::metrics::Event::CorruptMiss),
        1
    );
}

#[test]
fn stored_payload_and_tag_corruption_fail_mandatory_checksum() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    let page = f.copy(3, 23);
    let id = page.ciphertext.envelope().page.clone();
    let request = scope();
    for corrupt_tag in [false, true] {
        f.enqueue(page.clone()).unwrap();
        drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap();
        let location = f
            .store
            .writer
            .index()
            .lookup(&id)
            .unwrap()
            .unwrap()
            .location;
        let staging = f
            .store
            .writer
            .slabs()
            .allocate(
                location.extent.length(),
                f.admission
                    .reserve(None, ResourceClass::Ciphertext, location.extent.length())
                    .unwrap(),
            )
            .unwrap();
        let lease = f.store.writer.lease(&location).unwrap();
        let mut stored = drive(
            &f.reactor,
            f.store
                .writer
                .slabs()
                .read(&f.reactor, location.extent, staging, lease, &request),
        )
        .unwrap();
        let parsed = super::format::parse(&stored, location.extent).unwrap();
        assert_eq!(parsed.header.format_version, super::format::FORMAT_VERSION);
        assert_eq!(parsed.checksum, page.ciphertext.checksum());
        let offset = if corrupt_tag {
            parsed.ciphertext.end - 1
        } else {
            parsed.ciphertext.start
        };
        stored.bytes_mut().unwrap()[offset] ^= 1;
        let lease = f.store.writer.lease(&location).unwrap();
        drive(
            &f.reactor,
            f.store
                .writer
                .slabs()
                .write(&f.reactor, location.extent, stored, lease, &request),
        )
        .unwrap();
        let (read, token) = drive(&f.reactor, f.store.reader.read_with_token(&id, &request))
            .unwrap()
            .unwrap();
        // The I/O worker carries the stored expectation; crypto-worker validation
        // rejects corruption before AEAD can publish plaintext.
        assert_eq!(read.ciphertext.checksum(), page.ciphertext.checksum());
        assert_eq!(read.ciphertext.verify_checksum(), Err(Error::CorruptRecord));
        f.store.reader.invalidate(&token).unwrap();
        assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    }
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
}

#[test]
fn abandoned_write_retains_kernel_lease_and_cannot_publish() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    let page = f.copy(1, 64);
    let id = page.ciphertext.envelope().page.clone();
    f.enqueue(page).unwrap();
    let request = scope();
    let mut operation = f.store.writer.progress(1, &request);
    let waker = futures::task::noop_waker();
    let mut cx = Context::from_waker(&waker);
    assert!(operation.as_mut().poll(&mut cx).is_pending());
    assert!(f.reactor.in_flight() > 0);
    drop(operation);
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    assert_eq!(
        f.metrics
            .count(crate::telemetry::metrics::Event::DirtyDiscard),
        1
    );
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(
        f.metrics
            .gauge(crate::telemetry::metrics::Gauge::PendingDiskWrites),
        0
    );
    assert_eq!(
        f.metrics
            .count(crate::telemetry::metrics::Event::DiskPublication),
        0
    );
    assert!(f.admission.used(ResourceClass::Ciphertext) > 0);
    assert!(!f.store.writer.is_idle());
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    let deadline = Instant::now() + Duration::from_secs(10);
    while f.reactor.in_flight() != 0 {
        f.reactor.poll_budgeted(32).unwrap();
        assert!(Instant::now() < deadline);
    }
    f.store.writer.reclaim_idle_buffer();
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert!(f.store.writer.is_idle());
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
}

#[test]
fn cache_removal_during_write_fences_late_publication() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    let page = f.copy(1, 64);
    let id = page.ciphertext.envelope().page.clone();
    f.enqueue(page).unwrap();
    let request = scope();
    let mut operation = f.store.writer.progress(1, &request);
    let waker = futures::task::noop_waker();
    let mut cx = Context::from_waker(&waker);
    assert!(operation.as_mut().poll(&mut cx).is_pending());
    f.store
        .writer
        .remove_cache(&CacheId(crate::security::test_support::CACHE.into()))
        .unwrap();
    // Reinsert the same immutable page while the original submitted write still
    // owns its segment/buffer. The old completion cannot publish or remove it.
    let replacement = f.enqueue(f.copy(1, 64)).unwrap();
    assert!(replacement > 1);
    drive(&f.reactor, operation).unwrap();
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    assert_eq!(f.store.writer.pending_count(), 1);
    assert_eq!(
        f.metrics
            .gauge(crate::telemetry::metrics::Gauge::PendingDiskWrites),
        1
    );
    assert_eq!(
        f.metrics
            .count(crate::telemetry::metrics::Event::DiskPublication),
        0
    );
    drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap();
    assert_eq!(
        f.metrics
            .gauge(crate::telemetry::metrics::Gauge::PendingDiskWrites),
        0
    );
    assert_eq!(
        f.metrics
            .count(crate::telemetry::metrics::Event::DiskPublication),
        1
    );
    assert!(f.store.writer.index().lookup(&id).unwrap().is_some());
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
}

#[test]
fn sustained_rotation_reclaims_history_and_fences_held_pages_and_write_completions() {
    use crate::{
        control::state::for_caches,
        memory::{cache::MemoryCache, pool::tests::bundle_for},
        security::test_support::{keys, rotation_bundle},
    };
    use racer_identity::KeyPurpose;
    use std::sync::Arc;
    let keys = Rc::new(keys());
    let roots = (*keys.peer_trust_roots().unwrap()).clone();
    let cache = CacheId(crate::security::test_support::CACHE.into());
    let availability = for_caches(keys.clone(), vec![cache.clone()]);
    let f = Fixture::assemble(Directory::new(), availability.clone());
    let memory = MemoryCache::new(f.pool.clone(), availability);
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    let mut descriptor = f.copy(1, 3).metadata.immutable();
    descriptor.version.object.cache = cache.clone();
    let late = bundle_for(&f.admission, descriptor.clone());
    memory.publish(late.clone()).unwrap();
    let request = RequestScope::new(
        RequestId([0; 16]),
        Instant::now() + Duration::from_secs(300),
    )
    .unwrap();
    // More than the former writer limit, and twice as many keyring retirements.
    // Lookups and writer progress consult current availability without eager eviction.
    for generation in 2..=65539 {
        let lease = keys.active(&cache, KeyPurpose::Page).unwrap();
        let old = lease.reference().clone();
        let mut page = bundle_for(&f.admission, descriptor.clone());
        Arc::get_mut(&mut page.ciphertext.inner)
            .unwrap()
            .envelope
            .key_id = old.id;
        memory.publish(page.clone()).unwrap();
        f.enqueue(page.copy()).unwrap();
        let mut write = if generation == 2 {
            let mut operation = f.store.writer.progress(1, &request);
            assert!(
                operation
                    .as_mut()
                    .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                    .is_pending()
            );
            Some(operation)
        } else {
            None
        };
        keys.install(rotation_bundle(generation, roots.clone()))
            .unwrap();
        assert!(memory.get(page.plaintext.page()).unwrap().is_none());
        assert!(
            f.store
                .writer
                .copy_only(page.plaintext.page())
                .unwrap()
                .is_none()
        );
        assert_eq!(memory.publish(page.clone()), Err(Error::MissingKey));
        assert!(matches!(f.enqueue(page.copy()), Err(Error::MissingKey)));
        assert!(keys.lease(Some(&cache), old.id, KeyPurpose::Page).is_err());
        assert_eq!(lease.id(), old.id);
        let mut sealed = [0; 19];
        lease
            .seal_page(&cache, &[1; 24], b"retained", b"abc", &mut sealed)
            .unwrap();
        let mut opened = [0; 3];
        lease
            .open_page(&cache, old.id, &[1; 24], b"retained", &sealed, &mut opened)
            .unwrap();
        assert_eq!(&opened, b"abc");
        assert_eq!(page.plaintext.bytes(), &[1; 3]);
        if let Some(operation) = write.take() {
            assert!(f.store.writer.writes_in_flight() > 0);
            assert!(f.admission.used(ResourceClass::DirtyCiphertext) > 0);
            drive(&f.reactor, operation).unwrap();
            assert!(
                f.store
                    .writer
                    .index()
                    .lookup(page.plaintext.page())
                    .unwrap()
                    .is_none()
            );
        } else {
            // Unsubmitted stale copies are discarded without issuing a write.
            assert_eq!(
                drive(&f.reactor, f.store.writer.progress(1, &request)),
                Ok(0)
            );
        }
        assert_eq!(f.store.writer.pending_count(), 0);
        assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
        drop(lease);
    }
    assert_eq!(memory.publish(late.clone()), Err(Error::MissingKey));
    assert!(matches!(f.enqueue(late.copy()), Err(Error::MissingKey)));
    // Current keys still publish after all old limits have been exceeded.
    let mut page = bundle_for(&f.admission, descriptor);
    Arc::get_mut(&mut page.ciphertext.inner)
        .unwrap()
        .envelope
        .key_id = keys.active(&cache, KeyPurpose::Page).unwrap().id();
    memory.publish(page.clone()).unwrap();
    f.enqueue(page.copy()).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap();
    assert!(
        f.store
            .writer
            .index()
            .lookup(page.plaintext.page())
            .unwrap()
            .is_some()
    );
}

#[test]
fn truncated_payload_is_a_miss_and_write_failure_releases_dirty_accounting() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    let request = scope();
    let page = f.copy(7, 64);
    let id = page.ciphertext.envelope().page.clone();
    f.enqueue(page).unwrap();
    drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap();
    let file = std::fs::OpenOptions::new()
        .write(true)
        .open(f._directory.0.join("worker-0-slab-0.dat"))
        .unwrap();
    file.set_len(0).unwrap();
    assert!(
        drive(&f.reactor, f.store.reader.read(&id, &request))
            .unwrap()
            .is_none()
    );
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    let page = f.copy(8, 64);
    f.enqueue(page).unwrap();
    request.cancel().unwrap();
    assert_eq!(
        drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap(),
        0
    );
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
}

#[test]
fn stopped_admission_drains_two_accepted_copies_with_no_spare_ciphertext_quota() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    let first = f.copy(1, 64);
    let first_id = first.ciphertext.envelope().page.clone();
    let second = f.copy(2, 64);
    let second_id = second.ciphertext.envelope().page.clone();
    f.enqueue(first).unwrap();
    f.enqueue(second).unwrap();
    let used = f.admission.used(ResourceClass::Ciphertext);
    assert!(
        used > 160,
        "both accepted writes must already own padded staging quota"
    );
    let pressure = f
        .admission
        .reserve(
            None,
            ResourceClass::Ciphertext,
            f.admission.limit(ResourceClass::Ciphertext) - used,
        )
        .unwrap();
    f.admission.stop();
    assert!(
        f.admission
            .reserve(None, ResourceClass::Ciphertext, 512)
            .is_err()
    );
    let request = scope();
    drive(&f.reactor, f.store.writer.drain(&request)).unwrap();
    assert!(f.store.writer.index().lookup(&first_id).unwrap().is_some());
    assert!(f.store.writer.index().lookup(&second_id).unwrap().is_some());
    assert!(f.store.writer.is_idle());
    assert_eq!(f.store.writer.writes_in_flight(), 0);
    assert_eq!(f.reactor.in_flight(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    drop(pressure);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
}

#[test]
fn staging_pressure_rejects_before_queue_acceptance_and_preserves_accepted_work() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    let first = f.copy(1, 64);
    let second = f.copy(2, 64);
    f.enqueue(first).unwrap();
    let pressure = f
        .admission
        .reserve(
            None,
            ResourceClass::Ciphertext,
            f.admission.limit(ResourceClass::Ciphertext)
                - f.admission.used(ResourceClass::Ciphertext),
        )
        .unwrap();
    assert!(matches!(f.enqueue(second), Err(Error::Overloaded)));
    assert_eq!(f.store.writer.pending_count(), 1);
    f.admission.stop();
    drive(&f.reactor, f.store.writer.drain(&scope())).unwrap();
    assert!(f.store.writer.is_idle());
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    drop(pressure);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
}

#[test]
fn shutdown_deadline_discards_second_copy_and_fences_submitted_first_copy() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    f.enqueue(f.copy(1, 64)).unwrap();
    f.enqueue(f.copy(2, 64)).unwrap();
    f.admission.stop();
    let request = scope();
    let mut write = f.store.writer.progress(1, &request);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(write.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.store.writer.queued_count(), 1);
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    let expired =
        RequestScope::new(RequestId([9; 16]), Instant::now() - Duration::from_secs(1)).unwrap();
    let mut drain = f.store.writer.drain(&expired);
    assert!(drain.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.store.writer.queued_count(), 0);
    assert_eq!(f.store.writer.pending_count(), 1);
    assert!(!f.store.writer.is_idle());
    assert!(f.admission.used(ResourceClass::DirtyCiphertext) > 0);
    let submitted = &f.segments;
    let mut snapshot = submitted.snapshot().unwrap();
    snapshot[0].state = SegmentState::Sealed;
    // Active segment leases prohibit restore/reuse even though queued work is gone.
    assert!(submitted.restore(snapshot).is_err());
    drive(&f.reactor, write).unwrap();
    drive(&f.reactor, drain).unwrap();
    assert!(f.store.writer.is_idle());
    assert_eq!(f.reactor.in_flight(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert!(
        f.store
            .writer
            .index()
            .snapshot()
            .unwrap()
            .entries
            .is_empty()
    );
}

#[test]
fn incremental_ciphertext_reclamation_preserves_submitted_fence_and_remaining_queue() {
    let f = Fixture::new();
    f.store.configure(f.admission.clone(), 3, 16).unwrap();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    f.enqueue(f.copy(1, 64)).unwrap();
    f.enqueue(f.copy(2, 64)).unwrap();
    f.enqueue(f.copy(3, 64)).unwrap();
    let request = scope();
    let mut write = f.store.writer.progress(1, &request);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(write.as_mut().poll(&mut cx).is_pending());
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    let before = f.admission.used(ResourceClass::Ciphertext);
    let released = f.store.writer.reclaim_ciphertext(
        Some(&CacheId(crate::security::test_support::CACHE.into())),
        1,
    );
    assert!(released > 0);
    assert_eq!(
        before - f.admission.used(ResourceClass::Ciphertext),
        released
    );
    assert_eq!(f.store.writer.discarded_count(), 1);
    assert_eq!(f.store.writer.queued_count(), 1);
    assert_eq!(f.store.writer.pending_count(), 2);
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    assert!(!f.store.writer.is_idle());
    drive(&f.reactor, write).unwrap();
    drive(&f.reactor, f.store.writer.drain(&request)).unwrap();
    assert_eq!(f.store.writer.index().snapshot().unwrap().entries.len(), 2);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
}

#[test]
fn disk_read_reclaims_exact_staging_and_decode_charges_without_flushing_queue() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    let copy = f.copy(1, 64);
    let id = copy.ciphertext.envelope().page.clone();
    f.enqueue(copy).unwrap();
    let request = scope();
    drive(&f.reactor, f.store.writer.progress(1, &request)).unwrap();
    let disk_bytes = f
        .store
        .writer
        .index()
        .lookup(&id)
        .unwrap()
        .unwrap()
        .location
        .extent
        .length();
    f.enqueue(f.copy(2, 64)).unwrap();
    let last = f.copy(3, 64);
    let last_id = last.ciphertext.envelope().page.clone();
    f.enqueue(last).unwrap();
    let _pressure = f
        .admission
        .reserve(
            None,
            ResourceClass::Ciphertext,
            f.admission.limit(ResourceClass::Ciphertext)
                - f.admission.used(ResourceClass::Ciphertext),
        )
        .unwrap();
    let allocations = std::cell::RefCell::new(Vec::new());
    let (read, _) = drive(
        &f.reactor,
        f.store
            .reader
            .read_with_token_reclaim(&id, &request, |amount| {
                allocations.borrow_mut().push(amount);
                match f
                    .admission
                    .reserve(
                        Some(&id.version.object.cache),
                        ResourceClass::Ciphertext,
                        amount,
                    )
                    .map_err(Error::from)
                {
                    Err(Error::Overloaded) => {
                        let (cache, bytes) = f
                            .admission
                            .reclamation(
                                &id.version.object.cache,
                                ResourceClass::Ciphertext,
                                amount,
                            )
                            .unwrap();
                        f.store.writer.reclaim_ciphertext(cache.as_ref(), bytes);
                        f.admission
                            .reserve(
                                Some(&id.version.object.cache),
                                ResourceClass::Ciphertext,
                                amount,
                            )
                            .map_err(Into::into)
                    }
                    result => result,
                }
            }),
    )
    .unwrap()
    .unwrap();
    assert_eq!(*allocations.borrow(), [disk_bytes + 80]);
    assert_eq!(read.ciphertext.bytes(), &[1; 80]);
    assert_eq!(f.store.writer.discarded_count(), 1);
    assert_eq!(f.store.writer.queued_count(), 1);
    assert!(f.store.writer.copy_only(&last_id).unwrap().is_some());
}

#[test]
fn unreclaimable_segment_pressure_discards_copies_without_fatal_progress_error() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    f.enqueue(f.copy(1, 64)).unwrap();
    f.enqueue(f.copy(2, 64)).unwrap();
    let segments = &f.segments;
    let first = segments.append(32 * 1024 * 1024).unwrap();
    let second = segments.append(32 * 1024 * 1024).unwrap();
    f.admission.stop();
    assert_eq!(
        drive(&f.reactor, f.store.writer.progress(2, &scope())).unwrap(),
        2
    );
    assert_eq!(f.store.writer.discarded_count(), 2);
    assert!(f.store.writer.is_idle());
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    f.store.writer.reclaim_idle_buffer();
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert!(segments.recycle(SegmentId(0)).is_err());
    drop((first, second));
    segments.recycle(SegmentId(0)).unwrap();
}

#[test]
fn actual_enospc_completion_discards_both_writes_and_drains() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    f.enqueue(f.copy(1, 64)).unwrap();
    f.enqueue(f.copy(2, 64)).unwrap();
    // Real kernel ENOSPC from /dev/full, substituted only as the test fault target.
    // Production slab open remains exclusively O_DIRECT with no fallback.
    f.store.writer.slabs().replace_file_for_test(
        std::fs::OpenOptions::new()
            .write(true)
            .open("/dev/full")
            .unwrap(),
    );
    f.admission.stop();
    drive(&f.reactor, f.store.writer.drain(&scope())).unwrap();
    assert_eq!(f.store.writer.discarded_count(), 2);
    assert!(f.store.writer.is_idle());
    assert!(
        f.store
            .writer
            .index()
            .snapshot()
            .unwrap()
            .entries
            .is_empty()
    );
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert_eq!(f.reactor.in_flight(), 0);
}

#[test]
fn fill_accepted_before_stop_can_transfer_dirty_ownership_during_drain() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    let page = f.copy(1, 64);
    let id = page.ciphertext.envelope().page.clone();
    let dirty = f
        .admission
        .reserve(
            Some(&page.metadata.version.object.cache),
            ResourceClass::DirtyCiphertext,
            page.ciphertext.bytes().len(),
        )
        .unwrap();
    f.admission.stop();
    f.store.writer.enqueue(page, dirty).unwrap();
    drive(&f.reactor, f.store.writer.drain(&scope())).unwrap();
    assert!(f.store.writer.index().lookup(&id).unwrap().is_some());
    assert!(f.store.writer.is_idle());
}
