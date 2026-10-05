//! Staged replacement and publication, with file policy supplied by the caller.
use super::*;
use std::ffi::OsStr;

/// The directory must be pinned, trusted, and exclusively controlled throughout
/// this operation. Both names must be distinct single components. The stage must
/// be an empty, singly linked regular file. A verified, nonappend description is
/// reopened for writes, so an O_APPEND description supplied here is never used.
/// Callers serialize access to the stage inode and directory, including via aliases.
pub struct Replacement {
    /// Trusted parent directory pinned throughout validation and publication.
    pub directory: Rc<Descriptor>,
    /// Fresh, empty, single-link stage used to verify the reopened inode.
    pub staged: Rc<Descriptor>,
    /// Stage name as one component within the pinned directory.
    pub temporary: CString,
    /// Distinct destination component replaced atomically by rename.
    pub target: CString,
    /// Requested file and namespace durability fences.
    pub durability: Durability,
}

/// Where publication stopped. An accepted rename may execute even if cancellation
/// wins its completion race. Dropping the future likewise provides no publication
/// outcome; callers must fence outstanding I/O and reconcile the namespace.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ReplacementError<E> {
    /// Validation, writing, or staged-file sync failed before attempting rename.
    BeforeRename(E),
    /// Rename failed or was canceled; reconcile the namespace after fencing.
    RenameUncertain(E),
    /// Rename completed successfully; only the parent durability fence failed.
    Published(E),
}
impl<E: Copy> ReplacementError<E> {
    /// Return the underlying failure without discarding the publication phase.
    pub fn cause(&self) -> E {
        match *self {
            Self::BeforeRename(e) | Self::RenameUncertain(e) | Self::Published(e) => e,
        }
    }
}

/// Explicit fences after writing and after renaming. Stage cleanup/creation
/// fences, when required, must precede this operation and remain caller-owned.
#[derive(Clone, Copy, Debug)]
pub enum Durability {
    /// Namespace publication only; no crash durability is promised.
    Publish,
    /// Sync the staged file before rename, then sync its parent directory.
    FileAndDirectory,
}

