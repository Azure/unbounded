use super::*;
use crate::store::{checkpoint::Checkpointer, index::Index, recovery::Recovery};
use futures::executor::block_on;
use std::{
    fs,
    path::PathBuf,
    rc::Rc,
    sync::atomic::{AtomicU64, Ordering},
};

const SEGMENT_BYTES: u64 = 4 * 1024 * 1024;

#[test]
fn async_retirement_rejects_frozen_cut_and_canceled_submission() {
    use crate::runtime::{admission::Admission, deadline::RequestScope, reactor::Reactor};
    use std::time::{Duration, Instant};
    let directory = Directory::new();
    let (index, segments) = state(8);
    let checkpoint = Checkpointer::new(directory.0.clone(), index, segments);
    checkpoint.configure_geometry(geometry()).unwrap();
    let reactor = Rc::new(Reactor::new(Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ))));
    let scope = RequestScope::new(
        crate::model::identity::RequestId([202; 16]),
        Instant::now() + Duration::from_secs(10),
    )
    .unwrap();
    for slot in ["checkpoint.0", "checkpoint.1"] {
        fs::write(directory.0.join(slot), b"retained cut").unwrap();
    }
    block_on(checkpoint.snapshot_shard()).unwrap();
    assert!(matches!(
        checkpoint.invalidate_persisted_async(reactor.clone(), scope.clone()),
        Err(Error::Overloaded)
    ));
    checkpoint.finish_snapshot();
    scope.cancel().unwrap();
    assert_eq!(
        block_on(
            checkpoint
                .invalidate_persisted_async(reactor.clone(), scope)
                .unwrap()
        ),
        Err(Error::Cancelled)
    );
    assert_eq!(reactor.in_flight(), 0);
    for slot in ["checkpoint.0", "checkpoint.1"] {
        assert_eq!(fs::read(directory.0.join(slot)).unwrap(), b"retained cut");
    }
}

#[test]
fn async_retirement_invalidation_fences_both_slots_and_preserves_failure() {
    use crate::runtime::{admission::Admission, deadline::RequestScope, reactor::Reactor};
    use std::{
        task::{Context, Poll},
        time::{Duration, Instant},
    };
    let directory = Directory::new();
    let (index, segments) = state(8);
    let checkpoint = Checkpointer::new(directory.0.clone(), index, segments);
    let reactor = Rc::new(Reactor::new(Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ))));
    reactor.init().unwrap();
    let scope = || {
        RequestScope::new(
            crate::model::identity::RequestId([201; 16]),
            Instant::now() + Duration::from_secs(10),
        )
        .unwrap()
    };
    let drive = |mut operation: crate::error::Operation<'static, ()>| {
        let end = Instant::now() + Duration::from_secs(10);
        loop {
            reactor.poll_budgeted(16).unwrap();
            if let Poll::Ready(result) = operation
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            {
                break result;
            }
            assert!(Instant::now() < end);
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    };
    for slot in ["checkpoint.0", "checkpoint.1"] {
        fs::write(directory.0.join(slot), b"recoverable cut").unwrap();
    }
    let mut abandoned = checkpoint
        .invalidate_persisted_async(reactor.clone(), scope())
        .unwrap();
    assert!(
        abandoned
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending()
    );
    assert!(reactor.in_flight() > 0);
    drop(abandoned);
    let fence_reactor = reactor.clone();
    drive(Box::pin(async move {
        fence_reactor
            .file_fence(crate::model::identity::RequestId([201; 16]))
            .await
    }))
    .unwrap();
    assert_eq!(reactor.in_flight(), 0);
    drive(
        checkpoint
            .invalidate_persisted_async(reactor.clone(), scope())
            .unwrap(),
    )
    .unwrap();
    assert!(!directory.0.join("checkpoint.0").exists());
    assert!(!directory.0.join("checkpoint.1").exists());
    drive(
        checkpoint
            .invalidate_persisted_async(reactor.clone(), scope())
            .unwrap(),
    )
    .unwrap();
    fs::create_dir(directory.0.join("checkpoint.1")).unwrap();
    assert_eq!(
        drive(
            checkpoint
                .invalidate_persisted_async(reactor.clone(), scope())
                .unwrap()
        ),
        Err(Error::Io)
    );
    assert_eq!(reactor.in_flight(), 0);
}
static DIRECTORY_SEQUENCE: AtomicU64 = AtomicU64::new(0);

