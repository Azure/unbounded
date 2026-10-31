//! Worker-local pathname publication, independent of service/control activation.
//! Callers stage every endpoint before publishing and serialize directory access.

use crate::directory::{Dir, component};
use crate::{BoundSocket, Error, error, same_inode};
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt};
use std::rc::Rc;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Rename {
    Exchange,
    NoReplace,
}

impl Rename {
    #[must_use]
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
/// It must not panic, allocate, or invoke user code. The same applies to a
/// successful return from `rename`: implementations must report success without
/// unwinding after the filesystem mutation. These are low-level adapter methods,
/// not an alternative API for directly changing a socket's cleanup name.
/// The application can adapt a test-only filesystem without changing this logic.
pub trait Endpoint {
    type Error;
    fn ownership_error() -> Self::Error;
    fn owns(&self, basename: &str) -> bool;
    fn absent(&self, basename: &str) -> bool;
    /// Compare retained directory identities.
    ///
    /// # Errors
    /// Returns the adapter's validation or metadata error.
    fn same_directory(&self, other: &Self) -> Result<bool, Self::Error>;
    /// Perform a descriptor-relative rename under serialized directory access.
    ///
    /// # Errors
    /// Returns the adapter's validation or rename error without changing cleanup
    /// bookkeeping. An error must mean the rename did not take place.
    fn rename(&self, from: &str, to: &str, mode: Rename) -> Result<(), Self::Error>;
    fn set_basename(&self, basename: String);

    /// Adapters can preserve diagnostic stat errors instead of collapsing them
    /// into a failed ownership check. Existing boolean-only adapters still work.
    ///
    /// # Errors
    /// Returns the adapter's probe error if ownership could not be determined.
    fn owns_checked(&self, basename: &str) -> Result<bool, Self::Error> {
        Ok(self.owns(basename))
    }
    /// Determine absence without treating inaccessible paths as missing.
    ///
    /// # Errors
    /// Returns the adapter's probe error if absence could not be determined.
    fn absent_checked(&self, basename: &str) -> Result<bool, Self::Error> {
        Ok(self.absent(basename))
    }
    /// Validate distinct components and any adapter-specific namespace policy.
    ///
    /// # Errors
    /// Returns the adapter's validation error for malformed/overlapping names or
    /// an invalid owner. The default checks only component syntax and inequality.
    fn validate_names(&self, temporary: &str, canonical: &str) -> Result<(), Self::Error> {
        if temporary == canonical
            || component(temporary.as_bytes()).is_err()
            || component(canonical.as_bytes()).is_err()
        {
            return Err(Self::ownership_error());
        }
        Ok(())
    }
    fn completed_error() -> Self::Error {
        Self::ownership_error()
    }
    fn allocation_error() -> Self::Error {
        Self::ownership_error()
    }
}

impl Endpoint for BoundSocket {
    type Error = io::Error;

    fn ownership_error() -> Self::Error {
        super::rejected()
    }

    fn owns(&self, basename: &str) -> bool {
        self.owns_checked(basename).unwrap_or(false)
    }

    fn absent(&self, basename: &str) -> bool {
        self.absent_checked(basename).unwrap_or(false)
    }

    fn owns_checked(&self, basename: &str) -> io::Result<bool> {
        match Dir(&self.owner.directory).metadata(basename) {
            Ok(metadata) => Ok(metadata.file_type().is_socket()
                && metadata.dev() == self.device
                && metadata.ino() == self.inode),
            Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(false),
            Err(e) => Err(e),
        }
    }

    fn absent_checked(&self, basename: &str) -> io::Result<bool> {
        match Dir(&self.owner.directory).metadata(basename) {
            Ok(_) => Ok(false),
            Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(true),
            Err(e) => Err(e),
        }
    }

    fn validate_names(&self, temporary: &str, canonical: &str) -> io::Result<()> {
        self.owner.validate(&self.owner.directory)?;
        self.owner.layout.witness(temporary)?;
        if canonical != self.owner.layout.canonical {
            return Err(error(Error::InvalidLayout));
        }
        Ok(())
    }

    fn completed_error() -> io::Error {
        error(Error::PublicationCompleted)
    }
    fn allocation_error() -> io::Error {
        io::Error::new(
            io::ErrorKind::OutOfMemory,
            "cannot reserve endpoint publication journal",
        )
    }

    fn same_directory(&self, other: &Self) -> io::Result<bool> {
        Ok(same_inode(
            &self.owner.directory.metadata()?,
            &other.owner.directory.metadata()?,
        ))
    }

    fn rename(&self, from: &str, to: &str, mode: Rename) -> io::Result<()> {
        self.owner.validate(&self.owner.directory)?;
        if from == to {
            return Err(error(Error::InvalidName));
        }
        Dir(&self.owner.directory).rename(from, to, mode.flags())
    }

