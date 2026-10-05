//! Worker-local pathname publication, independent of service/control activation.
//! Callers stage every endpoint before publishing and serialize directory access.

use crate::{BoundSocket, file_path, same_inode};
use std::ffi::CString;
use std::fs;
use std::io;
use std::os::fd::AsRawFd;
use std::os::unix::fs::{FileTypeExt, MetadataExt};
use std::rc::Rc;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Rename {
    Exchange,
    NoReplace,
}

impl Rename {
    pub fn flags(self) -> u32 {
        match self {
            Self::Exchange => libc::RENAME_EXCHANGE,
            Self::NoReplace => libc::RENAME_NOREPLACE,
        }
    }
}

/// Minimal inode owner used by the transaction. Implementations must retain their
/// pinned directory and clean up only their own socket inode on final drop.
/// `set_basename` changes only in-memory cleanup bookkeeping after a rename.
/// The application can adapt a test-only filesystem without changing this logic.
pub trait Endpoint {
    type Error;
    fn ownership_error() -> Self::Error;
    fn owns(&self, basename: &str) -> bool;
    fn absent(&self, basename: &str) -> bool;
    fn same_directory(&self, other: &Self) -> Result<bool, Self::Error>;
    fn rename(&self, from: &str, to: &str, mode: Rename) -> Result<(), Self::Error>;
    fn set_basename(&self, basename: String);
}

impl Endpoint for BoundSocket {
    type Error = io::Error;

    fn ownership_error() -> Self::Error {
        super::rejected()
    }

    fn owns(&self, basename: &str) -> bool {
        fs::symlink_metadata(file_path(&self.directory).join(basename)).is_ok_and(|metadata| {
            metadata.file_type().is_socket()
                && metadata.dev() == self.device
                && metadata.ino() == self.inode
        })
    }

    fn absent(&self, basename: &str) -> bool {
        fs::symlink_metadata(file_path(&self.directory).join(basename))
            .is_err_and(|error| error.kind() == io::ErrorKind::NotFound)
    }

    fn same_directory(&self, other: &Self) -> io::Result<bool> {
        Ok(same_inode(
            &self.directory.metadata()?,
            &other.directory.metadata()?,
        ))
    }

    fn rename(&self, from: &str, to: &str, mode: Rename) -> io::Result<()> {
        let from = CString::new(from).map_err(|_| io::Error::from(io::ErrorKind::InvalidInput))?;
        let to = CString::new(to).map_err(|_| io::Error::from(io::ErrorKind::InvalidInput))?;
        // SAFETY: the owned directory pins both names; strings are NUL terminated.
        if unsafe {
            libc::renameat2(
                self.directory.as_raw_fd(),
                from.as_ptr(),
                self.directory.as_raw_fd(),
                to.as_ptr(),
                mode.flags(),
            )
        } != 0
        {
            return Err(io::Error::last_os_error());
        }
        Ok(())
    }

    fn set_basename(&self, basename: String) {
        *self.basename.borrow_mut() = basename;
    }
}

/// Staged names and retained inode owners, not yet exchanged. Names must be
/// distinct single components in the endpoint's caller-validated namespace.
pub struct Replacement<E: Endpoint> {
    next: Rc<E>,
    previous: Option<Rc<E>>,
    temporary: String,
    canonical: String,
}

impl<E: Endpoint> Replacement<E> {
    /// Check previous canonical ownership and directory identity, or absence for
    /// a new endpoint. This performs no rename and leaves staged cleanup to E.
    pub fn prepare(
        next: Rc<E>,
        previous: Option<Rc<E>>,
        temporary: String,
        canonical: String,
    ) -> Result<Self, E::Error> {
        if let Some(previous) = &previous {
            if !previous.owns(&canonical) || !previous.same_directory(&next)? {
                return Err(E::ownership_error());
            }
        } else if !next.absent(&canonical) {
            return Err(E::ownership_error());
        }
        Ok(Self {
            next,
            previous,
            temporary,
            canonical,
        })
    }
}

/// A rollback journal. Publish only after all endpoints have been staged.
/// Drop rolls back in reverse order; commit only disarms rollback, retaining
/// owners so the caller can defer cleanup outside an infallible control commit.
pub struct Publication<E: Endpoint> {
    replacements: Vec<Replacement<E>>,
    completed: bool,
}

impl<E: Endpoint> Default for Publication<E> {
    fn default() -> Self {
        Self {
            replacements: Vec::new(),
            completed: false,
        }
    }
}

impl<E: Endpoint> Publication<E> {
    pub fn publish(&mut self, replacement: Replacement<E>) -> Result<(), E::Error> {
        assert!(!self.completed, "publication already completed");
        let next = &replacement.next;
        if let Some(previous) = &replacement.previous {
            if !previous.owns(&replacement.canonical) || !next.owns(&replacement.temporary) {
                return Err(E::ownership_error());
            }
            next.rename(
                &replacement.temporary,
                &replacement.canonical,
                Rename::Exchange,
            )?;
            previous.set_basename(replacement.temporary.clone());
        } else {
            // No-replace itself protects an endpoint that appeared after prepare.
            next.rename(
                &replacement.temporary,
                &replacement.canonical,
                Rename::NoReplace,
            )?;
        }
        next.set_basename(replacement.canonical.clone());
        self.replacements.push(replacement);
        Ok(())
    }

    pub fn previous(&self) -> impl Iterator<Item = &Rc<E>> {
        self.replacements
            .iter()
            .filter_map(|entry| entry.previous.as_ref())
    }

    /// No filesystem calls, allocations, or owner drops.
    pub fn commit(&mut self) {
        self.completed = true;
    }

    /// Best-effort, scope-independent rollback; never exchange foreign inodes.
    /// An absent canonical name can be restored from the owned previous staging
    /// name. New endpoints without previous owners are cleaned up on final drop.
    /// This is idempotent even when a rollback rename fails.
    pub fn rollback(&mut self) {
        if self.completed {
            return;
        }
        for replacement in self.replacements.iter().rev() {
            let next = &replacement.next;
            if let Some(previous) = &replacement.previous {
                if next.owns(&replacement.canonical) && previous.owns(&replacement.temporary) {
                    if next
                        .rename(
                            &replacement.canonical,
                            &replacement.temporary,
                            Rename::Exchange,
                        )
                        .is_ok()
                    {
                        next.set_basename(replacement.temporary.clone());
                        previous.set_basename(replacement.canonical.clone());
                    }
                } else if next.absent(&replacement.canonical)
                    && previous.owns(&replacement.temporary)
                    && next
                        .rename(
                            &replacement.temporary,
                            &replacement.canonical,
                            Rename::NoReplace,
                        )
                        .is_ok()
                {
                    previous.set_basename(replacement.canonical.clone());
                }
            }
        }
        self.completed = true;
    }
}

impl<E: Endpoint> Drop for Publication<E> {
    fn drop(&mut self) {
        self.rollback();
    }
}
