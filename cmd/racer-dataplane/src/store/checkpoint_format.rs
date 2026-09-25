//! Bounded, versioned binary checkpoints. SHA-256 covers the header and body.
//!
//! Version 1 uses little-endian integers: magic[8], version:u32, flags:u32,
//! sequence:u64, total_bytes:u64, shard_count:u32, then shards and digest[32].
//! Each shard is worker:u16, six geometry u64s, counted segments, counted page
//! entries, and counted standalone descriptors. Counts and string lengths are
//! u32. A descriptor is cache UTF-8 string, key[32], quoted ETag string, length:u64.
//! A segment is id:u64, generation:u64, state:u8, used_bytes:u64. A page entry is
//! its descriptor, page:u64, key_id[16], segment:u64, generation:u64, slab:u64,
//! offset:u64, disk_length:u64. Encoder ordering is canonical by worker, segment,
//! and full version/page identity. No freshness or payload bytes are serialized.
use super::{
    direct::{DirectAlignment, DirectExtent},
    index::{IndexSnapshot, IndexedPage, RecordLocation},
    segment::{Generation, SegmentId, SegmentSnapshot, SegmentState, Segments},
    slab::{SlabId, SlabLocation},
};
use crate::{
    error::{Error, Result},
    model::{
        envelope::KeyId,
        identity::{
            CacheId, CacheKey, ObjectId, ObjectVersion, PageId, PageNumber, StrongEtag, WorkerId,
        },
        metadata::VersionMetadata,
    },
};
use sha2::{Digest, Sha256};
use std::collections::{HashMap, HashSet};

pub const CHECKPOINT_VERSION: u32 = 1;
pub const MAX_CHECKPOINT_BYTES: usize = 64 * 1024 * 1024;
const MAGIC: &[u8; 8] = b"RACERCP\0";
const HEADER_BYTES: usize = 32;
const DIGEST_BYTES: usize = 32;
const MAX_SHARDS: usize = 4096;
const MAX_ITEMS: usize = 1_000_000;
const MAX_STRING_BYTES: usize = 8192;

/// Persist allocation geometry so a valid checksum cannot hide incompatible slabs.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CheckpointGeometry {
    pub slab_bytes: u64,
    pub segment_bytes: u64,
    pub segment_count: u64,
    pub memory_alignment: u64,
    pub offset_alignment: u64,
    pub length_alignment: u64,
}

impl CheckpointGeometry {
    pub fn new(
        slab_bytes: u64,
        segment_bytes: u64,
        segment_count: u64,
        alignment: DirectAlignment,
    ) -> Result<Self> {
        let geometry = Self {
            slab_bytes,
            segment_bytes,
            segment_count,
            memory_alignment: alignment.memory() as u64,
            offset_alignment: alignment.offset(),
            length_alignment: alignment.length() as u64,
        };
        geometry.validate()?;
        Ok(geometry)
    }

    pub fn alignment(&self) -> Result<DirectAlignment> {
        DirectAlignment::validate(
            usize::try_from(self.memory_alignment).map_err(|_| Error::CorruptRecord)?,
            self.offset_alignment,
            usize::try_from(self.length_alignment).map_err(|_| Error::CorruptRecord)?,
        )
    }

