//! Filesystem SQEs use the parent reactor's original/cancel completion fences.
//! Every pointer backing, descriptor, and quota lives in the submission closure.
use super::*;
use std::{ffi::CString, num::NonZeroUsize, ops::Deref};
use zeroize::Zeroize;

pub mod operations;

/// Stable, quota-owning I/O storage with a cursor for partial writes.
/// The entire allocation is zeroized on drop, including already consumed bytes.
pub struct Buffer {
    data: Vec<u8>,

    start: usize,

    end: usize,

    _quota: Charge,
}

/// Read output owns its admission charge until drop. Storage is allocated once,
/// never reallocated with secret bytes, and fully zeroized before release.
pub struct ReadBuffer {
    buffer: Buffer,
}

impl Deref for ReadBuffer {
    /// The initialized read output exposed to callers.
    type Target = [u8];

    /// Borrow only initialized output, not the unused bounded-read capacity.
    fn deref(&self) -> &[u8] {
        &self.buffer.data[..self.buffer.end]
    }
}

impl AsRef<[u8]> for ReadBuffer {
    /// Borrow the completed read without transferring its admission charge.
    fn as_ref(&self) -> &[u8] {
        self
    }
}

// SAFETY: private non-resizing Vec retains allocation and quota through completion.
unsafe impl IoBuffer for Buffer {
    /// Buffer access uses the reactor's standard error boundary.
    type Error = Error;

    /// Borrow the unconsumed region while retaining its stable allocation.
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.data[self.start..self.end])
    }

    /// Mutably borrow the unconsumed region without allowing a resize.
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.data[self.start..self.end])
    }
}

impl Drop for Buffer {
    /// Erase the entire allocation before storage and its charge are released.
    fn drop(&mut self) {
        self.data.as_mut_slice().zeroize();
    }
}

impl Buffer {
    /// Consume a positive, in-bounds completion without moving the allocation.
    pub fn advance(&mut self, n: usize) -> Result<()> {
        if n == 0 || n > self.remaining() {
            return Err(Error::Io);
        }
        self.start += n;
        Ok(())
    }

    /// Return the number of unconsumed bytes available to the next operation.
    pub fn remaining(&self) -> usize {
        self.end - self.start
    }

    /// Borrow a checked prefix of the unconsumed region after its I/O fence.
    pub fn prefix(&self, n: usize) -> Result<&[u8]> {
        self.bytes()?.get(..n).ok_or(Error::Io)
    }

    /// Allocate initialized storage without imposing an SQE-sized upper bound.
    /// Bounded read output can exceed one SQE; submission buffers cannot.
    fn allocate(length: usize, quota: Charge) -> Result<Self> {
        let mut data = Vec::new();
        data.try_reserve_exact(length)
            .map_err(|_| Error::Overloaded)?;
        data.resize(length, 0);
        Ok(Self {
            data,
            start: 0,
            end: length,
            _quota: quota,
        })
    }
}

impl<S: Scope, B: Budget> Reactor<S, B> {
    /// Allocate zero-initialized, quota-owning storage sized for one I/O submission.
    pub fn file_buffer(&self, length: usize) -> Result<Buffer> {
        // SQE read/write lengths are u32, not an application-specific 1 MiB cap.
        if length > u32::MAX as usize {
            return Err(Error::Overloaded);
        }
        Buffer::allocate(length, self.charge(length)?)
    }

    /// Copy caller bytes into stable, zeroizing storage charged to this reactor.
    pub fn file_bytes(&self, bytes: &[u8]) -> Result<Buffer> {
        let mut buffer = self.file_buffer(bytes.len())?;
        buffer.data.copy_from_slice(bytes);
        Ok(buffer)
    }

