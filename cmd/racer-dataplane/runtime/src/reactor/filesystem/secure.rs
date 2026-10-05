//! Descriptor-relative traversal and metadata checks, without application policy.
//! Callers select limits, required ownership/access, and error classification.
use super::*;
use std::ffi::OsStr;
use std::os::unix::ffi::OsStrExt;
use std::path::{Component, Path};

pub const BENEATH: u64 = 0x08;
pub const NO_MAGICLINKS: u64 = 0x02;
pub const NO_SYMLINKS: u64 = 0x04;

/// Validate byte length and embedded NULs without normalizing the path.
pub fn path_name(path: &OsStr, limit: usize) -> Result<CString> {
    if path.as_bytes().len() > limit {
        return Err(Error::InvalidInput);
    }
    CString::new(path.as_bytes()).map_err(|_| Error::InvalidInput)
}

/// A single normal component, never a root, parent, current directory or path.
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

/// Missing metadata is distinct from a failed access requirement so the caller
/// can distinguish an I/O failure from an authorization failure.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AccessError {
    MissingMetadata,
    PermissionDenied,
}

#[derive(Clone, Copy, Debug)]
pub struct AccessRequirements {
    pub owner: u32,
    pub forbidden_mode: u16,
    pub links: Option<u32>,
}

/// Check stat completeness, ownership, forbidden mode bits and optional links.
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

/// Validate type/size fields before reading; callers still need a bounded read
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

#[cfg(test)]
mod tests {
    use super::*;

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

impl<S: Scope, B: Budget> Reactor<S, B>
where
    S::Error: PartialEq,
{
    /// Walk from `/` or `.`, pinning each directory and rejecting symlinks and
    /// parent components. Creation uses file_mkdir's owner-only mode. Each mkdir,
    /// including an existing component, is followed by a parent durability fence.
    /// Ownership and final-directory permissions are checked separately by callers.
    pub fn file_directory<'a>(
        &'a self,
        path: &'a Path,
        create: bool,
        path_limit: usize,
        scope: &'a S,
    ) -> Operation<'a, Rc<Descriptor>, S::Error> {
        Box::pin(async move {
            path_name(path.as_os_str(), path_limit)?;
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
                if create {
                    match self
                        .file_mkdir(fd.clone(), path_name(part, path_limit)?, scope)
                        .await
                    {
                        Ok(()) => (),
                        // A canceled mkdir may have missed the parent fsync.
                        Err(error) if error == Error::AlreadyExists.into() => (),
                        Err(error) => return Err(error),
                    }
                    self.file_sync(fd.clone(), scope).await?;
                }
                fd = self
                    .file_open(
                        Some(fd),
                        path_name(part, path_limit)?,
                        libc::O_RDONLY | libc::O_DIRECTORY,
                        BENEATH | NO_SYMLINKS,
                        scope,
                    )
                    .await?;
            }
            Ok(fd)
        })
    }

    /// Remove a caller-validated relative name and sync the parent even if the
    /// name was absent, restoring the fence after a previously canceled unlink.
    pub fn file_remove_synced<'a>(
        &'a self,
        directory: Rc<Descriptor>,
        name: CString,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            match self.file_unlink(directory.clone(), name, scope).await {
                Ok(()) => (),
                Err(error) if error == Error::NotFound.into() => (),
                Err(error) => return Err(error),
            }
            self.file_sync(directory, scope).await
        })
    }

    /// Remove and fence a stale stage, then create an exclusive owner-only file
    /// beneath the pinned directory without following symlinks. The caller owns
    /// stage naming, serialization, publication, and cleanup after failure.
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
