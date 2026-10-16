//! Schema-independent two-slot checkpoint hints with bounded reads and atomic publication.
//!
//! Callers serialize publication and decide whether a decoded image can be installed.
//! Slot failures are disposable; inaccessible directories are not. No payload scan or
//! fsync is performed. Directory trust and admission fencing remain caller policy.

use super::operations::{Durability, Replacement, ReplacementError};
use crate::reactor::Reactor;
use crate::{Budget, Error, Scope};
use std::fs::OpenOptions;
use std::io::{self, Read};
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;
use std::sync::atomic::{AtomicU64, Ordering};
use std::{ffi::CString, num::NonZeroUsize, os::unix::ffi::OsStrExt};

static TEMP_SEQUENCE: AtomicU64 = AtomicU64::new(0);

/// Schema-provided names and encoded length limits for two alternating slots.
pub struct Slots<const HEADER: usize> {
    /// Fixed single-component slot names, distinct from the temporary prefix.
    pub names: [&'static str; 2],

    /// Minimum complete encoded image length, including the header.
    pub minimum_bytes: usize,

    /// Maximum encoded length accepted before allocating a read buffer.
    pub maximum_bytes: usize,
}

/// A probed slot retains its file descriptor and header until bounded decoding.
struct Candidate<const HEADER: usize> {
    slot: usize,

    sequence: u64,

    file: CandidateFile,

    length: usize,

    header: [u8; HEADER],
}

/// Newest-first lazy decoding; discard rejected images before requesting another.
pub struct Candidates<T, const HEADER: usize> {
    slots: Vec<Candidate<HEADER>>,

    budget: usize,

    decode: fn(&[u8], usize) -> io::Result<T>,
}

impl<const HEADER: usize> Slots<HEADER> {
    /// Probe fixed headers only, ordering by an untrusted schema sequence hint.
    pub fn candidates<T>(
        &self,
        directory: &Path,
        budget: usize,
        sequence: fn(&[u8]) -> io::Result<u64>,
        decode: fn(&[u8], usize) -> io::Result<T>,
    ) -> io::Result<Candidates<T, HEADER>> {
        self.validate()?;
        let mut candidates = Candidates {
            slots: Vec::with_capacity(2),
            budget,
            decode,
        };
        match CandidateFile::open(directory, true) {
            Ok(_) => (),
            Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(candidates),
            Err(e) => return Err(e),
        }
        for (slot, name) in self.names.iter().enumerate() {
            let result = (|| -> io::Result<_> {
                let mut file = CandidateFile::open(&directory.join(name), false)?;
                let length = file.length()?;
                if length < self.minimum_bytes as u64
                    || length > self.maximum_bytes.min(budget) as u64
                {
                    return Err(io::Error::other(
                        "checkpoint encoded size exceeds budget or format limit",
                    ));
                }
                let mut header = [0; HEADER];
                file.read_exact(&mut header)?;
                Ok(Candidate {
                    slot,
                    sequence: sequence(&header)?,
                    file,
                    length: length as usize,
                    header,
                })
            })();
            match result {
                Ok(candidate) => candidates.slots.push(candidate),
                Err(e) if e.kind() == io::ErrorKind::NotFound => (),
                Err(e) => eprintln!("skipping checkpoint slot {slot}: {e}"),
            }
        }
        candidates.slots.sort_by_key(|candidate| candidate.sequence);
        Ok(candidates)
    }

    /// Choose the opposite slot and checked successor of the newest valid image.
    pub fn next_publication(&self, newest: Option<(usize, u64)>) -> Option<(usize, u64)> {
        match newest {
            Some((slot, sequence)) if slot < 2 => Some((1 - slot, sequence.checked_add(1)?)),
            Some(_) => None,
            None => Some((0, 1)),
        }
    }

    /// Publish through a fresh stage, retrying stale names without replacing them.
    /// The caller must serialize publications, including after process restarts.
    pub fn publish(&self, directory: &Path, slot: usize, bytes: &[u8]) -> io::Result<()> {
        self.validate()?;
        let name = self
            .names
            .get(slot)
            .ok_or_else(|| io::Error::other("invalid checkpoint slot"))?;
        let candidates = (0..128).map(|_| {
            // TODO(sim): use the simulated world's deterministic sequence.
            let sequence = TEMP_SEQUENCE.fetch_add(1, Ordering::Relaxed);
            directory.join(format!(".checkpoint.{}.{sequence}.tmp", std::process::id()))
        });
        super::operations::publish_new(directory, &directory.join(name), bytes, candidates)
    }

