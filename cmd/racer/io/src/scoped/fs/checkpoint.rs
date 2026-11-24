//! Schema-independent two-slot checkpoints with bounded reads and atomic
//! publication, on the scoped reactor.
//!
//! Callers serialize publication and decide whether a decoded image can be
//! installed. Slot failures are disposable (the slot is skipped); an
//! inaccessible directory is an error.
use super::{
    Buffer, Files, ReplaceFiles, SecureFiles,
    operations::{Durability, Replacement, ReplacementError},
    secure::{self, BENEATH, NO_SYMLINKS},
};
use crate::{
    Error, Result, Scope,
    scoped::{Reactor, descriptor::Descriptor},
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
pub struct Candidates<T, const HEADER: usize, E> {
    slots: Vec<Candidate<HEADER>>,

    budget: usize,

    decode: fn(&[u8], usize) -> Result<T>,

    /// Slots skipped because they failed to read or decode.
    pub skipped: Vec<(usize, E)>,
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
    pub async fn candidates<S: Scope, T>(
        &self,
        reactor: &Reactor<S>,
        directory: &Path,
        budget: usize,
        sequence: fn(&[u8]) -> Result<u64>,
        decode: fn(&[u8], usize) -> Result<T>,
        scope: &S,
    ) -> Result<Candidates<T, HEADER, S::Error>, S::Error> {
        self.validate()?;
        let mut candidates = Candidates {
            slots: Vec::with_capacity(2),
            budget,
            decode,
            skipped: Vec::new(),
        };
        let missing: S::Error = Error::NotFound.into();
        let dir = match reactor
            .file_directory(directory, false, super::PATH_BYTES, scope)
            .await
        {
            Ok(dir) => dir,
            Err(error) if error == missing => return Ok(candidates),
            Err(error) => return Err(error),
        };
        for (slot, name) in self.names.iter().enumerate() {
            match self
                .probe(reactor, &dir, slot, name, budget, sequence, scope)
                .await
            {
                Ok(candidate) => candidates.slots.push(candidate),
                Err(error) if error == missing => (),
                Err(error) => candidates.skipped.push((slot, error)),
            }
        }
        candidates.slots.sort_by_key(|candidate| candidate.sequence);
        Ok(candidates)
    }

    #[allow(clippy::too_many_arguments)]
    async fn probe<S: Scope>(
        &self,
        reactor: &Reactor<S>,
        dir: &Rc<Descriptor>,
        slot: usize,
        name: &str,
        budget: usize,
        sequence: fn(&[u8]) -> Result<u64>,
        scope: &S,
    ) -> Result<Candidate<HEADER>, S::Error> {
        let name = CString::new(name).map_err(|_| Error::InvalidConfiguration)?;
        let flags = libc::O_RDONLY | libc::O_NOFOLLOW;
        let file = reactor
            .file_open(Some(dir.clone()), name, flags, BENEATH | NO_SYMLINKS, scope)
            .await?;
        let stat = reactor.file_stat(file.clone(), scope).await?;
        secure::check_regular_size(&stat, self.maximum_bytes.min(budget) as u64)?;
        if stat.stx_size < self.minimum_bytes as u64 {
            return Err(Error::InvalidInput.into());
        }
        let header = read_exact(reactor, &file, 0, HEADER, scope).await?;
        Ok(Candidate {
            slot,
            sequence: sequence(&header.data[..HEADER])?,
            file,
            length: stat.stx_size as usize,
            header: header.data[..HEADER].to_vec(),
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
    pub async fn publish<S: Scope>(
        &self,
        reactor: &Reactor<S>,
        directory: &Path,
        slot: usize,
        bytes: &[u8],
        chunk: NonZeroUsize,
        scope: &S,
    ) -> std::result::Result<(), ReplacementError<S::Error>> {
        use ReplacementError::BeforeRename;
        self.validate()
            .map_err(|error| BeforeRename(error.into()))?;
        let name = self
            .names
            .get(slot)
            .ok_or_else(|| BeforeRename(Error::InvalidInput.into()))?;
        let missing: S::Error = Error::NotFound.into();
        let dir = match reactor
            .file_directory(directory, false, super::PATH_BYTES, scope)
            .await
        {
            Ok(dir) => dir,
            Err(error) if error == missing => reactor
                .file_directory(directory, true, super::PATH_BYTES, scope)
                .await
                .map_err(BeforeRename)?,
            Err(error) => return Err(BeforeRename(error)),
        };
        let temporary = ".checkpoint.stage";
        let stage = CString::new(temporary).unwrap();
        // A stale stage is disposable: unlink it without a directory fsync.
        match reactor.file_unlink(dir.clone(), stage.clone(), scope).await {
            Ok(()) => (),
            Err(error) if error == missing => (),
            Err(error) => return Err(BeforeRename(error)),
        }
        let flags = libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL;
        let staged = reactor
            .file_open(
                Some(dir.clone()),
                stage.clone(),
                flags,
                BENEATH | NO_SYMLINKS,
                scope,
            )
            .await
            .map_err(BeforeRename)?;
        reactor
            .file_replace_chunked(
                Replacement {
                    directory: dir,
                    staged,
                    temporary: stage,
                    target: CString::new(*name)
                        .map_err(|_| BeforeRename(Error::InvalidInput.into()))?,
                    durability: Durability::Publish,
                },
                bytes,
                chunk,
                scope,
            )
            .await
    }
}

/// Fill a new buffer from `offset` for exactly `length` bytes.
async fn read_exact<S: Scope>(
    reactor: &Reactor<S>,
    file: &Rc<Descriptor>,
    offset: u64,
    length: usize,
    scope: &S,
) -> Result<Buffer, S::Error> {
    let mut buffer = Buffer::allocate(length)?;
    let mut filled = 0;
    while filled < length {
        buffer.start = filled;
        buffer.end = length;
        let done = reactor
            .read_at(file.clone(), offset + filled as u64, buffer, (), scope)
            .await?;
        let bytes = done.bytes;
        buffer = done.buffer;
        if bytes == 0 {
            return Err(Error::InvalidInput.into());
        }
        filled += bytes;
    }
    buffer.start = 0;
    Ok(buffer)
}

impl<T, const HEADER: usize, E: From<Error>> Candidates<T, HEADER, E> {
    /// Decode the newest remaining image, skipping slots that fail to read or
    /// decode. `None` when every slot was tried.
    pub async fn next<S: Scope<Error = E>>(
        &mut self,
        reactor: &Reactor<S>,
        scope: &S,
    ) -> Option<(usize, T)> {
        while let Some(candidate) = self.slots.pop() {
            let slot = candidate.slot;
            match self.decode_one(reactor, candidate, scope).await {
                Ok(image) => return Some((slot, image)),
                Err(error) => self.skipped.push((slot, error)),
            }
        }
        None
    }

    async fn decode_one<S: Scope<Error = E>>(
        &self,
        reactor: &Reactor<S>,
        candidate: Candidate<HEADER>,
        scope: &S,
    ) -> Result<T, E> {
        if candidate.length < HEADER {
            return Err(Error::InvalidInput.into());
        }
        let mut bytes = Buffer::allocate(candidate.length)?;
        bytes.data[..HEADER].copy_from_slice(&candidate.header);
        let mut filled = HEADER;
        while filled < candidate.length {
            bytes.start = filled;
            bytes.end = candidate.length;
            let done = reactor
                .read_at(candidate.file.clone(), filled as u64, bytes, (), scope)
                .await?;
            let read = done.bytes;
            bytes = done.buffer;
            if read == 0 {
                return Err(Error::InvalidInput.into());
            }
            filled += read;
        }
        bytes.start = 0;
        // Never grow the allocation if the file grew after probing.
        let probe = Buffer::allocate(1)?;
        let done = reactor
            .read_at(candidate.file, candidate.length as u64, probe, (), scope)
            .await?;
        if done.bytes != 0 {
            return Err(Error::InvalidInput.into());
        }
        (self.decode)(&bytes.data[..candidate.length], self.budget).map_err(Into::into)
    }

    /// Slots still to decode.
    pub fn remaining(&self) -> usize {
        self.slots.len()
    }
}

#[cfg(test)]
mod tests {
    //! Slot alternation, lazy decoding, and simulated short reads.
    use super::*;
    use crate::scoped::fs::{
        fixtures::{Admission, Limits, Reactor, scope},
        test_support::drive,
    };
    use crate::sim::{Fault, SimOs};
    use std::path::Path;

    const SLOTS: Slots<8> = Slots {
        names: ["a.ckpt", "b.ckpt"],
        minimum_bytes: 8,
        maximum_bytes: 64,
    };

    fn reactor(sim: &SimOs) -> Reactor {
        Reactor::new(
            Rc::new(Admission::new(Limits {
                queue_entries: NonZeroUsize::new(8).unwrap(),
            })),
            sim,
        )
    }

    fn sequence(header: &[u8]) -> Result<u64> {
        Ok(u64::from_le_bytes(
            header.try_into().map_err(|_| Error::InvalidInput)?,
        ))
    }

    fn decode(bytes: &[u8], _: usize) -> Result<Vec<u8>> {
        if bytes[8..].starts_with(b"bad") {
            return Err(Error::InvalidInput);
        }
        Ok(bytes[8..].to_vec())
    }

    fn image(sequence: u64, body: &[u8]) -> Vec<u8> {
        let mut bytes = sequence.to_le_bytes().to_vec();
        bytes.extend_from_slice(body);
        bytes
    }

    /// Alternate slots, resume short reads, and keep a newer bad slot disposable.
    #[test]
    fn checkpoints_alternate_slots_and_decode_newest_first() {
        let sim = SimOs::new();
        let r = reactor(&sim);
        let request = scope();
        let dir = Path::new("/ckpt");
        let chunk = NonZeroUsize::new(5).unwrap();
        let mut none = drive(
            &r,
            Box::pin(SLOTS.candidates(&r, dir, 64, sequence, decode, &request)),
        )
        .unwrap();
        assert!(drive(&r, Box::pin(none.next(&r, &request))).is_none());
        assert_eq!(SLOTS.next_publication(None), Some((0, 1)));
        drive(
            &r,
            Box::pin(SLOTS.publish(&r, dir, 0, &image(1, b"one"), chunk, &request)),
        )
        .unwrap();
        let (slot, next) = SLOTS.next_publication(Some((0, 1))).unwrap();
        assert_eq!((slot, next), (1, 2));
        let mark = sim.trace().len();
        drive(
            &r,
            Box::pin(SLOTS.publish(&r, dir, slot, &image(next, b"two"), chunk, &request)),
        )
        .unwrap();
        // Publishing into an existing directory issues no fsync at all.
        let trace = sim.trace();
        assert!(trace[mark..].iter().any(|e| e.operation == "submit:rename"));
        assert!(!trace[mark..].iter().any(|e| e.operation == "submit:fsync"));
        let mut found = drive(
            &r,
            Box::pin(SLOTS.candidates(&r, dir, 64, sequence, decode, &request)),
        )
        .unwrap();
        // Short reads resume in place inside the single image buffer.
        sim.inject("read", Fault::Short(1)).unwrap();
        sim.inject("read", Fault::Short(1)).unwrap();
        assert_eq!(
            drive(&r, Box::pin(found.next(&r, &request))),
            Some((1, b"two".to_vec()))
        );
        assert_eq!(
            drive(&r, Box::pin(found.next(&r, &request))),
            Some((0, b"one".to_vec()))
        );
        assert!(drive(&r, Box::pin(found.next(&r, &request))).is_none());
        // A newer slot that fails to decode falls back to the older one.
        drive(
            &r,
            Box::pin(SLOTS.publish(&r, dir, 0, &image(3, b"bad"), chunk, &request)),
        )
        .unwrap();
        let mut fallback = drive(
            &r,
            Box::pin(SLOTS.candidates(&r, dir, 64, sequence, decode, &request)),
        )
        .unwrap();
        assert_eq!(
            drive(&r, Box::pin(fallback.next(&r, &request))),
            Some((1, b"two".to_vec()))
        );
        assert_eq!(fallback.skipped, [(0, Error::InvalidInput)]);
        // Oversized slots are skipped before allocation.
        let mut tight = drive(
            &r,
            Box::pin(SLOTS.candidates(&r, dir, 9, sequence, decode, &request)),
        )
        .unwrap();
        assert!(drive(&r, Box::pin(tight.next(&r, &request))).is_none());
        assert_eq!(tight.skipped.len(), 2);
        assert_eq!(SLOTS.next_publication(Some((2, 1))), None);
        assert_eq!(SLOTS.next_publication(Some((0, u64::MAX))), None);
    }
}