struct Directory(PathBuf);
impl Directory {
    fn new() -> Self {
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!(
                "checkpoint-test-{}-{}",
                std::process::id(),
                DIRECTORY_SEQUENCE.fetch_add(1, Ordering::Relaxed)
            ));
        fs::create_dir_all(&path).unwrap();
        Self(path)
    }
}
impl Drop for Directory {
    fn drop(&mut self) {
        fs::remove_dir_all(&self.0).unwrap();
    }
}

fn geometry() -> CheckpointGeometry {
    CheckpointGeometry::new(
        SEGMENT_BYTES * 2,
        SEGMENT_BYTES,
        2,
        DirectAlignment::validate(4096, 4096, 4096).unwrap(),
    )
    .unwrap()
}

fn descriptor(etag: &str, length: u64) -> VersionMetadata {
    VersionMetadata {
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([7; 32]),
            },
            etag: StrongEtag::parse(format!("\"{etag}\"").as_bytes()).unwrap(),
        },
        length,
    }
}

fn state(capacity: usize) -> (Rc<Index>, Rc<Segments>) {
    let g = geometry();
    let index = Rc::new(Index::new(WorkerId(0), capacity));
    let segments = Rc::new(Segments::new(WorkerId(0), g.segment_bytes));
    segments
        .configure(
            g.slab_bytes,
            g.segment_count as usize,
            g.alignment().unwrap(),
        )
        .unwrap();
    (index, segments)
}

fn shard() -> ShardImage {
    let (index, segments) = state(8);
    let metadata = descriptor("v1", 17);
    let page = PageId {
        version: metadata.version.clone(),
        number: PageNumber(0),
    };
    let lease = segments.append(4096).unwrap();
    let allocation = segments
        .snapshot()
        .unwrap()
        .into_iter()
        .find(|segment| matches!(segment.state, SegmentState::Open))
        .unwrap();
    index
        .publish(
            page,
            IndexedPage {
                location: RecordLocation {
                    segment: allocation.id,
                    generation: allocation.generation,
                    location: lease.location,
                },
                metadata,
                key_id: KeyId([9; 16]),
            },
        )
        .unwrap();
    drop(lease);
    index.publish_version(descriptor("empty", 0)).unwrap();
    ShardImage {
        worker: WorkerId(0),
        geometry: geometry(),
        index: index.snapshot().unwrap(),
        segments: segments.snapshot().unwrap(),
    }
}

fn image(sequence: u64) -> CheckpointImage {
    CheckpointImage {
        version: CHECKPOINT_VERSION,
        sequence,
        shards: vec![shard()],
    }
}

fn resign(bytes: &mut [u8]) {
    let end = bytes.len() - DIGEST_BYTES;
    let digest = Sha256::digest(&bytes[..end]);
    bytes[end..].copy_from_slice(&digest);
}

#[test]
fn binary_round_trip_retains_locations_keys_metadata_and_is_send() {
    fn assert_send<T: Send>() {}
    assert_send::<ShardImage>();
    let encoded = CheckpointCodec.encode(&image(7)).unwrap();
    let decoded = CheckpointCodec.decode(&encoded).unwrap();
    assert_eq!(decoded.sequence, 7);
    assert_eq!(&encoded[..8], b"RACERCP\0");
    assert_eq!(&encoded[8..12], &[1, 0, 0, 0]);
    let shard = &decoded.shards[0];
    assert_eq!(shard.geometry, geometry());
    let (_, entry) = &shard.index.entries[0];
    assert_eq!(entry.metadata, descriptor("v1", 17));
    assert_eq!(entry.key_id, KeyId([9; 16]));
    assert_eq!(entry.location.location.extent.length(), 4096);
    assert!(
        shard
            .index
            .metadata
            .iter()
            .any(|metadata| metadata == &descriptor("empty", 0))
    );
    assert_eq!(CheckpointCodec.encode(&decoded).unwrap(), encoded);
}

#[test]
fn empty_cut_has_stable_version_one_binary_vector() {
    let (index, segments) = state(8);
    let image = CheckpointImage {
        version: CHECKPOINT_VERSION,
        sequence: 1,
        shards: vec![ShardImage {
            worker: WorkerId(0),
            geometry: geometry(),
            index: index.snapshot().unwrap(),
            segments: segments.snapshot().unwrap(),
        }],
    };
    let bytes = CheckpointCodec.encode(&image).unwrap();
    assert_eq!(bytes.len(), 180);
    let digest: String = bytes[148..]
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect();
    assert_eq!(
        digest,
        "357123d5dbff2ad5724ac0ecfe16c26edd27dbed03833f1e7bc99698abc32442"
    );
}

