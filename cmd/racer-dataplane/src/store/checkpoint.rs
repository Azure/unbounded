//! Consistent logical cuts and alternating two-generation checkpoint publication.
//!
//! The coordinator keeps every owner frozen through publication, then explicitly
//! finishes each snapshot even on failure. Write/rename provides no fsync durability.
use super::{
    checkpoint_format::{
        CHECKPOINT_VERSION, CheckpointCodec, CheckpointGeometry, CheckpointImage, ShardImage,
    },
    index::Index,
    recovery::read_candidates,
    segment::Segments,
};
use crate::error::{Error, Operation, Result};
use std::{
    cell::Cell,
    fs::{self, OpenOptions},
    io::Write,
    path::{Path, PathBuf},
    rc::Rc,
    sync::atomic::{AtomicU64, Ordering},
};

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
        geometry.validate_live(self.index.worker(), &self.segments)?;
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
            let candidates = read_candidates(&self.directory)?;
            let newest = candidates.iter().max_by_key(|(_, image)| image.sequence);
            let sequence = match newest {
                Some((_, image)) => image.sequence.checked_add(1).ok_or(Error::Unavailable)?,
                None => 1,
            };
            // Replace the slot opposite the newest valid image, including after a
            // torn newer publication. The surviving valid generation stays intact.
            let slot = newest.map_or(0, |(slot, _)| 1 - slot);
            let bytes = CheckpointCodec.encode(&CheckpointImage {
                version: CHECKPOINT_VERSION,
                sequence,
                shards,
            })?;
            publish_bytes(&self.directory, slot, &bytes)
        })
    }
}

impl Drop for Checkpointer {
    fn drop(&mut self) {
        self.finish_snapshot();
    }
}

fn publish_bytes(directory: &Path, slot: usize, bytes: &[u8]) -> Result<()> {
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
