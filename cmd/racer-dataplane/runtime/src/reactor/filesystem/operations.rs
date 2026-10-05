//! Bounded reads and staged replacement, with file policy supplied by the caller.
use super::*;
use std::num::NonZeroUsize;
use zeroize::Zeroizing;

/// The caller owns stage creation, permissions, name validation, and serialization
/// against other replacements. Both names are relative to `directory`.
pub struct Replacement {
    pub directory: Rc<Descriptor>,
    pub staged: Rc<Descriptor>,
    pub temporary: CString,
    pub target: CString,
    pub durability: Durability,
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
    /// Read from offset zero through EOF, probing one byte beyond `limit`.
    /// Callers validate file type, initial stat size, permissions, and service
    /// ceilings before calling. Growth past the limit returns Overloaded.
    pub fn file_read_bounded<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        limit: usize,
        chunk: NonZeroUsize,
        scope: &'a S,
    ) -> Operation<'a, Zeroizing<Vec<u8>>, S::Error> {
        Box::pin(async move {
            let probe = limit.checked_add(1).ok_or(Error::InvalidInput)?;
            let mut out = Zeroizing::new(Vec::new());
            loop {
                let buffer = self.file_buffer((probe - out.len()).min(chunk.get()))?;
                let completion = self
                    .read_at(fd.clone(), out.len() as u64, buffer, (), scope)
                    .await?;
                if completion.bytes == 0 {
                    return Ok(out);
                }
                out.extend_from_slice(completion.buffer.prefix(completion.bytes)?);
                if out.len() > limit {
                    return Err(Error::Overloaded.into());
                }
            }
        })
    }

    /// Write the caller-prepared stage completely, then atomically rename it.
    /// Failure or cancellation leaves cleanup to the caller; accepted I/O keeps
    /// its normal reactor fences. Empty buffers may publish an empty file.
    pub fn file_replace<'a>(
        &'a self,
        replacement: Replacement,
        mut buffer: Buffer,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let Replacement {
                directory,
                staged,
                temporary,
                target,
                durability,
            } = replacement;
            let mut offset = 0;
            while buffer.remaining() != 0 {
                let completion = self
                    .write_at(staged.clone(), offset, buffer, (), scope)
                    .await?;
                buffer = completion.buffer;
                buffer.advance(completion.bytes)?;
                offset += completion.bytes as u64;
            }
            if matches!(durability, Durability::FileAndDirectory) {
                self.file_sync(staged.clone(), scope).await?;
            }
            drop(staged);
            self.file_rename(directory.clone(), temporary, target, scope)
                .await?;
            if matches!(durability, Durability::FileAndDirectory) {
                self.file_sync(directory, scope).await?;
            }
            Ok(())
        })
    }
}