impl<S: Scope, B: Budget> Reactor<S, B> {
    /// Write a fresh empty stage, then atomically rename it. Reused/nonempty stages
    /// are rejected, including for empty input. Failure cleanup remains caller-owned.
    pub fn file_replace<'a>(
        &'a self,
        replacement: Replacement,
        buffer: Buffer,
        scope: &'a S,
    ) -> Operation<'a, (), ReplacementError<S::Error>> {
        Box::pin(async move {
            let staged = self
                .prepare_replacement(&replacement, scope)
                .await
                .map_err(ReplacementError::BeforeRename)?;
            self.write_complete(staged.clone(), buffer, &mut 0, scope)
                .await
                .map_err(ReplacementError::BeforeRename)?;
            self.publish_replacement(replacement, staged, scope).await
        })
    }

    /// Write a fresh stage with reusable, bounded scratch, then publish it.
    /// The input may exceed the scratch limit. Stage creation and failure cleanup
    /// remain caller-owned, just as for [`Self::file_replace`].
    pub fn file_replace_chunked<'a>(
        &'a self,
        replacement: Replacement,
        bytes: &'a [u8],
        chunk: NonZeroUsize,
        scope: &'a S,
    ) -> Operation<'a, (), ReplacementError<S::Error>> {
        Box::pin(async move {
            let staged = self
                .prepare_replacement(&replacement, scope)
                .await
                .map_err(ReplacementError::BeforeRename)?;
            let mut offset = 0u64;
            let mut buffer = self
                .file_buffer(bytes.len().min(chunk.get()))
                .map_err(|e| ReplacementError::BeforeRename(e.into()))?;
            for bytes in bytes.chunks(chunk.get()) {
                buffer.start = 0;
                buffer.end = bytes.len();
                buffer.data[..bytes.len()].copy_from_slice(bytes);
                buffer = self
                    .write_complete(staged.clone(), buffer, &mut offset, scope)
                    .await
                    .map_err(ReplacementError::BeforeRename)?;
            }
            self.publish_replacement(replacement, staged, scope).await
        })
    }

    /// Verify a fresh single-link stage and reopen its exact inode without append.
    async fn prepare_replacement(
        &self,
        replacement: &Replacement,
        scope: &S,
    ) -> Result<Rc<Descriptor>, S::Error> {
        for name in [&replacement.temporary, &replacement.target] {
            secure::validate_component(OsStr::from_bytes(name.as_bytes()), PATH_BYTES)?;
        }
        if replacement.temporary == replacement.target {
            return Err(Error::InvalidInput.into());
        }
        let original = self.file_stat(replacement.staged.clone(), scope).await?;
        secure::check_regular_size(&original, 0)?;
        let required = libc::STATX_INO | libc::STATX_NLINK;
        if original.stx_mask & required != required || original.stx_nlink != 1 {
            return Err(Error::InvalidInput.into());
        }
        let staged = self
            .file_open(
                Some(replacement.directory.clone()),
                replacement.temporary.clone(),
                libc::O_WRONLY,
                secure::BENEATH | secure::NO_SYMLINKS,
                scope,
            )
            .await?;
        let current = self.file_stat(staged.clone(), scope).await?;
        secure::check_regular_size(&current, 0)?;
        if current.stx_mask & required != required
            || current.stx_nlink != 1
            || current.stx_ino != original.stx_ino
            || current.stx_dev_major != original.stx_dev_major
            || current.stx_dev_minor != original.stx_dev_minor
        {
            return Err(Error::InvalidInput.into());
        }
        Ok(staged)
    }

    /// Consume short writes while retaining the same scratch allocation.
    async fn write_complete(
        &self,
        staged: Rc<Descriptor>,
        mut buffer: Buffer,
        offset: &mut u64,
        scope: &S,
    ) -> Result<Buffer, S::Error> {
        while buffer.remaining() != 0 {
            let completion = self
                .write_at(staged.clone(), *offset, buffer, (), scope)
                .await?;
            buffer = completion.buffer;
            buffer.advance(completion.bytes)?;
            *offset += completion.bytes as u64;
        }
        Ok(buffer)
    }

    /// Apply the selected durability fences and preserve the failed publication phase.
    async fn publish_replacement(
        &self,
        replacement: Replacement,
        staged: Rc<Descriptor>,
        scope: &S,
    ) -> Result<(), ReplacementError<S::Error>> {
        if matches!(replacement.durability, Durability::FileAndDirectory) {
            self.file_sync(staged.clone(), scope)
                .await
                .map_err(ReplacementError::BeforeRename)?;
        }
        self.file_rename(
            replacement.directory.clone(),
            replacement.temporary,
            replacement.target,
            scope,
        )
        .await
        .map_err(ReplacementError::RenameUncertain)?;
        if matches!(replacement.durability, Durability::FileAndDirectory) {
            self.file_sync(replacement.directory, scope)
                .await
                .map_err(ReplacementError::Published)?;
        }
        Ok(())
    }
}

/// Blocking namespace-only replacement; never call on latency-sensitive workers.
/// No fsync is issued. New directories use 0700 and stages 0600, reduced by host
/// umask (simulation models umask 0). Existing permissions remain unchanged.
/// Target and candidates must be direct children of `directory`; parent symlinks
/// and `..` are rejected. The caller supplies finite candidates and exclusively
/// controls the directory during publication and best-effort failure cleanup.
pub fn publish_new(
    directory: &std::path::Path,
    target: &std::path::Path,
    bytes: &[u8],
    candidates: impl IntoIterator<Item = PathBuf>,
) -> std::io::Result<()> {
    use publication::*;
    use std::io;
    validate_directory(directory)?;
    let target = child(directory, target)?;
    let candidates: Vec<CString> = candidates
        .into_iter()
        .map(|p| {
            let name = child(directory, &p)?;
            if name == target {
                return Err(invalid());
            }
            Ok(name)
        })
        .collect::<io::Result<_>>()?;
    let directory = Directory::open(directory)?;
    for temporary in candidates {
        let file = match directory.create(&temporary) {
            Ok(file) => file,
            Err(error) if error.kind() == io::ErrorKind::AlreadyExists => continue,
            Err(error) => return Err(error),
        };
        let result = (|| {
            let mut offset = 0;
            while offset < bytes.len() {
                match write(&file, offset, &bytes[offset..]) {
                    Ok(0) => {
                        return Err(io::Error::new(io::ErrorKind::WriteZero, "zero stage write"));
                    }
                    Ok(n) => offset += n,
                    Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                    Err(e) => return Err(e),
                }
            }
            directory.rename(&temporary, &target)
        })();
        if result.is_err() {
            let _ = directory.unlink(&temporary);
        }
        return result;
    }
    Err(io::Error::new(
        io::ErrorKind::AlreadyExists,
        "stage candidates exhausted",
    ))
}

mod publication {
    //! Synchronous backend boundary for private, descriptor-relative publication.
    use super::*;
    use std::{
        io,
        path::{Component, Path},
    };

