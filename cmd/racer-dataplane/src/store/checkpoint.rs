//! Metadata-only checkpoints with alternating two-generation publication.
//!
//! Page data and its index live in the extent volume, which is durable on its
//! own. A checkpoint only carries object metadata: standalone descriptors and
//! the descriptors of versions with pages on disk. Write/rename provides no
//! fsync durability; a lost checkpoint only makes metadata cold.

use super::Decoder;
use super::catalog::Index;
use super::catalog::IndexSnapshot;
use super::versions::VersionEntry;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::CacheKey;
use crate::model::ObjectId;
use crate::model::ObjectVersion;
use crate::model::StrongEtag;
use crate::model::VersionMetadata;
use crate::model::WorkerId;
use crate::runtime::HashMap;
use crate::runtime::HashSet;
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

/// Version descriptors copied per reactor turn by an incremental snapshot.
const SNAPSHOT_BATCH: usize = 128;

pub struct Checkpointer {
    layout_digest: [u8; 32],
    directory: PathBuf,
    index: Rc<Index>,
    layout: Cell<Option<CheckpointLayout>>,
    /// A successful snapshot has not been finished yet.
    taken: Cell<bool>,
    writer: RefCell<std::rc::Weak<super::StoreWriter>>,
    paused: RefCell<Option<super::WriterPause>>,
}