    fn set_basename(&self, basename: String) {
        *self.basename.borrow_mut() = basename;
    }
}

/// Staged names and retained inode owners, not yet exchanged. Names must be
/// distinct single components in the endpoint's caller-validated namespace.
#[derive(Debug)]
pub struct Replacement<E: Endpoint> {
    next: Rc<E>,
    previous: Option<Rc<E>>,
    temporary: String,
    canonical: String,
    published_next: Option<String>,
    published_previous: Option<String>,
    restored_next: Option<String>,
    restored_previous: Option<String>,
    published: bool,
}

impl<E: Endpoint> Replacement<E> {
    /// Check previous canonical ownership and directory identity, or absence for
    /// a new endpoint. This performs no rename and leaves staged cleanup to E.
    ///
    /// # Errors
    /// Returns the adapter's validation/probe error for invalid names, directory
    /// mismatch, identical owners/inodes, missing staging ownership, or an
    /// unrecognized canonical entry.
    pub fn prepare(
        next: Rc<E>,
        previous: Option<Rc<E>>,
        temporary: String,
        canonical: String,
    ) -> Result<Self, E::Error> {
        if temporary == canonical
            || component(temporary.as_bytes()).is_err()
            || component(canonical.as_bytes()).is_err()
        {
            return Err(E::ownership_error());
        }
        next.validate_names(&temporary, &canonical)?;
        if !next.owns_checked(&temporary)? {
            return Err(E::ownership_error());
        }
        if let Some(previous) = &previous {
            previous.validate_names(&temporary, &canonical)?;
            if Rc::ptr_eq(previous, &next)
                || !previous.same_directory(&next)?
                || !previous.owns_checked(&canonical)?
                || next.owns_checked(&canonical)?
                || previous.owns_checked(&temporary)?
            {
                return Err(E::ownership_error());
            }
        } else if !next.absent_checked(&canonical)? {
            return Err(E::ownership_error());
        }
        Ok(Self {
            published_next: Some(canonical.clone()),
            published_previous: previous.as_ref().map(|_| temporary.clone()),
            restored_next: Some(temporary.clone()),
            restored_previous: previous.as_ref().map(|_| canonical.clone()),
            published: false,
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
#[derive(Debug)]
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
    /// Publish one prepared endpoint after rechecking its ownership guards.
    ///
    /// # Errors
    /// Returns the adapter's completed error after commit/rollback, allocation
    /// error if journal reservation fails, or validation/probe/rename error.
    /// Earlier successful entries remain journaled for rollback.
    pub fn publish(&mut self, replacement: Replacement<E>) -> Result<(), E::Error> {
        if self.completed {
            return Err(E::completed_error());
        }
        let next = &replacement.next;
        next.validate_names(&replacement.temporary, &replacement.canonical)?;
        if !next.owns_checked(&replacement.temporary)? {
            return Err(E::ownership_error());
        }
        if let Some(previous) = &replacement.previous {
            previous.validate_names(&replacement.temporary, &replacement.canonical)?;
            if !previous.same_directory(next)?
                || !previous.owns_checked(&replacement.canonical)?
                || next.owns_checked(&replacement.canonical)?
                || previous.owns_checked(&replacement.temporary)?
            {
                return Err(E::ownership_error());
            }
        }
        // All journal storage and both forward/rollback cleanup strings exist
        // before rename. Install the entry first so unwinding during bookkeeping
        // cannot lose a completed rename. Adapters must honor Endpoint's contract.
        self.replacements
            .try_reserve(1)
            .map_err(|_| E::allocation_error())?;
        self.replacements.push(replacement);
        let entry = self.replacements.last_mut().expect("entry just inserted");
        let mode = if entry.previous.is_some() {
            Rename::Exchange
        } else {
            Rename::NoReplace
        };
        if let Err(e) = entry.next.rename(&entry.temporary, &entry.canonical, mode) {
            self.replacements.pop();
            return Err(e);
        }
        entry.published = true;
        if let (Some(previous), Some(name)) = (&entry.previous, entry.published_previous.take()) {
            previous.set_basename(name);
        }
        if let Some(name) = entry.published_next.take() {
            entry.next.set_basename(name);
        }
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
    /// name. New endpoints move back to their vacant temporary name even when
    /// external references keep their listeners alive.
    /// This is idempotent even when a rollback rename fails.
    pub fn rollback(&mut self) {
        if self.completed {
            return;
        }
        // Disarm before invoking adapters: rollback is attempted only once even
        // if an adapter violates the no-panic contract during unwinding.
        self.completed = true;
        for replacement in self.replacements.iter_mut().rev() {
            if !replacement.published {
                continue;
            }
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
                        if let Some(name) = replacement.restored_next.take() {
                            next.set_basename(name);
                        }
                        if let Some(name) = replacement.restored_previous.take() {
                            previous.set_basename(name);
                        }
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
                    && let Some(name) = replacement.restored_previous.take()
                {
                    previous.set_basename(name);
                }
            } else if next.owns(&replacement.canonical)
                && next.absent(&replacement.temporary)
                && next
                    .rename(
                        &replacement.canonical,
                        &replacement.temporary,
                        Rename::NoReplace,
                    )
                    .is_ok()
                && let Some(name) = replacement.restored_next.take()
            {
                next.set_basename(name);
            }
        }
    }
}

impl<E: Endpoint> Drop for Publication<E> {
    fn drop(&mut self) {
        self.rollback();
    }
}
