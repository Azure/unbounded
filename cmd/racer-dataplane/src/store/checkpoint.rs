//! Consistent logical cuts and alternating two-generation checkpoint publication.
//!
//! The coordinator keeps every owner frozen through publication, then explicitly
//! finishes each snapshot even on failure. Write/rename provides no fsync durability.

use super::Decoder;
use super::catalog::Index;
use super::catalog::IndexSnapshot;
use super::catalog::IndexedPage;
use super::catalog::RecordLocation;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::CacheKey;
use crate::model::ObjectId;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::model::StrongEtag;
use crate::model::VersionMetadata;
use crate::model::WorkerId;
use crate::runtime::HashMap;
use crate::runtime::HashSet;
use page_alloc::Alignment;
use page_alloc::Extent;
use page_alloc::FreezeGuard;
use page_alloc::Generation;
use page_alloc::SegmentId;
use page_alloc::SegmentSnapshot;
use page_alloc::SegmentState;
use page_alloc::Segments;
use racer_control_wire::CacheId;
use racer_control_wire::KeyId;
use sha2::Digest;
use sha2::Sha256;
use std::cell::Cell;
use std::cell::RefCell;
use std::path::Path;
use std::path::PathBuf;
use std::rc::Rc;
use uring_runtime::drivers::yield_now;

pub(crate) const CHECKPOINT_NAMES: [&str; 2] = ["checkpoint.0", "checkpoint.1"];
const SLOTS: uring_runtime::reactor::filesystem::checkpoint::Slots<HEADER_BYTES> =
    uring_runtime::reactor::filesystem::checkpoint::Slots {
        names: CHECKPOINT_NAMES,
        minimum_bytes: HEADER_BYTES + 4 + DIGEST_BYTES,
        maximum_bytes: MAX_CHECKPOINT_BYTES,
    };

pub struct Checkpointer {
    layout_digest: [u8; 32],
    directory: PathBuf,
    index: Rc<Index>,
    segments: Rc<Segments>,
    geometry: Cell<Option<CheckpointGeometry>>,
    frozen: RefCell<Option<FreezeGuard>>,
    writer: RefCell<std::rc::Weak<super::StoreWriter>>,
    paused: RefCell<Option<super::WriterPause>>,
}

impl Checkpointer {
    pub fn new(directory: PathBuf, index: Rc<Index>, segments: Rc<Segments>) -> Self {
        Self {
            layout_digest: FILE_LAYOUT_DIGEST,
            directory,
            index,
            segments,
            geometry: Cell::new(None),
            frozen: RefCell::new(None),
            writer: RefCell::new(std::rc::Weak::new()),
            paused: RefCell::new(None),
        }
    }

    pub(super) fn bind_writer(&self, writer: &Rc<super::StoreWriter>) {
        *self.writer.borrow_mut() = Rc::downgrade(writer);
    }

    async fn pause_writer(&self) -> Result<Option<super::WriterPause>> {
        let writer = self.writer.borrow().upgrade();
        match writer {
            Some(writer) => writer.pause_for_checkpoint().await.map(Some),
            None => Ok(None),
        }
    }

    /// Bind this worker to its startup storage layout.
    pub fn with_layout(mut self, digest: [u8; 32]) -> Self {
        self.layout_digest = digest;
        self
    }

    pub fn geometry(&self) -> Result<CheckpointGeometry> {
        self.geometry.get().ok_or(Error::InvalidConfiguration)
    }

    /// Create and validate checkpoint storage even when payloads use raw devices.
    pub async fn prepare_directory(
        &self,
        reactor: &crate::runtime::Reactor,
        scope: &crate::runtime::RequestScope,
    ) -> Result<()> {
        reactor
            .file_directory(&self.directory, true, 4096, scope)
            .await?;
        Ok(())
    }

    /// Call after slab opening discovers actual geometry, before taking snapshots.
    pub fn configure_geometry(&self, mut geometry: CheckpointGeometry) -> Result<()> {
        geometry.layout_digest = self.layout_digest;
        geometry.validate_live(&self.segments)?;
        if self.frozen.borrow().is_some() {
            return Err(Error::Overloaded);
        }
        self.geometry.set(Some(geometry));
        Ok(())
    }

