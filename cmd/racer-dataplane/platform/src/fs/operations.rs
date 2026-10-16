//! Staged replacement and publication, with file policy supplied by the caller.
//!
//! `publish_new` runs in the ambient simulated world when a
//! [`Simulation`](crate::simulation::Simulation) is current, as the old
//! runtime did.
use super::*;
use std::ffi::OsStr;
use std::os::fd::{AsRawFd, FromRawFd};

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

/// Input ownership for the shared replacement pipeline.
enum ReplacementInput<'a> {
    /// Consume an already admitted buffer and release it once writing finishes.
    Owned(Buffer),
    /// Copy borrowed input through scratch retained until publication completes.
    Chunked(&'a [u8], NonZeroUsize),
}

/// A validated replacement paired with its verified, nonappend write descriptor.
/// Keeping these together prevents publication with another stage's descriptor.
struct PreparedReplacement {
    replacement: Replacement,

    staged: Rc<Descriptor>,
}

/// A prepared replacement whose entire input has reached its write completion fence.
/// Only this state can enter publication. Scratch remains charged for the same
/// interval as the chunked API's original reusable allocation.
struct WrittenReplacement {
    prepared: PreparedReplacement,

    _scratch: Option<Buffer>,
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
        self.replace_input(replacement, ReplacementInput::Owned(buffer), scope)
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
        self.replace_input(replacement, ReplacementInput::Chunked(bytes, chunk), scope)
    }

    /// Share validation, full-write completion, and publication across input forms.
    fn replace_input<'a>(
        &'a self,
        replacement: Replacement,
        input: ReplacementInput<'a>,
        scope: &'a S,
    ) -> Operation<'a, (), ReplacementError<S::Error>> {
        Box::pin(async move {
            let prepared = self
                .prepare_replacement(replacement, scope)
                .await
                .map_err(ReplacementError::BeforeRename)?;
            let written = self
                .write_replacement(prepared, input, scope)
                .await
                .map_err(ReplacementError::BeforeRename)?;
            self.publish_replacement(written, scope).await
        })
    }

    /// Verify a fresh single-link stage and reopen its exact inode without append.
    async fn prepare_replacement(
        &self,
        replacement: Replacement,
        scope: &S,
    ) -> Result<PreparedReplacement, S::Error> {
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
        Ok(PreparedReplacement {
            replacement,
            staged,
        })
    }

    /// Finish every input chunk before granting the capability to publish the stage.
    /// Empty input still passes stage validation and preserves allocation policy.
    async fn write_replacement(
        &self,
        prepared: PreparedReplacement,
        input: ReplacementInput<'_>,
        scope: &S,
    ) -> Result<WrittenReplacement, S::Error> {
        let mut offset = 0;
        let scratch = match input {
            ReplacementInput::Owned(buffer) => {
                self.write_complete(prepared.staged.clone(), buffer, &mut offset, scope)
                    .await?;
                None
            }
            ReplacementInput::Chunked(bytes, chunk) => {
                let mut buffer = self.file_buffer(bytes.len().min(chunk.get()))?;
                for bytes in bytes.chunks(chunk.get()) {
                    buffer.start = 0;
                    buffer.end = bytes.len();
                    buffer.data[..bytes.len()].copy_from_slice(bytes);
                    buffer = self
                        .write_complete(prepared.staged.clone(), buffer, &mut offset, scope)
                        .await?;
                }
                Some(buffer)
            }
        };
        Ok(WrittenReplacement {
            prepared,
            _scratch: scratch,
        })
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
                .write_at_offloaded(staged.clone(), *offset, buffer, (), scope)
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
        written: WrittenReplacement,
        scope: &S,
    ) -> Result<(), ReplacementError<S::Error>> {
        let WrittenReplacement { prepared, _scratch } = written;
        let PreparedReplacement {
            replacement,
            staged,
        } = prepared;
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
        simulated: Option<(crate::simulation::Simulation, std::path::PathBuf)>,
    }

    impl Directory {
        /// Walk and pin private directories without traversing parent symlinks.
        pub(super) fn open(path: &Path) -> io::Result<Self> {
            #[cfg(feature = "simulation")]
            if let Some(sim) = crate::simulation::Simulation::current() {
                let mut current =
                    std::path::PathBuf::from(if path.is_absolute() { "/" } else { "." });
                let flags = libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW;
                let mut fd = sim.open(None, &current, flags)?;
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
                        fd = sim.open(Some(&fd), Path::new(part), flags)?;
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
                // The simulator creates files 0o666 under its umask; restrict
                // the stage to 0o600 like the host path.
                let fd = sim.open(Some(&self.fd), Path::new(name_os(name)), flags)?;
                let path = self.simulated_path(name);
                sim.chmod(&path, 0o600)?;
                return Ok(fd);
            }
            host_open(self.fd.as_raw_fd(), name, flags, 0o600)
        }

        /// Publish a stage without an implicit durability fence.
        pub(super) fn rename(&self, from: &CString, to: &CString) -> io::Result<()> {
            #[cfg(feature = "simulation")]
            if let Some((sim, _)) = &self.simulated {
                return sim.rename(&self.simulated_path(from), &self.simulated_path(to), 0);
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
            if let Some((sim, _)) = &self.simulated {
                return sim.unlink(&self.simulated_path(name));
            }
            // SAFETY: name and pinned parent stay live for this syscall.
            status(unsafe { libc::unlinkat(self.fd.as_raw_fd(), name.as_ptr(), 0) })
        }
    }

    #[cfg(feature = "simulation")]
    impl Directory {
        /// The simulated path of a child of the pinned directory.
        fn simulated_path(&self, name: &CString) -> std::path::PathBuf {
            let (_, directory) = self.simulated.as_ref().expect("simulated directory");
            directory.join(name_os(name))
        }
    }

    /// Borrow a component name as an OS string without copying.
    #[cfg(feature = "simulation")]
    fn name_os(name: &CString) -> &OsStr {
        use std::os::unix::ffi::OsStrExt;
        OsStr::from_bytes(name.as_bytes())
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
        // SAFETY: openat returned a fresh descriptor that nothing else owns.
        Ok(unsafe { Descriptor::from_raw_fd(fd) })
    }

    /// Perform one positioned write and return short progress or the original error.
    pub(super) fn write(fd: &Descriptor, offset: usize, bytes: &[u8]) -> io::Result<usize> {
        #[cfg(feature = "simulation")]
        if let Some(fd) = fd.as_sim() {
            return fd
                .write_at(offset as u64, bytes)
                .map_err(crate::simulation::io_error);
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
