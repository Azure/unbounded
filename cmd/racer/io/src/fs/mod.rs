//! Secure, descriptor-relative filesystem primitives on the reactor ring:
//! private directories, bounded reads, durable atomic replacement, and
//! two-slot checkpoints. Every call is an ordinary ring operation, so it
//! never blocks the lane and runs unchanged on the simulated driver.
pub mod checkpoint;

use crate::{
    Error, Reactor, Result,
    ring::{Descriptor, IoBuffer, OpCtx},
};
use std::{
    ffi::{CString, OsStr},
    num::NonZeroUsize,
    ops::Deref,
    os::unix::ffi::OsStrExt,
    path::{Component, Path},
    rc::Rc,
};
use zeroize::Zeroize;

/// Reject resolution that escapes the supplied directory descriptor.
pub const BENEATH: u64 = 0x08;

/// Reject magic links such as `/proc/self/fd` entries.
pub const NO_MAGICLINKS: u64 = 0x02;

/// Reject all symlinks during path resolution.
pub const NO_SYMLINKS: u64 = 0x04;

/// Linux's pathname limit excluding NUL.
pub const PATH_BYTES: usize = crate::ring::ops::PATH_BYTES;

/// Largest file the composed helpers read or write.
pub const SMALL_FILE_BYTES: usize = 1024 * 1024;

/// Stable I/O storage with a cursor for partial transfers, zeroized on drop.
pub struct Buffer {
    data: Vec<u8>,

    start: usize,

    end: usize,
}

// SAFETY: the private Vec is never resized after allocation, so its heap
// allocation is stable across moves of the Buffer.
unsafe impl IoBuffer for Buffer {
    /// The unconsumed region.
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.data[self.start..self.end])
    }

    /// The unconsumed region, mutably.
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.data[self.start..self.end])
    }
}

impl Drop for Buffer {
    /// Erase the whole allocation.
    fn drop(&mut self) {
        self.data.as_mut_slice().zeroize();
    }
}

impl Buffer {
    /// Zeroed storage of `length` bytes (at most `u32::MAX`).
    pub fn new(length: usize) -> Result<Self> {
        if length > u32::MAX as usize {
            return Err(Error::Overloaded);
        }
        let mut data = Vec::new();
        data.try_reserve_exact(length)
            .map_err(|_| Error::Overloaded)?;
        data.resize(length, 0);
        Ok(Self {
            data,
            start: 0,
            end: length,
        })
    }

    /// A copy of `bytes`.
    pub fn from_bytes(bytes: &[u8]) -> Result<Self> {
        let mut buffer = Self::new(bytes.len())?;
        buffer.data.copy_from_slice(bytes);
        Ok(buffer)
    }

    /// Consume `n` positive, in-bounds bytes.
    pub fn advance(&mut self, n: usize) -> Result<()> {
        if n == 0 || n > self.remaining() {
            return Err(Error::Io);
        }
        self.start += n;
        Ok(())
    }

    /// Unconsumed bytes.
    pub fn remaining(&self) -> usize {
        self.end - self.start
    }

    /// A checked prefix of the unconsumed region.
    pub fn prefix(&self, n: usize) -> Result<&[u8]> {
        self.data[self.start..self.end].get(..n).ok_or(Error::Io)
    }

    /// Reset to `bytes`, which must fit the allocation.
    fn fill(&mut self, bytes: &[u8]) {
        self.start = 0;
        self.end = bytes.len();
        self.data[..bytes.len()].copy_from_slice(bytes);
    }
}

/// Output of a bounded read, zeroized on drop.
pub struct ReadBuffer {
    buffer: Buffer,
}

impl Deref for ReadBuffer {
    type Target = [u8];

    /// The bytes read.
    fn deref(&self) -> &[u8] {
        &self.buffer.data[..self.buffer.end]
    }
}

impl std::fmt::Debug for ReadBuffer {
    /// Show only the length; contents may be secret.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ReadBuffer")
            .field("len", &self.len())
            .finish()
    }
}

impl AsRef<[u8]> for ReadBuffer {
    /// The bytes read.
    fn as_ref(&self) -> &[u8] {
        self
    }
}

/// Required metadata was unavailable, or an access policy failed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AccessError {
    /// The stat result omitted a field needed for the decision.
    MissingMetadata,
    /// A present field did not satisfy the policy.
    PermissionDenied,
}