    pub fn snapshot_shard(&self) -> Operation<'_, ShardImage> {
        Box::pin(async move {
            let geometry = self.geometry.get().ok_or(Error::InvalidConfiguration)?;
            if self.frozen.borrow().is_some() {
                return Err(Error::Overloaded);
            }
            let paused = self.pause_writer().await?;
            let frozen = self.segments.freeze()?;
            let snapshot = (|| {
                let mut segments = self.segments.snapshot();
                let mut remaining = MAX_CHECKPOINT_BYTES;
                self.index.snapshot_holds(&mut segments, &mut remaining)?;
                let shard = ShardImage {
                    worker: self.index.worker(),
                    geometry,
                    index: self.index.snapshot()?,
                    segments,
                };
                shard.validate()?;
                Ok(shard)
            })();
            if snapshot.is_ok() {
                *self.frozen.borrow_mut() = Some(frozen);
                *self.paused.borrow_mut() = paused;
            }
            snapshot
        })
    }

    /// Copy at most 128 retained mappings per reactor turn and reject budget
    /// pressure before retaining the next batch. Frozen segment reuse makes
    /// concurrent invalidations safe; no payload is read or fsynced.
    pub fn snapshot_incremental(self: &Rc<Self>, budget: usize) -> Operation<'static, ShardImage> {
        let owner = self.clone();
        Box::pin(async move {
            let geometry = owner.geometry.get().ok_or(Error::InvalidConfiguration)?;
            if owner.frozen.borrow().is_some() {
                return Err(Error::Overloaded);
            }
            let segment_bytes = snapshot_base_bytes(owner.segments.count())?;
            let mut remaining = budget.checked_sub(segment_bytes).ok_or(Error::Overloaded)?;
            // Inspect borrowed index state before draining writes. Segments owns no
            // live hold records; Index adds those deadlines to the snapshot.
            owner.index.preflight_snapshot(&mut remaining)?;
            remaining = budget - segment_bytes;
            // Keep ownership in the future until success so cancellation also thaws.
            let paused = owner.pause_writer().await?;
            let frozen = owner.segments.freeze()?;
            let result = async {
                // Recheck while copying: draining writes can publish pages, and
                // holds can be added during traversal yields.
                let metadata = owner.index.snapshot_metadata_budgeted(&mut remaining)?;
                let mut segments = owner.segments.snapshot();
                let mut entries = Vec::new();
                let mut cursor = 0;
                loop {
                    let (next, batch) =
                        owner
                            .index
                            .snapshot_pages_budgeted(cursor, 128, &mut remaining)?;
                    let finished = batch.len() < 128;
                    for (page, entry) in batch {
                        entries.push((page, entry));
                    }
                    if finished {
                        break;
                    }
                    cursor = next;
                    yield_now().await;
                }
                // Publications are fenced. Capture deadlines after traversal so
                // holds acquired or extended during its yields are included.
                owner.index.snapshot_holds(&mut segments, &mut remaining)?;
                let mut shard = ShardImage {
                    worker: owner.index.worker(),
                    geometry,
                    index: super::catalog::IndexSnapshot { entries, metadata },
                    segments,
                };
                shard.prune_holds();
                Ok(shard)
            }
            .await;
            if result.is_ok() {
                *owner.frozen.borrow_mut() = Some(frozen);
                *owner.paused.borrow_mut() = paused;
            }
            result
        })
    }

    /// Required on every owning worker after success, failure, or coordinator abort.
    /// Images are Send values, deliberately containing no worker-local lease/Rc.
    pub fn finish_snapshot(&self) {
        self.frozen.borrow_mut().take();
        let paused = self.paused.borrow_mut().take();
        drop(paused);
    }

    /// One coordinator calls this after all owner-worker snapshots have succeeded.
    /// The coordinator must keep all owners frozen until this operation completes.
    pub fn publish(&self, shards: Vec<ShardImage>) -> Operation<'_, ()> {
        Box::pin(async move {
            let newest = candidates(&self.directory, MAX_CHECKPOINT_BYTES)?
                .next()
                .map(|(slot, image)| (slot, image.sequence));
            // Replace the slot opposite the newest valid image, including after a
            // torn newer publication. The surviving valid generation stays intact.
            let (slot, sequence) = SLOTS.next_publication(newest).ok_or(Error::Unavailable)?;
            let bytes = encode(&CheckpointImage {
                version: CHECKPOINT_VERSION,
                sequence,
                shards,
            })?;
            publish_bytes(&self.directory, slot, &bytes)
        })
    }

    /// Periodic disposable hints. The coordinator serializes generations and
    /// keeps segment reuse frozen until rename completes. No fsync is issued.
    pub fn publish_async(
        &self,
        shards: Vec<ShardImage>,
        reactor: Rc<crate::runtime::Reactor>,
        scope: crate::runtime::RequestScope,
        sequence: u64,
        slot: usize,
        budget: usize,
    ) -> Result<Operation<'static, ()>> {
        // Bound simultaneous decoded image, encoding and submission scratch.
        let estimated = snapshot_image_bytes(&shards)?;
        if estimated > budget / 2 {
            return Err(Error::Overloaded);
        }
        let directory = self.directory.clone();
        Ok(Box::pin(async move {
            let bytes = encode_incremental(&CheckpointImage {
                version: CHECKPOINT_VERSION,
                sequence,
                shards,
            })
            .await?;
            if bytes.len() > budget / 2 {
                return Err(Error::Overloaded);
            }
            SLOTS
                .publish_async(
                    &reactor,
                    &directory,
                    slot,
                    &bytes,
                    std::num::NonZeroUsize::new(16384).unwrap(),
                    &scope,
                )
                .await
                .map_err(Into::into)
        }))
    }
}

impl Drop for Checkpointer {
    fn drop(&mut self) {
        self.finish_snapshot();
    }
}

pub(super) fn snapshot_entry_bytes(metadata: &VersionMetadata) -> usize {
    PAGE_ENTRY_BYTES + descriptor_string_bytes(metadata)
}

pub(super) fn snapshot_metadata_bytes(metadata: &VersionMetadata) -> usize {
    DESCRIPTOR_BYTES + descriptor_string_bytes(metadata)
}

fn descriptor_string_bytes(metadata: &VersionMetadata) -> usize {
    // Encoder::descriptor writes each string once. Its three u32 length
    // prefixes are already included in DESCRIPTOR_BYTES and PAGE_ENTRY_BYTES.
    metadata.version.object.cache.0.len()
        + metadata.version.etag.as_bytes().len()
        + metadata
            .content_type
            .as_ref()
            .map_or(0, |mime| mime.as_bytes().len())
}

fn snapshot_segment_bytes(records: usize) -> Result<usize> {
    records
        .checked_mul(RETAINED_RECORD_BYTES)
        .and_then(|n| n.checked_add(SEGMENT_BYTES))
        .ok_or(Error::Overloaded)
}

pub(super) fn snapshot_base_bytes(segments: usize) -> Result<usize> {
    segments
        .checked_mul(snapshot_segment_bytes(0)?)
        .and_then(|n| n.checked_add(SHARD_BYTES))
        .ok_or(Error::Overloaded)
}

fn snapshot_image_bytes(shards: &[ShardImage]) -> Result<usize> {
    let mut total = HEADER_BYTES + 4 + DIGEST_BYTES;
    for shard in shards {
        total = total.checked_add(SHARD_BYTES).ok_or(Error::Overloaded)?;
        for segment in &shard.segments {
            total = total
                .checked_add(snapshot_segment_bytes(segment.retained_records.len())?)
                .ok_or(Error::Overloaded)?;
        }
        for bytes in shard
            .index
            .entries
            .iter()
            .map(|(_, entry)| snapshot_entry_bytes(&entry.metadata))
            .chain(shard.index.metadata.iter().map(snapshot_metadata_bytes))
        {
            total = total.checked_add(bytes).ok_or(Error::Overloaded)?;
        }
    }
    Ok(total)
}

fn publish_bytes(directory: &Path, slot: usize, bytes: &[u8]) -> Result<()> {
    SLOTS.publish(directory, slot, bytes).map_err(|_| Error::Io)
}