    /// Read from offset zero through EOF, probing one byte beyond `limit`.
    /// Output reserves and charges the full caller-selected limit up front.
    /// Callers validate type, permissions, and service ceilings before calling.
    pub fn file_read_bounded<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        limit: usize,
        chunk: NonZeroUsize,
        scope: &'a S,
    ) -> Operation<'a, ReadBuffer, S::Error> {
        Box::pin(async move {
            let probe = limit.checked_add(1).ok_or(Error::InvalidInput)?;
            let mut out = ReadBuffer {
                buffer: Buffer::allocate(limit, self.charge(limit)?)?,
            };
            out.buffer.end = 0;
            let mut buffer = self.file_buffer(probe.min(chunk.get()))?;
            loop {
                buffer.end = (probe - out.buffer.end).min(buffer.data.len());
                let completion = self
                    .read_at(fd.clone(), out.buffer.end as u64, buffer, (), scope)
                    .await?;
                let n = completion.bytes;
                buffer = completion.buffer;
                let bytes = buffer.prefix(n)?;
                if n == 0 {
                    return Ok(out);
                }
                if n > limit - out.buffer.end {
                    return Err(Error::Overloaded.into());
                }
                let end = out.buffer.end;
                out.buffer.data[end..end + n].copy_from_slice(bytes);
                out.buffer.end += n;
            }
        })
    }

    /// Open results use the same FD-owning CQE variant as accept, including when
    /// cancellation wins after a successful open. No integer FD can leak on drop.
    /// This secure convenience API creates owner-only files (0600, reduced by
    /// umask); callers needing other modes must prepare their descriptor separately.
    /// O_PATH accepts only DIRECTORY/NOFOLLOW/CLOEXEC, without forced NONBLOCK.
    /// Linux's 4096-byte path limit includes NUL; filesystem NAME_MAX still applies.
    pub fn file_open<'a>(
        &'a self,
        dir: Option<Rc<Descriptor>>,
        path: CString,
        flags: i32,
        resolve: u64,
        scope: &'a S,
    ) -> Operation<'a, Rc<Descriptor>, S::Error> {
        Box::pin(async move {
            if path.as_bytes().len() > PATH_BYTES {
                return Err(Error::InvalidInput.into());
            }
            let (flags, mode) = open_flags(flags)?;
            let quota = self.charge(path.as_bytes().len() + 128)?;
            let path = PathArg::new(path);
            let how = SyscallArg::new(
                types::OpenHow::new()
                    .flags(flags as u64)
                    .mode(mode)
                    .resolve(resolve),
            );
            let sqe = submission!(
                self,
                simulation::Op::Open {
                    dir: dir.clone(),
                    path: path.simulated(),
                    flags,
                    resolve
                },
                opcode::OpenAt2::new(
                    types::Fd(dir.as_ref().map_or(libc::AT_FDCWD, |d| d.as_raw_fd())),
                    path.as_ptr(),
                    how.as_ptr()
                )
                .build()
                .flags(squeue::Flags::ASYNC)
            );
            self.submit(sqe, scope, true, move |result| {
                drop((dir, path, how, quota));
                match result? {
                    KernelResult::Accepted(fd) => Ok(Rc::new(fd)),
                    result => {
                        value::<S::Error>(Ok(result))?;
                        Err(Error::Io.into())
                    }
                }
            })?
            .await
        })
    }

    /// Read basic metadata through an owned descriptor and stable output allocation.
    pub fn file_stat<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        scope: &'a S,
    ) -> Operation<'a, libc::statx, S::Error> {
        Box::pin(async move {
            let quota = self.charge(std::mem::size_of::<libc::statx>())?;
            let mut stat = SyscallArg::<libc::statx>::new(unsafe { std::mem::zeroed() });
            let sqe = submission!(
                self,
                simulation::Op::Stat {
                    fd: fd.clone(),
                    ptr: stat.as_mut_ptr()
                },
                opcode::Statx::new(
                    types::Fd(fd.as_raw_fd()),
                    c"".as_ptr(),
                    stat.as_mut_ptr().cast()
                )
                .flags(libc::AT_EMPTY_PATH)
                .mask(libc::STATX_BASIC_STATS)
                .build()
                .flags(squeue::Flags::ASYNC)
            );
            self.submit(sqe, scope, false, move |result| {
                let _quota = quota;
                value(result)?;
                drop(fd);
                Ok(stat.into_inner())
            })?
            .await
        })
    }

    /// Sync a file or directory while retaining its descriptor through the CQE fence.
    pub fn file_sync<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let sqe = submission!(
                self,
                simulation::Op::Sync(fd.clone()),
                opcode::Fsync::new(types::Fd(fd.as_raw_fd()))
                    .build()
                    .flags(squeue::Flags::ASYNC)
            );
            self.submit(sqe, scope, false, move |result| {
                drop(fd);
                value(result).map(|_| ())
            })?
            .await
        })
    }

    /// Rename within a pinned directory; cancellation is not proof it did not execute.
    /// Callers own name validation, namespace serialization, and durability policy.
    pub(super) fn file_rename<'a>(
        &'a self,
        dir: Rc<Descriptor>,
        from: CString,
        to: CString,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let quota = self.file_path_quota(&[&from, &to])?;
            let from = PathArg::new(from);
            let to = PathArg::new(to);
            let sqe = submission!(
                self,
                simulation::Op::Rename {
                    dir: dir.clone(),
                    from: from.simulated(),
                    to: to.simulated()
                },
                opcode::RenameAt::new(
                    types::Fd(dir.as_raw_fd()),
                    from.as_ptr(),
                    types::Fd(dir.as_raw_fd()),
                    to.as_ptr()
                )
                .build()
                .flags(squeue::Flags::ASYNC)
            );
            self.submit(sqe, scope, false, move |result| {
                drop((dir, from, to, quota));
                value(result).map(|_| ())
            })?
            .await
        })
    }

    /// Unlink relative to a pinned parent without an implicit directory sync.
    pub fn file_unlink<'a>(
        &'a self,
        dir: Rc<Descriptor>,
        name: CString,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        self.file_change(dir, name, NamespaceChange::Unlink, scope)
    }

    /// Retain a relative mutation's descriptor, pathname, and quota as one owner.
    /// Only a fenced kernel errno can satisfy the selected idempotency policy;
    /// scope cancellation and admission failures always propagate unchanged.
    fn file_change<'a>(
        &'a self,
        dir: Rc<Descriptor>,
        name: CString,
        change: NamespaceChange,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let quota = self.file_path_quota(&[&name])?;
            let name = PathArg::new(name);
            let sqe = match change {
                NamespaceChange::EnsureDirectory => submission!(
                    self,
                    simulation::Op::Mkdir {
                        dir: dir.clone(),
                        name: name.simulated()
                    },
                    opcode::MkDirAt::new(types::Fd(dir.as_raw_fd()), name.as_ptr())
                        .mode(0o700)
                        .build()
                        .flags(squeue::Flags::ASYNC)
                ),
                NamespaceChange::Unlink | NamespaceChange::RemoveIfPresent => submission!(
                    self,
                    simulation::Op::Unlink {
                        dir: dir.clone(),
                        name: name.simulated()
                    },
                    opcode::UnlinkAt::new(types::Fd(dir.as_raw_fd()), name.as_ptr())
                        .build()
                        .flags(squeue::Flags::ASYNC)
                ),
            };
            self.submit(sqe, scope, false, move |result| {
                drop((dir, name, quota));
                change.complete(result)
            })?
            .await
        })
    }

    /// Validate Linux pathname bounds and charge retained NUL-terminated bytes.
    fn file_path_quota(&self, paths: &[&CString]) -> Result<Charge> {
        if paths.iter().any(|p| p.as_bytes().len() > PATH_BYTES) {
            return Err(Error::InvalidInput);
        }
        self.charge(paths.iter().map(|p| p.as_bytes_with_nul().len()).sum())
    }
}

/// Linux's pathname limit excluding NUL; filesystems may impose smaller components.
const PATH_BYTES: usize = 4095;