    /// Pinned directory shared by validation, creation, publication and cleanup.
    pub(super) struct Directory {
        fd: Descriptor,
        #[cfg(feature = "simulation")]
        simulated: Option<(simulation::Simulation, PathBuf)>,
    }

    impl Directory {
        /// Walk and pin private directories without traversing parent symlinks.
        pub(super) fn open(path: &Path) -> io::Result<Self> {
            #[cfg(feature = "simulation")]
            if let Some(sim) = simulation::Simulation::current() {
                let mut current = PathBuf::from(if path.is_absolute() { "/" } else { "." });
                let mut fd = sim.open(
                    None,
                    &current,
                    libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW,
                )?;
                for part in path.components() {
                    if let Component::Normal(part) = part {
                        current.push(part);
                        match sim.metadata(&current) {
                            Ok(_) => (),
                            Err(error) if error.kind() == io::ErrorKind::NotFound => {
                                sim.create_dir_all(&current)?;
                                sim.chmod(&current, 0o700)?;
                            }
                            Err(error) => return Err(error),
                        }
                        fd = sim.open(
                            Some(&fd),
                            Path::new(part),
                            libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW,
                        )?;
                    }
                }
                return Ok(Self {
                    fd,
                    simulated: Some((sim, current)),
                });
            }
            let mut fd = host_open(
                libc::AT_FDCWD,
                if path.is_absolute() { c"/" } else { c"." },
                libc::O_RDONLY | libc::O_DIRECTORY,
                0,
            )?;
            for part in path.components() {
                if let Component::Normal(part) = part {
                    let name = secure::component(part, PATH_BYTES).map_err(|_| invalid())?;
                    // SAFETY: live pinned parent and NUL-terminated name.
                    if unsafe { libc::mkdirat(fd.as_raw_fd(), name.as_ptr(), 0o700) } < 0 {
                        let error = io::Error::last_os_error();
                        if error.raw_os_error() != Some(libc::EEXIST) {
                            return Err(error);
                        }
                    }
                    fd = host_open(fd.as_raw_fd(), &name, libc::O_RDONLY | libc::O_DIRECTORY, 0)?;
                }
            }
            Ok(Self {
                fd,
                #[cfg(feature = "simulation")]
                simulated: None,
            })
        }

        /// Exclusively create a private stage in the pinned directory.
        pub(super) fn create(&self, name: &CString) -> io::Result<Descriptor> {
            let flags =
                libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL | libc::O_CLOEXEC | libc::O_NOFOLLOW;
            #[cfg(feature = "simulation")]
            if let Some((sim, _)) = &self.simulated {
                return sim.open(
                    Some(&self.fd),
                    Path::new(OsStr::from_bytes(name.as_bytes())),
                    flags,
                );
            }
            host_open(self.fd.as_raw_fd(), name, flags, 0o600)
        }

        /// Publish a stage without an implicit durability fence.
        pub(super) fn rename(&self, from: &CString, to: &CString) -> io::Result<()> {
            #[cfg(feature = "simulation")]
            if let Some((sim, directory)) = &self.simulated {
                return sim.rename(
                    &directory.join(OsStr::from_bytes(from.as_bytes())),
                    &directory.join(OsStr::from_bytes(to.as_bytes())),
                    0,
                );
            }
            // SAFETY: names and pinned parent stay live for this syscall.
            status(unsafe {
                libc::renameat(
                    self.fd.as_raw_fd(),
                    from.as_ptr(),
                    self.fd.as_raw_fd(),
                    to.as_ptr(),
                )
            })
        }

        /// Remove a failed stage without changing the destination.
        pub(super) fn unlink(&self, name: &CString) -> io::Result<()> {
            #[cfg(feature = "simulation")]
            if let Some((sim, directory)) = &self.simulated {
                return sim.unlink(&directory.join(OsStr::from_bytes(name.as_bytes())));
            }
            // SAFETY: name and pinned parent stay live for this syscall.
            status(unsafe { libc::unlinkat(self.fd.as_raw_fd(), name.as_ptr(), 0) })
        }
    }

    /// Report invalid publication input with the host syscall error convention.
    pub(super) fn invalid() -> io::Error {
        io::Error::from_raw_os_error(libc::EINVAL)
    }

    /// Reject lexical escapes before creating any directory or file.
    pub(super) fn validate_directory(path: &Path) -> io::Result<()> {
        secure::path_name(path.as_os_str(), PATH_BYTES).map_err(|_| invalid())?;
        if path
            .components()
            .any(|p| matches!(p, Component::ParentDir | Component::Prefix(_)))
        {
            return Err(invalid());
        }
        Ok(())
    }

