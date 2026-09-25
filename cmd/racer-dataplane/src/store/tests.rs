use super::*;
use crate::{
    error::{Error, Result},
    memory::{page::CiphertextCopy, pool::BufferPool},
    model::{
        envelope::{KeyId, Nonce, PageEnvelope},
        identity::{
            CacheId, CacheKey, ObjectId, ObjectVersion, PageId, PageNumber, RequestId, StrongEtag,
            WorkerId,
        },
        limits::ResourceClass,
        metadata::{ExpiresAt, ObjectMetadata},
    },
    runtime::{
        admission::Admission,
        deadline::RequestScope,
        reactor::{IoBuffer, Reactor},
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
    store: Store,
    admission: Rc<Admission>,
    reactor: Rc<Reactor>,
    pool: Rc<BufferPool>,
    _directory: Directory,
}
impl Fixture {
    fn new() -> Self {
        let directory = Directory::new();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = Rc::new(BufferPool::new(admission.clone()));
        let index = Rc::new(index::Index::new(WorkerId(0), 16));
        let segments = Rc::new(segment::Segments::new(WorkerId(0), 32 * 1024 * 1024));
        let eviction = Rc::new(eviction::SegmentClock::new(
            index.clone(),
            segments.clone(),
            1,
        ));
        let slabs = Rc::new(slab::Slabs::new(
            WorkerId(0),
            directory.0.clone(),
            reactor.clone(),
            64 * 1024 * 1024,
            32 * 1024 * 1024,
        ));
        let reader = Rc::new(reader::StoreReader::new(
            eviction.clone(),
            index.clone(),
            segments.clone(),
            slabs.clone(),
            pool.clone(),
        ));
        let writer = Rc::new(writer::StoreWriter::new(
            index.clone(),
            segments.clone(),
            slabs,
        ));
        let store = Store {
            reader,
            writer,
            checkpoint: checkpoint::Checkpointer::new(
                directory.0.clone(),
                index.clone(),
                segments.clone(),
            ),
            recovery: recovery::Recovery::new(directory.0.clone(), index, segments),
            eviction,
        };
        store.configure(admission.clone(), 2, 16).unwrap();
        Self {
            store,
            admission,
            reactor,
            pool,
            _directory: directory,
        }
    }
    fn copy(&self, number: u8, length: usize) -> CiphertextCopy {
        let version = ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([number; 32]),
            },
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        };
        let envelope = PageEnvelope {
            page: PageId {
                version: version.clone(),
                number: PageNumber(0),
            },
            key_id: KeyId([1; 16]),
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
                version,
                length: length as u64,
                expires_at: ExpiresAt(std::time::UNIX_EPOCH),
            },
            ciphertext: self
                .pool
                .ciphertext(reservation, envelope, vec![number; length + 16])
                .unwrap(),
        }
    }
    fn enqueue(&self, copy: CiphertextCopy) -> Result<writer::DirtyTicket> {
        let dirty = self.admission.reserve(
            Some(&copy.metadata.version.object.cache),
            ResourceClass::DirtyCiphertext,
            copy.ciphertext.bytes().len(),
        )?;
        self.store.writer.enqueue(copy, dirty)
    }
}
fn scope() -> RequestScope {
    RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(10)).unwrap()
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
fn record_round_trip_preserves_ciphertext_zeroes_padding_and_rejects_torn_header() {
    let f = Fixture::new();
    let a = direct::DirectAlignment::validate(512, 512, 512).unwrap();
    for length in [3, crate::model::range::PAGE_BYTES as usize] {
        let page = f.copy(9, length);
        let disk = a
            .extent(0, format::RecordCodec.logical_length(&page).unwrap())
            .unwrap();
        let reserve = f
            .admission
            .reserve(None, ResourceClass::Ciphertext, disk.length())
            .unwrap();
        let buffer = a.allocate(disk.length(), reserve).unwrap();
        assert_eq!(buffer.bytes().unwrap().as_ptr() as usize % a.memory(), 0);
        let mut encoded = format::RecordCodec
            .encode(&page, segment::Generation(7), a, buffer)
            .unwrap();
        let parsed = format::RecordCodec.parse(&encoded.buffer, disk).unwrap();
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
            format::RecordCodec
                .decode(&encoded.buffer, &encoded.header)
                .unwrap(),
            *page.ciphertext.envelope()
        );
        encoded.header.generation = segment::Generation(8);
        assert!(
            format::RecordCodec
                .decode(&encoded.buffer, &encoded.header)
                .is_err()
        );
        encoded.buffer.bytes_mut().unwrap()[24] ^= 1;
        assert!(format::RecordCodec.parse(&encoded.buffer, disk).is_err());
    }
}
#[test]
fn dirty_queue_is_bounded_and_retirement_discards_without_io() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    let first = f.copy(1, 3);
    let id = first.ciphertext.envelope().page.clone();
    let ticket = f.enqueue(first.clone()).unwrap();
    assert_eq!(f.enqueue(first).unwrap().id(), ticket.id());
    f.enqueue(f.copy(2, 3)).unwrap();
    assert!(matches!(f.enqueue(f.copy(3, 3)), Err(Error::Overloaded)));
    assert!(f.store.writer.copy_only(&id).unwrap().is_some());
    f.store
        .writer
        .retire_key(&CacheId("cache".into()), KeyId([1; 16]))
        .unwrap();
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
    assert!(matches!(f.enqueue(f.copy(4, 3)), Err(Error::MissingKey)));
}

