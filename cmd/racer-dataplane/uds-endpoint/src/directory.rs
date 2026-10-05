//! Single-component, descriptor-relative operations. No helper follows a named symlink.

use crate::{Error, error, file_path, same_inode};
use std::ffi::CString;
use std::fs::{self, File, Metadata};
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::fs::PermissionsExt;

#[cfg(test)]
thread_local! {
    pub(crate) static FAIL_METADATA: std::cell::RefCell<Option<String>> = const { std::cell::RefCell::new(None) };
}

pub(crate) fn component(name: &[u8]) -> io::Result<CString> {
    if name.is_empty() || name == b"." || name == b".." || name.contains(&b'/') || name.len() > 255
    {
        return Err(error(Error::InvalidName));
    }
    CString::new(name).map_err(|_| error(Error::InvalidName))
}

#[derive(Clone, Copy)]
pub(crate) struct Dir<'a>(pub(crate) &'a File);

impl Dir<'_> {
    pub(crate) fn open(&self, name: &[u8], flags: i32, mode: u32) -> io::Result<File> {
        let name = component(name)?;
        // SAFETY: name is terminated and the borrowed directory remains open.
        let fd = unsafe {
            libc::openat(
                self.0.as_raw_fd(),
                name.as_ptr(),
                flags | libc::O_NOFOLLOW | libc::O_CLOEXEC,
                mode as libc::mode_t,
            )
        };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: openat returned a new descriptor, transferred exactly once.
        Ok(File::from(unsafe { OwnedFd::from_raw_fd(fd) }))
    }

    pub(crate) fn pin(&self, name: &str) -> io::Result<File> {
        self.open(name.as_bytes(), libc::O_PATH, 0)
    }

    pub(crate) fn metadata(&self, name: &str) -> io::Result<Metadata> {
        #[cfg(test)]
        if FAIL_METADATA.with(|failure| failure.borrow().as_deref() == Some(name)) {
            return Err(io::Error::from_raw_os_error(libc::EACCES));
        }
        self.pin(name)?.metadata()
    }

    pub(crate) fn mkdir(&self, name: &[u8], mode: u32) -> io::Result<()> {
        let name = component(name)?;
        // SAFETY: terminated name and live directory descriptor.
        status(unsafe { libc::mkdirat(self.0.as_raw_fd(), name.as_ptr(), mode as libc::mode_t) })
    }

    pub(crate) fn link(&self, from: &str, to: &str) -> io::Result<()> {
        let from = component(from.as_bytes())?;
        let to = component(to.as_bytes())?;
        // SAFETY: both terminated names are relative to the retained directory.
        status(unsafe {
            libc::linkat(
                self.0.as_raw_fd(),
                from.as_ptr(),
                self.0.as_raw_fd(),
                to.as_ptr(),
                0,
            )
        })
    }

    pub(crate) fn rename(&self, from: &str, to: &str, flags: u32) -> io::Result<()> {
        let from = component(from.as_bytes())?;
        let to = component(to.as_bytes())?;
        // SAFETY: both terminated names are relative to the retained directory.
        status(unsafe {
            libc::renameat2(
                self.0.as_raw_fd(),
                from.as_ptr(),
                self.0.as_raw_fd(),
                to.as_ptr(),
                flags,
            )
        })
    }

    /// A stat error is not absence. Callers must retain recovery proof on error.
    /// Directory mutation must be serialized by the owner; Linux has no
    /// compare-inode-and-unlink syscall protecting against hostile same-UID actors.
    pub(crate) fn unlink_owned(&self, name: &str, expected: &Metadata) -> io::Result<()> {
        let current = match self.metadata(name) {
            Ok(current) => current,
            Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
            Err(e) => return Err(e),
        };
        if !same_inode(&current, expected) {
            return Ok(());
        }
        let name = component(name.as_bytes())?;
        // SAFETY: terminated name and retained directory. Identity was checked above.
        status(unsafe { libc::unlinkat(self.0.as_raw_fd(), name.as_ptr(), 0) })
    }
}

fn status(result: i32) -> io::Result<()> {
    if result == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

/// fchmod does not support O_PATH. The proc descriptor link addresses the pinned
/// inode rather than a mutable directory entry. Keep `file` open for this call.
pub(crate) fn chmod_pin(file: &File, mode: u32) -> io::Result<()> {
    fs::set_permissions(file_path(file), fs::Permissions::from_mode(mode))
}
