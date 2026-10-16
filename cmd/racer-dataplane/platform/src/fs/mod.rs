//! Filesystem operations on the reactor, sharing its completion fences.
//!
//! Ported from the old runtime's `reactor::filesystem`, now built on the
//! library's typed operations (`open_at2`, `statx`, `fsync`, `rename_at`,
//! `unlink_at`, `mkdir_at`). Every descriptor, path and quota is retained by
//! the reactor until the operation is fenced. Metadata operations and bulk
//! file reads and writes ask for kernel-worker execution (`IOSQE_ASYNC`), as
//! before.
//!
//! Old path `uring_runtime::reactor::filesystem::X` maps to
//! `racer_platform::fs::X`.

use crate::{
    Budget, Error, Operation, Result, Scope,
    reactor::{
        Core, Failure, IoBuffer, OpScope, RacerBackend, Reactor, SubmissionKind,
        descriptor::Descriptor,
    },
};
use std::{
    any::Any,
    cell::{Cell, RefCell},
    ffi::CString,
    num::NonZeroUsize,
    ops::Deref,
    os::unix::ffi::OsStrExt,
    path::PathBuf,
    rc::Rc,
};
use uring::backend::OpenHow;
use uring::reactor::OpSpec;
use zeroize::Zeroize;

pub mod checkpoint;
pub mod operations;
pub mod secure;

/// Erased admission guard retained for the lifetime of its accounted owner.
type Charge = Box<dyn Any>;

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
    type Target = [u8];

    /// Borrow only initialized output, not the unused bounded-read capacity.
    fn deref(&self) -> &[u8] {
        &self.buffer.data[..self.buffer.end]
    }
}

impl AsRef<[u8]> for ReadBuffer {
    fn as_ref(&self) -> &[u8] {
        self
    }
}

// SAFETY: private non-resizing Vec retains allocation and quota through completion.
unsafe impl IoBuffer for Buffer {
    type Error = Error;

    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.data[self.start..self.end])
    }

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

/// Linux's pathname limit excluding NUL; filesystems may impose smaller components.
const PATH_BYTES: usize = 4095;

impl<S: Scope, B: Budget> Reactor<S, B> {
    /// Allocate zero-initialized, quota-owning storage sized for one I/O submission.
    pub fn file_buffer(&self, length: usize) -> Result<Buffer> {
        // Read/write lengths are u32, not an application-specific cap.
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
                    .read_at_offloaded(fd.clone(), out.buffer.end as u64, buffer, (), scope)
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

    /// Submit one filesystem operation in a kernel worker, retaining `quota`
    /// until its fence.
    async fn file_submit<'a, O: OpSpec<RacerBackend>>(
        &'a self,
        scope: &'a S,
        quota: Charge,
        build: impl FnOnce(
            &'a Core,
            &'a OpScope<S>,
        ) -> uring::reactor::Operation<'a, RacerBackend, O, OpScope<S>>,
    ) -> Result<O::Output<Failure<S::Error>>, S::Error> {
        let bytes = Self::bookkeeping::<Charge, O>();
        let (core, held) = self.admit(scope, None, SubmissionKind::OtherIo, bytes, quota)?;
        let (output, held) = build(core, OpScope::wrap(scope))
            .offload()
            .lease(held)
            .await;
        drop(held);
        Ok(output)
    }

    /// Open relative to `dir` (or the current directory). This secure
    /// convenience API creates owner-only files (0600, reduced by umask); O_PATH
    /// accepts only DIRECTORY/NOFOLLOW/CLOEXEC, without forced NONBLOCK.
    /// Linux's 4096-byte path limit includes NUL; NAME_MAX still applies.
    /// An opened descriptor that is never collected is closed after the fence.
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
            let how = OpenHow {
                flags: flags as u64,
                mode,
                resolve,
            };
            self.file_submit(scope, quota, |core, op_scope| {
                core.open_at2(dir.as_ref(), &path, how, op_scope)
            })
            .await?
            .map_err(Failure::into_scope)
        })
    }

    /// Read basic metadata through an owned descriptor.
    pub fn file_stat<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        scope: &'a S,
    ) -> Operation<'a, libc::statx, S::Error> {
        Box::pin(async move {
            let quota = self.charge(std::mem::size_of::<libc::statx>())?;
            self.file_submit(scope, quota, |core, op_scope| {
                core.statx(
                    Some(&fd),
                    c"",
                    libc::AT_EMPTY_PATH,
                    libc::STATX_BASIC_STATS,
                    op_scope,
                )
            })
            .await?
            .map_err(Failure::into_scope)
        })
    }

    /// Sync a file or directory while retaining its descriptor through the fence.
    pub fn file_sync<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            self.file_submit(scope, Box::new(()), |core, op_scope| {
                core.fsync(&fd, false, op_scope)
            })
            .await?
            .map_err(Failure::into_scope)
        })
    }

    /// Rename within a pinned directory; cancellation is not proof it did not
    /// execute. Callers own name validation, namespace serialization, and
    /// durability policy.
    pub(crate) fn file_rename<'a>(
        &'a self,
        dir: Rc<Descriptor>,
        from: CString,
        to: CString,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let quota = self.file_path_quota(&[&from, &to])?;
            self.file_submit(scope, quota, |core, op_scope| {
                core.rename_at(Some(&dir), &from, Some(&dir), &to, 0, op_scope)
            })
            .await?
            .map_err(Failure::into_scope)
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
            let result = match change {
                NamespaceChange::EnsureDirectory => {
                    self.file_submit(scope, quota, |core, op_scope| {
                        core.mkdir_at(Some(&dir), &name, 0o700, op_scope)
                    })
                    .await?
                }
                NamespaceChange::Unlink | NamespaceChange::RemoveIfPresent => {
                    self.file_submit(scope, quota, |core, op_scope| {
                        core.unlink_at(Some(&dir), &name, 0, op_scope)
                    })
                    .await?
                }
            };
            change.complete(result)
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

/// A namespace mutation paired with the only kernel error it may treat as success.
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
    fn complete<E: From<Error>>(self, result: Result<(), Failure<E>>) -> Result<(), E> {
        let accepted = match self {
            Self::EnsureDirectory => Some(libc::EEXIST),
            Self::Unlink => None,
            Self::RemoveIfPresent => Some(libc::ENOENT),
        };
        match result {
            Err(Failure::Runtime(error))
                if accepted.is_some() && error.raw_os_error() == accepted =>
            {
                Ok(())
            }
            result => result.map_err(Failure::into_scope),
        }
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

#[cfg(test)]
mod tests;