#[test]
fn metadata_order_does_not_affect_encoding_and_cross_shard_conflicts_fail() {
    let mut image = image(1);
    image.shards[0]
        .index
        .metadata
        .push(descriptor("another", 1));
    let first = CheckpointCodec.encode(&image).unwrap();
    image.shards[0].index.metadata.reverse();
    assert_eq!(CheckpointCodec.encode(&image).unwrap(), first);
    let mut other = shard();
    other.worker = WorkerId(1);
    other.index.entries.clear();
    other.index.metadata = vec![descriptor("v1", 18)];
    image.shards.push(other);
    assert!(CheckpointCodec.encode(&image).is_err());
}

#[test]
fn newer_incompatible_geometry_or_catalog_falls_back_and_keys_filter_on_load() {
    let directory = Directory::new();
    fs::write(
        directory.0.join("checkpoint.0"),
        CheckpointCodec.encode(&image(1)).unwrap(),
    )
    .unwrap();
    let mut newer = image(2);
    newer.shards[0].geometry.memory_alignment = 8192;
    fs::write(
        directory.0.join("checkpoint.1"),
        CheckpointCodec.encode(&newer).unwrap(),
    )
    .unwrap();
    let (index, segments) = state(1);
    let recovery = Recovery::new(directory.0.clone(), index, segments);
    recovery.configure_geometry(geometry()).unwrap();
    let recovered = block_on(recovery.load_with_keys(geometry().alignment().unwrap(), Some(&[])))
        .unwrap()
        .unwrap();
    assert_eq!(recovered.sequence, 1);
    assert!(recovered.shards[0].index.entries.is_empty());
    assert_eq!(recovered.shards[0].index.metadata.len(), 1);
    newer.shards[0].geometry = geometry();
    newer.shards[0].index.metadata.push(descriptor("extra", 0));
    fs::write(
        directory.0.join("checkpoint.1"),
        CheckpointCodec.encode(&newer).unwrap(),
    )
    .unwrap();
    assert_eq!(
        block_on(recovery.load(geometry().alignment().unwrap()))
            .unwrap()
            .unwrap()
            .sequence,
        1
    );
}

#[test]
fn oversized_sparse_files_and_symlinks_are_rejected_without_payload_reads() {
    use std::os::unix::fs::symlink;
    let directory = Directory::new();
    let file = fs::File::create(directory.0.join("checkpoint.0")).unwrap();
    file.set_len(MAX_CHECKPOINT_BYTES as u64 + 1).unwrap();
    fs::write(
        directory.0.join("payload"),
        CheckpointCodec.encode(&image(3)).unwrap(),
    )
    .unwrap();
    symlink(
        directory.0.join("payload"),
        directory.0.join("checkpoint.1"),
    )
    .unwrap();
    let (index, segments) = state(8);
    let recovery = Recovery::new(directory.0.clone(), index, segments);
    assert!(
        block_on(recovery.load(geometry().alignment().unwrap()))
            .unwrap()
            .is_none()
    );
}

#[test]
fn malformed_hash_version_length_counts_and_trailing_bytes_are_rejected() {
    let encoded = CheckpointCodec.encode(&image(1)).unwrap();
    for cut in [0, 7, 31, encoded.len() - 1] {
        assert!(CheckpointCodec.decode(&encoded[..cut]).is_err());
    }
    let mut corrupt = encoded.clone();
    corrupt[20] ^= 1;
    assert!(CheckpointCodec.decode(&corrupt).is_err());
    for (offset, value) in [(8, 2u32), (12, 1), (32, u32::MAX)] {
        let mut corrupt = encoded.clone();
        corrupt[offset..offset + 4].copy_from_slice(&value.to_le_bytes());
        resign(&mut corrupt);
        assert!(CheckpointCodec.decode(&corrupt).is_err());
    }
    let mut corrupt = encoded.clone();
    corrupt[24..32].copy_from_slice(&1u64.to_le_bytes());
    resign(&mut corrupt);
    assert!(CheckpointCodec.decode(&corrupt).is_err());
    let mut corrupt = encoded;
    corrupt.push(0);
    assert!(CheckpointCodec.decode(&corrupt).is_err());
}

#[test]
fn invalid_generation_bounds_duplicates_and_descriptor_conflicts_are_rejected() {
    let mut bad = image(1);
    bad.shards[0].index.entries[0].1.location.generation.0 += 1;
    assert!(CheckpointCodec.encode(&bad).is_err());
    let mut bad = image(1);
    bad.shards[0].index.entries[0].1.location.location.extent =
        DirectExtent::checked(SEGMENT_BYTES, 4096).unwrap();
    assert!(CheckpointCodec.encode(&bad).is_err());
    let mut bad = image(1);
    let duplicate = bad.shards[0].index.entries[0].clone();
    bad.shards[0].index.entries.push(duplicate);
    assert!(CheckpointCodec.encode(&bad).is_err());
    let mut bad = image(1);
    bad.shards[0].index.metadata.push(descriptor("v1", 18));
    assert!(CheckpointCodec.encode(&bad).is_err());
}

