//! Consistent logical cuts and alternating two-generation checkpoint publication.
//!
//! The coordinator keeps every owner frozen through publication, then explicitly
//! finishes each snapshot even on failure. Write/rename provides no fsync durability.
use super::{
    catalog::{Index, Segments},
    checkpoint_format::{
        self, CHECKPOINT_VERSION, CheckpointGeometry, CheckpointImage, ShardImage,
    },
    recovery::read_candidates,
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

    /// Explicit cache reset, serialized with checkpoint publication. Key/cache
    /// removal does not require this: recovery filters current UID/key availability.
    /// Payload files stay intact.
    pub fn invalidate_persisted(&self) -> Result<()> {
        if self.frozen.get() {
            return Err(Error::Overloaded);
        }
        for name in CHECKPOINT_NAMES {
            match fs::remove_file(self.directory.join(name)) {
                Ok(()) => {}
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                Err(_) => return Err(Error::Io),
            }
        }
        Ok(())
    }

    /// Asynchronous reset variant. Paths and directory ownership stay with accepted
    /// filesystem submissions until completion, including on cancellation.
    pub fn invalidate_persisted_async(
        &self,
        reactor: Rc<crate::runtime::reactor::Reactor>,
        scope: crate::runtime::deadline::RequestScope,
    ) -> Result<Operation<'static, ()>> {
        if self.frozen.get() {
            return Err(Error::Overloaded);
        }
        let directory = self.directory.clone();
        Ok(Box::pin(async move {
            use std::{ffi::CString, os::unix::ffi::OsStrExt};
            let path = CString::new(directory.as_os_str().as_bytes())
                .map_err(|_| Error::InvalidConfiguration)?;
            let directory = match reactor
                .file_open(None, path, libc::O_RDONLY | libc::O_DIRECTORY, 0, &scope)
                .await
            {
                Ok(directory) => directory,
                Err(Error::MissingKey) => return Ok(()),
                Err(error) => return Err(error),
            };
            for name in CHECKPOINT_NAMES {
                match reactor
                    .file_unlink(directory.clone(), CString::new(name).unwrap(), &scope)
                    .await
                {
                    Ok(()) | Err(Error::MissingKey) => (),
                    Err(error) => return Err(error),
                }
            }
            Ok(())
        }))
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
            let bytes = checkpoint_format::encode(&CheckpointImage {
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
        reactor: Rc<crate::runtime::reactor::Reactor>,
        scope: crate::runtime::deadline::RequestScope,
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
            let bytes = checkpoint_format::encode_incremental(&CheckpointImage {
                version: CHECKPOINT_VERSION,
                sequence,
                shards,
            })
            .await?;
            if bytes.len() > budget / 2 {
                return Err(Error::Overloaded);
            }
            use std::{ffi::CString, os::unix::ffi::OsStrExt};
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

pub(crate) async fn cooperative_turn() {
    let mut yielded = false;
    std::future::poll_fn(|cx| {
        if yielded {
            std::task::Poll::Ready(())
        } else {
            yielded = true;
            cx.waker().wake_by_ref();
            std::task::Poll::Pending
        }
    })
    .await
}

impl Drop for Checkpointer {
    fn drop(&mut self) {
        self.finish_snapshot();
    }
}

fn publish_bytes(directory: &Path, slot: usize, bytes: &[u8]) -> Result<()> {
    #[cfg(test)]
    if let Some(sim) = crate::runtime::reactor::simulation::Simulation::current() {
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