/// Newest-valid checkpoint selection with empty-cache fallback and no payload scan.
/// Recovery seals open segments and clears freshness. Checkpoints have no fsync
/// guarantee; record validation and AEAD convert stale payload references to misses.
pub struct Recovery {
    #[cfg(test)]
    directory: PathBuf,
    index: Rc<Index>,
    segments: Rc<Segments>,
    geometry: Cell<Option<CheckpointGeometry>>,
}

impl Recovery {
    /// Filter disposable checkpoint state before installation. Standalone metadata
    /// has no key ID, so it requires both a current cache UID and an active page key.
    pub fn filter_available(
        image: &mut CheckpointImage,
        mut metadata_available: impl FnMut(&racer_control_wire::CacheId) -> bool,
        mut available: impl FnMut(&racer_control_wire::CacheId, KeyId) -> bool,
    ) {
        for shard in &mut image.shards {
            shard
                .index
                .entries
                .retain(|(page, entry)| available(&page.version.object.cache, entry.key_id));
            shard
                .index
                .metadata
                .retain(|m| metadata_available(&m.version.object.cache));
            shard.prune_holds();
        }
    }
    pub fn new(_directory: PathBuf, index: Rc<Index>, segments: Rc<Segments>) -> Self {
        Self {
            #[cfg(test)]
            directory: _directory,
            index,
            segments,
            geometry: Cell::new(None),
        }
    }

    /// Configure actual opened slab geometry before installation. No I/O is done.
    pub fn configure_geometry(&self, geometry: CheckpointGeometry) -> Result<()> {
        geometry.validate_live(&self.segments)?;
        self.geometry.set(Some(geometry));
        Ok(())
    }

    /// Before admission, install one validated cut into this worker. None resets
    /// its index and allocation table; no slab is scanned or zeroed.
    pub fn install_shard(&self, image: Option<ShardImage>) -> Operation<'_, ()> {
        Box::pin(async move {
            let geometry = self.geometry.get().ok_or(Error::InvalidConfiguration)?;
            let image = match image {
                Some(image) => image,
                None => {
                    let empty = Segments::from_geometry(geometry.allocation_geometry()?)?;
                    ShardImage {
                        worker: self.index.worker(),
                        geometry,
                        index: IndexSnapshot {
                            entries: vec![],
                            metadata: vec![],
                        },
                        segments: empty.snapshot(),
                    }
                }
            };
            if image.worker != self.index.worker() || image.geometry != geometry {
                return Err(Error::CorruptRecord);
            }
            image.validate()?;
            self.segments.validate_restore(&image.segments)?;
            self.index.validate_snapshot(&image.index)?;
            self.index
                .validate_restored_holds(&image.index, &image.segments)?;
            // Keep both preflights even though restore validates again: a frozen or
            // leased table must reject the cut before the index is mutated.
            // Index::restore checks its own standalone metadata capacity before
            // mutation. There is no await between validation and these installs.
            // The coordinator must fence admission for the complete operation.
            self.index.restore(image.index)?;
            let deadlines = image.segments.clone();
            self.segments.restore(image.segments)?;
            self.index.restore_holds(&deadlines)?;
            Ok(())
        })
    }
}

/// Probe only fixed-size headers, then decode one slot at a time, newest first.
/// Callers must discard a rejected image before advancing. An inaccessible storage
/// directory is fatal; individual checkpoint files are disposable hints. Slab
/// opening/validation remains independently fatal during Store::open.
pub(crate) fn candidates(directory: &Path, budget: usize) -> Result<Candidates> {
    SLOTS
        .candidates(
            directory,
            budget,
            |header| sequence_hint(header).map_err(std::io::Error::other),
            |bytes, budget| decode_with_budget(bytes, budget).map_err(std::io::Error::other),
        )
        .map_err(|e| {
            eprintln!("racer: checkpoint storage directory unavailable: {e}");
            Error::Io
        })
}

pub(crate) type Candidates =
    uring_runtime::reactor::filesystem::checkpoint::Candidates<CheckpointImage, HEADER_BYTES>;

#[cfg(test)]
pub(crate) fn read_candidates(directory: &Path) -> Result<Vec<(usize, CheckpointImage)>> {
    Ok(candidates(directory, MAX_CHECKPOINT_BYTES)?.collect())
}

// Bounded, versioned binary checkpoints. SHA-256 covers the header and body.
// Version 3 uses little-endian integers: magic[8], version:u32, flags:u32,
// sequence:u64, total_bytes:u64, shard_count:u32, then shards and digest[32].
// Each shard is worker:u16, six geometry u64s, layout_digest[32], counted segments, counted page
// entries, and counted standalone descriptors. Counts and string lengths are
// u32. A descriptor is cache UTF-8 string, key[32], quoted ETag string, length:u64.
// A segment is id:u64, generation:u64, state:u8, used_bytes:u64. A page entry is
// its descriptor, page:u64, key_id[16], segment:u64, generation:u64, slab:u64,
// offset:u64, disk_length:u64. Encoder ordering is canonical by worker, segment,
// and full version/page identity. No freshness or payload bytes are serialized.
// Version 2 appends a counted ASCII MIME string to every descriptor. Zero length
// means absent. Version 3 binds each shard to its raw-device layout or file mode.
// Version 4 adds counted record-offset hold deadlines to each segment.
// Older versions are discarded as disposable cache hints.
pub const CHECKPOINT_VERSION: u32 = 4;
pub const FILE_LAYOUT_DIGEST: [u8; 32] = *b"racer-file-layout-v1\0\0\0\0\0\0\0\0\0\0\0\0";
pub const MAX_CHECKPOINT_BYTES: usize = 64 * 1024 * 1024;
const MAGIC: &[u8; 8] = b"RACERCP\0";
const HEADER_BYTES: usize = 32;
pub(super) const DIGEST_BYTES: usize = 32;
const MAX_SHARDS: usize = 4096;
const MAX_ITEMS: usize = 1_000_000;
const MAX_STRING_BYTES: usize = 8192;

// Shared with decode_with_budget/recovery_memory input.count calls for shards,
// segments, retained records, page entries, and standalone descriptors.
const SHARD_BYTES: usize = 94; // worker + geometry + digest + three u32 counts.
const SEGMENT_BYTES: usize = 29; // id + generation + state + used + record count.
pub(super) const RETAINED_RECORD_BYTES: usize = 20; // offset + seconds + nanos.
const DESCRIPTOR_BYTES: usize = 52; // key + length + three u32 string lengths.
const PAGE_ENTRY_BYTES: usize = DESCRIPTOR_BYTES + 64; // descriptor + page fields.