    /// Extract exactly one normal child component, without normalization.
    pub(super) fn child(directory: &Path, path: &Path) -> io::Result<CString> {
        let relative = path.strip_prefix(directory).map_err(|_| invalid())?;
        secure::component(relative.as_os_str(), PATH_BYTES).map_err(|_| invalid())
    }

    /// Convert a syscall status while retaining its errno.
    fn status(value: i32) -> io::Result<()> {
        if value < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(())
        }
    }

    /// Acquire a uniquely owned close-on-exec, no-follow host descriptor.
    fn host_open(
        dir: i32,
        name: &std::ffi::CStr,
        flags: i32,
        mode: libc::mode_t,
    ) -> io::Result<Descriptor> {
        // SAFETY: NUL-terminated name; successful descriptor is immediately owned.
        let fd = unsafe {
            libc::openat(
                dir,
                name.as_ptr(),
                flags | libc::O_CLOEXEC | libc::O_NOFOLLOW,
                mode,
            )
        };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(unsafe { Descriptor::from_raw_fd(fd) })
    }

    /// Perform one positioned write and return short progress or the original error.
    pub(super) fn write(fd: &Descriptor, offset: usize, bytes: &[u8]) -> io::Result<usize> {
        #[cfg(feature = "simulation")]
        if let Some(handle) = fd.as_sim() {
            return handle.file_write(offset as u64, bytes);
        }
        let offset = libc::off_t::try_from(offset).map_err(|_| invalid())?;
        // SAFETY: descriptor and read-only bytes remain live throughout pwrite.
        let n = unsafe { libc::pwrite(fd.as_raw_fd(), bytes.as_ptr().cast(), bytes.len(), offset) };
        if n < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(n as usize)
        }
    }
}

#[cfg(all(test, feature = "simulation"))]
mod chunked_tests {
    //! Reused scratch and publication faults exercised through simulated completions.
    use super::*;
    use crate::reactor::{
        filesystem::{
            publish_new,
            test_support::{drive, poll},
        },
        simulation::{Fault, Simulation},
        tests::{
            fixtures::{Admission, Limits, Reactor},
            scope,
        },
    };
    use std::path::{Path, PathBuf};

    /// Prepare a fresh private stage with namespace-only durability.
    fn replacement(r: &Reactor) -> Replacement {
        let open = |path, flags| {
            drive(
                r,
                r.file_open(None, CString::new(path).unwrap(), flags, 0, &scope()),
            )
            .unwrap()
        };
        Replacement {
            directory: open("/", libc::O_RDONLY | libc::O_DIRECTORY),
            staged: open("/stage", libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL),
            temporary: CString::new("stage").unwrap(),
            target: CString::new("target").unwrap(),
            durability: Durability::Publish,
        }
    }