    /// Publish a periodic hint using bounded reactor scratch and no fsync.
    /// The caller serializes this fixed stage and fences abandoned operations
    /// before retrying. Cancellation can leave a stage or an uncertain rename.
    pub async fn publish_async<S: Scope, B: Budget>(
        &self,
        reactor: &Reactor<S, B>,
        directory: &Path,
        slot: usize,
        bytes: &[u8],
        chunk: NonZeroUsize,
        scope: &S,
    ) -> Result<(), ReplacementError<S::Error>>
    where
        S::Error: PartialEq,
    {
        let before = ReplacementError::BeforeRename;
        self.validate()
            .map_err(|_| before(Error::InvalidConfiguration.into()))?;
        let name = self
            .names
            .get(slot)
            .ok_or_else(|| before(Error::InvalidInput.into()))?;
        let directory = CString::new(directory.as_os_str().as_bytes())
            .map_err(|_| before(Error::InvalidConfiguration.into()))?;
        let dir = reactor
            .file_open(
                None,
                directory,
                libc::O_RDONLY | libc::O_DIRECTORY,
                0,
                scope,
            )
            .await
            .map_err(before)?;
        let temporary = CString::new(".checkpoint.periodic.stage").unwrap();
        match reactor
            .file_unlink(dir.clone(), temporary.clone(), scope)
            .await
        {
            Ok(()) => (),
            Err(error) if error == Error::NotFound.into() => (),
            Err(error) => return Err(before(error)),
        }
        let staged = reactor
            .file_open(
                Some(dir.clone()),
                temporary.clone(),
                libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL,
                0,
                scope,
            )
            .await
            .map_err(before)?;
        reactor
            .file_replace_chunked(
                Replacement {
                    directory: dir,
                    staged,
                    temporary,
                    target: CString::new(*name).unwrap(),
                    durability: Durability::Publish,
                },
                bytes,
                chunk,
                scope,
            )
            .await
    }

    /// Reject malformed schemas before touching the namespace or slicing buffers.
    fn validate(&self) -> io::Result<()> {
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
            return Err(io::Error::other("invalid checkpoint slot configuration"));
        }
        Ok(())
    }
}

impl<T, const HEADER: usize> Iterator for Candidates<T, HEADER> {
    type Item = (usize, T);

    /// Decode one image at a time, falling back after read or schema failures.
    fn next(&mut self) -> Option<Self::Item> {
        while let Some(candidate) = self.slots.pop() {
            let Candidate {
                slot,
                mut file,
                length,
                header,
                ..
            } = candidate;
            let result = (|| {
                let mut bytes = Vec::new();
                bytes.try_reserve_exact(length).map_err(io::Error::other)?;
                bytes.resize(length, 0);
                bytes[..HEADER].copy_from_slice(&header);
                file.read_exact(&mut bytes[HEADER..])?;
                // Never grow the allocation if a concurrently changed file grows.
                if file.read(&mut [0])? != 0 {
                    return Err(io::Error::other("checkpoint grew after probing"));
                }
                (self.decode)(&bytes, self.budget)
            })();
            match result {
                Ok(image) => return Some((slot, image)),
                Err(e) => eprintln!("skipping checkpoint slot {slot} during read/decode: {e}"),
            }
        }
        None
    }
}

/// Real or simulated file with a bounded sequential cursor.
enum CandidateFile {
    Real(std::fs::File),
    // TODO(sim): `Sim(SimFd, offset)` for simulated worlds.
}

impl CandidateFile {
    /// Reject symlink endpoints and prevent special files from blocking startup.
    fn open(path: &Path, directory: bool) -> io::Result<Self> {
        let flags =
            libc::O_NOFOLLOW | libc::O_NONBLOCK | if directory { libc::O_DIRECTORY } else { 0 };
        OpenOptions::new()
            .read(true)
            .custom_flags(flags)
            .open(path)
            .map(Self::Real)
    }

    /// Accept only regular checkpoint files before allocating any image buffer.
    fn length(&self) -> io::Result<u64> {
        let (regular, length) = match self {
            Self::Real(file) => {
                let m = file.metadata()?;
                (m.is_file(), m.len())
            }
        };
        if !regular {
            return Err(io::Error::other("checkpoint is not a regular file"));
        }
        Ok(length)
    }
}

impl Read for CandidateFile {
    /// Advance only by bytes actually read, identically in simulation and production.
    fn read(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        match self {
            Self::Real(file) => file.read(bytes),
        }
    }
}