/// Persist allocation geometry so a valid checksum cannot hide incompatible slabs.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CheckpointGeometry {
    pub layout_digest: [u8; 32],
    pub slab_bytes: u64,
    pub segment_bytes: u64,
    pub segment_count: u64,
    pub memory_alignment: u64,
    pub offset_alignment: u64,
    pub length_alignment: u64,
}

impl From<page_alloc::SegmentGeometry> for CheckpointGeometry {
    fn from(geometry: page_alloc::SegmentGeometry) -> Self {
        let alignment = geometry.alignment();
        Self {
            slab_bytes: geometry.slab_bytes(),
            layout_digest: FILE_LAYOUT_DIGEST,
            segment_bytes: geometry.segment_bytes(),
            segment_count: geometry.segment_count(),
            memory_alignment: alignment.memory() as u64,
            offset_alignment: alignment.offset(),
            length_alignment: alignment.length() as u64,
        }
    }
}

impl CheckpointGeometry {
    /// Full-page capacity at actual direct-I/O alignment, conservatively allowing
    /// the largest supported record header. Short records can use tail space.
    pub fn payload_capacity(&self, reserve: usize, page_entries: usize) -> Result<(u64, u64)> {
        let record = self
            .alignment()?
            .extent(
                0,
                crate::model::PAGE_BYTES as usize + 16 + super::MAX_HEADER_BYTES,
            )?
            .length() as u64;
        let pages = self.segment_bytes / record;
        let usable = self.segment_count.saturating_sub(reserve as u64);
        Ok((
            (usable * pages).min(page_entries as u64) * crate::model::PAGE_BYTES,
            usable * (self.segment_bytes - pages * record),
        ))
    }
    pub fn new(
        slab_bytes: u64,
        segment_bytes: u64,
        segment_count: u64,
        alignment: Alignment,
    ) -> Result<Self> {
        let geometry = Self {
            slab_bytes,
            layout_digest: FILE_LAYOUT_DIGEST,
            segment_bytes,
            segment_count,
            memory_alignment: alignment.memory() as u64,
            offset_alignment: alignment.offset(),
            length_alignment: alignment.length() as u64,
        };
        geometry.validate()?;
        Ok(geometry)
    }
    pub fn alignment(&self) -> Result<Alignment> {
        Alignment::new(
            usize::try_from(self.memory_alignment).map_err(|_| Error::CorruptRecord)?,
            self.offset_alignment,
            usize::try_from(self.length_alignment).map_err(|_| Error::CorruptRecord)?,
        )
        .map_err(|_| Error::CorruptRecord)
    }
    pub fn validate(&self) -> Result<()> {
        self.allocation_geometry()?;
        Ok(())
    }
    fn allocation_geometry(&self) -> Result<page_alloc::SegmentGeometry> {
        let alignment = self.alignment()?;
        if self.segment_count > MAX_ITEMS as u64 {
            return Err(Error::CorruptRecord);
        }
        page_alloc::SegmentGeometry::new(
            self.slab_bytes,
            self.segment_bytes,
            self.segment_count,
            alignment,
        )
        .map_err(|_| Error::CorruptRecord)
    }
    pub fn matches_alignment(&self, alignment: Alignment) -> bool {
        self.memory_alignment == alignment.memory() as u64
            && self.offset_alignment == alignment.offset()
            && self.length_alignment == alignment.length() as u64
    }
    pub(crate) fn validate_live(&self, segments: &Segments) -> Result<()> {
        if segments.geometry() != Some(self.allocation_geometry()?) {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}

pub struct ShardImage {
    pub worker: WorkerId,
    pub geometry: CheckpointGeometry,
    pub index: IndexSnapshot,
    pub segments: Vec<SegmentSnapshot>,
}
impl ShardImage {
    fn prune_holds(&mut self) {
        let offsets: HashSet<_> = self
            .index
            .entries
            .iter()
            .map(|(_, entry)| entry.location.extent.offset())
            .collect();
        for segment in &mut self.segments {
            segment
                .retained_records
                .retain(|(offset, _)| offsets.contains(offset));
        }
    }

    /// Validate against an isolated allocation table, without touching live state.
    pub fn validate(&self) -> Result<()> {
        self.geometry.validate()?;
        self.index.validate_metadata()?;
        if self.segments.len() > MAX_ITEMS
            || self.segments.len() as u64 != self.geometry.segment_count
            || self.index.entries.len() > MAX_ITEMS
            || self.index.metadata.len() > MAX_ITEMS
        {
            return Err(Error::CorruptRecord);
        }
        let segments = Segments::from_geometry(self.geometry.allocation_geometry()?)
            .map_err(|_| Error::CorruptRecord)?;
        // Restoring the isolated table seals open segments, exactly as recovery does.
        segments
            .restore(self.segments.clone())
            .map_err(|_| Error::CorruptRecord)?;
        let mut pages = HashSet::default();
        let mut versions = HashSet::default();
        let mut extents = Vec::with_capacity(self.index.entries.len());
        for metadata in &self.index.metadata {
            validate_descriptor(metadata)?;
            if !versions.insert(&metadata.version) {
                return Err(Error::CorruptRecord);
            }
        }
        for (page, entry) in &self.index.entries {
            validate_descriptor(&entry.metadata)?;
            if !pages.insert(page) {
                return Err(Error::CorruptRecord);
            }
            segments
                .validate(
                    entry.location.segment,
                    entry.location.generation,
                    &entry.location.extent,
                )
                .map_err(|_| Error::CorruptRecord)?;
            let extent = entry.location.extent;
            extents.push((
                extent.offset(),
                extent
                    .offset()
                    .checked_add(extent.length() as u64)
                    .ok_or(Error::CorruptRecord)?,
            ));
        }
        let locations: HashMap<_, _> = self
            .index
            .entries
            .iter()
            .map(|(_, entry)| (entry.location.extent.offset(), entry.location.segment))
            .collect();
        let mut retained = HashSet::default();
        for segment in &self.segments {
            for (offset, until) in &segment.retained_records {
                if !retained.insert(*offset)
                    || until.duration_since(std::time::UNIX_EPOCH).is_err()
                    || locations.get(offset) != Some(&segment.id)
                {
                    return Err(Error::CorruptRecord);
                }
            }
        }
        extents.sort_unstable();
        if extents.windows(2).any(|pair| pair[0].1 > pair[1].0) {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }
}

pub struct CheckpointImage {
    pub version: u32,
    pub sequence: u64,
    pub shards: Vec<ShardImage>,
}

pub fn encode(image: &CheckpointImage) -> Result<Vec<u8>> {
    let mut encode = Box::pin(encode_incremental(image));
    let mut cx = std::task::Context::from_waker(std::task::Waker::noop());
    loop {
        if let std::task::Poll::Ready(result) = std::future::Future::poll(encode.as_mut(), &mut cx)
        {
            return result;
        }
    }
}
pub async fn encode_incremental(image: &CheckpointImage) -> Result<Vec<u8>> {
    validate_image(image)?;
    let mut out = Encoder(Vec::new());
    out.bytes(MAGIC)?;
    out.u32(image.version)?;
    out.u32(0)?; // Reserved flags.
    out.u64(image.sequence)?;
    out.u64(0)?; // Total encoded length, filled before hashing.
    out.count(image.shards.len())?;
    let mut shards: Vec<_> = image.shards.iter().collect();
    shards.sort_by_key(|shard| shard.worker.0);
    for shard in shards {
        out.bytes(&shard.worker.0.to_le_bytes())?;
        let g = shard.geometry;
        for value in [
            g.slab_bytes,
            g.segment_bytes,
            g.segment_count,
            g.memory_alignment,
            g.offset_alignment,
            g.length_alignment,
        ] {
            out.u64(value)?;
        }
        out.bytes(&g.layout_digest)?;
        out.count(shard.segments.len())?;
        let mut segments: Vec<_> = shard.segments.iter().collect();
        segments.sort_by_key(|segment| segment.id.0);
        for (i, segment) in segments.into_iter().enumerate() {
            if i % 128 == 0 {
                yield_now().await;
            }
            out.u64(segment.id.0)?;
            out.u64(segment.generation.0)?;
            out.bytes(&[match segment.state {
                SegmentState::Free => 0,
                SegmentState::Open => 1,
                SegmentState::Sealed => 2,
                SegmentState::Evicting => 3,
            }])?;
            out.u64(segment.used_bytes)?;
            out.count(segment.retained_records.len())?;
            let mut records = segment.retained_records.clone();
            records.sort_by_key(|(offset, _)| *offset);
            for (offset, until) in &records {
                let duration = until
                    .duration_since(std::time::UNIX_EPOCH)
                    .map_err(|_| Error::CorruptRecord)?;
                out.u64(*offset)?;
                out.u64(duration.as_secs())?;
                out.u32(duration.subsec_nanos())?;
            }
        }
        out.count(shard.index.entries.len())?;
        let mut entries: Vec<_> = shard.index.entries.iter().collect();
        entries.sort_by(|(a, _), (b, _)| {
            version_key(&a.version)
                .cmp(&version_key(&b.version))
                .then(a.number.0.cmp(&b.number.0))
        });
        for (i, (page, entry)) in entries.into_iter().enumerate() {
            if i % 128 == 0 {
                yield_now().await;
            }
            out.descriptor(&entry.metadata)?;
            out.u64(page.number.0)?;
            out.bytes(&entry.key_id.0)?;
            out.u64(entry.location.segment.0)?;
            out.u64(entry.location.generation.0)?;
            out.u64(0)?; // Reserved single-slab field preserves checkpoint encoding.
            out.u64(entry.location.extent.offset())?;
            out.u64(entry.location.extent.length() as u64)?;
        }
        out.count(shard.index.metadata.len())?;
        let mut metadata: Vec<_> = shard.index.metadata.iter().collect();
        metadata.sort_by(|a, b| version_key(&a.version).cmp(&version_key(&b.version)));
        for (i, metadata) in metadata.into_iter().enumerate() {
            if i % 128 == 0 {
                yield_now().await;
            }
            out.descriptor(metadata)?;
        }
    }
    let length = out
        .0
        .len()
        .checked_add(DIGEST_BYTES)
        .ok_or(Error::CorruptRecord)?;
    out.0[24..32].copy_from_slice(&(length as u64).to_le_bytes());
    let digest = Sha256::digest(&out.0);
    out.0.extend_from_slice(&digest);
    Ok(out.0)
}

/// Reject truncation, trailing data, unknown versions, and malformed allocations.
/// No payload files are read and no live shard is modified by decoding.
pub fn decode(bytes: &[u8]) -> Result<CheckpointImage> {
    decode_with_budget(bytes, MAX_CHECKPOINT_BYTES)
}

/// The budget includes the encoded buffer, decoded vectors/strings and concurrent
/// validation scratch. Preflight walks borrowed bytes only, before any allocation.
/// A downsized budget rejects the disposable cut rather than partially restoring it.
pub fn decode_with_budget(bytes: &[u8], budget: usize) -> Result<CheckpointImage> {
    recovery_memory(bytes, budget)?;
    if bytes.len() < HEADER_BYTES + 4 + DIGEST_BYTES || bytes.len() > MAX_CHECKPOINT_BYTES {
        return Err(Error::CorruptRecord);
    }
    let (body, digest) = bytes.split_at(bytes.len() - DIGEST_BYTES);
    if &Sha256::digest(body)[..] != digest {
        return Err(Error::CorruptRecord);
    }
    let mut input = Decoder(body);
    if input.take(8)? != MAGIC {
        return Err(Error::CorruptRecord);
    }
    let version = input.u32()?;
    if version != CHECKPOINT_VERSION || input.u32()? != 0 {
        return Err(Error::CorruptRecord);
    }
    let sequence = input.u64()?;
    if input.u64()? != bytes.len() as u64 {
        return Err(Error::CorruptRecord);
    }
    let count = input.count(MAX_SHARDS, SHARD_BYTES)?;
    let mut shards = Vec::with_capacity(count);
    for _ in 0..count {
        let worker = WorkerId(u16::from_le_bytes(input.array()?));
        let geometry = CheckpointGeometry {
            slab_bytes: input.u64()?,
            segment_bytes: input.u64()?,
            segment_count: input.u64()?,
            memory_alignment: input.u64()?,
            offset_alignment: input.u64()?,
            length_alignment: input.u64()?,
            layout_digest: input.array()?,
        };
        geometry.validate()?;
        let count = input.count(MAX_ITEMS, SEGMENT_BYTES)?;
        let mut segments = Vec::with_capacity(count);
        for _ in 0..count {
            segments.push(SegmentSnapshot {
                id: SegmentId(input.u64()?),
                generation: Generation(input.u64()?),
                state: match input.take(1)?[0] {
                    0 => SegmentState::Free,
                    1 => SegmentState::Open,
                    2 => SegmentState::Sealed,
                    3 => SegmentState::Evicting,
                    _ => return Err(Error::CorruptRecord),
                },
                used_bytes: input.u64()?,
                retained_records: {
                    let count = input.count(MAX_ITEMS, RETAINED_RECORD_BYTES)?;
                    let mut records = Vec::with_capacity(count);
                    for _ in 0..count {
                        let offset = input.u64()?;
                        let seconds = input.u64()?;
                        let nanos = input.u32()?;
                        if nanos >= 1_000_000_000 {
                            return Err(Error::CorruptRecord);
                        }
                        let until = std::time::UNIX_EPOCH
                            .checked_add(std::time::Duration::new(seconds, nanos))
                            .ok_or(Error::CorruptRecord)?;
                        records.push((offset, until));
                    }
                    records
                },
            });
        }
        let count = input.count(MAX_ITEMS, PAGE_ENTRY_BYTES)?;
        let mut entries = Vec::with_capacity(count);
        for _ in 0..count {
            let metadata = input.descriptor()?;
            let page = PageId {
                version: metadata.version.clone(),
                number: PageNumber(input.u64()?),
            };
            let key_id = KeyId(input.array()?);
            let segment = SegmentId(input.u64()?);
            let generation = Generation(input.u64()?);
            if input.u64()? != 0 {
                return Err(Error::CorruptRecord);
            }
            let offset = input.u64()?;
            let length = usize::try_from(input.u64()?).map_err(|_| Error::CorruptRecord)?;
            let extent = Extent::new(offset, length)?;
            entries.push((
                page,
                IndexedPage {
                    metadata,
                    key_id,
                    location: RecordLocation {
                        segment,
                        generation,
                        extent,
                    },
                },
            ));
        }
        let count = input.count(MAX_ITEMS, DESCRIPTOR_BYTES)?;
        let mut metadata = Vec::with_capacity(count);
        for _ in 0..count {
            metadata.push(input.descriptor()?);
        }
        shards.push(ShardImage {
            worker,
            geometry,
            index: IndexSnapshot { entries, metadata },
            segments,
        });
    }
    if !input.0.is_empty() {
        return Err(Error::CorruptRecord);
    }
    let image = CheckpointImage {
        version,
        sequence,
        shards,
    };
    validate_image(&image)?;
    Ok(image)
}

/// Conservative accounting, not serialized size: per-item scratch covers growing
/// reference hash tables (including old buckets during growth), extent sorting,
/// isolated segment slots, lease Rc allocations and free-tree nodes. String copies
/// include page identity duplication and descriptor parsing during validation.
/// Live restored index storage has its own page/catalog capacities.
pub(super) fn recovery_memory(bytes: &[u8], budget: usize) -> Result<usize> {
    if bytes.len() < HEADER_BYTES + 4 + DIGEST_BYTES || bytes.len() > MAX_CHECKPOINT_BYTES {
        return Err(Error::CorruptRecord);
    }
    sequence_hint(bytes)?;
    let mut used = bytes.len();
    let mut charge = |count: usize, size: usize| -> Result<()> {
        used = count
            .checked_mul(size)
            .and_then(|n| used.checked_add(n))
            .filter(|n| *n <= budget)
            .ok_or(Error::Overloaded)?;
        Ok(())
    };
    charge(1, std::mem::size_of::<CheckpointImage>())?;
    let mut input = Decoder(&bytes[HEADER_BYTES..bytes.len() - DIGEST_BYTES]);
    let shards = input.count(MAX_SHARDS, SHARD_BYTES)?;
    charge(shards, std::mem::size_of::<ShardImage>() + 512)?;
    for _ in 0..shards {
        input.take(2)?;
        input.take(16)?;
        let segment_count = input.u64()?;
        input.take(24)?;
        input.take(32)?;
        let segments = input.count(MAX_ITEMS, SEGMENT_BYTES)?;
        if segment_count != segments as u64 {
            return Err(Error::CorruptRecord);
        }
        charge(segments, std::mem::size_of::<SegmentSnapshot>() + 512)?;
        for _ in 0..segments {
            input.take(25)?;
            let count = input.count(MAX_ITEMS, RETAINED_RECORD_BYTES)?;
            charge(count, 128)?;
            input.take(count * RETAINED_RECORD_BYTES)?;
        }
        let pages = input.count(MAX_ITEMS, PAGE_ENTRY_BYTES)?;
        charge(pages, std::mem::size_of::<(PageId, IndexedPage)>() + 512)?;
        for _ in 0..pages {
            charge(input.descriptor_bytes()?, 4)?;
            input.take(64)?;
        }
        let metadata = input.count(MAX_ITEMS, DESCRIPTOR_BYTES)?;
        charge(metadata, std::mem::size_of::<VersionMetadata>() + 512)?;
        for _ in 0..metadata {
            charge(input.descriptor_bytes()?, 4)?;
        }
    }
    if !input.0.is_empty() {
        return Err(Error::CorruptRecord);
    }
    Ok(used)
}

/// Untrusted ordering hint only. Full length, digest and structure are checked
/// after reading the selected slot, never used to authorize installation.
pub(crate) fn sequence_hint(bytes: &[u8]) -> Result<u64> {
    if bytes.len() < HEADER_BYTES
        || &bytes[..8] != MAGIC
        || u32::from_le_bytes(bytes[8..12].try_into().unwrap()) != CHECKPOINT_VERSION
    {
        return Err(Error::CorruptRecord);
    }
    Ok(u64::from_le_bytes(bytes[16..24].try_into().unwrap()))
}

fn validate_descriptor(metadata: &VersionMetadata) -> Result<()> {
    let version = &metadata.version;
    if version.object.cache.0.is_empty()
        || version.object.cache.0.len() > MAX_STRING_BYTES
        || version.etag.as_bytes().len() > MAX_STRING_BYTES
        || metadata.length > crate::model::MAX_WIRE_INTEGER
    {
        return Err(Error::CorruptRecord);
    }
    StrongEtag::parse(version.etag.as_bytes()).map_err(|_| Error::CorruptRecord)?;
    Ok(())
}
fn version_key(version: &ObjectVersion) -> (&str, &[u8; 32], &[u8]) {
    (
        &version.object.cache.0,
        &version.object.key.0,
        version.etag.as_bytes(),
    )
}
fn validate_image(image: &CheckpointImage) -> Result<()> {
    if image.version != CHECKPOINT_VERSION
        || image.shards.is_empty()
        || image.shards.len() > MAX_SHARDS
    {
        return Err(Error::CorruptRecord);
    }
    let mut workers = HashSet::default();
    let mut pages = HashSet::default();
    let mut lengths: HashMap<&ObjectVersion, &VersionMetadata> = HashMap::default();
    for shard in &image.shards {
        if !workers.insert(shard.worker) {
            return Err(Error::CorruptRecord);
        }
        shard.validate()?;
        for (page, _) in &shard.index.entries {
            if !pages.insert(page) {
                return Err(Error::CorruptRecord);
            }
        }
        for metadata in shard
            .index
            .metadata
            .iter()
            .chain(shard.index.entries.iter().map(|(_, entry)| &entry.metadata))
        {
            if let Some(old) = lengths.get(&metadata.version)
                && !old.compatible(metadata)
            {
                return Err(Error::CorruptRecord);
            }
            lengths.insert(&metadata.version, metadata);
        }
    }
    Ok(())
}

struct Encoder(Vec<u8>);
impl Encoder {
    fn writer(&mut self) -> wire_codec::Writer<'_> {
        wire_codec::Writer::new(
            &mut self.0,
            MAX_CHECKPOINT_BYTES - DIGEST_BYTES,
            wire_codec::Endian::Little,
        )
    }
    fn bytes(&mut self, bytes: &[u8]) -> Result<()> {
        self.writer().bytes(bytes).map_err(|_| Error::CorruptRecord)
    }
    fn u32(&mut self, value: u32) -> Result<()> {
        self.writer().u32(value).map_err(|_| Error::CorruptRecord)
    }
    fn u64(&mut self, value: u64) -> Result<()> {
        self.writer().u64(value).map_err(|_| Error::CorruptRecord)
    }
    fn count(&mut self, value: usize) -> Result<()> {
        self.writer().count(value).map_err(|_| Error::CorruptRecord)
    }
    fn string(&mut self, value: &[u8]) -> Result<()> {
        // Descriptor validation owns field limits; preserve the u32 wire bound.
        self.writer()
            .length_prefixed(value, u32::MAX as usize)
            .map_err(|_| Error::CorruptRecord)
    }
    fn descriptor(&mut self, metadata: &VersionMetadata) -> Result<()> {
        self.string(metadata.version.object.cache.0.as_bytes())?;
        self.bytes(&metadata.version.object.key.0)?;
        self.string(metadata.version.etag.as_bytes())?;
        self.u64(metadata.length)?;
        self.string(
            metadata
                .content_type
                .as_ref()
                .map_or(&[][..], |v| v.as_bytes()),
        )?;
        Ok(())
    }
}
impl<'a> Decoder<'a> {
    fn descriptor_bytes(&mut self) -> Result<usize> {
        let cache = self.string()?.len();
        self.take(32)?;
        let etag = self.string()?.len();
        self.take(8)?;
        let mime = self.string()?.len();
        Ok(cache + etag + mime)
    }
    fn count(&mut self, max: usize, minimum_bytes: usize) -> Result<usize> {
        self.read(|reader| reader.count(max, minimum_bytes))
    }
    fn string(&mut self) -> Result<&'a [u8]> {
        self.read(|reader| reader.length_prefixed(MAX_STRING_BYTES))
    }
    fn descriptor(&mut self) -> Result<VersionMetadata> {
        let cache = std::str::from_utf8(self.string()?)
            .map_err(|_| Error::CorruptRecord)?
            .to_owned();
        let key = CacheKey(self.array()?);
        let etag = StrongEtag::parse(self.string()?).map_err(|_| Error::CorruptRecord)?;
        let length = self.u64()?;
        let value = self.string()?;
        let content_type = if value.is_empty() {
            None
        } else {
            Some(crate::model::ContentType::parse(value).map_err(|_| Error::CorruptRecord)?)
        };
        Ok(VersionMetadata {
            content_type,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(cache),
                    key,
                },
                etag,
            },
            length,
        })
    }
}