    #[test]
    fn chunked_publication_handles_short_writes_empty_and_non_durable_crash() {
        for bytes in [vec![], vec![7; 1024 * 1024 + 3]] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = Reactor::new(Rc::new(Admission::new(Limits {
                queue_entries: NonZeroUsize::new(8).unwrap(),
            })));
            sim.write_file(Path::new("/target"), b"old").unwrap();
            sim.disk().sync_all().unwrap();
            sim.set_max_chunk(8191).unwrap();
            drive(
                &r,
                r.file_replace_chunked(
                    replacement(&r),
                    &bytes,
                    NonZeroUsize::new(16384).unwrap(),
                    &scope(),
                ),
            )
            .unwrap();
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), bytes);
            if !bytes.is_empty() {
                let writes: Vec<_> = sim
                    .trace()
                    .into_iter()
                    .filter(|e| e.operation == "complete:write")
                    .map(|e| e.result)
                    .collect();
                assert!(writes.contains(&8191), "no actual short completion");
                assert!(writes.iter().all(|&n| n > 0 && n <= 8191));
            }
            sim.disk().crash().unwrap();
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"old");
        }
    }

    #[test]
    fn chunked_failures_and_abandonment_leave_target_and_fence_stage_owners() {
        for fault in ["write", "zero", "rename", "cancel", "abandon"] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = Reactor::new(Rc::new(Admission::new(Limits {
                queue_entries: NonZeroUsize::new(8).unwrap(),
            })));
            let request = scope();
            sim.write_file(Path::new("/target"), b"old").unwrap();
            let replacement = replacement(&r);
            let weak = Rc::downgrade(&replacement.staged);
            let mut future = r.file_replace_chunked(
                replacement,
                b"new bytes",
                NonZeroUsize::new(3).unwrap(),
                &request,
            );
            match fault {
                "abandon" => {
                    sim.inject("write", Fault::Delay(10)).unwrap();
                    for _ in 0..100 {
                        assert!(poll(&mut future).is_pending());
                        if sim.trace().iter().any(|e| e.operation == "submit:write") {
                            break;
                        }
                        r.poll_budgeted(8).unwrap();
                        r.wait(Duration::from_millis(1)).unwrap();
                    }
                    assert!(sim.trace().iter().any(|e| e.operation == "submit:write"));
                    drop(future);
                    // The securely reopened FD and scratch remain in the SQE,
                    // not the original stage description.
                    assert!(weak.upgrade().is_none());
                    assert_eq!(r.in_flight(), 1);
                    drive(&r, r.file_fence(())).unwrap();
                    assert!(weak.upgrade().is_none());
                    assert_eq!(r.in_flight(), 0);
                }
                "cancel" => {
                    request.cancel().unwrap();
                    assert_eq!(
                        drive(&r, future).map_err(|e| e.cause()),
                        Err(Error::Cancelled)
                    );
                }
                fault => {
                    sim.inject(
                        if fault == "zero" { "write" } else { fault },
                        if fault == "zero" {
                            Fault::Short(0)
                        } else {
                            Fault::Errno(libc::EIO)
                        },
                    )
                    .unwrap();
                    assert_eq!(
                        drive(&r, future).map_err(|e| e.cause()),
                        Err(if fault == "zero" {
                            Error::Io
                        } else {
                            Error::Os(libc::EIO)
                        })
                    );
                }
            }
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"old");
        }
    }

    #[test]
    fn synchronous_publication_skips_collisions_cleans_failure_and_has_no_fsync() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let directory = Path::new("/");
        let target = Path::new("/target");
        sim.write_file(target, b"old").unwrap();
        sim.write_file(Path::new("/stale"), b"keep").unwrap();
        sim.disk().sync_all().unwrap();
        assert!(publish_new(directory, target, b"new", [PathBuf::from("/stale")]).is_err());
        sim.inject("write", Fault::Short(0)).unwrap();
        assert!(publish_new(directory, target, b"new", [PathBuf::from("/failed")]).is_err());
        assert!(sim.metadata(Path::new("/failed")).is_err());
        sim.inject("rename", Fault::Errno(libc::EIO)).unwrap();
        assert!(publish_new(directory, target, b"new", [PathBuf::from("/failed")]).is_err());
        assert!(sim.metadata(Path::new("/failed")).is_err());
        assert_eq!(sim.read_file(target).unwrap(), b"old");
        sim.set_max_chunk(2).unwrap();
        publish_new(
            directory,
            target,
            b"replacement",
            [PathBuf::from("/stale"), PathBuf::from("/stage")],
        )
        .unwrap();
        assert_eq!(sim.read_file(Path::new("/stale")).unwrap(), b"keep");
        assert_eq!(sim.read_file(target).unwrap(), b"replacement");
        sim.disk().crash().unwrap();
        assert_eq!(sim.read_file(target).unwrap(), b"old");
    }

    #[test]
    fn synchronous_simulation_uses_private_modes_and_validates_before_creation() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let directory = Path::new("/private/child");
        let target = directory.join("target");
        assert!(
            publish_new(
                directory,
                &target,
                b"secret",
                [directory.join("../outside")]
            )
            .is_err()
        );
        assert!(sim.metadata(Path::new("/private")).is_err());
        publish_new(directory, &target, b"secret", [directory.join("stage")]).unwrap();
        let r = Reactor::new(Rc::new(Admission::new(Limits {
            queue_entries: NonZeroUsize::new(8).unwrap(),
        })));
        for (path, mode) in [
            (Path::new("/private"), 0o700),
            (directory, 0o700),
            (target.as_path(), 0o600),
        ] {
            let fd = Rc::new(sim.open(None, path, libc::O_RDONLY).unwrap());
            let stat = drive(&r, r.file_stat(fd, &scope())).unwrap();
            assert_eq!(stat.stx_mode & 0o777, mode);
        }
        sim.symlink(directory, Path::new("/link")).unwrap();
        assert!(
            publish_new(
                Path::new("/link"),
                Path::new("/link/target"),
                b"bad",
                [PathBuf::from("/link/stage")]
            )
            .is_err()
        );
        assert_eq!(sim.read_file(&target).unwrap(), b"secret");
    }
}