#[test]
fn overlapping_mappings_are_rejected_and_retirement_invalidates_both_slots() {
    let mut bad = image(1);
    let mut overlapping = bad.shards[0].index.entries[0].clone();
    overlapping.0.version.object.key = CacheKey([8; 32]);
    overlapping.1.metadata.version = overlapping.0.version.clone();
    bad.shards[0].index.entries.push(overlapping);
    assert!(CheckpointCodec.encode(&bad).is_err());
    let directory = Directory::new();
    let (index, segments) = state(8);
    let checkpointer = Checkpointer::new(directory.0.clone(), index, segments);
    block_on(checkpointer.publish(vec![shard()])).unwrap();
    block_on(checkpointer.publish(vec![shard()])).unwrap();
    checkpointer.invalidate_persisted().unwrap();
    assert!(!directory.0.join("checkpoint.0").exists());
    assert!(!directory.0.join("checkpoint.1").exists());
    checkpointer.invalidate_persisted().unwrap();
}

#[test]
fn alternating_publication_falls_back_to_valid_older_cut_and_ignores_temp_and_payload() {
    let directory = Directory::new();
    let (index, segments) = state(8);
    let checkpointer = Checkpointer::new(directory.0.clone(), index.clone(), segments.clone());
    let recovery = Recovery::new(directory.0.clone(), index, segments);
    recovery.configure_geometry(geometry()).unwrap();
    assert!(
        block_on(recovery.load(geometry().alignment().unwrap()))
            .unwrap()
            .is_none()
    );
    block_on(checkpointer.publish(vec![shard()])).unwrap();
    block_on(checkpointer.publish(vec![shard()])).unwrap();
    let load = || {
        block_on(recovery.load(geometry().alignment().unwrap()))
            .unwrap()
            .unwrap()
    };
    assert_eq!(load().sequence, 2);
    fs::write(directory.0.join("checkpoint.1"), b"torn").unwrap();
    fs::write(
        directory.0.join(".checkpoint.tmp"),
        CheckpointCodec.encode(&image(99)).unwrap(),
    )
    .unwrap();
    fs::write(directory.0.join("slab.0"), b"payload must not be scanned").unwrap();
    assert_eq!(load().sequence, 1);
    block_on(checkpointer.publish(vec![shard()])).unwrap();
    assert_eq!(load().sequence, 2);
    assert_eq!(
        fs::read(directory.0.join("slab.0")).unwrap(),
        b"payload must not be scanned"
    );
    fs::write(directory.0.join("checkpoint.0"), b"bad").unwrap();
    fs::write(directory.0.join("checkpoint.1"), b"bad").unwrap();
    assert!(
        block_on(recovery.load(geometry().alignment().unwrap()))
            .unwrap()
            .is_none()
    );
}

#[test]
fn recovery_validates_before_mutation_seals_segments_and_filters_missing_keys() {
    let directory = Directory::new();
    let (index, segments) = state(8);
    let keep = descriptor("keep", 0);
    index
        .publish_current(crate::model::metadata::ObjectMetadata {
            version: keep.version.clone(),
            length: keep.length,
            expires_at: crate::model::metadata::ExpiresAt(
                std::time::SystemTime::now() + std::time::Duration::from_secs(300),
            ),
        })
        .unwrap();
    assert!(index.current(&keep.version.object).unwrap().is_some());
    let recovery = Recovery::new(directory.0.clone(), index.clone(), segments.clone());
    recovery.configure_geometry(geometry()).unwrap();
    let before = segments.snapshot().unwrap();
    let mut bad = shard();
    bad.index.entries[0].1.location.generation.0 += 1;
    assert!(block_on(recovery.install_shard(Some(bad))).is_err());
    assert!(
        index
            .version(&descriptor("keep", 0).version)
            .unwrap()
            .is_some()
    );
    assert_eq!(
        segments.snapshot().unwrap()[0].used_bytes,
        before[0].used_bytes
    );
    let recovered = shard();
    let page = recovered.index.entries[0].0.clone();
    block_on(recovery.install_shard_with_keys(Some(recovered), Some(&[]))).unwrap();
    assert!(index.lookup(&page).unwrap().is_none());
    assert!(
        index
            .version(&descriptor("empty", 0).version)
            .unwrap()
            .is_some()
    );
    assert!(
        segments
            .snapshot()
            .unwrap()
            .iter()
            .all(|segment| !matches!(segment.state, SegmentState::Open))
    );
    assert!(index.current(&page.version.object).unwrap().is_none());
    block_on(recovery.install_shard(Some(shard()))).unwrap();
    assert!(index.lookup(&page).unwrap().is_some());
    block_on(recovery.install_shard(None)).unwrap();
    assert!(index.lookup(&page).unwrap().is_none());
    assert!(
        index
            .version(&descriptor("empty", 0).version)
            .unwrap()
            .is_none()
    );
}

