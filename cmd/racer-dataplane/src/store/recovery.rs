//! Newest-valid checkpoint selection with empty-cache fallback and no payload scan.
//!
//! Recovery seals open segments and clears freshness. Checkpoints have no fsync
//! guarantee; record validation and AEAD convert stale payload references to misses.
use super::{
    checkpoint::CHECKPOINT_NAMES,
    checkpoint_format::{
        CheckpointCodec, CheckpointGeometry, CheckpointImage, MAX_CHECKPOINT_BYTES, ShardImage,
    },
    direct::DirectAlignment,
    index::{Index, IndexSnapshot},
    segment::Segments,
};
use crate::{
    error::{Error, Operation, Result},
    model::envelope::KeyId,
};
use std::{
    cell::Cell,
    fs::OpenOptions,
    io::Read,
    os::unix::fs::OpenOptionsExt,
    path::{Path, PathBuf},
    rc::Rc,
};

pub struct Recovery {
    directory: PathBuf,
    index: Rc<Index>,
    segments: Rc<Segments>,
    geometry: Cell<Option<CheckpointGeometry>>,
}

impl Recovery {
    /// Cache-scoped filtering for production keyrings. Invoke before installation;
    /// descriptors remain available for pins, but unavailable payloads do not.
    pub fn filter_available_keys(
        image: &mut CheckpointImage,
        mut available: impl FnMut(&crate::model::identity::CacheId, KeyId) -> bool,
    ) {
        for shard in &mut image.shards {
            shard
                .index
                .entries
                .retain(|(page, entry)| available(&page.version.object.cache, entry.key_id));
        }
    }
    pub fn new(directory: PathBuf, index: Rc<Index>, segments: Rc<Segments>) -> Self {
        Self {
            directory,
            index,
            segments,
            geometry: Cell::new(None),
        }
    }

    /// Configure actual opened slab geometry before installation. No I/O is done.
    pub fn configure_geometry(&self, geometry: CheckpointGeometry) -> Result<()> {
        geometry.validate_live(self.index.worker(), &self.segments)?;
        self.geometry.set(Some(geometry));
        Ok(())
    }

    pub fn load(&self, alignment: DirectAlignment) -> Operation<'_, Option<CheckpointImage>> {
        self.load_with_keys(alignment, None)
    }

    /// Missing keys discard their dependent page mappings. Standalone immutable
    /// descriptors, including metadata-only objects, are retained.
    pub fn load_with_keys<'a>(
        &'a self,
        alignment: DirectAlignment,
        available: Option<&'a [KeyId]>,
    ) -> Operation<'a, Option<CheckpointImage>> {
        Box::pin(async move {
            let geometry = self.geometry.get();
            let mut candidates = read_candidates(&self.directory)?;
            for (_, image) in &mut candidates {
                for shard in &mut image.shards {
                    filter_keys(shard, available);
                }
            }
            candidates.retain(|(_, image)| {
                image.shards.iter().all(|shard| {
                    shard.geometry.matches_alignment(alignment)
                        && geometry.is_none_or(|expected| shard.geometry == expected)
                }) && image
                    .shards
                    .iter()
                    .find(|shard| shard.worker == self.index.worker())
                    .is_some_and(|shard| self.index.validate_snapshot(&shard.index).is_ok())
            });
            let image = candidates
                .into_iter()
                .max_by_key(|(_, image)| image.sequence)
                .map(|(_, image)| image);
            Ok(image)
        })
    }

    /// Before admission, install one validated cut into this worker. None resets
    /// its index and allocation table; no slab is scanned or zeroed.
    pub fn install_shard(&self, image: Option<ShardImage>) -> Operation<'_, ()> {
        self.install_shard_with_keys(image, None)
    }

    pub fn install_shard_with_keys<'a>(
        &'a self,
        image: Option<ShardImage>,
        available: Option<&'a [KeyId]>,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            let geometry = self.geometry.get().ok_or(Error::InvalidConfiguration)?;
            let mut image = match image {
                Some(image) => image,
                None => {
                    let empty = Segments::new(self.index.worker(), geometry.segment_bytes);
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
            filter_keys(&mut image, available);
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

fn filter_keys(shard: &mut ShardImage, available: Option<&[KeyId]>) {
    if let Some(available) = available {
        let keys: std::collections::HashSet<_> = available.iter().copied().collect();
        shard
            .index
            .entries
            .retain(|(_, entry)| keys.contains(&entry.key_id));
    }
}

/// Read only the two bounded metadata files. Nonregular files and torn, oversized,
/// incompatible, or checksum-invalid images are rejected independently.
pub(crate) fn read_candidates(directory: &Path) -> Result<Vec<(usize, CheckpointImage)>> {
    let mut candidates = Vec::new();
    for (slot, name) in CHECKPOINT_NAMES.iter().enumerate() {
        let file = match OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
            .open(directory.join(name))
        {
            Ok(file) => file,
            Err(error)
                if matches!(
                    error.kind(),
                    std::io::ErrorKind::NotFound | std::io::ErrorKind::InvalidInput
                ) || error.raw_os_error() == Some(libc::ELOOP) =>
            {
                continue;
            }
            Err(_) => return Err(Error::Io),
        };
        let metadata = file.metadata().map_err(|_| Error::Io)?;
        if !metadata.is_file() || metadata.len() > MAX_CHECKPOINT_BYTES as u64 {
            continue;
        }
        let mut bytes = Vec::new();
        file.take(MAX_CHECKPOINT_BYTES as u64 + 1)
            .read_to_end(&mut bytes)
            .map_err(|_| Error::Io)?;
        if let Ok(image) = CheckpointCodec.decode(&bytes) {
            candidates.push((slot, image));
        }
    }
    Ok(candidates)
}