fn segment_images(
    f: &Fixture,
) -> Vec<(
    segment::SegmentId,
    segment::Generation,
    segment::SegmentState,
    u64,
)> {
    f.store
        .writer
        .segments_for_test()
        .snapshot()
        .unwrap()
        .into_iter()
        .map(|s| (s.id, s.generation, s.state, s.used_bytes))
        .collect()
}

#[test]
fn index_capacity_rejection_preserves_segments_and_releases_all_charges_without_io() {
    let f = Fixture::new();
    let alignment = futures::executor::block_on(f.store.open()).unwrap();
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
        .store
        .writer
        .segments_for_test()
        .append(alignment.extent(0, 512).unwrap().length())
        .unwrap();
    let location = index::RecordLocation {
        segment: append.segment.id(),
        generation: append.segment.generation(),
        location: append.location,
    };
    index
        .publish(
            retained_id.clone(),
            index::IndexedPage {
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
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);

    assert!(matches!(f.enqueue(f.copy(4, 64)), Err(Error::Overloaded)));
    let mut malformed = f.copy(5, 64);
    malformed.metadata.length = 0;
    assert_eq!(
        format::RecordCodec.logical_length(&malformed),
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
    let before = segment_images(&f);

    // The second queued page must finish synchronously without another append
    // or reactor submission now that the first page occupies the only slot.
    let mut progress = f.store.writer.progress(1, &request);
    assert_eq!(progress.as_mut().poll(&mut cx), Poll::Ready(Ok(1)));
    drop(progress);
    assert_eq!(segment_images(&f), before);
    assert!(index.lookup(&second_id).unwrap().is_none());
    assert_eq!(index.lookup(&first_id).unwrap().unwrap().location, original);
    assert_eq!(f.store.writer.discarded_count(), 1);
    assert!(f.store.writer.is_idle());
    assert_eq!(f.store.writer.queued_count(), 0);
    assert_eq!(f.store.writer.writes_in_flight(), 0);
    assert_eq!(f.reactor.in_flight(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
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
    assert!(matches!(f.enqueue(f.copy(2, 64)), Err(Error::Overloaded)));
    index.remove_if_matches(&first_id, &replacement).unwrap();
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
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
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
        .allocate(location.location.extent.length(), None)
        .unwrap();
    let lease = f.store.writer.lease(&location).unwrap();
    drive(
        &f.reactor,
        f.store
            .writer
            .slabs()
            .write(location.location, damaged, lease, &request),
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
    assert_eq!(f.store.writer.pending_count(), 0);
    assert!(f.admission.used(ResourceClass::Ciphertext) > 0);
    assert!(!f.store.writer.is_idle());
    assert_eq!(f.store.writer.writes_in_flight(), 1);
    let deadline = Instant::now() + Duration::from_secs(10);
    while f.reactor.in_flight() != 0 {
        f.reactor.poll_budgeted(32).unwrap();
        assert!(Instant::now() < deadline);
    }
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert!(f.store.writer.is_idle());
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
}

#[test]
fn retirement_during_write_fences_late_publication() {
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
        .retire_key(&CacheId("cache".into()), KeyId([1; 16]))
        .unwrap();
    drive(&f.reactor, operation).unwrap();
    assert!(f.store.writer.index().lookup(&id).unwrap().is_none());
    assert_eq!(f.store.writer.pending_count(), 0);
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
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
    f.store
        .writer
        .retire_key(&CacheId("cache".into()), KeyId([1; 16]))
        .unwrap();
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
    assert!(f.store.writer.slabs().allocate(512, None).is_err());
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
    let submitted = f.store.writer.segments_for_test();
    let mut snapshot = submitted.snapshot().unwrap();
    snapshot[0].state = segment::SegmentState::Sealed;
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
fn unreclaimable_segment_pressure_discards_copies_without_fatal_progress_error() {
    let f = Fixture::new();
    futures::executor::block_on(f.store.open()).unwrap();
    f.reactor.init().unwrap();
    f.enqueue(f.copy(1, 64)).unwrap();
    f.enqueue(f.copy(2, 64)).unwrap();
    let segments = f.store.writer.segments_for_test();
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
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
    assert!(segments.recycle(segment::SegmentId(0)).is_err());
    drop((first, second));
    segments.recycle(segment::SegmentId(0)).unwrap();
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