#[test]
fn catalog_capacity_failure_preserves_all_live_state() {
    let directory = Directory::new();
    let (index, segments) = state(1);
    index.publish_version(descriptor("keep", 0)).unwrap();
    let recovery = Recovery::new(directory.0.clone(), index.clone(), segments.clone());
    recovery.configure_geometry(geometry()).unwrap();
    let mut recovered = shard();
    recovered.index.metadata = vec![descriptor("one", 0), descriptor("two", 0)];
    assert!(block_on(recovery.install_shard(Some(recovered))).is_err());
    assert!(
        index
            .version(&descriptor("keep", 0).version)
            .unwrap()
            .is_some()
    );
    assert!(
        segments
            .snapshot()
            .unwrap()
            .iter()
            .all(|segment| segment.used_bytes == 0)
    );
}

#[test]
fn snapshot_freeze_requires_explicit_owner_release() {
    let directory = Directory::new();
    let (index, segments) = state(8);
    let checkpointer = Checkpointer::new(directory.0.join("not-created"), index, segments.clone());
    assert!(!directory.0.join("not-created").exists());
    checkpointer.configure_geometry(geometry()).unwrap();
    let snapshot = block_on(checkpointer.snapshot_shard()).unwrap();
    assert!(segments.append(4096).is_err());
    assert!(block_on(checkpointer.snapshot_shard()).is_err());
    drop(snapshot);
    assert!(segments.append(4096).is_err());
    checkpointer.finish_snapshot();
    checkpointer.finish_snapshot();
    assert!(segments.append(4096).is_ok());
}

#[test]
fn failed_snapshot_releases_owner_and_failed_publication_preserves_prior_cut() {
    let directory = Directory::new();
    let (index, segments) = state(8);
    let checkpointer = Checkpointer::new(directory.0.clone(), index.clone(), segments.clone());
    checkpointer.configure_geometry(geometry()).unwrap();
    let invalid = shard().index.entries.pop().unwrap();
    index.publish(invalid.0, invalid.1).unwrap();
    // The index references a generation without an allocated record in this table.
    assert!(block_on(checkpointer.snapshot_shard()).is_err());
    assert!(segments.append(4096).is_ok());

    block_on(checkpointer.publish(vec![shard()])).unwrap();
    // Force rename to fail while leaving the previously published slot readable.
    fs::create_dir(directory.0.join("checkpoint.1")).unwrap();
    assert!(block_on(checkpointer.publish(vec![shard()])).is_err());
    let recovery = Recovery::new(directory.0.clone(), index, segments);
    let recovered = block_on(recovery.load(geometry().alignment().unwrap()))
        .unwrap()
        .unwrap();
    assert_eq!(recovered.sequence, 1);
    assert!(fs::read_dir(&directory.0).unwrap().all(|entry| {
        !entry
            .unwrap()
            .file_name()
            .to_string_lossy()
            .ends_with(".tmp")
    }));
}

#[test]
fn outstanding_lease_and_wrong_worker_cannot_partially_install() {
    let directory = Directory::new();
    let (index, segments) = state(8);
    index.publish_version(descriptor("keep", 0)).unwrap();
    let recovery = Recovery::new(directory.0.clone(), index.clone(), segments.clone());
    recovery.configure_geometry(geometry()).unwrap();
    let lease = segments.append(4096).unwrap();
    assert!(block_on(recovery.install_shard(Some(shard()))).is_err());
    assert!(
        index
            .version(&descriptor("keep", 0).version)
            .unwrap()
            .is_some()
    );
    drop(lease);
    let mut wrong = shard();
    wrong.worker = WorkerId(1);
    assert!(block_on(recovery.install_shard(Some(wrong))).is_err());
    assert!(
        index
            .version(&descriptor("keep", 0).version)
            .unwrap()
            .is_some()
    );
}