#[cfg(all(test, feature = "simulation"))]
mod tests {
    //! Bounded output ownership and staged publication phase regressions.
    use super::*;
    use crate::reactor::{
        filesystem::test_support::{drive, poll},
        simulation::{Fault, Simulation},
        tests::{
            fixtures::{Admission, Limits, Reactor},
            scope,
        },
    };
    use std::path::Path;

    /// Construct an eight-entry counting reactor in the current environment.
    fn reactor() -> Reactor {
        Reactor::new(Rc::new(Admission::new(Limits {
            queue_entries: NonZeroUsize::new(8).unwrap(),
        })))
    }

    /// Open one test pathname through the same completion path as production.
    fn open(r: &Reactor, path: &str, flags: i32) -> Rc<Descriptor> {
        drive(
            r,
            r.file_open(None, CString::new(path).unwrap(), flags, 0, &scope()),
        )
        .unwrap()
    }

    #[test]
    fn bounded_reads_cover_empty_exact_short_growth_and_errors() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let request = scope();
        sim.write_file(Path::new("/data"), b"abcdef").unwrap();
        let fd = open(&r, "/data", libc::O_RDONLY);
        sim.set_max_chunk(2).unwrap();
        let read =
            |limit| r.file_read_bounded(fd.clone(), limit, NonZeroUsize::new(3).unwrap(), &request);
        assert_eq!(&*drive(&r, read(6)).unwrap(), b"abcdef");
        let reads: Vec<_> = sim
            .trace()
            .into_iter()
            .filter(|e| e.operation == "complete:read")
            .map(|e| e.result)
            .collect();
        assert!(reads.contains(&2), "missing short read CQE: {reads:?}");
        assert!(
            reads.iter().all(|&n| n <= 2),
            "oversized read CQE: {reads:?}"
        );
        assert!(matches!(drive(&r, read(5)), Err(Error::Overloaded)));
        assert!(matches!(drive(&r, read(0)), Err(Error::Overloaded)));
        assert!(matches!(
            drive(&r, read(usize::MAX)),
            Err(Error::InvalidInput)
        ));
        sim.inject("read", Fault::Errno(libc::EIO)).unwrap();
        assert!(matches!(drive(&r, read(6)), Err(Error::Os(libc::EIO))));
        sim.write_file(Path::new("/empty"), b"").unwrap();
        assert!(
            drive(
                &r,
                r.file_read_bounded(
                    open(&r, "/empty", libc::O_RDONLY),
                    0,
                    NonZeroUsize::new(1).unwrap(),
                    &request
                )
            )
            .unwrap()
            .is_empty()
        );
        request.cancel().unwrap();
        assert!(matches!(drive(&r, read(6)), Err(Error::Cancelled)));
    }

    /// Create a fresh stage and pinned parent using the selected durability policy.
    fn replacement(r: &Reactor, durability: Durability) -> Replacement {
        Replacement {
            directory: open(r, "/", libc::O_RDONLY | libc::O_DIRECTORY),
            staged: open(r, "/stage", libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL),
            temporary: CString::new("stage").unwrap(),
            target: CString::new("target").unwrap(),
            durability,
        }
    }

    #[test]
    fn replacement_durability_is_explicit_and_short_writes_are_complete() {
        for durability in [Durability::Publish, Durability::FileAndDirectory] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let request = scope();
            sim.write_file(Path::new("/target"), b"old").unwrap();
            sim.disk().sync_all().unwrap();
            sim.set_max_chunk(2).unwrap();
            let replacement = replacement(&r, durability);
            drive(
                &r,
                r.file_replace(replacement, r.file_bytes(b"replacement").unwrap(), &request),
            )
            .unwrap();
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"replacement");
            let writes: Vec<_> = sim
                .trace()
                .into_iter()
                .filter(|e| e.operation == "complete:write")
                .map(|e| e.result)
                .collect();
            assert!(
                writes.len() > 1 && writes.iter().all(|&n| n > 0 && n <= 2),
                "expected short CQEs: {writes:?}"
            );
            sim.disk().crash().unwrap();
            assert_eq!(
                sim.read_file(Path::new("/target")).unwrap(),
                match durability {
                    Durability::Publish => b"old".as_slice(),
                    Durability::FileAndDirectory => b"replacement".as_slice(),
                }
            );
        }
    }

    #[test]
    fn replacement_rejects_nested_names_and_nonempty_or_mismatched_stages() {
        for case in [
            "nested-target",
            "nested-stage",
            "same",
            "reused",
            "empty-reused",
            "mismatched",
        ] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let mut replacement = replacement(&r, Durability::FileAndDirectory);
            sim.write_file(Path::new("/target"), b"old").unwrap();
            match case {
                "nested-target" => replacement.target = CString::new("child/target").unwrap(),
                "nested-stage" => replacement.temporary = CString::new("child/stage").unwrap(),
                "same" => replacement.target = replacement.temporary.clone(),
                "mismatched" => {
                    replacement.staged =
                        open(&r, "/other", libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL)
                }
                _ => {
                    sim.write_file(Path::new("/stage"), b"stale suffix")
                        .unwrap();
                }
            }
            let n = sim.trace().len();
            let result = drive(
                &r,
                r.file_replace(
                    replacement,
                    r.file_bytes(if case == "empty-reused" { b"" } else { b"new" })
                        .unwrap(),
                    &scope(),
                ),
            );
            assert_eq!(
                result,
                Err(ReplacementError::BeforeRename(Error::InvalidInput)),
                "{case}"
            );
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"old");
            assert!(!sim.trace()[n..].iter().any(|e| matches!(
                e.operation.as_str(),
                "submit:write" | "submit:rename" | "submit:fsync"
            )));
        }
    }

    #[test]
    fn read_output_owns_budget_and_prefix_uses_current_cursor() {
        use crate::reactor::tests::fixtures::ResourceClass;
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let mut buffer = r.file_bytes(b"abcdef").unwrap();
        buffer.advance(2).unwrap();
        assert_eq!(buffer.prefix(2).unwrap(), b"cd");
        assert_eq!(buffer.prefix(5), Err(Error::Io));
        drop(buffer);
        sim.write_file(Path::new("/data"), b"abc").unwrap();
        r.init().unwrap();
        let baseline = r.admission.used(ResourceClass::RequestContext);
        let out = drive(
            &r,
            r.file_read_bounded(
                open(&r, "/data", libc::O_RDONLY),
                100,
                NonZeroUsize::new(2).unwrap(),
                &scope(),
            ),
        )
        .unwrap();
        assert_eq!(&*out, b"abc");
        assert_eq!(
            r.admission.used(ResourceClass::RequestContext),
            baseline + 100
        );
        drop(out);
        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
    }

    #[test]
    fn post_rename_sync_failure_and_held_cancellation_report_publication_phase() {
        for cancel in [false, true] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let request = scope();
            sim.write_file(Path::new("/target"), b"old").unwrap();
            let replacement = replacement(&r, Durability::FileAndDirectory);
            let weak = Rc::downgrade(&replacement.directory);
            sim.inject("rename", Fault::HoldCompletion(20)).unwrap();
            let mut future = r.file_replace(replacement, r.file_bytes(b"new").unwrap(), &request);
            for _ in 0..100 {
                assert!(poll(&mut future).is_pending());
                r.poll_budgeted(8).unwrap();
                r.wait(Duration::from_millis(1)).unwrap();
                if sim.trace().iter().any(|e| e.operation == "complete:rename") {
                    break;
                }
            }
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"new");
            assert!(weak.upgrade().is_some());
            if cancel {
                request.cancel().unwrap();
            } else {
                sim.inject("fsync", Fault::Errno(libc::EIO)).unwrap();
            }
            let result = drive(&r, future);
            if cancel {
                assert_eq!(
                    result,
                    Err(ReplacementError::RenameUncertain(Error::Cancelled))
                );
            } else {
                assert_eq!(
                    result,
                    Err(ReplacementError::Published(Error::Os(libc::EIO)))
                );
            }
            drive(&r, r.file_fence(())).unwrap();
            assert!(weak.upgrade().is_none());
            assert_eq!(r.in_flight(), 0);
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"new");
        }
    }

    #[test]
    fn output_budget_failure_precedes_any_read() {
        struct Reject;
        impl Budget for Reject {
            type Charge = ();
            fn charge(&self, _: usize) -> Result<()> {
                Err(Error::Overloaded)
            }
        }
        let sim = Simulation::new();
        let _environment = sim.enter();
        sim.write_file(Path::new("/data"), b"abc").unwrap();
        let fd = Rc::new(sim.open(None, Path::new("/data"), libc::O_RDONLY).unwrap());
        let r = crate::reactor::Reactor::new(8, Reject);
        let request = scope();
        let mut future = r.file_read_bounded(fd, 3, NonZeroUsize::new(2).unwrap(), &request);
        assert!(matches!(
            poll(&mut future),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert!(!sim.trace().iter().any(|e| e.operation == "submit:read"));
    }

    #[test]
    fn explicitly_injected_short_completions_preserve_offsets_and_reused_scratch() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let request = scope();
        sim.write_file(Path::new("/data"), b"abcdef").unwrap();
        let fd = open(&r, "/data", libc::O_RDONLY);
        sim.inject("read", Fault::Short(1)).unwrap();
        let output = drive(
            &r,
            r.file_read_bounded(fd, 6, NonZeroUsize::new(3).unwrap(), &request),
        )
        .unwrap();
        assert_eq!(&*output, b"abcdef");
        let reads: Vec<_> = sim
            .trace()
            .into_iter()
            .filter(|e| e.operation == "complete:read")
            .map(|e| e.result)
            .collect();
        assert_eq!(reads, [1, 3, 2, 0]);
        let replacement = replacement(&r, Durability::Publish);
        sim.inject("write", Fault::Short(1)).unwrap();
        drive(
            &r,
            r.file_replace_chunked(
                replacement,
                &output,
                NonZeroUsize::new(3).unwrap(),
                &request,
            ),
        )
        .unwrap();
        let writes: Vec<_> = sim
            .trace()
            .into_iter()
            .filter(|e| e.operation == "complete:write")
            .map(|e| e.result)
            .collect();
        assert_eq!(writes, [1, 2, 3]);
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"abcdef");
    }

    #[test]
    fn failed_or_abandoned_stage_never_publishes_partial_data() {
        for operation in ["write", "fsync", "rename", "abandon"] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let request = scope();
            sim.write_file(Path::new("/target"), b"old").unwrap();
            let replacement = replacement(&r, Durability::FileAndDirectory);
            let mut future = r.file_replace(replacement, r.file_bytes(b"new").unwrap(), &request);
            if operation == "abandon" {
                sim.inject("write", Fault::Delay(10)).unwrap();
                for _ in 0..100 {
                    assert!(poll(&mut future).is_pending());
                    if sim.trace().iter().any(|e| e.operation == "submit:write") {
                        break;
                    }
                    r.poll_budgeted(8).unwrap();
                    r.wait(Duration::from_millis(1)).unwrap();
                }
                assert!(sim.trace().iter().any(|e| e.operation == "submit:write"));
                assert_eq!(r.in_flight(), 1);
                drop(future);
                drive(&r, r.file_fence(())).unwrap();
                assert_eq!(r.in_flight(), 0);
            } else {
                sim.inject(operation, Fault::Errno(libc::EIO)).unwrap();
                assert_eq!(
                    drive(&r, future).map_err(|e| e.cause()),
                    Err(Error::Os(libc::EIO))
                );
            }
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"old");
        }
    }

    #[test]
    fn abandoned_held_rename_keeps_directory_owned_until_fenced() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let request = scope();
        let synced_directories = sim
            .trace()
            .iter()
            .filter(|e| e.operation == "sync:directory")
            .count();
        let replacement = replacement(&r, Durability::FileAndDirectory);
        let weak = Rc::downgrade(&replacement.directory);
        sim.inject("rename", Fault::HoldCompletion(20)).unwrap();
        let mut future = r.file_replace(replacement, r.file_bytes(b"published").unwrap(), &request);
        for _ in 0..100 {
            assert!(poll(&mut future).is_pending());
            r.poll_budgeted(8).unwrap();
            r.wait(Duration::from_millis(1)).unwrap();
            if sim.trace().iter().any(|e| e.operation == "complete:rename") {
                break;
            }
        }
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"published");
        drop(future);
        assert!(weak.upgrade().is_some());
        drive(&r, r.file_fence(())).unwrap();
        assert!(weak.upgrade().is_none());
        assert_eq!(
            sim.trace()
                .iter()
                .filter(|e| e.operation == "sync:directory")
                .count(),
            synced_directories
        );
        assert_eq!(r.in_flight(), 0);
    }

    #[test]
    fn empty_replacement_and_zero_write_completion_are_distinct() {
        for empty in [true, false] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let request = scope();
            let replacement = replacement(&r, Durability::Publish);
            if !empty {
                sim.inject("write", Fault::Short(0)).unwrap();
            }
            let result = drive(
                &r,
                r.file_replace(
                    replacement,
                    r.file_bytes(if empty { b"" } else { b"x" }).unwrap(),
                    &request,
                ),
            );
            if empty {
                result.unwrap();
                assert!(sim.read_file(Path::new("/target")).unwrap().is_empty());
            } else {
                assert_eq!(result.map_err(|e| e.cause()), Err(Error::Io));
                assert!(sim.read_file(Path::new("/target")).is_err());
            }
        }
    }
}