#[cfg(test)]
// Standalone store fixtures use the production candidate scanner and cache-scoped
// filter, but do not create an application worker directory. No legacy loader is
// compiled into the dataplane.
impl Recovery {
    pub(crate) fn load(&self, alignment: Alignment) -> Operation<'_, Option<CheckpointImage>> {
        self.load_filtered(alignment, |_, _| true)
    }
    pub(super) fn load_filtered(
        &self,
        alignment: Alignment,
        available: impl Fn(&CacheId, KeyId) -> bool,
    ) -> Operation<'_, Option<CheckpointImage>> {
        let result = candidates(&self.directory, MAX_CHECKPOINT_BYTES).map(|mut cuts| {
            cuts.find_map(|(_, mut image)| {
                Recovery::filter_available(&mut image, |_| true, &available);
                let valid = image.shards.iter().all(|s| {
                    s.geometry.matches_alignment(alignment)
                        && self.geometry.get().is_none_or(|g| s.geometry == g)
                }) && image
                    .shards
                    .iter()
                    .find(|s| s.worker == self.index.worker())
                    .is_some_and(|s| self.index.validate_snapshot(&s.index).is_ok());
                valid.then_some(image)
            })
        });
        Box::pin(async move { result })
    }
}

#[cfg(test)]
mod budget_tests {
    use super::*;