/// A namespace mutation paired with the only kernel error it may treat as success.
/// Keeping operation and idempotency together prevents an unlink from accepting
/// EEXIST or directory creation from accepting ENOENT by mistake.
#[derive(Clone, Copy)]
enum NamespaceChange {
    /// Create a private directory or accept an existing entry for later validation.
    EnsureDirectory,
    /// Remove an entry, requiring it to exist when the operation executes.
    Unlink,
    /// Remove an entry or accept that it is already absent.
    RemoveIfPresent,
}

impl NamespaceChange {
    /// Decode a fenced completion without masking cancellation or other failures.
    fn complete<E: From<Error>>(self, result: Result<KernelResult, E>) -> Result<(), E> {
        let accepted = match self {
            Self::EnsureDirectory => Some(-libc::EEXIST),
            Self::Unlink => None,
            Self::RemoveIfPresent => Some(-libc::ENOENT),
        };
        if matches!(result, Ok(KernelResult::Value(n)) if Some(n) == accepted) {
            return Ok(());
        }
        value(result).map(|_| ())
    }
}

/// Stable pathname backing that preserves pointer provenance across owner moves.
/// Unlike CString's Box backing, this Vec must never resize after SQE publication.
struct PathArg(Vec<u8>);

impl PathArg {
    /// Transfer a validated pathname into stable syscall storage.
    fn new(path: CString) -> Self {
        Self(path.into_bytes_with_nul())
    }

    /// Borrow a pointer retained by the submission's completion closure.
    fn as_ptr(&self) -> *const libc::c_char {
        self.0.as_ptr().cast()
    }

    /// Copy the pathname into the simulator's typed operation.
    #[cfg(feature = "simulation")]
    fn simulated(&self) -> CString {
        CString::from_vec_with_nul(self.0.clone()).expect("validated path")
    }
}

/// Validate openat2 flag combinations and choose secure creation permissions.
fn open_flags(flags: i32) -> Result<(i32, u64)> {
    if flags & libc::O_PATH != 0 {
        if flags & !(libc::O_PATH | libc::O_CLOEXEC | libc::O_DIRECTORY | libc::O_NOFOLLOW) != 0 {
            return Err(Error::InvalidInput);
        }
        return Ok((flags | libc::O_CLOEXEC, 0));
    }
    let tmpfile = flags & libc::O_TMPFILE == libc::O_TMPFILE;
    if (tmpfile && flags & libc::O_ACCMODE == libc::O_RDONLY)
        || flags & libc::O_ACCMODE == libc::O_ACCMODE
    {
        return Err(Error::InvalidInput);
    }
    Ok((
        flags | libc::O_CLOEXEC | libc::O_NONBLOCK,
        if flags & libc::O_CREAT != 0 || tmpfile {
            0o600
        } else {
            0
        },
    ))
}

/// Translate a fenced scalar CQE through the caller's error boundary.
fn value<E: From<Error>>(result: Result<KernelResult, E>) -> Result<i32, E> {
    result?.value().map_err(Into::into)
}

pub mod secure {
    //! Descriptor-relative traversal and metadata checks without application policy.
    //! Callers select limits, required ownership/access, and error classification.
    use super::*;
    use std::{
        ffi::OsStr,
        path::{Component, Path},
    };

