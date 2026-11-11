//! Schema-independent two-slot checkpoints with bounded reads and atomic
//! publication, on the reactor ring.
//!
//! Callers serialize publication and decide whether a decoded image can be
//! installed. Slot failures are disposable (the slot is skipped); an
//! inaccessible directory is an error.
use super::{BENEATH, Durability, NO_SYMLINKS, Replacement, ReplacementError, check_regular_size};
use crate::{
    Error, Reactor, Result,
    ring::{Descriptor, OpCtx},
};
use std::{ffi::CString, num::NonZeroUsize, path::Path, rc::Rc};

/// Fixed slot names and encoded length limits.
#[derive(Clone, Copy, Debug)]
pub struct Slots<const HEADER: usize> {
    /// Single-component slot names, distinct, not starting with `.checkpoint.`.
    pub names: [&'static str; 2],

    /// Minimum complete image length, including the header.
    pub minimum_bytes: usize,

    /// Maximum image length accepted before allocating a read buffer.
    pub maximum_bytes: usize,
}

/// One probed slot.
struct Candidate<const HEADER: usize> {
    slot: usize,

    sequence: u64,

    file: Rc<Descriptor>,

    length: usize,

    header: Vec<u8>,
}

/// Newest-first lazy decoding of probed slots.
pub struct Candidates<T, const HEADER: usize> {
    slots: Vec<Candidate<HEADER>>,

    budget: usize,

    decode: fn(&[u8], usize) -> Result<T>,

    /// Slots skipped because they failed to read or decode.
    pub skipped: Vec<(usize, Error)>,
}

impl<const HEADER: usize> Slots<HEADER> {
    /// Reject malformed schemas before touching the namespace.
    fn validate(&self) -> Result<()> {
        if HEADER == 0
            || self.minimum_bytes < HEADER
            || self.maximum_bytes < self.minimum_bytes
            || self.names[0] == self.names[1]
            || self.names.iter().any(|name| {
                name.is_empty()
                    || *name == "."
                    || *name == ".."
                    || name.contains('/')
                    || name.contains('\0')
                    || name.starts_with(".checkpoint.")
            })
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }

    /// Probe each slot's header, ordering by the untrusted sequence hint. A
    /// missing directory yields no candidates.
    pub async fn candidates<T>(
        &self,
        reactor: &Reactor,
        directory: &Path,
        budget: usize,
        sequence: fn(&[u8]) -> Result<u64>,
        decode: fn(&[u8], usize) -> Result<T>,
        ctx: &OpCtx,
    ) -> Result<Candidates<T, HEADER>> {
        self.validate()?;
        let mut candidates = Candidates {
            slots: Vec::with_capacity(2),
            budget,
            decode,
            skipped: Vec::new(),
        };
        let path = super::path_name(directory.as_os_str(), super::PATH_BYTES)?;
        let flags = libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW;
        let dir = match reactor.open(None, path, flags, 0, ctx)?.await {
            Ok(dir) => Rc::new(dir),
            Err(Error::NotFound) => return Ok(candidates),
            Err(error) => return Err(error),
        };
        for (slot, name) in self.names.iter().enumerate() {
            match self
                .probe(reactor, &dir, slot, name, budget, sequence, ctx)
                .await
            {
                Ok(candidate) => candidates.slots.push(candidate),
                Err(Error::NotFound) => (),
                Err(error) => candidates.skipped.push((slot, error)),
            }
        }
        candidates.slots.sort_by_key(|candidate| candidate.sequence);
        Ok(candidates)
    }

    #[allow(clippy::too_many_arguments)]
    async fn probe(
        &self,
        reactor: &Reactor,
        dir: &Rc<Descriptor>,
        slot: usize,
        name: &str,
        budget: usize,
        sequence: fn(&[u8]) -> Result<u64>,
        ctx: &OpCtx,
    ) -> Result<Candidate<HEADER>> {
        let name = CString::new(name).map_err(|_| Error::InvalidConfiguration)?;
        let flags = libc::O_RDONLY | libc::O_NOFOLLOW;
        let file = Rc::new(
            reactor
                .open(
                    Some(dir.clone()),
                    name,
                    flags,
                    super::BENEATH | super::NO_SYMLINKS,
                    ctx,
                )?
                .await?,
        );
        let stat = reactor.stat(file.clone(), ctx).await?;
        check_regular_size(&stat, self.maximum_bytes.min(budget) as u64)?;
        if stat.stx_size < self.minimum_bytes as u64 {
            return Err(Error::InvalidInput);
        }
        let header = read_exact(reactor, &file, 0, vec![0; HEADER], 0, ctx).await?;
        Ok(Candidate {
            slot,
            sequence: sequence(&header)?,
            file,
            length: stat.stx_size as usize,
            header,
        })
    }

    /// The opposite slot and checked successor of the newest valid image.
    pub fn next_publication(&self, newest: Option<(usize, u64)>) -> Option<(usize, u64)> {
        match newest {
            Some((slot, sequence)) if slot < 2 => Some((1 - slot, sequence.checked_add(1)?)),
            Some(_) => None,
            None => Some((0, 1)),
        }
    }