    fn metadata() -> VersionMetadata {
        VersionMetadata {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(crate::test_support::security::CACHE.into()),
                    key: CacheKey([7; 32]),
                },
                etag: StrongEtag::parse(b"\"checkpoint\"").unwrap(),
            },
            length: crate::model::PAGE_BYTES,
            content_type: None,
        }
    }

    #[test]
    fn descriptor_charge_counts_each_string_and_prefix_once() {
        for mime in [None, Some(b"application/octet-stream".as_slice())] {
            let mut metadata = metadata();
            metadata.content_type = mime.map(|m| crate::model::ContentType::parse(m).unwrap());
            for strings in [false, true] {
                if strings {
                    metadata.version.object.cache.0 = "c".repeat(MAX_STRING_BYTES);
                    metadata.version.etag = StrongEtag::parse(
                        format!("\"{}\"", "e".repeat(MAX_STRING_BYTES - 2)).as_bytes(),
                    )
                    .unwrap();
                }
                let mut encoder = Encoder(Vec::new());
                encoder.descriptor(&metadata).unwrap();
                assert_eq!(snapshot_metadata_bytes(&metadata), encoder.0.len());
                assert_eq!(snapshot_entry_bytes(&metadata), encoder.0.len() + 64);
                assert_eq!(DESCRIPTOR_BYTES, 52);
                assert_eq!(PAGE_ENTRY_BYTES, 116);
            }
        }
    }

    #[test]
    fn segment_charge_handles_empty_full_and_overflow() {
        assert_eq!(snapshot_base_bytes(0), Ok(SHARD_BYTES));
        assert_eq!(snapshot_segment_bytes(0), Ok(29));
        assert_eq!(snapshot_segment_bytes(63), Ok(1289));
        assert_eq!(snapshot_segment_bytes(MAX_ITEMS), Ok(29 + 20 * MAX_ITEMS));
        assert_eq!(snapshot_segment_bytes(usize::MAX), Err(Error::Overloaded));
        assert_eq!(snapshot_base_bytes(usize::MAX), Err(Error::Overloaded));
    }

    #[test]
    fn image_charge_exactly_matches_encoding_at_record_capacity() {
        let alignment = Alignment::new(4096, 4096, 4096).unwrap();
        let full_record = alignment
            .extent(
                0,
                crate::model::PAGE_BYTES as usize + 16 + super::super::MAX_HEADER_BYTES,
            )
            .unwrap()
            .length();
        // Include a full 1 GiB segment, plus short records that pack more densely.
        for (segment_bytes, record_bytes) in [(1 << 30, full_record), (4 * 4096, 4096)] {
            let capacity = segment_bytes / record_bytes;
            for records in [0, 1, capacity] {
                let geometry = CheckpointGeometry::new(
                    segment_bytes as u64,
                    segment_bytes as u64,
                    1,
                    alignment,
                )
                .unwrap();
                let segments =
                    Segments::from_geometry(geometry.allocation_geometry().unwrap()).unwrap();
                let metadata = metadata();
                let mut entries = Vec::new();
                let mut retained = Vec::new();
                for number in 0..records {
                    let (lease, extent) = segments.append(record_bytes).unwrap();
                    let mut descriptor = metadata.clone();
                    descriptor.version.object.key.0[..8]
                        .copy_from_slice(&(number as u64).to_le_bytes());
                    entries.push((
                        PageId {
                            version: descriptor.version.clone(),
                            number: PageNumber(0),
                        },
                        IndexedPage {
                            metadata: descriptor,
                            key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                            location: RecordLocation {
                                segment: lease.id(),
                                generation: lease.generation(),
                                extent,
                            },
                        },
                    ));
                    retained.push((
                        extent.offset(),
                        std::time::UNIX_EPOCH + std::time::Duration::from_secs(60),
                    ));
                }
                let mut segments = segments.snapshot();
                segments[0].retained_records = retained;
                let image = CheckpointImage {
                    version: CHECKPOINT_VERSION,
                    sequence: 1,
                    shards: vec![ShardImage {
                        worker: WorkerId(0),
                        geometry,
                        index: IndexSnapshot {
                            entries,
                            metadata: vec![metadata],
                        },
                        segments,
                    }],
                };
                let encoded = encode(&image).unwrap();
                assert_eq!(snapshot_image_bytes(&image.shards).unwrap(), encoded.len());
                let decoded = decode(&encoded).unwrap();
                assert_eq!(
                    decoded.shards[0].segments[0].retained_records.len(),
                    records
                );
                assert_eq!(encode(&decoded).unwrap(), encoded);
            }
        }
    }

    #[test]
    fn empty_shard_exact_budget_and_unconfigured_zero_segments() {
        let index = Rc::new(Index::new(
            WorkerId(0),
            1,
            crate::test_support::availability(),
        ));
        let segments = Rc::new(Segments::new(4096));
        let checkpoint = Rc::new(Checkpointer::new(PathBuf::new(), index, segments.clone()));
        assert_eq!(segments.count(), 0);
        assert!(matches!(
            futures::executor::block_on(checkpoint.snapshot_incremental(0)),
            Err(Error::InvalidConfiguration)
        ));
        assert!(checkpoint.frozen.borrow().is_none());
        let alignment = Alignment::new(4096, 4096, 4096).unwrap();
        segments.configure(4096, 1, alignment).unwrap();
        checkpoint
            .configure_geometry(CheckpointGeometry::new(4096, 4096, 1, alignment).unwrap())
            .unwrap();
        let budget = snapshot_base_bytes(1).unwrap();
        assert!(matches!(
            futures::executor::block_on(checkpoint.snapshot_incremental(budget - 1)),
            Err(Error::Overloaded)
        ));
        assert!(checkpoint.frozen.borrow().is_none());
        assert!(checkpoint.paused.borrow().is_none());
        drop(segments.freeze().unwrap());
        let shard = futures::executor::block_on(checkpoint.snapshot_incremental(budget)).unwrap();
        assert!(shard.index.entries.is_empty());
        assert!(shard.segments[0].retained_records.is_empty());
        assert!(checkpoint.frozen.borrow().is_some());
        checkpoint.finish_snapshot();
        assert!(segments.append(4096).is_ok());
    }
}
