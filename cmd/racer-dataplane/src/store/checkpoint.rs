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
use racer_control_wire::CacheId;
use crate::model::CacheKey;
use racer_control_wire::KeyId;
use crate::model::ObjectId;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::model::StrongEtag;
use crate::model::VersionMetadata;
use crate::model::WorkerId;
use crate::runtime::HashMap;
use crate::runtime::HashSet;
use crate::runtime::cooperative_turn;
use page_alloc::Alignment;
use page_alloc::Extent;
use page_alloc::Generation;
use page_alloc::SegmentId;
use page_alloc::SegmentSnapshot;
use page_alloc::SegmentState;
use page_alloc::Segments;
use sha2::Digest;
use sha2::Sha256;
use std::cell::Cell;
use std::fs;
use std::fs::OpenOptions;
use std::io::Read;
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;
use std::path::PathBuf;
use std::rc::Rc;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering;

pub(crate) const CHECKPOINT_NAMES: [&str; 2] = ["checkpoint.0", "checkpoint.1"];
static TEMP_SEQUENCE: AtomicU64 = AtomicU64::new(0);

pub struct Checkpointer {
    directory: PathBuf,
    index: Rc<Index>,
    segments: Rc<Segments>,
    geometry: Cell<Option<CheckpointGeometry>>,
    frozen: Cell<bool>,
}

impl Checkpointer {
    pub fn new(directory: PathBuf, index: Rc<Index>, segments: Rc<Segments>) -> Self {
        Self {
            directory,
            index,
            segments,
            geometry: Cell::new(None),
            frozen: Cell::new(false),
        }
    }

    /// Call after slab opening discovers actual geometry, before taking snapshots.
    pub fn configure_geometry(&self, geometry: CheckpointGeometry) -> Result<()> {
        geometry.validate_live(&self.segments)?;
        if self.frozen.get() {
            return Err(Error::Overloaded);
        }
        self.geometry.set(Some(geometry));
        Ok(())
    }

    pub fn snapshot_shard(&self) -> Operation<'_, ShardImage> {
        Box::pin(async move {
            let geometry = self.geometry.get().ok_or(Error::InvalidConfiguration)?;
            if self.frozen.get() {
                return Err(Error::Overloaded);
            }
            self.segments.freeze()?;
            self.frozen.set(true);
            let snapshot = (|| {
                let shard = ShardImage {
                    worker: self.index.worker(),
                    geometry,
                    index: self.index.snapshot()?,
                    segments: self.segments.snapshot()?,
                };
                shard.validate()?;
                Ok(shard)
            })();
            if snapshot.is_err() {
                self.finish_snapshot();
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
            if owner.frozen.get() {
                return Err(Error::Overloaded);
            }
            owner.segments.freeze()?;
            owner.frozen.set(true);
            let result = async {
                let segments = owner.segments.snapshot()?;
                let metadata = owner.index.snapshot_metadata();
                let mut charged = segments.len() * 128
                    + metadata
                        .iter()
                        .map(|m| {
                            2048 + m.version.object.cache.0.len() * 4
                                + m.version.etag.as_bytes().len() * 4
                        })
                        .sum::<usize>();
                if charged > budget {
                    return Err(Error::Overloaded);
                }
                let mut entries = Vec::new();
                let mut cursor = 0;
                loop {
                    let (next, batch) = owner.index.snapshot_pages(cursor, 128);
                    let finished = batch.len() < 128;
                    for (page, entry) in batch {
                        charged += 2048
                            + page.version.object.cache.0.len() * 4
                            + page.version.etag.as_bytes().len() * 4;
                        if charged > budget {
                            return Err(Error::Overloaded);
                        }
                        entries.push((page, entry));
                    }
                    if finished {
                        break;
                    }
                    cursor = next;
                    cooperative_turn().await;
                }
                Ok(ShardImage {
                    worker: owner.index.worker(),
                    geometry,
                    index: super::catalog::IndexSnapshot { entries, metadata },
                    segments,
                })
            }
            .await;
            if result.is_err() {
                owner.finish_snapshot();
            }
            result
        })
    }