impl Checkpointer {
    pub fn new(directory: PathBuf, index: Rc<Index>) -> Self {
        Self {
            layout_digest: FILE_LAYOUT_DIGEST,
            directory,
            index,
            layout: Cell::new(None),
            taken: Cell::new(false),
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

    pub fn layout(&self) -> Result<CheckpointLayout> {
        self.layout.get().ok_or(Error::InvalidConfiguration)
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

    /// Call after storage opens, before taking snapshots.
    pub fn configure_layout(&self) -> Result<CheckpointLayout> {
        if self.taken.get() {
            return Err(Error::Overloaded);
        }
        let layout = CheckpointLayout {
            layout_digest: self.layout_digest,
        };
        self.layout.set(Some(layout));
        Ok(layout)
    }

    pub fn snapshot_shard(&self) -> Operation<'_, ShardImage> {
        Box::pin(async move {
            let layout = self.layout()?;
            if self.taken.get() {
                return Err(Error::Overloaded);
            }
            let paused = self.pause_writer().await?;
            let shard = ShardImage {
                worker: self.index.worker(),
                layout,
                index: self.index.snapshot()?,
            };
            shard.validate()?;
            self.taken.set(true);
            *self.paused.borrow_mut() = paused;
            Ok(shard)
        })
    }

    /// Copy at most 128 version descriptors per reactor turn and reject budget
    /// pressure before retaining the next batch. No payload is read or fsynced.
    pub fn snapshot_incremental(self: &Rc<Self>, budget: usize) -> Operation<'static, ShardImage> {
        let owner = self.clone();
        Box::pin(async move {
            let layout = owner.layout()?;
            if owner.taken.get() {
                return Err(Error::Overloaded);
            }
            let mut remaining = budget.checked_sub(SHARD_BYTES).ok_or(Error::Overloaded)?;
            // Inspect borrowed index state before draining writes.
            owner.index.preflight_snapshot(&mut remaining)?;
            remaining = budget - SHARD_BYTES;
            // Keep ownership in the future until success so cancellation resumes.
            let paused = owner.pause_writer().await?;
            let result = async {
                // Recheck while copying: draining writes can publish versions.
                let metadata = owner.index.snapshot_metadata_budgeted(&mut remaining)?;
                let mut versions = Vec::new();
                let mut cursor = None;
                loop {
                    let (next, batch) = owner.index.snapshot_versions_budgeted(
                        cursor,
                        SNAPSHOT_BATCH,
                        &mut remaining,
                    )?;
                    let finished = batch.len() < SNAPSHOT_BATCH;
                    versions.extend(batch);
                    if finished {
                        break;
                    }
                    cursor = next;
                    yield_now().await;
                }
                Ok(ShardImage {
                    worker: owner.index.worker(),
                    layout,
                    index: IndexSnapshot { metadata, versions },
                })
            }
            .await;
            if result.is_ok() {
                owner.taken.set(true);
                *owner.paused.borrow_mut() = paused;
            }
            result
        })
    }

    /// Required on every owning worker after success, failure, or coordinator abort.
    /// Images are Send values, deliberately containing no worker-local lease/Rc.
    pub fn finish_snapshot(&self) {
        self.taken.set(false);
        let paused = self.paused.borrow_mut().take();
        drop(paused);
    }

    /// Whether a successful snapshot still awaits [`Checkpointer::finish_snapshot`].
    pub fn is_snapshot_taken(&self) -> bool {
        self.taken.get()
    }

    /// One coordinator calls this after all owner-worker snapshots have succeeded.
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

    /// Periodic disposable hints. The coordinator serializes generations. No
    /// fsync is issued.
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

pub(super) fn snapshot_metadata_bytes(metadata: &VersionMetadata) -> usize {
    DESCRIPTOR_BYTES + descriptor_string_bytes(metadata)
}

pub(super) fn snapshot_version_bytes(entry: &VersionEntry) -> usize {
    VERSION_BYTES + descriptor_string_bytes(&entry.metadata) + entry.key_ids.len() * KEY_ID_BYTES
}

fn descriptor_string_bytes(metadata: &VersionMetadata) -> usize {
    // Encoder::descriptor writes each string once. Its three u32 length
    // prefixes are already included in DESCRIPTOR_BYTES.
    metadata.version.object.cache.0.len()
        + metadata.version.etag.as_bytes().len()
        + metadata
            .content_type
            .as_ref()
            .map_or(0, |mime| mime.as_bytes().len())
}

fn snapshot_image_bytes(shards: &[ShardImage]) -> Result<usize> {
    let mut total = HEADER_BYTES + 4 + DIGEST_BYTES;
    for shard in shards {
        total = total.checked_add(SHARD_BYTES).ok_or(Error::Overloaded)?;
        for bytes in shard
            .index
            .versions
            .iter()
            .map(snapshot_version_bytes)
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

/// Newest-valid checkpoint selection with cold-metadata fallback. Installation
/// only restores metadata; page data and holds come from the mounted volume.
pub struct Recovery {
    #[cfg(test)]
    directory: PathBuf,
    index: Rc<Index>,
    layout: Cell<Option<CheckpointLayout>>,
}

impl Recovery {
    /// Filter disposable checkpoint state before installation. Standalone metadata
    /// has no key ID, so it requires both a current cache UID and an active page
    /// key. A version descriptor keeps only its available page keys and is
    /// dropped when none remain.
    pub fn filter_available(
        image: &mut CheckpointImage,
        mut metadata_available: impl FnMut(&racer_control_wire::CacheId) -> bool,
        mut available: impl FnMut(&racer_control_wire::CacheId, KeyId) -> bool,
    ) {
        for shard in &mut image.shards {
            shard.index.versions.retain_mut(|entry| {
                let cache = &entry.version.object.cache;
                entry.key_ids.retain(|key_id| available(cache, *key_id));
                !entry.key_ids.is_empty()
            });
            shard
                .index
                .metadata
                .retain(|m| metadata_available(&m.version.object.cache));
        }
    }

    pub fn new(_directory: PathBuf, index: Rc<Index>) -> Self {
        Self {
            #[cfg(test)]
            directory: _directory,
            index,
            layout: Cell::new(None),
        }
    }

    /// Configure the opened storage layout before installation. No I/O is done.
    pub fn configure_layout(&self, layout: CheckpointLayout) {
        self.layout.set(Some(layout));
    }

    /// Before admission, install one validated cut into this worker. None resets
    /// its metadata. Pages on disk are untouched either way.
    pub fn install_shard(&self, image: Option<ShardImage>) -> Operation<'_, ()> {
        Box::pin(async move {
            let layout = self.layout.get().ok_or(Error::InvalidConfiguration)?;
            let image = image.unwrap_or_else(|| ShardImage {
                worker: self.index.worker(),
                layout,
                index: IndexSnapshot::default(),
            });
            if image.worker != self.index.worker() || image.layout != layout {
                return Err(Error::CorruptRecord);
            }
            image.validate()?;
            // Index::restore validates capacity before any mutation. There is no
            // await between validation and installation; the coordinator fences
            // admission for the complete operation.
            self.index.restore(image.index)
        })
    }
}

/// Probe only fixed-size headers, then decode one slot at a time, newest first.
/// Callers must discard a rejected image before advancing. An inaccessible storage
/// directory is fatal; individual checkpoint files are disposable hints.
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
// Integers are little-endian: magic[8], version:u32, flags:u32, sequence:u64,
// total_bytes:u64, shard_count:u32, then shards and digest[32]. Each shard is
// worker:u16, layout_digest[32] (the raw-device layout or file mode), counted
// version descriptors, and counted standalone descriptors. Counts and string
// lengths are u32. A descriptor is cache UTF-8 string, key[32], quoted ETag
// string, length:u64, and a counted ASCII MIME string where zero length means
// absent. A version descriptor is a descriptor followed by counted page key IDs
// (key_id[16] each). Encoder ordering is canonical by worker, full version
// identity, and key ID. No freshness, page locations, holds, or payload bytes
// are serialized. Checkpoints with any other version (including version 1,
// which carried page locations) are skipped as disposable cache hints.
pub const CHECKPOINT_VERSION: u32 = 2;
pub const FILE_LAYOUT_DIGEST: [u8; 32] = *b"racer-file-layout-v1\0\0\0\0\0\0\0\0\0\0\0\0";
pub const MAX_CHECKPOINT_BYTES: usize = 64 * 1024 * 1024;
const MAGIC: &[u8; 8] = b"RACERCP\0";
const HEADER_BYTES: usize = 32;
pub(super) const DIGEST_BYTES: usize = 32;
const MAX_SHARDS: usize = 4096;
const MAX_ITEMS: usize = 1_000_000;
const MAX_STRING_BYTES: usize = 8192;
/// Most distinct page keys recorded for one version.
pub(super) const MAX_VERSION_KEYS: usize = 256;

// Shared with decode_with_budget/recovery_memory input.count calls.
pub(super) const SHARD_BYTES: usize = 42; // worker + digest + two u32 counts.
const DESCRIPTOR_BYTES: usize = 52; // key + length + three u32 string lengths.
const VERSION_BYTES: usize = DESCRIPTOR_BYTES + 4; // descriptor + key ID count.
const KEY_ID_BYTES: usize = 16;

/// Storage layout a checkpoint belongs to, so a valid checksum cannot install
/// metadata for a different device mapping.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CheckpointLayout {
    pub layout_digest: [u8; 32],
}

pub struct ShardImage {
    pub worker: WorkerId,
    pub layout: CheckpointLayout,
    pub index: IndexSnapshot,
}
impl ShardImage {
    /// Structural validation without touching live state.
    pub fn validate(&self) -> Result<()> {
        self.index.validate_metadata()?;
        if self.index.versions.len() > MAX_ITEMS || self.index.metadata.len() > MAX_ITEMS {
            return Err(Error::CorruptRecord);
        }
        let mut versions = HashSet::default();
        for metadata in &self.index.metadata {
            validate_descriptor(metadata)?;
            if !versions.insert(&metadata.version) {
                return Err(Error::CorruptRecord);
            }
        }
        let mut versions = HashSet::default();
        for entry in &self.index.versions {
            validate_version(entry)?;
            if !versions.insert(&entry.version) {
                return Err(Error::CorruptRecord);
            }
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
        out.bytes(&shard.layout.layout_digest)?;
        out.count(shard.index.versions.len())?;
        let mut versions: Vec<_> = shard.index.versions.iter().collect();
        versions.sort_by(|a, b| version_order(&a.version).cmp(&version_order(&b.version)));
        for (i, entry) in versions.into_iter().enumerate() {
            if i % SNAPSHOT_BATCH == 0 {
                yield_now().await;
            }
            out.descriptor(&entry.metadata)?;
            let mut keys = entry.key_ids.clone();
            keys.sort_unstable_by_key(|key| key.0);
            out.count(keys.len())?;
            for key in keys {
                out.bytes(&key.0)?;
            }
        }
        out.count(shard.index.metadata.len())?;
        let mut metadata: Vec<_> = shard.index.metadata.iter().collect();
        metadata.sort_by(|a, b| version_order(&a.version).cmp(&version_order(&b.version)));
        for (i, metadata) in metadata.into_iter().enumerate() {
            if i % SNAPSHOT_BATCH == 0 {
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

/// Reject truncation, trailing data, unknown versions, and malformed entries.
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
        let layout = CheckpointLayout {
            layout_digest: input.array()?,
        };
        let count = input.count(MAX_ITEMS, VERSION_BYTES)?;
        let mut versions = Vec::with_capacity(count);
        for _ in 0..count {
            let metadata = input.descriptor()?;
            let keys = input.count(MAX_VERSION_KEYS, KEY_ID_BYTES)?;
            let mut entry = VersionEntry::new(metadata);
            for _ in 0..keys {
                entry.key_ids.push(KeyId(input.array()?));
            }
            versions.push(entry);
        }
        let count = input.count(MAX_ITEMS, DESCRIPTOR_BYTES)?;
        let mut metadata = Vec::with_capacity(count);
        for _ in 0..count {
            metadata.push(input.descriptor()?);
        }
        shards.push(ShardImage {
            worker,
            layout,
            index: IndexSnapshot { metadata, versions },
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
/// hash tables (including old buckets during growth) and duplicate checks.
/// String copies include descriptor parsing during validation. Live restored
/// state has its own version and catalog capacities.
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
        input.take(32)?;
        let versions = input.count(MAX_ITEMS, VERSION_BYTES)?;
        charge(versions, std::mem::size_of::<VersionEntry>() + 512)?;
        for _ in 0..versions {
            charge(input.descriptor_bytes()?, 4)?;
            let keys = input.count(MAX_VERSION_KEYS, KEY_ID_BYTES)?;
            charge(keys, 2 * KEY_ID_BYTES)?;
            input.take(keys * KEY_ID_BYTES)?;
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

fn validate_version(entry: &VersionEntry) -> Result<()> {
    validate_descriptor(&entry.metadata)?;
    let mut keys = HashSet::default();
    if entry.version != entry.metadata.version
        || entry.key_ids.len() > MAX_VERSION_KEYS
        || entry.key_ids.iter().any(|key| !keys.insert(*key))
    {
        return Err(Error::CorruptRecord);
    }
    Ok(())
}

fn version_order(version: &ObjectVersion) -> (&str, &[u8; 32], &[u8]) {
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
    let mut lengths: HashMap<&ObjectVersion, &VersionMetadata> = HashMap::default();
    for shard in &image.shards {
        if !workers.insert(shard.worker) {
            return Err(Error::CorruptRecord);
        }
        shard.validate()?;
        for metadata in shard
            .index
            .metadata
            .iter()
            .chain(shard.index.versions.iter().map(|entry| &entry.metadata))
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
// filter, but do not create an application worker directory. This test-only
// loader is not compiled into the dataplane.
impl Recovery {
    pub(crate) fn load(&self) -> Operation<'_, Option<CheckpointImage>> {
        self.load_filtered(|_, _| true)
    }
    pub(super) fn load_filtered(
        &self,
        available: impl Fn(&CacheId, KeyId) -> bool,
    ) -> Operation<'_, Option<CheckpointImage>> {
        let result = candidates(&self.directory, MAX_CHECKPOINT_BYTES).map(|mut cuts| {
            cuts.find_map(|(_, mut image)| {
                Recovery::filter_available(&mut image, |_| true, &available);
                let valid = image
                    .shards
                    .iter()
                    .all(|s| self.layout.get().is_none_or(|l| s.layout == l))
                    && image
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

    fn version(number: u64, keys: usize) -> VersionEntry {
        let mut descriptor = metadata();
        descriptor.version.object.key.0[..8].copy_from_slice(&number.to_le_bytes());
        let mut entry = VersionEntry::new(descriptor);
        for key in 0..keys {
            entry.add_key_id(KeyId([key as u8 + 1; 16]));
        }
        entry
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
                let mut entry = VersionEntry::new(metadata.clone());
                entry.add_key_id(KeyId([1; 16]));
                entry.add_key_id(KeyId([2; 16]));
                assert_eq!(
                    snapshot_version_bytes(&entry),
                    encoder.0.len() + 4 + 2 * KEY_ID_BYTES
                );
                assert_eq!(DESCRIPTOR_BYTES, 52);
                assert_eq!(VERSION_BYTES, 56);
            }
        }
    }

    #[test]
    fn image_charge_exactly_matches_encoding() {
        for (versions, keys) in [(0, 0), (1, 1), (300, 3)] {
            let image = CheckpointImage {
                version: CHECKPOINT_VERSION,
                sequence: 1,
                shards: vec![ShardImage {
                    worker: WorkerId(0),
                    layout: CheckpointLayout {
                        layout_digest: FILE_LAYOUT_DIGEST,
                    },
                    index: IndexSnapshot {
                        metadata: vec![metadata()],
                        versions: (1..=versions).map(|n| version(n, keys)).collect(),
                    },
                }],
            };
            let encoded = encode(&image).unwrap();
            assert_eq!(snapshot_image_bytes(&image.shards).unwrap(), encoded.len());
            let decoded = decode(&encoded).unwrap();
            assert_eq!(decoded.shards[0].index.versions.len(), versions as usize);
            assert_eq!(encode(&decoded).unwrap(), encoded);
        }
    }

    #[test]
    fn version_keys_are_bounded_and_unique() {
        let mut entry = version(1, 2);
        assert!(validate_version(&entry).is_ok());
        entry.key_ids.push(entry.key_ids[0]);
        assert_eq!(validate_version(&entry), Err(Error::CorruptRecord));
        let mut entry = version(1, 0);
        for key in 0..=MAX_VERSION_KEYS {
            entry.key_ids.push(KeyId([
                key as u8,
                (key >> 8) as u8,
                0,
                0,
                0,
                0,
                0,
                0,
                0,
                0,
                0,
                0,
                0,
                0,
                0,
                0,
            ]));
        }
        assert_eq!(validate_version(&entry), Err(Error::CorruptRecord));
    }

    #[test]
    fn empty_shard_exact_budget_and_unconfigured_layout() {
        let index = Rc::new(Index::new(
            WorkerId(0),
            1,
            crate::test_support::availability(),
        ));
        let checkpoint = Rc::new(Checkpointer::new(PathBuf::new(), index));
        assert!(matches!(
            futures::executor::block_on(checkpoint.snapshot_incremental(SHARD_BYTES)),
            Err(Error::InvalidConfiguration)
        ));
        checkpoint.configure_layout().unwrap();
        assert!(matches!(
            futures::executor::block_on(checkpoint.snapshot_incremental(SHARD_BYTES - 1)),
            Err(Error::Overloaded)
        ));
        assert!(!checkpoint.is_snapshot_taken());
        assert!(checkpoint.paused.borrow().is_none());
        let shard =
            futures::executor::block_on(checkpoint.snapshot_incremental(SHARD_BYTES)).unwrap();
        assert!(shard.index.versions.is_empty());
        assert!(shard.index.metadata.is_empty());
        assert!(checkpoint.is_snapshot_taken());
        assert!(matches!(
            futures::executor::block_on(checkpoint.snapshot_incremental(SHARD_BYTES)),
            Err(Error::Overloaded)
        ));
        checkpoint.finish_snapshot();
        assert!(!checkpoint.is_snapshot_taken());
    }

    #[test]
    fn version_one_checkpoints_are_skipped_as_cold_hints() {
        let mut bytes = encode(&CheckpointImage {
            version: CHECKPOINT_VERSION,
            sequence: 9,
            shards: vec![ShardImage {
                worker: WorkerId(0),
                layout: CheckpointLayout {
                    layout_digest: FILE_LAYOUT_DIGEST,
                },
                index: IndexSnapshot::default(),
            }],
        })
        .unwrap();
        bytes[8..12].copy_from_slice(&1u32.to_le_bytes());
        let body = bytes.len() - DIGEST_BYTES;
        let digest = Sha256::digest(&bytes[..body]);
        bytes[body..].copy_from_slice(&digest);
        assert_eq!(sequence_hint(&bytes), Err(Error::CorruptRecord));
        assert_eq!(decode(&bytes).err(), Some(Error::CorruptRecord));
        let directory = crate::test_support::enrollment::Directory::new();
        std::fs::write(directory.0.join(CHECKPOINT_NAMES[0]), &bytes).unwrap();
        assert!(read_candidates(&directory.0).unwrap().is_empty());
    }
}
