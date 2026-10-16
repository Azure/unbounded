//! Descriptor-relative traversal, metadata primitives, and secure policy helpers.
//! The primitives accept caller-selected bounds and access requirements. Composed
//! helpers cap files at 1 MiB and optionally require owner-only permissions and
//! a single link for regular files, using the descriptor's actual backend.
use super::*;
use crate::scope::LibraryScope;
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
    ) -> Result<Self::Scope, <Self::Scope as LibraryScope>::Error>;

    /// Fence all submissions of the previous attempt, including abandoned futures.
    fn fence<'a>(
        &'a self,
        previous: &'a Self::Scope,
    ) -> Operation<'a, (), <Self::Scope as LibraryScope>::Error>;

    /// Identify only the caller's missing-file error, never cancellation.
    fn is_missing(error: <Self::Scope as LibraryScope>::Error) -> bool;
}

impl<H: Host> Host for Rc<H> {
    /// Preserve the underlying host's cancellation and error policy.
    type Scope = H::Scope;

    /// Preserve the underlying host's retained accounting guards.
    type Budget = H::Budget;

    /// Borrow the same worker-local reactor through shared host ownership.
    fn reactor(&self) -> &Reactor<Self::Scope, Self::Budget> {
        (**self).reactor()
    }

    /// Delegate independent attempt allocation without replacing parent policy.
    fn fresh_scope(
        &self,
        parent: &Self::Scope,
    ) -> Result<Self::Scope, <Self::Scope as LibraryScope>::Error> {
        (**self).fresh_scope(parent)
    }

    /// Retain shared host ownership while the previous attempt is fenced.
    fn fence<'a>(
        &'a self,
        previous: &'a Self::Scope,
    ) -> Operation<'a, (), <Self::Scope as LibraryScope>::Error> {
        (**self).fence(previous)
    }

    /// Preserve the host's exact missing-file classification.
    fn is_missing(error: <Self::Scope as LibraryScope>::Error) -> bool {
        H::is_missing(error)
    }
}

/// Serializes a namespace owner and retains abandoned attempts until fenced.
pub struct Attempts<S: Scope> {
    busy: Cell<bool>,

    previous: RefCell<Option<S>>,
}