    /// Required on every owning worker after success, failure, or coordinator abort.
    /// Images are Send values, deliberately containing no worker-local lease/Rc.
    pub fn finish_snapshot(&self) {
        if self.frozen.replace(false) {
            self.segments.thaw();
        }
    }

    /// One coordinator calls this after all owner-worker snapshots have succeeded.
    /// The coordinator must keep all owners frozen until this operation completes.
    pub fn publish(&self, shards: Vec<ShardImage>) -> Operation<'_, ()> {
        Box::pin(async move {
            let newest = candidates(&self.directory, MAX_CHECKPOINT_BYTES)?
                .next()
                .map(|(slot, image)| (slot, image.sequence));
            let sequence = match &newest {
                Some((_, sequence)) => sequence.checked_add(1).ok_or(Error::Unavailable)?,
                None => 1,
            };
            // Replace the slot opposite the newest valid image, including after a
            // torn newer publication. The surviving valid generation stays intact.
            let slot = newest.map_or(0, |(slot, _)| 1 - slot);
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
        let estimated = shards
            .iter()
            .try_fold(0usize, |total, shard| {
                shard
                    .index
                    .entries
                    .iter()
                    .try_fold(total, |n, (page, _)| {
                        n.checked_add(
                            2048 + page.version.object.cache.0.len() * 4
                                + page.version.etag.as_bytes().len() * 4,
                        )
                    })
                    .and_then(|n| {
                        n.checked_add(
                            shard.index.metadata.len() * 2048 + shard.segments.len() * 128,
                        )
                    })
            })
            .ok_or(Error::Overloaded)?;
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
            use std::ffi::CString;
            use std::os::unix::ffi::OsStrExt;
            let dir = reactor
                .file_open(
                    None,
                    CString::new(directory.as_os_str().as_bytes())
                        .map_err(|_| Error::InvalidConfiguration)?,
                    libc::O_RDONLY | libc::O_DIRECTORY,
                    0,
                    &scope,
                )
                .await?;
            let temporary = CString::new(".checkpoint.periodic.stage").unwrap();
            match reactor
                .file_unlink(dir.clone(), temporary.clone(), &scope)
                .await
            {
                Ok(()) | Err(Error::MissingKey) => (),
                Err(error) => return Err(error),
            }
            let fd = reactor
                .file_open(
                    Some(dir.clone()),
                    temporary.clone(),
                    libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL,
                    0,
                    &scope,
                )
                .await?;
            let mut offset = 0usize;
            for chunk in bytes.chunks(16384) {
                let mut buffer = reactor.file_bytes(chunk)?;
                while buffer.remaining() != 0 {
                    let completion = reactor
                        .write_at(fd.clone(), offset as u64, buffer, (), &scope)
                        .await?;
                    if completion.bytes == 0 {
                        return Err(Error::Io);
                    }
                    offset += completion.bytes;
                    buffer = completion.buffer;
                    buffer.advance(completion.bytes)?;
                }
            }
            reactor
                .file_rename(
                    dir,
                    temporary,
                    CString::new(CHECKPOINT_NAMES[slot]).unwrap(),
                    &scope,
                )
                .await
        }))
    }
}

impl Drop for Checkpointer {
    fn drop(&mut self) {
        self.finish_snapshot();
    }
}