    /// Required metadata was unavailable, or the caller's access policy failed.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum AccessError {
        /// The stat result omitted a field needed for an access decision.
        MissingMetadata,
        /// A present field did not satisfy the requested access policy.
        PermissionDenied,
    }

    /// Caller-selected ownership, permission, and hard-link constraints.
    #[derive(Clone, Copy, Debug)]
    pub struct AccessRequirements {
        /// Required numeric owner ID.
        pub owner: u32,

        /// Any of these mode bits causes an access failure.
        pub forbidden_mode: u16,

        /// Optional exact link count; the metadata field is always required.
        pub links: Option<u32>,
    }

    /// Reject resolution that escapes the supplied directory descriptor.
    pub const BENEATH: u64 = 0x08;

    /// Reject magic links such as `/proc/self/fd` entries.
    pub const NO_MAGICLINKS: u64 = 0x02;

    /// Reject all symlinks during path resolution.
    pub const NO_SYMLINKS: u64 = 0x04;

    /// Filesystem hosting policy without application request IDs or executors.
    pub trait Host {
        /// Caller cancellation and error boundary.
        type Scope: Scope;

        /// Accounting retained by the reactor.
        type Budget: Budget;

        /// Serving worker's explicitly driven reactor.
        fn reactor(&self) -> &Reactor<Self::Scope, Self::Budget>;

        /// Allocate an independent attempt while retaining parent cancellation/deadline.
        fn fresh_scope(
            &self,
            parent: &Self::Scope,
        ) -> Result<Self::Scope, <Self::Scope as Scope>::Error>;

        /// Fence all submissions of the previous attempt, including abandoned futures.
        fn fence<'a>(
            &'a self,
            previous: &'a Self::Scope,
        ) -> Operation<'a, (), <Self::Scope as Scope>::Error>;

        /// Identify only the caller's missing-file error, never cancellation.
        fn is_missing(error: <Self::Scope as Scope>::Error) -> bool;
    }

    impl<H: Host> Host for Rc<H> {
        type Scope = H::Scope;

        type Budget = H::Budget;

        fn reactor(&self) -> &Reactor<Self::Scope, Self::Budget> {
            (**self).reactor()
        }

        fn fresh_scope(
            &self,
            parent: &Self::Scope,
        ) -> Result<Self::Scope, <Self::Scope as Scope>::Error> {
            (**self).fresh_scope(parent)
        }

        fn fence<'a>(
            &'a self,
            previous: &'a Self::Scope,
        ) -> Operation<'a, (), <Self::Scope as Scope>::Error> {
            (**self).fence(previous)
        }

        fn is_missing(error: <Self::Scope as Scope>::Error) -> bool {
            H::is_missing(error)
        }
    }

    /// Serializes a namespace owner and retains abandoned attempts until fenced.
    pub struct Attempts<S: Scope> {
        busy: Cell<bool>,

        previous: RefCell<Option<S>>,
    }

    impl<S: Scope> Default for Attempts<S> {
        fn default() -> Self {
            Self {
                busy: Cell::new(false),
                previous: RefCell::new(None),
            }
        }
    }

    impl<S: Scope> Attempts<S> {
        /// Fence the previous attempt before publishing a fresh submission scope.
        pub async fn begin<H: Host<Scope = S>>(
            &self,
            host: &H,
            parent: &S,
        ) -> Result<(crate::drivers::Busy<'_>, S), S::Error> {
            parent.check()?;
            let guard = crate::drivers::Busy::try_enter(&self.busy)?;
            let previous = self.previous.borrow().clone();
            if let Some(previous) = previous {
                host.fence(&previous).await?;
            }
            let scope = host.fresh_scope(parent)?;
            *self.previous.borrow_mut() = Some(scope.clone());
            Ok((guard, scope))
        }
    }

    /// Check owner-only access using the selected backend's effective owner.
    pub fn check_private(stat: &libc::statx, regular: bool) -> Result<(), AccessError> {
        #[cfg(feature = "simulation")]
        let simulated = simulation::Simulation::current().is_some();
        #[cfg(not(feature = "simulation"))]
        let simulated = false;
        // SAFETY: geteuid has no memory or lifetime preconditions.
        let owner = if simulated {
            0
        } else {
            unsafe { libc::geteuid() }
        };
        check_access(
            stat,
            AccessRequirements {
                owner,
                forbidden_mode: 0o077,
                links: regular.then_some(1),
            },
        )
    }

    /// Pin a symlink-free directory, optionally creating and checking private access.
    pub async fn directory<S: Scope, B: Budget>(
        r: &Reactor<S, B>,
        path: &Path,
        create: bool,
        private: bool,
        scope: &S,
    ) -> Result<Rc<Descriptor>, S::Error>
    where
        S::Error: From<AccessError>,
    {
        let fd = r.file_directory(path, create, 4096, scope).await?;
        if private {
            check_private(&r.file_stat(fd.clone(), scope).await?, false)?;
        }
        Ok(fd)
    }

    /// Read a regular file with a 1 MiB ceiling and optional private access checks.
    pub async fn read_file<S: Scope, B: Budget>(
        r: &Reactor<S, B>,
        fd: Rc<Descriptor>,
        limit: usize,
        private: bool,
        scope: &S,
    ) -> Result<ReadBuffer, S::Error>
    where
        S::Error: From<AccessError>,
    {
        if limit > 1024 * 1024 {
            return Err(Error::Overloaded.into());
        }
        let stat = r.file_stat(fd.clone(), scope).await?;
        check_regular_size(&stat, limit as u64)?;
        if private {
            check_private(&stat, true)?;
        }
        r.file_read_bounded(fd, limit, NonZeroUsize::new(16384).unwrap(), scope)
            .await
    }

    /// Read a projected file, permitting normal symlinks but never magic links.
    pub async fn read_path<S: Scope, B: Budget>(
        r: &Reactor<S, B>,
        path: &Path,
        limit: usize,
        scope: &S,
    ) -> Result<ReadBuffer, S::Error>
    where
        S::Error: From<AccessError>,
    {
        let fd = r
            .file_open(
                None,
                path_name(path.as_os_str(), 4096)?,
                libc::O_RDONLY,
                NO_MAGICLINKS,
                scope,
            )
            .await?;
        read_file(r, fd, limit, false, scope).await
    }

    /// Read one direct child without following symlinks or escaping its pinned parent.
    pub async fn read_at<S: Scope, B: Budget>(
        r: &Reactor<S, B>,
        dir: &Rc<Descriptor>,
        file: &str,
        limit: usize,
        private: bool,
        scope: &S,
    ) -> Result<ReadBuffer, S::Error>
    where
        S::Error: From<AccessError>,
    {
        let fd = r
            .file_open(
                Some(dir.clone()),
                component(file.as_ref(), 4096)?,
                libc::O_RDONLY,
                BENEATH | NO_SYMLINKS,
                scope,
            )
            .await?;
        read_file(r, fd, limit, private, scope).await
    }

    /// Durably replace a bounded file, preserving the exact publication failure phase.
    /// The caller exclusively owns the target and its deterministic stage namespace.
    pub async fn atomic_write<S: Scope, B: Budget>(
        r: &Reactor<S, B>,
        dir: &Rc<Descriptor>,
        target: &str,
        bytes: &[u8],
        scope: &S,
    ) -> Result<(), operations::ReplacementError<S::Error>> {
        use operations::{Durability, Replacement, ReplacementError::BeforeRename};
        let target_name = component(target.as_ref(), 4096).map_err(|e| BeforeRename(e.into()))?;
        if bytes.len() > 1024 * 1024 {
            return Err(BeforeRename(Error::Overloaded.into()));
        }
        let temporary = format!(".{target}.stage");
        let staged = r
            .file_stage(dir.clone(), temporary.as_ref(), 4096, scope)
            .await
            .map_err(BeforeRename)?;
        let buffer = r.file_bytes(bytes).map_err(|e| BeforeRename(e.into()))?;
        r.file_replace(
            Replacement {
                directory: dir.clone(),
                staged,
                temporary: component(temporary.as_ref(), 4096)
                    .map_err(|e| BeforeRename(e.into()))?,
                target: target_name,
                durability: Durability::FileAndDirectory,
            },
            buffer,
            scope,
        )
        .await
    }

    /// Remove one child and sync its parent, including when the child was absent.
    pub async fn remove<S: Scope, B: Budget>(
        r: &Reactor<S, B>,
        dir: &Rc<Descriptor>,
        file: &str,
        scope: &S,
    ) -> Result<(), S::Error> {
        r.file_remove_synced(dir.clone(), component(file.as_ref(), 4096)?, scope)
            .await
    }

    /// Validate byte length and embedded NULs without normalizing the path.
    pub fn path_name(path: &OsStr, limit: usize) -> Result<CString> {
        validate_path(path, limit)?;
        CString::new(path.as_bytes()).map_err(|_| Error::InvalidInput)
    }

    /// Validate one normal component, never a root, parent, current directory or path.
    pub fn component(name: &OsStr, limit: usize) -> Result<CString> {
        validate_component(name, limit)?;
        path_name(name, limit)
    }

    /// Check metadata completeness, owner, forbidden mode bits and optional links.
    /// The link field must be present even when no exact link count is requested.
    pub fn check_access(
        stat: &libc::statx,
        required: AccessRequirements,
    ) -> Result<(), AccessError> {
        let mask = libc::STATX_MODE | libc::STATX_UID | libc::STATX_NLINK;
        if stat.stx_mask & mask != mask {
            return Err(AccessError::MissingMetadata);
        }
        if stat.stx_mode & required.forbidden_mode != 0
            || stat.stx_uid != required.owner
            || required.links.is_some_and(|links| stat.stx_nlink != links)
        {
            return Err(AccessError::PermissionDenied);
        }
        Ok(())
    }

    /// Validate type and size before reading. A bounded read is still necessary
    /// because the file may grow after this snapshot.
    pub fn check_regular_size(stat: &libc::statx, limit: u64) -> Result<()> {
        let mask = libc::STATX_TYPE | libc::STATX_SIZE;
        if stat.stx_mask & mask != mask
            || stat.stx_mode as u32 & libc::S_IFMT != libc::S_IFREG
            || stat.stx_size > limit
        {
            return Err(Error::InvalidInput);
        }
        Ok(())
    }

    impl<S: Scope, B: Budget> Reactor<S, B> {
        /// Walk from `/` or `.`, pinning directories and rejecting parent components
        /// and symlinks. Creation uses owner-only mode and syncs each parent even
        /// when its child already exists. Access checks remain caller-owned.
        pub fn file_directory<'a>(
            &'a self,
            path: &'a Path,
            create: bool,
            path_limit: usize,
            scope: &'a S,
        ) -> Operation<'a, Rc<Descriptor>, S::Error> {
            Box::pin(async move {
                validate_path(path.as_os_str(), path_limit)?;
                if path
                    .components()
                    .any(|part| matches!(part, Component::ParentDir | Component::Prefix(_)))
                {
                    return Err(Error::InvalidConfiguration.into());
                }
                let mut fd = self
                    .file_open(
                        None,
                        CString::new(if path.is_absolute() { "/" } else { "." }).unwrap(),
                        libc::O_RDONLY | libc::O_DIRECTORY,
                        NO_SYMLINKS,
                        scope,
                    )
                    .await?;
                for part in path.components() {
                    let Component::Normal(part) = part else {
                        if matches!(part, Component::RootDir | Component::CurDir) {
                            continue;
                        }
                        return Err(Error::InvalidConfiguration.into());
                    };
                    let name = path_name(part, path_limit)?;
                    if create {
                        self.file_change(
                            fd.clone(),
                            name.clone(),
                            NamespaceChange::EnsureDirectory,
                            scope,
                        )
                        .await?;
                        self.file_sync(fd.clone(), scope).await?;
                    }
                    fd = self
                        .file_open(
                            Some(fd),
                            name,
                            libc::O_RDONLY | libc::O_DIRECTORY,
                            BENEATH | NO_SYMLINKS,
                            scope,
                        )
                        .await?;
                }
                Ok(fd)
            })
        }

        /// Remove a relative component and sync its parent even if it was absent,
        /// restoring the durability fence after a previously canceled unlink.
        pub fn file_remove_synced<'a>(
            &'a self,
            directory: Rc<Descriptor>,
            name: CString,
            scope: &'a S,
        ) -> Operation<'a, (), S::Error> {
            Box::pin(async move {
                validate_component(OsStr::from_bytes(name.as_bytes()), PATH_BYTES)?;
                self.file_change(
                    directory.clone(),
                    name,
                    NamespaceChange::RemoveIfPresent,
                    scope,
                )
                .await?;
                self.file_sync(directory, scope).await
            })
        }

        /// Remove and fence a stale stage, then exclusively create an owner-only
        /// file without following symlinks. Naming, serialization, publication,
        /// and failure cleanup remain caller-owned.
        pub fn file_stage<'a>(
            &'a self,
            directory: Rc<Descriptor>,
            temporary: &'a OsStr,
            name_limit: usize,
            scope: &'a S,
        ) -> Operation<'a, Rc<Descriptor>, S::Error> {
            Box::pin(async move {
                let name = component(temporary, name_limit)?;
                self.file_remove_synced(directory.clone(), name.clone(), scope)
                    .await?;
                self.file_open(
                    Some(directory),
                    name,
                    libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL,
                    BENEATH | NO_SYMLINKS,
                    scope,
                )
                .await
            })
        }
    }

    /// Check path bytes without allocating or changing their interpretation.
    fn validate_path(path: &OsStr, limit: usize) -> Result<()> {
        if path.as_bytes().len() > limit.min(PATH_BYTES) || path.as_bytes().contains(&0) {
            return Err(Error::InvalidInput);
        }
        Ok(())
    }

    /// Validate one relative name before descriptor-relative mutation.
    pub(super) fn validate_component(name: &OsStr, limit: usize) -> Result<()> {
        if !matches!(
            Path::new(name).components().next(),
            Some(Component::Normal(_))
        ) || name.as_bytes().contains(&b'/')
        {
            return Err(Error::InvalidInput);
        }
        validate_path(name, limit)
    }

    #[cfg(test)]
    mod tests {
        //! Pure validation covers malformed names and incomplete metadata snapshots.
        use super::*;

        /// Preserve arbitrary name bytes while rejecting escapes and invalid limits.
        #[test]
        fn names_preserve_bytes_and_reject_invalid_components_and_limits() {
            for name in ["", ".", "..", "/", "/child", "a/b", "a/", "a\0b"] {
                assert_eq!(
                    component(name.as_ref(), 4096),
                    Err(Error::InvalidInput),
                    "{name:?}"
                );
            }
            let name = OsStr::from_bytes(b"\xffchild");
            assert_eq!(component(name, 6).unwrap().as_bytes(), name.as_bytes());
            assert_eq!(component(name, 5), Err(Error::InvalidInput));
            assert_eq!(path_name(OsStr::new(""), 0).unwrap().as_bytes(), b"");
            assert!(path_name(OsStr::new("/a/../b"), 7).is_ok());
            assert_eq!(path_name(OsStr::new("a\0b"), 3), Err(Error::InvalidInput));
        }

        /// Require complete metadata before evaluating the requested access policy.
        #[test]
        fn access_checks_completeness_owner_modes_and_optional_exact_links() {
            let mut stat: libc::statx = unsafe { std::mem::zeroed() };
            stat.stx_mask = libc::STATX_MODE | libc::STATX_UID | libc::STATX_NLINK;
            stat.stx_mode = libc::S_IFREG as u16 | 0o600;
            stat.stx_uid = 123;
            stat.stx_nlink = 1;
            let required = AccessRequirements {
                owner: 123,
                forbidden_mode: 0o077,
                links: Some(1),
            };
            assert_eq!(check_access(&stat, required), Ok(()));
            for bit in [libc::STATX_MODE, libc::STATX_UID, libc::STATX_NLINK] {
                stat.stx_mask ^= bit;
                assert_eq!(
                    check_access(&stat, required),
                    Err(AccessError::MissingMetadata)
                );
                stat.stx_mask ^= bit;
            }
            assert_eq!(
                check_access(
                    &stat,
                    AccessRequirements {
                        owner: 124,
                        ..required
                    }
                ),
                Err(AccessError::PermissionDenied)
            );
            for bit in [0o040, 0o020, 0o010, 0o004, 0o002, 0o001] {
                stat.stx_mode |= bit;
                assert_eq!(
                    check_access(&stat, required),
                    Err(AccessError::PermissionDenied)
                );
                stat.stx_mode &= !bit;
            }
            for links in [0, 2, u32::MAX] {
                stat.stx_nlink = links;
                assert_eq!(
                    check_access(&stat, required),
                    Err(AccessError::PermissionDenied)
                );
                assert_eq!(
                    check_access(
                        &stat,
                        AccessRequirements {
                            links: None,
                            ..required
                        }
                    ),
                    Ok(())
                );
            }
        }

        /// Bound regular-file size and reject wrong types or missing stat fields.
        #[test]
        fn regular_size_checks_empty_exact_oversized_type_and_missing_fields() {
            let mut stat: libc::statx = unsafe { std::mem::zeroed() };
            stat.stx_mask = libc::STATX_TYPE | libc::STATX_SIZE;
            stat.stx_mode = libc::S_IFREG as u16;
            assert_eq!(check_regular_size(&stat, 0), Ok(()));
            stat.stx_size = 10;
            assert_eq!(check_regular_size(&stat, 10), Ok(()));
            assert_eq!(check_regular_size(&stat, 9), Err(Error::InvalidInput));
            for kind in [libc::S_IFDIR, libc::S_IFLNK, libc::S_IFIFO] {
                stat.stx_mode = kind as u16;
                assert_eq!(check_regular_size(&stat, 10), Err(Error::InvalidInput));
            }
            stat.stx_mode = libc::S_IFREG as u16;
            for bit in [libc::STATX_TYPE, libc::STATX_SIZE] {
                stat.stx_mask ^= bit;
                assert_eq!(check_regular_size(&stat, 10), Err(Error::InvalidInput));
                stat.stx_mask ^= bit;
            }
        }
    }
}