impl From<AccessError> for Error {
    /// Both access failures are permission errors.
    fn from(error: AccessError) -> Self {
        match error {
            AccessError::MissingMetadata => Error::Io,
            AccessError::PermissionDenied => Error::Os(libc::EACCES),
        }
    }
}

/// Ownership, permission, and hard-link constraints.
#[derive(Clone, Copy, Debug)]
pub struct AccessRequirements {
    /// Required numeric owner.
    pub owner: u32,

    /// Any of these mode bits fails the check.
    pub forbidden_mode: u16,

    /// Optional exact link count; the field is always required.
    pub links: Option<u32>,
}

/// Check completeness, owner, forbidden mode bits, and optional links.
pub fn check_access(
    stat: &libc::statx,
    required: AccessRequirements,
) -> std::result::Result<(), AccessError> {
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

/// Owner-only access for the descriptor's backend: the effective UID on the
/// host, UID 0 in the simulated OS. Regular files need exactly one link.
pub fn check_private(
    fd: &Descriptor,
    stat: &libc::statx,
    regular: bool,
) -> std::result::Result<(), AccessError> {
    let owner = if fd.is_sim() {
        0
    } else {
        // SAFETY: geteuid has no preconditions.
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

/// Require a regular file no larger than `limit`.
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

/// Validate length and embedded NULs without normalizing.
pub fn path_name(path: &OsStr, limit: usize) -> Result<CString> {
    if path.as_bytes().len() > limit.min(PATH_BYTES) || path.as_bytes().contains(&0) {
        return Err(Error::InvalidInput);
    }
    CString::new(path.as_bytes()).map_err(|_| Error::InvalidInput)
}

/// Validate one normal component (no root, parent, current, or slash).
pub fn component(name: &OsStr, limit: usize) -> Result<CString> {
    if !matches!(
        Path::new(name).components().next(),
        Some(Component::Normal(_))
    ) || name.as_bytes().contains(&b'/')
    {
        return Err(Error::InvalidInput);
    }
    path_name(name, limit)
}

/// Where a replacement stopped. An accepted rename may execute even if
/// cancellation wins its completion race; reconcile after fencing.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ReplacementError {
    /// Validation, writing, or staged-file sync failed before the rename.
    BeforeRename(Error),
    /// The rename failed or was canceled; its outcome is uncertain.
    RenameUncertain(Error),
    /// The rename completed; only the parent durability fence failed.
    Published(Error),
}

impl ReplacementError {
    /// The underlying failure.
    pub fn cause(&self) -> Error {
        match *self {
            Self::BeforeRename(e) | Self::RenameUncertain(e) | Self::Published(e) => e,
        }
    }
}

/// Fences after writing and after renaming.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Durability {
    /// Namespace publication only.
    Publish,
    /// Sync the stage before the rename, then sync the parent directory.
    FileAndDirectory,
}

/// A staged replacement in a pinned, exclusively controlled directory.
pub struct Replacement {
    /// Trusted parent directory.
    pub directory: Rc<Descriptor>,

    /// Fresh, empty, single-link stage.
    pub staged: Rc<Descriptor>,

    /// Stage name (one component).
    pub temporary: CString,

    /// Distinct destination name (one component).
    pub target: CString,

    /// Requested fences.
    pub durability: Durability,
}

impl Reactor {
    /// Walk from `/` or `.` pinning each directory, rejecting `..` and
    /// symlinks. With `create`, missing directories are created owner-only
    /// and each parent is synced. With `private`, the result must be
    /// owner-only.
    pub async fn open_directory(
        &self,
        path: &Path,
        create: bool,
        private: bool,
        ctx: &OpCtx,
    ) -> Result<Rc<Descriptor>> {
        path_name(path.as_os_str(), PATH_BYTES)?;
        let root = CString::new(if path.is_absolute() { "/" } else { "." }).unwrap();
        let flags = libc::O_RDONLY | libc::O_DIRECTORY;
        let mut fd = Rc::new(self.open(None, root, flags, NO_SYMLINKS, ctx)?.await?);
        for part in path.components() {
            let Component::Normal(part) = part else {
                if matches!(part, Component::RootDir | Component::CurDir) {
                    continue;
                }
                return Err(Error::InvalidConfiguration);
            };
            let name = path_name(part, PATH_BYTES)?;
            if create {
                self.mkdir(fd.clone(), name.clone(), 0o700, true, ctx)?
                    .await?;
                self.fsync(fd.clone(), false, ctx).await?;
            }
            fd = Rc::new(
                self.open(Some(fd), name, flags, BENEATH | NO_SYMLINKS, ctx)?
                    .await?,
            );
        }
        if private {
            let stat = self.stat(fd.clone(), ctx).await?;
            check_private(&fd, &stat, false)?;
        }
        Ok(fd)
    }

    /// Read from offset zero through EOF into at most `limit` bytes, probing
    /// one byte beyond to detect oversize files ([`Error::Overloaded`]).
    pub async fn read_bounded(
        &self,
        fd: Rc<Descriptor>,
        limit: usize,
        chunk: NonZeroUsize,
        ctx: &OpCtx,
    ) -> Result<ReadBuffer> {
        let probe = limit.checked_add(1).ok_or(Error::InvalidInput)?;
        let mut out = Buffer::new(limit)?;
        out.end = 0;
        let mut scratch = Buffer::new(probe.min(chunk.get()))?;
        loop {
            scratch.start = 0;
            scratch.end = (probe - out.end).min(scratch.data.len());
            let done = self
                .read_at(fd.clone(), out.end as u64, scratch, (), ctx)
                .await?;
            scratch = done.buffer;
            let n = done.bytes;
            if n == 0 {
                return Ok(ReadBuffer { buffer: out });
            }
            if n > limit - out.end {
                return Err(Error::Overloaded);
            }
            let end = out.end;
            out.data[end..end + n].copy_from_slice(scratch.prefix(n)?);
            out.end += n;
        }
    }

    /// Read a regular file of at most `limit` (at most [`SMALL_FILE_BYTES`])
    /// bytes, optionally requiring owner-only access.
    pub async fn read_small(
        &self,
        fd: Rc<Descriptor>,
        limit: usize,
        private: bool,
        ctx: &OpCtx,
    ) -> Result<ReadBuffer> {
        if limit > SMALL_FILE_BYTES {
            return Err(Error::Overloaded);
        }
        let stat = self.stat(fd.clone(), ctx).await?;
        check_regular_size(&stat, limit as u64)?;
        if private {
            check_private(&fd, &stat, true)?;
        }
        self.read_bounded(fd, limit, NonZeroUsize::new(16384).unwrap(), ctx)
            .await
    }

    /// Read a projected file by path, allowing ordinary symlinks but never
    /// magic links.
    pub async fn read_path(&self, path: &Path, limit: usize, ctx: &OpCtx) -> Result<ReadBuffer> {
        let name = path_name(path.as_os_str(), PATH_BYTES)?;
        let fd = self
            .open(None, name, libc::O_RDONLY, NO_MAGICLINKS, ctx)?
            .await?;
        self.read_small(Rc::new(fd), limit, false, ctx).await
    }

    /// Read one direct child of `dir` without following symlinks.
    pub async fn read_child(
        &self,
        dir: &Rc<Descriptor>,
        name: &str,
        limit: usize,
        private: bool,
        ctx: &OpCtx,
    ) -> Result<ReadBuffer> {
        let name = component(name.as_ref(), PATH_BYTES)?;
        let fd = self
            .open(
                Some(dir.clone()),
                name,
                libc::O_RDONLY,
                BENEATH | NO_SYMLINKS,
                ctx,
            )?
            .await?;
        self.read_small(Rc::new(fd), limit, private, ctx).await
    }

    /// Remove `name` (absent is fine) and sync the parent.
    pub async fn remove_synced(
        &self,
        dir: Rc<Descriptor>,
        name: CString,
        ctx: &OpCtx,
    ) -> Result<()> {
        component(OsStr::from_bytes(name.as_bytes()), PATH_BYTES)?;
        self.unlink(dir.clone(), name, 0, true, ctx)?.await?;
        self.fsync(dir, false, ctx).await
    }

    /// Remove any stale stage, then exclusively create an owner-only stage.
    pub async fn create_stage(
        &self,
        dir: Rc<Descriptor>,
        temporary: &OsStr,
        ctx: &OpCtx,
    ) -> Result<Rc<Descriptor>> {
        let name = component(temporary, PATH_BYTES)?;
        self.remove_synced(dir.clone(), name.clone(), ctx).await?;
        let flags = libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL;
        let fd = self
            .open(Some(dir), name, flags, BENEATH | NO_SYMLINKS, ctx)?
            .await?;
        Ok(Rc::new(fd))
    }

    /// Write `bytes` into a fresh, empty, single-link stage through scratch
    /// of at most `chunk` bytes, then atomically rename it over the target.
    pub async fn replace(
        &self,
        replacement: Replacement,
        bytes: &[u8],
        chunk: NonZeroUsize,
        ctx: &OpCtx,
    ) -> std::result::Result<(), ReplacementError> {
        use ReplacementError::*;
        let staged = self
            .verify_stage(&replacement, ctx)
            .await
            .map_err(BeforeRename)?;
        let mut scratch = Buffer::new(bytes.len().min(chunk.get())).map_err(BeforeRename)?;
        let mut offset = 0u64;
        for part in bytes.chunks(chunk.get()) {
            scratch.fill(part);
            while scratch.remaining() != 0 {
                let done = self
                    .write_at(staged.clone(), offset, scratch, (), ctx)
                    .await
                    .map_err(BeforeRename)?;
                scratch = done.buffer;
                scratch.advance(done.bytes).map_err(BeforeRename)?;
                offset += done.bytes as u64;
            }
        }
        let durable = replacement.durability == Durability::FileAndDirectory;
        if durable {
            self.fsync(staged, false, ctx).await.map_err(BeforeRename)?;
        }
        let rename = self
            .rename(
                replacement.directory.clone(),
                replacement.temporary,
                replacement.target,
                0,
                ctx,
            )
            .map_err(BeforeRename)?;
        rename.await.map_err(RenameUncertain)?;
        if durable {
            self.fsync(replacement.directory, false, ctx)
                .await
                .map_err(Published)?;
        }
        Ok(())
    }

    /// Verify a fresh single-link stage and reopen its exact inode without
    /// append mode.
    async fn verify_stage(&self, replacement: &Replacement, ctx: &OpCtx) -> Result<Rc<Descriptor>> {
        for name in [&replacement.temporary, &replacement.target] {
            component(OsStr::from_bytes(name.as_bytes()), PATH_BYTES)?;
        }
        if replacement.temporary == replacement.target {
            return Err(Error::InvalidInput);
        }
        let required = libc::STATX_INO | libc::STATX_NLINK;
        let original = self.stat(replacement.staged.clone(), ctx).await?;
        check_regular_size(&original, 0)?;
        if original.stx_mask & required != required || original.stx_nlink != 1 {
            return Err(Error::InvalidInput);
        }
        let staged = Rc::new(
            self.open(
                Some(replacement.directory.clone()),
                replacement.temporary.clone(),
                libc::O_WRONLY,
                BENEATH | NO_SYMLINKS,
                ctx,
            )?
            .await?,
        );
        let current = self.stat(staged.clone(), ctx).await?;
        check_regular_size(&current, 0)?;
        if current.stx_mask & required != required
            || current.stx_nlink != 1
            || current.stx_ino != original.stx_ino
            || current.stx_dev_major != original.stx_dev_major
            || current.stx_dev_minor != original.stx_dev_minor
        {
            return Err(Error::InvalidInput);
        }
        Ok(staged)
    }

    /// Durably replace `target` in `dir` with `bytes` (at most
    /// [`SMALL_FILE_BYTES`]) through the stage `.<target>.stage`.
    pub async fn atomic_write(
        &self,
        dir: &Rc<Descriptor>,
        target: &str,
        bytes: &[u8],
        ctx: &OpCtx,
    ) -> std::result::Result<(), ReplacementError> {
        use ReplacementError::BeforeRename;
        let target_name = component(target.as_ref(), PATH_BYTES).map_err(BeforeRename)?;
        if bytes.len() > SMALL_FILE_BYTES {
            return Err(BeforeRename(Error::Overloaded));
        }
        let temporary = format!(".{target}.stage");
        let staged = self
            .create_stage(dir.clone(), temporary.as_ref(), ctx)
            .await
            .map_err(BeforeRename)?;
        self.replace(
            Replacement {
                directory: dir.clone(),
                staged,
                temporary: component(temporary.as_ref(), PATH_BYTES).map_err(BeforeRename)?,
                target: target_name,
                durability: Durability::FileAndDirectory,
            },
            bytes,
            NonZeroUsize::new(64 * 1024).unwrap(),
            ctx,
        )
        .await
    }
}

#[cfg(test)]
mod tests;