fn publish_bytes(directory: &Path, slot: usize, bytes: &[u8]) -> Result<()> {
    #[cfg(test)]
    if let Some(sim) = uring_runtime::reactor::simulation::Simulation::current() {
        sim.create_dir_all(directory).map_err(|_| Error::Io)?;
        let temporary = directory.join(format!(".checkpoint.{}.tmp", sim.next_sequence()));
        let result = (|| {
            sim.write_file(&temporary, bytes).map_err(|_| Error::Io)?;
            sim.rename(&temporary, &directory.join(CHECKPOINT_NAMES[slot]), 0)
                .map_err(|_| Error::Io)
        })();
        if result.is_err() {
            let _ = sim.unlink(&temporary);
        }
        return result;
    }
    fs::create_dir_all(directory).map_err(|_| Error::Io)?;
    // A stale partial file never prevents a later publication, including after PID
    // reuse. Exactly one application coordinator serializes publications.
    let mut attempts = 0;
    let (temporary, mut file) = loop {
        if attempts == 128 {
            return Err(Error::Io);
        }
        attempts += 1;
        let sequence = TEMP_SEQUENCE.fetch_add(1, Ordering::Relaxed);
        let temporary =
            directory.join(format!(".checkpoint.{}.{sequence}.tmp", std::process::id()));
        match OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)
        {
            Ok(file) => break (temporary, file),
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(_) => return Err(Error::Io),
        }
    };
    let result = (|| {
        file.write_all(bytes).map_err(|_| Error::Io)?;
        drop(file);
        fs::rename(&temporary, directory.join(CHECKPOINT_NAMES[slot])).map_err(|_| Error::Io)
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result
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
                    let empty = Segments::new(geometry.segment_bytes);
                    empty.configure(
                        geometry.slab_bytes,
                        geometry.segment_count as usize,
                        geometry.alignment()?,
                    )?;
                    ShardImage {
                        worker: self.index.worker(),
                        geometry,
                        index: IndexSnapshot {
                            entries: vec![],
                            metadata: vec![],
                        },
                        segments: empty.snapshot()?,
                    }
                }
            };
            if image.worker != self.index.worker() || image.geometry != geometry {
                return Err(Error::CorruptRecord);
            }
            image.validate()?;
            self.segments.validate_restore(&image.segments)?;
            self.index.validate_snapshot(&image.index)?;
            // Index::restore checks its own standalone metadata capacity before
            // mutation. There is no await between validation and these installs.
            // The coordinator must fence admission for the complete operation.
            self.index.restore(image.index)?;
            self.segments.restore(image.segments)?;
            Ok(())
        })
    }
}

/// Probe only fixed-size headers, then decode one slot at a time, newest first.
/// Callers must discard a rejected image before advancing. An inaccessible storage
/// directory is fatal; individual checkpoint files are disposable hints. Slab
/// opening/validation remains independently fatal during Store::open.
pub(crate) fn candidates(directory: &Path, budget: usize) -> Result<Candidates> {
    // Preserve missing-directory cold starts for the standalone store API.
    match CandidateFile::open(directory, true) {
        Ok(_) => (),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return Ok(Candidates {
                slots: vec![],
                budget,
            });
        }
        Err(e) => {
            eprintln!("racer: checkpoint storage directory unavailable: {e}");
            return Err(Error::Io);
        }
    }
    let mut slots = Vec::with_capacity(CHECKPOINT_NAMES.len());
    for (slot, name) in CHECKPOINT_NAMES.iter().enumerate() {
        let result = (|| -> std::io::Result<_> {
            let mut file = CandidateFile::open(&directory.join(name), false)?;
            let length = file.length()?;
            if length < 68 || length > MAX_CHECKPOINT_BYTES as u64 || length > budget as u64 {
                return Err(std::io::Error::other(
                    "checkpoint encoded size exceeds recovery budget or format limit",
                ));
            }
            let mut header = [0; 32];
            file.read_exact(&mut header)?;
            let sequence = sequence_hint(&header)
                .map_err(|_| std::io::Error::other("invalid checkpoint header"))?;
            Ok((slot, sequence, file, length as usize, header))
        })();
        match result {
            Ok(candidate) => slots.push(candidate),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => (),
            Err(e) => eprintln!("racer: skipping checkpoint slot {slot}: {e}"),
        }
    }
    slots.sort_by_key(|(_, sequence, ..)| *sequence);
    Ok(Candidates { slots, budget })
}

