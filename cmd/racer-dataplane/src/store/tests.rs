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
    let deadline = Instant::now() + Duration::from_secs(10);
    while f.reactor.in_flight() != 0 {
        f.reactor.poll_budgeted(32).unwrap();
        assert!(Instant::now() < deadline);
    }
    assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
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
    assert!(matches!(
        drive(&f.reactor, f.store.writer.progress(1, &request)),
        Err(Error::Cancelled)
    ));
    // A pre-submission cancellation preserves the bounded queue for maintenance retry.
    assert_eq!(f.store.writer.pending_count(), 1);
    f.store
        .writer
        .retire_key(&CacheId("cache".into()), KeyId([1; 16]))
        .unwrap();
    assert_eq!(f.admission.used(ResourceClass::DirtyCiphertext), 0);
}