    /// Publish `bytes` into `slot` through a fixed stage with bounded scratch
    /// and no fsync (a periodic hint). Opens an existing directory without
    /// syncing; only a missing directory is created (and its parents synced).
    pub async fn publish(
        &self,
        reactor: &Reactor,
        directory: &Path,
        slot: usize,
        bytes: &[u8],
        chunk: NonZeroUsize,
        ctx: &OpCtx,
    ) -> std::result::Result<(), ReplacementError> {
        use ReplacementError::BeforeRename;
        self.validate().map_err(BeforeRename)?;
        let name = self
            .names
            .get(slot)
            .ok_or(BeforeRename(Error::InvalidInput))?;
        let dir = match reactor.open_directory(directory, false, false, ctx).await {
            Err(Error::NotFound) => reactor.open_directory(directory, true, false, ctx).await,
            other => other,
        }
        .map_err(BeforeRename)?;
        let temporary = ".checkpoint.stage";
        let stage = CString::new(temporary).unwrap();
        // A stale stage is disposable: unlink it without a directory fsync.
        reactor
            .unlink(dir.clone(), stage.clone(), 0, true, ctx)
            .map_err(BeforeRename)?
            .await
            .map_err(BeforeRename)?;
        let flags = libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL;
        let staged = Rc::new(
            reactor
                .open(
                    Some(dir.clone()),
                    stage.clone(),
                    flags,
                    BENEATH | NO_SYMLINKS,
                    ctx,
                )
                .map_err(BeforeRename)?
                .await
                .map_err(BeforeRename)?,
        );
        reactor
            .replace(
                Replacement {
                    directory: dir,
                    staged,
                    temporary: stage,
                    target: CString::new(*name).map_err(|_| BeforeRename(Error::InvalidInput))?,
                    durability: Durability::Publish,
                },
                bytes,
                chunk,
                ctx,
            )
            .await
    }
}

/// Exclusive read target: the tail of a vector from `from` on. The heap
/// allocation is never resized while it is owned by an operation.
struct Tail {
    bytes: Vec<u8>,
    from: usize,
}

// SAFETY: the Vec is owned by the operation and never resized while pending,
// so its heap allocation is stable across moves of the header.
unsafe impl crate::ring::IoBuffer for Tail {
    fn bytes(&self) -> Result<&[u8]> {
        self.bytes.get(self.from..).ok_or(Error::InvalidInput)
    }

    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        self.bytes.get_mut(self.from..).ok_or(Error::InvalidInput)
    }
}

/// Fill `bytes[from..]` from the file at `offset + (position - from)`, in place.
async fn read_exact(
    reactor: &Reactor,
    file: &Rc<Descriptor>,
    offset: u64,
    bytes: Vec<u8>,
    from: usize,
    ctx: &OpCtx,
) -> Result<Vec<u8>> {
    let mut tail = Tail { bytes, from };
    while tail.from < tail.bytes.len() {
        let position = offset + (tail.from - from) as u64;
        let done = reactor
            .read_at(file.clone(), position, tail, (), ctx)
            .await?;
        if done.bytes == 0 {
            return Err(Error::InvalidInput);
        }
        tail = done.buffer;
        tail.from += done.bytes;
    }
    Ok(tail.bytes)
}

impl<T, const HEADER: usize> Candidates<T, HEADER> {
    /// Decode the newest remaining image, skipping slots that fail to read or
    /// decode. `None` when every slot was tried.
    pub async fn next(&mut self, reactor: &Reactor, ctx: &OpCtx) -> Option<(usize, T)> {
        while let Some(candidate) = self.slots.pop() {
            let slot = candidate.slot;
            match self.decode_one(reactor, candidate, ctx).await {
                Ok(image) => return Some((slot, image)),
                Err(error) => self.skipped.push((slot, error)),
            }
        }
        None
    }

    async fn decode_one(
        &self,
        reactor: &Reactor,
        candidate: Candidate<HEADER>,
        ctx: &OpCtx,
    ) -> Result<T> {
        let mut bytes = Vec::new();
        bytes
            .try_reserve_exact(candidate.length)
            .map_err(|_| Error::Overloaded)?;
        bytes.extend_from_slice(&candidate.header);
        bytes.resize(candidate.length, 0);
        let bytes = read_exact(reactor, &candidate.file, HEADER as u64, bytes, HEADER, ctx).await?;
        // Never grow the allocation if the file grew after probing.
        let probe = reactor
            .read_at(candidate.file, candidate.length as u64, vec![0; 1], (), ctx)
            .await?;
        if probe.bytes != 0 {
            return Err(Error::InvalidInput);
        }
        (self.decode)(&bytes, self.budget)
    }

    /// Slots still to decode.
    pub fn remaining(&self) -> usize {
        self.slots.len()
    }
}