#[cfg(all(test, feature = "simulation"))]
mod secure_tests {
    //! Simulated namespace fault sequences complement host integration coverage.
    use super::*;
    use crate::reactor::{
        simulation::{Fault, Simulation},
        tests::{
            drive,
            fixtures::{Admission, Limits, Reactor},
            poll, scope,
        },
    };
    use std::{num::NonZeroUsize, path::Path};

    /// Construct the counting reactor used to observe simulated namespace fences.
    fn reactor() -> Reactor {
        Reactor::new(Rc::new(Admission::new(Limits {
            queue_entries: NonZeroUsize::new(8).unwrap(),
        })))
    }

    /// Keep common namespace backing and charges until abandoned SQEs are fenced.
    #[test]
    fn abandoned_namespace_changes_retain_descriptor_and_admission() {
        use crate::reactor::tests::fixtures::ResourceClass;

        for change in [
            NamespaceChange::EnsureDirectory,
            NamespaceChange::Unlink,
            NamespaceChange::RemoveIfPresent,
        ] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let request = scope();
            let directory =
                drive(&r, r.file_directory(Path::new("/"), false, 4096, &request)).unwrap();
            let weak = Rc::downgrade(&directory);
            let operation = match change {
                NamespaceChange::EnsureDirectory => "mkdir",
                NamespaceChange::Unlink | NamespaceChange::RemoveIfPresent => {
                    sim.write_file(Path::new("/entry"), b"old").unwrap();
                    "unlink"
                }
            };
            let baseline = r.admission.used(ResourceClass::RequestContext);
            sim.inject(operation, Fault::Delay(10)).unwrap();
            let mut future =
                r.file_change(directory, CString::new("entry").unwrap(), change, &request);
            assert!(poll(&mut future).is_pending());
            drop(future);
            assert_eq!(r.in_flight(), 1);
            assert!(weak.upgrade().is_some());
            assert!(r.admission.used(ResourceClass::RequestContext) > baseline);
            drive(&r, r.file_fence(())).unwrap();
            assert_eq!(r.in_flight(), 0);
            assert!(weak.upgrade().is_none());
            assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
        }
    }

    /// Validate paths before mutation and reject symlinks during pinned traversal.
    #[test]
    fn traversal_pins_private_directories_and_rejects_parents_symlinks_and_bad_names() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let request = scope();
        for path in [
            "/must-not-create/../child",
            "/also-absent/child/../../target",
        ] {
            let before = sim.trace().len();
            assert!(matches!(
                drive(&r, r.file_directory(Path::new(path), true, 4096, &request)),
                Err(Error::InvalidConfiguration)
            ));
            assert_eq!(sim.trace().len(), before);
        }
        assert!(sim.metadata(Path::new("/must-not-create")).is_err());
        assert!(sim.metadata(Path::new("/also-absent")).is_err());
        for path in ["/private/child", "private/./child"] {
            let dir = drive(&r, r.file_directory(Path::new(path), true, 4096, &request)).unwrap();
            let stat = drive(&r, r.file_stat(dir, &request)).unwrap();
            assert_eq!(stat.stx_mode as u32, libc::S_IFDIR | 0o700);
        }
        sim.disk().crash().unwrap();
        assert!(sim.metadata(Path::new("/private/child")).is_ok());
        sim.symlink(Path::new("private"), Path::new("/link"))
            .unwrap();
        for (path, limit, error) in [
            ("/link/child", 4096, Error::Os(libc::ELOOP)),
            ("/private/../child", 4096, Error::InvalidConfiguration),
            ("/absent", 4096, Error::NotFound),
            ("/private", 2, Error::InvalidInput),
            ("/bad\0name", 4096, Error::InvalidInput),
        ] {
            assert!(
                matches!(drive(&r, r.file_directory(Path::new(path), false, limit, &request)), Err(actual) if actual == error),
                "{path:?}"
            );
        }
        for path in ["/", ".", ""] {
            assert!(drive(&r, r.file_directory(Path::new(path), false, 4096, &request)).is_ok());
        }
        request.cancel().unwrap();
        assert!(matches!(
            drive(&r, r.file_directory(Path::new("/"), false, 4096, &request)),
            Err(Error::Cancelled)
        ));
    }

    /// Sync even existing directories and propagate both mkdir and fsync failures.
    #[test]
    fn existing_directory_creation_reestablishes_fsync_and_propagates_failure() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        sim.create_dir_all(Path::new("/existing")).unwrap();
        sim.inject("fsync", Fault::Errno(libc::EIO)).unwrap();
        assert!(matches!(
            drive(
                &r,
                r.file_directory(Path::new("/existing"), true, 4096, &scope())
            ),
            Err(Error::Os(libc::EIO))
        ));
        drive(
            &r,
            r.file_directory(Path::new("/existing"), true, 4096, &scope()),
        )
        .unwrap();
        sim.disk().crash().unwrap();
        assert!(sim.metadata(Path::new("/existing")).is_ok());
        sim.inject("mkdir", Fault::Errno(libc::EIO)).unwrap();
        assert!(matches!(
            drive(
                &r,
                r.file_directory(Path::new("/new"), true, 4096, &scope())
            ),
            Err(Error::Os(libc::EIO))
        ));
    }

    /// Fence stage cleanup without following links or publishing abandoned work.
    #[test]
    fn stage_removes_only_the_link_and_requires_cleanup_fence_before_exclusive_open() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let request = scope();
        let dir = drive(&r, r.file_directory(Path::new("/"), false, 4096, &request)).unwrap();
        sim.write_file(Path::new("/target"), b"keep").unwrap();
        sim.symlink(Path::new("target"), Path::new("/stage"))
            .unwrap();
        let stage = drive(
            &r,
            r.file_stage(dir.clone(), "stage".as_ref(), 4096, &request),
        )
        .unwrap();
        let stat = drive(&r, r.file_stat(stage, &request)).unwrap();
        assert_eq!(stat.stx_mode as u32, libc::S_IFREG | 0o600);
        assert_eq!(stat.stx_size, 0);
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"keep");
        assert!(matches!(
            drive(
                &r,
                r.file_stage(dir.clone(), "../target".as_ref(), 4096, &request)
            ),
            Err(Error::InvalidInput)
        ));
        for operation in ["unlink", "fsync", "open"] {
            sim.inject(operation, Fault::Errno(libc::EIO)).unwrap();
            assert!(matches!(
                drive(
                    &r,
                    r.file_stage(dir.clone(), "stage".as_ref(), 4096, &request)
                ),
                Err(Error::Os(libc::EIO))
            ));
            assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"keep");
        }
        sim.inject("open", Fault::Errno(libc::EEXIST)).unwrap();
        assert!(matches!(
            drive(
                &r,
                r.file_stage(dir.clone(), "stage".as_ref(), 4096, &request)
            ),
            Err(Error::AlreadyExists)
        ));
        sim.inject("fsync", Fault::Errno(libc::EIO)).unwrap();
        assert_eq!(
            drive(
                &r,
                r.file_remove_synced(dir.clone(), CString::new("absent").unwrap(), &request)
            ),
            Err(Error::Os(libc::EIO))
        );
        let mut abandoned = r.file_stage(dir, "stage".as_ref(), 4096, &request);
        sim.inject("unlink", Fault::Delay(10)).unwrap();
        assert!(poll(&mut abandoned).is_pending());
        drop(abandoned);
        drive(&r, r.file_fence(())).unwrap();
        assert_eq!(r.in_flight(), 0);
        assert_eq!(sim.read_file(Path::new("/target")).unwrap(), b"keep");
    }
}

