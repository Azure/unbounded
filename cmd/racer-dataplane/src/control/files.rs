//! Descriptor-relative, bounded control file access and durable atomic replacement.
use crate::error::{Error, Result};
use std::{
    ffi::CString,
    fs::File,
    io::{Read, Write},
    os::{
        fd::{AsRawFd, FromRawFd},
        unix::fs::MetadataExt,
    },
    path::{Component, Path},
};
pub(crate) fn read_path(path: &Path, limit: usize) -> Result<Vec<u8>> {
    use std::os::unix::fs::OpenOptionsExt;
    // Projection symlinks are allowed here, but FIFOs/devices never block or pass
    // the subsequent regular-file check.
    let file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK | libc::O_CLOEXEC)
        .open(path)
        .map_err(|_| Error::Io)?;
    read_file(file, limit, false)
}

fn name(s: &std::ffi::OsStr) -> Result<CString> {
    use std::os::unix::ffi::OsStrExt;
    CString::new(s.as_bytes()).map_err(|_| Error::InvalidConfiguration)
}
pub(crate) fn directory(path: &Path, create: bool, private: bool) -> Result<File> {
    let root = if path.is_absolute() { "/" } else { "." };
    let mut fd = File::open(root).map_err(|_| Error::Io)?;
    for component in path.components() {
        let Component::Normal(part) = component else {
            if matches!(component, Component::RootDir | Component::CurDir) {
                continue;
            }
            return Err(Error::InvalidConfiguration);
        };
        let c = name(part)?;
        if create {
            // SAFETY: valid directory descriptor and NUL-terminated single component.
            let result = unsafe { libc::mkdirat(fd.as_raw_fd(), c.as_ptr(), 0o700) };
            if result == 0 {
                fd.sync_all().map_err(|_| Error::Io)?;
            }
            if result < 0
                && std::io::Error::last_os_error().kind() != std::io::ErrorKind::AlreadyExists
            {
                return Err(Error::Io);
            }
        }
        fd = open_at(&fd, part, libc::O_RDONLY | libc::O_DIRECTORY)?;
    }
    if private {
        let m = fd.metadata().map_err(|_| Error::Io)?;
        // SAFETY: geteuid has no arguments or memory effects.
        if m.mode() & 0o077 != 0 || m.uid() != unsafe { libc::geteuid() } {
            return Err(Error::Unauthorized);
        }
    }
    Ok(fd)
}
pub(crate) fn open_at(dir: &File, component: &std::ffi::OsStr, flags: i32) -> Result<File> {
    if Path::new(component).components().count() != 1
        || !matches!(
            Path::new(component).components().next(),
            Some(Component::Normal(_))
        )
    {
        return Err(Error::InvalidRequest);
    }
    let c = name(component)?;
    // SAFETY: descriptor and pathname live through the call; ownership transfers once.
    let fd = unsafe {
        libc::openat(
            dir.as_raw_fd(),
            c.as_ptr(),
            flags | libc::O_NOFOLLOW | libc::O_CLOEXEC | libc::O_NONBLOCK,
            0o600,
        )
    };
    if fd < 0 {
        return Err(
            if std::io::Error::last_os_error().kind() == std::io::ErrorKind::NotFound {
                Error::MissingKey
            } else {
                Error::Io
            },
        );
    }
    Ok(unsafe { File::from_raw_fd(fd) })
}
pub(crate) fn read_at(dir: &File, component: &str, limit: usize, private: bool) -> Result<Vec<u8>> {
    let file = open_at(dir, component.as_ref(), libc::O_RDONLY)?;
    read_file(file, limit, private)
}
pub(crate) fn read_file(file: File, limit: usize, private: bool) -> Result<Vec<u8>> {
    let m = file.metadata().map_err(|_| Error::Io)?;
    if !m.is_file() || m.len() > limit as u64 {
        return Err(Error::InvalidRequest);
    }
    if private && (m.mode() & 0o077 != 0 || m.nlink() != 1 || m.uid() != unsafe { libc::geteuid() })
    {
        return Err(Error::Unauthorized);
    }
    let mut bytes = Vec::new();
    file.take(limit as u64 + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| Error::Io)?;
    if bytes.len() > limit {
        return Err(Error::Overloaded);
    }
    Ok(bytes)
}
pub(crate) fn atomic_write(dir: &File, target: &str, bytes: &[u8]) -> Result<()> {
    let mut random = [0; 16];
    getrandom::getrandom(&mut random).map_err(|_| Error::Io)?;
    let temporary = format!(".stage-{:032x}", u128::from_be_bytes(random));
    let mut file = open_at(
        dir,
        temporary.as_ref(),
        libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL,
    )?;
    let result = (|| {
        file.write_all(bytes).map_err(|_| Error::Io)?;
        file.sync_all().map_err(|_| Error::Io)?;
        let from = name(temporary.as_ref())?;
        let to = name(target.as_ref())?;
        // SAFETY: both names are directory-relative and descriptors stay owned.
        if unsafe { libc::renameat(dir.as_raw_fd(), from.as_ptr(), dir.as_raw_fd(), to.as_ptr()) }
            != 0
        {
            return Err(Error::Io);
        }
        dir.sync_all().map_err(|_| Error::Io)
    })();
    if result.is_err() {
        let _ = remove(dir, &temporary);
    }
    result
}
pub(crate) fn remove(dir: &File, component: &str) -> Result<()> {
    let c = name(component.as_ref())?;
    if unsafe { libc::unlinkat(dir.as_raw_fd(), c.as_ptr(), 0) } != 0
        && std::io::Error::last_os_error().kind() != std::io::ErrorKind::NotFound
    {
        return Err(Error::Io);
    }
    dir.sync_all().map_err(|_| Error::Io)
}

/// Open Kubernetes' ..data target once, never through the per-file symlink.
pub(crate) fn projected_file(
    directory_path: &Path,
    component: &str,
    limit: usize,
) -> Result<Vec<u8>> {
    use std::os::unix::ffi::OsStrExt;
    let dir = directory(directory_path, false, false)?;
    let mut target = [0u8; 4096];
    let n = unsafe {
        libc::readlinkat(
            dir.as_raw_fd(),
            c"..data".as_ptr(),
            target.as_mut_ptr().cast(),
            target.len(),
        )
    };
    if n < 0 || n as usize == target.len() {
        return Err(Error::InvalidRequest);
    }
    let target = std::ffi::OsStr::from_bytes(&target[..n as usize]);
    let generation = open_at(&dir, target, libc::O_RDONLY | libc::O_DIRECTORY)?;
    read_at(&generation, component, limit, false)
}