pub(crate) struct Candidates {
    slots: Vec<(usize, u64, CandidateFile, usize, [u8; 32])>,
    budget: usize,
}

impl Iterator for Candidates {
    type Item = (usize, CheckpointImage);
    fn next(&mut self) -> Option<Self::Item> {
        while let Some((slot, _, mut file, length, header)) = self.slots.pop() {
            // Length is checked before reserving. read_exact never grows this
            // buffer if the file grows; an extra stack byte detects that race.
            let result = (|| -> Result<CheckpointImage> {
                let mut bytes = Vec::new();
                bytes
                    .try_reserve_exact(length)
                    .map_err(|_| Error::Overloaded)?;
                bytes.resize(length, 0);
                bytes[..32].copy_from_slice(&header);
                file.read_exact(&mut bytes[32..]).map_err(|_| Error::Io)?;
                if file.read(&mut [0]).map_err(|_| Error::Io)? != 0 {
                    return Err(Error::CorruptRecord);
                }
                decode_with_budget(&bytes, self.budget)
            })();
            match result {
                Ok(image) => return Some((slot, image)),
                Err(e) => {
                    eprintln!("racer: skipping checkpoint slot {slot} during read/decode: {e}")
                }
            }
        }
        None
    }
}

enum CandidateFile {
    Real(std::fs::File),
    #[cfg(test)]
    Sim(uring_runtime::reactor::simulation::Handle, u64),
}
impl CandidateFile {
    fn open(path: &Path, directory: bool) -> std::io::Result<Self> {
        let flags =
            libc::O_NOFOLLOW | libc::O_NONBLOCK | if directory { libc::O_DIRECTORY } else { 0 };
        #[cfg(test)]
        if let Some(sim) = uring_runtime::reactor::simulation::Simulation::current() {
            let handle = sim
                .open(None, path, libc::O_RDONLY | flags)?
                .into_sim()
                .expect("simulated file");
            return Ok(Self::Sim(handle, 0));
        }
        OpenOptions::new()
            .read(true)
            .custom_flags(flags)
            .open(path)
            .map(Self::Real)
    }
    fn length(&self) -> std::io::Result<u64> {
        let (regular, length) = match self {
            Self::Real(file) => {
                let m = file.metadata()?;
                (m.is_file(), m.len())
            }
            #[cfg(test)]
            Self::Sim(handle, _) => {
                let m = handle.stat()?;
                (
                    m.stx_mode as u32 & libc::S_IFMT == libc::S_IFREG,
                    m.stx_size,
                )
            }
        };
        if !regular {
            return Err(std::io::Error::other("checkpoint is not a regular file"));
        }
        Ok(length)
    }
}
impl Read for CandidateFile {
    fn read(&mut self, bytes: &mut [u8]) -> std::io::Result<usize> {
        match self {
            Self::Real(file) => file.read(bytes),
            #[cfg(test)]
            Self::Sim(handle, offset) => {
                let n = handle.file_read(*offset, bytes)?;
                *offset += n as u64;
                Ok(n)
            }
        }
    }
}

#[cfg(test)]
pub(crate) fn read_candidates(directory: &Path) -> Result<Vec<(usize, CheckpointImage)>> {
    Ok(candidates(directory, MAX_CHECKPOINT_BYTES)?.collect())
}