#[cfg(all(test, feature = "simulation"))]
mod test_support {
    //! Bounded explicit driving for filesystem operations with arbitrary errors.
    use super::*;
    use crate::reactor::tests::fixtures::Reactor;
    use std::time::Instant;

    /// Poll one reply without implicitly advancing the backend.
    pub(super) fn poll<T, E>(future: &mut Operation<'_, T, E>) -> Poll<Result<T, E>> {
        future
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }

    /// Drive explicit completion turns, preserving each operation's error type.
    pub(super) fn drive<T, E>(reactor: &Reactor, mut future: Operation<'_, T, E>) -> Result<T, E> {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if let Poll::Ready(result) = poll(&mut future) {
                return result;
            }
            assert!(Instant::now() < deadline, "reactor failed to make progress");
            reactor.poll_budgeted(8).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }
}

#[cfg(test)]
mod memory_tests {
    //! Delayed consumers exercise pointer provenance without kernel access.
    use super::*;
    use std::ffi::CStr;

    /// Keep cursor mutations bounded and release quota on allocation failure or drop.
    #[test]
    fn shared_allocation_preserves_cursor_and_releases_charge_on_failure() {
        let charge = Rc::new(());
        let weak = Rc::downgrade(&charge);
        let mut buffer = Buffer::allocate(4, Box::new(charge)).unwrap();
        assert_eq!(buffer.bytes().unwrap(), &[0; 4]);
        buffer.bytes_mut().unwrap().copy_from_slice(b"data");
        buffer.advance(2).unwrap();
        assert_eq!(buffer.prefix(2).unwrap(), b"ta");
        assert_eq!(buffer.advance(3), Err(Error::Io));
        assert_eq!(buffer.remaining(), 2);
        assert!(weak.upgrade().is_some());
        drop(buffer);
        assert!(weak.upgrade().is_none());

        let charge = Rc::new(());
        let weak = Rc::downgrade(&charge);
        assert!(matches!(
            Buffer::allocate(usize::MAX, Box::new(charge)),
            Err(Error::Overloaded)
        ));
        assert!(weak.upgrade().is_none());
        assert_eq!(Buffer::allocate(0, Box::new(())).unwrap().remaining(), 0);
    }