    pub fn validate(&self) -> Result<()> {
        self.alignment()?;
        if self.segment_bytes == 0
            || self.slab_bytes == 0
            || self.segment_count == 0
            || self.segment_count > MAX_ITEMS as u64
            || self.slab_bytes % self.segment_bytes != 0
            || self.segment_count > self.slab_bytes / self.segment_bytes
            || self.segment_bytes % self.offset_alignment != 0
            || self.segment_bytes % self.length_alignment != 0
            || self.segment_count.checked_mul(self.segment_bytes).is_none()
        {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }

    pub fn matches_alignment(&self, alignment: DirectAlignment) -> bool {
        self.memory_alignment == alignment.memory() as u64
            && self.offset_alignment == alignment.offset()
            && self.length_alignment == alignment.length() as u64
    }

    pub(crate) fn validate_live(&self, worker: WorkerId, segments: &Segments) -> Result<()> {
        self.validate()?;
        if segments.worker() != worker
            || segments.slab_bytes() != self.slab_bytes
            || segments.segment_bytes() != self.segment_bytes
            || segments.snapshot()?.len() as u64 != self.segment_count
        {
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
        let segments = Segments::new(self.worker, self.geometry.segment_bytes);
        segments.configure(
            self.geometry.slab_bytes,
            self.geometry.segment_count as usize,
            self.geometry.alignment()?,
        )?;
        segments.validate_restore(&self.segments)?;
        // Restoring the isolated table seals open segments, exactly as recovery does.
        segments.restore(self.segments.clone())?;
        let mut pages = HashSet::new();
        let mut versions = HashSet::new();
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
            segments.validate_location(&entry.location)?;
            let extent = entry.location.location.extent;
            extents.push((
                extent.offset(),
                extent
                    .offset()
                    .checked_add(extent.length() as u64)
                    .ok_or(Error::CorruptRecord)?,
            ));
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

pub struct CheckpointCodec;
impl CheckpointCodec {
    pub fn encode(&self, image: &CheckpointImage) -> Result<Vec<u8>> {
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
            out.count(shard.segments.len())?;
            let mut segments: Vec<_> = shard.segments.iter().collect();
            segments.sort_by_key(|segment| segment.id.0);
            for segment in segments {
                out.u64(segment.id.0)?;
                out.u64(segment.generation.0)?;
                out.bytes(&[match segment.state {
                    SegmentState::Free => 0,
                    SegmentState::Open => 1,
                    SegmentState::Sealed => 2,
                    SegmentState::Evicting => 3,
                }])?;
                out.u64(segment.used_bytes)?;
            }
            out.count(shard.index.entries.len())?;
            let mut entries: Vec<_> = shard.index.entries.iter().collect();
            entries.sort_by(|(a, _), (b, _)| {
                version_key(&a.version)
                    .cmp(&version_key(&b.version))
                    .then(a.number.0.cmp(&b.number.0))
            });
            for (page, entry) in entries {
                out.descriptor(&entry.metadata)?;
                out.u64(page.number.0)?;
                out.bytes(&entry.key_id.0)?;
                out.u64(entry.location.segment.0)?;
                out.u64(entry.location.generation.0)?;
                out.u64(entry.location.location.slab.0)?;
                out.u64(entry.location.location.extent.offset())?;
                out.u64(entry.location.location.extent.length() as u64)?;
            }
            out.count(shard.index.metadata.len())?;
            let mut metadata: Vec<_> = shard.index.metadata.iter().collect();
            metadata.sort_by(|a, b| version_key(&a.version).cmp(&version_key(&b.version)));
            for metadata in metadata {
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
    pub fn decode(&self, bytes: &[u8]) -> Result<CheckpointImage> {
        if bytes.len() < HEADER_BYTES + 4 + DIGEST_BYTES || bytes.len() > MAX_CHECKPOINT_BYTES {
            return Err(Error::CorruptRecord);
        }
        let (body, digest) = bytes.split_at(bytes.len() - DIGEST_BYTES);
        if &Sha256::digest(body)[..] != digest {
            return Err(Error::CorruptRecord);
        }
        let mut input = Decoder(body);
        if input.take(8)? != MAGIC || input.u32()? != CHECKPOINT_VERSION || input.u32()? != 0 {
            return Err(Error::CorruptRecord);
        }
        let sequence = input.u64()?;
        if input.u64()? != bytes.len() as u64 {
            return Err(Error::CorruptRecord);
        }
        let count = input.count(MAX_SHARDS, 62)?;
        let mut shards = Vec::new();
        for _ in 0..count {
            let worker = WorkerId(u16::from_le_bytes(input.array()?));
            let geometry = CheckpointGeometry {
                slab_bytes: input.u64()?,
                segment_bytes: input.u64()?,
                segment_count: input.u64()?,
                memory_alignment: input.u64()?,
                offset_alignment: input.u64()?,
                length_alignment: input.u64()?,
            };
            geometry.validate()?;
            let count = input.count(MAX_ITEMS, 25)?;
            let mut segments = Vec::new();
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
                });
            }
            let count = input.count(MAX_ITEMS, 112)?;
            let mut entries = Vec::new();
            for _ in 0..count {
                let metadata = input.descriptor()?;
                let page = PageId {
                    version: metadata.version.clone(),
                    number: PageNumber(input.u64()?),
                };
                let key_id = KeyId(input.array()?);
                let segment = SegmentId(input.u64()?);
                let generation = Generation(input.u64()?);
                let slab = SlabId(input.u64()?);
                let offset = input.u64()?;
                let length = usize::try_from(input.u64()?).map_err(|_| Error::CorruptRecord)?;
                let extent = DirectExtent::checked(offset, length)?;
                entries.push((
                    page,
                    IndexedPage {
                        metadata,
                        key_id,
                        location: RecordLocation {
                            segment,
                            generation,
                            location: SlabLocation { slab, extent },
                        },
                    },
                ));
            }
            let count = input.count(MAX_ITEMS, 48)?;
            let mut metadata = Vec::new();
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
            version: CHECKPOINT_VERSION,
            sequence,
            shards,
        };
        validate_image(&image)?;
        Ok(image)
    }
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
    let mut workers = HashSet::new();
    let mut pages = HashSet::new();
    let mut lengths = HashMap::new();
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
            if lengths
                .insert(&metadata.version, metadata.length)
                .is_some_and(|length| length != metadata.length)
            {
                return Err(Error::CorruptRecord);
            }
        }
    }
    Ok(())
}

struct Encoder(Vec<u8>);
impl Encoder {
    fn bytes(&mut self, bytes: &[u8]) -> Result<()> {
        if bytes.len() > (MAX_CHECKPOINT_BYTES - DIGEST_BYTES).saturating_sub(self.0.len()) {
            return Err(Error::CorruptRecord);
        }
        self.0.extend_from_slice(bytes);
        Ok(())
    }
    fn u32(&mut self, value: u32) -> Result<()> {
        self.bytes(&value.to_le_bytes())
    }
    fn u64(&mut self, value: u64) -> Result<()> {
        self.bytes(&value.to_le_bytes())
    }
    fn count(&mut self, value: usize) -> Result<()> {
        self.u32(u32::try_from(value).map_err(|_| Error::CorruptRecord)?)
    }
    fn string(&mut self, value: &[u8]) -> Result<()> {
        self.count(value.len())?;
        self.bytes(value)
    }
    fn descriptor(&mut self, metadata: &VersionMetadata) -> Result<()> {
        self.string(metadata.version.object.cache.0.as_bytes())?;
        self.bytes(&metadata.version.object.key.0)?;
        self.string(metadata.version.etag.as_bytes())?;
        self.u64(metadata.length)
    }
}

struct Decoder<'a>(&'a [u8]);
impl<'a> Decoder<'a> {
    fn take(&mut self, length: usize) -> Result<&'a [u8]> {
        if length > self.0.len() {
            return Err(Error::CorruptRecord);
        }
        let (value, rest) = self.0.split_at(length);
        self.0 = rest;
        Ok(value)
    }
    fn array<const N: usize>(&mut self) -> Result<[u8; N]> {
        self.take(N)?.try_into().map_err(|_| Error::CorruptRecord)
    }
    fn u32(&mut self) -> Result<u32> {
        Ok(u32::from_le_bytes(self.array()?))
    }
    fn u64(&mut self) -> Result<u64> {
        Ok(u64::from_le_bytes(self.array()?))
    }
    fn count(&mut self, max: usize, minimum_bytes: usize) -> Result<usize> {
        let count = self.u32()? as usize;
        if count > max || count > self.0.len() / minimum_bytes {
            return Err(Error::CorruptRecord);
        }
        Ok(count)
    }
    fn string(&mut self) -> Result<&'a [u8]> {
        let length = self.count(MAX_STRING_BYTES, 1)?;
        self.take(length)
    }
    fn descriptor(&mut self) -> Result<VersionMetadata> {
        let cache = std::str::from_utf8(self.string()?)
            .map_err(|_| Error::CorruptRecord)?
            .to_owned();
        let key = CacheKey(self.array()?);
        let etag = StrongEtag::parse(self.string()?).map_err(|_| Error::CorruptRecord)?;
        Ok(VersionMetadata {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(cache),
                    key,
                },
                etag,
            },
            length: self.u64()?,
        })
    }
}

#[cfg(test)]
#[path = "checkpoint_tests.rs"]
mod tests;
