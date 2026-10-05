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
            #[cfg(feature = "simulation")]
            if let Some(sim) = crate::reactor::simulation::Simulation::current() {
                return directory.join(format!(".checkpoint.{}.tmp", sim.next_sequence()));
            }
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
    #[cfg(feature = "simulation")]
    Sim(crate::reactor::simulation::Handle, u64),
}

impl CandidateFile {
    /// Reject symlink endpoints and prevent special files from blocking startup.
    fn open(path: &Path, directory: bool) -> io::Result<Self> {
        let flags =
            libc::O_NOFOLLOW | libc::O_NONBLOCK | if directory { libc::O_DIRECTORY } else { 0 };
        #[cfg(feature = "simulation")]
        if let Some(sim) = crate::reactor::simulation::Simulation::current() {
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

    /// Accept only regular checkpoint files before allocating any image buffer.
    fn length(&self) -> io::Result<u64> {
        let (regular, length) = match self {
            Self::Real(file) => {
                let m = file.metadata()?;
                (m.is_file(), m.len())
            }
            #[cfg(feature = "simulation")]
            Self::Sim(handle, _) => {
                let m = handle.stat()?;
                (
                    m.stx_mode as u32 & libc::S_IFMT == libc::S_IFREG,
                    m.stx_size,
                )
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
            #[cfg(feature = "simulation")]
            Self::Sim(handle, offset) => {
                let n = handle.file_read(*offset, bytes)?;
                *offset += n as u64;
                Ok(n)
            }
        }
    }
}

#[cfg(all(test, feature = "simulation"))]
mod tests {
    use super::*;
    use crate::reactor::simulation::{Fault, Simulation};

    const SLOTS: Slots<8> = Slots {
        names: ["a", "b"],
        minimum_bytes: 9,
        maximum_bytes: 64,
    };

    /// A deliberately tiny schema keeps filesystem tests independent of Racer.
    fn image(sequence: u64) -> Vec<u8> {
        let mut bytes = sequence.to_le_bytes().to_vec();
        bytes.push(42);
        bytes
    }

    /// Read the test schema's untrusted ordering hint.
    fn hint(bytes: &[u8]) -> io::Result<u64> {
        Ok(u64::from_le_bytes(bytes[..8].try_into().unwrap()))
    }

    /// Reject corrupt bodies and enforce an independent decoded memory budget.
    fn decode(bytes: &[u8], budget: usize) -> io::Result<u64> {
        if bytes != image(hint(bytes)?) || budget < 16 {
            return Err(io::Error::other("invalid test image"));
        }
        hint(bytes)
    }

    #[test]
    fn candidate_read_failure_tries_older_slot() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let path = Path::new("/recovery-budget-test");
        sim.write_file(&path.join("a"), &image(1)).unwrap();
        sim.write_file(&path.join("b"), &image(2)).unwrap();
        let mut scan = SLOTS.candidates(path, 64, hint, decode).unwrap();
        sim.inject("read", Fault::Errno(libc::EIO)).unwrap();
        assert_eq!(scan.next(), Some((0, 1)));
        assert_eq!(scan.next(), None);
    }

    #[test]
    fn publication_preserves_older_valid_slot_and_rejects_sequence_overflow() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let path = Path::new("/alternating");
        sim.write_file(&path.join("a"), &image(1)).unwrap();
        sim.write_file(&path.join("b"), b"torn").unwrap();
        let newest = SLOTS.candidates(path, 64, hint, decode).unwrap().next();
        let (slot, sequence) = SLOTS.next_publication(newest).unwrap();
        assert_eq!((slot, sequence), (1, 2));
        SLOTS.publish(path, slot, &image(sequence)).unwrap();
        assert_eq!(
            SLOTS
                .candidates(path, 64, hint, decode)
                .unwrap()
                .collect::<Vec<_>>(),
            vec![(1, 2), (0, 1)]
        );
        assert_eq!(SLOTS.next_publication(Some((1, u64::MAX))), None);
        assert_eq!(SLOTS.next_publication(None), Some((0, 1)));
        assert_eq!(SLOTS.next_publication(Some((2, 7))), None);
        assert!(SLOTS.publish(path, 2, &image(1)).is_err());
    }

    #[test]
    fn budgets_decode_failure_and_missing_directory_fall_back() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let path = Path::new("/budget");
        assert!(
            SLOTS
                .candidates(path, 64, hint, decode)
                .unwrap()
                .next()
                .is_none()
        );
        sim.write_file(&path.join("a"), &image(1)).unwrap();
        let mut corrupt = image(2);
        corrupt[8] = 0;
        sim.write_file(&path.join("b"), &corrupt).unwrap();
        assert_eq!(
            SLOTS.candidates(path, 64, hint, decode).unwrap().next(),
            Some((0, 1))
        );
        for budget in [0, 8, 9, 15] {
            assert!(
                SLOTS
                    .candidates(path, budget, hint, decode)
                    .unwrap()
                    .next()
                    .is_none()
            );
        }
        assert!(SLOTS.candidates(&path.join("a"), 64, hint, decode).is_err());
        let invalid = Slots::<8> {
            minimum_bytes: 7,
            ..SLOTS
        };
        assert!(invalid.candidates(path, 64, hint, decode).is_err());
    }

    #[test]
    fn growth_and_truncation_after_probe_never_expand_the_read_buffer() {
        // Cursor-backed candidates exercise the exact bounded read path without
        // relying on whether a simulated write replaces or mutates an inode.
        let sim = Simulation::new();
        let _environment = sim.enter();
        let path = Path::new("/changed");
        for body in [vec![42, 99], vec![]] {
            let mut changed = 2u64.to_le_bytes().to_vec();
            changed.extend_from_slice(&body);
            sim.write_file(&path.join("a"), &image(1)).unwrap();
            sim.write_file(&path.join("b"), &changed).unwrap();
            let mut file = CandidateFile::open(&path.join("b"), false).unwrap();
            let mut header = [0; 8];
            file.read_exact(&mut header).unwrap();
            let mut scan = SLOTS.candidates(path, 64, hint, decode).unwrap();
            scan.slots.retain(|candidate| candidate.slot == 0);
            scan.slots.push(Candidate {
                slot: 1,
                sequence: 2,
                file,
                length: 9,
                header,
            });
            assert_eq!(scan.next(), Some((0, 1)));
            assert_eq!(scan.next(), None);
        }
    }
}