    /// Preserve the pathname allocation when its owner moves into a completion.
    #[test]
    fn delayed_pathname_consumer_survives_owner_moves() {
        let path = PathArg::new(CString::new("stage-to-publish").unwrap());
        let pointer = path.as_ptr();
        let entry = (path, pointer);
        let completion: Box<dyn FnOnce()> = Box::new(move || {
            let (path, pointer) = entry;
            assert_eq!(path.as_ptr(), pointer);
            // SAFETY: the moved owner retains the original terminating NUL.
            assert_eq!(
                unsafe { CStr::from_ptr(pointer) }.to_bytes(),
                b"stage-to-publish"
            );
            drop(path);
        });
        completion();
    }

    /// Preserve syscall input and output pointers across completion-owner moves.
    #[test]
    fn delayed_syscall_read_and_write_survive_owner_moves() {
        let input = SyscallArg::new([11u64, 22]);
        let read = input.as_ptr();
        let mut output = SyscallArg::new([0u64; 2]);
        let write = output.as_mut_ptr();
        let entry = (input, output);
        let completion: Box<dyn FnOnce()> = Box::new(move || {
            let (input, output) = entry;
            // SAFETY: separate live allocations have no borrowed contents.
            unsafe { write.write(read.read()) };
            assert_eq!(output.into_inner(), [11, 22]);
            assert_eq!(input.into_inner(), [11, 22]);
        });
        completion();
    }
}