// Bounded, versioned binary checkpoints. SHA-256 covers the header and body.
// Version 2 uses little-endian integers: magic[8], version:u32, flags:u32,
// sequence:u64, total_bytes:u64, shard_count:u32, then shards and digest[32].
// Each shard is worker:u16, six geometry u64s, counted segments, counted page
// entries, and counted standalone descriptors. Counts and string lengths are
// u32. A descriptor is cache UTF-8 string, key[32], quoted ETag string, length:u64.
// A segment is id:u64, generation:u64, state:u8, used_bytes:u64. A page entry is
// its descriptor, page:u64, key_id[16], segment:u64, generation:u64, slab:u64,
// offset:u64, disk_length:u64. Encoder ordering is canonical by worker, segment,
// and full version/page identity. No freshness or payload bytes are serialized.
// Version 2 appends a counted ASCII MIME string to every descriptor. Zero length
// means absent. Older versions are discarded as disposable cache hints.
pub const CHECKPOINT_VERSION: u32 = 2;
pub const MAX_CHECKPOINT_BYTES: usize = 64 * 1024 * 1024;
const MAGIC: &[u8; 8] = b"RACERCP\0";
const HEADER_BYTES: usize = 32;
pub(super) const DIGEST_BYTES: usize = 32;
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
        Ok(Alignment::new(
            usize::try_from(self.memory_alignment).map_err(|_| Error::CorruptRecord)?,
            self.offset_alignment,
            usize::try_from(self.length_alignment).map_err(|_| Error::CorruptRecord)?,
        )?)
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
    pub fn matches_alignment(&self, alignment: Alignment) -> bool {
        self.memory_alignment == alignment.memory() as u64
            && self.offset_alignment == alignment.offset()
            && self.length_alignment == alignment.length() as u64
    }
    pub(crate) fn validate_live(&self, segments: &Segments) -> Result<()> {
        self.validate()?;
        if segments.capacity_bytes() != self.slab_bytes
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
        let segments = Segments::new(self.geometry.segment_bytes);
        segments.configure(
            self.geometry.slab_bytes,
            self.geometry.segment_count as usize,
            self.geometry.alignment()?,
        )?;
        segments.validate_restore(&self.segments)?;
        // Restoring the isolated table seals open segments, exactly as recovery does.
        segments.restore(self.segments.clone())?;
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
            segments.validate(
                entry.location.segment,
                entry.location.generation,
                &entry.location.extent,
            )?;
            let extent = entry.location.extent;
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
        out.count(shard.segments.len())?;
        let mut segments: Vec<_> = shard.segments.iter().collect();
        segments.sort_by_key(|segment| segment.id.0);
        for (i, segment) in segments.into_iter().enumerate() {
            if i % 128 == 0 {
                cooperative_turn().await;
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
                cooperative_turn().await;
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
                cooperative_turn().await;
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
    let count = input.count(MAX_SHARDS, 62)?;
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
        };
        geometry.validate()?;
        let count = input.count(MAX_ITEMS, 25)?;
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
            });
        }
        let count = input.count(MAX_ITEMS, 112)?;
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
        let count = input.count(MAX_ITEMS, 48)?;
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
    let shards = input.count(MAX_SHARDS, 62)?;
    charge(shards, std::mem::size_of::<ShardImage>() + 512)?;
    for _ in 0..shards {
        input.take(2)?;
        input.take(16)?;
        let segment_count = input.u64()?;
        input.take(24)?;
        let segments = input.count(MAX_ITEMS, 25)?;
        if segment_count != segments as u64 {
            return Err(Error::CorruptRecord);
        }
        charge(segments, std::mem::size_of::<SegmentSnapshot>() + 512)?;
        input.take(segments * 25)?;
        let pages = input.count(MAX_ITEMS, 112)?;
        charge(pages, std::mem::size_of::<(PageId, IndexedPage)>() + 512)?;
        for _ in 0..pages {
            charge(input.descriptor_bytes()?, 4)?;
            input.take(64)?;
        }
        let metadata = input.count(MAX_ITEMS, 48)?;
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
            if let Some(old) = lengths.get(&metadata.version) {
                if !old.compatible(metadata) {
                    return Err(Error::CorruptRecord);
                }
            }
            lengths.insert(&metadata.version, metadata);
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
