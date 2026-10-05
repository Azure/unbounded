//! Bounded reads and staged replacement, with file policy supplied by the caller.
use super::*;
use std::{ffi::OsStr, num::NonZeroUsize, ops::Deref};

/// The directory must be pinned, trusted, and exclusively controlled throughout
/// this operation. Both names must be distinct single components. The stage must
/// be an empty, singly linked regular file. A verified, nonappend description is
/// reopened for writes, so an O_APPEND description supplied here is never used.
/// Callers serialize access to the stage inode and directory, including via aliases.
pub struct Replacement {
    pub directory: Rc<Descriptor>,
    pub staged: Rc<Descriptor>,
    pub temporary: CString,
    pub target: CString,
    pub durability: Durability,
}

/// Where publication stopped. An accepted rename may execute even if cancellation
/// wins its completion race. Dropping the future likewise provides no publication
/// outcome; callers must fence outstanding I/O and reconcile the namespace.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ReplacementError<E> {
    BeforeRename(E),
    RenameUncertain(E),
    /// Rename completed successfully; only the parent durability fence failed.
    Published(E),
}
impl<E: Copy> ReplacementError<E> {
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

/// Read output owns its admission charge until drop. Storage is allocated once,
/// never reallocated with secret bytes, and fully zeroized before release.
pub struct ReadBuffer {
    data: Vec<u8>,
    length: usize,
    _quota: Charge,
}
impl Deref for ReadBuffer {
    type Target = [u8];
    fn deref(&self) -> &[u8] {
        &self.data[..self.length]
    }
}
impl AsRef<[u8]> for ReadBuffer {
    fn as_ref(&self) -> &[u8] {
        self
    }
}
impl Drop for ReadBuffer {
    fn drop(&mut self) {
        self.data.as_mut_slice().zeroize();
    }
}

impl<S: Scope, B: Budget> Reactor<S, B> {
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
            let quota = self.charge(limit)?;
            let mut data = Vec::new();
            data.try_reserve_exact(limit)
                .map_err(|_| Error::Overloaded)?;
            data.resize(limit, 0);
            let mut out = ReadBuffer {
                data,
                length: 0,
                _quota: quota,
            };
            let mut buffer = self.file_buffer(probe.min(chunk.get()))?;
            loop {
                buffer.end = (probe - out.length).min(buffer.data.len());
                let completion = self
                    .read_at(fd.clone(), out.length as u64, buffer, (), scope)
                    .await?;
                let n = completion.bytes;
                buffer = completion.buffer;
                let bytes = buffer.prefix(n)?;
                if n == 0 {
                    return Ok(out);
                }
                if n > limit - out.length {
                    return Err(Error::Overloaded.into());
                }
                out.data[out.length..out.length + n].copy_from_slice(bytes);
                out.length += n;
            }
        })
    }

    pub(super) async fn prepare_replacement(
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

    pub(super) async fn write_complete(
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

    pub(super) async fn publish_replacement(
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
}