#[cfg(test)]
mod kernel_tests {
    //! Private Linux flag validation; host behavior lives in tests/runtime.rs.
    use super::*;

    /// Accept only the selected operation's idempotent errno, never scope failures.
    #[test]
    fn namespace_policy_is_bound_to_the_operation() {
        for (change, exists, absent) in [
            (
                NamespaceChange::EnsureDirectory,
                Ok(()),
                Err(Error::NotFound),
            ),
            (
                NamespaceChange::Unlink,
                Err(Error::AlreadyExists),
                Err(Error::NotFound),
            ),
            (
                NamespaceChange::RemoveIfPresent,
                Err(Error::AlreadyExists),
                Ok(()),
            ),
        ] {
            assert_eq!(change.complete::<Error>(Ok(KernelResult::Value(0))), Ok(()));
            assert_eq!(
                change.complete(Ok(KernelResult::Value(-libc::EEXIST))),
                exists
            );
            assert_eq!(
                change.complete(Ok(KernelResult::Value(-libc::ENOENT))),
                absent
            );
            assert_eq!(
                change.complete::<Error>(Ok(KernelResult::Value(-libc::EIO))),
                Err(Error::Os(libc::EIO))
            );
            for error in [
                Error::Cancelled,
                Error::DeadlineExceeded,
                Error::Overloaded,
                Error::AlreadyExists,
                Error::NotFound,
            ] {
                assert_eq!(change.complete::<Error>(Err(error)), Err(error));
            }
        }
    }

    /// Keep secure flags and Linux pathname bounds independent of service limits.
    #[test]
    fn flags_and_linux_path_bounds_are_not_application_limits() {
        assert_eq!(
            open_flags(libc::O_PATH),
            Ok((libc::O_PATH | libc::O_CLOEXEC, 0))
        );
        assert_eq!(
            open_flags(libc::O_PATH | libc::O_NONBLOCK),
            Err(Error::InvalidInput)
        );
        assert_eq!(
            open_flags(libc::O_PATH | libc::O_CREAT),
            Err(Error::InvalidInput)
        );
        assert_eq!(
            open_flags(libc::O_TMPFILE | libc::O_RDONLY),
            Err(Error::InvalidInput)
        );
        assert_eq!(
            open_flags(libc::O_TMPFILE | libc::O_WRONLY).unwrap().1,
            0o600
        );
        assert_eq!(open_flags(libc::O_CREAT | libc::O_WRONLY).unwrap().1, 0o600);
        assert_eq!(open_flags(libc::O_RDONLY).unwrap().1, 0);
        assert!(secure::path_name(std::ffi::OsStr::new(&"x".repeat(4095)), usize::MAX).is_ok());
        assert_eq!(
            secure::path_name(std::ffi::OsStr::new(&"x".repeat(4096)), usize::MAX),
            Err(Error::InvalidInput)
        );
    }
}