impl<S: Scope> Default for Attempts<S> {
    /// Start an idle namespace owner without a previous attempt to fence.
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

/// Check host metadata for owner-only access using the process's effective UID.
/// Require one link for regular files. Explicit-owner callers use [`check_access`].
pub fn check_private(stat: &libc::statx, regular: bool) -> Result<(), AccessError> {
    // SAFETY: geteuid has no memory or lifetime preconditions.
    check_access(
        stat,
        AccessRequirements {
            owner: unsafe { libc::geteuid() },
            forbidden_mode: 0o077,
            links: regular.then_some(1),
        },
    )
}

/// Check metadata in its descriptor's ownership domain, not the ambient
/// environment. A [`Simulation`](crate::simulation::Simulation) reports the
/// process's effective UID as the owner (see `World::set_owner`), so
/// simulated descriptors share the host check; a test can make the world
/// report another owner to exercise the rejection.
fn check_descriptor_private(
    fd: &Descriptor,
    stat: &libc::statx,
    regular: bool,
) -> Result<(), AccessError> {
    let _ = fd;
    check_private(stat, regular)
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
        check_descriptor_private(&fd, &r.file_stat(fd.clone(), scope).await?, false)?;
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
        check_descriptor_private(&fd, &stat, true)?;
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
            temporary: component(temporary.as_ref(), 4096).map_err(|e| BeforeRename(e.into()))?,
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
pub fn check_access(stat: &libc::statx, required: AccessRequirements) -> Result<(), AccessError> {
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
pub(crate) fn validate_component(name: &OsStr, limit: usize) -> Result<()> {
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
    use std::task::{Context, Poll};

    /// Serialize namespace use and retain failed or abandoned fences for retry.
    #[test]
    fn attempts_retain_previous_scope_until_fence_and_fresh_scope_succeed() {
        /// Identify independent attempts while allowing parent admission failure.
        #[derive(Clone)]
        struct Request(u64, bool);

        impl LibraryScope for Request {
            /// Keep fixture failures in the runtime boundary.
            type Error = Error;

            /// Reject canceled parent requests before any host callbacks.
            fn check(&self) -> Result<()> {
                if self.1 { Err(Error::Canceled) } else { Ok(()) }
            }
        }

        /// Record fence order and inject pending, failed, and successful host callbacks.
        struct Hosting {
            reactor: Reactor<Request, ()>,

            next: Cell<u64>,

            fences: RefCell<Vec<u64>>,

            ready: Cell<bool>,

            fail_fence: Cell<bool>,

            fail_fresh: Cell<bool>,
        }

        impl Host for Hosting {
            /// Preserve attempt identity in each fenced scope.
            type Scope = Request;

            /// This callback-only fixture owns no I/O allocation charges.
            type Budget = ();

            /// Expose a lazy reactor without allocating a kernel ring.
            fn reactor(&self) -> &Reactor<Request, ()> {
                &self.reactor
            }

            /// Allocate a new identity only after successful fencing.
            fn fresh_scope(&self, _: &Request) -> Result<Request> {
                if self.fail_fresh.get() {
                    return Err(Error::Unavailable);
                }
                let next = self.next.get() + 1;
                self.next.set(next);
                Ok(Request(next, false))
            }

            /// Hold the prior identity until the caller explicitly permits its fence.
            fn fence<'a>(&'a self, previous: &'a Request) -> Operation<'a, ()> {
                self.fences.borrow_mut().push(previous.0);
                Box::pin(futures::future::poll_fn(move |_| {
                    if !self.ready.get() {
                        Poll::Pending
                    } else if self.fail_fence.get() {
                        Poll::Ready(Err(Error::Io))
                    } else {
                        Poll::Ready(Ok(()))
                    }
                }))
            }

            /// Never classify cancellation as missing-file success.
            fn is_missing(error: Error) -> bool {
                error == Error::NotFound
            }
        }

        let host = Rc::new(Hosting {
            reactor: Reactor::new(4, ()),
            next: Cell::new(0),
            fences: RefCell::default(),
            ready: Cell::new(false),
            fail_fence: Cell::new(false),
            fail_fresh: Cell::new(false),
        });
        assert!(std::ptr::eq(Host::reactor(&host), &host.reactor));
        assert!(<Rc<Hosting> as Host>::is_missing(Error::NotFound));
        assert!(!<Rc<Hosting> as Host>::is_missing(Error::Canceled));
        let attempts = Attempts::default();
        let parent = Request(0, false);
        let (guard, scope) = futures::executor::block_on(attempts.begin(&host, &parent)).unwrap();
        assert_eq!(scope.0, 1);
        assert!(matches!(
            futures::executor::block_on(attempts.begin(&host, &parent)),
            Err(Error::Overloaded)
        ));
        drop(guard);
        let mut pending = Box::pin(attempts.begin(&host, &parent));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(pending.as_mut().poll(&mut cx).is_pending());
        assert_eq!(host.next.get(), 1);
        drop(pending);
        host.ready.set(true);
        host.fail_fence.set(true);
        assert!(matches!(
            futures::executor::block_on(attempts.begin(&host, &parent)),
            Err(Error::Io)
        ));
        host.fail_fence.set(false);
        host.fail_fresh.set(true);
        assert!(matches!(
            futures::executor::block_on(attempts.begin(&host, &parent)),
            Err(Error::Unavailable)
        ));
        host.fail_fresh.set(false);
        let (guard, scope) = futures::executor::block_on(attempts.begin(&host, &parent)).unwrap();
        assert_eq!(scope.0, 2);
        assert_eq!(&*host.fences.borrow(), &[1, 1, 1, 1]);
        drop(guard);
        assert!(matches!(
            futures::executor::block_on(attempts.begin(&host, &Request(0, true))),
            Err(Error::Canceled)
        ));
        assert_eq!(host.next.get(), 2);
        let (guard, _) = futures::executor::block_on(attempts.begin(&host, &parent)).unwrap();
        assert_eq!(host.fences.borrow().last(), Some(&2));
        drop(guard);
    }

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
